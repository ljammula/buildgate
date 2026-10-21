package workflow

import (
	"errors"
	"fmt"
	"time"

	temporalworkflow "go.temporal.io/sdk/workflow"

	"buildgate/internal/codereview"
	"buildgate/internal/policy"
	"buildgate/internal/reviewstep"
	"buildgate/internal/run"
)

// The functions below are RunWorkflow's phases, moved out of it whole. Each
// issues the same SDK calls, in the same order and under the same conditions,
// that RunWorkflow issued inline; a phase that fails returns the error and
// RunWorkflow returns an empty result with it, as before. None may hold a
// defer: RunWorkflow's deferred rollback reads its named results.

// widenForTimeoutMinutes returns options with StartToCloseTimeout widened for a
// configured -timeout-minutes.
//
// Widen StartToCloseTimeout for a configured -timeout-minutes beyond
// what the flat ActivityStartToCloseTimeout default covers (found via
// review): sandbox.LaunchSpec's own per-attempt deadline is already
// derived from this Activity's own context deadline, but that context
// was still bounded by the flat hour regardless — a build/verify
// configured to run longer was killed at ~59m50s no matter what
// -timeout-minutes said, sandboxed or not. Same effectiveTimeout formula as cmd/factoryd's realMain, so the configured budget means the same.
func widenForTimeoutMinutes(options temporalworkflow.ActivityOptions, timeoutMinutes int) temporalworkflow.ActivityOptions {
	if configured := time.Duration(timeoutMinutes+5) * time.Minute; timeoutMinutes > 0 && configured > options.StartToCloseTimeout {
		options.StartToCloseTimeout = configured
	}
	return options
}

// buildToVerdict runs everything after the pre-build checks: build, verify,
// the gates, evidence, the review steps, the oracle commit and the policy
// verdict, in that order.
func buildToVerdict(ctx temporalworkflow.Context, ids *activityIDs, buildVerifyCtx temporalworkflow.Context, buildVerifyActivityOptions temporalworkflow.ActivityOptions, input *RunWorkflowInput, current *RunProgress, result *RunWorkflowResult, worktreePath, branch string) error {
	verify, err := buildAndVerify(ctx, ids, buildVerifyCtx, *input, current, result, worktreePath, branch)
	if err != nil {
		return err
	}

	gates, err := postVerifyGates(ctx, ids, buildVerifyCtx, buildVerifyActivityOptions, input, current, result, worktreePath, branch)
	if err != nil {
		return err
	}

	evidenceResult, err := collectRunEvidence(ctx, ids, *input, current, result, gates, worktreePath, branch)
	if err != nil {
		return err
	}

	reviews, err := runReviewSteps(ctx, ids, buildVerifyCtx, *input, current, result, gates, worktreePath, branch)
	if err != nil {
		return err
	}

	if err = oracleCommitPhase(ctx, ids, buildVerifyActivityOptions, *input, current, result, worktreePath, branch, &gates, &evidenceResult, &verify, reviews); err != nil {
		return err
	}

	if err = evaluateRun(ctx, ids, *input, current, result, worktreePath, branch, verify, evidenceResult, gates, reviews); err != nil {
		return err
	}
	return nil
}

