package workflow

import (
	"context"
	"sync"
	"testing"

	"go.temporal.io/sdk/activity"
	temporalworkflow "go.temporal.io/sdk/workflow"

	"buildgate/internal/reproduction"
	"buildgate/internal/reviewstep"
	"buildgate/internal/run"
	"buildgate/internal/runner"
)

// heldFixture is one held reproduction of the review's first finding.
func heldFixture() run.HeldReproduction {
	return run.HeldReproduction{Finding: 0, Path: "p/repro_test.go", SHA256: "aa", HostPath: "/fixture/data/runs/r/review-repro/0/p/repro_test.go", SpecLine: 3, TestIDs: []string{"TestRepro"}, Runner: "go-test-json"}
}

// reproRun is what one fake launch reports for the reproduction's one ID.
func reproRun(outcome reproduction.Outcome, kind reproduction.Kind) ReproductionRunResult {
	return ReproductionRunResult{Run: run.ReproductionRun{Outcome: string(outcome), Tests: map[string]string{"TestRepro": string(kind)}}}
}

// reproductionPhase is one RunWorkflow execution with every earlier gate
// passing, a review that held held, and a fake reproduction Activity.
type reproductionPhase struct {
	result RunWorkflowResult
	err    error
	calls  []string
	passes []ReproductionRunInput
}

func runReproductionPhase(t *testing.T, input RunWorkflowInput, held []run.HeldReproduction, launch func(ReproductionRunInput) (ReproductionRunResult, error), legacy bool, retired ...bool) reproductionPhase {
	retiredPhase := len(retired) > 0 && retired[0]
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
	if legacy {
		env.OnGetVersion(reviewReproductionChange, temporalworkflow.DefaultVersion, 1).Return(temporalworkflow.DefaultVersion)
	}
	if retiredPhase {
		env.OnGetVersion(retireReviewReproductionChange, temporalworkflow.DefaultVersion, 1).Return(temporalworkflow.Version(1))
	} else {
		env.OnGetVersion(retireReviewReproductionChange, temporalworkflow.DefaultVersion, 1).Return(temporalworkflow.DefaultVersion)
	}
	env.RegisterActivityWithOptions(func(context.Context, ReviewStepInput) (VerifyActivityResult, error) {
		return VerifyActivityResult{Result: runner.Result{Command: []string{"python3", "combined_review.py"}, ExitCode: reviewstep.CombinedExitBase}, Reproductions: held}, nil
	}, activity.RegisterOptions{Name: RunReviewStepActivityName})
	var p reproductionPhase
	var mu sync.Mutex
	env.RegisterActivityWithOptions(func(ctx context.Context, in ReproductionRunInput) (ReproductionRunResult, error) {
		mu.Lock()
		p.calls = append(p.calls, activity.GetInfo(ctx).ActivityID)
		p.passes = append(p.passes, in)
		mu.Unlock()
		return launch(in)
	}, activity.RegisterOptions{Name: RunHeldTestActivityName})
	env.ExecuteWorkflow(RunWorkflow, input)
	p.err = env.GetWorkflowError()
	if p.err == nil {
		if err := env.GetWorkflowResult(&p.result); err != nil {
			t.Fatalf("workflow result: %v", err)
		}
	}
	return p
}

func reproductionInput() RunWorkflowInput {
	input := fixtureInput()
	input.CodeReviewPolicy = "advisory"
	input.ReviewReproduction = true
	return input
}

// byPass answers each pass with its own result.
func byPass(results map[string]ReproductionRunResult) func(ReproductionRunInput) (ReproductionRunResult, error) {
	return func(in ReproductionRunInput) (ReproductionRunResult, error) {
		res := results[in.Pass]
		res.Run.Pass, res.Run.Commit = in.Pass, in.Commit
		return res, nil
	}
}

// A history recorded before retireReviewReproductionChange (DefaultVersion of
// that change) replays the retired phase: a finding's reproduction is run
// head1, then head2 and the base only while it fails.
func TestHistoryRecordedBeforeTheRetirementReplaysTheReproductionPhase(t *testing.T) {
	fail := reproRun(reproduction.OutcomeFailed, reproduction.KindFail)
	p := runReproductionPhase(t, reproductionInput(), []run.HeldReproduction{heldFixture()}, byPass(map[string]ReproductionRunResult{"head1": fail, "head2": fail, "base": fail}), false)
	if p.err != nil {
		t.Fatalf("workflow error: %v", p.err)
	}
	if want := []string{"repro-0-head1", "repro-0-head2", "repro-0-base"}; len(p.calls) != len(want) || p.calls[0] != want[0] || p.calls[1] != want[1] || p.calls[2] != want[2] {
		t.Errorf("launches = %v, want %v", p.calls, want)
	}
	if len(p.result.ReviewReproductions) != 1 {
		t.Errorf("ReviewReproductions = %+v, want the one decided finding", p.result.ReviewReproductions)
	}
}

// From retireReviewReproductionChange on, a review writes no test the
// factory runs: no reproduction and no recheck is scheduled, whatever the
// input holds, and no review_reproduction check is recorded.
func TestRetiredReviewReproductionSchedulesNoLaunch(t *testing.T) {
	input := reproductionInput()
	input.RecheckReproductions = []run.RecheckEntry{{RunID: "run-q", HeldReproduction: heldFixture()}}
	fail := reproRun(reproduction.OutcomeFailed, reproduction.KindFail)
	p := runReproductionPhase(t, input, []run.HeldReproduction{heldFixture()}, byPass(map[string]ReproductionRunResult{"head1": fail, "head2": fail, "base": fail, "recheck": fail}), false, true)
	if p.err != nil {
		t.Fatalf("workflow error: %v", p.err)
	}
	if len(p.calls) != 0 || p.result.State != run.StateAccepted {
		t.Errorf("launches = %v, state %s; want none and an accepted run", p.calls, p.result.State)
	}
	for _, g := range p.result.GateResults {
		if g.Check == "review_reproduction" {
			t.Errorf("gates = %+v, want no review_reproduction check", p.result.GateResults)
		}
	}
}

// A history recorded before reviewReproductionChange (DefaultVersion) runs no
// reproduction and no recheck, even with the input set.
func TestHistoryRecordedBeforeReviewReproductionSchedulesNoReproduction(t *testing.T) {
	input := reproductionInput()
	input.RecheckReproductions = []run.RecheckEntry{{RunID: "run-q", HeldReproduction: heldFixture()}}
	fail := reproRun(reproduction.OutcomeFailed, reproduction.KindFail)
	p := runReproductionPhase(t, input, []run.HeldReproduction{heldFixture()}, byPass(map[string]ReproductionRunResult{"head1": fail, "head2": fail, "base": fail, "recheck": fail}), true)
	if p.err != nil {
		t.Fatalf("workflow error: %v", p.err)
	}
	if len(p.calls) != 0 {
		t.Errorf("launches = %v, want none at DefaultVersion", p.calls)
	}
}
