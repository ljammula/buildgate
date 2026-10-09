package workflow

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"
	temporalworkflow "go.temporal.io/sdk/workflow"

	"buildgate/internal/run"
	"buildgate/internal/runner"
)

// runWithBuildFailures runs RunWorkflow with a build Activity that fails
// with failType on its first `failures` attempts and succeeds afterwards. It
// returns the number of build attempts and the workflow's error and result.
func runWithBuildFailures(t *testing.T, failures int32, failType string, pin func(*testsuite.TestWorkflowEnvironment)) (attempts int32, err error, result RunWorkflowResult) {
	t.Helper()
	var calls atomic.Int32
	env := newWorkflowEnvironment(t,
		func(context.Context, RunWorkflowInput) (BuildActivityResult, error) {
			if calls.Add(1) <= failures {
				return BuildActivityResult{}, temporal.NewApplicationError("worker stopped", failType, []run.Attempt{{Kind: "build", ExitCode: -1}})
			}
			return BuildActivityResult{Result: runner.Result{ExitCode: 0}}, nil
		},
		func(context.Context, RunWorkflowInput) (VerifyActivityResult, error) {
			return VerifyActivityResult{Result: runner.Result{Command: []string{"sh", "-c", "make verify"}, ExitCode: 0}}, nil
		},
		func(context.Context, EvaluateGateInput) (run.GateResult, error) {
			return run.GateResult{Check: "canonical_verify", Passed: true}, nil
		},
	)
	if pin != nil {
		pin(env)
	}
	env.ExecuteWorkflow(RunWorkflow, fixtureInput())
	err = env.GetWorkflowError()
	if err == nil {
		if getErr := env.GetWorkflowResult(&result); getErr != nil {
			t.Fatalf("workflow result: %v", getErr)
		}
	}
	return calls.Load(), err, result
}

func TestBuildInfrastructureFailureIsRetriedOnceUnderVersion1AndRunAccepted(t *testing.T) {
	attempts, err, result := runWithBuildFailures(t, 1, InfrastructureFailureType, pinActivityRetriesVersion1)
	if err != nil {
		t.Fatalf("workflow error after one infrastructure failure: %v", err)
	}
	if attempts != 2 {
		t.Errorf("build attempts = %d, want 2", attempts)
	}
	if result.State != run.StateAccepted {
		t.Errorf("state = %v, want accepted", result.State)
	}
}

func TestBuildInfrastructureFailureTwiceUnderVersion1Halts(t *testing.T) {
	attempts, err, _ := runWithBuildFailures(t, 2, InfrastructureFailureType, pinActivityRetriesVersion1)
	if err == nil {
		t.Fatal("workflow succeeded after two infrastructure failures, want a halt")
	}
	if attempts != 2 {
		t.Errorf("build attempts = %d, want 2 (one retry, no more)", attempts)
	}
}

func TestBuildNonRetryableFailureTypesAreNotRetried(t *testing.T) {
	for _, failType := range nonRetryableActivityFailureTypes {
		attempts, err, _ := runWithBuildFailures(t, 1, failType, pinActivityRetriesVersion1)
		if err == nil {
			t.Errorf("%s: workflow succeeded, want a halt", failType)
		}
		if attempts != 1 {
			t.Errorf("%s: build attempts = %d, want 1 (no retry)", failType, attempts)
		}
	}
}

func TestBuildIsNotRetriedWhenActivityRetriesVersionIsPinnedToDefault(t *testing.T) {
	attempts, err, _ := runWithBuildFailures(t, 1, InfrastructureFailureType, pinNoActivityRetries)
	if err == nil {
		t.Fatal("workflow succeeded, want a halt for a run replayed from before activity-retries")
	}
	if attempts != 1 {
		t.Errorf("build attempts = %d, want 1", attempts)
	}
}

// TestNewExecutionDoesNotRetryALostActivity: a new RunWorkflow execution asks
// GetVersion for activity-retries with maximum 2 and gets 2, which does not
// retry: a lost build is never rerun by Temporal, the request waits in
// resume_review for a human. The pin names the maximum, so it fails if the
// workflow stops asking for version 2.
func TestNewExecutionDoesNotRetryALostActivity(t *testing.T) {
	recordsVersion2 := func(env *testsuite.TestWorkflowEnvironment) {
		env.OnGetVersion(activityRetriesChange, temporalworkflow.DefaultVersion, 2).Return(temporalworkflow.Version(2))
	}
	for name, pin := range map[string]func(*testsuite.TestWorkflowEnvironment){"version 2": recordsVersion2, "unpinned": nil} {
		attempts, err, _ := runWithBuildFailures(t, 1, InfrastructureFailureType, pin)
		if err == nil {
			t.Errorf("%s: workflow succeeded after a lost build, want a halt", name)
		}
		if attempts != 1 {
			t.Errorf("%s: build attempts = %d, want 1 (no automatic rerun)", name, attempts)
		}
	}
}

// TestHistoryRecordedAtVersion1ReplaysWithItsRetry: a workflow started before
// new executions stopped retrying recorded version 1 and replays with the
// retry its history holds.
func TestHistoryRecordedAtVersion1ReplaysWithItsRetry(t *testing.T) {
	attempts, err, result := runWithBuildFailures(t, 1, InfrastructureFailureType, pinActivityRetriesVersion1)
	if err != nil || attempts != 2 || result.State != run.StateAccepted {
		t.Errorf("version 1 replay: attempts=%d err=%v state=%v, want 2 attempts and accepted", attempts, err, result.State)
	}
}

