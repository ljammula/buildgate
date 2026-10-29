package workflow

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"

	"go.temporal.io/sdk/activity"
	temporalworkflow "go.temporal.io/sdk/workflow"

	"buildgate/internal/reproduction"
	"buildgate/internal/run"
	"buildgate/internal/runner"
)

// testsRedPhase is one RunWorkflow execution whose evidence changed a code file
// and a test file, with fake hold and launch Activities.
type testsRedPhase struct {
	result   RunWorkflowResult
	err      error
	mu       sync.Mutex
	calls    []string
	launches []ReproductionRunInput
	holds    []TestsRedHoldInput
}

func testsRedHeld() []run.HeldReproduction {
	return []run.HeldReproduction{{Finding: 0, Path: "pkg/a_test.go", SHA256: "aa", HostPath: "/fixture/held/0/pkg/a_test.go", TestIDs: []string{"TestA"}, Runner: "go-test-json"}}
}

// testsRedKinds is what one fake launch reports for TestA.
func testsRedKinds(outcome reproduction.Outcome, kind reproduction.Kind) ReproductionRunResult {
	return reproRun(outcome, kind).withTests("TestA")
}

// withTests renames the single test ID of a one-ID fake launch.
func (r ReproductionRunResult) withTests(id string) ReproductionRunResult {
	for _, k := range r.Run.Tests {
		r.Run.Tests = map[string]string{id: k}
	}
	return r
}

