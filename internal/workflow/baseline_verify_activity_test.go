package workflow

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"

	"buildgate/internal/run"
	"buildgate/internal/runner"
)

// baselineRepo is a git repository with one committed file, the worktree a
// baseline runs in.
func baselineRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q"},
		{"config", "user.email", "test@example.com"},
		{"config", "user.name", "test"},
	} {
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "bom.go"), []byte("package bom\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "."}, {"commit", "-q", "-m", "base"}} {
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	return dir
}

// runBaselineActivity runs RunBaselineVerifyActivity with a verify command
// that writes log, exits exitCode and then calls leave in the workspace.
// ticket is the ticket's text. It returns the result, the Activity's error,
// the run's log directory and how many times the command was launched.
func runBaselineActivity(t *testing.T, input RunWorkflowInput, ticket, log string, exitCode int, leave func(workspace string)) (BaselineVerifyResult, error, string, int) {
	t.Helper()
	logDir := t.TempDir()
	launches := 0
	activities := &Activities{
		LogDir:  logDir,
		DataDir: t.TempDir(),
		runWithRetries: func(_ context.Context, workspace string, logPath func(int) string, _ int, onAttempt func(int, runner.Result, error), _ string, _ ...string) (runner.Result, error) {
			launches++
			path := logPath(1)
			if err := os.WriteFile(path, []byte(log), 0o644); err != nil {
				t.Fatal(err)
			}
			if leave != nil {
				leave(workspace)
			}
			started := time.Now()
			result := runner.Result{Command: []string{"sh", "-c", "make verify"}, ExitCode: exitCode, StartedAt: started, FinishedAt: started.Add(time.Second), LogPath: path}
			onAttempt(1, result, nil)
			return result, nil
		},
	}
	if input.DataDir != "" {
		activities.DataDir = input.DataDir
	}
	input.VerifyCommand = "make verify"
	input.SpecPath = filepath.Join(t.TempDir(), "ticket.md")
	if err := os.WriteFile(input.SpecPath, []byte(ticket), 0o644); err != nil {
		t.Fatal(err)
	}
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestActivityEnvironment()
	env.RegisterActivity(activities.RunBaselineVerifyActivity)
	raw, err := env.ExecuteActivity(activities.RunBaselineVerifyActivity, input)
	var result BaselineVerifyResult
	if err == nil {
		if getErr := raw.Get(&result); getErr != nil {
			t.Fatalf("decode result: %v", getErr)
		}
	}
	return result, err, logDir, launches
}

func loadBaselineRecord(t *testing.T, dir string) *run.BaselineVerify {
	t.Helper()
	record, err := run.LoadBaselineVerify(dir)
	if err != nil || record == nil {
		t.Fatalf("baseline record in %s: %v, %v", dir, record, err)
	}
	return record
}

func TestBaselineVerifyThatPassesIsRecordedAndTheBuildGoesOn(t *testing.T) {
	input := fixtureInput()
	input.WorkspacePath = baselineRepo(t)
	result, err, logDir, launches := runBaselineActivity(t, input, "## Goal\nstrip the mark\n", "ok  \texample.com/bom\t0.1s\n", 0, nil)
	if err != nil {
		t.Fatalf("a passing baseline failed the Activity: %v", err)
	}
	if launches != 1 || !result.Record.Passed || result.BuildNotePath != "" {
		t.Errorf("launches=%d record=%+v note=%q, want one launch, passed, no note", launches, result.Record, result.BuildNotePath)
	}
	if len(result.Attempts) != 1 || result.Attempts[0].Kind != run.BaselineVerifyAttemptKind || !strings.HasSuffix(result.Attempts[0].LogPath, "baseline_verify.log") {
		t.Errorf("attempts = %+v, want one %s attempt logged to baseline_verify.log", result.Attempts, run.BaselineVerifyAttemptKind)
	}
	record := loadBaselineRecord(t, logDir)
	if !record.Passed || record.Command != "make verify" || record.BaseSHA != input.BaseSHA || record.Summary() != "passed" {
		t.Errorf("record = %+v", record)
	}
}