// captureRunBase records the commit this run is evaluated against
// (CaptureBaseSHAActivity) and checks the declared predecessor chain.
//
// CaptureBaseSHAActivity's own fresh read of the workspace's current
// HEAD — not input.BaseSHA, the caller's own pre-submission
// observation — is what this run is actually evaluated against from
// here on. Found via review: a caller that queues multiple runs
// against the same workspace (exactly what -repository's
// RepositoryOwnerWorkflow submission exists to let separate
// invocations do safely) captures its own BaseSHA once, before ever
// being granted its serialized turn — by the time an earlier-queued
// run's build has actually advanced the workspace, that captured
// value is stale, and every downstream check that diffs against it
// (PostBuildActivity's ancestor check, CollectEvidenceActivity's
// changed-file inventory and required-content evidence, and
// build_app.py's own --review-base-sha, since input.BaseSHA is
// overwritten here before RunBuildActivity ever sees it) would
// misattribute the *earlier* run's changes as this run's own. A
// caller with no queueing ahead of it (the common case today) simply
// gets back the same value it already had.
func captureRunBase(ctx temporalworkflow.Context, ids *activityIDs, input *RunWorkflowInput, current *RunProgress, result *RunWorkflowResult, projectPath string, isolatePriorResult bool) error {
	var actualBaseSHA string
	captureInput := CaptureBaseSHAInput{WorkspacePath: input.WorkspacePath, OnBranch: input.OnBranch}
	if isolatePriorResult {
		captureInput = CaptureBaseSHAInput{
			WorkspacePath:       input.WorkspacePath,
			ProjectPath:         projectPath,
			UsePriorResultSHA:   true,
			PriorRunID:          input.PriorRunID,
			PriorRunState:       input.PriorRunState,
			PriorRunProjectPath: input.PriorRunProjectPath,
			PriorRunResultSHA:   input.PriorRunResultSHA,
		}
	}
	current.Stage, current.StartedAt = "capture_base_sha", temporalworkflow.Now(ctx)
	if err := temporalworkflow.ExecuteActivity(ids.with(ctx, "capture-base-sha"), CaptureBaseSHAActivityName, captureInput).Get(ctx, &actualBaseSHA); err != nil {
		return fmt.Errorf("capture base sha activity: %w", err)
	}
	input.BaseSHA = actualBaseSHA
	if input.ResumeFrom != nil && input.ResumeFrom.BaseSHA != "" {
		// A resumed run continues the halted run's branch, so its gates
		// diff against the commit that branch started from, not the
		// shared checkout's HEAD as of the resume.
		input.BaseSHA = input.ResumeFrom.BaseSHA
	}
	result.BaseSHA = input.BaseSHA

	// Ported from cmd/factoryd's realMain — see run.FindChainSuccessor's own
	// doc comment for why this is needed at all: an isolated successor
	// starts its own fresh worktree directly from its declared
	// predecessor's recorded ResultSHA, not the shared checkout's HEAD, so
	// the shared checkout's HEAD never moves for that slice.
	// ValidateSliceChainActivity's baseSHA comparison against live HEAD is
	// unconditionally true against a since-superseded predecessor from
	// then on — not just for another isolated successor, but for *any*
	// later run that declares the same predecessor, isolated or not, since
	// its own freshly-captured baseSHA is HEAD, which the isolated
	// successor never advanced. Gated on PriorRunID alone, not
	// isolatePriorResult (found via review, GitHub Codex App): scoping
	// this to only a current isolated run left exactly that mixed-chain
	// case unprotected -- accepted run A advances shared HEAD, isolated
	// run B supersedes A without moving it, then a later run C (isolated
	// or not) declaring A as its own prior captures a baseSHA that still
	// equals A's ResultSHA and would pass ValidateSliceChainActivity
	// untouched, silently reopening the chain and discarding B's accepted
	// work. Matches realMain's own unconditional (on *priorRun != "" alone)
	// placement of this same check.
	if input.PriorRunID != "" {
		current.Stage, current.StartedAt = "check_chain_successor", temporalworkflow.Now(ctx)
		if err := temporalworkflow.ExecuteActivity(ids.with(ctx, "check-chain-successor"), CheckChainSuccessorActivityName, CheckChainSuccessorInput{
			DataDir:     input.DataDir,
			ProjectPath: projectPath,
			PriorRunID:  input.PriorRunID,
		}).Get(ctx, nil); err != nil {
			return wrapActivityFailure("check chain successor activity", err, nil, result.BaseSHA, result.CommittedByWorker, "", "")
		}
	}

	// Ported from cmd/factoryd's realMain — see ValidateSliceChainActivity's
	// own doc comment for why this validates against input.BaseSHA (just
	// overwritten above with this run's real execution-time value) rather
	// than trusting whatever the submitting process observed before this
	// run was ever queued. Skipped for an isolated predecessor chain
	// (isolatePriorResult): that case validates after preparing the
	// predecessor-based worktree instead — see the isolation block below.
	if input.PriorRunID != "" && !isolatePriorResult {
		current.Stage, current.StartedAt = "validate_slice_chain", temporalworkflow.Now(ctx)
		if err := temporalworkflow.ExecuteActivity(ids.with(ctx, "validate-slice-chain"), ValidateSliceChainActivityName, ValidateSliceChainInput{
			WorkspacePath:       input.WorkspacePath,
			BaseSHA:             input.BaseSHA,
			PriorRunID:          input.PriorRunID,
			PriorRunState:       input.PriorRunState,
			PriorRunProjectPath: input.PriorRunProjectPath,
			PriorRunResultSHA:   input.PriorRunResultSHA,
		}).Get(ctx, nil); err != nil {
			// wrapActivityFailure, not a plain fmt.Errorf %w wrap (found
			// via review): only a *temporal.ApplicationError at the very
			// top of RunWorkflow's own returned error survives Temporal's
			// cross-process serialization of a Workflow's failure with its
			// Details intact. result.BaseSHA was just overwritten above
			// with this run's real execution-time value — without
			// wrapActivityFailure re-attaching it here, a -repository-routed
			// halt on this specific check would fall back to
			// runViaRepositoryOwner's own stale pre-queue baseSHA instead,
			// reintroducing the exact "wrong baseline recorded" bug class
			// this mechanism exists to close for every other Activity in
			// this function.
			return wrapActivityFailure("validate slice chain activity", err, nil, result.BaseSHA, result.CommittedByWorker, "", "")
		}
	}
	return nil
}

