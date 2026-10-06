package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"buildgate/internal/run"
)

// TestIntegrationIntakeFailsFastAcrossProcesses proves a real `factoryd
// intake` subprocess refuses immediately, rather than waiting, when
// another process already holds the advisory lock on the same pilot dir
// (withIntakeLock now uses acquireExclusiveLock, the same fail-fast
// syscall.Flock helper acquireWorkerLock is built on -- see
// worker_config.go). This used to be
// TestIntegrationIntakeSerializesAcrossProcesses, which proved the
// opposite: a second invocation blocked until the first released the
// lock. That silent wait is exactly what an operator running a second
// intake against a pilot dir already being drafted should not get --
// same reasoning acquireWorkerLock's own doc comment already applies
// to a second concurrent worker.
func TestIntegrationIntakeFailsFastAcrossProcesses(t *testing.T) {
	t.Parallel()
	pilotDir := filepath.Join(t.TempDir(), "pilot")
	if err := os.MkdirAll(pilotDir, 0o750); err != nil {
		t.Fatalf("mkdir pilot dir: %v", err)
	}
	script, err := filepath.Abs("testdata/fake_goal_pilot.sh")
	if err != nil {
		t.Fatalf("resolve fake goal_pilot script: %v", err)
	}

	lockPath := filepath.Join(filepath.Dir(pilotDir), "."+filepath.Base(pilotDir)+".intake.lock")
	held, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatalf("open intake lock: %v", err)
	}
	defer held.Close()
	if err := syscall.Flock(int(held.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatalf("hold intake lock: %v", err)
	}
	defer syscall.Flock(int(held.Fd()), syscall.LOCK_UN)

	refused := factorydCommand(t, "intake",
		"-spec-input", "a calculator app",
		"-pilot-dir", pilotDir,
		"-goal-pilot-interpreter", "/bin/sh",
		"-goal-pilot-script", script,
	)
	var refusedOutput bytes.Buffer
	refused.Stdout, refused.Stderr = &refusedOutput, &refusedOutput
	done := make(chan error, 1)
	if err := refused.Start(); err != nil {
		t.Fatalf("start second intake invocation: %v", err)
	}
	go func() { done <- refused.Wait() }()

	select {
	case err := <-done:
		if err == nil {
			t.Fatalf("second intake invocation succeeded while another process held the pilot dir lock\n%s", refusedOutput.String())
		}
		if !strings.Contains(refusedOutput.String(), "already running against pilot dir") {
			t.Errorf("second intake invocation output = %q, want it to say another intake is already running against the pilot dir", refusedOutput.String())
		}
		// Compares against the canonicalized form withIntakeLock itself
		// derives the message from (filepath.Abs + resolveExistingAncestor),
		// not the raw pilotDir string this test built -- on macOS, a
		// t.TempDir() path resolves through /var's own symlink to
		// /private/var, so a literal comparison against the pre-resolution
		// string would spuriously fail here even though the message
		// correctly names the pilot dir.
		resolvedPilotDir, err := resolveExistingAncestor(pilotDir)
		if err != nil {
			t.Fatalf("resolve pilot dir for comparison: %v", err)
		}
		if !strings.Contains(refusedOutput.String(), resolvedPilotDir) {
			t.Errorf("second intake invocation output = %q, want it to name the pilot dir %s", refusedOutput.String(), resolvedPilotDir)
		}
	case <-time.After(5 * time.Second):
		_ = refused.Process.Kill()
		t.Fatalf("second intake invocation did not fail fast -- it blocked instead of refusing\n%s", refusedOutput.String())
	}
}

// TestIntegrationIntakeFailsClosedOnUnreadableTicketsDir is the regression
// test for a real Opus review finding, 2026-09-04: snapshotMarkdownFiles
// used to return an empty map on ANY os.ReadDir error, not just a
// genuinely missing directory -- a transient permission error at snapshot
// time would silently produce an empty ticketsBefore, making every
// pre-existing ticket file compare as "new" once the directory becomes
// readable again, exactly the misreport
// TestIntegrationIntakeDoesNotReportStaleTicketsAsFreshlyDrafted exists to
// catch. Proves the fix: an unreadable (not missing) spec/tickets/
// directory at snapshot time is now a hard, clear error, not a silently
// wrong empty baseline.
func TestIntegrationIntakeFailsClosedOnUnreadableTicketsDir(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("running as root: chmod 0 does not deny root read access, so this test's premise doesn't hold")
	}
	pilotDir := filepath.Join(t.TempDir(), "pilot")
	script, err := filepath.Abs("testdata/fake_goal_pilot.sh")
	if err != nil {
		t.Fatalf("resolve fake goal_pilot script: %v", err)
	}
	args := []string{"intake",
		"-spec-input", "a calculator app",
		"-pilot-dir", pilotDir,
		"-goal-pilot-interpreter", "/bin/sh",
		"-goal-pilot-script", script,
	}
	if out, err := factorydCommand(t, args...).CombinedOutput(); err != nil {
		t.Fatalf("first (successful) factoryd intake failed: %v: %s", err, out)
	}

	ticketsDir := filepath.Join(pilotDir, "spec", "tickets")
	if err := os.Chmod(ticketsDir, 0o000); err != nil {
		t.Fatalf("chmod tickets dir unreadable: %v", err)
	}
	defer os.Chmod(ticketsDir, 0o750) // restore so t.TempDir() cleanup can remove it

	out, err := factorydCommand(t, args...).CombinedOutput()
	if err == nil {
		t.Fatalf("second factoryd intake unexpectedly succeeded against an unreadable tickets dir: %s", out)
	}
	if !strings.Contains(string(out), "snapshot existing tickets") {
		t.Errorf("intake output = %q, want a clear error naming the snapshot failure, not a silent misreport", out)
	}
}

// TestIntegrationIntakeCheckpointSkipOnResumeReportsDistinctError is the
// regression test for another real Opus review finding, 2026-09-04:
// passing -checkpoint skip on a resumed invocation (spec.md already
// FROZEN) was honestly documented in both the flag help and USAGE.md --
// goal_pilot.py's own step 5 checkpoint_mode == "skip" branch never
// halts non-interactively, so it proceeds straight through building
// ticket 001 via ticket_runner.py to the always-on post-ticket-001
// checkpoint instead -- but no test exercised that path at all before
// this. Proves: intakeMain reports a distinct, honest error naming that
// real, consequential unattended work happened (not the generic
// "reached neither checkpoint" drafting-failure message, which would
// wrongly imply nothing happened).
func TestIntegrationIntakeCheckpointSkipOnResumeReportsDistinctError(t *testing.T) {
	t.Parallel()
	pilotDir := filepath.Join(t.TempDir(), "pilot")
	script, err := filepath.Abs("testdata/fake_goal_pilot.sh")
	if err != nil {
		t.Fatalf("resolve fake goal_pilot script: %v", err)
	}
	firstArgs := []string{"intake",
		"-spec-input", "a calculator app",
		"-pilot-dir", pilotDir,
		"-goal-pilot-interpreter", "/bin/sh",
		"-goal-pilot-script", script,
	}
	if out, err := factorydCommand(t, firstArgs...).CombinedOutput(); err != nil {
		t.Fatalf("first (successful) factoryd intake failed: %v: %s", err, out)
	}
	specPath := filepath.Join(pilotDir, "spec", "spec.md")
	spec, err := os.ReadFile(specPath)
	if err != nil {
		t.Fatalf("read drafted spec: %v", err)
	}
	frozen := strings.Replace(string(spec), "STATUS: DRAFT", "STATUS: FROZEN", 1)
	if err := os.WriteFile(specPath, []byte(frozen), 0o644); err != nil {
		t.Fatalf("freeze drafted spec: %v", err)
	}

	out, err := factorydCommand(t, append(append([]string{}, firstArgs...), "-checkpoint", "skip")...).CombinedOutput()
	if err == nil {
		t.Fatalf("resumed factoryd intake with -checkpoint skip unexpectedly succeeded: %s", out)
	}
	if !strings.Contains(string(out), "actually built ticket 001 unattended") {
		t.Errorf("intake output = %q, want the distinct post-ticket-001 message naming the real unattended build, not the generic drafting-failure error", out)
	}
	if strings.Contains(string(out), "did not reach either the spec-freeze checkpoint") {
		t.Errorf("intake output = %q, want the distinct message, not the generic 'reached neither checkpoint' one -- this run did reach a checkpoint, just the third one", out)
	}
	// Confirms the fixture really did simulate reaching this point (real
	// consequential work happened), not that the distinct message fired
	// for some unrelated reason.
	if _, statErr := os.Stat(filepath.Join(pilotDir, ".fixture-ticket-001-built")); statErr != nil {
		t.Errorf("stat fixture ticket-001-built marker: %v -- want the fixture to have reached its own -checkpoint skip branch", statErr)
	}
}