func TestRetriedActivityOptionsListsEveryNonInfrastructureType(t *testing.T) {
	opts := retriedActivityOptions(temporalworkflow.ActivityOptions{})
	if opts.RetryPolicy.MaximumAttempts != 2 {
		t.Errorf("MaximumAttempts = %d, want 2", opts.RetryPolicy.MaximumAttempts)
	}
	for _, typ := range opts.RetryPolicy.NonRetryableErrorTypes {
		if typ == InfrastructureFailureType {
			t.Error("InfrastructureFailure is listed non-retryable")
		}
	}
	if len(opts.RetryPolicy.NonRetryableErrorTypes) != 12 {
		t.Errorf("non-retryable types = %v, want the 12 listed failure types", opts.RetryPolicy.NonRetryableErrorTypes)
	}
}

// TestRepositoryOwnerStopLineCountsARetriedRunOnce runs under activity-retries
// version 1 (pinned): a build failing with InfrastructureFailure on every
// attempt is retried once, and the run still counts as ONE failed run toward
// the stop line.
func TestRepositoryOwnerStopLineCountsARetriedRunOnce(t *testing.T) {
	var buildCalls atomic.Int32
	env := newWorkflowEnvironment(t,
		func(context.Context, RunWorkflowInput) (BuildActivityResult, error) {
			buildCalls.Add(1)
			return BuildActivityResult{Attempts: []run.Attempt{{Kind: "build", ExitCode: -1}}}, temporal.NewApplicationError("worker unavailable", InfrastructureFailureType)
		},
		func(context.Context, RunWorkflowInput) (VerifyActivityResult, error) {
			t.Fatal("verify must not run after an infrastructure failure")
			return VerifyActivityResult{}, nil
		},
		func(context.Context, EvaluateGateInput) (run.GateResult, error) {
			return run.GateResult{Check: "canonical_verify", Passed: true}, nil
		},
	)
	pinActivityRetriesVersion1(env)
	repository := "fixture/stop-line-retried"
	env.SetStartWorkflowOptions(client.StartWorkflowOptions{ID: RepositoryOwnerWorkflowID(repository)})
	env.RegisterWorkflow(RunWorkflow)
	env.RegisterDelayedCallback(func() {
		for i := 1; i <= 4; i++ {
			id := fmt.Sprintf("run-%d", i)
			env.SignalWorkflow(SubmitRunSignalName, SubmitRunSignal{RequestID: id, Input: fixtureInputForTicket(id)})
		}
	}, 0)
	var tripped RepositoryOwnerResult
	env.RegisterDelayedCallback(func() {
		queryResult, err := env.QueryWorkflow(RepositoryOwnerQueryName)
		if err != nil {
			t.Fatalf("query repository owner: %v", err)
		}
		if err := queryResult.Get(&tripped); err != nil {
			t.Fatalf("decode query result: %v", err)
		}
		env.SignalWorkflow(ResetStopLineSignalName, ResetStopLineSignal{By: "test", Reason: "cleanup", Generation: tripped.StopLineGeneration})
	}, 10*time.Second)
	env.ExecuteWorkflow(RepositoryOwnerWorkflow, RepositoryOwnerWorkflowInput{
		Repository:               repository,
		IdleTimeout:              time.Second,
		StopLineFailureThreshold: 2,
	})
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error: %v", err)
	}
	var result RepositoryOwnerResult
	if err := env.GetWorkflowResult(&result); err != nil {
		t.Fatalf("workflow result: %v", err)
	}
	if got := buildCalls.Load(); got != 4 {
		t.Errorf("build calls = %d, want 4 (two runs, each tried twice, then the stop line trips)", got)
	}
	if !tripped.StopLineTripped || tripped.SystemicFailureStreak != 2 {
		t.Errorf("stop-line state = tripped=%v streak=%d, want tripped=true streak=2 (one per failed run)", tripped.StopLineTripped, tripped.SystemicFailureStreak)
	}
	for _, id := range []string{"run-3", "run-4"} {
		if got := result.Runs[id]; got.State != run.StateHalted {
			t.Errorf("%s result = %+v, want halted by the stop line", id, got)
		}
	}
}

// TestPostBuildAndCollectEvidenceAreNotRetried: their safety-net commit
// intent is how a lost attempt's commit is detected, so an infrastructure
// failure there halts after a single attempt.
func TestPostBuildAndCollectEvidenceAreNotRetried(t *testing.T) {
	for _, name := range []string{PostBuildActivityName, CollectEvidenceActivityName} {
		var calls atomic.Int32
		env := newWorkflowEnvironment(t,
			func(context.Context, RunWorkflowInput) (BuildActivityResult, error) {
				return BuildActivityResult{Result: runner.Result{ExitCode: 0}}, nil
			},
			func(context.Context, RunWorkflowInput) (VerifyActivityResult, error) {
				return VerifyActivityResult{Result: runner.Result{ExitCode: 0}}, nil
			},
			func(context.Context, EvaluateGateInput) (run.GateResult, error) {
				return run.GateResult{Check: "canonical_verify", Passed: true}, nil
			},
		)
		fail := func() error {
			calls.Add(1)
			return temporal.NewApplicationError("worker stopped", InfrastructureFailureType)
		}
		if name == PostBuildActivityName {
			env.RegisterActivityWithOptions(func(context.Context, PostBuildInput) (PostBuildResult, error) { return PostBuildResult{}, fail() }, activity.RegisterOptions{Name: name})
		} else {
			env.RegisterActivityWithOptions(func(context.Context, CollectEvidenceInput) (CollectedEvidence, error) {
				return CollectedEvidence{}, fail()
			}, activity.RegisterOptions{Name: name})
		}
		env.ExecuteWorkflow(RunWorkflow, fixtureInput())
		if env.GetWorkflowError() == nil {
			t.Errorf("%s: workflow succeeded, want a halt", name)
		}
		if got := calls.Load(); got != 1 {
			t.Errorf("%s: attempts = %d, want 1", name, got)
		}
	}
}
