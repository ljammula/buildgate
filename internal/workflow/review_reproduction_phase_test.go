package workflow

import (
	"context"
	"slices"
	"sync"
	"testing"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"
	temporalworkflow "go.temporal.io/sdk/workflow"

	"buildgate/internal/reproduction"
	"buildgate/internal/reviewstep"
	"buildgate/internal/run"
	"buildgate/internal/runner"
	"buildgate/internal/unresolved"
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

func runReproductionPhase(t *testing.T, input RunWorkflowInput, held []run.HeldReproduction, launch func(ReproductionRunInput) (ReproductionRunResult, error), legacy bool) reproductionPhase {
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
	}, activity.RegisterOptions{Name: RunReviewReproductionActivityName})
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

// reproductionGate is the review_reproduction gate's result, and whether it
// was recorded.
func reproductionGate(result RunWorkflowResult) (run.GateResult, bool) {
	for _, g := range result.GateResults {
		if g.Check == "review_reproduction" {
			return g, true
		}
	}
	return run.GateResult{}, false
}

// Each row of SC-022's table (reproduction.Decide), through the workflow:
// which launches it makes, in order, and what the finding and the run come
// to.
func TestReproductionPhaseFollowsTheDecisionTable(t *testing.T) {
	fail, pass := reproRun(reproduction.OutcomeFailed, reproduction.KindFail), reproRun(reproduction.OutcomePassed, reproduction.KindPass)
	errored := reproRun(reproduction.OutcomeFailed, reproduction.KindError)
	compile := reproRun(reproduction.OutcomeCompile, reproduction.KindCompile)
	missing := reproRun(reproduction.OutcomeMissing, reproduction.KindMissing)
	skipped := reproRun(reproduction.OutcomeInconclusive, reproduction.KindSkipped)
	for _, tc := range []struct {
		name         string
		literal      bool
		onBranch     bool
		runs         map[string]ReproductionRunResult
		wantCalls    []string
		wantVerdict  reproduction.Verdict
		wantAccepted bool
	}{
		{name: "passes on the result", runs: map[string]ReproductionRunResult{"head1": pass}, wantCalls: []string{"repro-0-head1"}, wantVerdict: reproduction.VerdictAdvice, wantAccepted: true},
		{name: "does not compile on the result", runs: map[string]ReproductionRunResult{"head1": compile}, wantCalls: []string{"repro-0-head1"}, wantVerdict: reproduction.VerdictAdvice, wantAccepted: true},
		{name: "leaves no report", runs: map[string]ReproductionRunResult{"head1": missing}, wantCalls: []string{"repro-0-head1"}, wantVerdict: reproduction.VerdictAdvice, wantAccepted: true},
		{name: "is skipped", runs: map[string]ReproductionRunResult{"head1": skipped}, wantCalls: []string{"repro-0-head1"}, wantVerdict: reproduction.VerdictAdvice, wantAccepted: true},
		{name: "fails once", runs: map[string]ReproductionRunResult{"head1": fail, "head2": pass}, wantCalls: []string{"repro-0-head1", "repro-0-head2"}, wantVerdict: reproduction.VerdictAdvice, wantAccepted: true},
		{name: "fails twice and passes on the base", runs: map[string]ReproductionRunResult{"head1": fail, "head2": fail, "base": pass}, wantCalls: []string{"repro-0-head1", "repro-0-head2", "repro-0-base"}, wantVerdict: reproduction.VerdictBlocks},
		{name: "fails twice and does not compile on the base", runs: map[string]ReproductionRunResult{"head1": fail, "head2": fail, "base": compile}, wantCalls: []string{"repro-0-head1", "repro-0-head2", "repro-0-base"}, wantVerdict: reproduction.VerdictBlocks},
		{name: "fails twice and fails another way on the base", runs: map[string]ReproductionRunResult{"head1": fail, "head2": fail, "base": errored}, wantCalls: []string{"repro-0-head1", "repro-0-head2", "repro-0-base"}, wantVerdict: reproduction.VerdictBlocks},
		{name: "fails twice and the base leaves no report", runs: map[string]ReproductionRunResult{"head1": fail, "head2": fail, "base": missing}, wantCalls: []string{"repro-0-head1", "repro-0-head2", "repro-0-base"}, wantVerdict: reproduction.VerdictAdvice, wantAccepted: true},
		{name: "fails the same way on the base", runs: map[string]ReproductionRunResult{"head1": fail, "head2": fail, "base": fail}, wantCalls: []string{"repro-0-head1", "repro-0-head2", "repro-0-base"}, wantVerdict: reproduction.VerdictAdvice, wantAccepted: true},
		{name: "fails the same way on the base and holds the literal", literal: true, runs: map[string]ReproductionRunResult{"head1": fail, "head2": fail, "base": fail}, wantCalls: []string{"repro-0-head1", "repro-0-head2", "repro-0-base"}, wantVerdict: reproduction.VerdictBlocks},
		{name: "fails twice with the base unknown", onBranch: true, runs: map[string]ReproductionRunResult{"head1": fail, "head2": fail}, wantCalls: []string{"repro-0-head1", "repro-0-head2"}, wantVerdict: reproduction.VerdictAdvice, wantAccepted: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := reproductionInput()
			if tc.onBranch {
				input.OnBranch = "factoryd/earlier"
			}
			held := heldFixture()
			held.HoldsLiteral = tc.literal
			p := runReproductionPhase(t, input, []run.HeldReproduction{held}, byPass(tc.runs), false)
			if p.err != nil {
				t.Fatalf("workflow error: %v", p.err)
			}
			if !slices.Equal(p.calls, tc.wantCalls) {
				t.Errorf("launches = %v, want %v", p.calls, tc.wantCalls)
			}
			for _, in := range p.passes {
				wantCommit := p.result.ResultSHA
				if in.Pass == "base" {
					wantCommit = "fixture-base-sha"
				}
				if in.Commit != wantCommit || in.Held.SHA256 != held.SHA256 {
					t.Errorf("%s ran on %q with hash %q, want %q and the held hash", in.Pass, in.Commit, in.Held.SHA256, wantCommit)
				}
			}
			if len(p.result.ReviewReproductions) != 1 {
				t.Fatalf("ReviewReproductions = %+v, want one", p.result.ReviewReproductions)
			}
			got := p.result.ReviewReproductions[0]
			wantDisposition := run.DispositionAdvice
			if tc.wantVerdict == reproduction.VerdictBlocks {
				wantDisposition = run.DispositionBlocks
			}
			if got.Reproduction.Verdict != string(tc.wantVerdict) || got.Disposition != wantDisposition || got.Reproduction.Reason == "" || len(got.Reproduction.Runs) != len(tc.wantCalls) {
				t.Errorf("finding = %+v, want verdict %s, disposition %s, a reason and one run per launch", got, tc.wantVerdict, wantDisposition)
			}
			gate, ok := reproductionGate(p.result)
			if !ok || gate.Passed != tc.wantAccepted {
				t.Errorf("review_reproduction gate = %+v (recorded %v), want passed=%v", gate, ok, tc.wantAccepted)
			}
			if wantState := map[bool]run.State{true: run.StateAccepted, false: run.StateQuarantined}[tc.wantAccepted]; p.result.State != wantState {
				t.Errorf("state = %s, want %s", p.result.State, wantState)
			}
		})
	}
}