// buildAndVerify runs the build, the post-build ancestor check and safety-net
// commit, and canonical verification, and returns verify's result.
func buildAndVerify(ctx temporalworkflow.Context, ids *activityIDs, buildVerifyCtx temporalworkflow.Context, input RunWorkflowInput, current *RunProgress, result *RunWorkflowResult, worktreePath, branch string) (VerifyActivityResult, error) {
	// RunBuildActivity's successful result is BuildActivityResult, carrying
	// per-attempt evidence alongside the flat runner.Result.
	var build BuildActivityResult
	var buildErr error
	current.Stage, current.StartedAt = "build", temporalworkflow.Now(ctx)
	buildErr = temporalworkflow.ExecuteActivity(ids.withRetries(buildVerifyCtx, "build"), RunBuildActivityName, input).Get(ctx, &build)
	if buildErr != nil {
		return VerifyActivityResult{}, wrapActivityFailure("run build activity", buildErr, result.Attempts, result.BaseSHA, result.CommittedByWorker, worktreePath, branch)
	}
	result.Build = build.Result
	result.Attempts = append(result.Attempts, build.Attempts...)

	// Ancestor check + safety-net commit, mirroring cmd/factoryd's direct
	// path: found live (see runner.GitIsAncestor's doc comment) that a
	// real build_app.py/pi harness invocation can reset the workspace
	// backward mid-run and commit on top of that older state, silently
	// discarding already-completed unrelated work if nothing catches it.
	var postBuild PostBuildResult
	postBuildInput := PostBuildInput{
		WorkspacePath: input.WorkspacePath,
		BaseSHA:       input.BaseSHA,
		BuildExitCode: result.Build.ExitCode,
		CheckpointDir: input.CheckpointDir,
		LogDir:        input.LogDir,
		SpecPath:      input.SpecPath,
	}
	current.Stage, current.StartedAt = "post_build", temporalworkflow.Now(ctx)
	if err := temporalworkflow.ExecuteActivity(ids.with(temporalworkflow.WithActivityOptions(ctx, hostGitActivityOptions()), "post-build"), PostBuildActivityName, postBuildInput).Get(ctx, &postBuild); err != nil {
		// build.Attempts (already accumulated into result.Attempts above)
		// would otherwise be lost here too — PostBuildActivity itself never
		// attaches Details since it never collects attempts of its own, but
		// wrapActivityFailure's priorAttempts still carries the build's.
		// ActivityCommittedFromError recovers PostBuildActivity's own
		// commit flag even when its failure happened *after* its own
		// safety-net commit landed (Get never populates postBuild on
		// failure, so result.CommittedByWorker alone — updated only on
		// success below — would otherwise miss it entirely).
		return VerifyActivityResult{}, wrapActivityFailure("post-build activity", err, result.Attempts, result.BaseSHA, result.CommittedByWorker || ActivityCommittedFromError(err), worktreePath, branch)
	}
	// Intermediate only: overwritten below with CollectEvidenceActivity's
	// post-verification value once that's available.
	result.ResultSHA = postBuild.ResultSHA
	result.CommittedByWorker = postBuild.CommittedByWorker

	result.State = run.StateVerifying
	var verify VerifyActivityResult
	current.Stage, current.StartedAt, current.State = "verify", temporalworkflow.Now(ctx), result.State
	if err := temporalworkflow.ExecuteActivity(ids.withRetries(buildVerifyCtx, "verify"), RunVerifyActivityName, input).Get(ctx, &verify); err != nil {
		// result.Attempts (build's, already accumulated above) is passed
		// through as priorAttempts: it would otherwise be lost here too,
		// the same Get semantics as RunBuildActivity's failure above.
		return VerifyActivityResult{}, wrapActivityFailure("run verify activity", err, result.Attempts, result.BaseSHA, result.CommittedByWorker, worktreePath, branch)
	}
	result.Verify = verify.Result
	result.Attempts = append(result.Attempts, verify.Attempts...)
	return verify, nil
}

// gateOutcome is what postVerifyGates found: the full-suite and named-gate
// results the later phases read and the oracle-commit phase may replace.
type gateOutcome struct {
	fullSuite    VerifyActivityResult
	ranFullSuite bool
	// namedGateInputs and namedGatesFailed are the named gates' results.
	namedGateInputs  []policy.NamedGateInput
	namedGatesFailed bool
	// passedReferenceOracleSHA256 is the tree hash the reference_oracle
	// gate ran against, set only when that gate ran AND passed: the pin
	// CommitOraclesActivity verifies against.
	passedReferenceOracleSHA256 string
}