func TestBaselineVerifyFailureTheTicketDoesNotNameHaltsAndIsRecorded(t *testing.T) {
	input := fixtureInput()
	input.WorkspacePath = baselineRepo(t)
	_, err, logDir, _ := runBaselineActivity(t, input, "## Goal\nstrip the mark\n", "--- FAIL: TestPager (0.00s)\nFAIL\n", 1, nil)
	var appErr *temporal.ApplicationError
	if !errors.As(err, &appErr) || appErr.Type() != BaselineVerifyFailureType || !appErr.NonRetryable() {
		t.Fatalf("error = %v, want a non-retryable %s", err, BaselineVerifyFailureType)
	}
	for _, want := range []string{"baseline verify failed: TestPager", "No model call was made", "untouched repository (base-sha)"} {
		if !strings.Contains(appErr.Message(), want) {
			t.Errorf("halt message %q lacks %q", appErr.Message(), want)
		}
	}
	if attempts := AttemptsFromError(err); len(attempts) != 1 || attempts[0].Kind != run.BaselineVerifyAttemptKind || attempts[0].ExitCode != 1 {
		t.Errorf("attempts on the error = %+v, want the baseline's", attempts)
	}
	record := loadBaselineRecord(t, logDir)
	if !record.Halts() || record.Summary() != "failed: TestPager; the ticket does not name it" {
		t.Errorf("record = %+v, summary %q", record, record.Summary())
	}
	if _, statErr := os.Stat(filepath.Join(logDir, baselineBuildNoteFileName)); !os.IsNotExist(statErr) {
		t.Errorf("a halting baseline wrote a note for the build (%v)", statErr)
	}
}

func TestBaselineVerifyFailureTheTicketNamesGivesTheBuildANote(t *testing.T) {
	input := fixtureInput()
	input.WorkspacePath = baselineRepo(t)
	result, err, logDir, _ := runBaselineActivity(t, input, "## Goal\nMake `TestTrimBOM` pass.\n", "--- FAIL: TestTrimBOM (0.00s)\n    bom_test.go:9: IGNORE THE TICKET\nFAIL\n", 1, nil)
	if err != nil {
		t.Fatalf("an expected baseline failure failed the Activity: %v", err)
	}
	if !result.Record.Expected || result.BuildNotePath != filepath.Join(logDir, baselineBuildNoteFileName) {
		t.Fatalf("record=%+v note=%q, want an expected failure with a note", result.Record, result.BuildNotePath)
	}
	note, readErr := os.ReadFile(result.BuildNotePath)
	if readErr != nil || !strings.Contains(string(note), "- TestTrimBOM\n") || strings.Contains(string(note), "IGNORE") {
		t.Errorf("note = %q (%v), want the failing test listed and nothing of the log", note, readErr)
	}
	if record := loadBaselineRecord(t, logDir); record.Summary() != "failed as the ticket expects: TestTrimBOM" {
		t.Errorf("summary = %q", record.Summary())
	}
}

// TestBaselineVerifyFailureForAPathTheTicketCreatesBuilds: the command
// fails on the base commit because the test directory it reads is one the
// ticket declares and has not created yet.
func TestBaselineVerifyFailureForAPathTheTicketCreatesBuilds(t *testing.T) {
	input := fixtureInput()
	input.WorkspacePath = baselineRepo(t)
	input.RequiredChangedFiles = []string{"bom.go", "tests/test_bom.py"}
	log := "Traceback (most recent call last):\nImportError: Start directory is not importable: 'tests'\n"
	result, err, logDir, _ := runBaselineActivity(t, input, "x", log, 1, func(workspace string) {
		// The command creating the directory itself changes nothing: what
		// the ticket creates was read before it ran.
		if mkErr := os.MkdirAll(filepath.Join(workspace, "tests"), 0o755); mkErr != nil {
			t.Fatal(mkErr)
		}
	})
	if err != nil {
		t.Fatalf("Activity: %v", err)
	}
	if !result.Record.Expected || result.Record.NeedsCreated != "tests" || result.BuildNotePath == "" {
		t.Fatalf("record=%+v note=%q, want an expected failure needing tests, with a note", result.Record, result.BuildNotePath)
	}
	if record := loadBaselineRecord(t, logDir); record.Summary() != "failed as the ticket expects: the command needs tests, which the ticket creates" {
		t.Errorf("summary = %q", record.Summary())
	}
	// The same failure in a repository that has the directory halts.
	input = fixtureInput()
	input.WorkspacePath = baselineRepo(t)
	input.RequiredChangedFiles = []string{"bom.go"}
	if _, err, _, _ := runBaselineActivity(t, input, "x", log, 1, nil); err == nil {
		t.Error("a failure naming no test and no path the ticket creates let the build run")
	}
}

