package workflow

import (
	"buildgate/internal/codereview"
	"buildgate/internal/conformity"
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

// reviewStepPassed reports whether a review step's launch passed, for its
// progress feed's end outcome. The combined step exits
// reviewstep.CombinedExitBase + bits, so its pass is 40, not 0; reading it
// as "exit != 0 fails" marked every passing combined review failed on the
// console's Timeline (found in a 2026-09-29 walkthrough) while
// the gates, which decode it with GateExitCodes, passed. Mirrors the
// cmd/factoryd's decoding.
func reviewStepPassed(stepName string, exitCode int) bool {
	if stepName == reviewstep.Combined {
		conformity, codeReview := reviewstep.GateExitCodes(exitCode)
		return conformity == 0 && codeReview == 0
	}
	return exitCode == 0
}

// RunReviewStepActivity invokes one of the two model-backed review-only
// phases named by input.Step (reviewstep.Steps: the per-criterion
// spec-conformity review, agent/pi/scripts/conformity_review.py, or the
// free-form standalone code review, agent/pi/scripts/code_review.py)
// through runner.RunWithRetries -- the Temporal path's own counterpart to
// cmd/factoryd's runReviewPhase closure (run_ticket.go). ONE
// Activity implementation for both steps (M4-K3): before this
// unification, RunSpecConformityReviewActivity and RunCodeReviewActivity
// were a second, near-identical copy of each other -- the exact "fixed
// one copy, not the other" shape run_ticket.go's own runReviewPhase doc
// comment already warns about, just on the Temporal side of the fence.
//
// RunWorkflow only calls this once CollectEvidenceActivity's own
// safety-net commit has already landed, for either step, so the workspace
// is guaranteed clean and the reviewer has a real commit to evaluate a
// commit-related criterion against -- see RunReviewStepActivityName's own
// doc comment for the full incident this exists to fix. Reuses
// VerifyActivityResult (not a new result type): this Activity's own
// output shape (a subprocess result, duration, log hash, attempts) is
// identical to RunVerifyActivity's.
func (a *Activities) RunReviewStepActivity(ctx context.Context, input ReviewStepInput) (progressResult VerifyActivityResult, err error) {
	// reviewstep.CombinedStep is deliberately not in reviewstep.Steps
	// (ByName never finds it -- see that function's own doc comment), so
	// it is checked for explicitly here before falling back to ByName for
	// the two standalone steps.
	step := reviewstep.CombinedStep
	if input.Step != reviewstep.Combined {
		var ok bool
		step, ok = reviewstep.ByName(input.Step)
		if !ok {
			return VerifyActivityResult{}, temporal.NewApplicationError(fmt.Sprintf("unknown review step %q", input.Step), InfrastructureFailureType)
		}
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
		if err != nil || !reviewStepPassed(step.Name, progressResult.Result.ExitCode) {
			outcome = "fail"
		}
		progressMark(ctx, a.logDirFor(input.RunWorkflowInput), step.Stage, "end", outcome, "")
	}()
	if err := a.fenceEarlierAttempts(ctx, input.RunWorkflowInput, true); err != nil {
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
	// included, once review.Upstream != "" (see conformity.PhaseRelaySpec's
	// own doc comment) -- so this review's own success must never depend
	// on resolving the BUILD's own route's credential at all: only
	// a.CheckRoute (no ResolveRouteCredentials) confirms input.RoutePolicy
	// still binds to this Worker's own config, and baseRelaySpec is built
	// with NO credential (only its ceiling/budget fields are ever read
	// from it once review.Upstream != ""). See
	// TestConformityReviewDoesNotResolveBuildRouteCredential.
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
		return VerifyActivityResult{}, temporal.NewApplicationError(reviewStepNoRelayError(step, input), RelayConfigurationFailureType)
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
	var review *conformity.ReviewRoute
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
		review = conformity.ReviewRelayFromPolicy(*input.ReviewRelayPolicy, creds.APIKey, creds.GitHubToken, creds.ChatGPTToken, creds.ChatGPTAccountID)
	}
	relaySpec := conformity.PhaseRelaySpec(*baseRelaySpec, review, input.PriorConsumedInputTokens+input.PriorConsumedOutputTokens, input.PriorConsumedCostMicroUSD)
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

	script := filepath.Join(filepath.Dir(a.buildAppScriptFor(input.RunWorkflowInput)), step.ScriptName)
	args := reviewStepArgs(a, step, input, script)
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
			RelaySpendPartial: res.RelaySpendPartial,
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
		return saveActivityAttemptJournal(ctx, a.checkpointDirFor(input.RunWorkflowInput), step.Name, attempts)
	}
	// registrySpec/composeSpec deliberately nil: this phase only ever reads
	// the already-committed workspace and calls the model through relay,
	// so relaunching this run's own compose services or attaching a
	// registry proxy for it would just be wasted work with nothing to
	// reach it -- see run_ticket.go's matching sandboxedForConformityReview
	// comment.
	heartbeatStart := time.Now()
	result, runErr := heartbeatWhileRunning(activityHeartbeatInterval, func() {
		activity.RecordHeartbeat(ctx, HeartbeatDetails{Stage: step.Stage, Elapsed: time.Since(heartbeatStart)})
	}, func() (runner.Result, error) {
		if a.hasFakeRunner() {
			return a.runWithRetriesFn()(ctx, input.WorkspacePath, logPath, 1, beforeAttempt, afterAttempt, buildAppInterpreter, args...)
		}
		skills, err := a.boundSkills(relayRoleReview, input.RunWorkflowInput.ReviewSkills)
		if err != nil {
			return runner.Result{}, err
		}
		return a.runSandboxWithRetries(ctx, input.RunWorkflowInput, logPath, 1, beforeAttempt, afterAttempt, &relaySpec, nil, nil, "", "", reviewHarnessEnv(input.RunWorkflowInput), skills, buildAppInterpreter, args...)
	})
	stepResult := VerifyActivityResult{Result: result, Attempts: withInherited(inherited, attempts)}
	if runErr == nil {
		stepResult.DurationMs = result.FinishedAt.Sub(result.StartedAt).Milliseconds()
		stepResult.LogSHA256, err = evidence.SHA256File(result.LogPath)
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
	if runErr != nil {
		checkpoint.Error = fmt.Sprintf("%s subprocess infrastructure failure: %v", step.Label, runErr)
		if errors.Is(runErr, sandbox.ErrCleanupUnconfirmed) {
			checkpoint.ErrorType = CleanupUnconfirmedFailureType
		}
	} else if err != nil {
		checkpoint.Error = fmt.Sprintf("%s evidence infrastructure failure: %v", step.Label, err)
	}
	checkpoint.Result = stepResult
	if saveErr := saveActivityCheckpoint(path, checkpoint, activity.GetInfo(ctx).Attempt); saveErr != nil {
		errType := InfrastructureFailureType
		if errors.Is(runErr, sandbox.ErrCleanupUnconfirmed) {
			errType = CleanupUnconfirmedFailureType
		}
		return stepResult, temporal.NewApplicationErrorWithCause(fmt.Sprintf("save %s Activity checkpoint", step.Label), errType, saveErr, stepResult.Attempts)
	}
	if runErr != nil {
		errType := InfrastructureFailureType
		if errors.Is(runErr, sandbox.ErrCleanupUnconfirmed) {
			errType = CleanupUnconfirmedFailureType
		}
		return stepResult, temporal.NewApplicationErrorWithCause(fmt.Sprintf("%s subprocess infrastructure failure", step.Label), errType, runErr, stepResult.Attempts)
	}
	if err != nil {
		return stepResult, temporal.NewApplicationErrorWithCause(fmt.Sprintf("%s evidence infrastructure failure", step.Label), InfrastructureFailureType, err, stepResult.Attempts)
	}
	return stepResult, nil
}

// reviewStepArgs builds step's own harness-script argv, keyed on
// step.Name -- the one place the two review steps' own flag/field
// differences (spec-acceptance-criteria vs. spec, conformity policy vs.
// code-review policy) live, now that RunReviewStepActivity's own body is
// shared (M4-K3).
func reviewStepArgs(a *Activities, step reviewstep.Step, input ReviewStepInput, script string) []string {
	if step.Name == reviewstep.Combined {
		// input.SpecPath and input.SpecAcceptanceCriteria: both are
		// ALREADY unconditionally staged by a.runSandboxWithRetries below
		// (specPath and extraRunInput respectively -- see that function's
		// own doc comment on extraRunInput), for every review-step
		// Activity launch, not just the Combined one -- so this needs no
		// staging change of its own, unlike cmd/factoryd
		// (which threads extraRunInput per call site). Same
		// effectiveDiffBase()/policy-fallback reasoning as the two
		// standalone branches below.
		return reviewstep.CombinedArgs(script, input.WorkspacePath, input.SpecPath, input.SpecAcceptanceCriteria, a.conformityPolicyFor(input.RunWorkflowInput), input.CodeReviewPolicy, input.effectiveDiffBase(), input.ReviewThinking, harnessArg(input.ReviewHarness))
	}
	if step.Name == reviewstep.CodeReview {
		// input.SpecPath, the same host path build_app.py's own --spec
		// already stages (buildActivityArgs) -- unlike conformity_review.py,
		// code_review.py takes a real --spec of its own (context only, per
		// its own doc comment), so this reuses the ticket spec's already-
		// staged mount instead of a second, code-review-specific file.
		// input.effectiveDiffBase(), not input.BaseSHA: see
		// RunWorkflowInput.DiffBaseSHA's own doc comment — a corrective
		// round's own BaseSHA is just its checkout point, not the
		// cumulative diff a -diff-base override names for this review to
		// read.
		return codereview.Args(script, input.WorkspacePath, input.SpecPath, input.CodeReviewPolicy, input.effectiveDiffBase(), input.ReviewThinking, harnessArg(input.ReviewHarness))
	}
	// input.SpecAcceptanceCriteria, not a.specAcceptanceCriteriaFor(...):
	// RunWorkflow's own decision to invoke this step at all (see its
	// specConformityVersion gate) can only see input.SpecAcceptanceCriteria
	// -- a deterministic Temporal workflow cannot read this Worker's own
	// static Activities.SpecAcceptanceCriteria fallback without risking
	// replay divergence if a different Worker with a different static
	// default later replays the same history. Resolving the Worker-static
	// fallback here, after the workflow already decided not to run this
	// step because the raw field was empty, would silently review against
	// criteria the workflow never agreed to check. A ticket that wants
	// review against a Worker-level default must still set
	// -spec-acceptance-criteria per run.
	// input.effectiveDiffBase(), not input.BaseSHA: same reasoning as the
	// code-review branch above.
	return conformity.Args(script, input.WorkspacePath, input.SpecAcceptanceCriteria, a.conformityPolicyFor(input.RunWorkflowInput), input.effectiveDiffBase(), input.ReviewThinking, harnessArg(input.ReviewHarness))
}

// reviewStepNoRelayError names step's own trigger condition for the
// no-relay-configured halt -- each step names its own reason in this
// halt's text, so it stays specific rather than a single shared string
// (mirrors run_ticket.go's own runReviewPhase noRelayErr parameter).
func reviewStepNoRelayError(step reviewstep.Step, input ReviewStepInput) string {
	if step.Name == reviewstep.Combined {
		return "ticket declares -spec-acceptance-criteria and -code-review-policy is set, but no relay is configured for this run -- the combined review needs model access and cannot be silently skipped"
	}
	if step.Name == reviewstep.CodeReview {
		return fmt.Sprintf("-code-review-policy is %q but no relay is configured for this run -- code review needs model access and cannot be silently skipped", input.CodeReviewPolicy)
	}
	return "ticket declares -spec-acceptance-criteria but no relay is configured for this run -- the spec-conformity review needs model access and cannot be silently skipped"
}