// postVerifyGates runs the full-suite check and the named gates, each only
// once canonical verification passed. It sets input.GateCommands.
func postVerifyGates(ctx temporalworkflow.Context, ids *activityIDs, buildVerifyCtx temporalworkflow.Context, buildVerifyActivityOptions temporalworkflow.ActivityOptions, input *RunWorkflowInput, current *RunProgress, result *RunWorkflowResult, worktreePath, branch string) (gateOutcome, error) {
	// Full-suite regression check (gap 3 of the plan's 2026-08-28 readiness
	// review), ported from cmd/factoryd: optional, and only
	// run once canonical verification itself passed — a run already
	// quarantining on canonical_verify gains nothing from also paying the
	// full suite's cost. Scheduled BEFORE CollectEvidenceActivity below —
	// found via review (GitHub Codex App, PR #33): a full-suite command
	// that itself mutates the checkout (codegen, formatting, snapshot
	// updates, coverage artifacts) and exits 0 could otherwise leave an
	// accepted worktree containing changes absent from
	// CollectEvidenceActivity's ResultSHA/ChangedFiles/diff/dependency/
	// required-content evidence and diff_scope, since that Activity had
	// already run and committed by the time this one executed. Running it
	// first means CollectEvidenceActivity's own commit-if-dirty step
	// covers full-suite's output the same way it already covers
	// verification's own, and every evidence field it collects reflects
	// it.
	var fullSuite VerifyActivityResult
	var ranFullSuite bool
	if input.FullSuiteCommand != "" && result.Build.ExitCode == 0 && result.Verify.ExitCode == 0 {
		current.Stage, current.StartedAt = "full_suite", temporalworkflow.Now(ctx)
		if err := temporalworkflow.ExecuteActivity(ids.withRetries(buildVerifyCtx, "full-suite-verify"), RunFullSuiteVerifyActivityName, *input).Get(ctx, &fullSuite); err != nil {
			return gateOutcome{}, wrapActivityFailure("run full-suite verify activity", err, result.Attempts, result.BaseSHA, result.CommittedByWorker, worktreePath, branch)
		}
		result.Attempts = append(result.Attempts, fullSuite.Attempts...)
		ranFullSuite = true
	}

	// The named gates (lint, security_audit, unit_tests,
	// integration_tests, reference_oracle), as full-suite is: each project-configured
	// command runs here, via RunNamedGateActivityName, only once canonical
	// verification itself passed.
	var namedGateInputs []policy.NamedGateInput
	namedGatesFailed := false
	// The tree hash the reference_oracle gate ran against, set only when that
	// gate ran AND passed: the pin CommitOraclesActivity verifies against.
	var passedReferenceOracleSHA256 string
	if result.Build.ExitCode == 0 && result.Verify.ExitCode == 0 {
		gateChecks, gateCommands := gateChecksToRun(ctx, *input)
		// The commands the gates below ran with are the ones the
		// post-oracle-commit rerun gets: a repository's own gates are
		// rerun only when they ran here.
		input.GateCommands = gateCommands
		for _, check := range gateChecks {
			command := input.GateCommands[check]
			var gateResult VerifyActivityResult
			current.Stage, current.StartedAt = check, temporalworkflow.Now(ctx)
			gateCtx := buildVerifyCtx
			oracleCanary := false
			if check == policy.ReferenceOracleGateID {
				// The gate Activity runs the oracle command and then its runtime
				// canary, each bounded by half the deadline: double the
				// single-command budget, as for the post-oracle-commit verify.
				oracleCanary = true
				oracleGateOptions := buildVerifyActivityOptions
				oracleGateOptions.StartToCloseTimeout *= 2
				gateCtx = temporalworkflow.WithActivityOptions(ctx, oracleGateOptions)
			}
			if err := temporalworkflow.ExecuteActivity(ids.withRetries(gateCtx, "gate-"+check), RunNamedGateActivityName, NamedGateActivityInput{RunWorkflowInput: *input, Check: check, Command: command, OracleCanary: oracleCanary}).Get(ctx, &gateResult); err != nil {
				return gateOutcome{}, wrapActivityFailure("run "+check+" gate activity", err, result.Attempts, result.BaseSHA, result.CommittedByWorker, worktreePath, branch)
			}
			result.Attempts = append(result.Attempts, gateResult.Attempts...)
			if gateResult.OracleCanary != nil {
				result.OracleCanary = gateResult.OracleCanary
			}
			namedGateInputs = append(namedGateInputs, policy.NamedGateInput{
				Check:                 check,
				Command:               gateResult.Result.Command,
				ExitCode:              gateResult.Result.ExitCode,
				DurationMs:            gateResult.DurationMs,
				LogSHA256:             gateResult.LogSHA256,
				ReferenceOracleSHA256: gateResult.ReferenceOracleSHA256,
				BaseCheck:             gateResult.BaseCheck,
			})
			if gateResult.Result.ExitCode != 0 {
				namedGatesFailed = true
			}
			if check == policy.ReferenceOracleGateID && gateResult.Result.ExitCode == 0 {
				passedReferenceOracleSHA256 = gateResult.ReferenceOracleSHA256
			}
		}
	}
	return gateOutcome{
		fullSuite:                   fullSuite,
		ranFullSuite:                ranFullSuite,
		namedGateInputs:             namedGateInputs,
		namedGatesFailed:            namedGatesFailed,
		passedReferenceOracleSHA256: passedReferenceOracleSHA256,
	}, nil
}

