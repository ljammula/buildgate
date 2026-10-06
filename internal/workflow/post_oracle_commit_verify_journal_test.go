package workflow

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"

	"buildgate/internal/run"
	"buildgate/internal/runner"
)

// runInActivity runs body inside an Activity execution context.
func runInActivity(t *testing.T, body func(ctx context.Context) error) error {
	t.Helper()
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestActivityEnvironment()
	env.RegisterActivityWithOptions(func(ctx context.Context) error { return body(ctx) }, activity.RegisterOptions{Name: "body"})
	_, err := env.ExecuteActivity("body")
	return err
}

func detailAttempts(t *testing.T, err error) []run.Attempt {
	t.Helper()
	var appErr *temporal.ApplicationError
	if !errors.As(err, &appErr) {
		t.Errorf("err = %v, not an ApplicationError", err)
		return nil
	}
	var attempts []run.Attempt
	if err := appErr.Details(&attempts); err != nil {
		t.Errorf("details: %v", err)
	}
	return attempts
}

func TestRecoverAttemptsRejectsPostCommitJournalHoldingAPlainVerifyAttempt(t *testing.T) {
	dir := t.TempDir()
	// Recovery returns early without the checkpoints directory (a real run
	// always has one by the time a journal exists).
	if err := os.MkdirAll(filepath.Join(dir, "activity-checkpoints"), 0o750); err != nil {
		t.Fatal(err)
	}
	bad := []run.Attempt{{Kind: "verify", StartedAt: "2024-01-01T00:00:00Z", LogPath: "/x.log"}}
	if err := saveActivityAttemptJournalForExecution(dir, "wf", "run", "act", 1, postOracleCommitJournalKind, bad); err != nil {
		t.Fatal(err)
	}
	if got := RecoverAttemptsFromCheckpointDir(dir); len(got) != 0 {
		t.Fatalf("recovery accepted a post-commit journal with a foreign attempt kind: %+v", got)
	}
	if _, _, err := loadActivityAttemptJournalForExecution(dir, "wf", "run", "act", 1); err == nil {
		t.Fatal("loader accepted a post-commit journal with a foreign attempt kind")
	}
	good := []run.Attempt{{Kind: postOracleCommitFullSuiteKind, StartedAt: "2024-01-01T00:00:00Z", LogPath: "/x.log"}}
	if err := saveActivityAttemptJournalForExecution(dir, "wf", "run", "act", 1, postOracleCommitJournalKind, good); err != nil {
		t.Fatal(err)
	}
	if got := RecoverAttemptsFromCheckpointDir(dir); len(got) != 1 {
		t.Fatalf("recovery rejected a valid post-commit journal: %+v", got)
	}
}

func TestPostOracleCommitVerifyActivityHaltsOnJournalWithoutIntent(t *testing.T) {
	a, input, commands := postVerifyFixture(t, func(string) int { return 0 }, nil)
	err := runInActivity(t, func(ctx context.Context) error {
		info := activity.GetInfo(ctx)
		journal := []run.Attempt{{Kind: postOracleCommitVerifyKind, StartedAt: "2024-01-01T00:00:00Z", LogPath: "/x.log"}}
		if err := saveActivityAttemptJournalForExecution(a.LogDir, info.WorkflowExecution.ID, info.WorkflowExecution.RunID, info.ActivityID, 1, postOracleCommitJournalKind, journal); err != nil {
			return err
		}
		_, err := a.RunPostOracleCommitVerifyActivity(ctx, input)
		return err
	})
	if err == nil || !strings.Contains(err.Error(), "journal exists without its intent") {
		t.Fatalf("err = %v, want the journal-without-intent halt", err)
	}
	if len(*commands) != 0 {
		t.Fatalf("commands ran: %v", *commands)
	}
}

func TestPostOracleCommitVerifyActivityReplaysStoredCheckpointError(t *testing.T) {
	a, input, commands := postVerifyFixture(t, func(string) int { return 0 }, nil)
	stored := []run.Attempt{{Kind: postOracleCommitVerifyKind, StartedAt: "2024-01-01T00:00:00Z", LogPath: "/x.log"}}
	err := runInActivity(t, func(ctx context.Context) error {
		info := activity.GetInfo(ctx)
		path := activityCheckpointPath(a.LogDir, info.WorkflowExecution.ID, info.WorkflowExecution.RunID, info.ActivityID)
		cp := activityCheckpoint[PostOracleCommitVerifyResult]{Completed: true, WorkflowID: info.WorkflowExecution.ID, RunID: info.WorkflowExecution.RunID, ActivityID: info.ActivityID, Error: "stored failure", Result: PostOracleCommitVerifyResult{Attempts: stored}}
		if err := saveActivityCheckpoint(path, cp, 1); err != nil {
			return err
		}
		_, err := a.RunPostOracleCommitVerifyActivity(ctx, input)
		if err == nil || !strings.Contains(err.Error(), "stored failure") {
			t.Errorf("err = %v, want the stored failure", err)
			return nil
		}
		if got := detailAttempts(t, err); len(got) != 1 || got[0].Kind != postOracleCommitVerifyKind {
			t.Errorf("Details attempts = %+v, want the stored ones", got)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(*commands) != 0 {
		t.Fatalf("a stored checkpoint must not re-run anything: %v", *commands)
	}
}

func TestPostOracleCommitVerifyActivityPhaseOneInfrastructureFailureSkipsPhaseTwo(t *testing.T) {
	a, input, commands := postVerifyFixture(t, func(string) int { return 0 }, nil)
	fake := a.runWithRetriesChecked
	a.runWithRetriesChecked = func(ctx context.Context, ws string, logPath func(int) string, n int, before func(int) error, after func(int, runner.Result, error) error, name string, args ...string) (runner.Result, error) {
		res, err := fake(ctx, ws, logPath, n, before, after, name, args...)
		if err == nil {
			err = errors.New("docker daemon went away")
		}
		return res, err
	}
	err := runInActivity(t, func(ctx context.Context) error {
		_, err := a.RunPostOracleCommitVerifyActivity(ctx, input)
		if err == nil {
			t.Error("phase-1 infrastructure failure returned no error")
			return nil
		}
		if got := detailAttempts(t, err); len(got) != 1 || got[0].Kind != postOracleCommitVerifyKind {
			t.Errorf("error Details attempts = %+v, want the one completed attempt", got)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(*commands) != 1 {
		t.Fatalf("phase 2 ran after a phase-1 infrastructure failure: %v", *commands)
	}
}
