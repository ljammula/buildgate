package workflow

import (
	"buildgate/internal/policy"
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

// registerSpecConformityReviewWorkflowEnvironment mirrors
// registerFullSuiteWorkflowEnvironment for the two-phase spec-conformity
// review's own opt-in Activity.
func registerSpecConformityReviewWorkflowEnvironment(
	t *testing.T,
	build func(context.Context, RunWorkflowInput) (BuildActivityResult, error),
	verify func(context.Context, RunWorkflowInput) (VerifyActivityResult, error),
	specConformity func(context.Context, ReviewStepInput) (VerifyActivityResult, error),
) *testsuite.TestWorkflowEnvironment {
	t.Helper()
	env := newWorkflowEnvironment(t, build, verify, func(context.Context, EvaluateGateInput) (run.GateResult, error) {
		return run.GateResult{Check: "canonical_verify", Passed: true}, nil
	})
	env.RegisterActivityWithOptions(specConformity, activity.RegisterOptions{Name: RunReviewStepActivityName})
	return env
}

// TestRunWorkflowRunsSpecConformityReviewWhenDeclared is the two-phase
// design's own "Done when" case ported to the Temporal path (see
// RunSpecConformityReviewActivityName's own doc comment): a ticket that
// declares SpecAcceptanceCriteria gets RunSpecConformityReviewActivityName
// called exactly once, its own input carries the criteria path through,
// and a failing review quarantines the run with its own "spec_conformity"
// gate name as the cause -- the same contract cmd/factoryd
// pins for policy.SpecConformity.
func TestRunWorkflowRunsSpecConformityReviewWhenDeclared(t *testing.T) {
	input := fixtureInput()
	input.SpecAcceptanceCriteria = "/fixture/criteria.md"
	calls := 0
	env := registerSpecConformityReviewWorkflowEnvironment(t,
		func(context.Context, RunWorkflowInput) (BuildActivityResult, error) {
			return BuildActivityResult{Result: runner.Result{ExitCode: 0}}, nil
		},
		func(context.Context, RunWorkflowInput) (VerifyActivityResult, error) {
			return VerifyActivityResult{Result: runner.Result{Command: []string{"sh", "-c", "make verify"}, ExitCode: 0}}, nil
		},
		func(_ context.Context, in ReviewStepInput) (VerifyActivityResult, error) {
			calls++
			if in.SpecAcceptanceCriteria != "/fixture/criteria.md" {
				t.Errorf("ReviewStepInput.SpecAcceptanceCriteria = %q, want /fixture/criteria.md", in.SpecAcceptanceCriteria)
			}
			return VerifyActivityResult{Result: runner.Result{Command: []string{"python3", "conformity_review.py"}, ExitCode: 1}}, nil
		},
	)

	env.ExecuteWorkflow(RunWorkflow, input)
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error: %v", err)
	}
	var result RunWorkflowResult
	if err := env.GetWorkflowResult(&result); err != nil {
		t.Fatalf("workflow result: %v", err)
	}
	if result.State != run.StateQuarantined {
		t.Fatalf("state = %q, want %q", result.State, run.StateQuarantined)
	}
	if calls != 1 {
		t.Fatalf("spec-conformity-review Activity calls = %d, want 1", calls)
	}
	var gate *run.GateResult
	for i := range result.GateResults {
		if result.GateResults[i].Check == "spec_conformity" {
			gate = &result.GateResults[i]
		}
	}
	if gate == nil || gate.Passed {
		t.Fatalf("GateResults = %+v, want a failing spec_conformity gate", result.GateResults)
	}
	if !result.SpecConformityConfigured {
		t.Fatal("SpecConformityConfigured = false, want true when the ticket declared SpecAcceptanceCriteria")
	}
}

// TestRunWorkflowSkipsSpecConformityReviewWhenNotDeclared confirms the
// gate is opt-in on the Temporal path too, matching cmd/factoryd's direct
// path: no SpecAcceptanceCriteria means RunWorkflow never calls
// RunSpecConformityReviewActivityName at all.
func TestRunWorkflowSkipsSpecConformityReviewWhenNotDeclared(t *testing.T) {
	calls := 0
	env := registerSpecConformityReviewWorkflowEnvironment(t,
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
		t.Fatalf("spec-conformity-review Activity calls = %d, want 0 when SpecAcceptanceCriteria is not declared", calls)
	}
	for _, g := range result.GateResults {
		if g.Check == "spec_conformity" {
			t.Fatalf("GateResults = %+v, want no spec_conformity gate", result.GateResults)
		}
	}
	if result.SpecConformityConfigured {
		t.Fatal("SpecConformityConfigured = true, want false when the ticket never declared SpecAcceptanceCriteria")
	}
}

