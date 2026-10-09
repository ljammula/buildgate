package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"buildgate/internal/run"
)

// None of the tests in this file call t.Parallel(): every one of them
// starts from newFixtureRepo/newFixtureRepoWithoutBootstrapScaffold, which
// call testfixture.NewGitRepo, which calls t.Setenv(GIT_CONFIG_GLOBAL/
// GIT_CONFIG_SYSTEM) -- and testing.T panics if t.Setenv is ever reached
// from a parallel test. Confirmed live: adding t.Parallel() here made
// TestIntegrationRequiredContentPresentGateQuarantinesCosmeticChange panic
// with "testing: test using t.Setenv ... can not use t.Parallel".

func TestIntegrationIsolateWorkspaceRollsBackWhenTerminalSaveFails(t *testing.T) {
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

	runID := "terminal-save-fail-run"
	cmd := factorydCommand(t,
		"-ticket", "fixture-ticket",
		"-sandbox-image", fakeSandboxImage,
		"-workspace", ws,
		"-spec", specPath,
		"-build-app-interpreter", "/bin/sh",
		"-build-app-script", scriptPath,
		// Not the instant "true" every other fixture in this file uses
		// (found via review round 3, CI flake): this test's own polling
		// loop below needs a real window between verify.log/verify.log.pid
		// both existing and the process moving on to its terminal save --
		// under load (many packages' tests running concurrently, as CI
		// does but a local `go test` of this package alone mostly doesn't)
		// an instant verify command let the whole run reach "FINAL
		// state=accepted" before the loop's very first poll ever observed
		// either file, even at a 2ms polling interval. A short, deterministic
		// sleep gives real headroom without meaningfully slowing the test.
		"-verify-command", "sleep 0.3",
		// -full-suite-command none: this test is about a terminal-save
		// failure after canonical_verify, not about full_suite_verify --
		// without this opt-out, the verify-command substitution
		// would try to run a second "sleep 0.3" as full_suite_verify
		// against the now-read-only run dir this test deliberately
		// creates, failing earlier (creating full_suite.log) with a
		// different error than the one this test asserts on.
		"-full-suite-command", "none",
		"-data-dir", dataDir,
		"-run-id", runID,
	)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null", "FAKE_BUILD_APP_MODE=commit")
	var out synchronizedBuffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Start(); err != nil {
		t.Fatalf("start factoryd: %v", err)
	}

	runDir := run.Dir(dataDir, runID)
	// Waiting for run.json's own State field to reach "verifying" is not
	// enough: that save happens *before* the verify subprocess even
	// starts (see the state=verifying save just above where verify.log is
	// first opened), so a chmod triggered by it can itself race the log
	// file's own creation under load -- caught live running this exact
	// test under the full suite's concurrency, where it failed one level
	// earlier than intended ("create log file: ... permission denied")
	// instead of at the terminal save this test means to target. Waiting
	// for verify.log to actually exist on disk instead removes that race:
	// once it exists, the verify subprocess (a fast, synchronous "true")
	// already has its file descriptor open and needs no further directory
	// permission to keep writing to it, so making the directory read-only
	// at that point can no longer fail anything before the terminal save.
	//
	// Sandboxing is unconditional, so verify always runs through
	// sandbox.Run now, not internal/runner.run -- there is no separate
	// verify.log.pid sidecar to also wait for (that was specific to
	// runner.run's own host-process bookkeeping); sandbox.Run's own
	// verify.log creation is the only file creation this chmod can race.
	//
	// On the Temporal path the Activity names that log
	// "<execution-key>.attempt-<n>-verify.log" (activityExecutionLogPath).
	verifyLogGlob := filepath.Join(runDir, "*.attempt-1-verify.log")
	deadline := time.Now().Add(5 * time.Second)
	madeReadOnly := false
	for time.Now().Before(deadline) {
		if logs, _ := filepath.Glob(verifyLogGlob); len(logs) > 0 {
			// A read-only run dir would also fail the Activities that
			// write evidence into it (collect_evidence's diff file) and
			// halt the workflow before its terminal save. A directory
			// squatting on run.json.tmp fails only run.Save's WriteFile,
			// which on the Temporal path is the terminal save
			// (applyRunWorkflowResult): no other save of r happens after
			// the workflow's Activities start.
			if err := os.Mkdir(filepath.Join(runDir, "run.json.tmp"), 0o750); err != nil {
				t.Fatalf("block run.json.tmp: %v", err)
			}
			madeReadOnly = true
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if !madeReadOnly {
		t.Fatalf("verify.log never appeared before the deadline; output so far:\n%s", out.String())
	}

	waitErr := cmd.Wait()
	// Restored before any assertion or cleanup below needs to read/remove
	// files in this directory, including t.TempDir()'s own cleanup.
	if err := os.Chmod(runDir, 0o750); err != nil {
		t.Fatalf("restore run dir permissions: %v", err)
	}
	if waitErr == nil {
		t.Fatalf("expected factoryd to exit nonzero after its terminal save failed, output:\n%s", out.String())
	}
	// Temporal's accepted path returns run.Save's error unwrapped.
	if !strings.Contains(out.String(), "write run: open ") || !strings.Contains(out.String(), filepath.Join("runs", runID, "run.json.tmp")+": is a directory") {
		t.Fatalf("expected output to mention the failed terminal save, got:\n%s", out.String())
	}

	branchList, err := exec.Command("git", "-C", ws, "branch", "--list", "factoryd/"+runID+"*").Output()
	if err != nil {
		t.Fatalf("git branch --list: %v", err)
	}
	if len(branchList) != 0 {
		t.Errorf("branch %q still exists after a failed terminal save, want it rolled back: %s", "factoryd/"+runID+"*", branchList)
	}
	worktreeList, err := exec.Command("git", "-C", ws, "worktree", "list").Output()
	if err != nil {
		t.Fatalf("git worktree list: %v", err)
	}
	if strings.Contains(string(worktreeList), runID) {
		t.Errorf("worktree for %q still registered after a failed terminal save, want it rolled back:\n%s", runID, worktreeList)
	}
}

// TestIntegrationIsolateWorkspaceRequiresReachableTemporalServer pins that
// every build runs on Temporal: a plain run whose Temporal server is
// unreachable halts with a durable record and says builds need Temporal,
// rather than executing anywhere else.
func TestIntegrationIsolateWorkspaceRequiresReachableTemporalServer(t *testing.T) {
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
	)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected a nonzero exit against an unreachable Temporal server, got success: %s", out)
	}
	if want := "Temporal at localhost:0 is unreachable: "; !strings.Contains(string(out), want) {
		t.Errorf("output = %q, want it to contain %q", out, want)
	}
	if want := "; builds run only on Temporal (factoryd doctor -fix starts it)"; !strings.Contains(string(out), want) {
		t.Errorf("output = %q, want it to contain %q", out, want)
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
	// Nothing should have been created against the shared checkout: this
	// halts before ever calling wsisolation.Prepare, so no worktree or
	// branch exists anywhere and the shared checkout is untouched.
	assertClean(t, ws)
}

// TestIntegrationIsolateWorkspaceRejectsPriorRun keeps the fail-closed
// validation for a missing predecessor while the valid isolated chain is
// covered by TestIntegrationSliceChainAcceptsIsolatedPriorRun.
func TestIntegrationIsolateWorkspaceRejectsPriorRun(t *testing.T) {
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
		"-prior-run", "some-prior-run",
	)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected a nonzero exit combining isolation with -prior-run, got success: %s", out)
	}
	if !strings.Contains(string(out), "load prior run") || !strings.Contains(string(out), "some-prior-run") {
		t.Errorf("output = %q, want it to name the missing predecessor", out)
	}
}

// TestIntegrationIsolateWorkspaceRejectsSymlinkedDataDirInsideWorkspace
// is the regression test for a real finding from codex review (round 2,
// 2026-08-28): the containment check used plain filepath.Abs, which
// leaves a symlink component unresolved, so a symlinked path to -workspace
// could pass the lexical prefix check even though the worktree is
// physically created inside the shared checkout. Here -workspace is
// approached through a symlink pointing at the real checkout, and
// -data-dir's "workspaces" subdirectory resolves (once symlinks are
// followed, as Prepare's own git commands would) inside that same real
// checkout -- must be rejected exactly like the non-symlinked case.
func TestIntegrationIsolateWorkspaceRejectsSymlinkedDataDirInsideWorkspace(t *testing.T) {
	ws := newFixtureRepo(t)
	wsSymlink := filepath.Join(t.TempDir(), "ws-symlink")
	if err := os.Symlink(ws, wsSymlink); err != nil {
		t.Fatalf("symlink workspace: %v", err)
	}
	dataDir := filepath.Join(wsSymlink, "factoryd-data") // resolves inside ws via the symlink
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
		t.Fatalf("expected a nonzero exit for a symlinked -data-dir inside -workspace, got success: %s", out)
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

// TestIntegrationIsolateWorkspaceRejectsSymlinkedWorkspacesChildInsideWorkspace
// is the regression test for a real GitHub Codex App review finding,
// 2026-08-29: the containment check resolves -data-dir itself, but the
// "workspaces" child it then joins on was left unresolved -- if that exact
// child already exists as a symlink into the target repository (a
// perfectly ordinary-looking -data-dir with one pre-existing symlinked
// subdirectory), the lexical prefix check against the unresolved joined
// path passes even though `git worktree add` (which follows the link)
// would still create the worktree physically inside the shared checkout.
// Here -data-dir itself is an ordinary directory outside -workspace, but
// its own "workspaces" child is a symlink resolving inside -workspace --
// must be rejected exactly like a symlinked -data-dir itself.
func TestIntegrationIsolateWorkspaceRejectsSymlinkedWorkspacesChildInsideWorkspace(t *testing.T) {
	ws := newFixtureRepo(t)
	dataDir := t.TempDir()
	if err := os.Symlink(ws, filepath.Join(dataDir, "workspaces")); err != nil {
		t.Fatalf("symlink data-dir's workspaces child into -workspace: %v", err)
	}
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
		t.Fatalf("expected a nonzero exit for a symlinked workspaces child inside -workspace, got success: %s", out)
	}
	resolvedWS, err := filepath.EvalSymlinks(ws)
	if err != nil {
		t.Fatalf("resolve workspace: %v", err)
	}
	want := fmt.Sprintf("the worktree directory %q resolves inside -workspace %q; use a -data-dir outside the target checkout", resolvedWS, resolvedWS)
	if !strings.Contains(string(out), want) {
		t.Errorf("output = %q, want it to contain %q", out, want)
	}
}

