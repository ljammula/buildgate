package workflow

import (
	"buildgate/internal/codereview"
	"buildgate/internal/evidence"
	"buildgate/internal/reviewstep"
	"buildgate/internal/run"
	"buildgate/internal/runner"
	"buildgate/internal/sandbox"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"
)

// reviewStepPassed reports whether a review launch passed, for its progress
// feed's end outcome. The launch exits reviewstep.CombinedExitBase when it
// gave a verdict, so its pass is 40, not 0; reading it as "exit != 0 fails"
// marked every passing review failed on the console's Timeline (found in a
// 2026-09-29 walkthrough) while the gate, which decodes it with
// ReviewExitCode, passed.
func reviewStepPassed(exitCode int) bool {
	return reviewstep.ReviewExitCode(exitCode) == 0
}

// RunReviewStepActivity invokes the review step (reviewstep.CombinedStep,
// agent/pi/scripts/combined_review.py) through runner.RunWithRetries.
// A step a retired plan scheduled (input.Step "spec_conformity" or
// "code_review", or a combined review carrying acceptance criteria, pending
// in a history recorded before reviewReproductionChange: retiredReviewStep)
// is refused with the non-retryable ReviewStepRetiredFailureType: this
// Worker no longer runs it.
//
// RunWorkflow only calls this once CollectEvidenceActivity's own
// safety-net commit has already landed, so the workspace is guaranteed
// clean and the reviewer has a real commit to evaluate -- see
// RunReviewStepActivityName's own doc comment. Reuses VerifyActivityResult
// (not a new result type): this Activity's own output shape (a subprocess
// result, duration, log hash, attempts) is identical to RunVerifyActivity's.
func (a *Activities) RunReviewStepActivity(ctx context.Context, input ReviewStepInput) (progressResult VerifyActivityResult, err error) {
	ctx, endGuard := GuardStep(ctx)
	defer endGuard()
	step := reviewstep.CombinedStep
	if retiredReviewStep(input) {
		return VerifyActivityResult{}, temporal.NewNonRetryableApplicationError(fmt.Sprintf("review step %q is retired: this Worker runs only the %q review without the spec-conformity task", input.Step, reviewstep.Combined), ReviewStepRetiredFailureType, nil)
	}
	// The stage "start" mark is deferred until past the checkpoint
	// short-circuit below: a redispatch of an already-completed Activity
	// must not add a spurious near-zero start/end pair to the feed.
	progressStarted := false
	defer func() {
		if !progressStarted {
			return
		}
		outcome := "pass"
		if err != nil || !reviewStepPassed(progressResult.Result.ExitCode) {
			outcome = "fail"
		}
		progressMark(ctx, a.logDirFor(input.RunWorkflowInput), step.Stage, "end", outcome, reviewProgressDetail(err))
	}()
	if err := a.fenceEarlierAttempts(ctx, input.RunWorkflowInput, true); err != nil {
		return VerifyActivityResult{}, err
	}
	// A stub an earlier attempt of this review left (its worker died
	// mid-launch) goes before anything reads the worktree.
	if err := a.sweepReviewStubs(ctx, input.RunWorkflowInput); err != nil {
		return VerifyActivityResult{}, err
	}
	if input.RoutePolicy != nil || input.ReviewRelayPolicy != nil {
		a.recordSpendStartAtAttemptOne(ctx, input.RunWorkflowInput)
	}
	checkpoint, path, found, err := loadRetriedActivityCheckpoint[VerifyActivityResult](ctx, a.checkpointDirFor(input.RunWorkflowInput))
	if err != nil {
		return VerifyActivityResult{}, checkpointLoadError(fmt.Sprintf("load %s Activity checkpoint", step.Label), err)
	}
	if found {
		if checkpoint.Error != "" {
			errType := checkpoint.ErrorType
			if errType == "" {
				errType = InfrastructureFailureType
			}
			return checkpoint.Result, temporal.NewApplicationError(checkpoint.Error, errType, checkpoint.Result.Attempts)
		}
		return checkpoint.Result, nil
	}
	progressStarted = true
	progressMark(ctx, a.logDirFor(input.RunWorkflowInput), step.Stage, "start", "", "")

	// Resolved before any intent record or Docker contact, as in
	// RunBuildActivity. Unlike RunBuildActivity, a nil relay spec here is
	// not "run unsandboxed/uncontained" -- it means this step was enabled
	// (RunWorkflow only calls this Activity for a step whose own policy
	// says to run it) but this run has no relay configured at all, so this
	// review cannot reach a model. Silently skipping this whole phase
	// would leave the operator's own declared review never checked, while
	// the run could still go on to be accepted -- exactly the silent
	// bypass the two-phase design exists to prevent (found via
	// adversarial review on cmd/factoryd's own matching guard in
	// run_ticket.go). Halt explicitly instead.
	// routedReview: a routes:-configured ReviewRelayPolicy names its own
	// route (Route != ""), which PhaseRelaySpec (below) then replaces
	// EVERY route-derived field of baseRelaySpec with, credentials
	// included, once review.Upstream != "" (see codereview.PhaseRelaySpec's
	// own doc comment) -- so this review's own success must never depend
	// on resolving the BUILD's own route's credential at all: only
	// a.CheckRoute (no ResolveRouteCredentials) confirms input.RoutePolicy
	// still binds to this Worker's own config, and baseRelaySpec is built
	// with NO credential (only its ceiling/budget fields are ever read
	// from it once review.Upstream != "").
	routedReview := input.RoutePolicy != nil && input.ReviewRelayPolicy != nil && input.ReviewRelayPolicy.Route != ""
	var baseRelaySpec *sandbox.RouteSpec
	if routedReview {
		if a.CheckRoute == nil {
			return VerifyActivityResult{}, temporal.NewApplicationError(
				fmt.Sprintf("this run names route %q but this Worker has no routes: config", input.RoutePolicy.Route),
				RelayConfigurationFailureType,
			)
		}
		if err := a.CheckRoute(relayRoleExecution, *input.RoutePolicy, input.Thinking); err != nil {
			return VerifyActivityResult{}, temporal.NewApplicationError(err.Error(), RelayConfigurationFailureType)
		}
		spec := input.RoutePolicy.Spec(sandbox.RouteSecret{}, sandbox.RouteSecret{}, sandbox.RouteSecret{}, sandbox.RouteSecret{}, a.runIDFor(input.RunWorkflowInput), a.dataDirFor(input.RunWorkflowInput))
		spec.CABundlePath = a.EgressCABundlePath
		baseRelaySpec = &spec
	} else {
		spec, err := a.relaySpecFor(input.RunWorkflowInput)
		if err != nil {
			return VerifyActivityResult{}, temporal.NewApplicationError(err.Error(), RelayConfigurationFailureType)
		}
		baseRelaySpec = spec
	}
	if baseRelaySpec == nil {
		return VerifyActivityResult{}, temporal.NewApplicationError(reviewStepNoRelayError(input), RelayConfigurationFailureType)
	}
	// roles.review, when set, owns this step's relay policy -- carried as
	// input.ReviewRelayPolicy, credential-free like input.RoutePolicy
	// itself, naming the review role's own route (Route != ""), resolved
	// and trust-checked here exactly like the build's own RoutePolicy
	// (checkAndResolveRoute, boundRelaySpec's own shared first half) --
	// this Worker's own a.ResolveRouteCredentials resolves that route's
	// real credential, never one implied by the submitted policy. nil
	// (roles.review unset) keeps PhaseRelaySpec on the build's own relay
	// spec fields, unchanged.
	var review *codereview.ReviewRoute
	if input.ReviewRelayPolicy != nil {
		if a.CheckRoute == nil {
			return VerifyActivityResult{}, temporal.NewApplicationError(
				fmt.Sprintf("this run names review route %q but this Worker has no routes: config", input.ReviewRelayPolicy.Route),
				RelayConfigurationFailureType,
			)
		}
		if input.ReviewRelayPolicy.Route == "" {
			return VerifyActivityResult{}, temporal.NewApplicationError(
				"this run's review policy names no route, but this Worker holds a routes: config and refuses a routeless review policy",
				RelayConfigurationFailureType,
			)
		}
		creds, err := a.checkAndResolveRoute(relayRoleReview, *input.ReviewRelayPolicy, input.ReviewThinking)
		if err != nil {
			return VerifyActivityResult{}, temporal.NewApplicationError(
				fmt.Sprintf("bind review route %q: %v", input.ReviewRelayPolicy.Route, err),
				RelayConfigurationFailureType,
			)
		}
		if err := input.ReviewRelayPolicy.ValidateUpstreamScheme(); err != nil {
			return VerifyActivityResult{}, temporal.NewApplicationError(fmt.Sprintf("review relay upstream: %v", err), RelayConfigurationFailureType)
		}
		review = codereview.ReviewRelayFromPolicy(*input.ReviewRelayPolicy, creds.APIKey, creds.GitHubToken, creds.ChatGPTToken, creds.ChatGPTAccountID)
	}
	relaySpec := codereview.PhaseRelaySpec(*baseRelaySpec, review, input.PriorConsumedInputTokens+input.PriorConsumedOutputTokens, input.PriorConsumedCostMicroUSD)
	if err := relaySpec.Validate(); err != nil {
		return VerifyActivityResult{}, temporal.NewApplicationError(fmt.Sprintf("%s relay configuration: %v", step.Label, err), RelayConfigurationFailureType)
	}
	if err := a.capRelayForAttempt(ctx, input.RunWorkflowInput, &relaySpec); err != nil {
		return VerifyActivityResult{}, err
	}

	// Same two-phase intent protocol as RunBuildActivity, closing the same
	// crash window for this subprocess: see RunBuildActivity's matching
	// comment for why this halts instead of re-running or trusting an
	// unconfirmed result. priorAttemptRecords reads records keyed by this
	// Activity's own ActivityID (info.ActivityID), which RunWorkflow's own two separate
	// ExecuteActivity calls (one per enabled step) each get their own of
	// -- so the two steps' checkpoints/journals/intents never collide,
	// even though they now share one Activity implementation (see
	// TestRunReviewStepActivityChecksInIndependentlyPerStep).
	intentFound, journalFound, err := priorAttemptRecords(ctx, a.checkpointDirFor(input.RunWorkflowInput))
	if err != nil {
		return VerifyActivityResult{}, priorRecordsFailure(step.Label, err)
	}
	if intentFound {
		return VerifyActivityResult{}, temporal.NewApplicationError(
			fmt.Sprintf("a prior attempt of this %s Activity recorded intent to invoke %s but crashed before reaching a durable checkpoint — halting rather than risk a duplicate invocation or trusting an unconfirmed result", step.Label, step.ScriptName),
			AmbiguousPriorAttemptType,
		)
	}
	if journalFound {
		return VerifyActivityResult{}, temporal.NewApplicationError(
			fmt.Sprintf("a %s Activity attempt journal exists without its intent or completed checkpoint — halting rather than risk a duplicate invocation", step.Label),
			AmbiguousPriorAttemptType,
		)
	}

	// SC-019: the review reads the instruction files as the base commit
	// holds them, and is shown what the build did to them as data. An
	// error here does not launch the review.
	// A prompt already in this step's session folder was not saved by this
	// launch: the build agent could have written it. It is removed, or the
	// step fails.
	prep, err := a.prepareReviewLaunchHeartbeating(ctx, input, step, filepath.Join(filepath.Dir(activityExecutionLogPath(ctx, a.logDirFor(input.RunWorkflowInput), step.Name+".log")), step.Name+".instructions"))
	defer func() { _ = prep.finish(ctx) }()
	if err != nil {
		return VerifyActivityResult{}, err
	}
	script := filepath.Join(filepath.Dir(a.buildAppScriptFor(input.RunWorkflowInput)), step.ScriptName)
	args := prep.args(reviewStepArgs(input, script))
	buildAppInterpreter := a.buildAppInterpreterFor(input.RunWorkflowInput)
	command := append([]string{buildAppInterpreter}, args...)
	if _, err := recordActivityIntent(ctx, a.checkpointDirFor(input.RunWorkflowInput), step.Name, command); err != nil {
		return VerifyActivityResult{}, temporal.NewApplicationErrorWithCause(fmt.Sprintf("record %s Activity intent", step.Label), InfrastructureFailureType, err)
	}

	logPath := activityLogPath(activityExecutionLogPath(ctx, a.logDirFor(input.RunWorkflowInput), step.Name+".log"))
	// Earlier Temporal attempts' evidence reaches the result and checkpoint only;
	// attempts (and so this attempt's journal) holds this attempt's own.
	inherited := a.earlierAttemptsFor(ctx, a.checkpointDirFor(input.RunWorkflowInput), step.Name)
	attempts := []run.Attempt{}
	beforeAttempt := func(attempt int) error {
		_, err := recordActivityAttemptIntent(ctx, a.checkpointDirFor(input.RunWorkflowInput), attempt, step.Name, command, time.Now().UTC().Format(time.RFC3339))
		return err
	}
	beforeAttempt = a.leaseChecked(ctx, a.checkpointDirFor(input.RunWorkflowInput), beforeAttempt)
	afterAttempt := func(attempt int, res runner.Result, _ error) error {
		attempts = append(attempts, run.Attempt{
			Kind: step.Name, Command: res.Command,
			StartedAt: res.StartedAt.Format(time.RFC3339), FinishedAt: res.FinishedAt.Format(time.RFC3339),
			ExitCode: res.ExitCode, LogPath: logPath(attempt), ImageDigest: res.ImageDigest,
			HarnessScriptsSHA256: res.ScriptsSHA256,
			Skills:               res.Skills, SkillsSHA256: res.SkillsSHA256, RepoSkills: res.RepoSkills,
			RelayImageDigest: res.RelayImageDigest, RelayNetwork: res.RelayNetworkName,
			RelayContainerName: res.RelayContainerName, RelayUpstream: res.RelayUpstream,
			RelayCredentialMode:         res.RelayCredentialMode,
			RelayRoute:                  res.RelayRoute,
			RelayBilling:                res.RelayBilling,
			RelayWorkerModelID:          res.RelayWorkerModelID,
			RelayReasoningEffort:        res.RelayReasoningEffort,
			RelayReasoningEffortAnomaly: res.RelayReasoningEffortAnomaly,
			RelayConsumedInputTokens:    res.RelayConsumedInputTokens, RelayConsumedOutputTokens: res.RelayConsumedOutputTokens,
			RelayConsumedCostMicroUSD: res.RelayConsumedCostMicroUSD, RelayCeilingExceeded: res.RelayCeilingExceeded,
			RelaySpendPartial: res.RelaySpendPartial, RelayCalls: run.AttemptCalls(res.RelayCalls),
			// Role/Thinking: roles.review's own resolved values, shared by
			// both steps (input.ReviewThinking). ExpectedEffort:
			// relaySpec.WorkerModelExtraJSON is this step's own launched
			// model JSON (PhaseRelaySpec above, not baseRelaySpec) -- see
			// run.ExpectedReasoningEffort's own doc comment.
			Role:           run.AttemptRoleReview,
			Harness:        harnessArg(input.ReviewHarness),
			Thinking:       input.ReviewThinking,
			ExpectedEffort: run.ExpectedReasoningEffort(input.ReviewThinking, relaySpec.WorkerModelExtraJSON),
		})
		prep.record(&attempts[len(attempts)-1])
		return saveActivityAttemptJournal(ctx, a.checkpointDirFor(input.RunWorkflowInput), step.Name, attempts)
	}
	// registrySpec/composeSpec deliberately nil: this phase only ever reads
	// the already-committed workspace and calls the model through relay,
	// so relaunching this run's own compose services or attaching a
	// registry proxy for it would just be wasted work with nothing to
	// reach it.
	heartbeatStart := time.Now()
	result, runErr := heartbeatWhileRunning(activityHeartbeatInterval, func() {
		recordStepHeartbeat(ctx, HeartbeatDetails{Stage: step.Stage, Elapsed: time.Since(heartbeatStart)})
	}, func() (runner.Result, error) {
		if a.hasFakeRunner() {
			return a.runWithRetriesFn()(ctx, input.WorkspacePath, logPath, 1, beforeAttempt, afterAttempt, buildAppInterpreter, args...)
		}
		skills, err := a.boundSkills(relayRoleReview, input.RunWorkflowInput.ReviewSkills)
		if err != nil {
			return runner.Result{}, err
		}
		return a.runSandboxWithRetries(relaunchUnserved(relaunchUnstartedOnly(withReviewSpec(prep.launchContext(ctx), input.RunWorkflowInput))), input.RunWorkflowInput, logPath, a.verifyMaxAttemptsFor(input.RunWorkflowInput), beforeAttempt, afterAttempt, &relaySpec, nil, nil, "", "", reviewHarnessEnv(input.RunWorkflowInput), skills, buildAppInterpreter, args...)
	})
	// Immediately after the launch, before the clean check below: the stubs
	// a mask over an absent path needed, and the snapshot's scratch.
	finishErr := prep.finish(ctx)
	a.retainReviewPrompts(ctx, input, step)
	return a.completeReviewStep(ctx, input, step, checkpoint, path, result, runErr, finishErr, withInherited(inherited, attempts))
}