// TestIntegrationInitRejectsSymlinkedArtifactPath is the regression test
// for a real finding from a GitHub Codex App review round, 2026-08-29:
// os.Stat follows symlinks, so a *dangling* symlink at a scaffold artifact
// path reported IsNotExist -- read as "safe to create" -- even though the
// path itself already existed as a symlink, and the subsequent
// os.WriteFile would follow it, creating its target outside -root.
func TestIntegrationInitRejectsSymlinkedArtifactPath(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	outsideTarget := filepath.Join(t.TempDir(), "escaped-architecture.md")
	if err := os.MkdirAll(filepath.Dir(outsideTarget), 0o750); err != nil {
		t.Fatalf("mkdir outside target dir: %v", err)
	}
	if err := os.Symlink(outsideTarget, filepath.Join(root, "ARCHITECTURE.md")); err != nil {
		t.Fatalf("symlink ARCHITECTURE.md: %v", err)
	}

	cmd := factorydCommand(t, "init", "-skip-doctor", "-project", "calculator-live-test", "-root", root)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected a nonzero exit for a symlinked artifact path, got success: %s", out)
	}
	if _, statErr := os.Lstat(outsideTarget); statErr == nil {
		t.Errorf("outside target %q was created through the symlink, want init to have refused before writing anything", outsideTarget)
	}
	// Checked up front for every path before writing any (same invariant
	// TestIntegrationCheckProjectRejectsTraversalProjectID pins for
	// check-project): the two non-conflicting files must not have been
	// written either, so a symlink discovered on the *last* checked path
	// can't leave a partial scaffold behind.
	if _, statErr := os.Stat(filepath.Join(root, "spec", "spec.md")); statErr == nil {
		t.Error("spec/spec.md was scaffolded despite a conflict on a sibling artifact, want nothing written")
	}
}

// TestIntegrationInitRejectsSymlinkedAncestorDirectory is
// TestIntegrationInitRejectsSymlinkedArtifactPath's counterpart for a
// symlinked *ancestor* rather than the artifact path itself: -root/spec
// pointing elsewhere, with no spec.md/contract.md created there yet, so
// the leaf-level Lstat conflict check alone would see "does not exist" and
// proceed -- os.MkdirAll would then follow the symlinked spec/ directory
// and os.WriteFile would create spec.md/contract.md through it, outside
// -root.
func TestIntegrationInitRejectsSymlinkedAncestorDirectory(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	outsideSpecDir := t.TempDir()
	if err := os.Symlink(outsideSpecDir, filepath.Join(root, "spec")); err != nil {
		t.Fatalf("symlink spec/: %v", err)
	}

	cmd := factorydCommand(t, "init", "-skip-doctor", "-project", "calculator-live-test", "-root", root)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected a nonzero exit for a symlinked ancestor directory, got success: %s", out)
	}
	if !strings.Contains(string(out), "symlink") {
		t.Errorf("output = %q, want it to name the symlinked-ancestor rejection", out)
	}
	if entries, readErr := os.ReadDir(outsideSpecDir); readErr == nil && len(entries) != 0 {
		t.Errorf("outside dir %q gained content through the symlinked ancestor, want nothing written: %v", outsideSpecDir, entries)
	}
}

// TestIntegrationCheckProjectResultsBindArtifactPathAndDigest is the
// regression test for a real GitHub Codex App review finding on PR #27
// ("Bind each result to its input artifact"): without a path and a
// content digest on each ProjectCheckResult, a PASS record can't be tied
// to which artifact file — or which version of it — actually produced
// that PASS. Confirms both fields are populated and the digest matches
// the artifact's own on-disk sha256sum.
func TestIntegrationCheckProjectResultsBindArtifactPathAndDigest(t *testing.T) {
	t.Parallel()
	specPath, _, _, _ := fixtureCheckProjectArtifacts(t)
	dataDir := t.TempDir()

	cmd := factorydCommand(t, "check-project",
		"-project", "fixture-project",
		"-spec", specPath,
		"-data-dir", dataDir,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("factoryd check-project: %v: %s", err, out)
	}

	entries, err := os.ReadDir(filepath.Join(dataDir, "project-checks"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("expected exactly 1 project-check record: entries=%v err=%v", entries, err)
	}
	b, err := os.ReadFile(filepath.Join(dataDir, "project-checks", entries[0].Name()))
	if err != nil {
		t.Fatalf("read project-check record: %v", err)
	}
	var record ProjectCheckRecord
	if err := json.Unmarshal(b, &record); err != nil {
		t.Fatalf("unmarshal project-check record: %v", err)
	}
	if len(record.Results) != 1 {
		t.Fatalf("record.Results = %+v, want exactly 1", record.Results)
	}
	got := record.Results[0]
	if got.Path != specPath {
		t.Errorf("Results[0].Path = %q, want %q", got.Path, specPath)
	}
	specBytes, err := os.ReadFile(specPath)
	if err != nil {
		t.Fatalf("read spec fixture: %v", err)
	}
	wantSHA := fmt.Sprintf("%x", sha256.Sum256(specBytes))
	if got.SHA256 != wantSHA {
		t.Errorf("Results[0].SHA256 = %q, want %q (sha256 of the artifact actually checked)", got.SHA256, wantSHA)
	}
}

// TestIntegrationCheckProjectRequiresTicketNumberWithTicket is the
// regression test for a real GitHub Codex App review finding on PR #27
// ("Require the ticket number for ticket checks"): -ticket-number used to
// default to 1, which silently exempts a later ticket from
// policy.TicketStructure's "This is an existing repo." context-line
// requirement whenever the caller forgets to pass -ticket-number — a
// malformed ticket 2+ could get a false PASS. Confirms -ticket without an
// explicit, positive -ticket-number is now refused outright rather than
// silently defaulting.
func TestIntegrationCheckProjectRequiresTicketNumberWithTicket(t *testing.T) {
	t.Parallel()
	_, _, _, ticketPath := fixtureCheckProjectArtifacts(t)
	dataDir := t.TempDir()

	cmd := factorydCommand(t, "check-project",
		"-project", "fixture-project",
		"-ticket", ticketPath,
		"-data-dir", dataDir,
	)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected a nonzero exit for -ticket without -ticket-number, got success: %s", out)
	}
	if !strings.Contains(string(out), "-ticket-number is required") {
		t.Errorf("output = %q, want it to name the missing -ticket-number", out)
	}
	if entries, err := os.ReadDir(filepath.Join(dataDir, "project-checks")); err == nil && len(entries) != 0 {
		t.Errorf("project-checks dir = %v, want no record written for a rejected invocation", entries)
	}
}

// TestIntegrationCheckProjectPersistsUnreadableArtifactAsFailedCheck is
// the regression test for a real GitHub Codex App review finding on PR
// #27 ("Persist unreadable artifacts as failed checks"): an earlier
// version of checkProjectMain returned an error immediately when a
// declared artifact could not be read, before ever calling
// saveProjectCheckRecord — leaving no durable record at all, so a failed
// read was indistinguishable from check-project never having run.
// Confirms a missing file now still produces a durable record marking
// that specific check failed with the read error as its reason.
func TestIntegrationCheckProjectPersistsUnreadableArtifactAsFailedCheck(t *testing.T) {
	t.Parallel()
	specPath, _, _, _ := fixtureCheckProjectArtifacts(t)
	dataDir := t.TempDir()
	missingPath := filepath.Join(t.TempDir(), "does-not-exist.md")

	cmd := factorydCommand(t, "check-project",
		"-project", "fixture-project",
		"-spec", specPath,
		"-contract", missingPath,
		"-data-dir", dataDir,
	)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected a nonzero exit for an unreadable artifact, got success: %s", out)
	}

	entries, err := os.ReadDir(filepath.Join(dataDir, "project-checks"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("expected exactly 1 project-check record even for a read failure: entries=%v err=%v", entries, err)
	}
	b, err := os.ReadFile(filepath.Join(dataDir, "project-checks", entries[0].Name()))
	if err != nil {
		t.Fatalf("read project-check record: %v", err)
	}
	var record ProjectCheckRecord
	if err := json.Unmarshal(b, &record); err != nil {
		t.Fatalf("unmarshal project-check record: %v", err)
	}
	if record.Passed {
		t.Errorf("record.Passed = true, want false: %+v", record)
	}
	if len(record.Results) != 2 {
		t.Fatalf("record.Results = %+v, want exactly 2 (spec + contract, even though contract's read failed)", record.Results)
	}
	var contractResult *ProjectCheckResult
	for i := range record.Results {
		if record.Results[i].Check == "program_design_structure" {
			contractResult = &record.Results[i]
		}
	}
	if contractResult == nil {
		t.Fatalf("record.Results = %+v, want a program_design_structure entry for the unreadable contract", record.Results)
	}
	if contractResult.Passed {
		t.Errorf("contract result.Passed = true, want false for an unreadable file")
	}
	if contractResult.SHA256 != "" {
		t.Errorf("contract result.SHA256 = %q, want empty for a file that could not be read", contractResult.SHA256)
	}
	if contractResult.Path != missingPath {
		t.Errorf("contract result.Path = %q, want %q", contractResult.Path, missingPath)
	}
	if len(contractResult.Reasons) == 0 || !strings.Contains(contractResult.Reasons[0], "could not read artifact") {
		t.Errorf("contract result.Reasons = %v, want it to name the read failure", contractResult.Reasons)
	}
}

