package main

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"buildgate/internal/requestdriver/requestdrivertest"
	"buildgate/internal/run"
	"buildgate/internal/sandbox"
	wsisolation "buildgate/internal/workspace"
)

// factorydPackageSource concatenates every non-test .go source file in
// this package (cmd/factoryd), for a static-source-guard test that
// locates a specific top-level function or statement by name/pattern.
// Reading a single hardcoded file (an earlier version of every caller
// below read only "main.go") breaks the moment that function moves to a
// different file in the same package -- exactly what a same-package,
// multiple-file modularization does routinely, on purpose, without
// changing any behavior these guards actually care about. Concatenating
// the whole package instead means a guard keeps working regardless of
// which file its target function currently lives in.
func factorydPackageSource(t *testing.T) string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read cmd/factoryd directory: %v", err)
	}
	var text strings.Builder
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		b, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		text.Write(b)
		text.WriteByte('\n')
	}
	return text.String()
}

// TestNeverReadsAgentAuthoredEvidence is a static guard for the plan's
// flagship invariant: "No stage may advance on an agent's self-report."
// BUILD_REPORT.md is agent-authored prose and main.go must only ever
// mention its path for a human-facing print line, never open/read it to
// decide anything. This is cheap and directly protects the invariant the
// rest of this test suite otherwise only protects indirectly.
func TestNeverReadsAgentAuthoredEvidence(t *testing.T) {
	t.Parallel()
	text := factorydPackageSource(t)

	readCalls := regexp.MustCompile(`os\.(ReadFile|Open)\([^)]*BUILD_REPORT`)
	if readCalls.MatchString(text) {
		t.Fatal("main.go reads BUILD_REPORT.md directly — agent prose must never decide pass/fail")
	}

	// At most one legitimate occurrence: a single fmt.Printf line that both
	// names the file and joins its path for display — two textual
	// occurrences of the literal, both inside that one print call.
	occurrences := strings.Count(text, "BUILD_REPORT.md")
	if occurrences > 2 {
		t.Fatalf("BUILD_REPORT.md referenced %d times; verify every reference is still print-only, not read as a signal", occurrences)
	}
}

// TestEvidenceReadOccursAfterFinalStateAssignment is a static guard for
// BUILD_EVIDENCE.json's non-gating role: even though factoryd may read this
// structured harness output (via the shared loadAgentEvidence helper, called
// from applyRunWorkflowResult, which every Temporal-routed run converges on
// — found live in review: the Temporal path originally never called it at
// all, leaving r.AgentEvidence always empty), each call must remain structurally downstream of
// every terminal-state decision in its own calling function, and the helper
// itself must never assign r.State.
func TestEvidenceReadOccursAfterFinalStateAssignment(t *testing.T) {
	t.Parallel()
	text := factorydPackageSource(t)

	funcStarts := regexp.MustCompile(`(?m)^func (\w+)\(`).FindAllStringSubmatchIndex(text, -1)
	if len(funcStarts) == 0 {
		t.Fatal("cmd/factoryd package has no top-level func declarations; static evidence-order guard cannot locate function bodies")
	}
	// enclosingFunc returns the name and [start,end) source range of the
	// top-level function containing source offset off.
	enclosingFunc := func(off int) (name string, start, end int) {
		end = len(text)
		for i, m := range funcStarts {
			if m[0] > off {
				break
			}
			start, name = m[0], text[m[2]:m[3]]
			if i+1 < len(funcStarts) {
				end = funcStarts[i+1][0]
			} else {
				end = len(text)
			}
		}
		return name, start, end
	}

	stateAssignments := regexp.MustCompile(`(?m)^\s*r\.State\s*=`).FindAllStringIndex(text, -1)
	if len(stateAssignments) == 0 {
		t.Fatal("cmd/factoryd package has no r.State assignments; static evidence-order guard cannot locate the gate decision")
	}

	// Call sites only, not the "func loadAgentEvidence(r *run.Run, ..."
	// definition itself, which always declares r's type right after it.
	callSites := regexp.MustCompile(`loadAgentEvidence\(r,`).FindAllStringIndex(text, -1)
	if len(callSites) < 1 {
		t.Fatalf("expected loadAgentEvidence to be called from applyRunWorkflowResult, found %d call site(s)", len(callSites))
	}
	seenFuncs := map[string]bool{}
	for _, call := range callSites {
		fnName, fnStart, fnEnd := enclosingFunc(call[0])
		var lastStateInFunc = -1
		for _, sa := range stateAssignments {
			if sa[0] >= fnStart && sa[0] < fnEnd {
				lastStateInFunc = sa[0]
			}
		}
		if lastStateInFunc == -1 {
			t.Fatalf("function %q calls loadAgentEvidence but has no r.State assignment to order it against", fnName)
		}
		if call[0] <= lastStateInFunc {
			t.Fatalf("loadAgentEvidence call in %q at offset %d must occur after its last r.State assignment at offset %d", fnName, call[0], lastStateInFunc)
		}
		seenFuncs[fnName] = true
	}
	if len(seenFuncs) < 1 {
		t.Fatalf("expected loadAgentEvidence calls ordered correctly in at least one function, verified in %v", seenFuncs)
	}

	defLoc := regexp.MustCompile(`(?m)^func loadAgentEvidence\(`).FindStringIndex(text)
	if defLoc == nil {
		t.Fatal("cmd/factoryd package has no loadAgentEvidence definition; static evidence-order guard cannot locate the shared helper")
	}
	_, defStart, defEnd := enclosingFunc(defLoc[0])
	if regexp.MustCompile(`r\.State\s*=`).MatchString(text[defStart:defEnd]) {
		t.Fatal("loadAgentEvidence must not assign r.State — it is best-effort agent evidence attachment, never the gate")
	}
}