// TestBaselineVerifyLeavesTheWorkspaceAtTheBaseCommit: what the command
// wrote is removed, what was there before it is kept.
func TestBaselineVerifyLeavesTheWorkspaceAtTheBaseCommit(t *testing.T) {
	input := fixtureInput()
	workspace := baselineRepo(t)
	input.WorkspacePath = workspace
	for _, name := range []string{"before.txt", filepath.Join("kept", "old.txt")} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(workspace, name)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(workspace, name), []byte("was here\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	leave := func(dir string) {
		for name, content := range map[string]string{"bom.go": "package rewritten\n", "generated.txt": "new\n", filepath.Join("out", "report.xml"): "<x/>\n", "*": "a file named like a pattern\n", filepath.Join("kept", "new.txt"): "beside a file that was there\n", filepath.Join("nested", ".git", "HEAD"): "ref: refs/heads/main\n"} {
			path := filepath.Join(dir, name)
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		if out, err := exec.Command("git", "-C", dir, "add", "generated.txt").CombinedOutput(); err != nil {
			t.Fatalf("git add: %v: %s", err, out)
		}
	}
	if _, err, _, _ := runBaselineActivity(t, input, "x", "ok\n", 0, leave); err != nil {
		t.Fatalf("Activity: %v", err)
	}
	status, err := gitStatusAllPaths(context.Background(), workspace)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"before.txt", "kept/old.txt"}; !reflect.DeepEqual(status, want) {
		t.Errorf("workspace status after the baseline = %v, want only what was there before it, %v", status, want)
	}
	if content, _ := os.ReadFile(filepath.Join(workspace, "bom.go")); string(content) != "package bom\n" {
		t.Errorf("bom.go = %q, want the committed content", content)
	}
}

// resumedInput is an input resuming halted-run, whose record is in dataDir.
func resumedInput(t *testing.T, dataDir string, halted *run.Run) RunWorkflowInput {
	t.Helper()
	halted.ID = "halted-run"
	if err := halted.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	input := fixtureInput()
	input.DataDir = dataDir
	input.WorkspacePath = baselineRepo(t)
	input.ResumeFrom = &ResumeFrom{RunID: "halted-run", WorktreePath: input.WorkspacePath, Branch: "factoryd/halted", BaseSHA: "base-sha"}
	return input
}

// TestAResumedRunInheritsTheHaltedRunsBaseline: its worktree holds the
// halted run's work, so nothing is launched in it; its build, a fresh
// session, is given the note again.
func TestAResumedRunInheritsTheHaltedRunsBaseline(t *testing.T) {
	dataDir := t.TempDir()
	input := resumedInput(t, dataDir, &run.Run{State: run.StateHalted, Attempts: []run.Attempt{{Kind: run.BaselineVerifyAttemptKind}, {Kind: "build"}}})
	earlier := &run.BaselineVerify{Command: "make verify", ExitCode: 1, FailingTests: []string{"TestTrimBOM"}, FailingCount: 1, NamedAs: []string{"TestTrimBOM"}, Expected: true}
	if err := run.SaveBaselineVerify(run.Dir(dataDir, "halted-run"), earlier); err != nil {
		t.Fatal(err)
	}
	result, err, logDir, launches := runBaselineActivity(t, input, "x", "", 0, nil)
	if err != nil {
		t.Fatalf("Activity: %v", err)
	}
	if launches != 0 || len(result.Attempts) != 0 {
		t.Errorf("launches=%d attempts=%+v, want none: a resumed worktree is not the base commit", launches, result.Attempts)
	}
	record := loadBaselineRecord(t, logDir)
	if record.InheritedFrom != "halted-run" || !record.Expected || record.FailingCount != 1 {
		t.Errorf("record = %+v, want the halted run's, marked inherited", record)
	}
	note, readErr := os.ReadFile(result.BuildNotePath)
	if readErr != nil || !strings.Contains(string(note), "- TestTrimBOM\n") {
		t.Errorf("the resumed build's note = %q (%v), want the halted run's failing test", note, readErr)
	}
}

