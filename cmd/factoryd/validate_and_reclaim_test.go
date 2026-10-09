package main

import (
	"bytes"
	"errors"
	"log"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"buildgate/internal/forge"
	"buildgate/internal/release"
	"buildgate/internal/run"
	"buildgate/internal/workflow"
)

// TestValidateDefaultSandboxIdentityRejectsRoot pins the default-on sandbox
// preflight for root-run services: inheriting 0:0 would be rejected later by
// LaunchSpec only after a run record exists, so configuration must fail first
// with an explicit non-root identity remedy. Root is UID 0 specifically --
// {0, 1000} (root UID, non-root GID) is still root and must be rejected;
// GID alone never determines root-ness.
func TestValidateDefaultSandboxIdentityRejectsRoot(t *testing.T) {
	t.Parallel()
	for _, ids := range [][2]int{{0, 0}, {0, 1000}} {
		if err := validateDefaultSandboxIdentity("", ids[0], ids[1]); err == nil {
			t.Fatalf("validateDefaultSandboxIdentity(empty, %d, %d) = nil, want root-identity rejection", ids[0], ids[1])
		}
	}
	if err := validateDefaultSandboxIdentity("1000:1000", 0, 0); err != nil {
		t.Fatalf("explicit non-root sandbox identity rejected: %v", err)
	}
}

// TestValidateDefaultSandboxIdentityAllowsNonRootUIDWithRootGID is the
// regression test for a real finding (2026-09-05 swarm review, confirmed
// still present 2026-09-08): the original condition rejected unless
// uid != 0 AND gid != 0, so a real non-root UID paired with GID 0 -- a
// legitimate, unprivileged identity, since GID 0 is just the root *group*
// and confers no special privilege by itself -- was incorrectly refused as
// if it were root.
func TestValidateDefaultSandboxIdentityAllowsNonRootUIDWithRootGID(t *testing.T) {
	t.Parallel()
	if err := validateDefaultSandboxIdentity("", 1000, 0); err != nil {
		t.Fatalf("validateDefaultSandboxIdentity(empty, 1000, 0) = %v, want nil -- UID 1000 is not root regardless of GID", err)
	}
}

func TestResolvePiTicketPathWithRelativeWorkspace(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	ticketsDir := filepath.Join(root, "spec", "tickets")
	if err := os.MkdirAll(ticketsDir, 0o750); err != nil {
		t.Fatalf("mkdir ticket directory: %v", err)
	}
	ticketPath := filepath.Join(ticketsDir, "002-relative.md")
	if err := os.WriteFile(ticketPath, []byte("ticket\n"), 0o644); err != nil {
		t.Fatalf("write ticket: %v", err)
	}
	if err := os.Mkdir(workspace, 0o750); err != nil {
		t.Fatalf("mkdir workspace: %v", err)
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("get working directory: %v", err)
	}
	relativeWorkspace, err := filepath.Rel(cwd, workspace)
	if err != nil {
		t.Fatalf("make relative workspace: %v", err)
	}
	gotPath, gotNumber, err := resolvePiTicketPath(relativeWorkspace, "002-relative", "")
	if err != nil {
		t.Fatalf("resolvePiTicketPath: %v", err)
	}
	if gotPath != ticketPath {
		t.Fatalf("path = %q, want %q", gotPath, ticketPath)
	}
	if gotNumber != 2 {
		t.Fatalf("ticket number = %d, want 2", gotNumber)
	}
}