// TestIntegrationSliceChainAcceptsMatchingPriorRun pins the -prior-run
// wiring for gap 2 from the plan's 2026-08-28 Opus review ("nothing
// links slice N+1 to slice N"): a run declaring -prior-run succeeds when
// its freshly captured base_sha equals the prior run's own result_sha —
// the ordinary case of running two slices back to back against the same
// shared workspace.
func TestIntegrationSliceChainAcceptsMatchingPriorRun(t *testing.T) {
	// not parallel-safe: newFixtureRepo/newFixtureRepoWithoutBootstrapScaffold call
	// t.Setenv (GIT_CONFIG_GLOBAL/GIT_CONFIG_SYSTEM), and Go's testing package
	// panics if a test combines t.Setenv with t.Parallel().
	ws := newFixtureRepo(t)
	dataDir := t.TempDir()

	// This test exercises prior-run chaining: the second run starts from
	// the first run's result.
	first := runFactorydWithSpecFlagsAndDataDir(t, ws, "commit", "true", "# fixture spec\nTests-Required: no -- integration fixture doesn't exercise tests_added\n", "30s", nil, nil, dataDir)
	if first.State != run.StateAccepted {
		t.Fatalf("first run state = %q, want %q", first.State, run.StateAccepted)
	}

	second := runFactorydWithSpecFlagsAndDataDir(t, ws, "commit", "true", "# fixture spec\nTests-Required: no -- integration fixture doesn't exercise tests_added\n", "30s", nil, []string{"-prior-run", first.ID}, dataDir)
	if second.State != run.StateAccepted {
		t.Fatalf("second run state = %q, want %q", second.State, run.StateAccepted)
	}
	if second.PriorRunID != first.ID {
		t.Errorf("PriorRunID = %q, want %q", second.PriorRunID, first.ID)
	}
	if second.BaseSHA != first.ResultSHA {
		t.Errorf("second.BaseSHA = %q, want it to equal first.ResultSHA %q", second.BaseSHA, first.ResultSHA)
	}
}

// TestIntegrationFullSuiteVerifyInvalidatesPriorRunOnRegression is gap 3's
// cross-run attribution half: when a chained run's own full_suite_verify
// fails, its declared -prior-run's already-accepted record must be
// durably flagged (InvalidatedByRunID/At/Reason) without changing that
// prior run's own State — a human decides what to do with the annotation,
// the same way every other override remains a human decision.
func TestIntegrationFullSuiteVerifyInvalidatesPriorRunOnRegression(t *testing.T) {
	// not parallel-safe: newFixtureRepo/newFixtureRepoWithoutBootstrapScaffold call
	// t.Setenv (GIT_CONFIG_GLOBAL/GIT_CONFIG_SYSTEM), and Go's testing package
	// panics if a test combines t.Setenv with t.Parallel().
	ws := newFixtureRepo(t)
	dataDir := t.TempDir()

	first := runFactorydWithSpecFlagsAndDataDir(t, ws, "commit", "true", "# fixture spec\nTests-Required: no -- integration fixture doesn't exercise tests_added\n", "30s", nil, nil, dataDir)
	if first.State != run.StateAccepted {
		t.Fatalf("first run state = %q, want %q", first.State, run.StateAccepted)
	}

	second := runFactorydWithSpecFlagsAndDataDir(t, ws, "commit", "true", "# fixture spec\nTests-Required: no -- integration fixture doesn't exercise tests_added\n", "30s", nil, []string{"-prior-run", first.ID, "-full-suite-command", "false"}, dataDir)
	if second.State != run.StateQuarantined {
		t.Fatalf("second run state = %q, want %q", second.State, run.StateQuarantined)
	}

	reloadedFirst, err := run.Load(dataDir, first.ID)
	if err != nil {
		t.Fatalf("reload first run: %v", err)
	}
	if reloadedFirst.State != run.StateAccepted {
		t.Errorf("first run State = %q after invalidation, want it unchanged at %q — invalidation must never itself change State", reloadedFirst.State, run.StateAccepted)
	}
	if reloadedFirst.InvalidatedByRunID != second.ID {
		t.Errorf("first run InvalidatedByRunID = %q, want %q", reloadedFirst.InvalidatedByRunID, second.ID)
	}
	if reloadedFirst.InvalidatedAt == "" {
		t.Error("first run InvalidatedAt is empty, want a timestamp")
	}
	if reloadedFirst.InvalidatedReason == "" {
		t.Error("first run InvalidatedReason is empty, want an explanation")
	}
}

// TestIntegrationSliceChainDetectsProductSpecDrift is the regression test
// for gap 5 of the plan's 2026-08-28 Opus review ("spec drift across a
// long build is unmanaged"): a chained run whose declared -prior-run was
// judged against a product spec.md that has since been edited must durably
// flag that predecessor, without blocking or changing the state of either
// run.
func TestIntegrationSliceChainDetectsProductSpecDrift(t *testing.T) {
	// not parallel-safe: newFixtureRepo/newFixtureRepoWithoutBootstrapScaffold call
	// t.Setenv (GIT_CONFIG_GLOBAL/GIT_CONFIG_SYSTEM), and Go's testing package
	// panics if a test combines t.Setenv with t.Parallel().
	ws := newFixtureRepo(t)
	dataDir := t.TempDir()

	first := runFactorydWithSpecFlagsAndDataDir(t, ws, "commit", "true", "# fixture spec\nTests-Required: no -- integration fixture doesn't exercise tests_added\n", "30s", nil, nil, dataDir)
	if first.State != run.StateAccepted {
		t.Fatalf("first run state = %q, want %q", first.State, run.StateAccepted)
	}
	reloadedFirst, err := run.Load(dataDir, first.ID)
	if err != nil {
		t.Fatalf("reload first run: %v", err)
	}
	if reloadedFirst.ProductSpecSHA256 == "" {
		t.Fatal("first run ProductSpecSHA256 is empty, want it recorded by the mandatory project-bootstrap preflight")
	}

	// Edit the project's root spec.md (a sibling of ws, per
	// projectBootstrapArtifactPaths) between the two runs — still frozen,
	// so the second run's own preflight still passes, but hashes
	// differently than what the first run recorded.
	specPath := filepath.Join(filepath.Dir(ws), "spec", "spec.md")
	revised := "STATUS: FROZEN -- test fixture (revised)\n\n# Fixture Project — Product Spec\n\nRevised mid-build.\n"
	if err := os.WriteFile(specPath, []byte(revised), 0o644); err != nil {
		t.Fatalf("revise spec.md: %v", err)
	}

	second := runFactorydWithSpecFlagsAndDataDir(t, ws, "commit", "true", "# fixture spec\nTests-Required: no -- integration fixture doesn't exercise tests_added\n", "30s", nil, []string{"-prior-run", first.ID}, dataDir)
	if second.State != run.StateAccepted {
		t.Fatalf("second run state = %q, want %q — a spec revision must never block or quarantine a chained run", second.State, run.StateAccepted)
	}

	reloadedFirst, err = run.Load(dataDir, first.ID)
	if err != nil {
		t.Fatalf("reload first run after drift: %v", err)
	}
	if reloadedFirst.State != run.StateAccepted {
		t.Errorf("first run State = %q after drift detection, want it unchanged at %q — detecting drift must never itself change State", reloadedFirst.State, run.StateAccepted)
	}
	if reloadedFirst.SpecDriftDetectedByRunID != second.ID {
		t.Errorf("first run SpecDriftDetectedByRunID = %q, want %q", reloadedFirst.SpecDriftDetectedByRunID, second.ID)
	}
	if reloadedFirst.SpecDriftDetectedAt == "" {
		t.Error("first run SpecDriftDetectedAt is empty, want a timestamp")
	}
	if !strings.Contains(reloadedFirst.SpecDriftReason, "spec/spec.md") {
		t.Errorf("first run SpecDriftReason = %q, want it to name spec/spec.md", reloadedFirst.SpecDriftReason)
	}
	if reloadedFirst.InvalidatedByRunID != "" {
		t.Errorf("first run InvalidatedByRunID = %q, want empty — spec drift is a distinct signal from a proven full-suite regression", reloadedFirst.InvalidatedByRunID)
	}
}

// TestIntegrationSliceChainNoDriftWhenSpecUnchanged is
// TestIntegrationSliceChainDetectsProductSpecDrift's negative case: an
// ordinary chained run against an untouched product spec must never mark
// its predecessor as drifted.
func TestIntegrationSliceChainNoDriftWhenSpecUnchanged(t *testing.T) {
	// not parallel-safe: newFixtureRepo/newFixtureRepoWithoutBootstrapScaffold call
	// t.Setenv (GIT_CONFIG_GLOBAL/GIT_CONFIG_SYSTEM), and Go's testing package
	// panics if a test combines t.Setenv with t.Parallel().
	ws := newFixtureRepo(t)
	dataDir := t.TempDir()

	first := runFactorydWithSpecFlagsAndDataDir(t, ws, "commit", "true", "# fixture spec\nTests-Required: no -- integration fixture doesn't exercise tests_added\n", "30s", nil, nil, dataDir)
	if first.State != run.StateAccepted {
		t.Fatalf("first run state = %q, want %q", first.State, run.StateAccepted)
	}

	second := runFactorydWithSpecFlagsAndDataDir(t, ws, "commit", "true", "# fixture spec\nTests-Required: no -- integration fixture doesn't exercise tests_added\n", "30s", nil, []string{"-prior-run", first.ID}, dataDir)
	if second.State != run.StateAccepted {
		t.Fatalf("second run state = %q, want %q", second.State, run.StateAccepted)
	}

	reloadedFirst, err := run.Load(dataDir, first.ID)
	if err != nil {
		t.Fatalf("reload first run: %v", err)
	}
	if reloadedFirst.SpecDriftDetectedByRunID != "" {
		t.Errorf("first run SpecDriftDetectedByRunID = %q, want empty — the product spec never changed between the two runs", reloadedFirst.SpecDriftDetectedByRunID)
	}
}