// A reproduction the shape rules refused is advice with no launch, and does
// not fail the check.
func TestRefusedReproductionIsAdviceWithNoLaunch(t *testing.T) {
	held := []run.HeldReproduction{{Finding: 2, Path: "README.md", Refused: "the reproduction path does not match the test patterns"}}
	p := runReproductionPhase(t, reproductionInput(), held, byPass(nil), false)
	if p.err != nil {
		t.Fatalf("workflow error: %v", p.err)
	}
	if len(p.calls) != 0 {
		t.Errorf("launches = %v, want none", p.calls)
	}
	if got := p.result.ReviewReproductions; len(got) != 1 || got[0].Finding != 2 || got[0].Disposition != run.DispositionAdvice || got[0].Reproduction.Verdict != run.ReproductionVerdictNotRun || got[0].Reproduction.Refused == "" {
		t.Errorf("ReviewReproductions = %+v, want the third finding as advice, not run, with its refusal", got)
	}
	if gate, ok := reproductionGate(p.result); !ok || !gate.Passed || p.result.State != run.StateAccepted {
		t.Errorf("gate = %+v (%v), state %s; want a passing check and an accepted run", gate, ok, p.result.State)
	}
}

// P2.6: a run without -review-reproduction (a bare factoryd -ticket) launches
// no reproduction and records no review_reproduction check, whatever its
// review held.
func TestSingleTicketRunSchedulesNoReproduction(t *testing.T) {
	input := reproductionInput()
	input.ReviewReproduction = false
	input.RecheckReproductions = []run.HeldReproduction{heldFixture()}
	fail := reproRun(reproduction.OutcomeFailed, reproduction.KindFail)
	p := runReproductionPhase(t, input, []run.HeldReproduction{heldFixture()}, byPass(map[string]ReproductionRunResult{"head1": fail, "head2": fail, "base": reproRun(reproduction.OutcomePassed, reproduction.KindPass), "recheck": fail}), false)
	if p.err != nil {
		t.Fatalf("workflow error: %v", p.err)
	}
	if len(p.calls) != 0 || p.result.ReviewReproductions != nil || p.result.ReviewReproductionRechecks != nil {
		t.Errorf("launches %v, findings %+v, rechecks %+v; want none", p.calls, p.result.ReviewReproductions, p.result.ReviewReproductionRechecks)
	}
	if _, ok := reproductionGate(p.result); ok || p.result.State != run.StateAccepted {
		t.Errorf("gates = %+v, state %s; want no review_reproduction check and an accepted run", p.result.GateResults, p.result.State)
	}
}