// TestValidateSandboxResourceLimitFlags is the regression test for real P2
// findings from two GitHub Codex App review rounds on PR #52. Round one:
// an empty -sandbox-memory/-sandbox-cpus/-sandbox-tmpfs-size parsed
// successfully as a flag and was then silently replaced by
// workflow.Activities' own zero-value fallback ("4g"/"2"/"256m") -- so an operator's
// actually-invalid configuration appeared to take effect while the worker
// ran with a materially different ceiling than requested, instead of
// failing loudly at startup. Round two: even a non-empty zero-valued
// -sandbox-memory/-sandbox-cpus (e.g. "0" or "0g") passed round one's
// check yet still means "no limit" to Docker itself, so an operator's
// apparently-valid configuration could still launch an unbounded worker.
func TestValidateSandboxResourceLimitFlags(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name                    string
		memory, cpus, tmpfsSize string
		wantErrContains         string
	}{
		{name: "valid", memory: "4g", cpus: "2", tmpfsSize: "256m", wantErrContains: ""},
		{name: "valid fractional cpus", memory: "4g", cpus: "0.5", tmpfsSize: "256m", wantErrContains: ""},
		{name: "empty memory", memory: "", cpus: "2", tmpfsSize: "256m", wantErrContains: "-sandbox-memory must not be empty"},
		{name: "empty cpus", memory: "4g", cpus: "", tmpfsSize: "256m", wantErrContains: "-sandbox-cpus must not be empty"},
		{name: "empty tmpfs size", memory: "4g", cpus: "2", tmpfsSize: "", wantErrContains: "-sandbox-tmpfs-size must not be empty"},
		{name: "zero memory bare", memory: "0", cpus: "2", tmpfsSize: "256m", wantErrContains: "-sandbox-memory must be a positive value"},
		{name: "zero memory with unit", memory: "0g", cpus: "2", tmpfsSize: "256m", wantErrContains: "-sandbox-memory must be a positive value"},
		{name: "malformed memory", memory: "not-a-size", cpus: "2", tmpfsSize: "256m", wantErrContains: "-sandbox-memory must be a positive integer with an optional b/k/m/g suffix"},
		{name: "memory below Docker's 6m minimum", memory: "1m", cpus: "2", tmpfsSize: "256m", wantErrContains: "-sandbox-memory must be at least 6m"},
		{name: "memory exactly at Docker's 6m minimum", memory: "6m", cpus: "2", tmpfsSize: "256m", wantErrContains: ""},
		{name: "zero cpus", memory: "4g", cpus: "0", tmpfsSize: "256m", wantErrContains: "-sandbox-cpus must be greater than zero"},
		{name: "negative cpus", memory: "4g", cpus: "-1", tmpfsSize: "256m", wantErrContains: "-sandbox-cpus must be greater than zero"},
		{name: "malformed cpus", memory: "4g", cpus: "not-a-number", tmpfsSize: "256m", wantErrContains: "-sandbox-cpus must be a finite decimal number"},
		{name: "NaN cpus", memory: "4g", cpus: "NaN", tmpfsSize: "256m", wantErrContains: "-sandbox-cpus must be a finite decimal number"},
		{name: "Inf cpus", memory: "4g", cpus: "+Inf", tmpfsSize: "256m", wantErrContains: "-sandbox-cpus must be a finite decimal number"},
		{name: "zero tmpfs size", memory: "4g", cpus: "2", tmpfsSize: "0", wantErrContains: "-sandbox-tmpfs-size must be a positive value"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateSandboxResourceLimitFlags("-sandbox", test.memory, test.cpus, test.tmpfsSize)
			if test.wantErrContains == "" {
				if err != nil {
					t.Fatalf("valid limits: want nil error, got %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.wantErrContains) {
				t.Fatalf("error = %v, want it to contain %q", err, test.wantErrContains)
			}
		})
	}
}