// TestIntegrationSliceChainAcceptsIsolatedPriorRun proves an isolated
// successor starts from its accepted predecessor's ResultSHA rather than
// the shared checkout HEAD, which remains unchanged by both runs.
func TestIntegrationSliceChainAcceptsIsolatedPriorRun(t *testing.T) {
	// not parallel-safe: newFixtureRepo/newFixtureRepoWithoutBootstrapScaffold call
	// t.Setenv (GIT_CONFIG_GLOBAL/GIT_CONFIG_SYSTEM), and Go's testing package
	// panics if a test combines t.Setenv with t.Parallel().
	ws := newFixtureRepo(t)
	dataDir := t.TempDir()
	headBefore, err := exec.Command("git", "-C", ws, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("capture shared HEAD: %v", err)
	}

	// Deliberately no isolation flag on either run: the default
	// is the whole point of this test.
	first := runFactorydWithSpecFlagsAndDataDir(t, ws, "commit", "true", "# fixture spec\nTests-Required: no -- integration fixture doesn't exercise tests_added\n", "30s", nil, nil, dataDir)
	if first.State != run.StateAccepted || first.Branch == "" {
		t.Fatalf("first run state=%q branch=%q, want accepted and isolated by default", first.State, first.Branch)
	}

	second := runFactorydWithSpecFlagsAndDataDir(t, ws, "commit", "true", "# fixture spec\nTests-Required: no -- integration fixture doesn't exercise tests_added\n", "30s", nil, []string{"-prior-run", first.ID}, dataDir)
	if second.State != run.StateAccepted {
		t.Fatalf("second run state = %q, want %q", second.State, run.StateAccepted)
	}
	if second.BaseSHA != first.ResultSHA {
		t.Errorf("second BaseSHA = %q, want first ResultSHA %q", second.BaseSHA, first.ResultSHA)
	}
	if second.ProjectPath != ws {
		t.Errorf("second ProjectPath = %q, want shared project %q", second.ProjectPath, ws)
	}
	if second.WorkspacePath == ws || second.Branch == "" {
		t.Errorf("second was not isolated: workspace=%q branch=%q", second.WorkspacePath, second.Branch)
	}
	headAfter, err := exec.Command("git", "-C", ws, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("capture shared HEAD after chain: %v", err)
	}
	if string(headAfter) != string(headBefore) {
		t.Errorf("shared checkout HEAD changed from %q to %q", headBefore, headAfter)
	}
}

// TestIntegrationSliceChainRejectsSupersededIsolatedPriorRun is the
// regression test for a real finding from an adversarial review round
// (2026-09-01): an isolated successor starts its own worktree directly
// from the declared -prior-run's recorded ResultSHA rather than the
// shared checkout's HEAD, so unlike a non-isolated run -- where a
// superseded prior's result_sha can no longer match live HEAD, and
// ValidateSliceChain's own base_sha comparison catches that automatically
// -- nothing else detects a third slice declaring the *first* slice as
// its prior when a second slice already chained onto it and was accepted.
// Without run.FindChainSuccessor, that stale declaration would silently
// succeed and build the third slice on top of the first, discarding the
// second slice's work with no error at all.
func TestIntegrationSliceChainRejectsSupersededIsolatedPriorRun(t *testing.T) {
	// not parallel-safe: newFixtureRepo/newFixtureRepoWithoutBootstrapScaffold call
	// t.Setenv (GIT_CONFIG_GLOBAL/GIT_CONFIG_SYSTEM), and Go's testing package
	// panics if a test combines t.Setenv with t.Parallel().
	ws := newFixtureRepo(t)
	dataDir := t.TempDir()

	first := runFactorydWithSpecFlagsAndDataDir(t, ws, "commit", "true", "# fixture spec\nTests-Required: no -- integration fixture doesn't exercise tests_added\n", "30s", nil, nil, dataDir)
	if first.State != run.StateAccepted {
		t.Fatalf("first run state = %q, want %q", first.State, run.StateAccepted)
	}

	second := runFactorydWithSpecFlagsAndDataDir(t, ws, "commit", "true", "# fixture spec\nTests-Required: no -- integration fixture doesn't exercise tests_added\n", "30s", nil, []string{"-prior-run", first.ID}, dataDir)
	if second.State != run.StateAccepted {
		t.Fatalf("second run state = %q, want %q", second.State, run.StateAccepted)
	}

	// Third slice wrongly declares the first run as its predecessor,
	// skipping the second -- must be rejected even though the first run
	// is itself perfectly valid (accepted, correct project, real
	// result_sha), since it is no longer the chain tip.
	third := runFactorydWithSpecFlagsAndDataDir(t, ws, "commit", "true", "# fixture spec\nTests-Required: no -- integration fixture doesn't exercise tests_added\n", "30s", nil, []string{"-prior-run", first.ID}, dataDir)
	if third.State != run.StateHalted {
		t.Fatalf("third run state = %q, want %q (a superseded prior run must not silently be chained onto)", third.State, run.StateHalted)
	}
	if !third.HaltConfirmed {
		t.Error("HaltConfirmed = false, want true: this is a local pre-submission failure")
	}
}

// TestIntegrationSliceChainRejectsMismatchedPriorRun pins the fail-closed
// side of the same check: declaring -prior-run against a run whose
// result_sha does not match this run's freshly captured base_sha (here,
// an entirely unrelated repository) halts before build_app.py ever runs.
func TestIntegrationSliceChainRejectsMismatchedPriorRun(t *testing.T) {
	// not parallel-safe: newFixtureRepo/newFixtureRepoWithoutBootstrapScaffold call
	// t.Setenv (GIT_CONFIG_GLOBAL/GIT_CONFIG_SYSTEM), and Go's testing package
	// panics if a test combines t.Setenv with t.Parallel().
	wsA := newFixtureRepo(t)
	wsB := newFixtureRepo(t) // a different repo entirely -> unrelated HEAD SHA
	dataDir := t.TempDir()

	first := runFactorydWithSpecFlagsAndDataDir(t, wsA, "commit", "true", "# fixture spec\nTests-Required: no -- integration fixture doesn't exercise tests_added\n", "30s", nil, nil, dataDir)
	if first.State != run.StateAccepted {
		t.Fatalf("first run state = %q, want %q", first.State, run.StateAccepted)
	}

	second := runFactorydWithSpecFlagsAndDataDir(t, wsB, "commit", "true", "# fixture spec\nTests-Required: no -- integration fixture doesn't exercise tests_added\n", "30s", nil, []string{"-prior-run", first.ID}, dataDir)
	if second.State != run.StateHalted {
		t.Fatalf("second run state = %q, want %q (base_sha must not silently accept an unrelated prior run's result_sha)", second.State, run.StateHalted)
	}
	if !second.HaltConfirmed {
		t.Error("HaltConfirmed = false, want true: this is a local pre-submission failure")
	}

	// Regression for a real finding from a local codex review round:
	// recordSpecDriftIfDetected must never mark a predecessor from an
	// entirely unrelated project as drifted just because the two projects'
	// spec.md contents happen to differ. wsA and wsB are fresh fixture
	// projects with identical scaffolded spec.md content, so this alone
	// wouldn't have caught a missing project-path guard; the point is that
	// the run halted (ValidateSliceChain rejected it) before dispatch, and
	// this asserts no drift annotation was still written regardless.
	reloadedFirst, err := run.Load(dataDir, first.ID)
	if err != nil {
		t.Fatalf("reload first run: %v", err)
	}
	if reloadedFirst.SpecDriftDetectedByRunID != "" {
		t.Errorf("first run SpecDriftDetectedByRunID = %q, want empty — a rejected chain against an unrelated project must never record drift", reloadedFirst.SpecDriftDetectedByRunID)
	}
}

// TestIntegrationSliceChainWithPlainTemporalAddressStillValidatesEagerly
// pins the fail-fast behavior for a missing predecessor before Temporal is
// contacted. A valid predecessor is covered by the live success test below.
func TestIntegrationSliceChainWithPlainTemporalAddressStillValidatesEagerly(t *testing.T) {
	// not parallel-safe: newFixtureRepo/newFixtureRepoWithoutBootstrapScaffold call
	// t.Setenv (GIT_CONFIG_GLOBAL/GIT_CONFIG_SYSTEM), and Go's testing package
	// panics if a test combines t.Setenv with t.Parallel().
	ws := newFixtureRepo(t)
	dataDir := t.TempDir()
	specPath := filepath.Join(t.TempDir(), "spec.md")
	if err := os.WriteFile(specPath, []byte("# fixture spec\nTests-Required: no -- integration fixture doesn't exercise tests_added\n"), 0o644); err != nil {
		t.Fatalf("write spec: %v", err)
	}
	scriptPath, err := filepath.Abs("testdata/fake_build_app.sh")
	if err != nil {
		t.Fatalf("abs script path: %v", err)
	}

	cmd := factorydCommand(t,
		"-ticket", "fixture-ticket",
		"-sandbox-image", fakeSandboxImage,
		"-workspace", ws,
		"-spec", specPath,
		"-build-app-interpreter", "/bin/sh",
		"-build-app-script", scriptPath,
		"-verify-command", "true",
		"-data-dir", dataDir,
		"-temporal-address", "localhost:0",
		"-prior-run", "some-prior-run",
	)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected a nonzero exit for a nonexistent -prior-run, got success: %s", out)
	}
	if !strings.Contains(string(out), "load prior run") || !strings.Contains(string(out), "some-prior-run") {
		t.Errorf("output = %q, want it to name the missing prior run", out)
	}
	entries, err := os.ReadDir(filepath.Join(dataDir, "runs"))
	if err != nil {
		t.Fatalf("read runs dir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("runs dir = %v, want exactly one halted run's durable record", entries)
	}
	r, err := run.Load(dataDir, entries[0].Name())
	if err != nil {
		t.Fatalf("load run: %v", err)
	}
	if r.State != run.StateHalted {
		t.Errorf("state = %q, want %q", r.State, run.StateHalted)
	}
}