// TestIntegrationIsolateWorkspaceRejectsGitUnsafeTicketName is the
// regression test for a real finding from codex review (round 2,
// 2026-08-28): -ticket is only ever validated as a safe *path* component
// (no "/", no ".."), which is not the same as a legal git ref name --
// e.g. two consecutive dots anywhere in the name (not just as a whole
// path segment) are illegal in a git ref but pass that path check. Must
// be caught before `git worktree add -b` ever runs, with a clear error,
// not surfaced as an opaque subprocess failure.
func TestIntegrationIsolateWorkspaceRejectsGitUnsafeTicketName(t *testing.T) {
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
		"-ticket", "foo..bar", // a valid path component, an illegal git ref (consecutive dots)
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
		t.Fatalf("expected a nonzero exit for a git-ref-unsafe -ticket, got success: %s", out)
	}
	if !strings.Contains(string(out), "not a valid git ref") {
		t.Errorf("output = %q, want it to name the invalid git ref", out)
	}
}

// TestIntegrationIsolateWorkspaceOverrideToHaltedRollsBack is the
// regression test for a real finding from codex review (round 2,
// 2026-08-28): the run-time rollback defer deliberately skips
// StateQuarantined so an operator override can still promote it to
// accepted, but an override *to* StateHalted is that decision resolved
// the other way -- and nothing else ever revisits the isolated worktree
// afterward. Without the fix, that worktree and branch were left behind
// indefinitely.
func TestIntegrationIsolateWorkspaceOverrideToHaltedRollsBack(t *testing.T) {
	ws := newFixtureRepo(t)
	dataDir := t.TempDir()

	specContent := "# Ticket: fixture\n\n## Out of scope\n\nAllowed-Files: content.txt\n"
	r := runFactorydWithSpecFlagsAndDataDir(t, ws, "commit_extra", "true", specContent, "30s", nil, []string{}, dataDir)
	if r.State != run.StateQuarantined {
		t.Fatalf("state = %q, want %q", r.State, run.StateQuarantined)
	}

	// What a failed round saved in the worktree must outlive it.
	roundLog := filepath.Join(r.WorkspacePath, ".pi-build-session", "feedback", "round-1", "verify.log")
	if err := os.MkdirAll(filepath.Dir(roundLog), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(roundLog, []byte("FAIL round one\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	overrideCmd := factorydCommand(t, "override",
		"-run", r.ID,
		"-data-dir", dataDir,
		"-by", "operator",
		"-reason", "reviewed manually: reject this run",
		"-state", "halted",
	)
	if out, err := overrideCmd.CombinedOutput(); err != nil {
		t.Fatalf("factoryd override: %v: %s", err, out)
	}

	if _, err := os.Stat(r.WorkspacePath); err == nil {
		t.Errorf("isolated worktree %q still exists after override to halted, want it rolled back", r.WorkspacePath)
	} else if !os.IsNotExist(err) {
		t.Errorf("unexpected error checking rolled-back worktree: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(run.Dir(dataDir, r.ID), "round-logs", "round-1", "verify.log")); err != nil || string(got) != "FAIL round one\n" {
		t.Errorf("retained round log = %q, %v, want the round's verify output", got, err)
	}
	branchList, err := exec.Command("git", "-C", ws, "branch", "--list", r.Branch).Output()
	if err != nil {
		t.Fatalf("git branch --list: %v", err)
	}
	if len(branchList) != 0 {
		t.Errorf("branch %q still exists after override-triggered rollback, want it deleted", r.Branch)
	}
}

// TestIntegrationRecordsPhase4Evidence checks the evidence fields added
// for the plan's Phase 4 (evidence hardening): the acceptance-oracle hash,
// the changed-file inventory, the diff size, and the verify-surface hash.
func TestIntegrationRecordsPhase4Evidence(t *testing.T) {
	ws := newFixtureRepo(t)
	r := runFactoryd(t, ws, "commit", "true")

	if r.State != run.StateAccepted {
		t.Fatalf("state = %q, want %q", r.State, run.StateAccepted)
	}
	if r.SpecSHA256 == "" {
		t.Error("SpecSHA256 is empty, want the ticket spec's content hash")
	}
	if !slices.Contains(r.ChangedFiles, "content.txt") {
		t.Errorf("ChangedFiles = %v, want it to include content.txt", r.ChangedFiles)
	}
	if r.DiffStat == nil || r.DiffStat.FilesChanged != 1 {
		t.Errorf("DiffStat = %+v, want FilesChanged=1", r.DiffStat)
	}
	// canonical_verify plus tests_added (which always runs -- see
	// runFactoryd's own opt-out injection) plus full_suite_verify: no
	// -full-suite-command/.factory.yml full_suite_command is configured
	// here, so it's substituted with the resolved verify command -- see
	// TestIntegrationFullSuiteDefaultsToVerifyCommand for that
	// substitution's own dedicated test. Only canonical_verify carries a
	// LogSHA256.
	if len(r.GateResults) != 3 || r.GateResults[0].LogSHA256 == "" {
		t.Fatalf("expected three gate results with the first carrying a non-empty LogSHA256, got %v", r.GateResults)
	}
}

// TestIntegrationFullSuiteVerifyAcceptsWhenDeclaredCommandPasses pins the
// happy path for -full-suite-command (gap 3 of the plan's 2026-08-28
// readiness review, the regression oracle): a declared full-suite command
// that passes is recorded as its own gate and does not block acceptance.
func TestIntegrationFullSuiteVerifyAcceptsWhenDeclaredCommandPasses(t *testing.T) {
	ws := newFixtureRepo(t)
	r := runFactorydWithSpecAndFlags(t, ws, "commit", "true", "# fixture spec\nTests-Required: no -- integration fixture doesn't exercise tests_added\n", "30s", nil, []string{"-full-suite-command", "true"})

	if r.State != run.StateAccepted {
		t.Fatalf("state = %q, want %q", r.State, run.StateAccepted)
	}
	var fullSuiteGate *run.GateResult
	for i := range r.GateResults {
		if r.GateResults[i].Check == "full_suite_verify" {
			fullSuiteGate = &r.GateResults[i]
		}
	}
	if fullSuiteGate == nil || !fullSuiteGate.Passed || fullSuiteGate.LogSHA256 == "" {
		t.Fatalf("expected a passing full_suite_verify gate with a log hash, got %+v", r.GateResults)
	}
}

func TestIntegrationFullSuiteVerifyCadenceRunsEveryNthSlice(t *testing.T) {
	ws := newFixtureRepo(t)
	dataDir := t.TempDir()
	flags := []string{"-full-suite-command", "false", "-full-suite-cadence", "2"}
	first := runFactorydWithSpecFlagsAndDataDir(t, ws, "commit", "true", "# fixture spec\nTests-Required: no -- integration fixture doesn't exercise tests_added\n", "30s", nil, flags, dataDir)
	if first.State != run.StateAccepted {
		t.Fatalf("first run state = %q, want %q because cadence starts at slice 1", first.State, run.StateAccepted)
	}
	if !first.FullSuiteConfigured || first.FullSuiteCadence != 2 || first.FullSuiteSlice != 1 || first.FullSuiteScheduled {
		t.Fatalf("first full-suite cadence evidence = configured:%v cadence:%d slice:%d scheduled:%v, want configured cadence=2 slice=1 not scheduled", first.FullSuiteConfigured, first.FullSuiteCadence, first.FullSuiteSlice, first.FullSuiteScheduled)
	}
	for _, gate := range first.GateResults {
		if gate.Check == "full_suite_verify" {
			t.Fatalf("first run GateResults = %+v, want cadence to skip full_suite_verify", first.GateResults)
		}
	}

	secondFlags := append(append([]string{}, flags...), "-prior-run", first.ID)
	second := runFactorydWithSpecFlagsAndDataDir(t, ws, "commit", "true", "# fixture spec\nTests-Required: no -- integration fixture doesn't exercise tests_added\n", "30s", nil, secondFlags, dataDir)
	if second.State != run.StateQuarantined {
		t.Fatalf("second run state = %q, want %q because cadence runs on slice 2", second.State, run.StateQuarantined)
	}
	if !second.FullSuiteConfigured || second.FullSuiteCadence != 2 || second.FullSuiteSlice != 2 || !second.FullSuiteScheduled {
		t.Fatalf("second full-suite cadence evidence = configured:%v cadence:%d slice:%d scheduled:%v, want configured cadence=2 slice=2 scheduled", second.FullSuiteConfigured, second.FullSuiteCadence, second.FullSuiteSlice, second.FullSuiteScheduled)
	}
	for _, gate := range second.GateResults {
		if gate.Check == "full_suite_verify" {
			if gate.Passed {
				t.Fatalf("second run full_suite_verify gate = %+v, want failure from false command", gate)
			}
			return
		}
	}
	t.Fatalf("second run GateResults = %+v, want full_suite_verify on cadence boundary", second.GateResults)
}

// TestIntegrationFullSuiteVerifyQuarantinesOnRegression is gap 3's actual
// target scenario: the ticket's own targeted -verify-command passes, but a
// declared -full-suite-command fails — this slice appears to have
// regressed a different, already-accepted slice's test. The run must
// quarantine even though its own canonical_verify passed, since
// full_suite_verify is the only gate that can catch this.
func TestIntegrationFullSuiteVerifyQuarantinesOnRegression(t *testing.T) {
	ws := newFixtureRepo(t)
	r := runFactorydWithSpecAndFlags(t, ws, "commit", "true", "# fixture spec\nTests-Required: no -- integration fixture doesn't exercise tests_added\n", "30s", nil, []string{"-full-suite-command", "false"})

	if r.State != run.StateQuarantined {
		t.Fatalf("state = %q, want %q", r.State, run.StateQuarantined)
	}
	var canonicalGate, fullSuiteGate *run.GateResult
	for i := range r.GateResults {
		switch r.GateResults[i].Check {
		case "canonical_verify":
			canonicalGate = &r.GateResults[i]
		case "full_suite_verify":
			fullSuiteGate = &r.GateResults[i]
		}
	}
	if canonicalGate == nil || !canonicalGate.Passed {
		t.Fatalf("expected a passing canonical_verify gate, got %+v", r.GateResults)
	}
	if fullSuiteGate == nil || fullSuiteGate.Passed {
		t.Fatalf("expected a failing full_suite_verify gate, got %+v", r.GateResults)
	}
}

// TestIntegrationSecurityCommandQuarantinesWithGateName is the named-gate
// mechanism's case: a failing -security-command quarantines the run with
// "security_audit" as the cause, the same way -full-suite-command's
// failure quarantines on "full_suite_verify"
// (TestIntegrationFullSuiteVerifyQuarantinesOnRegression, above). The
// ticket's own -verify-command passes; only the named gate fails.
func TestIntegrationSecurityCommandQuarantinesWithGateName(t *testing.T) {
	ws := newFixtureRepo(t)
	r := runFactorydWithSpecAndFlags(t, ws, "commit", "true", "# fixture spec\nTests-Required: no -- integration fixture doesn't exercise tests_added\n", "30s", nil, []string{"-security-command", "false"})

	if r.State != run.StateQuarantined {
		t.Fatalf("state = %q, want %q", r.State, run.StateQuarantined)
	}
	var canonicalGate, securityGate *run.GateResult
	for i := range r.GateResults {
		switch r.GateResults[i].Check {
		case "canonical_verify":
			canonicalGate = &r.GateResults[i]
		case "security_audit":
			securityGate = &r.GateResults[i]
		}
	}
	if canonicalGate == nil || !canonicalGate.Passed {
		t.Fatalf("expected a passing canonical_verify gate, got %+v", r.GateResults)
	}
	if securityGate == nil || securityGate.Passed || securityGate.LogSHA256 == "" {
		t.Fatalf("expected a failing security_audit gate with a log hash, got %+v", r.GateResults)
	}
}

// TestIntegrationLintAndUnitTestCommandsBothRecordPassingGates is the
// named-gate mechanism's happy-path case: two configured named gates
// (lint, unit_tests) that both
// pass are each recorded as their own gate with a log hash, and neither
// blocks acceptance; the two left unconfigured (security_audit,
// integration_tests) are simply absent from GateResults.
func TestIntegrationLintAndUnitTestCommandsBothRecordPassingGates(t *testing.T) {
	ws := newFixtureRepo(t)
	r := runFactorydWithSpecAndFlags(t, ws, "commit", "true", "# fixture spec\nTests-Required: no -- integration fixture doesn't exercise tests_added\n", "30s", nil, []string{"-lint-command", "true", "-unit-test-command", "true"})

	if r.State != run.StateAccepted {
		t.Fatalf("state = %q, want %q", r.State, run.StateAccepted)
	}
	seen := map[string]run.GateResult{}
	for _, g := range r.GateResults {
		seen[g.Check] = g
	}
	for _, check := range []string{"lint", "unit_tests"} {
		g, ok := seen[check]
		if !ok || !g.Passed || g.LogSHA256 == "" {
			t.Errorf("expected a passing %s gate with a log hash, got %+v (present=%v)", check, g, ok)
		}
	}
	for _, check := range []string{"security_audit", "integration_tests"} {
		if _, ok := seen[check]; ok {
			t.Errorf("expected no %s gate (not configured), got %+v", check, seen[check])
		}
	}
}

// TestIntegrationFullSuiteVerifyMutationIsCapturedInEvidence pins a real P1
// finding from GitHub's own Codex App review round on PR #33: running
// -full-suite-command after final evidence collection (ResultSHA,
// ChangedFiles, diff, dependency changes) meant a full-suite command that
// itself mutates the checkout (codegen, formatting, snapshot updates,
// coverage artifacts) and exits 0 could leave an accepted worktree
// containing changes invisible to every one of those evidence fields and
// to diff_scope. The fix moved -full-suite-command's execution before that
// collection, so its own dirt is swept into the same safety-net commit
// verification's own output already uses and is reflected everywhere.
func TestIntegrationFullSuiteVerifyMutationIsCapturedInEvidence(t *testing.T) {
	ws := newFixtureRepo(t)
	r := runFactorydWithSpecAndFlags(t, ws, "commit", "true", "# fixture spec\nTests-Required: no -- integration fixture doesn't exercise tests_added\n", "30s", nil, []string{"-full-suite-command", "echo mutated > full_suite_side_effect.txt"})

	if r.State != run.StateAccepted {
		t.Fatalf("state = %q, want %q", r.State, run.StateAccepted)
	}
	if !r.CommittedByFactoryd {
		t.Error("CommittedByFactoryd = false, want true: the full-suite command left the workspace dirty")
	}
	if !slices.Contains(r.ChangedFiles, "full_suite_side_effect.txt") {
		t.Errorf("ChangedFiles = %v, want it to include full_suite_side_effect.txt (the full-suite command's own output)", r.ChangedFiles)
	}
	// The commit factoryd made must actually contain the file — not just
	// list it in ChangedFiles evidence while leaving it uncommitted, which
	// would silently reintroduce the exact gap the fix closed.
	out, err := exec.Command("git", "-C", ws, "show", r.ResultSHA+":full_suite_side_effect.txt").CombinedOutput()
	if err != nil {
		t.Fatalf("git show result_sha:full_suite_side_effect.txt: %v: %s", err, out)
	}
}

// TestIntegrationFullSuiteVerifyFailureDoesNotCommitMutation is a real P1
// finding from both a GitHub Codex App review round and a local `codex
// review` pass on PR #33: the safety-net commit block was gated only on
// build+verify exit codes, so a full-suite command that mutated the
// checkout and then FAILED still had its output committed and HEAD
// advanced, even though the run quarantines on full_suite_verify — on a
// shared (non-isolated) workspace, the next run captures base_sha fresh from that
// same HEAD, silently inheriting a quarantined run's rejected output as its
// own baseline (gap 4 of the plan's 2026-08-28 readiness review).
func TestIntegrationFullSuiteVerifyFailureDoesNotCommitMutation(t *testing.T) {
	ws := newFixtureRepo(t)
	r := runFactorydWithSpecAndFlags(t, ws, "commit", "true", "# fixture spec\nTests-Required: no -- integration fixture doesn't exercise tests_added\n", "30s", nil, []string{"-full-suite-command", "echo mutated > full_suite_side_effect.txt; exit 1"})

	if r.State != run.StateQuarantined {
		t.Fatalf("state = %q, want %q", r.State, run.StateQuarantined)
	}
	if r.CommittedByFactoryd {
		t.Error("CommittedByFactoryd = true, want false: a failing full-suite command's mutation must not be committed")
	}
	// r.WorkspacePath, not ws directly: isolation defaults to
	// true, so the run actually executed in its own worktree, not ws's
	// own checkout — WorkspacePath is whichever directory that was,
	// correct regardless of isolation.
	dirty, err := exec.Command("git", "-C", r.WorkspacePath, "status", "--porcelain").CombinedOutput()
	if err != nil {
		t.Fatalf("git status: %v: %s", err, dirty)
	}
	if !strings.Contains(string(dirty), "full_suite_side_effect.txt") {
		t.Errorf("git status --porcelain = %q, want it to show full_suite_side_effect.txt as uncommitted", dirty)
	}
	// The file must not be part of HEAD's own tree: a later run reading
	// HEAD as its own base_sha must not see the failed full-suite's
	// output as already committed, even though the agent's own build
	// (mode "commit") legitimately advanced HEAD with its own change.
	if out, err := exec.Command("git", "-C", r.WorkspacePath, "show", "HEAD:full_suite_side_effect.txt").CombinedOutput(); err == nil {
		t.Errorf("git show HEAD:full_suite_side_effect.txt succeeded (%q), want it absent from HEAD's tree", out)
	}
}

// TestIntegrationFullSuiteVerifySkippedWhenNotDeclared confirms the
// `-full-suite-command none` opt-out: a request that explicitly declines
// the verify-command substitution (see TestIntegrationFullSuiteDefaultsToVerifyCommand
// for the opposite, default case) still gets no full_suite_verify gate at
// all, the same convention every other optional EvaluateRun gate follows.
func TestIntegrationFullSuiteVerifySkippedWhenNotDeclared(t *testing.T) {
	ws := newFixtureRepo(t)
	r := runFactorydWithSpecAndFlags(t, ws, "commit", "true", "# fixture spec\nTests-Required: no -- integration fixture doesn't exercise tests_added\n", "30s", nil, []string{"-full-suite-cadence", "2", "-full-suite-command", "none"})

	for _, g := range r.GateResults {
		if g.Check == "full_suite_verify" {
			t.Fatalf("GateResults = %v, want no full_suite_verify gate under the explicit -full-suite-command none opt-out", r.GateResults)
		}
	}
	if r.FullSuiteConfigured {
		t.Error("FullSuiteConfigured = true, want false under the none opt-out")
	}
	if r.FullSuiteSource != fullSuiteSourceNone {
		t.Errorf("FullSuiteSource = %q, want %q", r.FullSuiteSource, fullSuiteSourceNone)
	}
}

// TestIntegrationFullSuiteDefaultsToVerifyCommand is the regression test
// for the full-suite substitution's run_ticket.go entry point:
// with no -full-suite-command and no .factory.yml full_suite_command, the
// full_suite_verify gate now runs anyway, against this run's own resolved
// canonical verify command, rather than never running at all (which used
// to leave RequiredGates permanently denying release -- see
// internal/release.MergePolicy's own RequiredGates comment). The gate
// still genuinely executes (not just marked passed) -- proven here by the
// verify command being "true" (so it passes) and by
// TestIntegrationFullSuiteSubstitutedCommandStillDenies below (a failing
// substituted command still denies).
func TestIntegrationFullSuiteDefaultsToVerifyCommand(t *testing.T) {
	ws := newFixtureRepo(t)
	r := runFactoryd(t, ws, "commit", "true")

	if r.State != run.StateAccepted {
		t.Fatalf("state = %q, want %q: %+v", r.State, run.StateAccepted, r.GateResults)
	}
	if !r.FullSuiteConfigured || !r.FullSuiteScheduled {
		t.Errorf("FullSuiteConfigured=%v FullSuiteScheduled=%v, want both true", r.FullSuiteConfigured, r.FullSuiteScheduled)
	}
	if r.FullSuiteSource != fullSuiteSourceVerifyCommand {
		t.Errorf("FullSuiteSource = %q, want %q", r.FullSuiteSource, fullSuiteSourceVerifyCommand)
	}
	found := false
	for _, g := range r.GateResults {
		if g.Check == "full_suite_verify" {
			found = true
			if !g.Passed {
				t.Errorf("full_suite_verify gate = %+v, want it to pass (substituted command is \"true\")", g)
			}
		}
	}
	if !found {
		t.Fatalf("GateResults = %+v, want a full_suite_verify gate to have actually run", r.GateResults)
	}
}

// TestIntegrationFullSuiteSubstitutedCommandStillDenies proves the
// substituted full_suite_verify gate genuinely runs the command a second
// time, independently of canonical_verify -- it is not marked passed
// just because canonical_verify (the identical command string) already
// passed once. The command touches a marker file and succeeds only the
// first time it runs (canonical_verify), then fails the second time
// (full_suite_verify, run against the same final workspace state) --
// this is exactly the "must still actually run that command and must
// still pass -- never mark it passed without running" requirement the
// full-suite substitution carries. If full_suite_verify were ever
// skipped, or trivially marked passed without executing, this
// run would incorrectly reach StateAccepted instead.
func TestIntegrationFullSuiteSubstitutedCommandStillDenies(t *testing.T) {
	ws := newFixtureRepo(t)
	verifyCommand := `f=full_suite_marker.txt; if [ -f "$f" ]; then exit 1; else touch "$f"; fi`
	r := runFactoryd(t, ws, "commit", verifyCommand)

	if r.State != run.StateQuarantined {
		t.Fatalf("state = %q, want %q: %+v", r.State, run.StateQuarantined, r.GateResults)
	}
	foundFailing := false
	for _, g := range r.GateResults {
		if g.Check == "full_suite_verify" && !g.Passed {
			foundFailing = true
		}
	}
	if !foundFailing {
		t.Fatalf("GateResults = %+v, want a failing full_suite_verify gate", r.GateResults)
	}
}

func TestIntegrationRecordsDependencyLockfileTouched(t *testing.T) {
	ws := newFixtureRepo(t)
	r := runFactoryd(t, ws, "lockfile", "true")

	if r.State != run.StateAccepted {
		t.Fatalf("state = %q, want %q", r.State, run.StateAccepted)
	}
	want := []string{"frontend/pubspec.lock"}
	if !slices.Equal(r.DependencyLockfilesTouched, want) {
		t.Errorf("DependencyLockfilesTouched = %v, want %v", r.DependencyLockfilesTouched, want)
	}
}

// TestIntegrationRecordsSemanticPackageLockChanges proves the direct
// supervisor wires PackageLockDependencyDiff into durable run evidence and
// compares the base commit with the final workspace after verification.
func TestIntegrationRecordsSemanticPackageLockChanges(t *testing.T) {
	ws := newFixtureRepo(t)
	baseLock := `{"packages":{"":{"name":"app","version":"1.0.0"},"node_modules/a":{"name":"a","version":"1.0.0"}}}`
	if err := os.WriteFile(filepath.Join(ws, "package-lock.json"), []byte(baseLock), 0o644); err != nil {
		t.Fatalf("write base package-lock.json: %v", err)
	}
	if out, err := exec.Command("git", "-C", ws, "add", "package-lock.json").CombinedOutput(); err != nil {
		t.Fatalf("git add package-lock.json: %v: %s", err, out)
	}
	if out, err := exec.Command("git", "-C", ws, "commit", "-q", "-m", "add package lock").CombinedOutput(); err != nil {
		t.Fatalf("commit base package-lock.json: %v: %s", err, out)
	}

	resultLock := `{"packages":{"":{"name":"app","version":"1.0.0"},"node_modules/a":{"name":"a","version":"1.1.0"},"node_modules/b":{"name":"b","version":"2.0.0"}}}`
	verifyCommand := "cat > package-lock.json <<'EOF'\n" + resultLock + "\nEOF"
	r := runFactoryd(t, ws, "commit", verifyCommand)

	if r.State != run.StateAccepted {
		t.Fatalf("state = %q, want %q", r.State, run.StateAccepted)
	}
	want := []struct{ name, base, result string }{
		{name: "a", base: "1.0.0", result: "1.1.0"},
		{name: "b", result: "2.0.0"},
	}
	if len(r.DependencyChanges) != len(want) {
		t.Fatalf("DependencyChanges = %+v, want %+v", r.DependencyChanges, want)
	}
	for i, want := range want {
		got := r.DependencyChanges[i]
		if got.Name != want.name || got.Base != want.base || got.Result != want.result {
			t.Errorf("DependencyChanges[%d] = %+v, want %+v", i, got, want)
		}
	}
}

// TestIntegrationRecordsSemanticComposerLockChanges proves the direct
// supervisor wires ComposerLockDependencyDiff into durable run evidence and
// compares the base commit with the final workspace after verification.
func TestIntegrationRecordsSemanticComposerLockChanges(t *testing.T) {
	ws := newFixtureRepo(t)
	baseLock := `{"packages":[{"name":"vendor/a","version":"1.0.0"}],"packages-dev":[{"name":"vendor/test","version":"2.0.0"}]}`
	if err := os.WriteFile(filepath.Join(ws, "composer.lock"), []byte(baseLock), 0o644); err != nil {
		t.Fatalf("write base composer.lock: %v", err)
	}
	if out, err := exec.Command("git", "-C", ws, "add", "composer.lock").CombinedOutput(); err != nil {
		t.Fatalf("git add composer.lock: %v: %s", err, out)
	}
	if out, err := exec.Command("git", "-C", ws, "commit", "-q", "-m", "add composer lock").CombinedOutput(); err != nil {
		t.Fatalf("commit base composer.lock: %v: %s", err, out)
	}

	resultLock := `{"packages":[{"name":"vendor/a","version":"1.1.0"},{"name":"vendor/b","version":"3.0.0"}],"packages-dev":[{"name":"vendor/test","version":"2.0.0"}]}`
	verifyCommand := "cat > composer.lock <<'EOF'\n" + resultLock + "\nEOF"
	r := runFactoryd(t, ws, "commit", verifyCommand)

	if r.State != run.StateAccepted {
		t.Fatalf("state = %q, want %q", r.State, run.StateAccepted)
	}
	want := []struct{ name, base, result string }{
		{name: "vendor/a", base: "1.0.0", result: "1.1.0"},
		{name: "vendor/b", result: "3.0.0"},
	}
	if len(r.DependencyChanges) != len(want) {
		t.Fatalf("DependencyChanges = %+v, want %+v", r.DependencyChanges, want)
	}
	for i, want := range want {
		got := r.DependencyChanges[i]
		if got.Name != want.name || got.Base != want.base || got.Result != want.result {
			t.Errorf("DependencyChanges[%d] = %+v, want %+v", i, got, want)
		}
	}
}

// TestIntegrationRecordsSemanticPubspecLockChanges proves the direct
// supervisor wires PubspecLockDependencyDiff into durable run evidence and
// compares the base commit with the final workspace after verification.
func TestIntegrationRecordsSemanticPubspecLockChanges(t *testing.T) {
	ws := newFixtureRepo(t)
	baseLock := `packages:
  a:
    version: "1.0.0"`
	if err := os.WriteFile(filepath.Join(ws, "pubspec.lock"), []byte(baseLock), 0o644); err != nil {
		t.Fatalf("write base pubspec.lock: %v", err)
	}
	if out, err := exec.Command("git", "-C", ws, "add", "pubspec.lock").CombinedOutput(); err != nil {
		t.Fatalf("git add pubspec.lock: %v: %s", err, out)
	}
	if out, err := exec.Command("git", "-C", ws, "commit", "-q", "-m", "add pubspec lock").CombinedOutput(); err != nil {
		t.Fatalf("commit base pubspec.lock: %v: %s", err, out)
	}

	resultLock := `packages:
  a:
    version: "1.1.0"
  b:
    version: "2.0.0"`
	verifyCommand := "cat > pubspec.lock <<'EOF'\n" + resultLock + "\nEOF"
	r := runFactoryd(t, ws, "commit", verifyCommand)

	if r.State != run.StateAccepted {
		t.Fatalf("state = %q, want %q", r.State, run.StateAccepted)
	}
	want := []struct{ name, base, result string }{
		{name: "a", base: "1.0.0", result: "1.1.0"},
		{name: "b", result: "2.0.0"},
	}
	if len(r.DependencyChanges) != len(want) {
		t.Fatalf("DependencyChanges = %+v, want %+v", r.DependencyChanges, want)
	}
	for i, want := range want {
		got := r.DependencyChanges[i]
		if got.Name != want.name || got.Base != want.base || got.Result != want.result {
			t.Errorf("DependencyChanges[%d] = %+v, want %+v", i, got, want)
		}
	}
}

// TestIntegrationRecordsSemanticGoSumChanges proves the direct supervisor
// wires GoSumDependencyDiff into durable run evidence and compares the base
// commit with the final workspace after verification.
func TestIntegrationRecordsSemanticGoSumChanges(t *testing.T) {
	ws := newFixtureRepo(t)
	baseLock := `github.com/a/a v1.0.0 h1:abc=
github.com/a/a v1.0.0/go.mod h1:def=`
	if err := os.WriteFile(filepath.Join(ws, "go.sum"), []byte(baseLock), 0o644); err != nil {
		t.Fatalf("write base go.sum: %v", err)
	}
	if out, err := exec.Command("git", "-C", ws, "add", "go.sum").CombinedOutput(); err != nil {
		t.Fatalf("git add go.sum: %v: %s", err, out)
	}
	if out, err := exec.Command("git", "-C", ws, "commit", "-q", "-m", "add go.sum").CombinedOutput(); err != nil {
		t.Fatalf("commit base go.sum: %v: %s", err, out)
	}

	resultLock := `github.com/a/a v1.1.0 h1:mno=
github.com/a/a v1.1.0/go.mod h1:pqr=
github.com/b/b v2.0.0 h1:stu=
github.com/b/b v2.0.0/go.mod h1:vwx=`
	verifyCommand := "cat > go.sum <<'EOF'\n" + resultLock + "\nEOF"
	r := runFactoryd(t, ws, "commit", verifyCommand)

	if r.State != run.StateAccepted {
		t.Fatalf("state = %q, want %q", r.State, run.StateAccepted)
	}
	want := []struct{ name, base, result string }{
		{name: "github.com/a/a", base: "v1.0.0", result: "v1.1.0"},
		{name: "github.com/b/b", result: "v2.0.0"},
	}
	if len(r.DependencyChanges) != len(want) {
		t.Fatalf("DependencyChanges = %+v, want %+v", r.DependencyChanges, want)
	}
	for i, want := range want {
		got := r.DependencyChanges[i]
		if got.Name != want.name || got.Base != want.base || got.Result != want.result {
			t.Errorf("DependencyChanges[%d] = %+v, want %+v", i, got, want)
		}
	}
}

func TestIntegrationRecordsAgentEvidence(t *testing.T) {
	ws := newFixtureRepo(t)
	r := runFactoryd(t, ws, "commit", "true")

	if r.State != run.StateAccepted {
		t.Fatalf("state = %q, want %q", r.State, run.StateAccepted)
	}
	if r.AgentEvidence == nil {
		t.Fatal("AgentEvidence = nil, want structured BUILD_EVIDENCE.json contents")
	}
	e := r.AgentEvidence
	if e.Generated != "2026-08-26T12:34:56+00:00" || e.ReviewPolicy != "advisory" || !e.Succeeded || e.StoppedReason != "canonical verification passed; advisory review" {
		t.Errorf("AgentEvidence summary = %+v, want fixture summary values", e)
	}
	if e.Provider == nil || *e.Provider != "ai-stack-local" || e.Model == nil || *e.Model != "Qwen3.8-27B-MTPLX-Optimized-Quality" {
		t.Errorf("AgentEvidence identity = provider %v model %v, want fixture identity", e.Provider, e.Model)
	}
	if len(e.Rounds) != 1 {
		t.Fatalf("AgentEvidence.Rounds = %+v, want exactly one round", e.Rounds)
	}
	round := e.Rounds[0]
	if round.Index != 1 || round.Agent != "pi" || round.AgentReturnCode != 0 || round.AgentTimedOut || round.ReviewerOutcome != "flagged" || round.ReviewerDetail != "cache.py: stale value" || round.VerifyPassed == nil || !*round.VerifyPassed || round.VerifyTimedOut || round.DurationS != 12.5 {
		t.Errorf("AgentEvidence round = %+v, want fixture round values", round)
	}
	if round.Usage["input"] != float64(120) || round.Usage["output"] != float64(30) {
		t.Errorf("AgentEvidence round usage = %v, want input=120 output=30", round.Usage)
	}
}

func TestIntegrationMissingAgentEvidenceDoesNotAffectOutcome(t *testing.T) {
	ws := newFixtureRepo(t)
	r := runFactoryd(t, ws, "fail", "true")

	if r.State != run.StateQuarantined {
		t.Fatalf("state = %q, want normal build-failure outcome %q", r.State, run.StateQuarantined)
	}
	if r.AgentEvidence != nil {
		t.Errorf("AgentEvidence = %+v, want nil when BUILD_EVIDENCE.json is absent", r.AgentEvidence)
	}
}

// TestIntegrationDiffScopeGateQuarantinesOutOfScopeChange is the regression test
// for a real finding from review: a ticket's prose "Out of scope" section
// was never actually enforced — a run with an unrelated file changed
// alongside the intended one (found for real: a harness housekeeping
// change to .gitignore) could still be accepted purely on build+verify
// exit codes. The ticket here declares Allowed-Files listing only
// content.txt; the fake agent (mode commit_extra) also touches
// extra-out-of-scope.txt, so the run must quarantine on the diff_scope gate.
// The scope check runs after canonical verification (a verify command
// can itself rewrite files — a formatter, codegen — so checking scope
// before it ran would miss those changes), so canonical_verify does run
// and does pass here; it's the diff_scope gate specifically that must
// fail and take the run through halted to quarantined regardless.
func TestIntegrationDiffScopeGateQuarantinesOutOfScopeChange(t *testing.T) {
	ws := newFixtureRepo(t)
	specContent := "# Ticket: fixture\n\n## Out of scope\n\nAllowed-Files: content.txt\n"

	r := runFactorydWithSpec(t, ws, "commit_extra", "true", specContent, "30s")

	if r.State != run.StateQuarantined {
		t.Fatalf("state = %q, want %q — an out-of-scope file must quarantine the run even though build+verify would pass", r.State, run.StateQuarantined)
	}
	var scopeGate, verifyGate *run.GateResult
	for i := range r.GateResults {
		switch r.GateResults[i].Check {
		case "diff_scope":
			scopeGate = &r.GateResults[i]
		case "canonical_verify":
			verifyGate = &r.GateResults[i]
		}
	}
	if scopeGate == nil {
		t.Fatalf("expected a diff_scope gate result, got %v", r.GateResults)
	}
	if scopeGate.Passed {
		t.Error("diff_scope gate Passed = true, want false")
	}
	if verifyGate == nil || !verifyGate.Passed {
		t.Errorf("expected canonical_verify to have run and passed (it's the diff_scope gate that should fail), got %+v", verifyGate)
	}
	if !slices.Contains(r.ChangedFiles, "extra-out-of-scope.txt") {
		t.Errorf("ChangedFiles = %v, want it to include the out-of-scope file", r.ChangedFiles)
	}
}

// TestIntegrationDiffScopeGateSkippedWithoutAllowedFiles confirms that
// tickets without an Allowed-Files: key (the common case, and every
// ticket that predates this feature) are entirely unaffected — the same
// out-of-scope change that quarantines TestIntegrationDiffScopeGateQuarantinesOutOfScopeChange
// above must be accepted here since no scope was declared to violate.
func TestIntegrationDiffScopeGateSkippedWithoutAllowedFiles(t *testing.T) {
	ws := newFixtureRepo(t)
	r := runFactoryd(t, ws, "commit_extra", "true")

	if r.State != run.StateAccepted {
		t.Fatalf("state = %q, want %q — no Allowed-Files declared, so no scope to violate", r.State, run.StateAccepted)
	}
	for _, g := range r.GateResults {
		if g.Check == "diff_scope" {
			t.Errorf("unexpected diff_scope gate result %+v when the ticket declared no Allowed-Files", g)
		}
	}
}

// TestIntegrationDiffScopeGateCatchesVerifyCommandChanges is the
// regression test for a second real finding from review: the diff-scope
// gate originally ran before canonical verification, so a verify command
// that itself rewrites files (a formatter, codegen, a snapshot updater)
// could introduce an out-of-scope change that was never checked. Here the
// agent only touches the in-scope content.txt, but the verify command
// itself writes (and leaves uncommitted) an unrelated file — the gate
// must still catch it because scope is now checked after verification,
// against a union of committed *and* uncommitted changes.
func TestIntegrationDiffScopeGateCatchesVerifyCommandChanges(t *testing.T) {
	ws := newFixtureRepo(t)
	specContent := "# Ticket: fixture\n\n## Out of scope\n\nAllowed-Files: content.txt\n"
	verifyCommand := "echo written-by-verify >> written-by-verify.txt; true"

	r := runFactorydWithSpec(t, ws, "commit", verifyCommand, specContent, "30s")

	if r.State != run.StateQuarantined {
		t.Fatalf("state = %q, want %q — a file the verify command itself wrote must still violate scope", r.State, run.StateQuarantined)
	}
	var scopeGate, verifyGate *run.GateResult
	for i := range r.GateResults {
		switch r.GateResults[i].Check {
		case "diff_scope":
			scopeGate = &r.GateResults[i]
		case "canonical_verify":
			verifyGate = &r.GateResults[i]
		}
	}
	if verifyGate == nil || !verifyGate.Passed {
		t.Errorf("expected canonical_verify to have run and passed, got %+v", verifyGate)
	}
	if scopeGate == nil || scopeGate.Passed {
		t.Errorf("expected a failing diff_scope gate, got %+v", scopeGate)
	}
	if !slices.Contains(r.ChangedFiles, "written-by-verify.txt") {
		t.Errorf("ChangedFiles = %v, want it to include the file the verify command wrote (uncommitted)", r.ChangedFiles)
	}
}

// TestIntegrationRequiredFilesChangedGateQuarantinesUnchangedFile is the
// regression test for the false-accept gap tracked in CLAIMS.md: a
// passing Verify-Command only proves the declared command exited 0, not
// that the ticket's required implementation change was made. Here the
// ticket declares content.txt as required, but the fake agent (mode
// commit_other) only touches an unrelated scaffolding file, other.txt;
// the verify command itself is trivially "true", so canonical_verify
// passes, and only the required_files_changed gate must catch the gap.
func TestIntegrationRequiredFilesChangedGateQuarantinesUnchangedFile(t *testing.T) {
	ws := newFixtureRepo(t)
	specContent := "# Ticket: fixture\n\n## Out of scope\n\nRequired-Changed-Files: content.txt\n"

	r := runFactorydWithSpec(t, ws, "commit_other", "true", specContent, "30s")

	if r.State != run.StateQuarantined {
		t.Fatalf("state = %q, want %q — an unchanged required file must quarantine the run even though verify passes", r.State, run.StateQuarantined)
	}
	var requiredGate, verifyGate *run.GateResult
	for i := range r.GateResults {
		switch r.GateResults[i].Check {
		case "required_files_changed":
			requiredGate = &r.GateResults[i]
		case "canonical_verify":
			verifyGate = &r.GateResults[i]
		}
	}
	if requiredGate == nil {
		t.Fatalf("expected a required_files_changed gate result, got %v", r.GateResults)
	}
	if requiredGate.Passed {
		t.Error("required_files_changed gate Passed = true, want false")
	}
	if verifyGate == nil || !verifyGate.Passed {
		t.Errorf("expected canonical_verify to have run and passed (it's the required_files_changed gate that should fail), got %+v", verifyGate)
	}
	if slices.Contains(r.ChangedFiles, "content.txt") {
		t.Errorf("ChangedFiles = %v, want it to NOT include the never-touched required file", r.ChangedFiles)
	}
}

// TestIntegrationRequiredFilesChangedGateRejectsPreExistingDirt proves the
// current rule: required_files_changed is checked against the run's own
// worktree, which is cut from the base commit, so uncommitted state in the
// operator's shared checkout never reaches a build. A required file dirty
// there must not halt the run (no submitter-side check exists); the build
// runs (mode commit_other touches only other.txt), and the gate fails
// because content.txt is unchanged in the run's own worktree -- the shared
// checkout's dirt is not attributed to the run.
func TestIntegrationRequiredFilesChangedGateRejectsPreExistingDirt(t *testing.T) {
	ws := newFixtureRepo(t)
	f, err := os.OpenFile(filepath.Join(ws, "content.txt"), os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("dirty content.txt: %v", err)
	}
	if _, err := f.WriteString("pre-existing uncommitted edit\n"); err != nil {
		t.Fatalf("dirty content.txt: %v", err)
	}
	f.Close()

	specContent := "# Ticket: fixture\n\n## Out of scope\n\nRequired-Changed-Files: content.txt\n"
	r := runFactorydWithSpecAndFlags(t, ws, "commit_other", "true", specContent, "30s", nil, nil)

	if r.State != run.StateQuarantined {
		t.Fatalf("state = %q, want %q -- dirt in the shared checkout must not halt the run, and must not satisfy the gate", r.State, run.StateQuarantined)
	}
	var buildRan bool
	for _, a := range r.Attempts {
		if a.Kind == "build" {
			buildRan = true
		}
	}
	if !buildRan {
		t.Error("no build attempt recorded, want the build to run")
	}
	var requiredGate *run.GateResult
	for i := range r.GateResults {
		if r.GateResults[i].Check == "required_files_changed" {
			requiredGate = &r.GateResults[i]
		}
	}
	if requiredGate == nil || requiredGate.Passed {
		t.Errorf("required_files_changed gate = %+v, want present and failed", requiredGate)
	}
	if slices.Contains(r.ChangedFiles, "content.txt") {
		t.Errorf("ChangedFiles = %v, want no content.txt (the shared checkout's dirt is not this run's change)", r.ChangedFiles)
	}
}

// TestIntegrationRequiredFilesChangedGateSkippedWithoutDeclaration confirms
// that tickets without a Required-Changed-Files: key (the common case, and
// every ticket that predates this feature) are entirely unaffected — the
// same unchanged file that quarantines
// TestIntegrationRequiredFilesChangedGateQuarantinesUnchangedFile above
// must be accepted here since no required-files list was declared.
func TestIntegrationRequiredFilesChangedGateSkippedWithoutDeclaration(t *testing.T) {
	ws := newFixtureRepo(t)
	r := runFactoryd(t, ws, "commit_other", "true")

	if r.State != run.StateAccepted {
		t.Fatalf("state = %q, want %q — no Required-Changed-Files declared, so nothing required", r.State, run.StateAccepted)
	}
	for _, g := range r.GateResults {
		if g.Check == "required_files_changed" {
			t.Errorf("unexpected required_files_changed gate result %+v when the ticket declared no Required-Changed-Files", g)
		}
	}
}

// TestIntegrationRequiredContentPresentGatePasses confirms the passing
// path: the ticket declares REQUIRED_MARKER_STRING as required content,
// and the fake agent (mode commit_with_marker) actually writes it into
// content.txt.
func TestIntegrationRequiredContentPresentGatePasses(t *testing.T) {
	ws := newFixtureRepo(t)
	specContent := "# Ticket: fixture\n\n## Out of scope\n\nRequired-Changed-Files: content.txt\nRequired-Content: REQUIRED_MARKER_STRING\n"

	r := runFactorydWithSpec(t, ws, "commit_with_marker", "true", specContent, "30s")

	if r.State != run.StateAccepted {
		t.Fatalf("state = %q, want %q — the required content was actually written", r.State, run.StateAccepted)
	}
	var contentGate *run.GateResult
	for i := range r.GateResults {
		if r.GateResults[i].Check == "required_content_present" {
			contentGate = &r.GateResults[i]
		}
	}
	if contentGate == nil || !contentGate.Passed {
		t.Fatalf("expected a passing required_content_present gate result, got %+v", contentGate)
	}
}

// TestIntegrationRequiredContentPresentGateQuarantinesCosmeticChange is
// the regression test for the gap required_files_changed alone leaves
// open: the ticket declares REQUIRED_MARKER_STRING as required content,
// but the fake agent (mode commit) only appends unrelated text —
// content.txt genuinely changed (required_files_changed passes), yet the
// specific required marker never appeared, so required_content_present
// must still catch it and quarantine.
func TestIntegrationRequiredContentPresentGateQuarantinesCosmeticChange(t *testing.T) {
	ws := newFixtureRepo(t)
	specContent := "# Ticket: fixture\n\n## Out of scope\n\nRequired-Changed-Files: content.txt\nRequired-Content: REQUIRED_MARKER_STRING\n"

	r := runFactorydWithSpec(t, ws, "commit", "true", specContent, "30s")

	if r.State != run.StateQuarantined {
		t.Fatalf("state = %q, want %q — content.txt changed but never contained the required marker", r.State, run.StateQuarantined)
	}
	var contentGate, requiredFilesGate *run.GateResult
	for i := range r.GateResults {
		switch r.GateResults[i].Check {
		case "required_content_present":
			contentGate = &r.GateResults[i]
		case "required_files_changed":
			requiredFilesGate = &r.GateResults[i]
		}
	}
	if requiredFilesGate == nil || !requiredFilesGate.Passed {
		t.Errorf("expected required_files_changed to pass (content.txt did change), got %+v", requiredFilesGate)
	}
	if contentGate == nil || contentGate.Passed {
		t.Errorf("expected a failing required_content_present gate, got %+v", contentGate)
	}
}

// TestIntegrationRequiredContentPresentGateRejectsContentAlreadyAtBase
// confirms required content already present before the run started
// doesn't count as evidence of this run's own change — mirroring
// RequiredFilesPreDirty's protection but for content instead of file
// identity. The fixture pre-seeds content.txt with the marker at base;
// the agent still only makes its normal unrelated edit (mode commit).
func TestIntegrationRequiredContentPresentGateRejectsContentAlreadyAtBase(t *testing.T) {
	ws := newFixtureRepo(t)
	f, err := os.OpenFile(filepath.Join(ws, "content.txt"), os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("seed content.txt: %v", err)
	}
	if _, err := f.WriteString("REQUIRED_MARKER_STRING\n"); err != nil {
		t.Fatalf("seed content.txt: %v", err)
	}
	f.Close()
	commitCmd := exec.Command("git", "-C", ws, "commit", "-q", "-am", "pre-seed marker before base")
	commitCmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	if out, err := commitCmd.CombinedOutput(); err != nil {
		t.Fatalf("commit seeded marker: %v: %s", err, out)
	}

	specContent := "# Ticket: fixture\n\n## Out of scope\n\nRequired-Changed-Files: content.txt\nRequired-Content: REQUIRED_MARKER_STRING\n"
	r := runFactorydWithSpec(t, ws, "commit", "true", specContent, "30s")

	if r.State != run.StateQuarantined {
		t.Fatalf("state = %q, want %q — the marker was already present at base, not added by this run", r.State, run.StateQuarantined)
	}
	var contentGate *run.GateResult
	for i := range r.GateResults {
		if r.GateResults[i].Check == "required_content_present" {
			contentGate = &r.GateResults[i]
		}
	}
	if contentGate == nil || contentGate.Passed {
		t.Errorf("expected a failing required_content_present gate, got %+v", contentGate)
	}
}

// TestIntegrationRequiredContentPresentGateSkippedWithoutDeclaration
// confirms tickets without a Required-Content: key are unaffected.
func TestIntegrationRequiredContentPresentGateSkippedWithoutDeclaration(t *testing.T) {
	ws := newFixtureRepo(t)
	specContent := "# Ticket: fixture\n\n## Out of scope\n\nRequired-Changed-Files: content.txt\n"

	r := runFactorydWithSpec(t, ws, "commit", "true", specContent, "30s")

	if r.State != run.StateAccepted {
		t.Fatalf("state = %q, want %q — no Required-Content declared, so nothing required", r.State, run.StateAccepted)
	}
	for _, g := range r.GateResults {
		if g.Check == "required_content_present" {
			t.Errorf("unexpected required_content_present gate result %+v when the ticket declared no Required-Content", g)
		}
	}
}

// TestIntegrationHaltsWhenBaseSHANoLongerAnAncestor is the regression
// test for a real finding from a live factoryd run against
// a Flutter + Go app repo: the workspace's git history no longer contained
// base_sha as an ancestor of HEAD by the time build_app.py returned — a
// real build_app.py/pi harness invocation had reset the workspace
// backward mid-run and committed on top of that older state, which
// (diffed against the real base_sha) looked like a large, spurious
// deletion of unrelated already-completed work. Nothing caught this
// before the ancestry check existed except a narrow Allowed-Files
// happening to flag the resulting diff via diff_scope — a ticket without
// one would have silently accepted it. The fixture workspace here has
// two real commits before the run starts (newFixtureRepo's root, then
// one more so base_sha isn't the root itself); the fake agent (mode
// rewind_and_commit) resets to the root and commits new, unrelated work
// from there.
func TestIntegrationHaltsWhenBaseSHANoLongerAnAncestor(t *testing.T) {
	ws := newFixtureRepo(t)
	// A second real commit, so base_sha (captured at this point by
	// factoryd) has an actual ancestor for the rewind to discard.
	if err := os.WriteFile(filepath.Join(ws, "second.txt"), []byte("second commit content\n"), 0o644); err != nil {
		t.Fatalf("write second.txt: %v", err)
	}
	addCmd := exec.Command("git", "-C", ws, "add", "-A")
	if out, err := addCmd.CombinedOutput(); err != nil {
		t.Fatalf("git add: %v: %s", err, out)
	}
	commitCmd := exec.Command("git", "-C", ws, "commit", "-q", "-m", "second commit, this becomes base_sha")
	commitCmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	if out, err := commitCmd.CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v: %s", err, out)
	}

	r := runFactoryd(t, ws, "rewind_and_commit", "true")

	if r.State != run.StateHalted {
		t.Fatalf("state = %q, want %q — base_sha no longer an ancestor of HEAD must halt", r.State, run.StateHalted)
	}
	if len(r.GateResults) != 0 {
		t.Errorf("GateResults = %v, want none — the run must halt before any gate evaluates", r.GateResults)
	}
}

// TestIntegrationTimedOutBuildStillRecordsAttempt pins the fix for a
// finding from review: a build_app.py invocation killed by the
// supervisor's own -timeout used to leave r.Attempts empty (the append
// only happened after the error check), so a timed-out run's halted
// run.json carried no evidence at all about the attempt that actually
// ran. It must now record the command/timestamps/log path even though
// the attempt never finished.
func TestIntegrationTimedOutBuildStillRecordsAttempt(t *testing.T) {
	ws := newFixtureRepo(t)
	// Sandboxing is unconditional and reserves a fixed teardown margin
	// (sandboxAttemptTeardownMargin, 10s) off this run's own deadline
	// before it will even start an attempt -- long enough that the real
	// timeout-cancellation path this test means to exercise fires
	// instead of that earlier "insufficient time remaining" pre-check.
	r := runFactorydWithSpec(t, ws, "hang", "true", "# fixture spec\nTests-Required: no -- integration fixture doesn't exercise tests_added\n", "15s")

	if r.State != run.StateHalted {
		t.Fatalf("state = %q, want %q", r.State, run.StateHalted)
	}
	if len(r.Attempts) != 1 {
		t.Fatalf("expected one recorded attempt even though the build timed out, got %v", r.Attempts)
	}
	if r.Attempts[0].Kind != "build" {
		t.Errorf("recorded attempt Kind = %q, want build", r.Attempts[0].Kind)
	}
	if len(r.Attempts[0].Command) == 0 {
		t.Error("recorded attempt has no Command — timeout evidence was lost")
	}
	if r.Attempts[0].ExitCode != -1 {
		t.Errorf("recorded attempt ExitCode = %d, want -1 (infrastructure failure sentinel, not a real exit status)", r.Attempts[0].ExitCode)
	}
}

// TestIntegrationSignalCancellationHaltsAndReapsBuild proves SIGINT takes
// the same context-cancellation path as the supervisor timeout, while
// remaining distinguishable in factoryd's own output.
func TestIntegrationSignalCancellationHaltsAndReapsBuild(t *testing.T) {
	ws := newFixtureRepo(t)
	dataDir := t.TempDir()
	pidPath := filepath.Join(t.TempDir(), "hung-child.pid")
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
		"-timeout", "30s",
		"-verify-command", "true",
		"-data-dir", dataDir,
	)
	cmd.Env = append(os.Environ(),
		"FAKE_BUILD_APP_MODE=hang",
		"FAKE_BUILD_APP_PID_FILE="+pidPath,
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
	)
	var output synchronizedBuffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Start(); err != nil {
		t.Fatalf("start factoryd: %v", err)
	}
	processWaited := false
	t.Cleanup(func() {
		if !processWaited {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})

	var runJSONPath string
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		entries, readErr := os.ReadDir(filepath.Join(dataDir, "runs"))
		if readErr == nil && len(entries) == 1 {
			candidate := filepath.Join(dataDir, "runs", entries[0].Name(), "run.json")
			b, readErr := os.ReadFile(candidate)
			var current run.Run
			if readErr == nil && json.Unmarshal(b, &current) == nil && current.State == run.StateSliceRunning {
				if _, readErr := os.Stat(pidPath); readErr == nil {
					runJSONPath = candidate
					break
				}
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if runJSONPath == "" {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		processWaited = true
		t.Fatalf("factoryd never reached slice_running with a hung child; output:\n%s", output.String())
	}

	pidBytes, err := os.ReadFile(pidPath)
	if err != nil {
		t.Fatalf("read hung child PID: %v", err)
	}
	hungPID, err := strconv.Atoi(strings.TrimSpace(string(pidBytes)))
	if err != nil {
		t.Fatalf("parse hung child PID %q: %v", pidBytes, err)
	}
	if err := cmd.Process.Signal(syscall.SIGINT); err != nil {
		t.Fatalf("signal factoryd: %v", err)
	}
	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()
	select {
	case <-waitCh:
	// Not 5s: the exit waits for the cancelled build Activity to kill its
	// container and the Temporal worker to stop.
	case <-time.After(10 * time.Second):
		_ = cmd.Process.Kill()
		<-waitCh
		processWaited = true
		t.Fatalf("factoryd did not exit after SIGINT; output:\n%s", output.String())
	}
	processWaited = true

	b, err := os.ReadFile(runJSONPath)
	if err != nil {
		t.Fatalf("read run.json: %v", err)
	}
	var r run.Run
	if err := json.Unmarshal(b, &r); err != nil {
		t.Fatalf("unmarshal run.json: %v", err)
	}
	if r.State != run.StateHalted {
		t.Errorf("state = %q, want %q", r.State, run.StateHalted)
	}
	// The operator-cancellation line, and the wait's context error, which a
	// supervisor timeout would report as "context deadline exceeded" instead
	// (the "temporal workflow did not complete" wrap in cmd/factoryd/run_temporal.go).
	if !strings.Contains(output.String(), "operator cancelled this run (SIGINT/SIGTERM)") ||
		!strings.Contains(output.String(), "temporal workflow did not complete: context canceled") || strings.Contains(output.String(), "deadline exceeded") {
		t.Errorf("factoryd output lacks the operator-cancellation error (context canceled, not a deadline):\n%s", output.String())
	}

	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && syscall.Kill(hungPID, 0) == nil {
		time.Sleep(10 * time.Millisecond)
	}
	if err := syscall.Kill(hungPID, 0); err == nil || err == syscall.EPERM {
		t.Errorf("hung child process %d is still alive after factoryd exited", hungPID)
	}
}

// TestIntegrationInfrastructureFailureRetriesBuild proves the retry path
// through the real factoryd binary: the fixture overflows runner.Run's
// scanner once, then makes and commits its normal successful change.
func TestIntegrationInfrastructureFailureRetriesBuild(t *testing.T) {
	ws := newFixtureRepo(t)
	r := runFactoryd(t, ws, "infra_once", "true")

	if r.State != run.StateAccepted {
		t.Fatalf("state = %q, want %q", r.State, run.StateAccepted)
	}
	// 2 build attempts, 1 verify attempt, and 1 full_suite_verify
	// attempt (the full-suite substitution): no -full-suite-command/.factory.yml
	// full_suite_command is configured here, so it's substituted with the
	// resolved verify command ("true") and genuinely runs.
	if len(r.Attempts) != 4 {
		t.Fatalf("Attempts = %v, want 2 build attempts, 1 verify attempt, and 1 full_suite_verify attempt", r.Attempts)
	}
	if r.Attempts[0].Kind != "build" || r.Attempts[1].Kind != "build" || r.Attempts[2].Kind != "verify" || r.Attempts[3].Kind != "full_suite_verify" {
		t.Errorf("attempt Kinds = [%q %q %q %q], want [build build verify full_suite_verify]", r.Attempts[0].Kind, r.Attempts[1].Kind, r.Attempts[2].Kind, r.Attempts[3].Kind)
	}
	if r.Attempts[0].ExitCode != -1 {
		t.Errorf("first attempt ExitCode = %d, want -1 (infrastructure failure sentinel)", r.Attempts[0].ExitCode)
	}
	if r.Attempts[1].ExitCode != 0 {
		t.Errorf("second attempt ExitCode = %d, want 0", r.Attempts[1].ExitCode)
	}
	// The Temporal path prefixes every attempt log with the Activity
	// attempt's key, "<64 hex sha256>.attempt-<n>-" (activityExecutionLogPath,
	// internal/workflow/activity_records.go); both runner attempts here are
	// inside Temporal attempt 1, so they share the prefix and keep the runner's
	// own build_app.log / build_app.attempt2.log suffixes.
	firstLog := filepath.Base(r.Attempts[0].LogPath)
	logPrefix := strings.TrimSuffix(firstLog, "build_app.log")
	if !regexp.MustCompile(`^[0-9a-f]{64}\.attempt-1-$`).MatchString(logPrefix) {
		t.Errorf("first attempt log = %q, want <execution key>.attempt-1-build_app.log", r.Attempts[0].LogPath)
	}
	if got := filepath.Base(r.Attempts[1].LogPath); got != logPrefix+"build_app.attempt2.log" {
		t.Errorf("second attempt log = %q, want %sbuild_app.attempt2.log", r.Attempts[1].LogPath, logPrefix)
	}
}

// TestIntegrationInfrastructureFailureRetriesVerify proves canonical-
// verification retries are preserved as durable attempt evidence. The first
// verification emits a line beyond runner.Run's scanner limit; the second
// succeeds normally.
func TestIntegrationInfrastructureFailureRetriesVerify(t *testing.T) {
	ws := newFixtureRepo(t)
	// The marker lives in a test-owned directory: an isolated worktree's own
	// .git is a file, not a directory, so $PWD/.git/ is not writable there.
	verifyCommand := `marker="` + t.TempDir() + `/fake-verify-infra-once"; if [ ! -e "$marker" ]; then : >"$marker"; awk 'BEGIN { for (i = 0; i < 1048577; i++) printf "x" }'; else exit 0; fi`
	r := runFactorydWithSpecAndFlags(t, ws, "commit", verifyCommand, "# fixture spec\nTests-Required: no -- integration fixture doesn't exercise tests_added\n", "30s", nil, nil)

	if r.State != run.StateAccepted {
		t.Fatalf("state = %q, want %q", r.State, run.StateAccepted)
	}
	// 1 build attempt, 2 verify attempts, and 1 full_suite_verify
	// attempt (the full-suite substitution): no -full-suite-command/.factory.yml
	// full_suite_command is configured here, so it's substituted with the
	// resolved verify command and genuinely runs a third time -- the
	// marker file from the first two invocations already exists by then,
	// so this one exits 0 immediately rather than re-triggering the
	// infrastructure-failure line.
	if len(r.Attempts) != 4 {
		t.Fatalf("Attempts = %v, want 1 build attempt, 2 verify attempts, and 1 full_suite_verify attempt", r.Attempts)
	}
	if r.Attempts[0].Kind != "build" || r.Attempts[1].Kind != "verify" || r.Attempts[2].Kind != "verify" || r.Attempts[3].Kind != "full_suite_verify" {
		t.Errorf("attempt Kinds = [%q %q %q %q], want [build verify verify full_suite_verify]", r.Attempts[0].Kind, r.Attempts[1].Kind, r.Attempts[2].Kind, r.Attempts[3].Kind)
	}
	if r.Attempts[1].ExitCode != -1 {
		t.Errorf("first verify attempt ExitCode = %d, want -1 (infrastructure failure sentinel)", r.Attempts[1].ExitCode)
	}
	if r.Attempts[2].ExitCode != 0 {
		t.Errorf("second verify attempt ExitCode = %d, want 0", r.Attempts[2].ExitCode)
	}
	// Temporal prefixes each attempt log with "<64 hex sha256>.attempt-<n>-"
	// (activityExecutionLogPath, internal/workflow/activity_records.go); both
	// verify runner attempts are inside Temporal attempt 1.
	verifyLog := filepath.Base(r.Attempts[1].LogPath)
	logPrefix := strings.TrimSuffix(verifyLog, "verify.log")
	if !regexp.MustCompile(`^[0-9a-f]{64}\.attempt-1-$`).MatchString(logPrefix) {
		t.Errorf("first verify attempt log = %q, want <execution key>.attempt-1-verify.log", r.Attempts[1].LogPath)
	}
	if got := filepath.Base(r.Attempts[2].LogPath); got != logPrefix+"verify.attempt2.log" {
		t.Errorf("second verify attempt log = %q, want %sverify.attempt2.log", r.Attempts[2].LogPath, logPrefix)
	}
}

func TestIntegrationFactoryCommitsAsSafetyNet(t *testing.T) {
	ws := newFixtureRepo(t)
	r := runFactoryd(t, ws, "leave_dirty", "true")

	if r.State != run.StateAccepted {
		t.Fatalf("state = %q, want %q", r.State, run.StateAccepted)
	}
	if !r.CommittedByFactoryd {
		t.Error("CommittedByFactoryd = false, want true: factoryd should have committed the agent's uncommitted diff")
	}
	assertClean(t, ws)
}

// TestIntegrationFactoryCommitsVerificationOutputAsSafetyNet is real-git
// proof of a bug found live in review: canonical verification can itself
// leave the workspace dirty (a formatter, codegen) without committing its
// own output, distinct from TestIntegrationFactoryCommitsAsSafetyNet above
// (which covers the agent leaving *build*-time dirt). factoryd used to
// capture ResultSHA before checking for verification's own dirt, then
// fold that dirt into ChangedFiles/DiffStat anyway — recording evidence
// for an accepted run that its own result_sha commit couldn't reproduce.
func TestIntegrationFactoryCommitsVerificationOutputAsSafetyNet(t *testing.T) {
	ws := newFixtureRepo(t)
	// mode "commit" leaves the workspace clean after the build; this
	// verify command then simulates a formatter/codegen step rewriting a
	// tracked file and exiting 0 without committing it.
	r := runFactoryd(t, ws, "commit", "echo formatted >> content.txt")

	if r.State != run.StateAccepted {
		t.Fatalf("state = %q, want %q", r.State, run.StateAccepted)
	}
	if !r.CommittedByFactoryd {
		t.Error("CommittedByFactoryd = false, want true: factoryd should have committed canonical verification's uncommitted output")
	}
	assertClean(t, ws)

	committedContent, err := exec.Command("git", "-C", ws, "show", r.ResultSHA+":content.txt").Output()
	if err != nil {
		t.Fatalf("git show %s:content.txt: %v", r.ResultSHA, err)
	}
	if !strings.Contains(string(committedContent), "formatted") {
		t.Fatalf("content.txt as of ResultSHA = %q, want it to contain verification's output", committedContent)
	}
	found := slices.Contains(r.ChangedFiles, "content.txt")
	if !found {
		t.Fatalf("ChangedFiles = %v, want it to include content.txt", r.ChangedFiles)
	}
}

// TestIntegrationFactoryDoesNotCommitVerificationOutputWhenVerificationFails
// is real-git proof of a bug found live in a second review round: the
// verification-output safety-net commit above used to run unconditionally,
// even when verification itself failed and this run will quarantine
// regardless — advancing HEAD with a quarantined run's dirt anyway, which a
// later accepted run would then silently inherit as part of its own
// base_sha. The commit must only happen when both build and verification
// succeeded.

// TestIntegrationOnBranchChecksOutExistingBranchAndKeepsItOnAccept covers
// -on-branch's own contract for a PR-review corrective round: the run
// executes on an already-existing
// branch (not a fresh one), and an accepted outcome leaves that branch
// intact, exactly as ordinary isolation acceptance already keeps
// its own disposable branch -- but here the branch pre-existed this run.
func TestIntegrationOnBranchChecksOutExistingBranchAndKeepsItOnAccept(t *testing.T) {
	ws := newFixtureRepo(t)
	if out, err := exec.Command("git", "-C", ws, "branch", "existing-pr-branch").CombinedOutput(); err != nil {
		t.Fatalf("create existing-pr-branch: %v: %s", err, out)
	}
	dataDir := t.TempDir()
	specContent := "# Ticket: fixture\n\n## Out of scope\n\nAllowed-Files: content.txt\n"
	r := runFactorydWithSpecFlagsAndDataDir(t, ws, "commit", "true", specContent, "30s", nil,
		[]string{"-on-branch", "existing-pr-branch"}, dataDir)

	if r.State != run.StateAccepted {
		t.Fatalf("state = %q, want %q", r.State, run.StateAccepted)
	}
	if r.Branch != "existing-pr-branch" {
		t.Fatalf("r.Branch = %q, want %q", r.Branch, "existing-pr-branch")
	}
	branchList, err := exec.Command("git", "-C", ws, "branch", "--list", "existing-pr-branch").Output()
	if err != nil {
		t.Fatalf("git branch --list: %v", err)
	}
	if len(branchList) == 0 {
		t.Error("existing-pr-branch was deleted by an accepted -on-branch run, want it kept")
	}
	log, err := exec.Command("git", "-C", ws, "log", "existing-pr-branch", "--oneline").Output()
	if err != nil {
		t.Fatalf("git log existing-pr-branch: %v", err)
	}
	if len(log) == 0 {
		t.Error("existing-pr-branch has no commits after an accepted -on-branch run")
	}
}

// TestIntegrationOnBranchKeepsBranchOnHalt proves a halted -on-branch run
// never deletes the existing branch it was given -- unlike an ordinary
// isolated run's own disposable branch, which IS rolled back on halt (see
// TestIntegrationIsolateWorkspaceRollsBackWhenTerminalSaveFails).
func TestIntegrationOnBranchKeepsBranchOnHalt(t *testing.T) {
	ws := newFixtureRepo(t)
	if out, err := exec.Command("git", "-C", ws, "branch", "existing-pr-branch-2").CombinedOutput(); err != nil {
		t.Fatalf("create existing-pr-branch-2: %v: %s", err, out)
	}
	dataDir := t.TempDir()
	specContent := "# Ticket: fixture\n\n## Out of scope\n\nAllowed-Files: content.txt\n"
	// "fail" mode: build_app.py exits 1 without touching the workspace,
	// which run_ticket.go's own retry exhaustion eventually reports as
	// halted.
	r := runFactorydWithSpecFlagsAndDataDir(t, ws, "fail", "true", specContent, "30s", nil,
		[]string{"-on-branch", "existing-pr-branch-2", "-build-app-max-attempts", "1"}, dataDir)

	if r.State != run.StateHalted && r.State != run.StateQuarantined {
		t.Fatalf("state = %q, want halted or quarantined", r.State)
	}
	branchList, err := exec.Command("git", "-C", ws, "branch", "--list", "existing-pr-branch-2").Output()
	if err != nil {
		t.Fatalf("git branch --list: %v", err)
	}
	if len(branchList) == 0 {
		t.Error("existing-pr-branch-2 was deleted by a non-accepted -on-branch run, want it kept regardless of outcome")
	}
}

// TestIntegrationDiffBaseComputesCumulativeChangedFiles covers the reason
// -diff-base exists: a corrective PR-review round's own base_sha is
// just the branch tip it started from, so without -diff-base the
// diff-shape gates (here, required_files_changed) would judge only the
// round's own small delta instead of the cumulative diff the PR as a
// whole will merge.
//
// History built here: commit A (newFixtureRepo's own init commit, still
// main's tip) -- branch "review-branch" adds b_test.go and edits
// content.txt as commit B -- this run (-on-branch review-branch) then
// edits content.txt again (FAKE_BUILD_APP_MODE=commit) as the round's own
// commit C, which alone touches only content.txt. The ticket declares
// Required-Changed-Files: content.txt,b_test.go -- satisfiable only by
// the cumulative range A..HEAD, not by B..HEAD (round B's own base_sha),
// which is exactly what -diff-base=A selects.
func TestIntegrationDiffBaseComputesCumulativeChangedFiles(t *testing.T) {
	ws := newFixtureRepo(t)

	commitA, err := exec.Command("git", "-C", ws, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("rev-parse HEAD: %v", err)
	}
	diffBase := strings.TrimSpace(string(commitA))

	if out, err := exec.Command("git", "-C", ws, "checkout", "-b", "review-branch").CombinedOutput(); err != nil {
		t.Fatalf("create review-branch: %v: %s", err, out)
	}
	if err := os.WriteFile(filepath.Join(ws, "b_test.go"), []byte("package fixture\n"), 0o644); err != nil {
		t.Fatalf("write b_test.go: %v", err)
	}
	if err := os.WriteFile(filepath.Join(ws, "content.txt"), []byte("from review-branch\n"), 0o644); err != nil {
		t.Fatalf("edit content.txt: %v", err)
	}
	for _, args := range [][]string{{"add", "-A"}, {"commit", "-q", "-m", "commit B: review-branch adds b_test.go"}} {
		if out, err := exec.Command("git", append([]string{"-C", ws}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	if out, err := exec.Command("git", "-C", ws, "checkout", "main").CombinedOutput(); err != nil {
		t.Fatalf("checkout main: %v: %s", err, out)
	}

	dataDir := t.TempDir()
	specContent := "# Ticket: fixture\n\n## Out of scope\n\nAllowed-Files: content.txt, b_test.go\nRequired-Changed-Files: content.txt, b_test.go\n"
	r := runFactorydWithSpecFlagsAndDataDir(t, ws, "commit", "true", specContent, "30s", nil,
		[]string{"-on-branch", "review-branch", "-diff-base", diffBase}, dataDir)

	if r.State != run.StateAccepted {
		t.Fatalf("state = %q, want %q (GateResults/HaltError: %v/%q)", r.State, run.StateAccepted, r.GateResults, r.HaltError)
	}
	if r.DiffBaseSHA != diffBase {
		t.Errorf("DiffBaseSHA = %q, want %q", r.DiffBaseSHA, diffBase)
	}
	if !slices.Contains(r.ChangedFiles, "content.txt") || !slices.Contains(r.ChangedFiles, "b_test.go") {
		t.Errorf("ChangedFiles = %v, want it to include both content.txt and b_test.go from the cumulative range", r.ChangedFiles)
	}
}

// TestIntegrationDiffBaseRefusesNonAncestorSHA proves -diff-base is
// validated against base_sha before this run ever starts executing: a SHA
// that is not an ancestor of base_sha (here, a commit that only exists on
// an unrelated branch) must halt the run rather than silently compute a
// nonsensical diff range.
func TestIntegrationDiffBaseRefusesNonAncestorSHA(t *testing.T) {
	ws := newFixtureRepo(t)

	if out, err := exec.Command("git", "-C", ws, "checkout", "-b", "unrelated-branch").CombinedOutput(); err != nil {
		t.Fatalf("create unrelated-branch: %v: %s", err, out)
	}
	if err := os.WriteFile(filepath.Join(ws, "unrelated.txt"), []byte("not an ancestor of main\n"), 0o644); err != nil {
		t.Fatalf("write unrelated.txt: %v", err)
	}
	for _, args := range [][]string{{"add", "-A"}, {"commit", "-q", "-m", "unrelated commit"}} {
		if out, err := exec.Command("git", append([]string{"-C", ws}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	notAncestor, err := exec.Command("git", "-C", ws, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("rev-parse HEAD: %v", err)
	}
	if out, err := exec.Command("git", "-C", ws, "checkout", "main").CombinedOutput(); err != nil {
		t.Fatalf("checkout main: %v: %s", err, out)
	}
	if out, err := exec.Command("git", "-C", ws, "branch", "review-branch-2").CombinedOutput(); err != nil {
		t.Fatalf("create review-branch-2: %v: %s", err, out)
	}

	dataDir := t.TempDir()
	specContent := "# Ticket: fixture\n\n## Out of scope\n\nAllowed-Files: content.txt\n"
	r := runFactorydWithSpecFlagsAndDataDir(t, ws, "commit", "true", specContent, "30s", nil,
		[]string{"-on-branch", "review-branch-2", "-diff-base", strings.TrimSpace(string(notAncestor))}, dataDir)

	if r.State != run.StateHalted {
		t.Fatalf("state = %q, want %q", r.State, run.StateHalted)
	}
}