// TestRunMainWithReadyRejectsInvalidSandboxResourceLimit proves the
// direct/Temporal/repository-owner path's own -sandbox-cpus validation is
// wired into runMainWithReady's actual startup, before it ever touches
// -workspace/-spec, so a plainly invalid value fails fast rather than
// surfacing much later as an infrastructure failure deep in a sandboxed
// attempt.
func TestRunMainWithReadyRejectsInvalidSandboxResourceLimit(t *testing.T) {
	dp := newTestDeps(t)
	path := isolateSessionConfig(t)
	writeSessionConfig(t, path, "sandbox_cpus: \"0\"\n")
	// An explicit, offline -build-app-script: routes:/models:/roles: is
	// the only session-config schema and this fixture configures none,
	// so a default, model-backed build would fail the earlier "requires a
	// relay" gate first (relayNeededForExecution) -- irrelevant to what
	// this test actually checks, the -sandbox-cpus validation's own
	// place in startup ordering, which still runs before -workspace/-spec
	// are ever touched either way.
	err := runMainWithReady(dp, context.Background(), []string{
		"-ticket", "t", "-workspace", "/does/not/exist", "-spec", "/does/not/exist",
		"-build-app-interpreter", "/bin/sh", "-build-app-script", "/does/not/exist/offline_build.py",
	}, nil)
	if err == nil || !strings.Contains(err.Error(), "-sandbox-cpus must be greater than zero") {
		t.Fatalf("runMainWithReady with sandbox_cpus: 0: error = %v, want it to name the invalid -sandbox-cpus flag", err)
	}
}

// TestServeMainRejectsInvalidSandboxResourceLimitFlags proves serve's own
// -sandbox-* flags (the ceiling every API-started run gets) are validated
// at startup, before ever binding a listener.
func TestServeMainRejectsInvalidSandboxResourceLimitFlags(t *testing.T) {
	dp := newTestDeps(t)
	if err := serveMain(dp, []string{"-data-dir", t.TempDir(), "-sandbox-cpus", "0"}); err == nil || !strings.Contains(err.Error(), "-sandbox-cpus must be greater than zero") {
		t.Fatalf("serveMain with -sandbox-cpus=0: error = %v, want it to name the invalid -sandbox-cpus flag", err)
	}
}

func testHeldIsolationLock(t *testing.T, repoDir string) *wsisolation.DirectLock {
	t.Helper()
	lock, err := wsisolation.AcquireDirectLock(repoDir)
	if err != nil {
		t.Fatalf("acquire test repository lock: %v", err)
	}
	t.Cleanup(func() { _ = lock.Close() })
	return lock
}

func TestReconcileIsolationMarkersReapsNonterminalDirectRun(t *testing.T) {
	// not parallel: newFixtureRepo (testfixture.NewGitRepo) calls t.Setenv internally.
	repoDir := newFixtureRepo(t)
	dataDir := t.TempDir()
	lock := testHeldIsolationLock(t, repoDir)
	marker := requestdrivertest.IsolationMarker(t, repoDir, dataDir, "direct-crash", "direct")
	if err := (&run.Run{ID: marker.RunID, State: run.StateSliceRunning, ProjectPath: repoDir, WorkspacePath: repoDir}).Save(dataDir); err != nil {
		t.Fatalf("save nonterminal run: %v", err)
	}
	if err := reconcileIsolationMarkers(context.Background(), dataDir, repoDir, "", lock, nil); err != nil {
		t.Fatalf("reconcileIsolationMarkers returned an error for a clean reap: %v", err)
	}
	if _, err := os.Stat(marker.WorktreePath); !os.IsNotExist(err) {
		t.Fatalf("worktree still exists after reconciliation: %v", err)
	}
	if _, err := os.Stat(wsisolation.IsolationMarkerPath(dataDir, marker.RunID)); !os.IsNotExist(err) {
		t.Fatalf("marker still exists after reconciliation: %v", err)
	}
	// Reconciliation is idempotent when the marker has already been removed.
	if err := reconcileIsolationMarkers(context.Background(), dataDir, repoDir, "", lock, nil); err != nil {
		t.Fatalf("reconcileIsolationMarkers returned an error on an already-clean idempotent pass: %v", err)
	}
}