func runTestsRedPhase(t *testing.T, input RunWorkflowInput, held TestsRedHoldResult, verifyExit int, results map[string]ReproductionRunResult, version bool) *testsRedPhase {
	t.Helper()
	p := &testsRedPhase{}
	env := newWorkflowEnvironment(t,
		func(context.Context, RunWorkflowInput) (BuildActivityResult, error) {
			return BuildActivityResult{Result: runner.Result{ExitCode: 0}}, nil
		},
		func(context.Context, RunWorkflowInput) (VerifyActivityResult, error) {
			return VerifyActivityResult{Result: runner.Result{Command: []string{"sh", "-c", "make verify"}, ExitCode: verifyExit}}, nil
		},
		func(context.Context, EvaluateGateInput) (run.GateResult, error) {
			return run.GateResult{Check: "canonical_verify", Passed: true}, nil
		},
	)
	if !version {
		env.OnGetVersion(testsRedOnBaseChange, temporalworkflow.DefaultVersion, 1).Return(temporalworkflow.DefaultVersion)
	}
	env.RegisterActivityWithOptions(func(context.Context, CollectEvidenceInput) (CollectedEvidence, error) {
		return CollectedEvidence{ResultSHA: "result-sha", ChangedFiles: []string{"pkg/a.go", "pkg/a_test.go"}}, nil
	}, activity.RegisterOptions{Name: CollectEvidenceActivityName})
	env.RegisterActivityWithOptions(func(ctx context.Context, in TestsRedHoldInput) (TestsRedHoldResult, error) {
		p.mu.Lock()
		defer p.mu.Unlock()
		p.calls = append(p.calls, activity.GetInfo(ctx).ActivityID)
		p.holds = append(p.holds, in)
		return held, nil
	}, activity.RegisterOptions{Name: HoldChangedTestsActivityName})
	env.RegisterActivityWithOptions(func(ctx context.Context, in ReproductionRunInput) (ReproductionRunResult, error) {
		p.mu.Lock()
		defer p.mu.Unlock()
		p.calls = append(p.calls, activity.GetInfo(ctx).ActivityID)
		p.launches = append(p.launches, in)
		res := results[in.Pass]
		res.Run.Pass, res.Run.Commit = in.Pass, in.Commit
		return res, nil
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

func testsRedInput() RunWorkflowInput {
	input := fixtureInput()
	input.TestsRequiredOptOut = ""
	return input
}

func testsRedGate(result RunWorkflowResult) (run.GateResult, bool) {
	for _, g := range result.GateResults {
		if g.Check == "tests_red_on_base" {
			return g, true
		}
	}
	return run.GateResult{}, false
}

func hasNote(result RunWorkflowResult, want string) bool {
	return slices.ContainsFunc(result.Notes, func(n string) bool { return strings.Contains(n, want) })
}

// The proof's verdict table through the workflow: which launches it makes, in
// order, on which commits, and what the gate and the run come to.
func TestTestsRedOnBaseFollowsTheVerdictTable(t *testing.T) {
	pass := testsRedKinds(reproduction.OutcomePassed, reproduction.KindPass)
	fail := testsRedKinds(reproduction.OutcomeFailed, reproduction.KindFail)
	compile := testsRedKinds(reproduction.OutcomeCompile, reproduction.KindCompile)
	missing := testsRedKinds(reproduction.OutcomeMissing, reproduction.KindMissing)
	for _, tc := range []struct {
		name         string
		runs         map[string]ReproductionRunResult
		wantCalls    []string
		wantVerdict  string
		wantAccepted bool
		wantNote     string
	}{
		{"fails on the base, passes on the result", map[string]ReproductionRunResult{"result": pass, "base": fail}, []string{"tests-red-hold", "tests-red-0-result", "tests-red-0-base"}, run.TestsRedProved, true, ""},
		{"does not compile on the base", map[string]ReproductionRunResult{"result": pass, "base": compile}, []string{"tests-red-hold", "tests-red-0-result", "tests-red-0-base"}, run.TestsRedProved, true, ""},
		{"passes on both", map[string]ReproductionRunResult{"result": pass, "base": pass}, []string{"tests-red-hold", "tests-red-0-result", "tests-red-0-base"}, run.TestsRedNotExercised, false, ""},
		{"fails on the result: the base is not run, no verdict", map[string]ReproductionRunResult{"result": fail, "base": fail}, []string{"tests-red-hold", "tests-red-0-result"}, run.TestsRedNoVerdict, true, "tests_red_on_base: no verdict (a changed test failed when run alone on the result commit although the full suite passed)"},
		{"the base leaves no report", map[string]ReproductionRunResult{"result": pass, "base": missing}, []string{"tests-red-hold", "tests-red-0-result", "tests-red-0-base"}, run.TestsRedNoVerdict, true, "tests_red_on_base: no verdict"},
		{"the result leaves no report", map[string]ReproductionRunResult{"result": missing, "base": pass}, []string{"tests-red-hold", "tests-red-0-result", "tests-red-0-base"}, run.TestsRedNoVerdict, true, "tests_red_on_base: no verdict"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := runTestsRedPhase(t, testsRedInput(), TestsRedHoldResult{Held: testsRedHeld()}, 0, tc.runs, true)
			if p.err != nil {
				t.Fatalf("workflow error: %v", p.err)
			}
			if !slices.Equal(p.calls, tc.wantCalls) {
				t.Errorf("calls = %v, want %v", p.calls, tc.wantCalls)
			}
			for _, in := range p.launches {
				wantCommit := "result-sha"
				if in.Pass == run.TestsRedPassBase {
					wantCommit = "fixture-base-sha"
				}
				if in.Commit != wantCommit || in.AttemptKind != "tests_red_on_base" || in.Held.SHA256 != "aa" {
					t.Errorf("%s launch = commit %q, kind %q, hash %q; want %q, tests_red_on_base, aa", in.Pass, in.Commit, in.AttemptKind, in.Held.SHA256, wantCommit)
				}
			}
			proof := p.result.TestsRedOnBase
			if proof == nil || proof.Verdict != tc.wantVerdict || proof.Reason == "" || len(proof.Files) != 1 || proof.Files[0].SHA256 != "aa" || len(proof.Files[0].Runs) != len(tc.wantCalls)-1 {
				t.Fatalf("TestsRedOnBase = %+v, want verdict %s, a reason, the held hash and one run per launch", proof, tc.wantVerdict)
			}
			gate, ok := testsRedGate(p.result)
			if !ok || gate.Passed != tc.wantAccepted {
				t.Errorf("tests_red_on_base gate = %+v (recorded %v), want passed=%v", gate, ok, tc.wantAccepted)
			}
			if (p.result.State == run.StateAccepted) != tc.wantAccepted {
				t.Errorf("state = %s, want accepted=%v", p.result.State, tc.wantAccepted)
			}
			if tc.wantNote != "" && !hasNote(p.result, tc.wantNote) {
				t.Errorf("Notes = %q, want one containing %q", p.result.Notes, tc.wantNote)
			}
			if tc.wantNote == "" && len(p.result.Notes) != 0 {
				t.Errorf("Notes = %q, want none", p.result.Notes)
			}
		})
	}
}

// Nothing is launched, and no verdict reached, when the proof cannot start:
// the gate passes with a note and never refuses.
func TestTestsRedOnBaseWithNothingToRunIsNoVerdict(t *testing.T) {
	onBranch := testsRedInput()
	onBranch.OnBranch = "factoryd/earlier"
	for _, tc := range []struct {
		name      string
		input     RunWorkflowInput
		held      TestsRedHoldResult
		wantCalls []string
	}{
		{"no runner takes a changed test file", testsRedInput(), TestsRedHoldResult{Reason: "no changed test file has a test runner the factory can run"}, []string{"tests-red-hold"}},
		{"no base commit is known", onBranch, TestsRedHoldResult{Held: testsRedHeld()}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := runTestsRedPhase(t, tc.input, tc.held, 0, nil, true)
			if p.err != nil {
				t.Fatalf("workflow error: %v", p.err)
			}
			if !slices.Equal(p.calls, tc.wantCalls) || len(p.launches) != 0 {
				t.Errorf("calls = %v, want %v and no launch", p.calls, tc.wantCalls)
			}
			if gate, ok := testsRedGate(p.result); !ok || !gate.Passed || p.result.State != run.StateAccepted {
				t.Errorf("gate = %+v (%v), state %s; want a passing gate and an accepted run", gate, ok, p.result.State)
			}
			if !hasNote(p.result, "tests_red_on_base: no verdict") {
				t.Errorf("Notes = %q, want the no-verdict note", p.result.Notes)
			}
			if p.result.TestsRedOnBase == nil || p.result.TestsRedOnBase.Verdict != run.TestsRedNoVerdict {
				t.Errorf("TestsRedOnBase = %+v, want a no_verdict record", p.result.TestsRedOnBase)
			}
		})
	}
}

// A ticket that opted out of tests launches nothing and passes; a failed
// canonical verification launches nothing either: the proof runs only once
// the run's own checks passed.
func TestTestsRedOnBaseRunsOnlyAfterEarlierGatesPassAndWithoutAnOptOut(t *testing.T) {
	optOut := testsRedInput()
	optOut.TestsRequiredOptOut = "docs only"
	p := runTestsRedPhase(t, optOut, TestsRedHoldResult{Held: testsRedHeld()}, 0, nil, true)
	if p.err != nil || len(p.calls) != 0 || p.result.State != run.StateAccepted || p.result.TestsRedOnBase != nil {
		t.Errorf("opt-out: err %v, calls %v, state %s, proof %+v; want no launch, accepted, no proof", p.err, p.calls, p.result.State, p.result.TestsRedOnBase)
	}
	p = runTestsRedPhase(t, testsRedInput(), TestsRedHoldResult{Held: testsRedHeld()}, 1, nil, true)
	if p.err != nil || len(p.calls) != 0 || p.result.State != run.StateQuarantined || p.result.TestsRedOnBase != nil {
		t.Errorf("failed verify: err %v, calls %v, state %s, proof %+v; want no launch, quarantined, no proof", p.err, p.calls, p.result.State, p.result.TestsRedOnBase)
	}
}

// A history recorded before testsRedOnBaseChange (DefaultVersion) took none of
// the proof's Activities and replays without them: the gate then asks only
// that a test file changed, and notes that the tests were not run.
func TestHistoryRecordedBeforeTestsRedOnBaseReplaysUnchanged(t *testing.T) {
	p := runTestsRedPhase(t, testsRedInput(), TestsRedHoldResult{Held: testsRedHeld()}, 0, nil, false)
	if p.err != nil || len(p.calls) != 0 {
		t.Fatalf("err %v, calls %v; want none of the proof's Activities at DefaultVersion", p.err, p.calls)
	}
	if p.result.State != run.StateAccepted || !hasNote(p.result, "the changed tests were not run") || p.result.TestsRedOnBase != nil {
		t.Errorf("state %s, notes %q, proof %+v; want accepted with the not-run note and no proof", p.result.State, p.result.Notes, p.result.TestsRedOnBase)
	}
}

// reproduction is a leaf that cannot import run, so its verdict strings are
// copies: they must be the run record's.
func TestRedVerdictsAreTheRunRecordsStrings(t *testing.T) {
	for _, p := range [][2]string{
		{reproduction.RedProved, run.TestsRedProved}, {reproduction.RedNotExercised, run.TestsRedNotExercised},
		{reproduction.RedNoVerdict, run.TestsRedNoVerdict},
	} {
		if p[0] != p[1] {
			t.Errorf("reproduction verdict %q != run verdict %q", p[0], p[1])
		}
	}
}