// TestIntegrationSliceChainAcceptsIsolatedPriorRunViaPlainTemporal proves
// plain Temporal execution carries an isolated predecessor's ResultSHA into
// the next worktree while leaving the shared checkout untouched.
func TestIntegrationSliceChainAcceptsIsolatedPriorRunViaPlainTemporal(t *testing.T) {
	// not parallel-safe: newFixtureRepo/newFixtureRepoWithoutBootstrapScaffold call
	// t.Setenv (GIT_CONFIG_GLOBAL/GIT_CONFIG_SYSTEM), and Go's testing package
	// panics if a test combines t.Setenv with t.Parallel().
	address := isolatedTemporalAddress(t)
	ws := newFixtureRepo(t)
	dataDir := t.TempDir()
	headBefore, err := exec.Command("git", "-C", ws, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("capture shared HEAD: %v", err)
	}

	first := runFactorydWithSpecFlagsAndDataDir(t, ws, "commit", "true", "# fixture spec\nTests-Required: no -- integration fixture doesn't exercise tests_added\n", "30s", nil,
		[]string{"-temporal-address", address}, dataDir)
	if first.State != run.StateAccepted || first.Branch == "" {
		t.Fatalf("first run state=%q branch=%q, want accepted isolated predecessor", first.State, first.Branch)
	}
	second := runFactorydWithSpecFlagsAndDataDir(t, ws, "commit", "true", "# fixture spec\nTests-Required: no -- integration fixture doesn't exercise tests_added\n", "30s", nil,
		[]string{"-temporal-address", address, "-prior-run", first.ID}, dataDir)
	if second.State != run.StateAccepted {
		t.Fatalf("second run state=%q, want %q", second.State, run.StateAccepted)
	}
	if second.BaseSHA != first.ResultSHA {
		t.Errorf("second BaseSHA=%q, want first ResultSHA %q", second.BaseSHA, first.ResultSHA)
	}
	if second.Branch == "" || second.WorkspacePath == ws {
		t.Errorf("second was not isolated: branch=%q workspace=%q", second.Branch, second.WorkspacePath)
	}
	headAfter, err := exec.Command("git", "-C", ws, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("capture shared HEAD after chain: %v", err)
	}
	if string(headAfter) != string(headBefore) {
		t.Errorf("shared checkout HEAD changed from %q to %q", headBefore, headAfter)
	}
}

// TestIntegrationSliceChainRejectsSupersededIsolatedPriorRunViaPlainTemporal
// is the Temporal-path counterpart of
// TestIntegrationSliceChainRejectsSupersededIsolatedPriorRun. Before
// CheckChainSuccessorActivity, RunWorkflow's isolatePriorResult branch
// validated a declared -prior-run only via CaptureBaseSHAActivity/
// ValidateSliceChainActivity's ResultSHA-against-itself comparison — always
// true by construction — so a third slice could silently reopen a chain
// tip a second slice had already superseded, on this path alone.
func TestIntegrationSliceChainRejectsSupersededIsolatedPriorRunViaPlainTemporal(t *testing.T) {
	// not parallel-safe: newFixtureRepo/newFixtureRepoWithoutBootstrapScaffold call
	// t.Setenv (GIT_CONFIG_GLOBAL/GIT_CONFIG_SYSTEM), and Go's testing package
	// panics if a test combines t.Setenv with t.Parallel().
	address := isolatedTemporalAddress(t)
	ws := newFixtureRepo(t)
	dataDir := t.TempDir()

	first := runFactorydWithSpecFlagsAndDataDir(t, ws, "commit", "true", "# fixture spec\nTests-Required: no -- integration fixture doesn't exercise tests_added\n", "30s", nil,
		[]string{"-temporal-address", address}, dataDir)
	if first.State != run.StateAccepted {
		t.Fatalf("first run state = %q, want %q", first.State, run.StateAccepted)
	}

	second := runFactorydWithSpecFlagsAndDataDir(t, ws, "commit", "true", "# fixture spec\nTests-Required: no -- integration fixture doesn't exercise tests_added\n", "30s", nil,
		[]string{"-temporal-address", address, "-prior-run", first.ID}, dataDir)
	if second.State != run.StateAccepted {
		t.Fatalf("second run state = %q, want %q", second.State, run.StateAccepted)
	}

	// Third slice wrongly declares the first run as its predecessor,
	// skipping the second -- must be rejected even though the first run is
	// itself perfectly valid, since it is no longer the chain tip.
	third := runFactorydWithSpecFlagsAndDataDir(t, ws, "commit", "true", "# fixture spec\nTests-Required: no -- integration fixture doesn't exercise tests_added\n", "30s", nil,
		[]string{"-temporal-address", address, "-prior-run", first.ID}, dataDir)
	if third.State != run.StateHalted {
		t.Fatalf("third run state = %q, want %q (a superseded prior run must not silently be chained onto via Temporal)", third.State, run.StateHalted)
	}
}

// TestIntegrationSliceChainRejectsTraversalPriorRun is the regression
// test for a real finding from codex review (round 3, 2026-08-28):
// run.Load does not itself validate its id argument, so an unvalidated
// -prior-run containing ".." could resolve outside -data-dir/runs
// entirely and load an unrelated run.json as this run's declared
// predecessor.
func TestIntegrationSliceChainRejectsTraversalPriorRun(t *testing.T) {
	// not parallel-safe: newFixtureRepo/newFixtureRepoWithoutBootstrapScaffold call
	// t.Setenv (GIT_CONFIG_GLOBAL/GIT_CONFIG_SYSTEM), and Go's testing package
	// panics if a test combines t.Setenv with t.Parallel().
	ws := newFixtureRepo(t)
	dataDir := t.TempDir()
	specPath := filepath.Join(t.TempDir(), "spec.md")
	if err := os.WriteFile(specPath, []byte("# fixture spec\nTests-Required: no -- integration fixture doesn't exercise tests_added\n"), 0o644); err != nil {
		t.Fatalf("write spec: %v", err)
	}
	scriptPath, err := filepath.Abs("testdata/fake_build_app.sh")
	if err != nil {
		t.Fatalf("abs script path: %v", err)
	}

	cmd := factorydCommand(t,
		"-ticket", "fixture-ticket",
		"-sandbox-image", fakeSandboxImage,
		"-workspace", ws,
		"-spec", specPath,
		"-build-app-interpreter", "/bin/sh",
		"-build-app-script", scriptPath,
		"-verify-command", "true",
		"-data-dir", dataDir,
		"-prior-run", "../../../../etc/passwd",
	)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected a nonzero exit for a traversal -prior-run, got success: %s", out)
	}
	if !strings.Contains(string(out), "-prior-run must be a single path component") {
		t.Errorf("output = %q, want it to name the rejected -prior-run value", out)
	}
}

// TestIntegrationIsolateWorkspaceExecutesInSeparateWorktree pins an Opus
// review's highest-leverage finding: with isolation, the
// agent executes in its own git worktree (internal/workspace.Prepare),
// not the shared checkout at -workspace directly. An accepted run's
// result lives on that isolated branch; the shared checkout is left
// completely untouched -- still at baseSHA, still clean.
func TestIntegrationIsolateWorkspaceExecutesInSeparateWorktree(t *testing.T) {
	// not parallel-safe: newFixtureRepo/newFixtureRepoWithoutBootstrapScaffold call
	// t.Setenv (GIT_CONFIG_GLOBAL/GIT_CONFIG_SYSTEM), and Go's testing package
	// panics if a test combines t.Setenv with t.Parallel().
	ws := newFixtureRepo(t)
	headBefore, err := exec.Command("git", "-C", ws, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("git rev-parse HEAD: %v", err)
	}

	r := runFactorydWithSpecAndFlags(t, ws, "commit", "true", "# fixture spec\nTests-Required: no -- integration fixture doesn't exercise tests_added\n", "30s", nil, []string{})

	if r.State != run.StateAccepted {
		t.Fatalf("state = %q, want %q", r.State, run.StateAccepted)
	}
	if r.Branch == "" {
		t.Error("Branch is empty, want the isolated branch name recorded")
	}
	if r.WorkspacePath == r.ProjectPath {
		t.Errorf("WorkspacePath = %q, want it distinct from ProjectPath %q for an isolated run", r.WorkspacePath, r.ProjectPath)
	}
	if r.ProjectPath != ws {
		t.Errorf("ProjectPath = %q, want the original -workspace %q", r.ProjectPath, ws)
	}

	// The isolated worktree holds the real result and is left in place
	// (not rolled back) for an accepted run.
	if info, err := os.Stat(r.WorkspacePath); err != nil || !info.IsDir() {
		t.Fatalf("isolated worktree %q does not exist after an accepted run: %v", r.WorkspacePath, err)
	}
	assertClean(t, r.WorkspacePath)

	// The shared checkout never moved: same HEAD as before the run, still
	// clean -- the agent never touched it.
	headAfter, err := exec.Command("git", "-C", ws, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("git rev-parse HEAD: %v", err)
	}
	if string(headAfter) != string(headBefore) {
		t.Errorf("shared checkout HEAD changed from %q to %q; an isolated run must never advance the shared checkout", headBefore, headAfter)
	}
	assertClean(t, ws)
}