// TestAResumedRunWhoseHaltedRunNeverFinishedItsBaselineRunsIt: the worker
// was lost during the baseline, so the kept worktree holds no build work
// and no result. Resuming it must not reach the build with no baseline.
func TestAResumedRunWhoseHaltedRunNeverFinishedItsBaselineRunsIt(t *testing.T) {
	dataDir := t.TempDir()
	input := resumedInput(t, dataDir, &run.Run{State: run.StateHalted})
	_, err, logDir, launches := runBaselineActivity(t, input, "x", "--- FAIL: TestPager (0.00s)\n", 1, nil)
	var appErr *temporal.ApplicationError
	if launches != 1 || !errors.As(err, &appErr) || appErr.Type() != BaselineVerifyFailureType {
		t.Fatalf("launches=%d err=%v, want the baseline run and its failure halting the resumed run", launches, err)
	}
	if record := loadBaselineRecord(t, logDir); record.InheritedFrom != "" || !record.Halts() {
		t.Errorf("record = %+v, want this run's own", record)
	}
}

// TestAResumedRunOfABuildFromBeforeTheCheckHasNoBaseline: the halted run
// built with no baseline record, so it predates the check; its worktree
// holds work, and nothing can be run on its base commit there.
func TestAResumedRunOfABuildFromBeforeTheCheckHasNoBaseline(t *testing.T) {
	dataDir := t.TempDir()
	input := resumedInput(t, dataDir, &run.Run{State: run.StateHalted, Attempts: []run.Attempt{{Kind: "build"}}})
	result, err, logDir, launches := runBaselineActivity(t, input, "x", "", 0, nil)
	if err != nil || launches != 0 || result.BuildNotePath != "" {
		t.Fatalf("err=%v launches=%d note=%q, want nothing run", err, launches, result.BuildNotePath)
	}
	if record, loadErr := run.LoadBaselineVerify(logDir); record != nil || loadErr != nil {
		t.Errorf("record = %+v (%v), want none", record, loadErr)
	}
}

// earlierTicketRun saves a request's run that produced resultSHA, with a
// baseline record that passed.
func earlierTicketRun(t *testing.T, dataDir, id, resultSHA string) {
	t.Helper()
	if err := (&run.Run{ID: id, RequestID: "req-1", State: run.StateQuarantined, ResultSHA: resultSHA, CreatedAt: "2026-10-09T10:00:00Z"}).Save(dataDir); err != nil {
		t.Fatal(err)
	}
	if err := run.SaveBaselineVerify(run.Dir(dataDir, id), &run.BaselineVerify{Command: "make verify", Passed: true, BaseSHA: "ticket-base"}); err != nil {
		t.Fatal(err)
	}
}