// TestReconcileIsolationMarkersPropagatesReapFailure proves the returned
// error surfaces a genuine reap failure (found via review: the standalone
// `factoryd reconcile` command used to unconditionally return nil, so an
// operator or script could never tell a real cleanup failure from
// success). The worktree is re-registered under a different branch than
// the marker declares after Prepare creates it, the same
// "worktree exists but points somewhere unexpected" condition
// ReapIsolationMarker's own safety check refuses, deterministically
// forcing the reap this marker otherwise qualifies for to fail.
func TestReconcileIsolationMarkersPropagatesReapFailure(t *testing.T) {
	// not parallel: newFixtureRepo (testfixture.NewGitRepo) calls t.Setenv internally.
	repoDir := newFixtureRepo(t)
	dataDir := t.TempDir()
	lock := testHeldIsolationLock(t, repoDir)
	marker := requestdrivertest.IsolationMarker(t, repoDir, dataDir, "reap-failure", "direct")
	if err := (&run.Run{ID: marker.RunID, State: run.StateSliceRunning, ProjectPath: repoDir, WorkspacePath: repoDir}).Save(dataDir); err != nil {
		t.Fatalf("save nonterminal run: %v", err)
	}
	if out, err := exec.Command("git", "-C", repoDir, "worktree", "remove", "--force", marker.WorktreePath).CombinedOutput(); err != nil {
		t.Fatalf("detach worktree: %v: %s", err, out)
	}
	if out, err := exec.Command("git", "-C", repoDir, "worktree", "add", "-b", "reap-failure-different-branch", marker.WorktreePath).CombinedOutput(); err != nil {
		t.Fatalf("re-register worktree under a different branch: %v: %s", err, out)
	}
	err := reconcileIsolationMarkers(context.Background(), dataDir, repoDir, "", lock, nil)
	if err == nil {
		t.Fatal("expected reconcileIsolationMarkers to return an error for a forced reap failure")
	}
	if !strings.Contains(err.Error(), "reap marker") {
		t.Errorf("error = %q, want it to name the reap failure", err)
	}
	if _, statErr := os.Stat(marker.WorktreePath); statErr != nil {
		t.Errorf("worktree should still exist after a refused reap: %v", statErr)
	}
}

// TestReconcileIsolationMarkersPreservesWhenSandboxDockerUnresolvable
// proves an inability to check for a live sandbox container preserves the
// marker rather than assuming none exists (found via review, round 2: an
// earlier version treated -sandbox-docker not being resolvable on this
// machine as "sandboxing was never in play" and skipped the check
// entirely -- but a prior sandboxed run's container can still be alive
// and holding the checkout even if this invocation's own environment
// can't currently reach Docker to prove it, and there is no reliable
// signal, for a run with no completed attempt yet, that it was never
// sandboxed to begin with). Passes a nonexistent Docker executable name,
// not "" -- "" is the operator's own explicit, deliberate opt-out (see
// reconcileMain's own flag help) and is exercised by every other
// reconcile test in this file via -sandbox-docker=""; this test is
// specifically about the unresolvable-but-configured case.
func TestReconcileIsolationMarkersPreservesWhenSandboxDockerUnresolvable(t *testing.T) {
	// not parallel: newFixtureRepo (testfixture.NewGitRepo) calls t.Setenv internally.
	repoDir := newFixtureRepo(t)
	dataDir := t.TempDir()
	lock := testHeldIsolationLock(t, repoDir)
	marker := requestdrivertest.IsolationMarker(t, repoDir, dataDir, "docker-unresolvable", "direct")
	if err := (&run.Run{ID: marker.RunID, State: run.StateSliceRunning, ProjectPath: repoDir, WorkspacePath: repoDir}).Save(dataDir); err != nil {
		t.Fatalf("save nonterminal run: %v", err)
	}
	// No live runner process for this marker either (liveRunnerProcessPID
	// finds no pid file, correctly returning "not running"), so the
	// sandbox-container check below is the only thing standing between
	// this marker and a reap -- isolating exactly what this test means to
	// prove.
	err := reconcileIsolationMarkers(context.Background(), dataDir, repoDir, "definitely-not-a-real-docker-binary-xyz", lock, nil)
	if err != nil {
		t.Fatalf("reconcileIsolationMarkers returned an error for a preserved (not reaped) marker: %v", err)
	}
	if _, statErr := os.Stat(marker.WorktreePath); statErr != nil {
		t.Errorf("worktree should still exist -- Docker liveness was inconclusive, not proof of absence: %v", statErr)
	}
	if _, statErr := os.Stat(wsisolation.IsolationMarkerPath(dataDir, marker.RunID)); statErr != nil {
		t.Errorf("marker should still exist -- Docker liveness was inconclusive, not proof of absence: %v", statErr)
	}
}

