package workflow

import (
	"context"
	"slices"
	"sync"
	"testing"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/testsuite"
	temporalworkflow "go.temporal.io/sdk/workflow"

	"buildgate/internal/reviewstep"
	"buildgate/internal/run"
	"buildgate/internal/runner"
)

func sizeWorkflowFixture(t *testing.T) *testsuite.TestWorkflowEnvironment {
	t.Helper()
	return newWorkflowEnvironment(t,
		func(context.Context, RunWorkflowInput) (BuildActivityResult, error) {
			return BuildActivityResult{Result: runner.Result{ExitCode: 0}}, nil
		},
		func(context.Context, RunWorkflowInput) (VerifyActivityResult, error) {
			return VerifyActivityResult{Result: runner.Result{Command: []string{"make", "verify"}, ExitCode: 0}}, nil
		},
		func(context.Context, EvaluateGateInput) (run.GateResult, error) {
			return run.GateResult{Passed: true}, nil
		},
	)
}

func TestRunWorkflowMeasuresAfterGatesBeforeReview(t *testing.T) {
	env := sizeWorkflowFixture(t)
	var mu sync.Mutex
	var order []string
	mark := func(s string) {
		mu.Lock()
		defer mu.Unlock()
		order = append(order, s)
	}
	env.RegisterActivityWithOptions(func(context.Context, CollectEvidenceInput) (CollectedEvidence, error) {
		mark("evidence")
		return CollectedEvidence{}, nil
	}, activity.RegisterOptions{Name: CollectEvidenceActivityName})
	env.RegisterActivityWithOptions(func(context.Context, MeasureChangeSizeInput) (*run.ChangeSize, error) {
		mark("size")
		return &run.ChangeSize{SourceLines: 3, SourceFiles: 1, Functions: 1}, nil
	}, activity.RegisterOptions{Name: MeasureChangeSizeActivityName})
	env.RegisterActivityWithOptions(func(context.Context, ReviewStepInput) (VerifyActivityResult, error) {
		mark("review")
		return VerifyActivityResult{Result: runner.Result{Command: []string{"review"}, ExitCode: reviewstep.CombinedExitBase}}, nil
	}, activity.RegisterOptions{Name: RunReviewStepActivityName})
	input := fixtureInput()
	input.CodeReviewPolicy, input.ReleaseMaxChangedLines, input.ReleaseMaxFilesChanged = "required", 1000, 25
	env.ExecuteWorkflow(RunWorkflow, input)
	if err := env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(order, []string{"evidence", "size", "review"}) {
		t.Fatalf("activity order = %v, want evidence, size, review", order)
	}
}

func TestRunWorkflowDoesNotMeasureWhenDiffScopeFails(t *testing.T) {
	env := sizeWorkflowFixture(t)
	measured := 0
	env.RegisterActivityWithOptions(func(context.Context, CollectEvidenceInput) (CollectedEvidence, error) {
		return CollectedEvidence{ChangedFiles: []string{"blocked.go"}}, nil
	}, activity.RegisterOptions{Name: CollectEvidenceActivityName})
	env.RegisterActivityWithOptions(func(context.Context, MeasureChangeSizeInput) (*run.ChangeSize, error) {
		measured++
		return &run.ChangeSize{}, nil
	}, activity.RegisterOptions{Name: MeasureChangeSizeActivityName})
	input := fixtureInput()
	input.AllowedFiles = []string{"allowed.go"}
	env.ExecuteWorkflow(RunWorkflow, input)
	if err := env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
	var result RunWorkflowResult
	if err := env.GetWorkflowResult(&result); err != nil {
		t.Fatal(err)
	}
	if measured != 0 || result.State != run.StateQuarantined {
		t.Fatalf("measure calls = %d, state = %s; want no measure and a quarantine", measured, result.State)
	}
}

func TestRunWorkflowMeasuresOverCapAndSkipsCodeReview(t *testing.T) {
	for name, size := range map[string]run.ChangeSize{
		"source lines": {SourceLines: 1001, SourceFiles: 2, Functions: 7},
		"source files": {SourceLines: 20, SourceFiles: 26, Functions: 7},
	} {
		t.Run(name, func(t *testing.T) {
			env := sizeWorkflowFixture(t)
			measured, reviewed := 0, 0
			env.RegisterActivityWithOptions(func(context.Context, MeasureChangeSizeInput) (*run.ChangeSize, error) {
				measured++
				return &size, nil
			}, activity.RegisterOptions{Name: MeasureChangeSizeActivityName})
			env.RegisterActivityWithOptions(func(context.Context, ReviewStepInput) (VerifyActivityResult, error) {
				reviewed++
				return VerifyActivityResult{}, nil
			}, activity.RegisterOptions{Name: RunReviewStepActivityName})
			input := fixtureInput()
			input.CodeReviewPolicy = "required"
			input.ReleaseMaxChangedLines = 1000
			input.ReleaseMaxFilesChanged = 25
			env.ExecuteWorkflow(RunWorkflow, input)
			if err := env.GetWorkflowError(); err != nil {
				t.Fatal(err)
			}
			var result RunWorkflowResult
			if err := env.GetWorkflowResult(&result); err != nil {
				t.Fatal(err)
			}
			if measured != 1 || reviewed != 0 || result.State != run.StateAccepted || result.ChangeSize == nil || result.ChangeSize.SourceLines != size.SourceLines || result.ChangeSize.SourceFiles != size.SourceFiles {
				t.Fatalf("measured %d, reviewed %d, result %+v; want measured, accepted and unreviewed", measured, reviewed, result)
			}
		})
	}
}

func TestHistoryRecordedBeforeChangeSizeReplaysWithoutMeasurement(t *testing.T) {
	env := sizeWorkflowFixture(t)
	measured := 0
	env.RegisterActivityWithOptions(func(context.Context, MeasureChangeSizeInput) (*run.ChangeSize, error) {
		measured++
		return &run.ChangeSize{}, nil
	}, activity.RegisterOptions{Name: MeasureChangeSizeActivityName})
	env.OnGetVersion(changeSizeMeasurementChange, temporalworkflow.DefaultVersion, 1).Return(temporalworkflow.DefaultVersion)
	env.ExecuteWorkflow(RunWorkflow, fixtureInput())
	if err := env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
	var result RunWorkflowResult
	if err := env.GetWorkflowResult(&result); err != nil {
		t.Fatal(err)
	}
	if measured != 0 || result.ChangeSize != nil || result.State != run.StateAccepted {
		t.Fatalf("measured %d, result %+v; want old history accepted without measurement", measured, result)
	}
}