// collectRunEvidence runs CollectEvidenceActivity after verification and the
// gates, and copies what it found into result.
func collectRunEvidence(ctx temporalworkflow.Context, ids *activityIDs, input RunWorkflowInput, current *RunProgress, result *RunWorkflowResult, gates gateOutcome, worktreePath, branch string) (CollectedEvidence, error) {
	// The changed-file inventory (and everything derived from it) is
	// collected after verification (and, above, the optional full-suite
	// check on a new execution), not right after the build — canonical
	// verification can itself rewrite files, matching cmd/factoryd's own
	// documented reasoning for the same ordering.
	var evidenceResult CollectedEvidence
	collectInput := CollectEvidenceInput{
		WorkspacePath: input.WorkspacePath,
		// input.effectiveDiffBase(), not input.BaseSHA: see DiffBaseSHA's
		// own doc comment — a corrective round's own BaseSHA is just its
		// checkout point, not the cumulative diff base a -diff-base
		// override names.
		BaseSHA:              input.effectiveDiffBase(),
		RequiredChangedFiles: input.RequiredChangedFiles,
		RequiredContent:      input.RequiredContent,
		BuildExitCode:        result.Build.ExitCode,
		VerifyExitCode:       result.Verify.ExitCode,
		// FullSuiteRan/FullSuiteExitCode: found via review (GitHub Codex
		// App, PR #33, same round as the reorder above) — without these,
		// CollectEvidenceActivity's safety-net commit was gated only on
		// build+verify succeeding, so a full-suite command that mutated
		// the checkout and then FAILED still had its output committed and
		// HEAD advanced, even though this run quarantines on
		// full_suite_verify. On a non-isolated workspace (required for
		// -prior-run chaining, since isolation-onto-prior-run isn't
		// supported), the next run captures base_sha fresh from that same
		// HEAD — silently inheriting a quarantined run's rejected output
		// as its own baseline, exactly gap 4 from the plan's 2026-08-28
		// readiness review.
		FullSuiteRan:      gates.ranFullSuite,
		FullSuiteExitCode: gates.fullSuite.Result.ExitCode,
		NamedGatesFailed:  gates.namedGatesFailed,
		CheckpointDir:     input.CheckpointDir,
		LogDir:            input.LogDir,
	}
	current.Stage, current.StartedAt = "evidence", temporalworkflow.Now(ctx)
	if err := temporalworkflow.ExecuteActivity(ids.with(temporalworkflow.WithActivityOptions(ctx, hostGitActivityOptions()), "collect-evidence"), CollectEvidenceActivityName, collectInput).Get(ctx, &evidenceResult); err != nil {
		// result.Attempts here already has both build's and verify's —
		// same reasoning as the post-build activity failure above. Same
		// ActivityCommittedFromError recovery too: CollectEvidenceActivity's
		// own verification-output commit can land, then a later step in
		// the same invocation (diff collection, a required-content read)
		// fail.
		return CollectedEvidence{}, wrapActivityFailure("collect evidence activity", err, result.Attempts, result.BaseSHA, result.CommittedByWorker || ActivityCommittedFromError(err), worktreePath, branch)
	}
	result.ResultSHA = evidenceResult.ResultSHA
	result.ChangedFiles = evidenceResult.ChangedFiles
	result.DiffStat = evidenceResult.DiffStat
	result.DiffAvailable = evidenceResult.DiffAvailable
	result.DiffTruncated = evidenceResult.DiffTruncated
	result.DependencyChanges = evidenceResult.DependencyChanges
	result.Oracles = evidenceResult.Oracles
	// OR'd with the build-time value, not overwritten: either
	// PostBuildActivity (build-time dirt) or CollectEvidenceActivity
	// (verification-time dirt) committing on this run's behalf makes the
	// result factory-owned, and CommittedByWorker should stay true if
	// either one did.
	result.CommittedByWorker = result.CommittedByWorker || evidenceResult.Committed
	return evidenceResult, nil
}

// reviewOutcome is which review steps ran and what each found.
type reviewOutcome struct {
	specConformity          VerifyActivityResult
	ranSpecConformityReview bool
	codeReview              VerifyActivityResult
	ranCodeReview           bool
}