// TestIntegrationIsolateWorkspaceRollsBackOnQuarantine pins the plan's
// named "automatic rollback to last-known-good" made real by workspace
// isolation (internal/release.Rollback): a run that quarantines never
// touched the shared checkout, so rollback is discarding the isolated
// worktree and branch entirely, not resetting shared history.
func TestIntegrationIsolateWorkspacePreservesWorktreeOnQuarantine(t *testing.T) {
	// not parallel-safe: newFixtureRepo/newFixtureRepoWithoutBootstrapScaffold call
	// t.Setenv (GIT_CONFIG_GLOBAL/GIT_CONFIG_SYSTEM), and Go's testing package
	// panics if a test combines t.Setenv with t.Parallel().
	ws := newFixtureRepo(t)
	dataDir := t.TempDir()
	headBefore, err := exec.Command("git", "-C", ws, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("git rev-parse HEAD: %v", err)
	}

	specContent := "# Ticket: fixture\n\n## Out of scope\n\nAllowed-Files: content.txt\n"
	r := runFactorydWithSpecFlagsAndDataDir(t, ws, "commit_extra", "true", specContent, "30s", nil, []string{}, dataDir)

	if r.State != run.StateQuarantined {
		t.Fatalf("state = %q, want %q", r.State, run.StateQuarantined)
	}
	if r.Branch == "" {
		t.Error("Branch is empty, want the isolated branch name recorded even for a rejected run")
	}

	// NOT rolled back: unlike StateHalted, a quarantined run remains
	// override-eligible (run.ApplyOverride requires exactly
	// StateQuarantined) -- deleting the isolated branch here would make a
	// later override to accepted record a WorkspacePath/ResultSHA that no
	// longer identifies anything real (found via codex review round 1).
	if info, err := os.Stat(r.WorkspacePath); err != nil || !info.IsDir() {
		t.Fatalf("isolated worktree %q should still exist after quarantine (it remains override-eligible): %v", r.WorkspacePath, err)
	}
	branchList, err := exec.Command("git", "-C", ws, "branch", "--list", r.Branch).Output()
	if err != nil {
		t.Fatalf("git branch --list: %v", err)
	}
	if len(branchList) == 0 {
		t.Errorf("branch %q should still exist after quarantine, want it preserved", r.Branch)
	}

	// The shared checkout was never touched in the first place.
	headAfter, err := exec.Command("git", "-C", ws, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("git rev-parse HEAD: %v", err)
	}
	if string(headAfter) != string(headBefore) {
		t.Errorf("shared checkout HEAD changed from %q to %q; a quarantined isolated run must never touch it", headBefore, headAfter)
	}
	assertClean(t, ws)

	// An operator can still override this run to accepted, and the
	// isolated worktree the override's record now points at genuinely
	// still exists and holds the real result.
	overrideCmd := factorydCommand(t, "override",
		"-run", r.ID,
		"-data-dir", dataDir,
		"-by", "operator",
		"-reason", "reviewed manually: out-of-scope file was benign",
		"-state", "accepted",
	)
	if out, err := overrideCmd.CombinedOutput(); err != nil {
		t.Fatalf("factoryd override: %v: %s", err, out)
	}
	if info, err := os.Stat(r.WorkspacePath); err != nil || !info.IsDir() {
		t.Fatalf("isolated worktree %q must still exist after override to accepted: %v", r.WorkspacePath, err)
	}
}

// TestIntegrationIsolateWorkspaceRollsBackOnHalt pins that automatic
// rollback still applies to a genuinely terminal rejection -- unlike
// StateQuarantined, StateHalted is never override-eligible
// (run.ApplyOverride requires exactly StateQuarantined), so discarding
// the isolated worktree immediately is safe. Uses "rewind_and_commit",
// the existing fixture mode for the base_sha-ancestry check, which halts
// (not quarantines) the run.
func TestIntegrationIsolateWorkspaceRollsBackOnHalt(t *testing.T) {
	// not parallel-safe: newFixtureRepo/newFixtureRepoWithoutBootstrapScaffold call
	// t.Setenv (GIT_CONFIG_GLOBAL/GIT_CONFIG_SYSTEM), and Go's testing package
	// panics if a test combines t.Setenv with t.Parallel().
	ws := newFixtureRepo(t)
	// A second real commit, so base_sha (captured by factoryd) has an
	// actual ancestor for the rewind to discard -- newFixtureRepo's lone
	// root commit would otherwise equal base_sha itself, making the
	// rewind a no-op (see TestIntegrationHaltsWhenBaseSHANoLongerAnAncestor).
	if err := os.WriteFile(filepath.Join(ws, "second.txt"), []byte("second commit content\n"), 0o644); err != nil {
		t.Fatalf("write second.txt: %v", err)
	}
	if out, err := exec.Command("git", "-C", ws, "add", "-A").CombinedOutput(); err != nil {
		t.Fatalf("git add: %v: %s", err, out)
	}
	commitCmd := exec.Command("git", "-C", ws, "commit", "-q", "-m", "second commit, this becomes base_sha")
	commitCmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	if out, err := commitCmd.CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v: %s", err, out)
	}

	headBefore, err := exec.Command("git", "-C", ws, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("git rev-parse HEAD: %v", err)
	}

	dataDir := t.TempDir()
	r := runFactorydWithSpecFlagsAndDataDir(t, ws, "rewind_and_commit", "true", "# fixture spec\nTests-Required: no -- integration fixture doesn't exercise tests_added\n", "30s", nil, []string{}, dataDir)

	if r.State != run.StateHalted {
		t.Fatalf("state = %q, want %q", r.State, run.StateHalted)
	}
	if r.Branch == "" {
		t.Error("Branch is empty, want the isolated branch name recorded even for a halted run")
	}

	if _, err := os.Stat(r.WorkspacePath); err == nil {
		t.Errorf("isolated worktree %q still exists after halt, want it rolled back", r.WorkspacePath)
	} else if !os.IsNotExist(err) {
		t.Errorf("unexpected error checking rolled-back worktree: %v", err)
	}

	// The regression test for a real GitHub Codex App review finding on
	// this PR: build_app.py wrote BUILD_REPORT.md into the isolated
	// worktree before this halt, and the rollback above deletes that
	// worktree outright -- without retention wired into the rollback
	// defer itself (loadAgentEvidence's own call is never reached on this
	// early-halt path), the report would be lost with nothing to read
	// even though this run's own agent genuinely produced one.
	retainedReport, err := os.ReadFile(filepath.Join(run.Dir(dataDir, r.ID), "BUILD_REPORT.md"))
	if err != nil {
		t.Fatalf("read retained BUILD_REPORT.md: %v", err)
	}
	if !strings.Contains(string(retainedReport), "Rewound and committed") {
		t.Errorf("retained BUILD_REPORT.md = %q, want it to contain the fixture's own report text", retainedReport)
	}
	branchList, err := exec.Command("git", "-C", ws, "branch", "--list", r.Branch).Output()
	if err != nil {
		t.Fatalf("git branch --list: %v", err)
	}
	if len(branchList) != 0 {
		t.Errorf("branch %q still exists after rollback, want it deleted", r.Branch)
	}

	headAfter, err := exec.Command("git", "-C", ws, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("git rev-parse HEAD: %v", err)
	}
	if string(headAfter) != string(headBefore) {
		t.Errorf("shared checkout HEAD changed from %q to %q; a halted isolated run must never touch it", headBefore, headAfter)
	}
	assertClean(t, ws)
}

// TestIntegrationIsolateWorkspaceRejectsDataDirInsideWorkspace pins the
// fix for a real finding from codex review (round 1): -data-dir defaults
// to the relative "data", which resolves inside -workspace whenever
// factoryd runs with its working directory under the target checkout.
// Creating the isolated worktree there would leave it behind inside the
// very checkout isolation exists to protect. Rejected outright, before
// wsisolation.Prepare ever runs `git worktree add` -- and, per a codex
// review round 3 finding, before any run directory, spec snapshot, or
// run.json is created either, so nothing is left behind inside the
// shared checkout even by the rejection itself. No run.json exists for
// this outcome at all, so this test drives the binary directly rather
// than through a helper that expects one.
func TestIntegrationIsolateWorkspaceRejectsDataDirInsideWorkspace(t *testing.T) {
	// not parallel-safe: newFixtureRepo/newFixtureRepoWithoutBootstrapScaffold call
	// t.Setenv (GIT_CONFIG_GLOBAL/GIT_CONFIG_SYSTEM), and Go's testing package
	// panics if a test combines t.Setenv with t.Parallel().
	ws := newFixtureRepo(t)
	dataDir := filepath.Join(ws, "factoryd-data")
	specPath := filepath.Join(t.TempDir(), "spec.md")
	if err := os.WriteFile(specPath, []byte("# fixture spec\nTests-Required: no -- integration fixture doesn't exercise tests_added\n"), 0o644); err != nil {
		t.Fatalf("write spec: %v", err)
	}
	scriptPath, err := filepath.Abs("testdata/fake_build_app.sh")
	if err != nil {
		t.Fatalf("abs script path: %v", err)
	}

	cmd := factorydCommand(t,
		"-ticket", "fixture-ticket",
		"-sandbox-image", fakeSandboxImage,
		"-workspace", ws,
		"-spec", specPath,
		"-build-app-interpreter", "/bin/sh",
		"-build-app-script", scriptPath,
		"-verify-command", "true",
		"-data-dir", dataDir,
	)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected a nonzero exit for -data-dir inside -workspace, got success: %s", out)
	}
	// This invocation names -sandbox-image explicitly (this package's own
	// fake docker, not the canonical default), so the containment
	// violation is reported by the sandboxImageExplicit branch of the
	// rejection, not the "resolves inside -workspace" message a caller
	// who left -sandbox-image unset would get.
	if !strings.Contains(string(out), "-sandbox-image requires -data-dir outside -workspace") {
		t.Errorf("output = %q, want it to name the containment violation", out)
	}
	if _, err := os.Stat(dataDir); err == nil {
		t.Errorf("data dir %q was created inside the shared checkout despite rejection, want no artifacts left behind", dataDir)
	} else if !os.IsNotExist(err) {
		t.Errorf("unexpected error checking for leaked artifacts: %v", err)
	}
}