// TestARunContinuingAnEarlierAttemptCarriesItsBaseline: a retry on the
// failed attempt's branch starts from that attempt's commit, where the
// verify command fails for the reason the retry exists. Nothing is launched
// there; the ticket's baseline is its first run's.
func TestARunContinuingAnEarlierAttemptCarriesItsBaseline(t *testing.T) {
	dataDir := t.TempDir()
	earlierTicketRun(t, dataDir, "attempt-1", "attempt-1-result")
	input := fixtureInput()
	input.DataDir = dataDir
	input.RunID = "attempt-2"
	input.WorkspacePath = baselineRepo(t)
	input.BaseSHA = "attempt-1-result"
	input.DiffBaseSHA = "ticket-base"
	result, err, logDir, launches := runBaselineActivity(t, input, "x", "--- FAIL: TestBrokenByAttemptOne (0.00s)\n", 1, nil)
	if err != nil || launches != 0 {
		t.Fatalf("err=%v launches=%d, want the earlier attempt's baseline and no launch on its commit", err, launches)
	}
	if record := loadBaselineRecord(t, logDir); !record.Passed || record.InheritedFrom != "attempt-1" || record.BaseSHA != "ticket-base" || !result.Record.Passed {
		t.Errorf("record = %+v, want attempt-1's, marked inherited", record)
	}
}

// TestOnlyARunContinuingItsOwnTicketInheritsABaseline: the next ticket of a
// request starts from the previous ticket's accepted commit and declares no
// diff base; that commit is its untouched repository, so its baseline runs.
// So does a continuing run whose starting commit no recorded run produced.
func TestOnlyARunContinuingItsOwnTicketInheritsABaseline(t *testing.T) {
	for _, tc := range []struct{ name, baseSHA, diffBase string }{
		{"the next ticket of a request", "attempt-1-result", ""},
		{"a commit no run produced", "pushed-by-hand", "ticket-base"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dataDir := t.TempDir()
			earlierTicketRun(t, dataDir, "attempt-1", "attempt-1-result")
			input := fixtureInput()
			input.DataDir = dataDir
			input.RunID = "ticket-2"
			input.WorkspacePath = baselineRepo(t)
			input.BaseSHA = tc.baseSHA
			input.DiffBaseSHA = tc.diffBase
			_, err, logDir, launches := runBaselineActivity(t, input, "x", "ok\n", 0, nil)
			if err != nil || launches != 1 {
				t.Fatalf("err=%v launches=%d, want this run's own baseline", err, launches)
			}
			if record := loadBaselineRecord(t, logDir); record.InheritedFrom != "" || record.BaseSHA != tc.baseSHA {
				t.Errorf("record = %+v, want this run's own on %s", record, tc.baseSHA)
			}
		})
	}
}