func TestFullSuiteCadenceDueCountsDurablePriorChain(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	project := "/repo"
	saveRun := func(id, priorID string) *run.Run {
		r := &run.Run{ID: id, State: run.StateAccepted, ProjectPath: project, PriorRunID: priorID}
		if err := r.Save(dataDir); err != nil {
			t.Fatalf("save %s: %v", id, err)
		}
		return r
	}
	first := saveRun("slice-1", "")
	second := saveRun("slice-2", first.ID)

	for _, tc := range []struct {
		name    string
		prior   *run.Run
		cadence int
		want    bool
	}{
		{name: "first slice", cadence: 2, want: false},
		{name: "second slice", prior: first, cadence: 2, want: true},
		{name: "third slice", prior: second, cadence: 3, want: true},
		{name: "legacy default", prior: first, cadence: 1, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, _, err := fullSuiteCadenceDue(dataDir, project, tc.prior, tc.cadence)
			if err != nil {
				t.Fatalf("fullSuiteCadenceDue: %v", err)
			}
			if got != tc.want {
				t.Fatalf("due = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestFullSuiteCadenceDueFailsClosedForInvalidDurableChain(t *testing.T) {
	t.Parallel()
	project := "/repo"
	for _, tc := range []struct {
		name  string
		setup func(*testing.T, string) *run.Run
	}{
		{name: "missing predecessor", setup: func(t *testing.T, dataDir string) *run.Run {
			r := &run.Run{ID: "slice-2", State: run.StateAccepted, ProjectPath: project, PriorRunID: "missing"}
			if err := r.Save(dataDir); err != nil {
				t.Fatalf("save run: %v", err)
			}
			return r
		}},
		{name: "cycle", setup: func(t *testing.T, dataDir string) *run.Run {
			a := &run.Run{ID: "slice-a", State: run.StateAccepted, ProjectPath: project, PriorRunID: "slice-b"}
			b := &run.Run{ID: "slice-b", State: run.StateAccepted, ProjectPath: project, PriorRunID: "slice-a"}
			if err := a.Save(dataDir); err != nil {
				t.Fatalf("save slice-a: %v", err)
			}
			if err := b.Save(dataDir); err != nil {
				t.Fatalf("save slice-b: %v", err)
			}
			return a
		}},
		{name: "mismatched project", setup: func(t *testing.T, dataDir string) *run.Run {
			r := &run.Run{ID: "slice-other", State: run.StateAccepted, ProjectPath: "/other"}
			if err := r.Save(dataDir); err != nil {
				t.Fatalf("save run: %v", err)
			}
			return r
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dataDir := t.TempDir()
			prior := tc.setup(t, dataDir)
			if _, _, err := fullSuiteCadenceDue(dataDir, project, prior, 2); err == nil {
				t.Fatal("fullSuiteCadenceDue: expected invalid chain error")
			}
		})
	}
}

// TestDrainAfterShutdownWaitsForInFlightRunsEvenWhenShutdownErrors is the
// regression test for a real P1 finding from codex review of PR #18: a
// still-open GET /runs/{id}/events SSE stream for a nonterminal run can
// keep server.Shutdown's handler-drain from completing within its own
// bounded deadline, returning a non-nil error — serveMain used to return
// as soon as that happened, before ever reaching inFlightRuns.Wait(),
// abandoning the background Temporal worker that wait exists to drain.
// This proves drainAfterShutdown always waits out inFlight — even when
// handed a non-nil shutdownErr — before returning, and still reports that
// error afterward rather than silently swallowing it.
func TestDrainAfterShutdownWaitsForInFlightRunsEvenWhenShutdownErrors(t *testing.T) {
	t.Parallel()
	var inFlight sync.WaitGroup
	inFlight.Add(1)

	waited := make(chan struct{})
	go func() {
		inFlight.Wait()
		close(waited)
	}()

	// Give the goroutine above a moment to actually start blocking on
	// Wait() before drainAfterShutdown is called, so this test can't pass
	// by accident just because Add/Done raced ahead of Wait().
	select {
	case <-waited:
		t.Fatal("inFlight.Wait() returned before Done was ever called")
	case <-time.After(20 * time.Millisecond):
	}

	shutdownErr := errors.New("context deadline exceeded")
	drainDone := make(chan error, 1)
	go func() { drainDone <- drainAfterShutdown(shutdownErr, &inFlight) }()

	// drainAfterShutdown must not return while inFlight is still
	// outstanding — the exact bug this test guards against.
	select {
	case err := <-drainDone:
		t.Fatalf("drainAfterShutdown returned (err=%v) before the in-flight run finished", err)
	case <-time.After(50 * time.Millisecond):
	}

	inFlight.Done()

	select {
	case err := <-drainDone:
		if !errors.Is(err, shutdownErr) {
			t.Fatalf("drainAfterShutdown error = %v, want it to wrap %v", err, shutdownErr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("drainAfterShutdown did not return after the in-flight run finished")
	}
}

// TestRunsNeedingReclaimFindsNonterminalRuns proves the pure "which runs
// need a recovery Worker" decision runsNeedingReclaim was factored out of
// daemonMain's own reclaimAbandonedRunQueues closure for: only nonterminal
// runs not already reclaimed are returned, and a terminal run (accepted/
// halted/quarantined) is correctly excluded even if never reclaimed.
func TestRunsNeedingReclaimFindsNonterminalRuns(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	seed := func(id string, state run.State) {
		t.Helper()
		r := &run.Run{ID: id, State: state, CreatedAt: time.Now().Format(time.RFC3339Nano)}
		if err := r.Save(dataDir); err != nil {
			t.Fatalf("seed run %s: %v", id, err)
		}
	}
	seed("run-slice-running", run.StateSliceRunning)
	seed("run-verifying", run.StateVerifying)
	seed("run-accepted", run.StateAccepted)
	// HaltConfirmed: true — a genuinely confirmed halt, not the
	// unconfirmed case TestRunsNeedingReclaimIncludesUnconfirmedHalt
	// covers separately.
	confirmedHalt := &run.Run{ID: "run-halted", State: run.StateHalted, HaltConfirmed: true, CreatedAt: time.Now().Format(time.RFC3339Nano)}
	if err := confirmedHalt.Save(dataDir); err != nil {
		t.Fatalf("seed run-halted: %v", err)
	}
	seed("run-quarantined", run.StateQuarantined)

	ids, err := runsNeedingReclaim(dataDir, "", nil)
	if err != nil {
		t.Fatalf("runsNeedingReclaim: %v", err)
	}
	slices.Sort(ids)
	want := []string{"run-slice-running", "run-verifying"}
	if !slices.Equal(ids, want) {
		t.Fatalf("ids = %v, want %v", ids, want)
	}
}

// TestRunsNeedingReclaimSkipsAlreadyReclaimed is the regression test for a
// real P1 finding from review: an earlier reclaim scan ran only once, at
// daemon startup, so a run abandoned later (after the daemon was already
// running) was never picked up until the daemon itself restarted. The fix
// makes the scan periodic; this proves the id-based skip that periodic
// re-scanning needs to avoid starting a second, redundant recovery Worker
// for a run it already reclaimed.
func TestRunsNeedingReclaimSkipsAlreadyReclaimed(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	r := &run.Run{ID: "run-already-reclaimed", State: run.StateSliceRunning, CreatedAt: time.Now().Format(time.RFC3339Nano)}
	if err := r.Save(dataDir); err != nil {
		t.Fatalf("seed run: %v", err)
	}

	ids, err := runsNeedingReclaim(dataDir, "", map[string]bool{"run-already-reclaimed": true})
	if err != nil {
		t.Fatalf("runsNeedingReclaim: %v", err)
	}
	if len(ids) != 0 {
		t.Fatalf("ids = %v, want none: already-reclaimed run must not be returned again", ids)
	}
}

// TestRunsNeedingReclaimDiscoversRunsAddedAfterAnEarlierScan is the direct
// proof that runsNeedingReclaim supports the periodic-rescan fix: a run
// that didn't exist during an earlier scan (simulating a submitter
// crashing *after* the daemon already started and completed its startup
// scan) is discovered on a later call — exactly what a periodic ticker
// calling this repeatedly needs.
func TestRunsNeedingReclaimDiscoversRunsAddedAfterAnEarlierScan(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()

	firstScan, err := runsNeedingReclaim(dataDir, "", nil)
	if err != nil {
		t.Fatalf("first scan: %v", err)
	}
	if len(firstScan) != 0 {
		t.Fatalf("first scan = %v, want none: no runs exist yet", firstScan)
	}

	// A run created after the first scan — as if a submitting invocation
	// started and crashed only after the daemon's own startup scan had
	// already completed.
	r := &run.Run{ID: "run-crashed-later", State: run.StateVerifying, CreatedAt: time.Now().Format(time.RFC3339Nano)}
	if err := r.Save(dataDir); err != nil {
		t.Fatalf("seed run: %v", err)
	}

	secondScan, err := runsNeedingReclaim(dataDir, "", nil)
	if err != nil {
		t.Fatalf("second scan: %v", err)
	}
	if !slices.Equal(secondScan, []string{"run-crashed-later"}) {
		t.Fatalf("second scan = %v, want [run-crashed-later]", secondScan)
	}
}

// TestRunsNeedingReclaimExcludesOtherRepositories is the regression test
// for a real P2 finding from codex review: a `-data-dir` shared with runs
// from a different repository (or a direct/plain `-temporal-address`
// invocation that never went through a RepositoryOwnerWorkflow at all, and
// so always has an empty Repository) previously had no way to be excluded
// from this scan. Reclaiming one of those anyway starts a recovery Worker
// on this daemon's own task queue for a request its own repository owner
// has never heard of, and reconcileReclaimedRun then queries that same
// wrong owner forever — it can never observe the request done or in
// progress, so the run is "reclaimed" permanently, doing real (if idle)
// work for nothing.
func TestRunsNeedingReclaimExcludesOtherRepositories(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	mine := &run.Run{ID: "run-mine", State: run.StateVerifying, Repository: "org/mine", CreatedAt: time.Now().Format(time.RFC3339Nano)}
	if err := mine.Save(dataDir); err != nil {
		t.Fatalf("seed run-mine: %v", err)
	}
	other := &run.Run{ID: "run-other-repo", State: run.StateVerifying, Repository: "org/other", CreatedAt: time.Now().Format(time.RFC3339Nano)}
	if err := other.Save(dataDir); err != nil {
		t.Fatalf("seed run-other-repo: %v", err)
	}
	direct := &run.Run{ID: "run-direct-mode", State: run.StateVerifying, CreatedAt: time.Now().Format(time.RFC3339Nano)}
	if err := direct.Save(dataDir); err != nil {
		t.Fatalf("seed run-direct-mode: %v", err)
	}

	ids, err := runsNeedingReclaim(dataDir, "org/mine", nil)
	if err != nil {
		t.Fatalf("runsNeedingReclaim: %v", err)
	}
	if !slices.Equal(ids, []string{"run-mine"}) {
		t.Fatalf("ids = %v, want [run-mine]: a run belonging to a different repository or no repository at all must never be reclaimed by this daemon", ids)
	}
}

// TestTerminalReclaimedRunIDsFindsFinishedRuns is the regression test for a
// real P2 finding from codex review: an earlier version of the daemon's
// periodic reclaim never stopped a reclaimed run's recovery Worker once
// that run actually finished, so a long-lived daemon accumulated one
// Worker per abandoned run ever seen, without bound. This proves the pure
// "which reclaimed runs are now terminal" decision directly.
func TestTerminalReclaimedRunIDsFindsFinishedRuns(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	seed := func(id string, state run.State) {
		t.Helper()
		r := &run.Run{ID: id, State: state, CreatedAt: time.Now().Format(time.RFC3339Nano)}
		if err := r.Save(dataDir); err != nil {
			t.Fatalf("seed run %s: %v", id, err)
		}
	}
	seed("run-still-running", run.StateVerifying)
	seed("run-now-accepted", run.StateAccepted)
	// HaltConfirmed: true — a genuinely confirmed halt, not the
	// unconfirmed case TestTerminalReclaimedRunIDsExcludesUnconfirmedHalt
	// covers separately.
	confirmedHalt := &run.Run{ID: "run-now-halted", State: run.StateHalted, HaltConfirmed: true, CreatedAt: time.Now().Format(time.RFC3339Nano)}
	if err := confirmedHalt.Save(dataDir); err != nil {
		t.Fatalf("seed run-now-halted: %v", err)
	}

	reclaimed := map[string]bool{"run-still-running": true, "run-now-accepted": true, "run-now-halted": true}
	ids := terminalReclaimedRunIDs(dataDir, reclaimed)
	slices.Sort(ids)
	want := []string{"run-now-accepted", "run-now-halted"}
	if !slices.Equal(ids, want) {
		t.Fatalf("ids = %v, want %v", ids, want)
	}
}

// TestApplyRunWorkflowResultSetsHaltConfirmed is the regression test for a
// real P1 finding from codex review: applyRunWorkflowResult wrote r.State
// from the owner's own confirmed result but never touched
// r.HaltConfirmed, so reconciling a run that was previously written by a
// give-up path (see run.Run.HaltConfirmed's doc comment) left that field
// at its stale, unconfirmed value even though the applied result is, by
// construction, always confirmed. Left stale, an operator later
// overriding a reconciled quarantine back to StateHalted would still read
// as an unconfirmed halt — the daemon's reclaim scan would pick it up
// again and this same function could silently overwrite the operator's
// override with the old owner result, including re-sending its
// quarantine notification.
func TestApplyRunWorkflowResultSetsHaltConfirmed(t *testing.T) {
	dataDir := t.TempDir()
	r := &run.Run{
		ID:            "run-reconciled",
		Ticket:        "fixture-ticket",
		State:         run.StateHalted,
		HaltConfirmed: false,
		CreatedAt:     time.Now().Format(time.RFC3339Nano),
	}
	if err := r.Save(dataDir); err != nil {
		t.Fatalf("seed run: %v", err)
	}

	result := workflow.RunWorkflowResult{State: run.StateAccepted, ResultSHA: "deadbeef"}
	if err := applyRunWorkflowResult(newTestDeps(t), r, dataDir, r.ID, r.Ticket, t.TempDir(), "basesha", "task-queue", result, true, &release.MergePolicy{}, forge.GHPullRequestOpener{}, false); err != nil {
		t.Fatalf("applyRunWorkflowResult: %v", err)
	}
	if !r.HaltConfirmed {
		t.Fatalf("HaltConfirmed = false after applying a confirmed owner result, want true")
	}
	reloaded, err := run.Load(dataDir, r.ID)
	if err != nil {
		t.Fatalf("load reconciled run: %v", err)
	}
	if !reloaded.HaltConfirmed {
		t.Fatalf("persisted HaltConfirmed = false after applying a confirmed owner result, want true")
	}
	if reloaded.State != run.StateAccepted {
		t.Fatalf("persisted State = %q, want %q", reloaded.State, run.StateAccepted)
	}
}

// TestApplyRunWorkflowResultCopiesHaltReasonCode is the regression test
// for a code-review finding: this function copied every other
// RunWorkflowResult evidence field (Attempts, ChangedFiles, ...) but not
// HaltReasonCode, so a repository-owner
// run halted on its own relay ceiling (recorded on the RunWorkflowResult --
// see that field's own doc comment for why it must be recovered there,
// distinct from runViaTemporal's own handling) never reached the
// durable run.Run.HaltReasonCode this daemon's own halt-reason feature
// exists to set.
func TestApplyRunWorkflowResultCopiesHaltReasonCode(t *testing.T) {
	dataDir := t.TempDir()
	r := &run.Run{
		ID:        "run-relay-ceiling",
		Ticket:    "fixture-ticket",
		CreatedAt: time.Now().Format(time.RFC3339Nano),
	}
	if err := r.Save(dataDir); err != nil {
		t.Fatalf("seed run: %v", err)
	}

	// A non-accepted terminal state always returns an error here (see
	// TestApplyRunWorkflowResultNotifiesHaltedNotQuarantined's own comment
	// on this function's "run quarantined: ..." return, worded that way
	// regardless of the actual state) -- this test cares about the field
	// copied before that return, not the error itself.
	result := workflow.RunWorkflowResult{State: run.StateHalted, HaltReasonCode: run.HaltReasonRelayCeilingExceeded}
	_ = applyRunWorkflowResult(newTestDeps(t), r, dataDir, r.ID, r.Ticket, t.TempDir(), "basesha", "task-queue", result, true, &release.MergePolicy{}, forge.GHPullRequestOpener{}, false)
	if r.HaltReasonCode != run.HaltReasonRelayCeilingExceeded {
		t.Fatalf("HaltReasonCode = %q, want %q", r.HaltReasonCode, run.HaltReasonRelayCeilingExceeded)
	}
	reloaded, err := run.Load(dataDir, r.ID)
	if err != nil {
		t.Fatalf("load reconciled run: %v", err)
	}
	if reloaded.HaltReasonCode != run.HaltReasonRelayCeilingExceeded {
		t.Fatalf("persisted HaltReasonCode = %q, want %q", reloaded.HaltReasonCode, run.HaltReasonRelayCeilingExceeded)
	}
}

// TestApplyRunWorkflowResultNotifiesHaltedNotQuarantined is the regression
// test for the 2026-09-05 Opus review finding S2's evidence-fidelity bug:
// this Temporal-routed path's own non-accepted notification hardcoded
// State: run.StateQuarantined regardless of r's actual state, so a halted
// run (one that never reached a policy gate decision at all) emitted a
// durable notification record claiming it was quarantined instead.
func TestApplyRunWorkflowResultNotifiesHaltedNotQuarantined(t *testing.T) {
	dataDir := t.TempDir()
	r := &run.Run{
		ID:        "run-halted",
		Ticket:    "fixture-ticket",
		State:     run.StateReady,
		CreatedAt: time.Now().Format(time.RFC3339Nano),
	}
	if err := r.Save(dataDir); err != nil {
		t.Fatalf("seed run: %v", err)
	}

	// A non-accepted terminal state always returns an error here (see
	// this function's own "run quarantined: ..." return, worded that way
	// regardless of the actual state) -- this test cares about the
	// notification recorded before that return, not the error itself.
	result := workflow.RunWorkflowResult{State: run.StateHalted}
	_ = applyRunWorkflowResult(newTestDeps(t), r, dataDir, r.ID, r.Ticket, t.TempDir(), "basesha", "task-queue", result, true, &release.MergePolicy{}, forge.GHPullRequestOpener{}, false)
	if r.State != run.StateHalted {
		t.Fatalf("State = %q, want %q", r.State, run.StateHalted)
	}
	if len(r.Notifications) != 1 {
		t.Fatalf("len(Notifications) = %d, want 1", len(r.Notifications))
	}
	if r.Notifications[0].State != run.StateHalted {
		t.Errorf("Notifications[0].State = %q, want %q (not %q)", r.Notifications[0].State, run.StateHalted, run.StateQuarantined)
	}
	if strings.Contains(r.Notifications[0].Reason, "policy gate did not pass") {
		t.Errorf("Notifications[0].Reason = %q, want a halted-specific reason, not the quarantine-gate wording", r.Notifications[0].Reason)
	}
}

// TestApplyRunWorkflowResultRecordsReleaseDecisionUnderStableProject is the
// regression test for a real Codex review finding on PR #45:
// reconcileReclaimedRun's own call into applyRunWorkflowResult passes
// fresh.WorkspacePath as this function's workspacePath parameter, which
// for an isolated run is the isolated worktree under
// .../workspaces/<run-id> -- not the stable original checkout.
// release.ProjectFromWorkspace(workspacePath) would then derive
// "workspaces" (the worktree's own parent directory's basename) as the
// project instead of the real one, storing the release decision under the
// wrong project and consulting the wrong kill switch. This deliberately
// passes a workspacePath argument distinct from r.ProjectPath (mimicking
// exactly that isolated-worktree-vs-stable-checkout mismatch) and proves
// the recorded decision's project is derived from r.ProjectPath, which
// applyRunWorkflowResult never reassigns, regardless of what
// workspacePath happens to be.
func TestApplyRunWorkflowResultRecordsReleaseDecisionUnderStableProject(t *testing.T) {
	dataDir := t.TempDir()
	projectRoot := t.TempDir()
	stableProjectPath := filepath.Join(projectRoot, "checkout")
	isolatedWorktreePath := filepath.Join(t.TempDir(), "workspaces", "run-isolated-reconciled")
	r := &run.Run{
		ID:          "run-isolated-reconciled",
		Ticket:      "fixture-ticket",
		ProjectPath: stableProjectPath,
		State:       run.StateSliceRunning,
		CreatedAt:   time.Now().Format(time.RFC3339Nano),
	}
	if err := r.Save(dataDir); err != nil {
		t.Fatalf("seed run: %v", err)
	}

	result := workflow.RunWorkflowResult{State: run.StateAccepted, ResultSHA: "deadbeef"}
	// isolatedWorktreePath, not stableProjectPath: exactly what
	// reconcileReclaimedRun passes as workspacePath for an isolated run
	// (fresh.WorkspacePath).
	if err := applyRunWorkflowResult(newTestDeps(t), r, dataDir, r.ID, r.Ticket, isolatedWorktreePath, "basesha", "task-queue", result, false, &release.MergePolicy{}, forge.GHPullRequestOpener{}, false); err != nil {
		t.Fatalf("applyRunWorkflowResult: %v", err)
	}

	wrongProject := release.ProjectFromWorkspace(isolatedWorktreePath)
	wrongDecisionPath := filepath.Join(dataDir, "projects", wrongProject, "release-decisions", r.ID+".json")
	if _, err := os.Stat(wrongDecisionPath); !os.IsNotExist(err) {
		t.Fatalf("release decision recorded under %q (derived from the isolated worktree path), err=%v -- want it under the stable project instead", wrongProject, err)
	}

	rightProject := release.ProjectFromWorkspace(stableProjectPath)
	rightDecisionPath := filepath.Join(dataDir, "projects", rightProject, "release-decisions", r.ID+".json")
	if _, err := os.Stat(rightDecisionPath); err != nil {
		t.Fatalf("stat %s: %v -- want the release decision recorded under the stable project derived from r.ProjectPath", rightDecisionPath, err)
	}
}

// TestRunReleasePolicyRoundTrip is a regression test proving
// runReleasePolicy/mergePolicyFromRun round-trip every field of an
// internal/release.MergePolicy through the run.ReleasePolicy shape
// persisted on a run record, and mergePolicyFromRun must report ok=false
// (never guess a permissive policy) for a legacy run record with no
// persisted policy.
func TestRunReleasePolicyRoundTrip(t *testing.T) {
	t.Parallel()
	original := release.MergePolicy{
		ProtectedPaths:                 []string{"a/b", "c/d"},
		MaxFilesChanged:                12,
		MaxInsertions:                  345,
		RollbackPlan:                   "git revert the merge commit on main",
		AllowOverrides:                 true,
		AllowDependencyLockfileChanges: true,
		AllowUnsandboxed:               true,
		RequiredGates:                  []string{"canonical_verify", "full_suite_verify"},
		AllowSkippedProjectCheck:       true,
	}
	r := &run.Run{ReleasePolicy: runReleasePolicy(original)}
	recovered, ok := mergePolicyFromRun(r)
	if !ok {
		t.Fatal("ok = false, want true for a run carrying a persisted ReleasePolicy")
	}
	if !reflect.DeepEqual(recovered, original) {
		t.Errorf("recovered = %+v, want %+v", recovered, original)
	}

	legacy := &run.Run{ID: "legacy-run-no-policy"}
	if _, ok := mergePolicyFromRun(legacy); ok {
		t.Error("ok = true, want false for a legacy run record with no persisted ReleasePolicy")
	}
}

// TestReconcileEvaluatesPersistedReleasePolicyNotAlwaysDenyingDefault is
// the regression test for a reconciled/reclaimed run's release policy:
// before this fix, reconcileReclaimedRun always evaluated release
// eligibility against a
// zero-value release.MergePolicy{} (0 files/insertions allowed, empty
// rollback plan -- MergePolicyCheck denies unconditionally), regardless
// of what policy the run actually started with. This exercises the exact
// mechanism reclaim.go now uses -- recovering the policy via
// mergePolicyFromRun(fresh) and passing it to applyRunWorkflowResult --
// and proves a run that started with a real, permissive policy is
// allowed on reconciliation, while a legacy run with no persisted policy
// keeps today's fail-closed denial.
func TestReconcileEvaluatesPersistedReleasePolicyNotAlwaysDenyingDefault(t *testing.T) {
	t.Run("permissive persisted policy allows", func(t *testing.T) {
		dataDir := t.TempDir()
		r := &run.Run{
			ID:            "run-o2-permissive",
			Ticket:        "fixture-ticket",
			ProjectPath:   t.TempDir(),
			State:         run.StateSliceRunning,
			ReleasePolicy: runReleasePolicy(*allowingMergePolicyForTest()),
			CreatedAt:     time.Now().Format(time.RFC3339Nano),
		}
		if err := r.Save(dataDir); err != nil {
			t.Fatalf("seed run: %v", err)
		}
		// releasePolicy passed as nil, exactly as reconcileReclaimedRun
		// now calls this function (a Codex-review follow-up moved the
		// mergePolicyFromRun recovery from reclaim.go into
		// applyRunWorkflowResult itself, centrally, for every
		// releasePolicy==nil caller -- see this function's own top-of-file
		// doc comment): this proves the centralized fallback recovers r's
		// own persisted ReleasePolicy, not just that a caller-supplied
		// pointer built from mergePolicyFromRun works.
		result := allowingRunWorkflowResultForTest(run.StateAccepted, "deadbeef")
		if err := applyRunWorkflowResult(newTestDeps(t), r, dataDir, r.ID, r.Ticket, r.ProjectPath, "basesha", "task-queue", result, false, nil, forge.GHPullRequestOpener{}, false); err != nil {
			t.Fatalf("applyRunWorkflowResult: %v", err)
		}
		project := release.ProjectFromWorkspace(r.ProjectPath)
		decision, err := release.LoadDecision(dataDir, project, r.ID)
		if err != nil {
			t.Fatalf("load decision: %v", err)
		}
		if !decision.Allowed {
			t.Errorf("decision.Allowed = false, want true: reasons=%v", decision.Reasons)
		}
	})

	t.Run("legacy run with no persisted policy stays fail-closed", func(t *testing.T) {
		dataDir := t.TempDir()
		r := &run.Run{
			ID:          "run-o2-legacy",
			Ticket:      "fixture-ticket",
			ProjectPath: t.TempDir(),
			State:       run.StateSliceRunning,
			CreatedAt:   time.Now().Format(time.RFC3339Nano),
		}
		if err := r.Save(dataDir); err != nil {
			t.Fatalf("seed run: %v", err)
		}
		var releasePolicyPtr *release.MergePolicy
		if recovered, ok := mergePolicyFromRun(r); ok {
			releasePolicyPtr = &recovered
		}
		if releasePolicyPtr != nil {
			t.Fatal("mergePolicyFromRun unexpectedly recovered a policy for a legacy run record")
		}
		result := allowingRunWorkflowResultForTest(run.StateAccepted, "deadbeef")
		if err := applyRunWorkflowResult(newTestDeps(t), r, dataDir, r.ID, r.Ticket, r.ProjectPath, "basesha", "task-queue", result, false, releasePolicyPtr, forge.GHPullRequestOpener{}, false); err != nil {
			t.Fatalf("applyRunWorkflowResult: %v", err)
		}
		project := release.ProjectFromWorkspace(r.ProjectPath)
		decision, err := release.LoadDecision(dataDir, project, r.ID)
		if err != nil {
			t.Fatalf("load decision: %v", err)
		}
		if decision.Allowed {
			t.Error("decision.Allowed = true, want false: a legacy run with no persisted policy must stay fail-closed against release.MergePolicy{}")
		}
	})
}

// TestReconcilePersistedPolicyRendersProtectedPathEvaluationInPRBody is the
// regression test for a related apply_run_result.go bug found in the same
// review pass as TestReconcileEvaluatesPersistedReleasePolicyNotAlwaysDenyingDefault
// above: applyRunWorkflowResult used to pass openEvidencePullRequest the
// caller's original releasePolicy pointer -- nil for a reconciled/reclaimed run --
// even after recovering a real policy from the run record into
// decisionPolicy for the release decision itself. renderRiskHeader
// (release_and_evidence.go) treats a nil policy as "not evaluated", so a
// recovered, ALLOWED run's own PR claimed its protected-path check was
// never evaluated at all, even though the decision that let the PR open
// was in fact evaluated against a real, persisted policy. The fix threads
// prPolicy (the policy the decision actually used) into
// openEvidencePullRequest instead. This proves the PR body renders "not
// touched" (a real evaluation, since the persisted policy's ProtectedPaths
// don't match this run's ChangedFiles) rather than "not evaluated".
func TestReconcilePersistedPolicyRendersProtectedPathEvaluationInPRBody(t *testing.T) {
	dataDir := t.TempDir()
	policy := allowingMergePolicyForTest()
	policy.ProtectedPaths = []string{"secrets/prod.env"} // not among allowingRunWorkflowResultForTest's ChangedFiles
	r := &run.Run{
		ID:              "run-o2-pr-body",
		Ticket:          "fixture-ticket",
		ProjectPath:     t.TempDir(),
		State:           run.StateSliceRunning,
		ReleasePolicy:   runReleasePolicy(*policy),
		CreatedAt:       time.Now().Format(time.RFC3339Nano),
		Branch:          "factoryd/run-o2-pr-body",
		OpenPullRequest: true,
	}
	if err := r.Save(dataDir); err != nil {
		t.Fatalf("seed run: %v", err)
	}

	opener := &fakePullRequestOpener{url: "https://github.com/acme/widgets/pull/42"}
	result := allowingRunWorkflowResultForTest(run.StateAccepted, "deadbeef")
	// releasePolicy passed as nil, exactly as reconcileReclaimedRun calls
	// this function: applyRunWorkflowResult must recover r's own
	// ReleasePolicy and pass that recovered policy (not the nil it
	// received) on to openEvidencePullRequest.
	if err := applyRunWorkflowResult(newTestDeps(t), r, dataDir, r.ID, r.Ticket, r.ProjectPath, "basesha", "task-queue", result, false, nil, opener, false); err != nil {
		t.Fatalf("applyRunWorkflowResult: %v", err)
	}

	if !opener.called {
		t.Fatal("opener not called -- want the release decision allowed and a PR opened")
	}
	if !strings.Contains(opener.calledBody, "- Protected paths: not touched") {
		t.Errorf("PR body = %q, want it to contain %q (a real evaluation against the recovered policy)", opener.calledBody, "- Protected paths: not touched")
	}
	if strings.Contains(opener.calledBody, "not evaluated") {
		t.Errorf("PR body = %q, want no \"not evaluated\" line: a policy was recovered and used for this run's release decision", opener.calledBody)
	}
}

// allowingMergePolicyForTest is a release.MergePolicy that a matching
// run fixture (see allowingRunWorkflowResultForTest) legitimately passes
// -- unlike &release.MergePolicy{}, which denies by construction
// (RollbackPlan == "" alone guarantees that) and, since the release-
// decision gate added ahead of -open-pull-request, would leave
// openEvidencePullRequest correctly never called at all. AllowUnsandboxed
// is set because these fixtures record no build Attempts with an
// ImageDigest.
func allowingMergePolicyForTest() *release.MergePolicy {
	return &release.MergePolicy{RollbackPlan: "reviewed and reversible", MaxFilesChanged: 10, MaxInsertions: 1000, AllowUnsandboxed: true}
}

// allowingRunWorkflowResultForTest is a workflow.RunWorkflowResult that,
// combined with allowingMergePolicyForTest, produces an Allowed release
// decision: a passing gate result, a non-nil changed-file inventory, and
// a diff stat under the policy's limits (see MergePolicyCheck for what it
// inspects).
func allowingRunWorkflowResultForTest(state run.State, resultSHA string) workflow.RunWorkflowResult {
	return workflow.RunWorkflowResult{
		State:        state,
		ResultSHA:    resultSHA,
		ChangedFiles: []string{"lib/app.go"},
		DiffStat:     &run.DiffStat{FilesChanged: 1, Insertions: 2},
		GateResults:  []run.GateResult{{Check: "verify", Passed: true}},
	}
}

// TestApplyRunWorkflowResultOpensPullRequestOnAccept is the regression
// test for a real Codex review finding, PR #64: -open-pull-request only
// ever reached runMainWithReady's own direct-execution accepted branch,
// never applyRunWorkflowResult -- the one completion point every
// Temporal-routed path (runViaTemporal, runViaRepositoryOwner, and
// reconcileReclaimedRun's own delayed reconciliation) shares instead, so
// open_pull_request: true silently opened no PR for any of them. r.Branch
// is deliberately left empty here so openEvidencePullRequest's own real
// call (this function hardcodes forge.GHPullRequestOpener{}, so a test
// without a real git/gh on PATH cannot exercise a successful push) takes
// its own no-op skip path instead of needing one -- what this proves is
// that the call is reached at all, which is exactly what was missing;
// TestOpenEvidencePullRequestRecordsURLOnSuccess already covers the
// push/PR-open mechanics themselves via an injected fake opener.
//
// Uses allowingMergePolicyForTest/allowingRunWorkflowResultForTest, not
// &release.MergePolicy{}: since the release-decision gate added ahead of
// -open-pull-request, an always-denying policy would correctly stop this
// call from ever reaching openEvidencePullRequest at all -- this test
// needs a release decision that legitimately comes back Allowed so it
// still exercises the call this test is actually about.
// not parallel-safe: redirects the global log package's output (log.SetOutput)
func TestApplyRunWorkflowResultOpensPullRequestOnAccept(t *testing.T) {
	dataDir := t.TempDir()
	r := &run.Run{
		ID: "run-1", Ticket: "fixture-ticket", ProjectPath: filepath.Join(t.TempDir(), "checkout"),
		State: run.StateSliceRunning, CreatedAt: time.Now().Format(time.RFC3339Nano),
		OpenPullRequest: true,
	}
	if err := r.Save(dataDir); err != nil {
		t.Fatalf("seed run: %v", err)
	}

	var logBuf bytes.Buffer
	previousOutput := log.Writer()
	log.SetOutput(&logBuf)
	t.Cleanup(func() { log.SetOutput(previousOutput) })

	result := allowingRunWorkflowResultForTest(run.StateAccepted, "deadbeef")
	if err := applyRunWorkflowResult(newTestDeps(t), r, dataDir, r.ID, r.Ticket, r.ProjectPath, "basesha", "task-queue", result, true, allowingMergePolicyForTest(), forge.GHPullRequestOpener{}, false); err != nil {
		t.Fatalf("applyRunWorkflowResult: %v", err)
	}
	if !strings.Contains(logBuf.String(), "-open-pull-request set but no isolated branch exists") {
		t.Errorf("logs = %q, want openEvidencePullRequest's own skip message -- proving this Temporal-routed completion path reaches it at all", logBuf.String())
	}
}

// TestApplyRunWorkflowResultDoesNotOpenPullRequestWhenNotRequested proves
// the flip side: a run that never set OpenPullRequest must not reach
// openEvidencePullRequest at all, even though it still hits an accepted
// completion.
// not parallel-safe: redirects the global log package's output (log.SetOutput)
func TestApplyRunWorkflowResultDoesNotOpenPullRequestWhenNotRequested(t *testing.T) {
	dataDir := t.TempDir()
	r := &run.Run{
		ID: "run-1", Ticket: "fixture-ticket", ProjectPath: filepath.Join(t.TempDir(), "checkout"),
		State: run.StateSliceRunning, CreatedAt: time.Now().Format(time.RFC3339Nano),
	}
	if err := r.Save(dataDir); err != nil {
		t.Fatalf("seed run: %v", err)
	}

	var logBuf bytes.Buffer
	previousOutput := log.Writer()
	log.SetOutput(&logBuf)
	t.Cleanup(func() { log.SetOutput(previousOutput) })

	result := workflow.RunWorkflowResult{State: run.StateAccepted, ResultSHA: "deadbeef"}
	if err := applyRunWorkflowResult(newTestDeps(t), r, dataDir, r.ID, r.Ticket, r.ProjectPath, "basesha", "task-queue", result, true, &release.MergePolicy{}, forge.GHPullRequestOpener{}, false); err != nil {
		t.Fatalf("applyRunWorkflowResult: %v", err)
	}
	if strings.Contains(logBuf.String(), "-open-pull-request") {
		t.Errorf("logs = %q, want no mention of -open-pull-request: this run never requested it", logBuf.String())
	}
}

// TestApplyRunWorkflowResultUnderRunLockCompletesWithFakeOpener is the
// regression test for the deadlock Codex found on PR #86: runViaRepositoryOwner
// and reconcileReclaimedRun both call applyRunWorkflowResult from inside a
// run.WithLock closure they already hold on this same run id (see their own
// call sites), and an earlier version of the accepted branch's
// notifyAcceptedRun/openEvidencePullRequest re-acquired that same lock
// (flock on a second open file descriptor within the same process blocks
// rather than erroring), hanging forever. This reproduces that exact
// locked-call shape -- applyRunWorkflowResult invoked from inside
// run.WithLock(dataDir, id, ...), underRunLock=true, with -open-pull-request
// requested and a real (fake) opener actually reached -- on a background
// goroutine guarded by a timeout, so a regression hangs this test instead
// of the whole suite.
// not parallel-safe: redirects the global log package's output (log.SetOutput)
func TestApplyRunWorkflowResultUnderRunLockCompletesWithFakeOpener(t *testing.T) {
	dataDir := t.TempDir()
	r := &run.Run{
		ID: "run-1", Ticket: "fixture-ticket", ProjectPath: filepath.Join(t.TempDir(), "checkout"),
		State: run.StateSliceRunning, CreatedAt: time.Now().Format(time.RFC3339Nano),
		Branch: "factoryd/run-1", OpenPullRequest: true,
	}
	if err := r.Save(dataDir); err != nil {
		t.Fatalf("seed run: %v", err)
	}

	opener := &fakePullRequestOpener{url: "https://github.com/acme/widgets/pull/9"}
	result := allowingRunWorkflowResultForTest(run.StateAccepted, "deadbeef")

	done := make(chan error, 1)
	go func() {
		done <- run.WithLock(dataDir, r.ID, func() error {
			fresh, err := run.Load(dataDir, r.ID)
			if err != nil {
				return err
			}
			return applyRunWorkflowResult(newTestDeps(t), fresh, dataDir, r.ID, fresh.Ticket, fresh.ProjectPath, "basesha", "task-queue", result, true, allowingMergePolicyForTest(), opener, true)
		})
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("applyRunWorkflowResult under an already-held run.WithLock: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timed out -- applyRunWorkflowResult likely deadlocked re-acquiring run.WithLock from inside a caller that already holds it (PR #86 regression)")
	}

	if !opener.called {
		t.Fatal("opener was never called")
	}
	loaded, err := run.Load(dataDir, r.ID)
	if err != nil {
		t.Fatalf("load run: %v", err)
	}
	if loaded.PullRequestURL != "https://github.com/acme/widgets/pull/9" {
		t.Errorf("PullRequestURL = %q, want the fake opener's own URL", loaded.PullRequestURL)
	}
	if len(loaded.Notifications) != 1 || loaded.Notifications[0].State != run.StateAccepted {
		t.Errorf("Notifications = %+v, want exactly one accepted notification", loaded.Notifications)
	}
}

// TestApplyRunWorkflowResultSkipsEvidenceWhenNotAttributable is the
// regression test for a real P2 finding from codex review:
// reconcileReclaimedRun's delayed call into applyRunWorkflowResult can run
// long after this run's own build actually finished — long enough for the
// repository owner to have already started a later queued run against the
// same shared workspace, which overwrites BUILD_EVIDENCE.json with that
// later run's own provider/model/usage. Attributing that file's contents
// to this run's audit record regardless of how it got there would be
// silently wrong. attributeWorkspaceEvidence=false must skip reading it
// entirely, even when a file is genuinely present.
func TestApplyRunWorkflowResultSkipsEvidenceWhenNotAttributable(t *testing.T) {
	dataDir := t.TempDir()
	workspace := t.TempDir()
	evidencePath := filepath.Join(workspace, "BUILD_EVIDENCE.json")
	if err := os.WriteFile(evidencePath, []byte(`{"provider":"someone-elses-later-run"}`), 0o644); err != nil {
		t.Fatalf("write BUILD_EVIDENCE.json: %v", err)
	}
	r := &run.Run{ID: "run-reconciled-no-evidence", Ticket: "fixture-ticket", CreatedAt: time.Now().Format(time.RFC3339Nano)}
	if err := r.Save(dataDir); err != nil {
		t.Fatalf("seed run: %v", err)
	}

	result := workflow.RunWorkflowResult{State: run.StateAccepted}
	if err := applyRunWorkflowResult(newTestDeps(t), r, dataDir, r.ID, r.Ticket, workspace, "basesha", "task-queue", result, false, &release.MergePolicy{}, forge.GHPullRequestOpener{}, false); err != nil {
		t.Fatalf("applyRunWorkflowResult: %v", err)
	}
	if r.AgentEvidence != nil {
		t.Fatalf("AgentEvidence = %+v, want nil: attributeWorkspaceEvidence=false must not attribute a possibly-unrelated later run's evidence", r.AgentEvidence)
	}
}

// TestApplyRunWorkflowResultCopiesSpecConformityConfigured and
// TestApplyRunWorkflowResultRetainsConformityEvidence are the regression
// tests for the Temporal-path parity gap an independent design review
// found on the two-phase spec-conformity-review port (PR #178): this
// function copied every other RunWorkflowResult evidence field but not
// SpecConformityConfigured, and never retained CONFORMITY_EVIDENCE.json
// into the run's own durable directory the way run_ticket.go's own
// equivalent phase does -- so a Temporal-routed run whose ticket declared
// -spec-acceptance-criteria was indistinguishable, in the durable record,
// from one that never declared it at all.
func TestApplyRunWorkflowResultCopiesSpecConformityConfigured(t *testing.T) {
	dataDir := t.TempDir()
	r := &run.Run{ID: "run-spec-conformity", Ticket: "fixture-ticket", CreatedAt: time.Now().Format(time.RFC3339Nano)}
	if err := r.Save(dataDir); err != nil {
		t.Fatalf("seed run: %v", err)
	}

	// A non-accepted terminal state always returns an error here (see
	// TestApplyRunWorkflowResultNotifiesHaltedNotQuarantined's own comment
	// on this) -- this test cares about r.SpecConformityConfigured, not
	// the error itself.
	result := workflow.RunWorkflowResult{State: run.StateQuarantined, SpecConformityConfigured: true}
	_ = applyRunWorkflowResult(newTestDeps(t), r, dataDir, r.ID, r.Ticket, t.TempDir(), "basesha", "task-queue", result, false, &release.MergePolicy{}, forge.GHPullRequestOpener{}, false)
	if !r.SpecConformityConfigured {
		t.Fatal("SpecConformityConfigured = false after applying a result with SpecConformityConfigured=true, want true")
	}
	reloaded, err := run.Load(dataDir, r.ID)
	if err != nil {
		t.Fatalf("load run: %v", err)
	}
	if !reloaded.SpecConformityConfigured {
		t.Fatal("persisted SpecConformityConfigured = false, want true")
	}
}

func TestApplyRunWorkflowResultRetainsConformityEvidence(t *testing.T) {
	dataDir := t.TempDir()
	workspace := t.TempDir()
	const conformityEvidence = `{"criteria":[{"criterion":"handles empty input","verdict":"met"}]}`
	if err := os.WriteFile(filepath.Join(workspace, "CONFORMITY_EVIDENCE.json"), []byte(conformityEvidence), 0o644); err != nil {
		t.Fatalf("write fixture CONFORMITY_EVIDENCE.json: %v", err)
	}
	r := &run.Run{ID: "run-conformity-evidence", Ticket: "fixture-ticket", CreatedAt: time.Now().Format(time.RFC3339Nano)}
	if err := r.Save(dataDir); err != nil {
		t.Fatalf("seed run: %v", err)
	}

	result := workflow.RunWorkflowResult{State: run.StateQuarantined, SpecConformityConfigured: true}
	_ = applyRunWorkflowResult(newTestDeps(t), r, dataDir, r.ID, r.Ticket, workspace, "basesha", "task-queue", result, true, &release.MergePolicy{}, forge.GHPullRequestOpener{}, false)
	got, err := os.ReadFile(filepath.Join(run.Dir(dataDir, r.ID), "CONFORMITY_EVIDENCE.json"))
	if err != nil {
		t.Fatalf("read retained CONFORMITY_EVIDENCE.json: %v", err)
	}
	if string(got) != conformityEvidence {
		t.Errorf("retained CONFORMITY_EVIDENCE.json = %q, want %q", got, conformityEvidence)
	}
}

// TestApplyRunWorkflowResultLoadsVerdictsAndOracleCoverageIntoTheRunRecord
// is a Temporal-path regression: retaining CONFORMITY_EVIDENCE.json as a
// file was never enough -- the verdicts must reach the run record (they
// are what the PR evidence renders), and the oracle's MANIFEST.json
// coverage must be read from the directory the run record names, so a
// Temporal-routed run's evidence cross-checks the oracle exactly like
// any other run's.
func TestApplyRunWorkflowResultLoadsVerdictsAndOracleCoverageIntoTheRunRecord(t *testing.T) {
	dataDir := t.TempDir()
	workspace := t.TempDir()
	const conformityEvidence = `{"schema_version":1,"succeeded":true,"review_verdicts":[{"criterion":"1. Returns 200.","verdict":"clean"},{"criterion":"2. Clean code.","verdict":"clean"}]}`
	if err := os.WriteFile(filepath.Join(workspace, "CONFORMITY_EVIDENCE.json"), []byte(conformityEvidence), 0o644); err != nil {
		t.Fatal(err)
	}
	oracleDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(oracleDir, "MANIFEST.json"),
		[]byte(`[{"criterion":"1. Returns 200.","oracle_file":"o.go","rationale":"x"},{"criterion":"2. Clean code.","oracle_file":null,"rationale":"y"}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	r := &run.Run{ID: "run-verdicts", Ticket: "fixture-ticket", ReferenceOracleDir: oracleDir, CreatedAt: time.Now().Format(time.RFC3339Nano)}
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}

	result := workflow.RunWorkflowResult{State: run.StateQuarantined, SpecConformityConfigured: true}
	_ = applyRunWorkflowResult(newTestDeps(t), r, dataDir, r.ID, r.Ticket, workspace, "basesha", "task-queue", result, true, &release.MergePolicy{}, forge.GHPullRequestOpener{}, false)

	reloaded, err := run.Load(dataDir, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(reloaded.SpecConformityVerdicts) != 2 {
		t.Fatalf("persisted SpecConformityVerdicts = %+v, want the 2 verdicts from CONFORMITY_EVIDENCE.json", reloaded.SpecConformityVerdicts)
	}
	if len(reloaded.OracleCoveredCriteria) != 1 || reloaded.OracleCoveredCriteria[0] != "1. Returns 200." {
		t.Fatalf("persisted OracleCoveredCriteria = %v, want only the oracle-backed criterion", reloaded.OracleCoveredCriteria)
	}
}

// A reclaimed run (attributeWorkspaceEvidence=false) must not attribute
// workspace conformity evidence to itself -- but it still knows its own
// oracle directory from the run record, so coverage is still recorded.
func TestApplyRunWorkflowResultReclaimedRunLoadsCoverageButNotVerdicts(t *testing.T) {
	dataDir := t.TempDir()
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "CONFORMITY_EVIDENCE.json"),
		[]byte(`{"review_verdicts":[{"criterion":"1. x","verdict":"clean"}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	oracleDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(oracleDir, "MANIFEST.json"),
		[]byte(`[{"criterion":"1. x","oracle_file":"o.go","rationale":"x"}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	r := &run.Run{ID: "run-reclaimed", Ticket: "fixture-ticket", ReferenceOracleDir: oracleDir, CreatedAt: time.Now().Format(time.RFC3339Nano)}
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}

	result := workflow.RunWorkflowResult{State: run.StateQuarantined, SpecConformityConfigured: true}
	_ = applyRunWorkflowResult(newTestDeps(t), r, dataDir, r.ID, r.Ticket, workspace, "basesha", "task-queue", result, false, &release.MergePolicy{}, forge.GHPullRequestOpener{}, false)

	reloaded, err := run.Load(dataDir, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(reloaded.SpecConformityVerdicts) != 0 {
		t.Errorf("reclaimed run attributed workspace verdicts to itself: %+v", reloaded.SpecConformityVerdicts)
	}
	if len(reloaded.OracleCoveredCriteria) != 1 {
		t.Errorf("reclaimed run OracleCoveredCriteria = %v, want its coverage from the run record's own oracle dir", reloaded.OracleCoveredCriteria)
	}
}

// TestApplyRunWorkflowResultSkipsConformityEvidenceWhenNotConfigured
// covers the common case, where SpecConformityConfigured is false because
// the ticket never declared -spec-acceptance-criteria: no
// CONFORMITY_EVIDENCE.json was ever written, and this must not log a
// spurious "could not retain" warning for every such run.
// not parallel-safe: redirects the global log package's output (log.SetOutput)
func TestApplyRunWorkflowResultSkipsConformityEvidenceWhenNotConfigured(t *testing.T) {
	dataDir := t.TempDir()
	workspace := t.TempDir()
	r := &run.Run{ID: "run-no-conformity", Ticket: "fixture-ticket", CreatedAt: time.Now().Format(time.RFC3339Nano)}
	if err := r.Save(dataDir); err != nil {
		t.Fatalf("seed run: %v", err)
	}

	var logBuf bytes.Buffer
	previousOutput := log.Writer()
	log.SetOutput(&logBuf)
	t.Cleanup(func() { log.SetOutput(previousOutput) })

	result := workflow.RunWorkflowResult{State: run.StateQuarantined}
	_ = applyRunWorkflowResult(newTestDeps(t), r, dataDir, r.ID, r.Ticket, workspace, "basesha", "task-queue", result, true, &release.MergePolicy{}, forge.GHPullRequestOpener{}, false)
	if _, err := os.Stat(filepath.Join(run.Dir(dataDir, r.ID), "CONFORMITY_EVIDENCE.json")); !os.IsNotExist(err) {
		t.Fatalf("CONFORMITY_EVIDENCE.json retained/stat err = %v, want it to not exist", err)
	}
	if strings.Contains(logBuf.String(), "could not retain CONFORMITY_EVIDENCE.json") {
		t.Errorf("logs = %q, want no retention warning when SpecConformityConfigured is false", logBuf.String())
	}
}

// TestAlreadyReconciledResultErrorPreservesQuarantine is the regression
// test for a real P1 finding from codex review: runViaRepositoryOwner's
// own final-result lock closure returned nil unconditionally whenever a
// concurrent daemon reconciliation had already brought this exact request
// to a terminal state — even when that terminal state was quarantined, not
// accepted. That silently broke applyRunWorkflowResult's own contract of a
// non-nil error for anything other than StateAccepted: the submitting
// CLI/API would report success for a run the policy gate actually
// rejected.
func TestAlreadyReconciledResultErrorPreservesQuarantine(t *testing.T) {
	t.Parallel()
	r := &run.Run{
		State:         run.StateQuarantined,
		Notifications: []run.NotificationRecord{{Reason: "policy gate did not pass: canonical_verify"}},
	}
	err := alreadyReconciledResultError(r)
	if err == nil {
		t.Fatal("alreadyReconciledResultError(quarantined) = nil, want a non-nil error")
	}
	if !strings.Contains(err.Error(), "policy gate did not pass: canonical_verify") {
		t.Fatalf("error = %q, want it to carry the quarantine reason", err.Error())
	}
}

func TestAlreadyReconciledResultErrorAccepts(t *testing.T) {
	t.Parallel()
	if err := alreadyReconciledResultError(&run.Run{State: run.StateAccepted}); err != nil {
		t.Fatalf("alreadyReconciledResultError(accepted) = %v, want nil", err)
	}
}

func TestAlreadyReconciledResultErrorReportsOtherTerminalStates(t *testing.T) {
	t.Parallel()
	err := alreadyReconciledResultError(&run.Run{State: run.StateHalted})
	if err == nil {
		t.Fatal("alreadyReconciledResultError(halted) = nil, want a non-nil error")
	}
	if !strings.Contains(err.Error(), string(run.StateHalted)) {
		t.Fatalf("error = %q, want it to name the reconciled state", err.Error())
	}
}

// TestRunsNeedingReclaimIncludesUnconfirmedHalt is the regression test for
// a real P1 finding from codex review: a StateHalted run whose halt is
// unconfirmed (its submitter gave up waiting and could not confirm its own
// best-effort child termination succeeded — see run.Run.HaltConfirmed's
// doc comment) was previously excluded here identically to a genuinely
// confirmed halt, so a reclaim scan never started a recovery Worker for it
// even though the underlying Temporal child might still be running with
// nothing polling its queue. A confirmed halt must still be excluded.
func TestRunsNeedingReclaimIncludesUnconfirmedHalt(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	unconfirmed := &run.Run{ID: "run-halted-unconfirmed", State: run.StateHalted, HaltConfirmed: false, CreatedAt: time.Now().Format(time.RFC3339Nano)}
	if err := unconfirmed.Save(dataDir); err != nil {
		t.Fatalf("seed unconfirmed halt: %v", err)
	}
	confirmed := &run.Run{ID: "run-halted-confirmed", State: run.StateHalted, HaltConfirmed: true, CreatedAt: time.Now().Format(time.RFC3339Nano)}
	if err := confirmed.Save(dataDir); err != nil {
		t.Fatalf("seed confirmed halt: %v", err)
	}

	ids, err := runsNeedingReclaim(dataDir, "", nil)
	if err != nil {
		t.Fatalf("runsNeedingReclaim: %v", err)
	}
	if !slices.Equal(ids, []string{"run-halted-unconfirmed"}) {
		t.Fatalf("ids = %v, want [run-halted-unconfirmed]: a confirmed halt must stay excluded, an unconfirmed one must now be reclaimed", ids)
	}
}

// TestTerminalReclaimedRunIDsExcludesUnconfirmedHalt is
// TestRunsNeedingReclaimIncludesUnconfirmedHalt's mirror image on the
// stop-the-recovery-Worker side: an unconfirmed halt must not be treated as
// terminal either, or a periodic re-scan would immediately stop the very
// recovery Worker just started for it.
func TestTerminalReclaimedRunIDsExcludesUnconfirmedHalt(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	seed := func(id string, state run.State, confirmed bool) {
		t.Helper()
		r := &run.Run{ID: id, State: state, HaltConfirmed: confirmed, CreatedAt: time.Now().Format(time.RFC3339Nano)}
		if err := r.Save(dataDir); err != nil {
			t.Fatalf("seed run %s: %v", id, err)
		}
	}
	seed("run-halted-unconfirmed", run.StateHalted, false)
	seed("run-halted-confirmed", run.StateHalted, true)

	reclaimed := map[string]bool{"run-halted-unconfirmed": true, "run-halted-confirmed": true}
	ids := terminalReclaimedRunIDs(dataDir, reclaimed)
	if !slices.Equal(ids, []string{"run-halted-confirmed"}) {
		t.Fatalf("ids = %v, want [run-halted-confirmed]: an unconfirmed halt must not count as terminal yet", ids)
	}
}

func TestTerminalReclaimedRunIDsSkipsUnloadableRuns(t *testing.T) {
	t.Parallel()
	// A run.json that can't be loaded (deleted, corrupt, mid-write) must be
	// treated as "not yet terminal" — the conservative direction, since
	// prematurely stopping a Worker still servicing a run in progress
	// would strand it exactly like the crash reclaim exists to recover
	// from.
	ids := terminalReclaimedRunIDs(t.TempDir(), map[string]bool{"run-does-not-exist": true})
	if len(ids) != 0 {
		t.Fatalf("ids = %v, want none", ids)
	}
}

func TestRunsNeedingReclaimOnMissingRunsDirectoryIsEmptyNotError(t *testing.T) {
	t.Parallel()
	ids, err := runsNeedingReclaim(filepath.Join(t.TempDir(), "does-not-exist"), "", nil)
	if err != nil {
		t.Fatalf("runsNeedingReclaim: %v, want no error for a data dir with no runs subdirectory yet", err)
	}
	if len(ids) != 0 {
		t.Fatalf("ids = %v, want none", ids)
	}
}

// TestScratchRunInFlightKeepsUnconfirmedHalt pins the predicate every
// scratch-cache removal shares: an unconfirmed halt (a Temporal submitter
// that gave up waiting while a daemon-side Worker may still be building)
// is in flight; a confirmed halt, an accepted run, or a missing record is
// not.
func TestScratchRunInFlightKeepsUnconfirmedHalt(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	for _, r := range []*run.Run{
		{ID: "verifying", State: run.StateVerifying},
		{ID: "unconfirmed-halt", State: run.StateHalted},
		{ID: "confirmed-halt", State: run.StateHalted, HaltConfirmed: true},
		{ID: "accepted", State: run.StateAccepted},
	} {
		r.CreatedAt = time.Now().Format(time.RFC3339Nano)
		if err := r.Save(dataDir); err != nil {
			t.Fatalf("seed run %s: %v", r.ID, err)
		}
	}
	inFlight := scratchRunInFlight(dataDir)
	for id, want := range map[string]bool{"verifying": true, "unconfirmed-halt": true, "confirmed-halt": false, "accepted": false, "missing": false} {
		if got := inFlight(id); got != want {
			t.Errorf("scratchRunInFlight(%q) = %v, want %v", id, got, want)
		}
	}
}