// runReviewSteps runs the spec-conformity and code-review steps once every
// earlier gate passed, chaining each step's relay spend into the next.
func runReviewSteps(ctx temporalworkflow.Context, ids *activityIDs, buildVerifyCtx temporalworkflow.Context, input RunWorkflowInput, current *RunProgress, result *RunWorkflowResult, gates gateOutcome, worktreePath, branch string) (reviewOutcome, error) {
	// spec-conformity review and code review (reviewstep.Plan -- see
	// RunReviewStepActivityName's own doc comment for the full
	// two-phase-design incident and reviewstep/internal/conformity/
	// internal/codereview for the shared logic cmd/factoryd
	// also uses). The plan runs only once every earlier gate this run has
	// already evaluated passed -- so the workspace is guaranteed clean by
	// CollectEvidenceActivity's own safety-net commit just above, giving
	// the reviewer a real commit to evaluate.
	//
	// SpecConformityConfigured/CodeReviewConfigured are set from the raw
	// input fields, not gated on any of the conditions below -- mirrors
	// run.Run.SpecConformityConfigured's own "declared, independent of
	// whether the gate ran" contract (see RunWorkflowResult.
	// SpecConformityConfigured's own doc comment), as cmd/factoryd sets r.SpecConformityConfigured before its own gate runs.
	result.SpecConformityConfigured = input.SpecAcceptanceCriteria != ""
	// Standalone AI code review (run_ticket.go's own "standalone AI code
	// review" block, PR M2-B/M2-C): a free-form review of the diff for
	// concrete defects, distinct from the spec-conformity step (which
	// checks the diff against declared acceptance criteria, when the
	// ticket declared any). Runs under the exact same gate predicate the
	// spec-conformity step uses, minus its own "configured" check, and
	// REGARDLESS of the spec-conformity step's own outcome (a code-review
	// policy the operator explicitly opted into is not conditioned on
	// whether the ticket separately declared acceptance criteria, or on
	// whether that separate review passed).
	result.CodeReviewConfigured = input.CodeReviewPolicy != "" && input.CodeReviewPolicy != codereview.PolicyOff
	// activityFailureContext preserves each step's own wrapActivityFailure
	// context string from before this unification (M4-K3), plus the
	// Combined step's own (M4-K? -- combined-review call).
	activityFailureContext := map[string]string{
		reviewstep.SpecConformity: "run spec-conformity-review activity",
		reviewstep.CodeReview:     "run code-review activity",
		reviewstep.Combined:       "run combined-review activity",
	}
	gatesPassed := result.Build.ExitCode == 0 && result.Verify.ExitCode == 0 && (!gates.ranFullSuite || gates.fullSuite.Result.ExitCode == 0) && !gates.namedGatesFailed
	stepResults := map[string]VerifyActivityResult{}
	ranStep := map[string]bool{}
	// consumedInputTokens/OutputTokens/CostMicroUSD accumulate this run's
	// relay spend as each step runs, so the next step's own PriorConsumed*
	// chains build's spend plus every earlier step's own -- the same
	// chaining run_ticket.go's own runReviewPhase calls perform (the
	// code-review step's own PriorConsumed* is build's plus the
	// spec-conformity step's own, zero when that step never ran; the
	// Combined step's own PriorConsumed* is build's alone, since it
	// replaces both standalone steps and there is no earlier review-phase
	// spend to chain in).
	consumedInputTokens := result.Build.RelayConsumedInputTokens
	consumedOutputTokens := result.Build.RelayConsumedOutputTokens
	consumedCostMicroUSD := result.Build.RelayConsumedCostMicroUSD
	if gatesPassed {
		for _, step := range reviewstep.Plan(result.SpecConformityConfigured, result.CodeReviewConfigured) {
			stepInput := ReviewStepInput{
				RunWorkflowInput:          input,
				Step:                      step.Name,
				PriorConsumedInputTokens:  consumedInputTokens,
				PriorConsumedOutputTokens: consumedOutputTokens,
				PriorConsumedCostMicroUSD: consumedCostMicroUSD,
			}
			current.Stage, current.StartedAt = step.Stage, temporalworkflow.Now(ctx)
			var stepResult VerifyActivityResult
			if err := temporalworkflow.ExecuteActivity(ids.withRetries(buildVerifyCtx, "review-"+step.Name), RunReviewStepActivityName, stepInput).Get(ctx, &stepResult); err != nil {
				return reviewOutcome{}, wrapActivityFailure(activityFailureContext[step.Name], err, result.Attempts, result.BaseSHA, result.CommittedByWorker, worktreePath, branch)
			}
			result.Attempts = append(result.Attempts, stepResult.Attempts...)
			if step.Name == reviewstep.Combined {
				// One launch, two gates: Command/DurationMs/LogSHA256 are
				// shared, ExitCode is derived from the launch's own
				// two-independent-bits contract (reviewstep.GateExitCodes)
				// -- populating both stepResults/ranStep entries this way
				// means every downstream reader below (the oracle-commit
				// predicate, policyInput's own SpecConformity*/CodeReview*
				// fields) stays unchanged, whether one launch or two
				// produced them.
				conformityExitCode, codeReviewExitCode := reviewstep.GateExitCodes(stepResult.Result.ExitCode)
				conformityResult, codeReviewResult := stepResult, stepResult
				conformityResult.Result.ExitCode = conformityExitCode
				codeReviewResult.Result.ExitCode = codeReviewExitCode
				stepResults[reviewstep.SpecConformity] = conformityResult
				stepResults[reviewstep.CodeReview] = codeReviewResult
				ranStep[reviewstep.SpecConformity] = true
				ranStep[reviewstep.CodeReview] = true
			} else {
				stepResults[step.Name] = stepResult
				ranStep[step.Name] = true
			}
			consumedInputTokens += stepResult.Result.RelayConsumedInputTokens
			consumedOutputTokens += stepResult.Result.RelayConsumedOutputTokens
			consumedCostMicroUSD += stepResult.Result.RelayConsumedCostMicroUSD
		}
	}
	specConformity, ranSpecConformityReview := stepResults[reviewstep.SpecConformity], ranStep[reviewstep.SpecConformity]
	codeReview, ranCodeReview := stepResults[reviewstep.CodeReview], ranStep[reviewstep.CodeReview]
	return reviewOutcome{specConformity: specConformity, ranSpecConformityReview: ranSpecConformityReview, codeReview: codeReview, ranCodeReview: ranCodeReview}, nil
}