// A history recorded before reviewReproductionChange (DefaultVersion) runs no
// reproduction and no recheck, even with the input set.
func TestHistoryRecordedBeforeReviewReproductionSchedulesNoReproduction(t *testing.T) {
	input := reproductionInput()
	input.RecheckReproductions = []run.HeldReproduction{heldFixture()}
	fail := reproRun(reproduction.OutcomeFailed, reproduction.KindFail)
	p := runReproductionPhase(t, input, []run.HeldReproduction{heldFixture()}, byPass(map[string]ReproductionRunResult{"head1": fail, "head2": fail, "base": fail, "recheck": fail}), true)
	if p.err != nil {
		t.Fatalf("workflow error: %v", p.err)
	}
	if len(p.calls) != 0 {
		t.Errorf("launches = %v, want none at DefaultVersion", p.calls)
	}
	if _, ok := reproductionGate(p.result); ok {
		t.Errorf("gates = %+v, want no review_reproduction check", p.result.GateResults)
	}
}

// A corrective build's recheck runs each held copy once on the result
// commit, before the review: a pass on every ID passes, anything else is
// still failing and fails the check.
func TestRecheckRunsEachHeldCopyOnceOnTheResult(t *testing.T) {
	pass, fail := reproRun(reproduction.OutcomePassed, reproduction.KindPass), reproRun(reproduction.OutcomeFailed, reproduction.KindFail)
	for _, tc := range []struct {
		name         string
		res          ReproductionRunResult
		wantVerdict  reproduction.Verdict
		wantAccepted bool
	}{
		{"passes", pass, reproduction.VerdictPasses, true},
		{"still fails", fail, reproduction.VerdictStillFailing, false},
		{"does not compile", reproRun(reproduction.OutcomeCompile, reproduction.KindCompile), reproduction.VerdictStillFailing, false},
		{"leaves no report", reproRun(reproduction.OutcomeMissing, reproduction.KindMissing), reproduction.VerdictStillFailing, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := reproductionInput()
			input.RecheckReproductions = []run.HeldReproduction{heldFixture()}
			p := runReproductionPhase(t, input, nil, byPass(map[string]ReproductionRunResult{"recheck": tc.res}), false)
			if p.err != nil {
				t.Fatalf("workflow error: %v", p.err)
			}
			if !slices.Equal(p.calls, []string{"repro-recheck-0"}) || p.passes[0].Commit != p.result.ResultSHA {
				t.Fatalf("launches = %v on %q, want one recheck on the result %q", p.calls, p.passes[0].Commit, p.result.ResultSHA)
			}
			if got := p.result.ReviewReproductionRechecks; len(got) != 1 || got[0].Verdict != string(tc.wantVerdict) || len(got[0].Runs) != 1 {
				t.Errorf("rechecks = %+v, want one %s with its run", got, tc.wantVerdict)
			}
			if gate, ok := reproductionGate(p.result); !ok || gate.Passed != tc.wantAccepted {
				t.Errorf("gate = %+v (%v), want passed=%v", gate, ok, tc.wantAccepted)
			}
		})
	}
}

// A result commit that adds or changes the reproduction's path is still
// failing, whatever its launch would report (the Activity launches nothing
// for it).
func TestRecheckResultCommitAddingPathIsStillFailing(t *testing.T) {
	input := reproductionInput()
	input.RecheckReproductions = []run.HeldReproduction{heldFixture()}
	p := runReproductionPhase(t, input, nil, func(in ReproductionRunInput) (ReproductionRunResult, error) {
		return ReproductionRunResult{Run: run.ReproductionRun{Pass: in.Pass, Commit: in.Commit}, PathInCommit: true}, nil
	}, false)
	if p.err != nil {
		t.Fatalf("workflow error: %v", p.err)
	}
	if got := p.result.ReviewReproductionRechecks; len(got) != 1 || got[0].Verdict != string(reproduction.VerdictStillFailing) {
		t.Errorf("rechecks = %+v, want still failing", got)
	}
	if p.result.State != run.StateQuarantined {
		t.Errorf("state = %s, want quarantined", p.result.State)
	}
}

// A reproduction the factory could not hold or mount halts the run as no
// verdict on the work.
func TestReproductionUnheldHaltsWithItsCause(t *testing.T) {
	p := runReproductionPhase(t, reproductionInput(), []run.HeldReproduction{heldFixture()}, func(ReproductionRunInput) (ReproductionRunResult, error) {
		return ReproductionRunResult{}, temporal.NewNonRetryableApplicationError("review reproduction: hash", ReproductionUnheldFailureType, nil)
	}, false)
	if p.err == nil {
		t.Fatal("workflow succeeded, want a halt")
	}
	if got := UnresolvedCauseFromError(p.err); got != unresolved.ReproductionUnheld {
		t.Errorf("cause = %q, want %q", got, unresolved.ReproductionUnheld)
	}
}
