package workflow

import (
	"buildgate/internal/runner"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"
)

// TestRunVerifyActivityHaltsOnAmbiguousPriorIntent is
// TestRunBuildActivityHaltsOnAmbiguousPriorIntent's counterpart for the
// verify Activity's own instance of the same crash window.
func TestRunVerifyActivityHaltsOnAmbiguousPriorIntent(t *testing.T) {
	calls := 0
	dir := t.TempDir()
	activities := &Activities{
		LogDir: dir,
		runWithRetries: func(context.Context, string, func(int) string, int, func(int, runner.Result, error), string, ...string) (runner.Result, error) {
			calls++
			return runner.Result{ExitCode: 0}, nil
		},
	}
	wrapper := func(ctx context.Context, input RunWorkflowInput) error {
		info := activity.GetInfo(ctx)
		if _, err := recordActivityIntentForExecution(dir, info.WorkflowExecution.ID, info.WorkflowExecution.RunID, info.ActivityID, 1, "build", []string{"stale"}, "2024-01-01T00:00:00Z"); err != nil {
			return err
		}
		_, err := activities.RunVerifyActivity(ctx, input)
		return err
	}

	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestActivityEnvironment()
	env.RegisterActivity(wrapper)
	_, err := env.ExecuteActivity(wrapper, fixtureInput())
	if err == nil {
		t.Fatal("execute verify Activity with pre-existing intent: want error, got nil")
	}
	if !strings.Contains(err.Error(), "prior attempt") {
		t.Fatalf("error = %q, want it to mention the prior attempt", err.Error())
	}
	if calls != 0 {
		t.Fatalf("subprocess calls with a pre-existing intent record = %d, want 0", calls)
	}
}

func TestRunVerifyActivityDuplicateInvocationRunsSubprocessOnce(t *testing.T) {
	calls := 0
	var logPaths []string
	logPath := filepath.Join(t.TempDir(), "verify.log")
	if err := os.WriteFile(logPath, []byte("verified\n"), 0o644); err != nil {
		t.Fatalf("write verify log: %v", err)
	}
	activities := &Activities{
		LogDir: t.TempDir(),
		runWithRetries: func(_ context.Context, _ string, activityLogPath func(int) string, _ int, _ func(int, runner.Result, error), _ string, _ ...string) (runner.Result, error) {
			calls++
			logPaths = append(logPaths, activityLogPath(1))
			started := time.Now()
			return runner.Result{ExitCode: 0, StartedAt: started, FinishedAt: started.Add(time.Second), LogPath: logPath}, nil
		},
	}
	wrapper := func(ctx context.Context, input RunWorkflowInput) error {
		if _, err := activities.RunVerifyActivity(ctx, input); err != nil {
			return err
		}
		_, err := activities.RunVerifyActivity(ctx, input)
		return err
	}

	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestActivityEnvironment()
	env.RegisterActivity(wrapper)
	if _, err := env.ExecuteActivity(wrapper, fixtureInput()); err != nil {
		t.Fatalf("execute duplicate verify Activity: %v", err)
	}
	if calls != 1 {
		t.Fatalf("subprocess calls after duplicate invocation = %d, want 1", calls)
	}

	if _, err := env.ExecuteActivity(wrapper, fixtureInput()); err != nil {
		t.Fatalf("execute verify Activity with different ID: %v", err)
	}
	if calls != 2 {
		t.Fatalf("subprocess calls after different Activity ID = %d, want 2", calls)
	}
	if logPaths[0] == logPaths[1] {
		t.Fatalf("verify log paths for different Activity executions = %q, want distinct paths", logPaths[0])
	}
}