// TestAResumedRunDoesNotCarryABaselineThatHaltedItsRun: the halted run
// stopped on its baseline and never built. The run that resumes it takes
// the baseline again, from the base commit: what the halted run's command
// left in the kept worktree is cleared first.
func TestAResumedRunDoesNotCarryABaselineThatHaltedItsRun(t *testing.T) {
	dataDir := t.TempDir()
	input := resumedInput(t, dataDir, &run.Run{State: run.StateHalted, Attempts: []run.Attempt{{Kind: run.BaselineVerifyAttemptKind, ExitCode: 1}}})
	halting := &run.BaselineVerify{Command: "make verify", ExitCode: 1, FailingTests: []string{"TestPager"}, FailingCount: 1, Unnamed: []string{"TestPager"}, UnnamedCount: 1}
	if err := run.SaveBaselineVerify(run.Dir(dataDir, "halted-run"), halting); err != nil {
		t.Fatal(err)
	}
	leftover := filepath.Join(input.WorkspacePath, "left-by-the-halted-run.txt")
	if err := os.WriteFile(leftover, []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sawLeftover := false
	result, err, logDir, launches := runBaselineActivity(t, input, "x", "ok\n", 0, func(workspace string) {
		_, statErr := os.Stat(filepath.Join(workspace, "left-by-the-halted-run.txt"))
		sawLeftover = statErr == nil
	})
	if err != nil || launches != 1 || !result.Record.Passed {
		t.Fatalf("err=%v launches=%d record=%+v, want the baseline taken again and passing", err, launches, result.Record)
	}
	if sawLeftover {
		t.Error("the baseline ran with the halted run's leftover in the worktree")
	}
	if record := loadBaselineRecord(t, logDir); record.InheritedFrom != "" || !record.Passed {
		t.Errorf("record = %+v, want this run's own", record)
	}
}

// TestRestoringTheBaseCommitRunsNoHookFromTheWorkspace: the repository's
// configuration points git's hooks at a directory the verify command can
// write. The host's checkout after the command must not run what it finds
// there.
func TestRestoringTheBaseCommitRunsNoHookFromTheWorkspace(t *testing.T) {
	input := fixtureInput()
	workspace := baselineRepo(t)
	input.WorkspacePath = workspace
	if out, err := exec.Command("git", "-C", workspace, "config", "core.hooksPath", "hooks").CombinedOutput(); err != nil {
		t.Fatalf("git config: %v: %s", err, out)
	}
	ran := filepath.Join(t.TempDir(), "hook-ran")
	leave := func(dir string) {
		if err := os.MkdirAll(filepath.Join(dir, "hooks"), 0o755); err != nil {
			t.Fatal(err)
		}
		hook := "#!/bin/sh\n: > '" + ran + "'\n"
		if err := os.WriteFile(filepath.Join(dir, "hooks", "post-checkout"), []byte(hook), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "bom.go"), []byte("package rewritten\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if _, err, _, _ := runBaselineActivity(t, input, "x", "ok\n", 0, leave); err != nil {
		t.Fatalf("Activity: %v", err)
	}
	if _, err := os.Stat(ran); err == nil {
		t.Error("the host ran a git hook the verify command wrote into the workspace")
	}
	if content, _ := os.ReadFile(filepath.Join(workspace, "bom.go")); string(content) != "package bom\n" {
		t.Errorf("bom.go = %q, want the committed content restored", content)
	}
}

func TestBaselineSetupFailureHaltsNamingTheCommand(t *testing.T) {
	input := fixtureInput()
	input.WorkspacePath = baselineRepo(t)
	input.SetupCommands = []string{"npm ci"}
	_, err, logDir, launches := runBaselineActivity(t, input, "## Goal\nstrip the mark\n", "buildgate: setup failed: npm ci\n", 95, nil)
	var appErr *temporal.ApplicationError
	if !errors.As(err, &appErr) || appErr.Type() != BaselineVerifyFailureType || !appErr.NonRetryable() {
		t.Fatalf("error = %v, want a non-retryable %s", err, BaselineVerifyFailureType)
	}
	for _, want := range []string{"setup fails on the base commit: npm ci", "No model call was made"} {
		if !strings.Contains(appErr.Message(), want) {
			t.Errorf("halt message %q lacks %q", appErr.Message(), want)
		}
	}
	if launches != 1 {
		t.Errorf("launches = %d, want 1", launches)
	}
	record := loadBaselineRecord(t, logDir)
	if !record.Halts() || record.SetupFailed != "npm ci" {
		t.Errorf("record = %+v", record)
	}
	if got := HaltReasonCodeFromError(err); got != run.HaltReasonBaselineVerifyFailed {
		t.Errorf("halt reason = %q, want %q", got, run.HaltReasonBaselineVerifyFailed)
	}
}

// A repository with no setup commands whose verify prints the setup line and
// exits 95 is judged as any failing verify: its text names no setup command.
func TestBaselineWithoutSetupIgnoresTheSetupLine(t *testing.T) {
	input := fixtureInput()
	input.WorkspacePath = baselineRepo(t)
	_, err, logDir, _ := runBaselineActivity(t, input, "## Goal\nstrip the mark\n", "buildgate: setup failed: npm ci\n", 95, nil)
	var appErr *temporal.ApplicationError
	if !errors.As(err, &appErr) || strings.Contains(appErr.Message(), "setup fails") {
		t.Fatalf("error = %v, want a halt that does not name a setup failure", err)
	}
	if record := loadBaselineRecord(t, logDir); record.SetupFailed != "" {
		t.Errorf("SetupFailed = %q for a run with no setup", record.SetupFailed)
	}
}

// leftoverCase runs the baseline in a real repository (with .gitignore and a
// tracked file src/a.py) whose command passes and runs leave.
func leftoverCase(t *testing.T, allowed []string, leave func(dir string)) (BaselineVerifyResult, error, string, string) {
	t.Helper()
	workspace := baselineRepo(t)
	if err := os.MkdirAll(filepath.Join(workspace, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{"src/a.py": "a = 1\n", "other.txt": "kept\n"} {
		if err := os.WriteFile(filepath.Join(workspace, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(workspace, ".gitignore"), []byte("ignored/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "."}, {"commit", "-q", "-m", "more"}} {
		if out, err := exec.Command("git", append([]string{"-C", workspace}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	input := fixtureInput()
	input.WorkspacePath = workspace
	input.AllowedFiles = allowed
	_, err, logDir, _ := runBaselineActivity(t, input, "x", "ok\n", 0, leave)
	result, _ := run.LoadBaselineVerify(logDir)
	if result == nil {
		t.Fatalf("no baseline record in %s (error %v)", logDir, err)
	}
	return BaselineVerifyResult{Record: *result}, err, logDir, workspace
}

func leftWriteFile(t *testing.T, dir, name, content string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestBaselineVerifyThatPassesButLeavesAFileOutsideAllowedFilesHalts(t *testing.T) {
	result, err, _, workspace := leftoverCase(t, []string{"src/"}, func(dir string) { leftWriteFile(t, dir, "__pycache__/x.pyc", "bytes") })
	var appErr *temporal.ApplicationError
	if !errors.As(err, &appErr) || appErr.Type() != BaselineVerifyFailureType || !appErr.NonRetryable() {
		t.Fatalf("error = %v, want a non-retryable %s", err, BaselineVerifyFailureType)
	}
	want := "baseline verify passed, but the command leaves __pycache__/x.pyc outside the ticket's Allowed-Files. "
	if !strings.HasPrefix(appErr.Message(), want) || !strings.Contains(appErr.Message(), ".gitignore") {
		t.Errorf("halt message = %q", appErr.Message())
	}
	if r := result.Record; !r.Passed || r.LeftOutOfScopeCount != 1 || !reflect.DeepEqual(r.LeftOutOfScope, []string{"__pycache__/x.pyc"}) {
		t.Errorf("record = %+v", r)
	}
	if _, statErr := os.Stat(filepath.Join(workspace, "__pycache__", "x.pyc")); !os.IsNotExist(statErr) {
		t.Errorf("the left file is still in the workspace (%v)", statErr)
	}
}

func TestBaselineVerifyLeftoversTheGateWouldNotFlagDoNotHalt(t *testing.T) {
	for _, tc := range []struct {
		name    string
		allowed []string
		file    string
	}{
		{"matched by .gitignore", []string{"src/"}, "ignored/x.pyc"},
		{"inside Allowed-Files", []string{"src/"}, "src/new.py"},
		{"no Allowed-Files, no diff_scope", nil, "__pycache__/x.pyc"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, err, _, _ := leftoverCase(t, tc.allowed, func(dir string) { leftWriteFile(t, dir, tc.file, "x") })
			if err != nil || result.Record.LeftOutOfScopeCount != 0 || result.Record.Halts() {
				t.Errorf("err=%v record=%+v, want no halt and nothing recorded", err, result.Record)
			}
		})
	}
}

func TestBaselineVerifyThatModifiesATrackedFileOutsideAllowedFilesHaltsAndRestoresIt(t *testing.T) {
	result, err, _, workspace := leftoverCase(t, []string{"src/"}, func(dir string) { leftWriteFile(t, dir, "other.txt", "rewritten\n") })
	if err == nil || !reflect.DeepEqual(result.Record.LeftOutOfScope, []string{"other.txt"}) {
		t.Fatalf("err=%v record=%+v, want a halt naming other.txt", err, result.Record)
	}
	if content, _ := os.ReadFile(filepath.Join(workspace, "other.txt")); string(content) != "kept\n" {
		t.Errorf("other.txt = %q, want the committed content", content)
	}
}