// oracleCommitPhase commits the verified reference-oracle bytes into the
// result when every gate and review passed, then re-runs verification against
// the committed tree. It may replace verify, gates and evidence with the
// committed tree's results.
func oracleCommitPhase(ctx temporalworkflow.Context, ids *activityIDs, buildVerifyActivityOptions temporalworkflow.ActivityOptions, input RunWorkflowInput, current *RunProgress, result *RunWorkflowResult, worktreePath, branch string, gates *gateOutcome, evidenceResult *CollectedEvidence, verify *VerifyActivityResult, reviews reviewOutcome) error {
	// Committed oracles, Temporal parity with cmd/factoryd:
	// the factory host commits the verified oracle bytes into the result.
	// It has to be here rather than in CollectEvidenceActivity because
	// spec conformity (above) can only run after that Activity, and an
	// oracle must never be committed for a run that fails conformity or
	// code review. Same predicate as the verification safety-net commit
	// plus "reference oracle passed" and "conformity/code-review passed
	// if declared". The Activity itself is a no-op unless the manifest
	// declares a target_path.
	if oracleCommitWanted(input, result, gates, reviews) {
		var committedOracles CommittedOracles
		current.Stage, current.StartedAt = "commit_oracles", temporalworkflow.Now(ctx)
		if err := temporalworkflow.ExecuteActivity(ids.with(ctx, "commit-oracles"), CommitOraclesActivityName, CommitOraclesInput{RunWorkflowInput: input, PinnedOracleSHA256: gates.passedReferenceOracleSHA256, ValidateOnly: input.NoCommitOracles}).Get(ctx, &committedOracles); err != nil {
			return wrapActivityFailure("commit accepted oracles activity", err, result.Attempts, result.BaseSHA, result.CommittedByWorker || ActivityCommittedFromError(err), worktreePath, branch)
		}
		if committedOracles.Active {
			result.ResultSHA = committedOracles.ResultSHA
			result.ChangedFiles = committedOracles.ChangedFiles
			result.DiffStat = committedOracles.DiffStat
			result.DiffAvailable = committedOracles.DiffAvailable
			result.DiffTruncated = committedOracles.DiffTruncated
			result.Oracles = committedOracles.Oracles
			evidenceResult.ChangedFiles = committedOracles.ChangedFiles
			if committedOracles.RequiredContentFinalFiles != nil {
				evidenceResult.RequiredContentFinalFiles = committedOracles.RequiredContentFinalFiles
			}
			result.CommittedByWorker = result.CommittedByWorker || committedOracles.Committed
		}

		if committedOracles.Committed {
			if err := rerunVerifyOnCommittedTree(ctx, ids, buildVerifyActivityOptions, input, current, result, worktreePath, branch, gates, verify); err != nil {
				return err
			}
		}
	}
	return nil
}

// evaluateRun builds the policy input from everything the earlier phases
// found and records the verdict (EvaluateRunActivity) in result.
func evaluateRun(ctx temporalworkflow.Context, ids *activityIDs, input RunWorkflowInput, current *RunProgress, result *RunWorkflowResult, worktreePath, branch string, verify VerifyActivityResult, evidenceResult CollectedEvidence, gates gateOutcome, reviews reviewOutcome) error {
	policyInput := policy.EvaluateRunInput{
		VerifyCommand:             result.Verify.Command,
		BuildExitCode:             result.Build.ExitCode,
		VerifyExitCode:            result.Verify.ExitCode,
		VerifyDurationMs:          verify.DurationMs,
		VerifyLogSHA256:           verify.LogSHA256,
		VerifyRerun:               verify.Rerun,
		ChangedFiles:              evidenceResult.ChangedFiles,
		AllowedFiles:              input.AllowedFiles,
		RequiredChangedFiles:      input.RequiredChangedFiles,
		RequiredContent:           input.RequiredContent,
		RequiredContentBaseFiles:  evidenceResult.RequiredContentBaseFiles,
		RequiredContentFinalFiles: evidenceResult.RequiredContentFinalFiles,
		NamedGates:                gates.namedGateInputs,
		TestPatterns:              input.TestPatterns,
		TestsRequiredOptOut:       input.TestsRequiredOptOut,
		Oracles:                   result.Oracles,
	}
	if gates.ranFullSuite {
		policyInput.FullSuiteCommand = gates.fullSuite.Result.Command
		policyInput.FullSuiteExitCode = gates.fullSuite.Result.ExitCode
		policyInput.FullSuiteDurationMs = gates.fullSuite.DurationMs
		policyInput.FullSuiteLogSHA256 = gates.fullSuite.LogSHA256
		policyInput.FullSuiteRerun = gates.fullSuite.Rerun
	}
	if reviews.ranSpecConformityReview {
		policyInput.SpecConformityCommand = reviews.specConformity.Result.Command
		policyInput.SpecConformityExitCode = reviews.specConformity.Result.ExitCode
		policyInput.SpecConformityDurationMs = reviews.specConformity.DurationMs
		policyInput.SpecConformityLogSHA256 = reviews.specConformity.LogSHA256
	}
	if reviews.ranCodeReview {
		policyInput.CodeReviewCommand = reviews.codeReview.Result.Command
		policyInput.CodeReviewExitCode = reviews.codeReview.Result.ExitCode
		policyInput.CodeReviewDurationMs = reviews.codeReview.DurationMs
		policyInput.CodeReviewLogSHA256 = reviews.codeReview.LogSHA256
	}
	var evalResult policy.EvaluateRunResult
	current.Stage, current.StartedAt = "evaluate", temporalworkflow.Now(ctx)
	if err := temporalworkflow.ExecuteActivity(ids.with(ctx, "evaluate"), EvaluateRunActivityName, EvaluateRunInput{Policy: policyInput, LogDir: input.LogDir}).Get(ctx, &evalResult); err != nil {
		return wrapActivityFailure("evaluate run policy activity", err, result.Attempts, result.BaseSHA, result.CommittedByWorker, worktreePath, branch)
	}
	result.GateResults = evalResult.GateResults
	result.State = run.StateQuarantined
	if evalResult.Accepted {
		result.State = run.StateAccepted
	}
	return nil
}