// TestIntegrationRunRefusesWhenDataDirIsNotMountVisible is the
// regression test for a real gap found in review: `factoryd <run>` ran
// no doctor preflight at all before this fix
// (confirmed via `grep -n "runDoctorChecks\|doctorChecksFor"
// run_ticket.go`, zero matches), so a -data-dir the configured Docker
// backend doesn't actually share into its containers (colima's default
// $HOME-only shared mount, the same trap -workspace's own
// doctorCheckMountVisibility already guards against elsewhere) only
// surfaced minutes into a real sandboxed build. -sandbox-docker here
// names a nonexistent binary -- the same
// "factoryd-doctor-test-nonexistent-binary" fixture pattern
// doctorCheckMountVisibility's own existing unit tests use -- so the
// mount probe fails deterministically without a real Docker daemon.
func TestIntegrationRunRefusesWhenDataDirIsNotMountVisible(t *testing.T) {
	// not parallel-safe: newFixtureRepo (via testfixture.NewGitRepo) calls
	// t.Setenv, which panics if called after t.Parallel().
	ws := newFixtureRepo(t)
	dataDir := t.TempDir()
	specPath := filepath.Join(t.TempDir(), "spec.md")
	if err := os.WriteFile(specPath, []byte("# fixture spec\nTests-Required: no -- integration fixture doesn't exercise tests_added\n"), 0o644); err != nil {
		t.Fatalf("write spec: %v", err)
	}
	scriptPath, err := filepath.Abs("testdata/fake_build_app.sh")
	if err != nil {
		t.Fatalf("abs script path: %v", err)
	}

	cmd := factorydCommand(t,
		"-ticket", "fixture-ticket",
		"-sandbox-image", fakeSandboxImage,
		"-workspace", ws,
		"-spec", specPath,
		"-build-app-interpreter", "/bin/sh",
		"-build-app-script", scriptPath,
		"-verify-command", "true",
		"-data-dir", dataDir,
	)
	// -sandbox-docker is session-config only (no CLI flag exists for it,
	// per TestMain's own doc comment) -- pointed at a nonexistent binary
	// so the mount probe fails deterministically, overriding the
	// package-wide fake_docker.sh default every other subprocess in this
	// suite inherits.
	cmd.Env = append(append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null"),
		isolatedSessionConfigEnv(t, "sandbox_docker: factoryd-doctor-test-nonexistent-binary\n")...)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected a nonzero exit for a non-mount-visible -data-dir, got success: %s", out)
	}
	if !strings.Contains(string(out), "mount visibility (-data-dir reachable inside a container)") {
		t.Errorf("output = %q, want it to name the -data-dir mount-visibility check", out)
	}
	if !strings.Contains(string(out), "colima") {
		t.Errorf("output = %q, want it to name the colima $HOME-only cause", out)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "runs")); !os.IsNotExist(err) {
		t.Errorf("runs dir under -data-dir exists (stat err = %v), want the run rejected before any run record was written", err)
	}
}

// TestIntegrationRunSkipsDataDirMountCheckWhenDataDirDoesNotExistYet
// proves the new -data-dir mount-visibility check (see
// TestIntegrationRunRefusesWhenDataDirIsNotMountVisible above) does not
// itself create -data-dir to run its probe: doctorCheckMountVisibility
// requires its target to already exist rather than create it (its own
// doc comment), and run_ticket.go's project-bootstrap preflight further
// down is deliberately ordered to avoid creating -data-dir as a side
// effect before a run is accepted (see that preflight's own doc comment)
// -- this check follows the same rule. -skip-project-check bypasses that
// later preflight so the run fails for a different, unrelated reason
// (the fake docker binary is still unreachable for the sandboxed build
// itself), proving control reached past this check rather than being
// rejected by it.
func TestIntegrationRunSkipsDataDirMountCheckWhenDataDirDoesNotExistYet(t *testing.T) {
	// not parallel-safe: newFixtureRepo (via testfixture.NewGitRepo) calls
	// t.Setenv, which panics if called after t.Parallel().
	ws := newFixtureRepo(t)
	dataDir := filepath.Join(t.TempDir(), "not-created-yet")
	specPath := filepath.Join(t.TempDir(), "spec.md")
	if err := os.WriteFile(specPath, []byte("# fixture spec\nTests-Required: no -- integration fixture doesn't exercise tests_added\n"), 0o644); err != nil {
		t.Fatalf("write spec: %v", err)
	}
	scriptPath, err := filepath.Abs("testdata/fake_build_app.sh")
	if err != nil {
		t.Fatalf("abs script path: %v", err)
	}

	cmd := factorydCommand(t,
		"-ticket", "fixture-ticket",
		"-sandbox-image", fakeSandboxImage,
		"-workspace", ws,
		"-spec", specPath,
		"-build-app-interpreter", "/bin/sh",
		"-build-app-script", scriptPath,
		"-verify-command", "true",
		"-data-dir", dataDir,
		"-skip-project-check",
	)
	// -sandbox-docker is session-config only -- see the sibling test's own
	// comment on isolatedSessionConfigEnv.
	cmd.Env = append(append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null"),
		isolatedSessionConfigEnv(t, "sandbox_docker: factoryd-doctor-test-nonexistent-binary\n")...)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected a nonzero exit (the fake docker binary is unreachable for the real sandboxed build), got success: %s", out)
	}
	if strings.Contains(string(out), "mount visibility (-data-dir reachable inside a container)") {
		t.Errorf("output = %q, want the mount check skipped (not run, not failed) since -data-dir did not exist yet", out)
	}
}

// TestIntegrationIsolateWorkspaceDefaultStepsAsideForDataDirInsideWorkspace,
// TestIntegrationProjectBootstrapPreflightRecordAvoidsInWorkspaceDataDir, and
// TestIntegrationProjectBootstrapPreflightRecordAvoidsInWorkspaceDataDirWithIsolationDisabled
// were deleted: all three depended on a -data-dir resolving inside
// -workspace being tolerated for an implicitly-sandboxed run (the
// "isolation silently steps aside" case, reachable in practice only
// through -allow-unsandboxed, since a default invocation was already
// sandboxed by 2026-09-04 and therefore already hard-rejected this exact
// configuration). Sandboxing is now unconditional with no such escape, so
// a -data-dir inside -workspace is *always* a hard containment violation,
// synchronous, before wsisolation.Prepare, the project-bootstrap
// preflight, or build_app.py ever run -- the "steps aside" and
// "reaches project-bootstrap" scenarios these three tests built on are no
// longer reachable by any configuration.
// TestIntegrationIsolateWorkspaceRejectsRelativeDataDirInsideWorkspace is
// the regression test for a real finding from codex review (round 3,
// 2026-08-28): the containment check's first fix resolved -data-dir via
// filepath.EvalSymlinks, but EvalSymlinks on a *relative* path returns a
// relative result, which can never match the always-absolute -workspace
// in the prefix check -- silently defeating the check for exactly
// -data-dir's own relative default ("data"), the single most common way
// to hit this in the first place. Runs the binary with its working
// directory set to ws itself and a relative "data" -data-dir, exactly
// the scenario the original finding named.
func TestIntegrationIsolateWorkspaceRejectsRelativeDataDirInsideWorkspace(t *testing.T) {
	// not parallel-safe: newFixtureRepo/newFixtureRepoWithoutBootstrapScaffold call
	// t.Setenv (GIT_CONFIG_GLOBAL/GIT_CONFIG_SYSTEM), and Go's testing package
	// panics if a test combines t.Setenv with t.Parallel().
	ws := newFixtureRepo(t)
	specPath := filepath.Join(t.TempDir(), "spec.md")
	if err := os.WriteFile(specPath, []byte("# fixture spec\nTests-Required: no -- integration fixture doesn't exercise tests_added\n"), 0o644); err != nil {
		t.Fatalf("write spec: %v", err)
	}
	scriptPath, err := filepath.Abs("testdata/fake_build_app.sh")
	if err != nil {
		t.Fatalf("abs script path: %v", err)
	}

	cmd := factorydCommand(t,
		"-ticket", "fixture-ticket",
		"-sandbox-image", fakeSandboxImage,
		"-workspace", ws,
		"-spec", specPath,
		"-build-app-interpreter", "/bin/sh",
		"-build-app-script", scriptPath,
		"-verify-command", "true",
		"-data-dir", "data", // relative, resolved against cmd.Dir below
	)
	cmd.Dir = ws
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected a nonzero exit for a relative -data-dir inside -workspace, got success: %s", out)
	}
	// This invocation names -sandbox-image explicitly (this package's own
	// fake docker, not the canonical default), so the containment
	// violation is reported by the sandboxImageExplicit branch of the
	// rejection, not the "resolves inside -workspace" message a caller
	// who left -sandbox-image unset would get.
	if !strings.Contains(string(out), "-sandbox-image requires -data-dir outside -workspace") {
		t.Errorf("output = %q, want it to name the containment violation", out)
	}
}