// TestRunFullSuiteVerifyActivityDuplicateInvocationRunsSubprocessOnce is
// TestRunVerifyActivityDuplicateInvocationRunsSubprocessOnce's counterpart
// for RunFullSuiteVerifyActivity (gap 3 of the plan's 2026-08-28 readiness
// review, the regression oracle): its checkpoint must key by this
// Activity's own ActivityID, not collide with RunVerifyActivity's.
func TestRunFullSuiteVerifyActivityDuplicateInvocationRunsSubprocessOnce(t *testing.T) {
	calls := 0
	var logPaths []string
	logPath := filepath.Join(t.TempDir(), "full_suite_verify.log")
	if err := os.WriteFile(logPath, []byte("verified\n"), 0o644); err != nil {
		t.Fatalf("write full-suite verify log: %v", err)
	}
	activities := &Activities{
		LogDir: t.TempDir(),
		runWithRetries: func(_ context.Context, _ string, activityLogPath func(int) string, _ int, _ func(int, runner.Result, error), _ string, _ ...string) (runner.Result, error) {
			calls++
			logPaths = append(logPaths, activityLogPath(1))
			started := time.Now()
			return runner.Result{ExitCode: 0, StartedAt: started, FinishedAt: started.Add(time.Second), LogPath: logPath}, nil
		},
	}
	input := fixtureInput()
	input.FullSuiteCommand = "make verify-full"
	wrapper := func(ctx context.Context, input RunWorkflowInput) error {
		if _, err := activities.RunFullSuiteVerifyActivity(ctx, input); err != nil {
			return err
		}
		_, err := activities.RunFullSuiteVerifyActivity(ctx, input)
		return err
	}

	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestActivityEnvironment()
	env.RegisterActivity(wrapper)
	if _, err := env.ExecuteActivity(wrapper, input); err != nil {
		t.Fatalf("execute duplicate full-suite verify Activity: %v", err)
	}
	if calls != 1 {
		t.Fatalf("subprocess calls after duplicate invocation = %d, want 1", calls)
	}

	if _, err := env.ExecuteActivity(wrapper, input); err != nil {
		t.Fatalf("execute full-suite verify Activity with different ID: %v", err)
	}
	if calls != 2 {
		t.Fatalf("subprocess calls after different Activity ID = %d, want 2", calls)
	}
	if logPaths[0] == logPaths[1] {
		t.Fatalf("full-suite verify log paths for different Activity executions = %q, want distinct paths", logPaths[0])
	}
}

func TestBaselineCheckFullSuiteThatLeavesAnUnignoredFileHalts(t *testing.T) {
	workspace := baselineRepo(t)
	logDir := t.TempDir()
	checkpointDir := t.TempDir()
	activities := &Activities{
		LogDir: logDir,
		runWithRetries: func(_ context.Context, dir string, logPath func(int) string, _ int, onAttempt func(int, runner.Result, error), _ string, _ ...string) (runner.Result, error) {
			started := time.Now()
			path := logPath(1)
			if err := os.WriteFile(path, []byte("ok\n"), 0o600); err != nil {
				t.Fatalf("write full-suite log: %v", err)
			}
			leftWriteFile(t, dir, "test", "SSL key log\n")
			result := runner.Result{
				Command:    []string{"sh", "-c", "make full"},
				ExitCode:   0,
				StartedAt:  started,
				FinishedAt: started.Add(time.Second),
				LogPath:    path,
			}
			onAttempt(1, result, nil)
			return result, nil
		},
	}
	input := fixtureInput()
	input.BaselineCheck = true
	input.FullSuiteCommand = "make full"
	input.WorkspacePath = workspace
	input.LogDir = logDir
	input.CheckpointDir = checkpointDir
	input.RunID = "baseline-check"

	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestActivityEnvironment()
	env.RegisterActivity(activities.RunFullSuiteVerifyActivity)
	_, err := env.ExecuteActivity(activities.RunFullSuiteVerifyActivity, input)
	if err == nil || !strings.Contains(err.Error(), "test") || !strings.Contains(err.Error(), "No model call was made") {
		t.Fatalf("err=%v, want the baseline check to halt on its leftover", err)
	}
}

// A workspace whose state cannot be read after a baseline check's full-suite
// command is an infrastructure failure, in the returned error and in the
// checkpoint a redelivery replays, never a baseline verdict.
func TestBaselineCheckFullSuiteUnreadableWorkspaceIsInfrastructure(t *testing.T) {
	workspace := baselineRepo(t)
	checkpointDir := t.TempDir()
	activities := &Activities{
		LogDir: t.TempDir(),
		runWithRetries: func(_ context.Context, dir string, logPath func(int) string, _ int, onAttempt func(int, runner.Result, error), _ string, _ ...string) (runner.Result, error) {
			started := time.Now()
			path := logPath(1)
			if err := os.WriteFile(path, []byte("ok\n"), 0o600); err != nil {
				t.Fatalf("write full-suite log: %v", err)
			}
			if err := os.RemoveAll(filepath.Join(dir, ".git")); err != nil {
				t.Fatal(err)
			}
			result := runner.Result{Command: []string{"sh", "-c", "make full"}, StartedAt: started, FinishedAt: started.Add(time.Second), LogPath: path}
			onAttempt(1, result, nil)
			return result, nil
		},
	}
	input := fixtureInput()
	input.BaselineCheck = true
	input.FullSuiteCommand = "make full"
	input.WorkspacePath = workspace
	input.LogDir = activities.LogDir
	input.CheckpointDir = checkpointDir
	input.RunID = "baseline-check"

	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestActivityEnvironment()
	env.RegisterActivity(activities.RunFullSuiteVerifyActivity)
	_, err := env.ExecuteActivity(activities.RunFullSuiteVerifyActivity, input)
	var appErr *temporal.ApplicationError
	if !errors.As(err, &appErr) || appErr.Type() != InfrastructureFailureType {
		t.Fatalf("err=%v, want an %s", err, InfrastructureFailureType)
	}
	var checkpoints []string
	_ = filepath.WalkDir(checkpointDir, func(path string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			data, _ := os.ReadFile(path)
			checkpoints = append(checkpoints, string(data))
		}
		return nil
	})
	if joined := strings.Join(checkpoints, "\n"); !strings.Contains(joined, `"error_type": "`+InfrastructureFailureType+`"`) {
		t.Fatalf("checkpoints = %q, want the error recorded as %s", joined, InfrastructureFailureType)
	}
}