// oracleCommitWanted is the predicate for committing the verified oracle
// bytes: the verification safety-net commit's, plus "reference oracle
// passed" and "conformity/code-review passed if declared".
func oracleCommitWanted(input RunWorkflowInput, result *RunWorkflowResult, gates *gateOutcome, reviews reviewOutcome) bool {
	return gates.passedReferenceOracleSHA256 != "" && input.ReferenceOracleDir != "" &&
		result.Build.ExitCode == 0 && result.Verify.ExitCode == 0 && (!gates.ranFullSuite || gates.fullSuite.Result.ExitCode == 0) && !gates.namedGatesFailed &&
		(input.SpecAcceptanceCriteria == "" || (reviews.ranSpecConformityReview && reviews.specConformity.Result.ExitCode == 0)) &&
		(!result.CodeReviewConfigured || (reviews.ranCodeReview && reviews.codeReview.Result.ExitCode == 0))
}

// The host commit changed HEAD after every gate ran, so canonical
// verification (and the full suite, when declared) re-runs against the
// committed tree, (Codex review of
// #202). A failing re-run replaces the run's own result so the
// canonical_verify / full_suite_verify gate quarantines; a re-run that
// left the workspace dirty fails the run, since its output is in no
// commit. The Activity also re-runs the named gates, so its budget is
// scaled by the phase count (verify, full suite, named gates).
func rerunVerifyOnCommittedTree(ctx temporalworkflow.Context, ids *activityIDs, buildVerifyActivityOptions temporalworkflow.ActivityOptions, input RunWorkflowInput, current *RunProgress, result *RunWorkflowResult, worktreePath, branch string, gates *gateOutcome, verify *VerifyActivityResult) error {
	postVerifyOptions := buildVerifyActivityOptions
	postVerifyOptions.StartToCloseTimeout *= time.Duration(postOracleCommitPhaseCount(input))
	postVerifyCtx := temporalworkflow.WithActivityOptions(ctx, postVerifyOptions)
	var post PostOracleCommitVerifyResult
	current.Stage, current.StartedAt = "post_oracle_commit_verify", temporalworkflow.Now(ctx)
	if err := temporalworkflow.ExecuteActivity(ids.withRetries(postVerifyCtx, "post-oracle-commit-verify"), RunPostOracleCommitVerifyActivityName, input).Get(ctx, &post); err != nil {
		return wrapActivityFailure("run post-oracle-commit verify activity", err, result.Attempts, result.BaseSHA, true, worktreePath, branch)
	}
	result.Attempts = append(result.Attempts, post.Attempts...)
	if post.Verify.Result.ExitCode != 0 {
		*verify = post.Verify
		result.Verify = post.Verify.Result
	}
	if post.FullSuite != nil && post.FullSuite.Result.ExitCode != 0 {
		gates.fullSuite = *post.FullSuite
	}
	// A named gate that fails on the committed tree replaces that
	// gate's earlier (passing) result so the gate quarantines the run.
	for _, g := range post.NamedGates {
		if g.Result.Result.ExitCode == 0 {
			continue
		}
		gates.namedGatesFailed = true
		for i := range gates.namedGateInputs {
			if gates.namedGateInputs[i].Check == g.Check {
				gates.namedGateInputs[i].Command = g.Result.Result.Command
				gates.namedGateInputs[i].ExitCode = g.Result.Result.ExitCode
				gates.namedGateInputs[i].DurationMs = g.Result.DurationMs
				gates.namedGateInputs[i].LogSHA256 = g.Result.LogSHA256
			}
		}
	}
	if !post.Clean {
		return wrapActivityFailure("post-oracle-commit verification", errors.New("post-commit verification left the workspace dirty: its output is not in the result commit, so the accepted tree could differ from what passed"), result.Attempts, result.BaseSHA, true, worktreePath, branch)
	}
	return nil
}
