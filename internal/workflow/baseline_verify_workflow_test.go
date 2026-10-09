package workflow

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/converter"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"
	temporalworkflow "go.temporal.io/sdk/workflow"

	"buildgate/internal/run"
	"buildgate/internal/runner"
)

// baselineFixture runs RunWorkflow with the given RunBaselineVerifyActivity.
// It returns the workflow error and result, the ActivityIDs started in
// order, and the input the build Activity was given (nil when it never ran).
func baselineFixture(t *testing.T, baseline func(context.Context, RunWorkflowInput) (BaselineVerifyResult, error), pin func(*testsuite.TestWorkflowEnvironment)) (error, RunWorkflowResult, []string, *RunWorkflowInput) {
	t.Helper()
	var mu sync.Mutex
	var buildInput *RunWorkflowInput
	env := newWorkflowEnvironmentWithBaseline(t,
		func(_ context.Context, input RunWorkflowInput) (BuildActivityResult, error) {
			mu.Lock()
			defer mu.Unlock()
			buildInput = &input
			return BuildActivityResult{Result: runner.Result{ExitCode: 0}, Attempts: []run.Attempt{{Kind: "build"}}}, nil
		},
		func(context.Context, RunWorkflowInput) (VerifyActivityResult, error) {
			return VerifyActivityResult{Result: runner.Result{Command: []string{"sh", "-c", "make verify"}, ExitCode: 0}, Attempts: []run.Attempt{{Kind: "verify"}}}, nil
		},
		func(context.Context, EvaluateGateInput) (run.GateResult, error) {
			return run.GateResult{Check: "canonical_verify", Passed: true}, nil
		},
		baseline,
	)
	if pin != nil {
		pin(env)
	}
	var ids []string
	env.SetOnActivityStartedListener(func(info *activity.Info, _ context.Context, _ converter.EncodedValues) {
		mu.Lock()
		defer mu.Unlock()
		ids = append(ids, info.ActivityID)
	})
	env.ExecuteWorkflow(RunWorkflow, fixtureInput())
	var result RunWorkflowResult
	err := env.GetWorkflowError()
	if err == nil {
		if getErr := env.GetWorkflowResult(&result); getErr != nil {
			t.Fatalf("workflow result: %v", getErr)
		}
	}
	return err, result, ids, buildInput
}

var baselineAttempt = run.Attempt{Kind: run.BaselineVerifyAttemptKind, Command: []string{"sh", "-c", "make verify"}, ExitCode: 1, LogPath: "/logs/baseline_verify.log"}

func TestRunWorkflowRunsTheBaselineVerifyBetweenPreflightAndTheBuild(t *testing.T) {
	err, result, ids, buildInput := baselineFixture(t, func(context.Context, RunWorkflowInput) (BaselineVerifyResult, error) {
		passed := baselineAttempt
		passed.ExitCode = 0
		return BaselineVerifyResult{Record: run.BaselineVerify{Passed: true}, Attempts: []run.Attempt{passed}}, nil
	}, nil)
	if err != nil || result.State != run.StateAccepted {
		t.Fatalf("err=%v state=%v, want accepted", err, result.State)
	}
	preflight, baseline, build := indexOf(ids, "preflight"), indexOf(ids, "baseline-verify"), indexOf(ids, "build")
	if preflight < 0 || baseline != preflight+1 || build != baseline+1 {
		t.Errorf("Activity order %q, want preflight, baseline-verify, build in a row", ids)
	}
	if len(result.Attempts) == 0 || result.Attempts[0].Kind != run.BaselineVerifyAttemptKind {
		t.Errorf("attempts = %+v, want the baseline's first", result.Attempts)
	}
	if buildInput == nil || buildInput.BaselineNotePath != "" {
		t.Errorf("build input = %+v, want a build given no baseline note", buildInput)
	}
}

func TestABaselineFailureTheTicketDoesNotNameHaltsBeforeTheBuild(t *testing.T) {
	record := run.BaselineVerify{Command: "make verify", ExitCode: 1, FailingTests: []string{"tests/test_pager.py::test_less"}, FailingCount: 1, Unnamed: []string{"tests/test_pager.py::test_less"}, UnnamedCount: 1}
	err, _, ids, buildInput := baselineFixture(t, func(context.Context, RunWorkflowInput) (BaselineVerifyResult, error) {
		return BaselineVerifyResult{}, temporal.NewNonRetryableApplicationError(record.HaltMessage(), BaselineVerifyFailureType, nil, []run.Attempt{baselineAttempt})
	}, nil)
	if err == nil {
		t.Fatal("workflow succeeded, want a halt on the baseline")
	}
	if !hasApplicationErrorType(err, BaselineVerifyFailureType) {
		t.Errorf("workflow error does not keep type %q: %v", BaselineVerifyFailureType, err)
	}
	if buildInput != nil || containsString(ids, "build") {
		t.Errorf("the build ran after a baseline the ticket does not name: %q", ids)
	}
	if got := HaltReasonCodeFromError(err); got != run.HaltReasonBaselineVerifyFailed {
		t.Errorf("halt reason code = %q, want %q", got, run.HaltReasonBaselineVerifyFailed)
	}
	if attempts := AttemptsFromError(err); len(attempts) != 1 || attempts[0].Kind != run.BaselineVerifyAttemptKind {
		t.Errorf("attempts on the error = %+v, want the baseline's", attempts)
	}
	if wp, br := IsolatedWorkspaceFromError(err); wp == "" || br == "" {
		t.Errorf("isolated workspace on the error = %q %q, want the worktree the rollback discards", wp, br)
	}
	if !containsString(ids, "rollback-workspace") {
		t.Errorf("the worktree was not rolled back: %q", ids)
	}
}

