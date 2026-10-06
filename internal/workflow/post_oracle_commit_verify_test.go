package workflow

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/testsuite"

	"buildgate/internal/runner"
)

// postVerifyFixture returns a clean single-commit workspace plus Activities
// whose fake runner records every command and exits with exitFor(command).
// dirty, when non-nil, is called after each command to let a test write files.
func postVerifyFixture(t *testing.T, exitFor func(cmd string) int, dirty func(workspace, cmd string)) (*Activities, RunWorkflowInput, *[]string) {
	t.Helper()
	ws := t.TempDir()
	coGit(t, ws, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(ws, "a.txt"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	coGit(t, ws, "add", "-A")
	coGit(t, ws, "commit", "-q", "-m", "base")
	var commands []string
	a := &Activities{
		LogDir: t.TempDir(),
		runWithRetriesChecked: func(_ context.Context, workspace string, logPath func(int) string, _ int, before func(int) error, after func(int, runner.Result, error) error, _ string, args ...string) (runner.Result, error) {
			cmd := args[len(args)-1]
			commands = append(commands, cmd)
			if err := before(1); err != nil {
				return runner.Result{}, err
			}
			log := logPath(1)
			if err := os.MkdirAll(filepath.Dir(log), 0o750); err != nil {
				return runner.Result{}, err
			}
			if err := os.WriteFile(log, []byte("log of "+cmd+"\n"), 0o600); err != nil {
				return runner.Result{}, err
			}
			if dirty != nil {
				dirty(workspace, cmd)
			}
			started := time.Date(2024, time.January, 1, 0, 0, len(commands), 0, time.UTC)
			res := runner.Result{Command: []string{"sh", "-c", cmd}, StartedAt: started, FinishedAt: started.Add(time.Second), ExitCode: exitFor(cmd), LogPath: log}
			if err := after(1, res, nil); err != nil {
				return runner.Result{}, err
			}
			return res, nil
		},
	}
	input := fixtureInput()
	input.WorkspacePath = ws
	input.LogDir = a.LogDir
	input.VerifyCommand = "make verify"
	input.FullSuiteCommand = "make full"
	return a, input, &commands
}

func execPostVerify(t *testing.T, a *Activities, input RunWorkflowInput, times int) (PostOracleCommitVerifyResult, error) {
	t.Helper()
	wrapper := func(ctx context.Context, in RunWorkflowInput) (PostOracleCommitVerifyResult, error) {
		var res PostOracleCommitVerifyResult
		var err error
		for i := 0; i < times; i++ {
			res, err = a.RunPostOracleCommitVerifyActivity(ctx, in)
		}
		return res, err
	}
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestActivityEnvironment()
	env.RegisterActivityWithOptions(wrapper, activity.RegisterOptions{Name: "wrapper"})
	raw, err := env.ExecuteActivity("wrapper", input)
	if err != nil {
		return PostOracleCommitVerifyResult{}, err
	}
	var res PostOracleCommitVerifyResult
	if err := raw.Get(&res); err != nil {
		t.Fatal(err)
	}
	return res, nil
}

func TestPostOracleCommitVerifyActivityRunsVerifyAndFullSuiteAndIsIdempotent(t *testing.T) {
	a, input, commands := postVerifyFixture(t, func(string) int { return 0 }, nil)
	res, err := execPostVerify(t, a, input, 2)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(*commands, "|"); got != "make verify|make full" {
		t.Fatalf("commands = %q: the second call must return the checkpoint, not re-run", got)
	}
	if !res.Clean || res.Verify.Result.ExitCode != 0 || res.FullSuite == nil || res.FullSuite.Result.ExitCode != 0 {
		t.Fatalf("res = %+v", res)
	}
	if len(res.Attempts) != 2 || res.Attempts[0].Kind != "verify_after_oracle_commit" || res.Attempts[1].Kind != "full_suite_verify_after_oracle_commit" {
		t.Fatalf("Attempts = %+v", res.Attempts)
	}
	if res.Verify.LogSHA256 == "" || res.FullSuite.LogSHA256 == "" {
		t.Fatal("log hashes missing")
	}
	// Log paths are distinct from verify.log and from each other.
	l0, l1 := res.Attempts[0].LogPath, res.Attempts[1].LogPath
	if l0 == l1 || strings.HasSuffix(l0, "-verify.log") || !strings.HasSuffix(l0, "verify_after_oracle_commit.log") || !strings.HasSuffix(l1, "full_suite_verify_after_oracle_commit.log") {
		t.Fatalf("log paths = %q, %q", l0, l1)
	}
	// The dedicated checkpoint feeds crash recovery, and the journal it left
	// behind is one the loader and recovery both accept.
	if got := RecoverAttemptsFromCheckpointDir(a.LogDir); len(got) != 2 {
		t.Fatalf("recovered attempts = %+v", got)
	}
}

func TestPostOracleCommitVerifyActivityWithoutFullSuiteRunsOnlyVerify(t *testing.T) {
	a, input, commands := postVerifyFixture(t, func(string) int { return 0 }, nil)
	input.FullSuiteCommand = ""
	res, err := execPostVerify(t, a, input, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(*commands) != 1 || res.FullSuite != nil || !res.Clean {
		t.Fatalf("commands = %v res = %+v", *commands, res)
	}
}

func TestPostOracleCommitVerifyActivityReportsFailuresWithoutErroring(t *testing.T) {
	a, input, commands := postVerifyFixture(t, func(cmd string) int {
		if cmd == "make verify" {
			return 3
		}
		return 1
	}, nil)
	res, err := execPostVerify(t, a, input, 1)
	if err != nil {
		t.Fatalf("a failing command is a result, not an Activity error: %v", err)
	}
	if res.Verify.Result.ExitCode != 3 || res.FullSuite == nil || res.FullSuite.Result.ExitCode != 1 {
		t.Fatalf("res = %+v", res)
	}
	if len(*commands) != 2 {
		t.Fatalf("the full suite must still run when verify failed (direct-path parity): %v", *commands)
	}
}

func TestPostOracleCommitVerifyActivityReportsDirtyWorkspace(t *testing.T) {
	a, input, _ := postVerifyFixture(t, func(string) int { return 0 }, func(workspace, cmd string) {
		if cmd == "make full" {
			os.WriteFile(filepath.Join(workspace, "generated.txt"), []byte("x\n"), 0o644)
		}
	})
	res, err := execPostVerify(t, a, input, 1)
	if err != nil {
		t.Fatal(err)
	}
	if res.Clean {
		t.Fatal("a re-run that wrote a file was reported clean")
	}
}

func TestPostOracleCommitVerifyActivityHaltsOnAmbiguousPriorAttempt(t *testing.T) {
	a, input, commands := postVerifyFixture(t, func(string) int { return 0 }, nil)
	wrapper := func(ctx context.Context, in RunWorkflowInput) error {
		info := activity.GetInfo(ctx)
		if _, err := recordActivityIntentForExecution(a.LogDir, info.WorkflowExecution.ID, info.WorkflowExecution.RunID, info.ActivityID, 1, postOracleCommitVerifyKind, []string{"stale"}, "2024-01-01T00:00:00Z"); err != nil {
			return err
		}
		_, err := a.RunPostOracleCommitVerifyActivity(ctx, in)
		return err
	}
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestActivityEnvironment()
	env.RegisterActivity(wrapper)
	_, err := env.ExecuteActivity(wrapper, input)
	if err == nil || !strings.Contains(err.Error(), "prior attempt") {
		t.Fatalf("err = %v, want an ambiguous-prior-attempt halt", err)
	}
	if len(*commands) != 0 {
		t.Fatalf("commands ran despite the ambiguous intent: %v", *commands)
	}
}

// Intent numbering continues across the two phases, so a crash inside the
// full-suite phase is recovered as an unknown full-suite attempt instead of
// being mistaken for the already-journaled verify attempt.
func TestPostOracleCommitVerifyActivityCrashInSecondPhaseIsRecoverable(t *testing.T) {
	a, input, _ := postVerifyFixture(t, func(string) int { return 0 }, nil)
	fake := a.runWithRetriesChecked
	a.runWithRetriesChecked = func(ctx context.Context, ws string, logPath func(int) string, n int, before func(int) error, after func(int, runner.Result, error) error, name string, args ...string) (runner.Result, error) {
		if args[len(args)-1] == "make full" {
			if err := before(1); err != nil {
				return runner.Result{}, err
			}
			panic("simulated worker crash before the full suite finished")
		}
		return fake(ctx, ws, logPath, n, before, after, name, args...)
	}
	wrapper := func(ctx context.Context, in RunWorkflowInput) (err error) {
		defer func() { _ = recover() }()
		_, err = a.RunPostOracleCommitVerifyActivity(ctx, in)
		return err
	}
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestActivityEnvironment()
	env.RegisterActivity(wrapper)
	if _, err := env.ExecuteActivity(wrapper, input); err != nil {
		t.Fatal(err)
	}
	var kinds []string
	var unknown int
	for _, at := range RecoverAttemptsFromCheckpointDir(a.LogDir) {
		kinds = append(kinds, at.Kind)
		if at.ExitCode == -1 && at.FinishedAt == "" {
			unknown++
		}
	}
	if strings.Join(kinds, ",") != "verify_after_oracle_commit,full_suite_verify_after_oracle_commit" || unknown != 1 {
		t.Fatalf("recovered kinds = %v (unknown=%d)", kinds, unknown)
	}
}
