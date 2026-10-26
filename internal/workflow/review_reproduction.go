package workflow

import (
	"fmt"

	temporalworkflow "go.temporal.io/sdk/workflow"

	"buildgate/internal/policy"
	"buildgate/internal/reproduction"
	"buildgate/internal/reviewstep"
	"buildgate/internal/run"
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

// RunReviewReproductionActivityName runs one held reviewer's reproduction
// once on one commit (RunReviewReproductionActivity, SC-022).
const RunReviewReproductionActivityName = "RunReviewReproductionActivity"

// ReproductionUnheldFailureType is the non-retryable ApplicationError type of
// a reviewer's reproduction the factory accepted and then could not hold: its
// host copy could not be written, its hash no longer matched before a
// launch, or the launch refused its read-only mount. It is no verdict on the
// work (unresolved.ReproductionUnheld): the run halts with its worktree kept.
const ReproductionUnheldFailureType = "ReproductionUnheld"

// The passes of a reproduction: two launches on the result commit, one on
// the base commit only after both failed, and a corrective build's recheck.
const (
	reproductionPassHead1 = "head1"
	reproductionPassHead2 = "head2"
	reproductionPassBase  = "base"
)

// reproductionOutcome is what the reproduction phase and the recheck found.
// ran is set when the run took the phase (version 1, ReviewReproduction).
type reproductionOutcome struct {
	ran      bool
	findings []run.FindingReproduction
	rechecks []run.ReviewReproduction
}

// policyInput is the review_reproduction check's input, nil when the run did
// not take the phase.
func (o reproductionOutcome) policyInput() *policy.ReviewReproductionInput {
	if !o.ran {
		return nil
	}
	in := &policy.ReviewReproductionInput{}
	for _, f := range o.findings {
		in.Decided++
		if f.Disposition == run.DispositionBlocks {
			in.Refusing++
		}
	}
	for _, rc := range o.rechecks {
		in.Decided++
		if rc.Verdict != string(reproduction.VerdictPasses) {
			in.Refusing++
		}
	}
	return in
}

// refuses reports whether any reproduction refuses the run.
func (o reproductionOutcome) refuses() bool {
	in := o.policyInput()
	return in != nil && in.Refusing > 0
}

// reproductionContext is ctx with the reproduction Activity's options: the
// build/verify options (heartbeat, retries) with a start-to-close that
// covers one capped launch, its setup and the scratch worktree.
func reproductionContext(ctx, buildVerifyCtx temporalworkflow.Context) temporalworkflow.Context {
	options := temporalworkflow.GetActivityOptions(buildVerifyCtx)
	options.StartToCloseTimeout = reproductionActivityLimit
	return temporalworkflow.WithActivityOptions(ctx, options)
}

// runReproduction launches one pass of a held reproduction
// (RunReviewReproductionActivity), records its attempts, and returns its run.
func runReproductionPass(ctx temporalworkflow.Context, ids *activityIDs, rctx temporalworkflow.Context, input RunWorkflowInput, result *RunWorkflowResult, held run.HeldReproduction, id, pass, commit, worktreePath, branch string) (ReproductionRunResult, error) {
	var res ReproductionRunResult
	in := ReproductionRunInput{RunWorkflowInput: input, Held: held, Pass: pass, Commit: commit}
	if err := temporalworkflow.ExecuteActivity(ids.withRetries(rctx, id), RunReviewReproductionActivityName, in).Get(ctx, &res); err != nil {
		return res, wrapActivityFailure("run review reproduction activity", err, result.Attempts, result.BaseSHA, result.CommittedByWorker, worktreePath, branch)
	}
	result.Attempts = append(result.Attempts, res.Attempts...)
	return res, nil
}

// runResultOf is a recorded run as reproduction.Decide reads it.
func runResultOf(r run.ReproductionRun) *reproduction.RunResult {
	out := &reproduction.RunResult{Outcome: reproduction.Outcome(r.Outcome), Tests: make(map[string]reproduction.Kind, len(r.Tests))}
	for id, k := range r.Tests {
		out.Tests[id] = reproduction.Kind(k)
	}
	return out
}

// reproduceReviewFindings runs every reproduction the review held: head1,
// head2 only when head1 failed, and the base (gateBaseCommit) only when both
// failed; then reproduction.Decide. A refused reproduction is advice with no
// launch. Activity IDs are repro-<finding>-<pass>.
func reproduceReviewFindings(ctx temporalworkflow.Context, ids *activityIDs, buildVerifyCtx temporalworkflow.Context, input RunWorkflowInput, current *RunProgress, result *RunWorkflowResult, held []run.HeldReproduction, worktreePath, branch string) ([]run.FindingReproduction, error) {
	rctx := reproductionContext(ctx, buildVerifyCtx)
	base, _ := gateBaseCommit(input)
	var out []run.FindingReproduction
	for _, h := range held {
		rep := run.ReviewReproduction{Path: h.Path, SHA256: h.SHA256, Runner: h.Runner, TestIDs: h.TestIDs, Refused: h.Refused}
		if h.Refused != "" {
			rep.Verdict = run.ReproductionVerdictNotRun
			out = append(out, run.FindingReproduction{Finding: h.Finding, Reproduction: rep, Disposition: run.DispositionAdvice})
			continue
		}
		current.Stage, current.StartedAt = reproductionAttemptKind, temporalworkflow.Now(ctx)
		evidence := reproduction.Evidence{HoldsLiteral: h.HoldsLiteral}
		launch := func(pass, commit string) (*reproduction.RunResult, error) {
			res, err := runReproductionPass(ctx, ids, rctx, input, result, h, fmt.Sprintf("repro-%d-%s", h.Finding, pass), pass, commit, worktreePath, branch)
			if err != nil {
				return nil, err
			}
			rep.Runs = append(rep.Runs, res.Run)
			return runResultOf(res.Run), nil
		}
		var err error
		if evidence.Head1, err = launch(reproductionPassHead1, result.ResultSHA); err != nil {
			return nil, err
		}
		if evidence.Head1.Outcome == reproduction.OutcomeFailed {
			if evidence.Head2, err = launch(reproductionPassHead2, result.ResultSHA); err != nil {
				return nil, err
			}
		}
		if evidence.Head2 != nil && evidence.Head2.Outcome == reproduction.OutcomeFailed && base != "" {
			if evidence.Base, err = launch(reproductionPassBase, base); err != nil {
				return nil, err
			}
		}
		d := reproduction.Decide(evidence)
		rep.Verdict, rep.Reason = string(d.Verdict), d.Reason
		disposition := run.DispositionAdvice
		if d.Verdict == reproduction.VerdictBlocks {
			disposition = run.DispositionBlocks
		}
		out = append(out, run.FindingReproduction{Finding: h.Finding, Reproduction: rep, Disposition: disposition})
	}
	return out, nil
}

// recheckReproductions runs each of input.RecheckReproductions once on the
// run's result commit (Activity ID repro-recheck-<k>) and judges it with
// reproduction.Recheck: anything but a pass on every test ID, a result
// commit that holds the path included, is still failing.
func recheckReproductions(ctx temporalworkflow.Context, ids *activityIDs, buildVerifyCtx temporalworkflow.Context, input RunWorkflowInput, current *RunProgress, result *RunWorkflowResult, worktreePath, branch string) ([]run.ReviewReproduction, error) {
	rctx := reproductionContext(ctx, buildVerifyCtx)
	var out []run.ReviewReproduction
	for k, e := range input.RecheckReproductions {
		h := e.HeldReproduction
		current.Stage, current.StartedAt = reproductionAttemptKind, temporalworkflow.Now(ctx)
		res, err := runReproductionPass(ctx, ids, rctx, input, result, h, fmt.Sprintf("repro-recheck-%d", k), run.ReproductionPassRecheck, result.ResultSHA, worktreePath, branch)
		if err != nil {
			return nil, err
		}
		entry := e
		rep := run.ReviewReproduction{Path: h.Path, SHA256: h.SHA256, Runner: h.Runner, TestIDs: h.TestIDs, Runs: []run.ReproductionRun{res.Run}, Recheck: &entry}
		var ran *reproduction.RunResult
		if !res.PathInCommit && res.Run.Outcome != "" {
			ran = runResultOf(res.Run)
		}
		d := reproduction.Recheck(ran, h.TestIDs, res.PathInCommit)
		rep.Verdict, rep.Reason = string(d.Verdict), d.Reason
		out = append(out, rep)
	}
	return out, nil
}