// TestRunWorkflowRunsSpecConformityReviewAfterCollectingEvidence pins the
// two-phase design's own core invariant directly, the same way
// TestRunWorkflowRunsFullSuiteVerifyBeforeCollectingEvidence pins full
// suite's ordering: the spec-conformity review must run AFTER
// CollectEvidenceActivity's own safety-net commit has already landed, not
// before -- see RunSpecConformityReviewActivityName's own doc comment for
// why (the reviewer needs a real commit to evaluate a commit-related
// criterion against).
func TestRunWorkflowRunsSpecConformityReviewAfterCollectingEvidence(t *testing.T) {
	input := fixtureInput()
	input.SpecAcceptanceCriteria = "/fixture/criteria.md"
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
			return VerifyActivityResult{Result: runner.Result{Command: []string{"python3", "conformity_review.py"}, ExitCode: 0}}, nil
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

// registerCodeReviewWorkflowEnvironment mirrors
// registerSpecConformityReviewWorkflowEnvironment for the standalone
// code-review phase's own opt-in Activity (M2-C).
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

// TestRunWorkflowSkipsCodeReviewWhenNotDeclared mirrors
// TestRunWorkflowSkipsSpecConformityReviewWhenNotDeclared: the code-review
// gate is opt-in on the Temporal path too, matching cmd/factoryd's direct
// path -- an empty (or "off") CodeReviewPolicy means RunWorkflow never
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
// code-review phase's own ordering, mirroring
// TestRunWorkflowRunsSpecConformityReviewAfterCollectingEvidence: it must
// run AFTER CollectEvidenceActivity's own safety-net commit, for the same
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

// TestRunWorkflowRunsCombinedReviewWhenBothEnabledAndSplitsExitBits
// proves reviewstep.Plan's own folding (combined-review call): declaring
// BOTH -spec-acceptance-criteria and -code-review-policy runs ONE
// RunReviewStepActivity call (Step == reviewstep.Combined), not two, and
// the workflow derives the two gates from that one launch's own exit
// code via reviewstep.GateExitCodes -- exit 1 (bit 0 only) here proves
// code review's own gate still passes REGARDLESS of spec-conformity's
// own outcome (M2-C's decided design, now expressed as an independent
// bit rather than an independent Activity call), mirroring
// run_ticket.go's identical predicate. Also pins the 2-Activity order
// (CollectEvidence, then the one combined review step).
func TestRunWorkflowRunsCombinedReviewWhenBothEnabledAndSplitsExitBits(t *testing.T) {
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
				t.Fatalf("ReviewStepInput.Step = %q, want %q (both reviews enabled must fold into one combined launch)", in.Step, reviewstep.Combined)
			}
			reviewCalls++
			record(reviewstep.Combined)
			// CombinedExitBase+1: bit 0 set (conformity did not succeed),
			// bit 1 clear (code review succeeded).
			return VerifyActivityResult{Result: runner.Result{Command: []string{"python3", "combined_review.py"}, ExitCode: reviewstep.CombinedExitBase + 1}}, nil
		},
		activity.RegisterOptions{Name: RunReviewStepActivityName},
	)

	env.ExecuteWorkflow(RunWorkflow, input)
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error: %v", err)
	}
	if reviewCalls != 1 {
		t.Fatalf("review Activity calls = %d, want 1 (both reviews enabled must fold into one combined launch)", reviewCalls)
	}
	if !slices.Equal(order, []string{CollectEvidenceActivityName, reviewstep.Combined}) {
		t.Fatalf("Activity call order = %v, want [%s, %s]", order, CollectEvidenceActivityName, reviewstep.Combined)
	}
	var result RunWorkflowResult
	if err := env.GetWorkflowResult(&result); err != nil {
		t.Fatalf("workflow result: %v", err)
	}
	if result.State != run.StateQuarantined {
		t.Fatalf("state = %q, want %q (spec_conformity failed)", result.State, run.StateQuarantined)
	}
	spec := gateByCheck(t, result.GateResults, "spec_conformity")
	if spec.Passed {
		t.Fatalf("spec_conformity gate = %+v, want failed", spec)
	}
	codeReviewGate := gateByCheck(t, result.GateResults, "code_review")
	if !codeReviewGate.Passed {
		t.Fatalf("code_review gate = %+v, want passed (bit 1 clear, regardless of spec-conformity's own outcome)", codeReviewGate)
	}
}

// TestRunWorkflowSkipsCommitOraclesWhenCodeReviewFails is
// TestRunWorkflowCommitsOraclesOnlyAfterSpecConformityPasses' counterpart
// for the code-review phase: CommitOraclesActivity must never run for a
// run whose declared CodeReviewPolicy's own review failed, mirroring
// run_ticket.go's identical predicate.
func TestRunWorkflowSkipsCommitOraclesWhenCodeReviewFails(t *testing.T) {
	input := fixtureInput()
	input.CodeReviewPolicy = "required"
	setGateCommand(&input, policy.ReferenceOracleGateID, "go test ./.oracle/...")
	input.ReferenceOracleDir = "/oracle/store"
	input.AllowedFiles = []string{"pkg/x.go"}

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
	env.RegisterActivityWithOptions(func(_ context.Context, in NamedGateActivityInput) (VerifyActivityResult, error) {
		return VerifyActivityResult{
			Result:                runner.Result{Command: []string{"sh", "-c", in.Command}, ExitCode: 0},
			ReferenceOracleSHA256: "treehash",
		}, nil
	}, activity.RegisterOptions{Name: RunNamedGateActivityName})
	env.RegisterActivityWithOptions(func(context.Context, CollectEvidenceInput) (CollectedEvidence, error) {
		return CollectedEvidence{ResultSHA: "collect-sha", ChangedFiles: []string{"pkg/x.go"}}, nil
	}, activity.RegisterOptions{Name: CollectEvidenceActivityName})
	env.RegisterActivityWithOptions(func(context.Context, ReviewStepInput) (VerifyActivityResult, error) {
		return VerifyActivityResult{Result: runner.Result{Command: []string{"python3", "code_review.py"}, ExitCode: 1}}, nil
	}, activity.RegisterOptions{Name: RunReviewStepActivityName})
	commitCalled := false
	env.RegisterActivityWithOptions(func(context.Context, CommitOraclesInput) (CommittedOracles, error) {
		commitCalled = true
		return CommittedOracles{}, nil
	}, activity.RegisterOptions{Name: CommitOraclesActivityName})

	env.ExecuteWorkflow(RunWorkflow, input)
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error: %v", err)
	}
	if commitCalled {
		t.Fatal("CommitOraclesActivity called, want it skipped: the declared code review failed")
	}
}
