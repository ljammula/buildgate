package workflow

import (
	"buildgate/internal/reviewstep"
	"buildgate/internal/run"
	"buildgate/internal/runner"
	"context"
	"slices"
	"sync"
	"testing"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/testsuite"
)

// registerCodeReviewWorkflowEnvironment registers the review phase's own
// opt-in Activity.
func registerCodeReviewWorkflowEnvironment(
	t *testing.T,
	build func(context.Context, RunWorkflowInput) (BuildActivityResult, error),
	verify func(context.Context, RunWorkflowInput) (VerifyActivityResult, error),
	codeReview func(context.Context, ReviewStepInput) (VerifyActivityResult, error),
) *testsuite.TestWorkflowEnvironment {
	t.Helper()
	env := newWorkflowEnvironment(t, build, verify, func(context.Context, EvaluateGateInput) (run.GateResult, error) {
		return run.GateResult{Check: "canonical_verify", Passed: true}, nil
	})
	env.RegisterActivityWithOptions(codeReview, activity.RegisterOptions{Name: RunReviewStepActivityName})
	return env
}

// TestRunWorkflowSkipsCodeReviewWhenNotDeclared: the code-review gate is
// opt-in -- an empty (or "off") CodeReviewPolicy means RunWorkflow never
// calls RunCodeReviewActivityName at all.
func TestRunWorkflowSkipsCodeReviewWhenNotDeclared(t *testing.T) {
	calls := 0
	env := registerCodeReviewWorkflowEnvironment(t,
		func(context.Context, RunWorkflowInput) (BuildActivityResult, error) {
			return BuildActivityResult{Result: runner.Result{ExitCode: 0}}, nil
		},
		func(context.Context, RunWorkflowInput) (VerifyActivityResult, error) {
			return VerifyActivityResult{Result: runner.Result{Command: []string{"sh", "-c", "make verify"}, ExitCode: 0}}, nil
		},
		func(context.Context, ReviewStepInput) (VerifyActivityResult, error) {
			calls++
			return VerifyActivityResult{}, nil
		},
	)

	env.ExecuteWorkflow(RunWorkflow, fixtureInput())
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error: %v", err)
	}
	var result RunWorkflowResult
	if err := env.GetWorkflowResult(&result); err != nil {
		t.Fatalf("workflow result: %v", err)
	}
	if result.State != run.StateAccepted {
		t.Fatalf("state = %q, want %q", result.State, run.StateAccepted)
	}
	if calls != 0 {
		t.Fatalf("code-review Activity calls = %d, want 0 when CodeReviewPolicy is not declared", calls)
	}
	for _, g := range result.GateResults {
		if g.Check == "code_review" {
			t.Fatalf("GateResults = %+v, want no code_review gate", result.GateResults)
		}
	}
	if result.CodeReviewConfigured {
		t.Fatal("CodeReviewConfigured = true, want false when the run never declared CodeReviewPolicy")
	}
}

// TestRunWorkflowRunsCodeReviewAfterCollectingEvidence pins the
// code-review phase's own ordering: it must run AFTER CollectEvidenceActivity's own safety-net commit, for the same
// already-committed-workspace reason.
func TestRunWorkflowRunsCodeReviewAfterCollectingEvidence(t *testing.T) {
	input := fixtureInput()
	input.CodeReviewPolicy = "advisory"
	var order []string
	var mu sync.Mutex
	record := func(name string) {
		mu.Lock()
		defer mu.Unlock()
		order = append(order, name)
	}

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
	env.RegisterActivityWithOptions(
		func(context.Context, CollectEvidenceInput) (CollectedEvidence, error) {
			record(CollectEvidenceActivityName)
			return CollectedEvidence{}, nil
		},
		activity.RegisterOptions{Name: CollectEvidenceActivityName},
	)
	env.RegisterActivityWithOptions(
		func(context.Context, ReviewStepInput) (VerifyActivityResult, error) {
			record(RunReviewStepActivityName)
			return VerifyActivityResult{Result: runner.Result{Command: []string{"python3", "code_review.py"}, ExitCode: 0}}, nil
		},
		activity.RegisterOptions{Name: RunReviewStepActivityName},
	)

	env.ExecuteWorkflow(RunWorkflow, input)
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error: %v", err)
	}
	if !slices.Equal(order, []string{CollectEvidenceActivityName, RunReviewStepActivityName}) {
		t.Fatalf("Activity call order = %v, want [%s, %s]", order, CollectEvidenceActivityName, RunReviewStepActivityName)
	}
}