// TestRunVerifyActivityRecordsAttempts is
// TestRunBuildActivityRecordsAttempts' counterpart for the verify
// Activity's own onAttempt callback.
func TestRunVerifyActivityRecordsAttempts(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "verify.log")
	if err := os.WriteFile(logPath, []byte("verified\n"), 0o644); err != nil {
		t.Fatalf("write verify log: %v", err)
	}
	activities := &Activities{
		LogDir: t.TempDir(),
		runWithRetries: func(_ context.Context, _ string, _ func(int) string, _ int, onAttempt func(int, runner.Result, error), _ string, _ ...string) (runner.Result, error) {
			started := time.Now()
			onAttempt(1, runner.Result{Command: []string{"attempt-1"}, ExitCode: -1, StartedAt: started, FinishedAt: started.Add(time.Second)}, context.DeadlineExceeded)
			finalResult := runner.Result{Command: []string{"attempt-2"}, ExitCode: 0, StartedAt: started, FinishedAt: started.Add(2 * time.Second), LogPath: logPath}
			onAttempt(2, finalResult, nil)
			return finalResult, nil
		},
	}
	wrapper := func(ctx context.Context, input RunWorkflowInput) (VerifyActivityResult, error) {
		return activities.RunVerifyActivity(ctx, input)
	}

	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestActivityEnvironment()
	env.RegisterActivity(wrapper)
	raw, err := env.ExecuteActivity(wrapper, fixtureInput())
	if err != nil {
		t.Fatalf("execute verify Activity: %v", err)
	}
	var result VerifyActivityResult
	if err := raw.Get(&result); err != nil {
		t.Fatalf("decode verify Activity result: %v", err)
	}
	if len(result.Attempts) != 2 {
		t.Fatalf("Attempts = %+v, want 2 entries", result.Attempts)
	}
	if result.Attempts[0].Kind != "verify" || result.Attempts[0].ExitCode != -1 {
		t.Errorf("Attempts[0] = %+v, want kind=verify exit_code=-1", result.Attempts[0])
	}
	if result.Attempts[1].Kind != "verify" || result.Attempts[1].ExitCode != 0 {
		t.Errorf("Attempts[1] = %+v, want kind=verify exit_code=0", result.Attempts[1])
	}
}

func TestRunVerifyActivityHeartbeatsDuringSubprocess(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "verify.log")
	if err := os.WriteFile(logPath, []byte("verified\n"), 0o644); err != nil {
		t.Fatalf("write verify log: %v", err)
	}
	var heartbeats atomic.Int32
	activities := &Activities{
		LogDir: t.TempDir(),
		runWithRetries: func(ctx context.Context, _ string, _ func(int) string, _ int, _ func(int, runner.Result, error), _ string, _ ...string) (runner.Result, error) {
			for i := 0; i < 5; i++ {
				activity.RecordHeartbeat(ctx)
				heartbeats.Add(1)
				time.Sleep(time.Millisecond)
			}
			started := time.Now()
			return runner.Result{ExitCode: 0, StartedAt: started, FinishedAt: started.Add(time.Millisecond), LogPath: logPath}, nil
		},
	}
	wrapper := func(ctx context.Context, input RunWorkflowInput) error {
		_, err := activities.RunVerifyActivity(ctx, input)
		return err
	}
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestActivityEnvironment()
	env.RegisterActivity(wrapper)
	if _, err := env.ExecuteActivity(wrapper, fixtureInput()); err != nil {
		t.Fatalf("execute verify Activity: %v", err)
	}
	if heartbeats.Load() == 0 {
		t.Fatal("no heartbeats recorded during the verify subprocess")
	}
}