func TestReconcileIsolationMarkersRequiresHeldRepositoryLock(t *testing.T) {
	// not parallel: newFixtureRepo (testfixture.NewGitRepo) calls t.Setenv internally.
	repoDir := newFixtureRepo(t)
	dataDir := t.TempDir()
	marker := requestdrivertest.IsolationMarker(t, repoDir, dataDir, "lock-required", "direct")
	reconcileIsolationMarkers(context.Background(), dataDir, repoDir, "", nil, nil)
	if _, err := os.Stat(marker.WorktreePath); err != nil {
		t.Fatalf("reconciliation without lock removed worktree: %v", err)
	}
	lock := testHeldIsolationLock(t, repoDir)
	reconcileIsolationMarkers(context.Background(), dataDir, repoDir, "", lock, nil)
	if _, err := os.Stat(marker.WorktreePath); !os.IsNotExist(err) {
		t.Fatalf("reconciliation with lock did not reap worktree: %v", err)
	}
}

func TestReconcileIsolationMarkersPreservesAcceptedAndCompletedTemporalWork(t *testing.T) {
	// not parallel: newFixtureRepo (testfixture.NewGitRepo) calls t.Setenv internally.
	repoDir := newFixtureRepo(t)
	dataDir := t.TempDir()
	lock := testHeldIsolationLock(t, repoDir)
	accepted := requestdrivertest.IsolationMarker(t, repoDir, dataDir, "accepted-run", "direct")
	if err := (&run.Run{ID: accepted.RunID, State: run.StateAccepted, ProjectPath: repoDir}).Save(dataDir); err != nil {
		t.Fatalf("save accepted run: %v", err)
	}
	quarantined := requestdrivertest.IsolationMarker(t, repoDir, dataDir, "quarantined-run", "direct")
	if err := (&run.Run{ID: quarantined.RunID, State: run.StateQuarantined, ProjectPath: repoDir}).Save(dataDir); err != nil {
		t.Fatalf("save quarantined run: %v", err)
	}
	temporal := requestdrivertest.IsolationMarker(t, repoDir, dataDir, "temporal-complete", "temporal")
	if err := wsisolation.RemoveIsolationMarker(wsisolation.IsolationMarkerPath(dataDir, temporal.RunID)); err != nil {
		t.Fatalf("remove temporary marker path: %v", err)
	}
	temporal.RunID = "durable-temporal-complete"
	temporal.WorkflowID = "workflow-temporal-complete"
	temporal.ActivityRunID = "activity-run"
	temporal.ActivityID = "prepare-activity"
	if err := wsisolation.WriteIsolationMarker(wsisolation.IsolationMarkerPath(dataDir, temporal.RunID), temporal); err != nil {
		t.Fatalf("write durable temporal marker: %v", err)
	}
	if err := (&run.Run{ID: temporal.RunID, State: run.StateSliceRunning, ProjectPath: repoDir}).Save(dataDir); err != nil {
		t.Fatalf("save temporal run: %v", err)
	}
	checkpointDir := temporal.CheckpointDir
	if err := os.MkdirAll(filepath.Join(checkpointDir, "activity-checkpoints"), 0o750); err != nil {
		t.Fatalf("create checkpoint directory: %v", err)
	}
	key := sha256.Sum256([]byte(fmt.Sprintf("%d:%s%d:%s%d:%s", len(temporal.WorkflowID), temporal.WorkflowID, len(temporal.ActivityRunID), temporal.ActivityRunID, len(temporal.ActivityID), temporal.ActivityID)))
	checkpointPath := filepath.Join(checkpointDir, "activity-checkpoints", fmt.Sprintf("%x.json", key))
	checkpoint := `{"completed":true,"schema_version":2,"workflow_id":"` + temporal.WorkflowID + `","run_id":"` + temporal.ActivityRunID + `","activity_id":"` + temporal.ActivityID + `","result":{"worktree_path":"` + temporal.WorktreePath + `","branch":"` + temporal.Branch + `"}}`
	if err := os.WriteFile(checkpointPath, []byte(checkpoint), 0o600); err != nil {
		t.Fatalf("write prepare checkpoint: %v", err)
	}
	reconcileIsolationMarkersWith(context.Background(), dataDir, repoDir, "", lock, func(context.Context, string) bool { return false })
	for _, marker := range []wsisolation.IsolationMarker{accepted, quarantined, temporal} {
		if _, err := os.Stat(marker.WorktreePath); err != nil {
			t.Errorf("preserved worktree %s missing: %v", marker.RunID, err)
		}
		if _, err := os.Stat(wsisolation.IsolationMarkerPath(dataDir, marker.RunID)); err != nil {
			t.Errorf("preserved marker %s missing: %v", marker.RunID, err)
		}
		if err := wsisolation.Remove(repoDir, marker.WorktreePath, marker.Branch); err != nil {
			t.Errorf("cleanup %s: %v", marker.RunID, err)
		}
	}
}

