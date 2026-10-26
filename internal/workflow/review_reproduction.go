package workflow

import (
	temporalworkflow "go.temporal.io/sdk/workflow"

	"buildgate/internal/policy"
	"buildgate/internal/reviewstep"
)

// reviewReproductionChange is the GetVersion change ID under which
// RunWorkflow's review phase runs the one review step (reviewstep.Plan) and
// decodes its launch with reviewstep.ReviewExitCode. It is asked once, at
// the top of runReviewSteps and only when every earlier gate passed, so the
// point it is asked at is deterministic. A history recorded before it
// (DefaultVersion) is already past the review and replays the retired plan
// and decode (legacyReviewPlan, legacyReviewOutcome) unchanged.
const reviewReproductionChange = "review-reproduction"

// ReviewStepRetiredFailureType is the non-retryable ApplicationError type of
// a review step a retired plan scheduled (the spec-conformity review or the
// standalone code review) that reaches a Worker that no longer runs it: an
// Activity pending in a history recorded before reviewReproductionChange.
// It is no verdict on the work (unresolved.NoVerdict), so the run halts with
// its worktree kept and the request waits in resume_review.
const ReviewStepRetiredFailureType = "ReviewStepRetired"

// planReviewSteps is the review plan this execution runs, and whether it is
// the retired one a history recorded before reviewReproductionChange holds.
func planReviewSteps(ctx temporalworkflow.Context, input RunWorkflowInput, codeReviewConfigured bool) ([]reviewstep.Step, bool) {
	if temporalworkflow.GetVersion(ctx, reviewReproductionChange, temporalworkflow.DefaultVersion, 1) == 1 {
		return reviewstep.Plan(codeReviewConfigured), false
	}
	return legacyReviewPlan(input.SpecAcceptanceCriteria != "", codeReviewConfigured), true
}

// legacyReviewPlan is the review plan of a history recorded before
// reviewReproductionChange: the spec-conformity review ran when the ticket
// declared acceptance criteria, the code review when its policy was on, and
// both folded into one combined launch. Only each step's name (its Activity
// ID and ReviewStepInput.Step) and stage matter on replay.
func legacyReviewPlan(conformityOn, codeReviewOn bool) []reviewstep.Step {
	switch {
	case conformityOn && codeReviewOn:
		return []reviewstep.Step{reviewstep.CombinedStep}
	case conformityOn:
		return []reviewstep.Step{{Name: reviewstep.SpecConformity, Stage: "conformity_review", Label: "spec-conformity review"}}
	case codeReviewOn:
		return []reviewstep.Step{{Name: reviewstep.CodeReview, Stage: "code_review", Label: "code review"}}
	}
	return nil
}

// legacyCombinedExitCodes is the retired combined launch's exit decode: the
// launch exited reviewstep.CombinedExitBase + bits, bit 0 for a
// spec-conformity review that did not pass, bit 1 for a code review that did
// not. Any other status failed both.
func legacyCombinedExitCodes(exit int) (conformityExitCode, codeReviewExitCode int) {
	bits := exit - reviewstep.CombinedExitBase
	if bits < 0 || bits > 3 {
		return 1, 1
	}
	return bits & 1, (bits >> 1) & 1
}

// legacyReviewOutcome adds one retired step's result to outcome: the code
// review's exit code as the code_review gate's, and the spec-conformity
// review's as outcome.legacyConformity, which evaluateRun hands the policy as
// the spec_conformity gate the history was recorded under.
func legacyReviewOutcome(outcome reviewOutcome, step reviewstep.Step, stepResult VerifyActivityResult) reviewOutcome {
	switch step.Name {
	case reviewstep.Combined:
		conformityExitCode, codeReviewExitCode := legacyCombinedExitCodes(stepResult.Result.ExitCode)
		conformity, codeReview := stepResult, stepResult
		conformity.Result.ExitCode, codeReview.Result.ExitCode = conformityExitCode, codeReviewExitCode
		outcome.legacyConformity, outcome.codeReview, outcome.ranCodeReview = &conformity, codeReview, true
	case reviewstep.SpecConformity:
		outcome.legacyConformity = &stepResult
	default:
		outcome.codeReview, outcome.ranCodeReview = stepResult, true
	}
	return outcome
}

// legacyConformityGates is namedGates with the spec_conformity gate a history
// recorded before reviewReproductionChange decided the run under appended
// last, after every named gate and before code_review, as the policy then
// ordered it. namedGates is never modified.
func legacyConformityGates(namedGates []policy.NamedGateInput, reviews reviewOutcome) []policy.NamedGateInput {
	c := reviews.legacyConformity
	if c == nil {
		return namedGates
	}
	return append(namedGates[:len(namedGates):len(namedGates)], policy.NamedGateInput{
		Check: reviewstep.SpecConformity, Command: c.Result.Command, ExitCode: c.Result.ExitCode,
		DurationMs: c.DurationMs, LogSHA256: c.LogSHA256,
	})
}

// legacyConformityBlocksOracleCommit is the retired oracle-commit clause: a
// ticket that declared acceptance criteria commits its oracle only when the
// spec-conformity review ran and passed. It applies only to a history
// recorded before reviewReproductionChange.
func legacyConformityBlocksOracleCommit(input RunWorkflowInput, reviews reviewOutcome) bool {
	if !reviews.legacy || input.SpecAcceptanceCriteria == "" {
		return false
	}
	return reviews.legacyConformity == nil || reviews.legacyConformity.Result.ExitCode != 0
}
