package workflow

import (
	"context"
	"testing"

	"go.temporal.io/sdk/activity"
	temporalworkflow "go.temporal.io/sdk/workflow"

	"buildgate/internal/reviewstep"
	"buildgate/internal/run"
	"buildgate/internal/runner"
)

// reviewReplayCall is what one RunReviewStepActivity call was given.
type reviewReplayCall struct{ activityID, step string }

// runReviewReplayFixture runs RunWorkflow on input with every earlier gate
// passing and the review launch exiting exit. legacy pins
// reviewReproductionChange to DefaultVersion, what a history recorded before
// it replays as. It returns the result and the review calls made.
func runReviewReplayFixture(t *testing.T, input RunWorkflowInput, exit int, legacy bool) (RunWorkflowResult, []reviewReplayCall) {
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
	var calls []reviewReplayCall
	env.RegisterActivityWithOptions(func(ctx context.Context, in ReviewStepInput) (VerifyActivityResult, error) {
		calls = append(calls, reviewReplayCall{activityID: activity.GetInfo(ctx).ActivityID, step: in.Step})
		return VerifyActivityResult{Result: runner.Result{Command: []string{"python3", "review.py"}, ExitCode: exit}}, nil
	}, activity.RegisterOptions{Name: RunReviewStepActivityName})
	env.ExecuteWorkflow(RunWorkflow, input)
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error: %v", err)
	}
	var result RunWorkflowResult
	if err := env.GetWorkflowResult(&result); err != nil {
		t.Fatalf("workflow result: %v", err)
	}
	return result, calls
}

// gateOutcomes is each gate's check and whether it passed, in order.
func gateOutcomes(gates []run.GateResult) map[string]bool {
	out := map[string]bool{}
	for _, g := range gates {
		out[g.Check] = g.Passed
	}
	return out
}

// TestHistoryRecordedBeforeReviewReproductionReplaysUnchanged pins
// reviewReproductionChange to DefaultVersion, what a history recorded before
// it replays as: the retired plan schedules the same review Activity, under
// the same Activity ID and step name, as the history recorded, and the
// retired decode turns its exit status into the same gates -- the
// spec_conformity gate included -- so the commands after the review (the
// oracle commit, the policy verdict) are the ones the history holds. Each
// case also runs at version 1, which takes the one-step plan and decode, so
// the legacy path is shown to be the one exercised.
func TestHistoryRecordedBeforeReviewReproductionReplaysUnchanged(t *testing.T) {
	for _, tc := range []struct {
		name         string
		criteria     bool
		policy       string
		exit         int
		wantStep     string
		wantGates    map[string]bool
		wantAccepted bool
		// The same input at version 1.
		wantV1Calls    int
		wantV1Gates    map[string]bool
		wantV1Accepted bool
	}{
		{
			name: "both reviews on: one combined launch, its two exit bits split", criteria: true, policy: "required",
			exit: reviewstep.CombinedExitBase + 1, wantStep: reviewstep.Combined,
			wantGates:   map[string]bool{"spec_conformity": false, "code_review": true},
			wantV1Calls: 1, wantV1Gates: map[string]bool{"code_review": false},
		},
		{
			name: "both reviews on and both passed", criteria: true, policy: "required",
			exit: reviewstep.CombinedExitBase, wantStep: reviewstep.Combined,
			wantGates: map[string]bool{"spec_conformity": true, "code_review": true}, wantAccepted: true,
			wantV1Calls: 1, wantV1Gates: map[string]bool{"code_review": true}, wantV1Accepted: true,
		},
		{
			name: "conformity alone: the standalone spec-conformity step", criteria: true, policy: "",
			exit: 1, wantStep: reviewstep.SpecConformity,
			wantGates:   map[string]bool{"spec_conformity": false},
			wantV1Calls: 0, wantV1Gates: map[string]bool{}, wantV1Accepted: true,
		},
		{
			name: "code review alone: the standalone code-review step, exit 0 a pass", criteria: false, policy: "advisory",
			exit: 0, wantStep: reviewstep.CodeReview,
			wantGates: map[string]bool{"code_review": true}, wantAccepted: true,
			wantV1Calls: 1, wantV1Gates: map[string]bool{"code_review": false},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := fixtureInput()
			if tc.criteria {
				input.SpecAcceptanceCriteria = "/fixture/criteria.md"
			}
			input.CodeReviewPolicy = tc.policy

			result, calls := runReviewReplayFixture(t, input, tc.exit, true)
			if len(calls) != 1 || calls[0].step != tc.wantStep || calls[0].activityID != "review-"+tc.wantStep {
				t.Fatalf("legacy review calls = %+v, want one %q step with Activity ID %q", calls, tc.wantStep, "review-"+tc.wantStep)
			}
			if got := reviewGates(result.GateResults); !equalGates(got, tc.wantGates) {
				t.Errorf("legacy review gates = %v, want %v", got, tc.wantGates)
			}
			if accepted := result.State == run.StateAccepted; accepted != tc.wantAccepted {
				t.Errorf("legacy state = %q, want accepted=%v", result.State, tc.wantAccepted)
			}

			result, calls = runReviewReplayFixture(t, input, tc.exit, false)
			if len(calls) != tc.wantV1Calls {
				t.Fatalf("version 1 review calls = %+v, want %d", calls, tc.wantV1Calls)
			}
			for _, c := range calls {
				if c.step != reviewstep.Combined {
					t.Errorf("version 1 review step = %q, want %q", c.step, reviewstep.Combined)
				}
			}
			if got := reviewGates(result.GateResults); !equalGates(got, tc.wantV1Gates) {
				t.Errorf("version 1 review gates = %v, want %v", got, tc.wantV1Gates)
			}
			if accepted := result.State == run.StateAccepted; accepted != tc.wantV1Accepted {
				t.Errorf("version 1 state = %q, want accepted=%v", result.State, tc.wantV1Accepted)
			}
		})
	}
}