// reviewStepArgs builds the review launch's harness-script argv.
// input.SpecPath is already unconditionally staged by
// a.runSandboxWithRetries (specPath) for every review-step launch; the
// approved spec (input.ReviewSpecPath, a run with -review-spec) is staged
// beside it by withReviewSpec, and --reproductions is passed, only when the
// build holds reproductions (input.ReviewReproduction): the spec is sent to
// the reviewer only to be cited. Otherwise every finding is advice and the
// reviewer sees neither the numbered spec nor the reproduction rules. input.effectiveDiffBase(), not
// input.BaseSHA: see RunWorkflowInput.DiffBaseSHA's own doc comment -- a
// corrective round's own BaseSHA is just its checkout point, not the
// cumulative diff a -diff-base override names for this review to read.
func reviewStepArgs(input ReviewStepInput, script string) []string {
	return reviewstep.CombinedArgs(script, input.WorkspacePath, input.SpecPath, reviewSpecPathFor(input.RunWorkflowInput), input.CodeReviewPolicy, input.effectiveDiffBase(), input.ReviewThinking, harnessArg(input.ReviewHarness), input.ReviewReproduction)
}

// reviewSpecPathFor is the approved spec the review launch is given: the
// run's snapshot when the build holds reproductions (the reviewer cites its
// lines), empty otherwise (nothing to cite).
func reviewSpecPathFor(input RunWorkflowInput) string {
	if !input.ReviewReproduction {
		return ""
	}
	return input.ReviewSpecPath
}

