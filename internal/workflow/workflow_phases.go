package workflow

import (
	"fmt"
	"slices"
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
// the gates, evidence, the tests_red_on_base proof, the review steps and
// the policy verdict, in that order.
func buildToVerdict(ctx temporalworkflow.Context, ids *activityIDs, buildVerifyCtx temporalworkflow.Context, buildVerifyActivityOptions temporalworkflow.ActivityOptions, input *RunWorkflowInput, current *RunProgress, result *RunWorkflowResult, worktreePath, branch string) error {
	verify, err := buildAndVerify(ctx, ids, buildVerifyCtx, *input, current, result, worktreePath, branch)
	if err != nil {
		return err
	}

	gates, err := postVerifyGates(ctx, ids, buildVerifyCtx, input, current, result, worktreePath, branch)
	if err != nil {
		return err
	}

	evidenceResult, err := collectRunEvidence(ctx, ids, *input, current, result, gates, worktreePath, branch)
	if err != nil {
		return err
	}

	testsRed, err := runTestsRedOnBase(ctx, ids, buildVerifyCtx, *input, current, result, gates, worktreePath, branch)
	if err != nil {
		return err
	}

	var reviews reviewOutcome
	version := temporalworkflow.GetVersion(ctx, changeSizeMeasurementChange, temporalworkflow.DefaultVersion, 1)
	passed := beforeReviewPassed(*input, result, gates, testsRed)
	if version == 1 && passed {
		if err := measureChangeSize(ctx, ids, *input, current, result, worktreePath, branch); err != nil {
			return err
		}
	}
	reviews, err = reviewAndChallenge(ctx, ids, buildVerifyCtx, *input, current, result, gates, version, passed, worktreePath, branch)
	if err != nil {
		return err
	}

	// Marks the executions that no longer commit a reference oracle's bytes
	// or verify the committed tree (referenceOracleRemovedChange).
	temporalworkflow.GetVersion(ctx, referenceOracleRemovedChange, temporalworkflow.DefaultVersion, 1)

	if err = evaluateRun(ctx, ids, *input, current, result, worktreePath, branch, verify, evidenceResult, gates, reviews, testsRed); err != nil {
		return err
	}
	return nil
}

// Histories before this marker replay without the measurement Activity.
const changeSizeMeasurementChange = "source-change-size"

func beforeReviewPassed(input RunWorkflowInput, result *RunWorkflowResult, gates gateOutcome, testsRed testsRedOutcome) bool {
	if !earlierGatesPassed(result, gates) {
		return false
	}
	scope := policy.Scope{AllowedFiles: input.AllowedFiles, Unapproved: input.ScopeUnapproved, AllowedPaths: input.AllowedPaths}
	if scope.Declared() && len(scope.Judge(policy.ExcludeHarnessByproducts(result.ChangedFiles)).Refused()) != 0 {
		return false
	}
	gate, _ := policy.TestsRedOnBase(policy.TestFiles(result.ChangedFiles, input.TestPatterns), input.TestsRequiredOptOut, input.Bug, testsRed.policyInput())
	return gate.Passed
}

func measureChangeSize(ctx temporalworkflow.Context, ids *activityIDs, input RunWorkflowInput, current *RunProgress, result *RunWorkflowResult, worktreePath, branch string) error {
	current.Stage, current.StartedAt = "change_size", temporalworkflow.Now(ctx)
	in := MeasureChangeSizeInput{WorkspacePath: input.WorkspacePath, BaseSHA: input.effectiveDiffBase(), ResultSHA: result.ResultSHA, TestPatterns: input.TestPatterns}
	if err := temporalworkflow.ExecuteActivity(ids.with(temporalworkflow.WithActivityOptions(ctx, hostGitActivityOptions()), "measure-change-size"), MeasureChangeSizeActivityName, in).Get(ctx, &result.ChangeSize); err != nil {
		return wrapActivityFailure("measure change size activity", err, result.Attempts, result.BaseSHA, result.CommittedByWorker, worktreePath, branch)
	}
	if result.ChangeSize == nil {
		return wrapActivityFailure("measure change size activity", fmt.Errorf("measurement returned no result"), result.Attempts, result.BaseSHA, result.CommittedByWorker, worktreePath, branch)
	}
	return nil
}

func overSizeCap(input RunWorkflowInput, size *run.ChangeSize) bool {
	return size != nil && (size.SourceFiles > input.ReleaseMaxFilesChanged || size.SourceLines > input.ReleaseMaxChangedLines)
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
	build, buildErr := runBuildStep(ctx, ids, buildVerifyCtx, input, current)
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
	result.noComposeSidecars = verify.NoComposeSidecars
	result.Attempts = append(result.Attempts, verify.Attempts...)
	return verify, nil
}

// runBuildStep runs the build: RunBuildActivity, or, for a replay run
// (input.Replay, admitted by refuseIncoherentReplay under replayRunChange),
// ApplyReplayActivity, a host check that builds and launches nothing and
// answers with the same BuildActivityResult.
func runBuildStep(ctx temporalworkflow.Context, ids *activityIDs, buildVerifyCtx temporalworkflow.Context, input RunWorkflowInput, current *RunProgress) (BuildActivityResult, error) {
	var build BuildActivityResult
	if input.Replay != nil {
		current.Stage, current.StartedAt = replayStage, temporalworkflow.Now(ctx)
		err := temporalworkflow.ExecuteActivity(ids.with(temporalworkflow.WithActivityOptions(ctx, hostGitActivityOptions()), "apply-replay"), ApplyReplayActivityName, input).Get(ctx, &build)
		return build, err
	}
	current.Stage, current.StartedAt = "build", temporalworkflow.Now(ctx)
	err := temporalworkflow.ExecuteActivity(ids.withRetries(buildVerifyCtx, "build"), RunBuildActivityName, input).Get(ctx, &build)
	return build, err
}

// gateOutcome is what postVerifyGates found: the full-suite and repository-gate
// results the later phases read.
type gateOutcome struct {
	fullSuite    VerifyActivityResult
	ranFullSuite bool
	// repoGateInputs and repoGatesFailed are the repository gates' results.
	repoGateInputs  []policy.RepoGateInput
	repoGatesFailed bool
}

// postVerifyGates runs the full-suite check and the repository gates, each only
// once canonical verification passed. It sets input.GateCommands.
func postVerifyGates(ctx temporalworkflow.Context, ids *activityIDs, buildVerifyCtx temporalworkflow.Context, input *RunWorkflowInput, current *RunProgress, result *RunWorkflowResult, worktreePath, branch string) (gateOutcome, error) {
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

	// The repository gates, as full-suite is: each project-configured
	// command runs here, via RunNamedGateActivityName, only once canonical
	// verification itself passed.
	var repoGateInputs []policy.RepoGateInput
	repoGatesFailed := false
	if result.Build.ExitCode == 0 && result.Verify.ExitCode == 0 {
		gateChecks, gateCommands := gateChecksToRun(ctx, *input)
		// The commands the gates below ran with: a repository's own gates
		// are in them only when they ran here.
		input.GateCommands = gateCommands
		for _, check := range gateChecks {
			command := input.GateCommands[check]
			var gateResult VerifyActivityResult
			current.Stage, current.StartedAt = check, temporalworkflow.Now(ctx)
			if err := temporalworkflow.ExecuteActivity(ids.withRetries(buildVerifyCtx, "gate-"+check), RunNamedGateActivityName, RepoGateActivityInput{RunWorkflowInput: *input, Check: check, Command: command}).Get(ctx, &gateResult); err != nil {
				return gateOutcome{}, wrapActivityFailure("run "+check+" gate activity", err, result.Attempts, result.BaseSHA, result.CommittedByWorker, worktreePath, branch)
			}
			result.Attempts = append(result.Attempts, gateResult.Attempts...)
			repoGateInputs = append(repoGateInputs, policy.RepoGateInput{
				Check:      check,
				Command:    gateResult.Result.Command,
				ExitCode:   gateResult.Result.ExitCode,
				DurationMs: gateResult.DurationMs,
				LogSHA256:  gateResult.LogSHA256,
				BaseCheck:  gateResult.BaseCheck,
			})
			if gateResult.Result.ExitCode != 0 {
				repoGatesFailed = true
			}
		}
	}
	return gateOutcome{
		fullSuite:       fullSuite,
		ranFullSuite:    ranFullSuite,
		repoGateInputs:  repoGateInputs,
		repoGatesFailed: repoGatesFailed,
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
		BaseSHA:        input.effectiveDiffBase(),
		BuildExitCode:  result.Build.ExitCode,
		VerifyExitCode: result.Verify.ExitCode,
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
		RepoGatesFailed:   gates.repoGatesFailed,
		CheckpointDir:     input.CheckpointDir,
		LogDir:            input.LogDir,
		SetAside:          slices.Concat(input.ChallengeHeld, input.ChallengeSetAside),
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
	// OR'd with the build-time value, not overwritten: either
	// PostBuildActivity (build-time dirt) or CollectEvidenceActivity
	// (verification-time dirt) committing on this run's behalf makes the
	// result factory-owned, and CommittedByWorker should stay true if
	// either one did.
	result.CommittedByWorker = result.CommittedByWorker || evidenceResult.Committed
	return evidenceResult, nil
}

// reviewOutcome is which review steps ran and what each found. legacy marks
// a history recorded before reviewReproductionChange, whose retired
// spec-conformity review's result, when it ran, is legacyConformity.
type reviewOutcome struct {
	codeReview       VerifyActivityResult
	ranCodeReview    bool
	legacy           bool
	legacyConformity *VerifyActivityResult
	reproductions    reproductionOutcome
}

// earlierGatesPassed reports whether build, canonical verification, the full
// suite and the repository gates all passed: the point from which the proof of the
// changed tests and the review run.
func earlierGatesPassed(result *RunWorkflowResult, gates gateOutcome) bool {
	return result.Build.ExitCode == 0 && result.Verify.ExitCode == 0 && (!gates.ranFullSuite || gates.fullSuite.Result.ExitCode == 0) && !gates.repoGatesFailed
}

// runReviewSteps runs the review step (planReviewSteps) once every earlier
// gate passed, chaining its relay spend from the build's.
func runReviewSteps(ctx temporalworkflow.Context, ids *activityIDs, buildVerifyCtx temporalworkflow.Context, input RunWorkflowInput, current *RunProgress, result *RunWorkflowResult, gates gateOutcome, worktreePath, branch string) (reviewOutcome, error) {
	// The plan runs only once every earlier gate this run has already
	// evaluated passed -- so the workspace is guaranteed clean by
	// CollectEvidenceActivity's own safety-net commit just above, giving
	// the reviewer a real commit to evaluate.
	//
	// CodeReviewConfigured is set from the raw input field, not gated on any
	// of the conditions below: "declared, independent of whether the gate
	// ran", as run.Run's own contract says.
	result.CodeReviewConfigured = input.CodeReviewPolicy != "" && input.CodeReviewPolicy != codereview.PolicyOff
	var outcome reviewOutcome
	if !earlierGatesPassed(result, gates) {
		return outcome, nil
	}
	steps, legacy := planReviewSteps(ctx, input, result.CodeReviewConfigured)
	outcome.legacy = legacy
	// A history recorded before retireReviewReproductionChange may hold the
	// retired reviewer-reproduction phase (a run given -review-reproduction,
	// which no Worker sets any more): it replays unchanged. From that change
	// on a review writes no test the factory runs, so no run takes the phase.
	retired := temporalworkflow.GetVersion(ctx, retireReviewReproductionChange, temporalworkflow.DefaultVersion, 1) == 1
	outcome.reproductions.ran = !retired && !legacy && input.ReviewReproduction && len(steps) > 0
	if outcome.reproductions.ran {
		rechecks, err := recheckReproductions(ctx, ids, buildVerifyCtx, input, current, result, worktreePath, branch)
		if err != nil {
			return reviewOutcome{}, err
		}
		outcome.reproductions.rechecks, result.ReviewReproductionRechecks = rechecks, rechecks
	}
	for _, step := range steps {
		stepResult, err := runOneReviewStep(ctx, ids, buildVerifyCtx, input, current, result, step, worktreePath, branch)
		if err != nil {
			return reviewOutcome{}, err
		}
		if legacy {
			outcome = legacyReviewOutcome(outcome, step, stepResult)
			continue
		}
		// The launch exits reviewstep.CombinedExitBase when it gave a verdict:
		// the gate's exit code is derived from that (ReviewExitCode), the
		// command, duration and log hash are the launch's own.
		stepResult.Result.ExitCode = reviewstep.ReviewExitCode(stepResult.Result.ExitCode)
		outcome.codeReview, outcome.ranCodeReview = stepResult, true
		if outcome.reproductions.ran {
			findings, err := reproduceReviewFindings(ctx, ids, buildVerifyCtx, input, current, result, stepResult.Reproductions, worktreePath, branch)
			if err != nil {
				return reviewOutcome{}, err
			}
			outcome.reproductions.findings, result.ReviewReproductions = findings, findings
		}
	}
	return outcome, nil
}

// runOneReviewStep launches step (RunReviewStepActivity) with the build's
// relay spend as its prior spend and records its attempts.
func runOneReviewStep(ctx temporalworkflow.Context, ids *activityIDs, buildVerifyCtx temporalworkflow.Context, input RunWorkflowInput, current *RunProgress, result *RunWorkflowResult, step reviewstep.Step, worktreePath, branch string) (VerifyActivityResult, error) {
	stepInput := ReviewStepInput{
		RunWorkflowInput:          input,
		Step:                      step.Name,
		PriorConsumedInputTokens:  result.Build.RelayConsumedInputTokens,
		PriorConsumedOutputTokens: result.Build.RelayConsumedOutputTokens,
		PriorConsumedCostMicroUSD: result.Build.RelayConsumedCostMicroUSD,
	}
	current.Stage, current.StartedAt = step.Stage, temporalworkflow.Now(ctx)
	var stepResult VerifyActivityResult
	if err := temporalworkflow.ExecuteActivity(ids.withRetries(buildVerifyCtx, "review-"+step.Name), RunReviewStepActivityName, stepInput).Get(ctx, &stepResult); err != nil {
		return VerifyActivityResult{}, wrapActivityFailure("run "+step.Label+" activity", err, result.Attempts, result.BaseSHA, result.CommittedByWorker, worktreePath, branch)
	}
	result.Attempts = append(result.Attempts, stepResult.Attempts...)
	return stepResult, nil
}

// evaluateRun builds the policy input from everything the earlier phases
// found and records the verdict (EvaluateRunActivity) in result.
func evaluateRun(ctx temporalworkflow.Context, ids *activityIDs, input RunWorkflowInput, current *RunProgress, result *RunWorkflowResult, worktreePath, branch string, verify VerifyActivityResult, evidenceResult CollectedEvidence, gates gateOutcome, reviews reviewOutcome, testsRed testsRedOutcome) error {
	policyInput := policy.EvaluateRunInput{
		VerifyCommand:       result.Verify.Command,
		BuildExitCode:       result.Build.ExitCode,
		VerifyExitCode:      result.Verify.ExitCode,
		VerifyDurationMs:    verify.DurationMs,
		VerifyLogSHA256:     verify.LogSHA256,
		VerifyRerun:         verify.Rerun,
		ChangedFiles:        evidenceResult.ChangedFiles,
		AllowedFiles:        input.AllowedFiles,
		ScopeUnapproved:     input.ScopeUnapproved,
		AllowedPaths:        input.AllowedPaths,
		RepoGates:           legacyConformityGates(gates.repoGateInputs, reviews),
		TestPatterns:        input.TestPatterns,
		TestsRequiredOptOut: input.TestsRequiredOptOut,
		TestsOptOutRefused:  input.Bug,
	}
	if gates.ranFullSuite {
		policyInput.FullSuiteCommand = gates.fullSuite.Result.Command
		policyInput.FullSuiteExitCode = gates.fullSuite.Result.ExitCode
		policyInput.FullSuiteDurationMs = gates.fullSuite.DurationMs
		policyInput.FullSuiteLogSHA256 = gates.fullSuite.LogSHA256
		policyInput.FullSuiteRerun = gates.fullSuite.Rerun
	}
	policyInput.TestsRedOnBase = testsRed.policyInput()
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
	result.Notes = evalResult.Notes
	result.State = run.StateQuarantined
	if evalResult.Accepted {
		result.State = run.StateAccepted
	}
	return nil
}