// TestReconcileIsolationMarkersRevokesWorkerGroupWriteOnPreservedTerminalRun
// is the regression test for a real gap found via GitHub Codex App review of
// PR #62 (P2): runSandboxWithRetries's own defer is what normally revokes
// EnableWorkerGroupWrite's grant once a run reaches a terminal state and its
// worktree is kept rather than rolled back, but a factoryd process killed
// after the terminal run.json save and before that defer runs left the
// grant active forever -- reconciliation on restart previously preserved a
// terminal run's worktree unconditionally, without ever revoking it, so
// every process sharing factoryd's own GID kept write access to a
// host-owned worktree indefinitely.
func TestReconcileIsolationMarkersRevokesWorkerGroupWriteOnPreservedTerminalRun(t *testing.T) {
	// not parallel: newFixtureRepo (testfixture.NewGitRepo) calls t.Setenv internally.
	repoDir := newFixtureRepo(t)
	dataDir := t.TempDir()
	lock := testHeldIsolationLock(t, repoDir)
	accepted := requestdrivertest.IsolationMarker(t, repoDir, dataDir, "accepted-ungranted", "direct")
	if err := (&run.Run{ID: accepted.RunID, State: run.StateAccepted, ProjectPath: repoDir}).Save(dataDir); err != nil {
		t.Fatalf("save accepted run: %v", err)
	}
	if err := wsisolation.EnableWorkerGroupWrite(accepted.WorktreePath, os.Getgid()); err != nil {
		t.Fatalf("simulate un-revoked grant: %v", err)
	}
	info, err := os.Stat(accepted.WorktreePath)
	if err != nil {
		t.Fatalf("stat worktree before reconciliation: %v", err)
	}
	if info.Mode().Perm()&0o020 == 0 {
		t.Fatalf("precondition failed: worktree %s is not group-writable before reconciliation", accepted.WorktreePath)
	}
	reconcileIsolationMarkersWith(context.Background(), dataDir, repoDir, "", lock, func(context.Context, string) bool { return false })
	info, err = os.Stat(accepted.WorktreePath)
	if err != nil {
		t.Fatalf("stat preserved worktree: %v", err)
	}
	if info.Mode().Perm()&0o070 != 0 {
		t.Errorf("reconciliation left worktree %s group-accessible (mode %o), want group revoked", accepted.WorktreePath, info.Mode().Perm())
	}
	if _, err := os.Stat(wsisolation.IsolationMarkerPath(dataDir, accepted.RunID)); err != nil {
		t.Errorf("preserved marker missing: %v", err)
	}
}

func TestReconcileIsolationMarkersPreservesMalformedAndLegacyEvidence(t *testing.T) {
	// not parallel: newFixtureRepo (testfixture.NewGitRepo) calls t.Setenv internally.
	repoDir := newFixtureRepo(t)
	dataDir := t.TempDir()
	lock := testHeldIsolationLock(t, repoDir)
	marker := requestdrivertest.IsolationMarker(t, repoDir, dataDir, "malformed-marker", "direct")
	marker.ParentDir = t.TempDir()
	if err := wsisolation.WriteIsolationMarker(wsisolation.IsolationMarkerPath(dataDir, marker.RunID), marker); err != nil {
		t.Fatalf("write escaped marker: %v", err)
	}
	legacy := requestdrivertest.IsolationMarker(t, repoDir, dataDir, "legacy-marker", "direct")
	legacy.Version = 99
	if err := wsisolation.WriteIsolationMarker(wsisolation.IsolationMarkerPath(dataDir, legacy.RunID), legacy); err != nil {
		t.Fatalf("write legacy marker: %v", err)
	}
	reconcileIsolationMarkers(context.Background(), dataDir, repoDir, "", lock, nil)
	for _, marker := range []wsisolation.IsolationMarker{marker, legacy} {
		if _, err := os.Stat(marker.WorktreePath); err != nil {
			t.Errorf("preserved worktree %s missing: %v", marker.RunID, err)
		}
		if err := wsisolation.Remove(repoDir, marker.WorktreePath, marker.Branch); err != nil {
			t.Errorf("cleanup %s: %v", marker.RunID, err)
		}
	}
}