// withReviewSpec stages the approved spec (reviewSpecPathFor) beside the
// ticket's spec for the review launch, which reads it as --approved-spec; ctx
// unchanged when there is none.
func withReviewSpec(ctx context.Context, input RunWorkflowInput) context.Context {
	path := reviewSpecPathFor(input)
	if path == "" {
		return ctx
	}
	return withExtraRunInputs(ctx, path)
}

// retiredReviewStep reports whether input is a review a retired plan
// scheduled: a step other than reviewstep.Combined, or a combined review
// carrying acceptance criteria. Only legacyReviewPlan (a history recorded
// before reviewReproductionChange) schedules the combined review with
// SpecAcceptanceCriteria set, since nothing sets it since; that launch also
// ran the retired spec-conformity task, so this Worker cannot run it.
func retiredReviewStep(input ReviewStepInput) bool {
	return input.Step != reviewstep.Combined || input.SpecAcceptanceCriteria != ""
}

// reviewStepNoRelayError names the review's trigger condition for the
// no-relay-configured halt.
func reviewStepNoRelayError(input ReviewStepInput) string {
	return fmt.Sprintf("-code-review-policy is %q but no relay is configured for this run -- code review needs model access and cannot be silently skipped", input.CodeReviewPolicy)
}

// reviewProgressDetail is the progress feed's detail for a review that
// ended with err: the fixed sentence of a ReviewInstructionsFailure, nothing
// otherwise.
func reviewProgressDetail(err error) string {
	var appErr *temporal.ApplicationError
	if errors.As(err, &appErr) && appErr.Type() == ReviewInstructionsFailureType {
		return ReviewInstructionsFailureMessage
	}
	return ""
}