// TestRunWorkflowRunsOneReviewLaunchAndDecodesItsExitStatus proves
// reviewstep.Plan's one step: a declared -code-review-policy runs ONE
// RunReviewStepActivity call (Step == reviewstep.Combined), whatever the
// ticket's acceptance criteria, and the code_review gate is derived from
// that launch's exit status via reviewstep.ReviewExitCode: a verdict
// (CombinedExitBase) passes, no verdict (CombinedExitBase+2) and every other
// status fail. No spec_conformity gate exists. Also pins the 2-Activity order
// (CollectEvidence, then the one review step).
func TestRunWorkflowRunsOneReviewLaunchAndDecodesItsExitStatus(t *testing.T) {
	for _, tc := range []struct {
		name       string
		exit       int
		wantPassed bool
	}{
		{"a verdict", reviewstep.CombinedExitBase, true},
		{"no verdict", reviewstep.CombinedExitBase + 2, false},
		{"a crash", 1, false},
		{"a stale bit 0", reviewstep.CombinedExitBase + 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := fixtureInput()
			input.SpecAcceptanceCriteria = "/fixture/criteria.md"
			input.CodeReviewPolicy = "required"
			var order []string
			var mu sync.Mutex
			record := func(name string) {
				mu.Lock()
				defer mu.Unlock()
				order = append(order, name)
			}

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
			env.RegisterActivityWithOptions(
				func(context.Context, CollectEvidenceInput) (CollectedEvidence, error) {
					record(CollectEvidenceActivityName)
					return CollectedEvidence{}, nil
				},
				activity.RegisterOptions{Name: CollectEvidenceActivityName},
			)
			reviewCalls := 0
			env.RegisterActivityWithOptions(
				func(_ context.Context, in ReviewStepInput) (VerifyActivityResult, error) {
					if in.Step != reviewstep.Combined {
						t.Fatalf("ReviewStepInput.Step = %q, want %q", in.Step, reviewstep.Combined)
					}
					reviewCalls++
					record(reviewstep.Combined)
					return VerifyActivityResult{Result: runner.Result{Command: []string{"python3", "combined_review.py"}, ExitCode: tc.exit}}, nil
				},
				activity.RegisterOptions{Name: RunReviewStepActivityName},
			)

			env.ExecuteWorkflow(RunWorkflow, input)
			if err := env.GetWorkflowError(); err != nil {
				t.Fatalf("workflow error: %v", err)
			}
			if reviewCalls != 1 {
				t.Fatalf("review Activity calls = %d, want 1", reviewCalls)
			}
			if !slices.Equal(order, []string{CollectEvidenceActivityName, reviewstep.Combined}) {
				t.Fatalf("Activity call order = %v, want [%s, %s]", order, CollectEvidenceActivityName, reviewstep.Combined)
			}
			var result RunWorkflowResult
			if err := env.GetWorkflowResult(&result); err != nil {
				t.Fatalf("workflow result: %v", err)
			}
			for _, g := range result.GateResults {
				if g.Check == "spec_conformity" {
					t.Fatalf("GateResults = %+v, want no spec_conformity gate", result.GateResults)
				}
			}
			codeReviewGate := gateByCheck(t, result.GateResults, "code_review")
			if codeReviewGate.Passed != tc.wantPassed {
				t.Fatalf("code_review gate = %+v, want passed=%v", codeReviewGate, tc.wantPassed)
			}
			wantState := run.StateQuarantined
			if tc.wantPassed {
				wantState = run.StateAccepted
			}
			if result.State != wantState {
				t.Fatalf("state = %q, want %q", result.State, wantState)
			}
		})
	}
}

func gateByCheck(t *testing.T, gates []run.GateResult, check string) run.GateResult {
	t.Helper()
	for _, g := range gates {
		if g.Check == check {
			return g
		}
	}
	t.Fatalf("no %s gate in %+v", check, gates)
	return run.GateResult{}
}