func TestReconcileIsolationMarkersReapsTemporalIntentWithoutCheckpoint(t *testing.T) {
	// not parallel: newFixtureRepo (testfixture.NewGitRepo) calls t.Setenv internally.
	repoDir := newFixtureRepo(t)
	dataDir := t.TempDir()
	lock := testHeldIsolationLock(t, repoDir)
	marker := requestdrivertest.IsolationMarker(t, repoDir, dataDir, "temporal-intent", "temporal")
	reconcileIsolationMarkersWith(context.Background(), dataDir, repoDir, "", lock, func(context.Context, string) bool { return false })
	if _, err := os.Stat(marker.WorktreePath); !os.IsNotExist(err) {
		t.Fatalf("Temporal intent worktree still exists after reconciliation: %v", err)
	}
}

func TestReconcileIsolationMarkersLeavesTemporalIntentWithoutWorktree(t *testing.T) {
	// not parallel: newFixtureRepo (testfixture.NewGitRepo) calls t.Setenv internally.
	repoDir := newFixtureRepo(t)
	dataDir := t.TempDir()
	lock := testHeldIsolationLock(t, repoDir)
	marker := requestdrivertest.IsolationMarker(t, repoDir, dataDir, "temporal-missing", "temporal")
	if err := wsisolation.Remove(repoDir, marker.WorktreePath, marker.Branch); err != nil {
		t.Fatalf("remove prepared worktree: %v", err)
	}
	if err := wsisolation.WriteIsolationMarker(wsisolation.IsolationMarkerPath(dataDir, marker.RunID), marker); err != nil {
		t.Fatalf("restore marker: %v", err)
	}
	reconcileIsolationMarkersWith(context.Background(), dataDir, repoDir, "", lock, func(context.Context, string) bool { return false })
	if _, err := os.Stat(wsisolation.IsolationMarkerPath(dataDir, marker.RunID)); err != nil {
		t.Fatalf("missing-worktree marker was not preserved: %v", err)
	}
}

func TestReconcileIsolationMarkersPreservesCheckpointPathMismatch(t *testing.T) {
	// not parallel: newFixtureRepo (testfixture.NewGitRepo) calls t.Setenv internally.
	repoDir := newFixtureRepo(t)
	dataDir := t.TempDir()
	lock := testHeldIsolationLock(t, repoDir)
	marker := requestdrivertest.IsolationMarker(t, repoDir, dataDir, "temporal-mismatch", "temporal")
	marker.ActivityRunID = "activity-run"
	marker.ActivityID = "prepare-activity"
	if err := (&run.Run{ID: marker.RunID, State: run.StateSliceRunning, ProjectPath: repoDir}).Save(dataDir); err != nil {
		t.Fatalf("save temporal run: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(marker.CheckpointDir, "activity-checkpoints"), 0o750); err != nil {
		t.Fatalf("create checkpoint directory: %v", err)
	}
	key := sha256.Sum256([]byte(fmt.Sprintf("%d:%s%d:%s%d:%s", len(marker.WorkflowID), marker.WorkflowID, len(marker.ActivityRunID), marker.ActivityRunID, len(marker.ActivityID), marker.ActivityID)))
	checkpointPath := filepath.Join(marker.CheckpointDir, "activity-checkpoints", fmt.Sprintf("%x.json", key))
	checkpoint := `{"completed":true,"schema_version":2,"workflow_id":"` + marker.WorkflowID + `","run_id":"` + marker.ActivityRunID + `","activity_id":"` + marker.ActivityID + `","result":{"worktree_path":"wrong","branch":"wrong"}}`
	if err := os.WriteFile(checkpointPath, []byte(checkpoint), 0o600); err != nil {
		t.Fatalf("write conflicting checkpoint: %v", err)
	}
	if err := wsisolation.WriteIsolationMarker(wsisolation.IsolationMarkerPath(dataDir, marker.RunID), marker); err != nil {
		t.Fatalf("rewrite marker: %v", err)
	}
	reconcileIsolationMarkersWith(context.Background(), dataDir, repoDir, "", lock, func(context.Context, string) bool { return false })
	if _, err := os.Stat(marker.WorktreePath); err != nil {
		t.Fatalf("conflicting checkpoint worktree was removed: %v", err)
	}
	if err := wsisolation.Remove(repoDir, marker.WorktreePath, marker.Branch); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
}

// TestDirectRunSandboxWithRetriesReportsExhaustedTeardownMarginAsAnError is
// the missing regression test for a real bug found live and fixed in PR #45:
// when less than sandboxAttemptTeardownMargin remains before the run's own
// deadline, but the deadline itself hasn't passed, ctx.Err() is nil -- so
// returning it unconditionally returned (zero-value Result, nil), which
// reads to every caller as a successful, empty attempt. Found empirically
// with -timeout 10s: a caller two levels up then hashed the zero-value
// LogPath as verify evidence and crashed on "open : no such file or
// directory" instead of halting with a legible cause. That fix landed with
// no test pinning it; this is that test. It never contacts Docker -- with no
// script and no spec to stage, the deadline check is reached before any
// launch. Its internal/workflow counterpart (the same bug, unfixed there
// until now) is
// TestRunSandboxWithRetriesReportsExhaustedTeardownMarginAsAnError.
func TestDirectRunSandboxWithRetriesReportsExhaustedTeardownMarginAsAnError(t *testing.T) {
	t.Parallel()
	logDir := t.TempDir()
	logPath := func(int) string { return filepath.Join(logDir, "build.log") }

	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(sandboxAttemptTeardownMargin/2))
	defer cancel()

	res, err := runSandboxWithRetries(ctx, t.TempDir(), "", "", "", logPath, 1, "factory-worker:test@sha256:deadbeef", "docker", "", sandbox.DefaultWorkerUID, "run-id", t.TempDir(), "4g", "2", "256m", nil, nil, nil, sandbox.RegistryProxyHooks{}, nil, sandbox.ComposeServicesHooks{}, nil, nil, "sh", "-c", "true")
	if err == nil {
		t.Fatalf("runSandboxWithRetries with less than the teardown margin left: want an error, got nil (result %+v)", res)
	}
	if !strings.Contains(err.Error(), "insufficient time remaining for a sandboxed attempt") {
		t.Fatalf("error = %q, want it to name the exhausted teardown margin", err.Error())
	}
}