// failReviewInstructions ends a review whose masks, stubs or cleanup failed
// (SC-019): the fixed message and failure type, the cleaned cause on the
// attempt, and the same durable checkpoint as any other failed review.
func (a *Activities) failReviewInstructions(ctx context.Context, step reviewstep.Step, checkpoint activityCheckpoint[VerifyActivityResult], path string, stepResult VerifyActivityResult, cause error) (VerifyActivityResult, error) {
	failure := reviewInstructionsFailureOf(step.Name, cause, stepResult.Attempts)
	stepResult.Attempts = AttemptsFromError(failure)
	checkpoint.Result = stepResult
	checkpoint.Error, checkpoint.ErrorType = ReviewInstructionsFailureMessage, ReviewInstructionsFailureType
	if saveErr := saveActivityCheckpoint(path, checkpoint, activity.GetInfo(ctx).Attempt); saveErr != nil {
		return stepResult, temporal.NewApplicationErrorWithCause(fmt.Sprintf("save %s Activity checkpoint", step.Label), InfrastructureFailureType, saveErr, stepResult.Attempts)
	}
	return stepResult, failure
}

// completeReviewStep turns a review launch's outcome into the Activity's
// result: the evidence checks, the durable checkpoint and the error type.
// finishErr is the failure to remove the launch's stubs, which is an
// infrastructure failure of its own.
func (a *Activities) completeReviewStep(ctx context.Context, input ReviewStepInput, step reviewstep.Step, checkpoint activityCheckpoint[VerifyActivityResult], path string, result runner.Result, runErr, finishErr error, attempts []run.Attempt) (VerifyActivityResult, error) {
	stepResult := VerifyActivityResult{Result: result, Attempts: attempts}
	if cause := reviewInstructionsCause(runErr, finishErr); cause != nil {
		return a.failReviewInstructions(ctx, step, checkpoint, path, stepResult, cause)
	}
	var err error
	if runErr == nil {
		stepResult.DurationMs = result.FinishedAt.Sub(result.StartedAt).Milliseconds()
		err = finishErr
		if err == nil {
			stepResult.LogSHA256, err = evidence.SHA256File(result.LogPath)
		}
		if err == nil {
			// A full pi agent with tool access runs this phase against the
			// rw workspace -- see run_ticket.go's own matching comment for
			// why this must halt, not fold into a further safety-net
			// commit, if it somehow left the workspace dirty. Its job is
			// to read and report, never to edit.
			var clean bool
			clean, err = runner.GitIsClean(input.WorkspacePath)
			if err == nil && !clean {
				err = fmt.Errorf("%s left the workspace dirty -- refusing to auto-commit reviewer-authored output", step.Label)
			}
		}
	}
	if runErr == nil && err == nil && reviewStepPassed(result.ExitCode) {
		// SC-022: the reproductions are held from the clean worktree the
		// review judged, before the checkpoint records the review, and only
		// when the launch gave a verdict: a review with no verdict holds
		// nothing.
		var holdErr error
		if stepResult.Reproductions, holdErr = a.holdReproductions(ctx, input.RunWorkflowInput); holdErr != nil {
			return a.failHold(ctx, step.Label, checkpoint, path, stepResult, holdErr)
		}
	}
	errType := launchErrorType(stepErr(ctx, runErr))
	if runErr != nil {
		checkpoint.Error = fmt.Sprintf("%s subprocess infrastructure failure: %v", step.Label, runErr)
		checkpoint.ErrorType = checkpointErrorType(runErr)
	} else if err != nil {
		checkpoint.Error = fmt.Sprintf("%s evidence infrastructure failure: %v", step.Label, err)
	}
	checkpoint.Result = stepResult
	if saveErr := saveActivityCheckpoint(path, checkpoint, activity.GetInfo(ctx).Attempt); saveErr != nil {
		return stepResult, temporal.NewApplicationErrorWithCause(fmt.Sprintf("save %s Activity checkpoint", step.Label), errType, saveErr, stepResult.Attempts)
	}
	if runErr != nil {
		return stepResult, temporal.NewApplicationErrorWithCause(fmt.Sprintf("%s subprocess infrastructure failure", step.Label), errType, runErr, stepResult.Attempts)
	}
	if err != nil {
		return stepResult, temporal.NewApplicationErrorWithCause(fmt.Sprintf("%s evidence infrastructure failure", step.Label), InfrastructureFailureType, err, stepResult.Attempts)
	}
	return stepResult, nil
}
