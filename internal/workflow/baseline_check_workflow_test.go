package workflow

import (
	"context"
	"strings"
	"sync"
	"testing"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/converter"

	"buildgate/internal/run"
	"buildgate/internal/runner"
)

// baselineCheckRun runs RunWorkflow with BaselineCheck on and a full-suite
// command, the full-suite Activity answering with fullSuiteExit, and returns
// the workflow error, its result and the names of the Activities started.
func baselineCheckRun(t *testing.T, fullSuiteCommand string, fullSuiteExit int) (error, RunWorkflowResult, []string) {
	t.Helper()
	fail := func(name string) func(context.Context, RunWorkflowInput) (BuildActivityResult, error) {
		return func(context.Context, RunWorkflowInput) (BuildActivityResult, error) {
			t.Errorf("%s ran in a baseline check", name)
			return BuildActivityResult{}, nil
		}
	}
	env := newWorkflowEnvironmentWithBaseline(t, fail("the build"),
		func(context.Context, RunWorkflowInput) (VerifyActivityResult, error) {
			t.Error("canonical verify ran in a baseline check")
			return VerifyActivityResult{}, nil
		},
		func(context.Context, EvaluateGateInput) (run.GateResult, error) {
			return run.GateResult{Passed: true}, nil
		},
		passingBaselineVerify,
	)
	registerIsolationActivities(env)
	env.RegisterActivityWithOptions(func(context.Context, RunWorkflowInput) (VerifyActivityResult, error) {
		return VerifyActivityResult{Result: runner.Result{ExitCode: fullSuiteExit}, Attempts: []run.Attempt{{Kind: "full_suite_verify", ExitCode: fullSuiteExit}}}, nil
	}, activity.RegisterOptions{Name: RunFullSuiteVerifyActivityName})
	var mu sync.Mutex
	var started []string
	env.SetOnActivityStartedListener(func(info *activity.Info, _ context.Context, _ converter.EncodedValues) {
		mu.Lock()
		defer mu.Unlock()
		started = append(started, info.ActivityType.Name)
	})
	input := fixtureInput()
	input.BaselineCheck, input.FullSuiteCommand = true, fullSuiteCommand
	env.ExecuteWorkflow(RunWorkflow, input)
	var result RunWorkflowResult
	err := env.GetWorkflowError()
	if err == nil {
		if getErr := env.GetWorkflowResult(&result); getErr != nil {
			t.Fatalf("workflow result: %v", getErr)
		}
	}
	return err, result, started
}

func activityStarted(started []string, name string) bool {
	for _, s := range started {
		if s == name {
			return true
		}
	}
	return false
}

// A baseline check stops after the baseline: no build, no verdict, no
// State, and the isolated worktree is rolled back.
func TestABaselineCheckStopsAfterTheBaseline(t *testing.T) {
	err, result, started := baselineCheckRun(t, "", 0)
	if err != nil || !result.BaselineChecked || result.State != "" {
		t.Fatalf("err=%v checked=%v state=%q, want a passed check with no state", err, result.BaselineChecked, result.State)
	}
	if !activityStarted(started, RunBaselineVerifyActivityName) || activityStarted(started, RunFullSuiteVerifyActivityName) {
		t.Errorf("activities = %v, want the baseline and no full suite (none configured)", started)
	}
	if !activityStarted(started, RollbackIsolatedWorkspaceActivityName) {
		t.Errorf("activities = %v, want the worktree rolled back", started)
	}
}

// A full-suite command that differs from the verify command runs too; a red
// one halts the check with BaselineVerifyFailureType and its own sentence.
func TestABaselineCheckRunsADifferentFullSuiteCommand(t *testing.T) {
	err, result, started := baselineCheckRun(t, "make full", 0)
	if err != nil || !result.BaselineChecked || !activityStarted(started, RunFullSuiteVerifyActivityName) {
		t.Fatalf("err=%v checked=%v activities=%v, want the full suite run and passed", err, result.BaselineChecked, started)
	}
	err, result, _ = baselineCheckRun(t, "make full", 4)
	if err == nil || result.BaselineChecked || !strings.Contains(err.Error(), "full-suite command exits 4") || !strings.Contains(err.Error(), BaselineVerifyFailureType) {
		t.Fatalf("err=%v checked=%v, want a BaselineVerifyFailure naming the full-suite exit", err, result.BaselineChecked)
	}
}

// The full-suite command equal to the verify command is not run twice.
func TestABaselineCheckSkipsAFullSuiteEqualToTheVerifyCommand(t *testing.T) {
	_, result, started := baselineCheckRun(t, fixtureInput().VerifyCommand, 0)
	if !result.BaselineChecked || activityStarted(started, RunFullSuiteVerifyActivityName) {
		t.Fatalf("checked=%v activities=%v, want no second run of the same command", result.BaselineChecked, started)
	}
}
