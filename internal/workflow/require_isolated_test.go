package workflow

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/converter"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"
	temporalworkflow "go.temporal.io/sdk/workflow"

	"buildgate/internal/run"
	"buildgate/internal/runner"
)

// runIsolationGuardFixture runs RunWorkflow on input and returns the workflow
// error, the result, and how many Activities started.
func runIsolationGuardFixture(t *testing.T, input RunWorkflowInput, pin func(*testsuite.TestWorkflowEnvironment)) (error, RunWorkflowResult, int32) {
	t.Helper()
	env := newWorkflowEnvironment(t,
		func(context.Context, RunWorkflowInput) (BuildActivityResult, error) {
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
	var started atomic.Int32
	env.SetOnActivityStartedListener(func(*activity.Info, context.Context, converter.EncodedValues) { started.Add(1) })
	env.ExecuteWorkflow(RunWorkflow, input)
	var result RunWorkflowResult
	err := env.GetWorkflowError()
	if err == nil {
		if getErr := env.GetWorkflowResult(&result); getErr != nil {
			t.Fatalf("workflow result: %v", getErr)
		}
	}
	return err, result, started.Load()
}

func TestRunWorkflowRejectsANonIsolatedRunOnANewExecution(t *testing.T) {
	input := fixtureInput()
	input.IsolateWorkspace = false
	err, _, started := runIsolationGuardFixture(t, input, nil)
	if err == nil {
		t.Fatal("workflow succeeded for IsolateWorkspace false, want a refusal")
	}
	var appErr *temporal.ApplicationError
	if !errors.As(err, &appErr) {
		t.Fatalf("error %v carries no application error", err)
	}
	if !appErr.NonRetryable() || appErr.Type() != NonIsolatedRunFailureType {
		t.Errorf("application error = type %q non-retryable %v, want type %q non-retryable", appErr.Type(), appErr.NonRetryable(), NonIsolatedRunFailureType)
	}
	if started != 0 {
		t.Errorf("%d Activities started before the refusal, want 0", started)
	}
}

func TestRunWorkflowRunsAnIsolatedRunOnANewExecution(t *testing.T) {
	err, result, _ := runIsolationGuardFixture(t, fixtureInput(), nil)
	if err != nil || result.State != run.StateAccepted {
		t.Errorf("isolated run: err=%v state=%v, want accepted", err, result.State)
	}
}

// TestHistoryRecordedBeforeTheIsolationGuardReplaysUnchanged pins the change
// to DefaultVersion, what a history recorded before require-isolated-workspace
// replays as: a non-isolated run keeps the branches it recorded.
func TestHistoryRecordedBeforeTheIsolationGuardReplaysUnchanged(t *testing.T) {
	input := fixtureInput()
	input.IsolateWorkspace = false
	pin := func(env *testsuite.TestWorkflowEnvironment) {
		env.OnGetVersion(requireIsolatedWorkspaceChange, temporalworkflow.DefaultVersion, 1).Return(temporalworkflow.DefaultVersion)
	}
	err, result, _ := runIsolationGuardFixture(t, input, pin)
	if err != nil || result.State != run.StateAccepted {
		t.Errorf("replay of a pre-guard history: err=%v state=%v, want accepted", err, result.State)
	}
}