// TestIntegrationIsolateWorkspaceDoesNotTouchSharedStaleEvidence pins the
// fix for a real finding from codex review (round 1): the early stale-BUILD_EVIDENCE.json cleanup ran against the shared
// -workspace checkout unconditionally, before the isolated worktree
// (which can never have stale evidence in the first place, being freshly
// checked out) was even prepared -- mutating exactly the checkout
// isolation exists to leave untouched.
func TestIntegrationIsolateWorkspaceDoesNotTouchSharedStaleEvidence(t *testing.T) {
	// not parallel-safe: newFixtureRepo/newFixtureRepoWithoutBootstrapScaffold call
	// t.Setenv (GIT_CONFIG_GLOBAL/GIT_CONFIG_SYSTEM), and Go's testing package
	// panics if a test combines t.Setenv with t.Parallel().
	ws := newFixtureRepo(t)
	stalePath := filepath.Join(ws, "BUILD_EVIDENCE.json")
	staleContent := []byte(`{"generated":"a-prior-run","succeeded":true}`)
	if err := os.WriteFile(stalePath, staleContent, 0o644); err != nil {
		t.Fatalf("write stale BUILD_EVIDENCE.json: %v", err)
	}

	r := runFactorydWithSpecAndFlags(t, ws, "commit", "true", "# fixture spec\nTests-Required: no -- integration fixture doesn't exercise tests_added\n", "30s", nil, []string{})

	if r.State != run.StateAccepted {
		t.Fatalf("state = %q, want %q", r.State, run.StateAccepted)
	}
	got, err := os.ReadFile(stalePath)
	if err != nil {
		t.Fatalf("shared checkout's own BUILD_EVIDENCE.json was removed, want it left untouched: %v", err)
	}
	if string(got) != string(staleContent) {
		t.Errorf("shared checkout's BUILD_EVIDENCE.json changed, want it untouched:\ngot:  %s\nwant: %s", got, staleContent)
	}
}

// TestIntegrationIsolateWorkspaceClearsTrackedStaleEvidence is the
// regression test for a real GitHub Codex App review finding, 2026-08-29:
// the reasoning that skipped stale-evidence cleanup for isolation
// ("a fresh worktree can't have stale evidence") only holds for *untracked*
// residue. `git worktree add` populates the new checkout from baseSHA like
// any other checkout, so if BUILD_EVIDENCE.json was ever committed into the
// repository's own history (an older harness, or an accidental `git add -A`
// sweep), a fresh isolated worktree checks that stale, tracked copy out --
// exactly the file loadAgentEvidence reads to attribute provider/model/
// rounds to *this* run.
func TestIntegrationIsolateWorkspaceClearsTrackedStaleEvidence(t *testing.T) {
	// not parallel-safe: newFixtureRepo/newFixtureRepoWithoutBootstrapScaffold call
	// t.Setenv (GIT_CONFIG_GLOBAL/GIT_CONFIG_SYSTEM), and Go's testing package
	// panics if a test combines t.Setenv with t.Parallel().
	ws := newFixtureRepo(t)
	stalePath := filepath.Join(ws, "BUILD_EVIDENCE.json")
	staleContent := []byte(`{"generated":"a-prior-run","review_policy":"advisory","provider":"stale-provider","model":"stale-model","succeeded":true,"stopped_reason":"stale","rounds":[]}`)
	if err := os.WriteFile(stalePath, staleContent, 0o644); err != nil {
		t.Fatalf("write tracked BUILD_EVIDENCE.json: %v", err)
	}
	if out, err := exec.Command("git", "-C", ws, "add", "BUILD_EVIDENCE.json").CombinedOutput(); err != nil {
		t.Fatalf("git add BUILD_EVIDENCE.json: %v: %s", err, out)
	}
	if out, err := exec.Command("git", "-C", ws, "-c", "user.email=t@example.com", "-c", "user.name=t", "commit", "-m", "track stale evidence").CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v: %s", err, out)
	}

	// commit_other leaves BUILD_EVIDENCE.json completely untouched (only
	// the "commit" mode writes a fresh one), so this run's isolated
	// worktree would otherwise still carry the tracked stale copy checked
	// out from baseSHA above.
	r := runFactorydWithSpecAndFlags(t, ws, "commit_other", "true", "# fixture spec\nTests-Required: no -- integration fixture doesn't exercise tests_added\n", "30s", nil, []string{})

	if r.AgentEvidence != nil {
		t.Errorf("AgentEvidence = %+v, want nil (build_app.py wrote no fresh evidence, and the tracked stale copy should have been removed from the isolated worktree before it ran)", r.AgentEvidence)
	}
}

// TestIntegrationIsolateWorkspaceRollsBackWhenStaleEvidenceCleanupFails is
// the regression test for a real GitHub Codex App review finding,
// 2026-08-29: an earlier version of the tracked-stale-evidence fix above
// removed BUILD_EVIDENCE.json from the isolated worktree *before* the
// rollback defer was registered. A failure in that removal (e.g. the
// tracked path is a nonempty directory, which os.Remove refuses) halted the
// run while leaving the worktree and branch Prepare had just created fully
// stranded -- a halted run isn't override-eligible, and nothing else ever
// revisits them. The cleanup now runs after the defer is registered, so
// this failure path gets the same rollback every other post-Prepare
// failure already does.
func TestIntegrationIsolateWorkspaceRollsBackWhenStaleEvidenceCleanupFails(t *testing.T) {
	// not parallel-safe: newFixtureRepo/newFixtureRepoWithoutBootstrapScaffold call
	// t.Setenv (GIT_CONFIG_GLOBAL/GIT_CONFIG_SYSTEM), and Go's testing package
	// panics if a test combines t.Setenv with t.Parallel().
	ws := newFixtureRepo(t)
	nonEmptyDir := filepath.Join(ws, "BUILD_EVIDENCE.json")
	if err := os.MkdirAll(nonEmptyDir, 0o755); err != nil {
		t.Fatalf("mkdir stale BUILD_EVIDENCE.json dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(nonEmptyDir, "inner.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write inner file: %v", err)
	}
	if out, err := exec.Command("git", "-C", ws, "add", "BUILD_EVIDENCE.json/inner.txt").CombinedOutput(); err != nil {
		t.Fatalf("git add: %v: %s", err, out)
	}
	if out, err := exec.Command("git", "-C", ws, "-c", "user.email=t@example.com", "-c", "user.name=t", "commit", "-m", "track a directory named BUILD_EVIDENCE.json").CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v: %s", err, out)
	}

	dataDir := t.TempDir()
	specPath := filepath.Join(t.TempDir(), "spec.md")
	if err := os.WriteFile(specPath, []byte("# fixture spec\nTests-Required: no -- integration fixture doesn't exercise tests_added\n"), 0o644); err != nil {
		t.Fatalf("write spec: %v", err)
	}
	scriptPath, err := filepath.Abs("testdata/fake_build_app.sh")
	if err != nil {
		t.Fatalf("abs script path: %v", err)
	}

	runID := "evidence-cleanup-fail-run"
	cmd := factorydCommand(t,
		"-ticket", "fixture-ticket",
		"-sandbox-image", fakeSandboxImage,
		"-workspace", ws,
		"-spec", specPath,
		"-build-app-interpreter", "/bin/sh",
		"-build-app-script", scriptPath,
		"-verify-command", "true",
		"-data-dir", dataDir,
		"-run-id", runID,
	)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null", "FAKE_BUILD_APP_MODE=commit")
	out, _ := cmd.CombinedOutput()
	if !strings.Contains(string(out), "remove stale BUILD_EVIDENCE.json from isolated worktree") {
		t.Fatalf("expected the run to fail removing the tracked, nonempty BUILD_EVIDENCE.json directory, got: %s", out)
	}

	branchList, err := exec.Command("git", "-C", ws, "branch", "--list", "factoryd/"+runID).Output()
	if err != nil {
		t.Fatalf("git branch --list: %v", err)
	}
	if len(branchList) != 0 {
		t.Errorf("branch %q still exists after evidence-cleanup failure, want it rolled back: %s", "factoryd/"+runID, branchList)
	}
	worktreeList, err := exec.Command("git", "-C", ws, "worktree", "list").Output()
	if err != nil {
		t.Fatalf("git worktree list: %v", err)
	}
	if strings.Contains(string(worktreeList), runID) {
		t.Errorf("worktree for %q still registered after evidence-cleanup failure, want it rolled back:\n%s", runID, worktreeList)
	}
}

// TestIntegrationIsolateWorkspaceRollsBackWhenTerminalSaveFails is the
// regression test for a real GitHub Codex App review finding, 2026-08-29:
// the rollback defer decided whether to skip rollback purely from r.State,
// which is already set to StateAccepted/StateQuarantined in memory by the
// time the one terminal save runs. If that save itself fails (a read-only
// or full data volume), the durable run.json stays stuck at whatever state
// was last saved (typically "verifying") -- not override-eligible -- while
// the defer, seeing only the in-memory terminal state, skipped rollback
// anyway, leaking the worktree and branch with no path back to them. This
// makes the run directory read-only right after it reaches "verifying"
// (the last state saved before the terminal transition), forcing the
// terminal save to fail with a real permission error, then proves the
// isolated worktree and branch are cleaned up anyway.