// TestDirectRunSandboxWithRetriesDoesNotPadMarginForADisabledComposeSpec
// is the regression for a real bug found live while wiring compose
// services into this function: -compose-services defaults on, so a
// composeSpec is non-nil for nearly every call regardless of whether the
// target repo has a compose file at all, and an earlier version of the
// margin math padded margin by 2*ComposeServicesCleanupTimeout+
// ComposeServicesReadyTimeout (over 4 minutes at the package defaults)
// whenever composeSpec was merely non-nil -- not only when compose
// actually launched anything. That silently shrank every short-timeout
// caller's own effective attempt window repo-wide (this repo's own
// integration suite started halting on "insufficient time remaining"
// across dozens of otherwise-unrelated tests the moment -compose-services
// defaulted on). A composeSpec with no ComposeYAML (BeginComposeServices
// Lifecycle's own documented "disabled, not failed" outcome) must add
// nothing to margin.
func TestDirectRunSandboxWithRetriesDoesNotPadMarginForADisabledComposeSpec(t *testing.T) {
	t.Parallel()
	logDir := t.TempDir()
	logPath := func(int) string { return filepath.Join(logDir, "build.log") }

	// Inside sandboxAttemptTeardownMargin alone, but with plenty of room
	// for the padded (bugged) compose margin, this deadline must still
	// let the attempt run to completion rather than being refused as
	// having insufficient time remaining.
	ctx, cancel := context.WithTimeout(context.Background(), sandboxAttemptTeardownMargin*3)
	defer cancel()

	docker := filepath.Join(t.TempDir(), "docker-fake")
	if err := writeExecutable(docker, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatalf("write fake docker: %v", err)
	}
	composeSpec := &sandbox.ComposeServicesSpec{} // empty ComposeYAML: disabled, matching a target repo with no compose file at all.
	res, err := runSandboxWithRetries(ctx, t.TempDir(), "", "", "", logPath, 1, "factory-worker:test@sha256:deadbeef", docker, sandboxTestUser(), sandbox.DefaultWorkerUID, "run-id", t.TempDir(), "4g", "2", "256m", nil, nil, nil, sandbox.RegistryProxyHooks{}, composeSpec, sandbox.ComposeServicesHooks{}, nil, nil, "sh", "-c", "true")
	if err != nil {
		t.Fatalf("runSandboxWithRetries with a disabled composeSpec = %v, want nil (result %+v)", err, res)
	}
}