func TestAnExpectedBaselineFailureGivesTheBuildItsNote(t *testing.T) {
	err, result, _, buildInput := baselineFixture(t, func(context.Context, RunWorkflowInput) (BaselineVerifyResult, error) {
		return BaselineVerifyResult{
			Record:        run.BaselineVerify{ExitCode: 1, FailingTests: []string{"TestTrimBOM"}, FailingCount: 1, Expected: true},
			Attempts:      []run.Attempt{baselineAttempt},
			BuildNotePath: "/logs/baseline_failure.md",
		}, nil
	}, nil)
	if err != nil || result.State != run.StateAccepted {
		t.Fatalf("err=%v state=%v, want accepted", err, result.State)
	}
	if buildInput == nil || buildInput.BaselineNotePath != "/logs/baseline_failure.md" {
		t.Errorf("build input = %+v, want the baseline note", buildInput)
	}
}

// TestHistoryRecordedBeforeTheBaselineVerifyReplaysWithoutIt pins the change
// to DefaultVersion, what a history recorded before baseline-verify replays
// as: preflight is followed by the build, with no Activity between them.
func TestHistoryRecordedBeforeTheBaselineVerifyReplaysWithoutIt(t *testing.T) {
	var called atomic.Bool
	pin := func(env *testsuite.TestWorkflowEnvironment) {
		env.OnGetVersion(baselineVerifyChange, temporalworkflow.DefaultVersion, 1).Return(temporalworkflow.DefaultVersion)
	}
	err, result, ids, buildInput := baselineFixture(t, func(context.Context, RunWorkflowInput) (BaselineVerifyResult, error) {
		called.Store(true)
		return BaselineVerifyResult{}, nil
	}, pin)
	if err != nil || result.State != run.StateAccepted {
		t.Fatalf("replay of a history from before the baseline verify: err=%v state=%v, want accepted", err, result.State)
	}
	if called.Load() || containsString(ids, "baseline-verify") {
		t.Errorf("the baseline verify ran on a history recorded before it: %q", ids)
	}
	if preflight, build := indexOf(ids, "preflight"), indexOf(ids, "build"); preflight < 0 || build != preflight+1 {
		t.Errorf("Activity order %q, want the build right after preflight", ids)
	}
	if buildInput == nil || buildInput.BaselineNotePath != "" {
		t.Errorf("build input = %+v, want no baseline note", buildInput)
	}
}

// TestBaselineFailuresDoNotTripTheStopLine: a verify command that fails on
// the base commit says nothing about the factory, so any number of runs
// halting on it leave the repository's queue open.
func TestBaselineFailuresDoNotTripTheStopLine(t *testing.T) {
	record := run.BaselineVerify{Command: "make verify", ExitCode: 127}
	env := newWorkflowEnvironmentWithBaseline(t,
		func(context.Context, RunWorkflowInput) (BuildActivityResult, error) {
			t.Error("a build ran after a failed baseline")
			return BuildActivityResult{}, nil
		},
		func(context.Context, RunWorkflowInput) (VerifyActivityResult, error) {
			return VerifyActivityResult{}, nil
		},
		func(context.Context, EvaluateGateInput) (run.GateResult, error) { return run.GateResult{}, nil },
		func(context.Context, RunWorkflowInput) (BaselineVerifyResult, error) {
			return BaselineVerifyResult{}, temporal.NewNonRetryableApplicationError(record.HaltMessage(), BaselineVerifyFailureType, nil, []run.Attempt{baselineAttempt})
		},
	)
	repository := "fixture/baseline-stop-line"
	env.SetStartWorkflowOptions(client.StartWorkflowOptions{ID: RepositoryOwnerWorkflowID(repository)})
	env.RegisterWorkflow(RunWorkflow)
	env.RegisterDelayedCallback(func() {
		for _, id := range []string{"run-1", "run-2", "run-3"} {
			env.SignalWorkflow(SubmitRunSignalName, SubmitRunSignal{RequestID: id, Input: fixtureInputForTicket(id)})
		}
	}, 0)
	env.ExecuteWorkflow(RepositoryOwnerWorkflow, RepositoryOwnerWorkflowInput{Repository: repository, IdleTimeout: time.Second, StopLineFailureThreshold: 2})
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error: %v", err)
	}
	var result RepositoryOwnerResult
	if err := env.GetWorkflowResult(&result); err != nil {
		t.Fatalf("workflow result: %v", err)
	}
	if result.StopLineTripped || result.SystemicFailureStreak != 0 {
		t.Errorf("stop line: tripped=%v streak=%d, want an open queue", result.StopLineTripped, result.SystemicFailureStreak)
	}
	for _, id := range []string{"run-1", "run-2", "run-3"} {
		got := result.Runs[id]
		if got.State != run.StateHalted || got.HaltReasonCode != run.HaltReasonBaselineVerifyFailed {
			t.Errorf("%s = state %q code %q, want halted with %q", id, got.State, got.HaltReasonCode, run.HaltReasonBaselineVerifyFailed)
		}
		if len(got.Attempts) != 1 || got.Attempts[0].Kind != run.BaselineVerifyAttemptKind {
			t.Errorf("%s attempts = %+v, want the baseline's", id, got.Attempts)
		}
	}
}