// reviewGates is gateOutcomes narrowed to the two review gates.
func reviewGates(gates []run.GateResult) map[string]bool {
	out := map[string]bool{}
	for check, passed := range gateOutcomes(gates) {
		if check == reviewstep.SpecConformity || check == reviewstep.CodeReview {
			out[check] = passed
		}
	}
	return out
}

func equalGates(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || w != v {
			return false
		}
	}
	return true
}

// TestHistoryRecordedBeforeReviewReproductionKeepsTheOracleCommitClause: a
// history recorded before reviewReproductionChange committed a ticket's
// oracle only when its spec-conformity review passed, so its replay does not
// schedule CommitOraclesActivity after a failed one. Version 1 has no such
// clause.
func TestHistoryRecordedBeforeReviewReproductionKeepsTheOracleCommitClause(t *testing.T) {
	for _, tc := range []struct {
		name       string
		legacy     bool
		wantCommit bool
	}{
		{"legacy: conformity failed, no commit", true, false},
		{"version 1: no conformity clause", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := fixtureInput()
			input.SpecAcceptanceCriteria = "/fixture/criteria.md"
			input.CodeReviewPolicy = "required"
			setGateCommand(&input, "reference_oracle", "go test ./.oracle/...")
			input.ReferenceOracleDir = "/oracle/store"
			env := newWorkflowEnvironmentSansCommitOracles(t,
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
			if tc.legacy {
				env.OnGetVersion(reviewReproductionChange, temporalworkflow.DefaultVersion, 1).Return(temporalworkflow.DefaultVersion)
			}
			env.RegisterActivityWithOptions(func(_ context.Context, in NamedGateActivityInput) (VerifyActivityResult, error) {
				return VerifyActivityResult{Result: runner.Result{Command: []string{"sh", "-c", in.Command}, ExitCode: 0}, ReferenceOracleSHA256: "treehash"}, nil
			}, activity.RegisterOptions{Name: RunNamedGateActivityName})
			// Exit 41: under the retired decode, conformity failed and the
			// code review passed; under ReviewExitCode, 41 is no verdict, so
			// version 1 is given a verdict (40) instead.
			exit := reviewstep.CombinedExitBase + 1
			if !tc.legacy {
				exit = reviewstep.CombinedExitBase
			}
			env.RegisterActivityWithOptions(func(context.Context, ReviewStepInput) (VerifyActivityResult, error) {
				return VerifyActivityResult{Result: runner.Result{Command: []string{"python3", "review.py"}, ExitCode: exit}}, nil
			}, activity.RegisterOptions{Name: RunReviewStepActivityName})
			committed := false
			env.RegisterActivityWithOptions(func(context.Context, CommitOraclesInput) (CommittedOracles, error) {
				committed = true
				return CommittedOracles{}, nil
			}, activity.RegisterOptions{Name: CommitOraclesActivityName})
			env.ExecuteWorkflow(RunWorkflow, input)
			if err := env.GetWorkflowError(); err != nil {
				t.Fatalf("workflow error: %v", err)
			}
			if committed != tc.wantCommit {
				t.Errorf("CommitOraclesActivity called = %v, want %v", committed, tc.wantCommit)
			}
		})
	}
}
