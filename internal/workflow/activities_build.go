package workflow

import (
	"buildgate/internal/harness"
	"buildgate/internal/policy"
	"buildgate/internal/run"
	"buildgate/internal/runner"
	"buildgate/internal/sandbox"
	"buildgate/internal/ticketspec"
	wsisolation "buildgate/internal/workspace"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"
)

// harnessEnv is the worker environment of a job's harness
// (harness.Descriptor.WorkerEnv). The name was validated when the submitting
// process resolved the role, so an unknown name here is a corrupted input:
// the job then runs with no extra environment and its own --harness argument
// makes the script refuse the same name.
func harnessEnv(name string) []string {
	d, err := harness.Lookup(name)
	if err != nil {
		return nil
	}
	return d.WorkerEnv
}

// harnessArg is the harness name a job's --harness argument (and its recorded
// Attempt.Harness) carries: the registry's canonical spelling, with "" meaning
// pi exactly as harness.Lookup (and so harnessEnv) resolves it, so the command
// and the worker environment can never disagree. An unknown name is passed
// through unchanged for the script's own adapter lookup to refuse.
func harnessArg(name string) string {
	if d, err := harness.Lookup(name); err == nil {
		return d.Name
	}
	return name
}

// executionHarnessEnv and reviewHarnessEnv are harnessEnv for the execution
// (build, corrective rounds) and review (conformity, code review, combined
// review) jobs of one execution.
func executionHarnessEnv(input RunWorkflowInput) []string { return harnessEnv(input.Harness) }

func reviewHarnessEnv(input RunWorkflowInput) []string { return harnessEnv(input.ReviewHarness) }

// autofixArgs is the build script's --autofix-command argument per entry.
func autofixArgs(commands []string) []string {
	var args []string
	for _, c := range commands {
		args = append(args, "--autofix-command", c)
	}
	return args
}

// buildActivityArgs constructs the argv passed to build_app.py on the
// Temporal paths. Pulled out as a pure function, like cmd/factoryd's buildAppArgs (sandbox_exec.go):
// the exact flags threaded through are unit-testable independent of any
// Activity/subprocess machinery.
//
// verifyCommand, when non-empty, is passed as --verify-command (2026-09-09):
// without it, build_app.py's internal per-round corrective verify silently
// re-derives its own command via resolve_verify_command()'s root-only
// auto-detection instead of the same command this Activity's own
// canonical-verify step (verifyCommandFor's other call sites) already
// resolved -- which can disagree with it entirely for a monorepo whose
// auto-detected root command needs a toolchain the sandbox doesn't have.
//
// fastCheckCommand, when non-empty, is passed as --fast-check-command --
// the Temporal counterpart to cmd/factoryd's -fast-check-command: a run routed through -temporal-address
// or -repository silently ignored .factory.yml's fast_check_command and
// always paid for the full verify command.
// thinking is roles.execution's resolved Pi reasoning-effort level
// (internal/modelrole.Resolve, threaded through RunWorkflowInput.Thinking
// by the submitting process -- see cmd/factoryd/run_temporal.go) -- empty
// when roles.execution is unset, which omits --thinking entirely and
// preserves today's argv byte-for-byte.
func buildActivityArgs(buildAppScript, workspace, spec, conformityPolicy string, maxRounds, timeoutMinutes int, baseSHA, verifyCommand, fastCheckCommand, referenceOracleCommand, specAcceptanceCriteria, thinking, harness string, setup ...string) []string {
	args := []string{
		buildAppScript,
		"--workspace", workspace,
		"--spec", spec,
		// --conformity-policy gates the per-criterion spec-conformity
		// review, and must be threaded here too so the Temporal paths
		// don't silently diverge.
		"--conformity-policy", conformityPolicy,
		"--max-rounds", fmt.Sprint(maxRounds),
		"--timeout-minutes", fmt.Sprint(timeoutMinutes),
		"--review-base-sha", baseSHA,
	}
	if verifyCommand != "" {
		args = append(args, "--verify-command", verifyCommand)
	}
	if fastCheckCommand != "" {
		args = append(args, "--fast-check-command", fastCheckCommand)
	}
	for _, c := range setup {
		args = append(args, "--setup-command", c)
	}
	if referenceOracleCommand != "" {
		// build_app.py's own --reference-oracle-command (Phase 0.5): the
		// Temporal/-repository counterpart of cmd/factoryd's buildAppArgs'
		// identical parameter.
		// Only ever non-empty when RunBuildActivity also mounted the
		// snapshotted oracle into this same container (input.
		// ReferenceOracleInLoopRetry) -- the command is meaningless without
		// its files, and never forwarded on the strength of
		// ReferenceOracleCommand alone (that field also drives the separate
		// post-build named gate, which every existing user of it relies on
		// staying the only place oracle content is exposed).
		args = append(args, "--reference-oracle-command", referenceOracleCommand)
	}
	if specAcceptanceCriteria != "" {
		// build_app.py's own --spec-acceptance-criteria. RunBuildActivity's
		// own call site below now always passes "" here, matching
		// cmd/factoryd: a commit-related criterion can only
		// be evaluated meaningfully against an already-committed
		// workspace, and .git is mounted read-only inside the sandbox
		// unconditionally, so build_app.py's own (round-loop) launch can
		// never satisfy one on its own. The spec-conformity review now
		// runs as a SEPARATE, later sandboxed launch
		// (RunSpecConformityReviewActivity, agent/pi/scripts/
		// conformity_review.py), after CollectEvidenceActivity's own
		// safety-net commit lands -- see run_ticket.go's "conformity
		// review, phase 2" doc comment for the full incident. This
		// branch (and the specAcceptanceCriteria parameter) stay general
		// rather than being deleted outright, matching cmd/factoryd's own
		// buildAppArgs, in case a future standalone caller with a
		// genuinely writable .git ever wants both in one pass.
		args = append(args, "--spec-acceptance-criteria", specAcceptanceCriteria)
	}
	if thinking != "" {
		args = append(args, "--thinking", thinking)
	}
	args = append(args, "--harness", harness)
	return args
}

// RunBuildActivity invokes build_app.py through runner.RunWithRetries. A
// completed invocation is returned from its durable checkpoint on redispatch.
func (a *Activities) RunBuildActivity(ctx context.Context, input RunWorkflowInput) (progressResult BuildActivityResult, err error) {
	// The stage "start" mark is deferred until past the checkpoint
	// short-circuit below: a redispatch of an already-completed Activity
	// must not add a spurious near-zero start/end pair to the feed.
	progressStarted := false
	defer func() {
		if !progressStarted {
			return
		}
		outcome := "pass"
		if err != nil || progressResult.Result.ExitCode != 0 {
			outcome = "fail"
		}
		progressMark(ctx, a.logDirFor(input), "build", "end", outcome, composeRejectedProgressDetail(err))
	}()
	if err := a.fenceEarlierAttempts(ctx, input, true); err != nil {
		return BuildActivityResult{}, err
	}
	if input.RoutePolicy != nil {
		a.recordSpendStartAtAttemptOne(ctx, input)
	}
	checkpoint, path, found, err := loadRetriedActivityCheckpoint[BuildActivityResult](ctx, a.checkpointDirFor(input))
	if err != nil {
		return BuildActivityResult{}, checkpointLoadError("load build Activity checkpoint", err)
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
	progressMark(ctx, a.logDirFor(input), "build", "start", "", "")

	// Resolved before any intent record, workspace mutation, or Docker
	// contact: a run that asked for a relay-contained build and cannot get
	// one must fail before build_app.py is invoked at all, never fall back to
	// a build with no such containment. Only this Activity ever gets a relay
	// — canonical verification and the full-suite gate call no model.
	relaySpec, err := a.relaySpecFor(input)
	if err != nil {
		return BuildActivityResult{}, temporal.NewApplicationError(err.Error(), RelayConfigurationFailureType)
	}
	if err := a.capRelayForAttempt(ctx, input, relaySpec); err != nil {
		return BuildActivityResult{}, err
	}
	// Same fail-closed placement for the registry proxy: an unusable proxy
	// configuration is an infrastructure failure surfaced before any
	// intent record or container launch, never a silent unproxied build.
	registrySpec, err := a.registryProxySpecFor(input)
	if err != nil {
		return BuildActivityResult{}, temporal.NewApplicationError(err.Error(), InfrastructureFailureType)
	}
	// Same fail-closed placement for compose services.
	composeSpec, err := a.composeServicesSpecFor(input, "build")
	if err != nil {
		return BuildActivityResult{}, temporal.NewApplicationError(err.Error(), InfrastructureFailureType)
	}
	// Same fail-closed placement for the in-loop reference oracle: a run
	// that asked for it (input.ReferenceOracleInLoopRetry) but did not
	// carry all three of its siblings must fail before any host mutation
	// or container launch, never quietly run without the in-loop check the
	// submitter believes is active. cmd/factoryd already rejects the same
	// combination at flag-parse time; this is the defense-in-depth copy for
	// any other submitter of RunWorkflowInput.
	if input.ReferenceOracleInLoopRetry && (input.ReferenceOracleDir == "" || input.ReferenceOracleMountPath == "" || input.GateCommands[policy.ReferenceOracleGateID] == "") {
		return BuildActivityResult{}, temporal.NewApplicationError("ReferenceOracleInLoopRetry requires ReferenceOracleDir, ReferenceOracleMountPath, and ReferenceOracleCommand to all be set", InfrastructureFailureType)
	}

	// Two-phase intent protocol closing the crash window between the
	// subprocess finishing and the checkpoint rename below: if a prior
	// attempt of this exact Activity execution recorded intent but never
	// reached a completed checkpoint, a crash happened in that window and
	// build_app.py may already have run (and committed) once. Re-running it
	// blind risks a second invocation; trusting it succeeded risks an
	// accepted run with no checkpointed evidence. Halt instead.
	intentFound, journalFound, err := priorAttemptRecords(ctx, a.checkpointDirFor(input))
	if err != nil {
		return BuildActivityResult{}, priorRecordsFailure("build", err)
	}
	if intentFound {
		return BuildActivityResult{}, temporal.NewApplicationError(
			"a prior attempt of this build Activity recorded intent to invoke build_app.py but crashed before reaching a durable checkpoint — halting rather than risk a duplicate invocation or trusting an unconfirmed result",
			AmbiguousPriorAttemptType,
		)
	}
	if journalFound {
		return BuildActivityResult{}, temporal.NewApplicationError(
			"a build Activity attempt journal exists without its intent or completed checkpoint — halting rather than risk a duplicate invocation",
			AmbiguousPriorAttemptType,
		)
	}
	// A Temporal retry (attempt > 1) reaches here with the earlier
	// attempts' records deliberately not counted (see retriedAttempt): keep
	// that attempt's work and resume from it with a fresh harness session.
	// A run that adopted a halted run's worktree (input.ResumeFrom) takes the
	// same path on its first attempt: the worktree holds a lost build's work.
	var resumed resumeHandoff
	if retriedAttempt(ctx) || input.ResumeFrom != nil {
		resumed, err = a.prepareBuildHandoff(ctx, input)
		if err != nil {
			return BuildActivityResult{}, err
		}
		defer dropCheckpointRef(ctx, input.WorkspacePath, resumed.Ref)
		detail := fmt.Sprintf("build attempt %d resumed after the worker stopped; kept the earlier attempt's work (checkpoint %s)", activity.GetInfo(ctx).Attempt, shortSHA(resumed.SnapshotSHA))
		if input.ResumeFrom != nil && activity.GetInfo(ctx).Attempt <= 1 {
			detail = fmt.Sprintf("build resumed from run %s after its worker was lost; kept its work (checkpoint %s)", input.ResumeFrom.RunID, shortSHA(resumed.SnapshotSHA))
		}
		activity.GetLogger(ctx).Info(detail)
		progressMark(ctx, a.logDirFor(input), "build", "note", "", detail)
	}
	// This runs only after the repository owner has granted the request its
	// serialized turn, so cleanup cannot erase another in-flight run's
	// BUILD_EVIDENCE.json while that run is still collecting evidence.
	if err := os.Remove(filepath.Join(input.WorkspacePath, "BUILD_EVIDENCE.json")); err != nil && !os.IsNotExist(err) {
		return BuildActivityResult{}, temporal.NewApplicationErrorWithCause("remove stale build evidence", InfrastructureFailureType, err)
	}
	// Host-side info/exclude installed before the build (as in cmd/factoryd's runMainWithReady): without it the driver
	// falls back to appending its bookkeeping names to the tracked
	// .gitignore on every Temporal/API run (Codex review of PR #98).
	// Best-effort for the same reason as there.
	if err := wsisolation.ExcludeHarnessArtifacts(input.WorkspacePath); err != nil {
		activity.GetLogger(ctx).Warn("exclude harness artifacts via info/exclude", "error", err)
	}

	// In-loop reference oracle (Phase 0.5), the Temporal/-repository
	// counterpart of cmd/factoryd's build-phase block: opt-in via
	// input.ReferenceOracleInLoopRetry only
	// (validated complete above), snapshotted-then-hashed through the same
	// sandbox.SnapshotReferenceOracle that this file's named-gate Activity also uses, mounted read-only into THIS build container,
	// with the command forwarded to build_app.py. All three locals stay
	// empty otherwise, so every existing run -- including one that
	// configured the oracle trio solely for the post-build gate -- is
	// byte-for-byte unchanged. "reference-oracle-build-snapshot", distinct
	// from the gate's "reference-oracle-snapshot": both can be alive in one
	// run and one launch's cleanup must never remove the other's copy.
	var buildOracleDir, buildOracleMountPath, buildOracleCommand, buildOracleSHA256 string
	if input.ReferenceOracleInLoopRetry {
		snapshotDir, absErr := filepath.Abs(filepath.Join(a.logDirFor(input), "reference-oracle-build-snapshot"))
		if absErr != nil {
			return BuildActivityResult{}, temporal.NewApplicationErrorWithCause("resolve reference-oracle build snapshot path", InfrastructureFailureType, absErr)
		}
		buildOracleSHA256, err = sandbox.SnapshotReferenceOracle(input.WorkspacePath, input.ReferenceOracleDir, snapshotDir)
		if err != nil {
			return BuildActivityResult{}, temporal.NewApplicationErrorWithCause("reference-oracle build snapshot", InfrastructureFailureType, err)
		}
		defer os.RemoveAll(snapshotDir)
		buildOracleDir = snapshotDir
		buildOracleMountPath = input.ReferenceOracleMountPath
		buildOracleCommand = input.GateCommands[policy.ReferenceOracleGateID]
	}

	// "" for specAcceptanceCriteria, not a.specAcceptanceCriteriaFor(input):
	// see buildActivityArgs' own doc comment on that parameter for why.
	args := buildActivityArgs(a.buildAppScriptFor(input), input.WorkspacePath, input.SpecPath, a.conformityPolicyFor(input), a.maxRoundsFor(input), a.timeoutMinutesFor(input), input.BaseSHA, a.verifyCommandFor(input), a.fastCheckCommandFor(input), buildOracleCommand, "", input.Thinking, harnessArg(input.Harness), input.SetupCommands...)
	// The build script is the only reader of the autofix list (see
	// TestOnlyTheBuildReadsAutofix): it runs the commands inside each round.
	args = append(args, autofixArgs(input.AutofixCommands)...)
	runCtx, args := withEarlierWorkArgs(ctx, args, resumed.NotePath, input.EarlierAttemptPath, input.BaselineNotePath)
	args = append(args, resumeFromStateArgs(input)...)
	buildAppInterpreter := a.buildAppInterpreterFor(input)
	// Computed before recording intent (not after, as originally written)
	// so the intent record itself can carry the real command about to run
	// — see activityIntent's doc comment for why.
	command := append([]string{buildAppInterpreter}, args...)
	if _, err := recordActivityIntent(ctx, a.checkpointDirFor(input), "build", command); err != nil {
		return BuildActivityResult{}, temporal.NewApplicationErrorWithCause("record build Activity intent", InfrastructureFailureType, err)
	}
	logPath := activityLogPath(activityExecutionLogPath(ctx, a.logDirFor(input), "build_app.log"))
	// Mirrors cmd/factoryd: record every attempt as it
	// happens, before the caller decides whether to retry, so a killed or
	// otherwise incomplete invocation still gets partial evidence. See
	// BuildActivityResult.Attempts' doc comment for why this was
	// previously always empty for a Temporal-routed run.
	// Earlier Temporal attempts' evidence reaches the result and checkpoint only;
	// attempts (and so this attempt's journal) holds this attempt's own.
	inherited := a.earlierAttemptsFor(ctx, a.checkpointDirFor(input), "build")
	attempts := []run.Attempt{}
	// buildWorkerModelExtraJSON is relaySpec's own worker_model_extra_json
	// -- "" when this run has no relay at all (an offline build script) --
	// the exact model JSON this build attempt actually launched with, for
	// run.ExpectedReasoningEffort below. Read once here, not inside
	// afterAttempt, since relaySpec never changes across retries of this
	// same Activity.
	buildWorkerModelExtraJSON := ""
	if relaySpec != nil {
		buildWorkerModelExtraJSON = relaySpec.WorkerModelExtraJSON
	}
	beforeAttempt := func(attempt int) error {
		// RFC3339, matching afterAttempt's own completed-Attempt
		// StartedAt format below exactly (not RFC3339Nano) — found via
		// review: RecoverAttemptsFromCheckpointDir sorts attempts by
		// comparing these StartedAt strings lexically, which only equals
		// chronological order when every attempt uses the same
		// precision. A synthesized in-flight attempt recovered from this
		// intent record used to carry sub-second precision while a real
		// completed sibling attempt's StartedAt did not, so — for two
		// attempts started within the same whole second — the intent's
		// fractional digit ('.', which sorts before 'Z') made it compare
		// as earlier even when the completed attempt genuinely started
		// first, silently reordering Attempts.
		_, err := recordActivityAttemptIntent(ctx, a.checkpointDirFor(input), attempt, "build", command, time.Now().UTC().Format(time.RFC3339))
		return err
	}
	beforeAttempt = a.leaseChecked(ctx, a.checkpointDirFor(input), beforeAttempt)
	afterAttempt := func(attempt int, res runner.Result, attemptErr error) error {
		attempts = append(attempts, run.Attempt{
			Kind:                  "build",
			ResumedFromCheckpoint: resumed.SnapshotSHA,
			Command:               res.Command,
			SetupSHA256:           run.SetupDigest(input.SetupCommands),
			FactoryDirSHA256:      res.FactoryDirSHA256, FactoryDirCommit: res.FactoryDirCommit, FactoryDirError: res.FactoryDirError,
			StartedAt:            res.StartedAt.Format(time.RFC3339),
			FinishedAt:           res.FinishedAt.Format(time.RFC3339),
			ExitCode:             res.ExitCode,
			LogPath:              logPath(attempt),
			ImageDigest:          res.ImageDigest,
			HarnessScriptsSHA256: res.ScriptsSHA256,
			Skills:               res.Skills,
			SkillsSHA256:         res.SkillsSHA256,
			RepoSkills:           res.RepoSkills,
			// Role/Thinking: roles.execution's own resolved values,
			// threaded through RunWorkflowInput.Thinking (see its own doc
			// comment) -- every build round, corrective retries included,
			// is still Kind "build" and runs under roles.execution.
			// ExpectedEffort: what Pi's own thinkingLevelMap translation
			// (if any) actually turns Thinking into -- see
			// run.ExpectedReasoningEffort's own doc comment for why this,
			// not Thinking directly, is what a clamp-hint comparison needs.
			Role:           run.AttemptRoleExecution,
			Harness:        harnessArg(input.Harness),
			Thinking:       input.Thinking,
			ExpectedEffort: run.ExpectedReasoningEffort(input.Thinking, buildWorkerModelExtraJSON),
			// Relay evidence, mirroring cmd/factoryd: which
			// relay image, network, container, and upstream this attempt's
			// model traffic was actually confined to. Empty for a run with no
			// relay, and always empty for verification/full-suite attempts,
			// which never get one.
			RelayImageDigest:            res.RelayImageDigest,
			RelayNetwork:                res.RelayNetworkName,
			RelayContainerName:          res.RelayContainerName,
			RelayUpstream:               res.RelayUpstream,
			RelayCredentialMode:         res.RelayCredentialMode,
			RelayRoute:                  res.RelayRoute,
			RelayBilling:                res.RelayBilling,
			RelayWorkerModelID:          res.RelayWorkerModelID,
			RelayReasoningEffort:        res.RelayReasoningEffort,
			RelayReasoningEffortAnomaly: res.RelayReasoningEffortAnomaly,
			// Actual relay spend, not just what the worker could reach --
			// see run.Attempt's own doc comment on these four fields.
			RelayConsumedInputTokens:  res.RelayConsumedInputTokens,
			RelayConsumedOutputTokens: res.RelayConsumedOutputTokens,
			RelayConsumedCostMicroUSD: res.RelayConsumedCostMicroUSD,
			RelayCeilingExceeded:      res.RelayCeilingExceeded,
			RelaySpendPartial:         res.RelaySpendPartial,
			// safety-contract.md SC-012: the oracle content hash is
			// recorded with the attempt that ran against it, the same as
			// cmd/factoryd's buildAttempt. Empty unless the in-loop
			// oracle was mounted into this build.
			ReferenceOracleSHA256: buildOracleSHA256,
		})
		if err := saveActivityAttemptJournal(ctx, a.checkpointDirFor(input), "build", attempts); err != nil {
			return err
		}
		return nil
	}
	buildHeartbeatStart := time.Now()
	subResult, runErr := heartbeatWhileRunning(activityHeartbeatInterval, func() {
		activity.RecordHeartbeat(ctx, HeartbeatDetails{Stage: "build", Elapsed: time.Since(buildHeartbeatStart)})
	}, func() (runner.Result, error) {
		if a.hasFakeRunner() {
			return a.runWithRetriesFn()(
				ctx,
				input.WorkspacePath,
				logPath,
				a.buildMaxAttemptsFor(input),
				beforeAttempt,
				afterAttempt,
				buildAppInterpreter,
				args...,
			)
		}
		skills, err := a.boundSkills(relayRoleExecution, input.Skills)
		if err != nil {
			return runner.Result{}, err
		}
		return a.runSandboxWithRetries(forBuildLaunch(runCtx), input, logPath, a.buildMaxAttemptsFor(input), beforeAttempt, afterAttempt, relaySpec, registrySpec, composeSpec, buildOracleDir, buildOracleMountPath, executionHarnessEnv(input), skills, buildAppInterpreter, args...)
	})
	runErr = a.dropFinishedBuildSession(ctx, input, runErr)
	result := BuildActivityResult{Result: subResult, Attempts: withInherited(inherited, attempts)}
	checkpoint.Result = result
	if runErr != nil {
		checkpoint.Error = "build subprocess infrastructure failure: " + runErr.Error()
		checkpoint.ErrorType = buildActivityErrorType(runErr)
	}
	// result.Attempts is attached as this error's Details on every failure
	// return below (found via review): Temporal does not deliver an
	// Activity's return value to its caller alongside a non-nil error,
	// only the error itself — so returning `result` here is not enough to
	// get this attempt evidence to RunWorkflow when the Activity fails.
	// ApplicationError.Details is the mechanism Temporal provides for
	// carrying structured data across that boundary specifically when an
	// Activity fails; RunWorkflow recovers it via AttemptsFromError.
	if err := saveActivityCheckpoint(path, checkpoint, activity.GetInfo(ctx).Attempt); err != nil {
		return result, temporal.NewApplicationErrorWithCause("save build Activity checkpoint", buildActivityErrorType(runErr), err, result.Attempts)
	}
	if runErr != nil {
		return result, temporal.NewApplicationErrorWithCause("build subprocess infrastructure failure", buildActivityErrorType(runErr), runErr, result.Attempts)
	}
	return result, nil
}

// buildActivityErrorType classifies a RunBuildActivity subprocess failure
// for Temporal's ApplicationError type -- InfrastructureFailureType by
// default, CleanupUnconfirmedFailureType for an unconfirmed sandbox/relay
// teardown, ComposeServicesRejectedFailureType for a rejected target-repo
// compose file, or RelayCeilingExceededFailureType when the failure is this
// run's relay legitimately crossing its configured absolute ceiling (see
// sandbox.ErrRelayCeilingExceeded's own doc comment) -- distinct from both
// of the others so a caller several hops up this error's wrapping chain
// (RunWorkflow, cmd/factoryd, via RelayCeilingExceededFromError) can tell a
// run that hit its own budget apart from one whose teardown was merely
// ambiguous, or one that failed for an unrelated infrastructure reason.
func buildActivityErrorType(runErr error) string {
	switch {
	case errors.Is(runErr, sandbox.ErrSandboxRerun):
		// Before the ceiling: a command killed mid-request leaves its
		// request unsettled, which is a consequence, not the cause.
		return SandboxRerunFailureType
	case errors.Is(runErr, sandbox.ErrRelayCeilingExceeded):
		return RelayCeilingExceededFailureType
	case errors.Is(runErr, sandbox.ErrComposeServicesRejected):
		return ComposeServicesRejectedFailureType
	default:
		return launchErrorType(runErr)
	}
}

// composeRejectedProgressDetail is the build stage's progress detail for a
// run halted on a rejected compose file, "" otherwise: the feed is where an
// operator watching the run first looks, and "fail" alone reads as a
// build that ran and failed.
func composeRejectedProgressDetail(err error) string {
	if errors.Is(err, sandbox.ErrComposeServicesRejected) {
		return "target repo's compose file rejected before the build started"
	}
	return ""
}

// PostBuildActivity runs right after RunBuildActivity, before anything
// else touches the workspace's git state: it verifies input.BaseSHA is
// still an ancestor of the workspace's current HEAD (see
// runner.GitIsAncestor's doc comment for why — found live, a real
// harness invocation can reset the workspace backward mid-run and commit
// on top of that older state), then — mirroring cmd/factoryd's direct
// path — commits any uncommitted diff as a safety net if the build
// succeeded but the agent left it uncommitted, since accepted work is a
// factory-owned guarantee, not an assumption about the agent.
//
// Checkpointed like RunBuildActivity/RunVerifyActivity, unlike an earlier
// version of this Activity: found via review, a plain "redispatch finds
// the workspace already clean and does nothing" claim was true of the
// *workspace state* but not of the *evidence* — on redispatch after a
// prior attempt's own safety-net commit (below) already ran, the
// workspace being clean now makes this Activity take the early "already
// clean" return path instead of the commit path, silently reporting
// CommittedByWorker=false even though the factory itself made that
// commit. The checkpoint records which path actually ran, not just
// whatever the workspace happens to look like whenever this Activity
// next executes.
func (a *Activities) PostBuildActivity(ctx context.Context, input PostBuildInput) (progressResult PostBuildResult, err error) {
	postBuildLogDir := input.LogDir
	if postBuildLogDir == "" {
		postBuildLogDir = a.LogDir
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
		if err != nil {
			outcome = "fail"
		}
		progressMark(ctx, postBuildLogDir, "post_build", "end", outcome, "")
	}()
	checkpointDir := a.resolveCheckpointDir(input.CheckpointDir, input.LogDir)
	if err := a.fenceAttempt(ctx, checkpointDir); err != nil {
		return PostBuildResult{}, err
	}
	checkpoint, path, found, err := loadActivityCheckpoint[PostBuildResult](ctx, checkpointDir)
	if err != nil {
		return PostBuildResult{}, checkpointLoadError("load post-build Activity checkpoint", err)
	}
	if found {
		if checkpoint.Error != "" {
			return checkpoint.Result, attachCommittedDetail(temporal.NewApplicationError(checkpoint.Error, InfrastructureFailureType), checkpoint.Result.CommittedByWorker)
		}
		return checkpoint.Result, nil
	}
	progressStarted = true
	progressMark(ctx, postBuildLogDir, "post_build", "start", "", "")

	// Same two-phase intent protocol as RunBuildActivity/RunVerifyActivity,
	// closing the crash window between the safety-net commit succeeding and
	// the checkpoint below persisting: found via review, without this a
	// worker that dies in that window loses no workspace state (the commit
	// is already there) but loses the *evidence* — redispatch would see no
	// checkpoint, re-run runPostBuild, find the workspace already clean from
	// the prior attempt's own commit, and silently report
	// CommittedByWorker=false, the exact bug this checkpointing was added to
	// fix. Only runPostBuild's actual commit path records intent (see
	// below), since the other return paths have no side effect to lose.
	intentFound, err := priorActivityIntent(ctx, checkpointDir)
	if err != nil {
		return PostBuildResult{}, temporal.NewApplicationErrorWithCause("load post-build Activity intent", InfrastructureFailureType, err)
	}
	if intentFound {
		return PostBuildResult{}, temporal.NewApplicationError(
			"a prior attempt of this post-build Activity recorded intent to commit the verified diff but crashed before reaching a durable checkpoint — halting rather than risk misreporting CommittedByWorker",
			AmbiguousPriorAttemptType,
		)
	}

	result, activityErr := a.runPostBuild(ctx, checkpointDir, input)
	checkpoint.Result = result
	if activityErr != nil {
		// Same convention as RunBuildActivity's own checkpoint.Error:
		// the cached-found path above always re-wraps this as
		// InfrastructureFailureType regardless of the original error's
		// own type, so nothing is lost by not preserving it here either.
		checkpoint.Error = activityErr.Error()
	}
	if err := saveActivityCheckpoint(path, checkpoint, activity.GetInfo(ctx).Attempt); err != nil {
		return result, attachCommittedDetail(temporal.NewApplicationErrorWithCause("save post-build Activity checkpoint", InfrastructureFailureType, err), result.CommittedByWorker)
	}
	return result, attachCommittedDetail(activityErr, result.CommittedByWorker)
}

// runPostBuild is PostBuildActivity's actual logic, factored out so the
// checkpoint wrapper above can save its result (success or failure)
// uniformly regardless of which of these paths returned.
func (a *Activities) runPostBuild(ctx context.Context, checkpointDir string, input PostBuildInput) (PostBuildResult, error) {
	postBuildHEAD, err := runner.GitRevParseHEAD(input.WorkspacePath)
	if err != nil {
		return PostBuildResult{}, temporal.NewApplicationErrorWithCause("capture post-build HEAD", InfrastructureFailureType, err)
	}
	isAncestor, err := runner.GitIsAncestor(input.WorkspacePath, input.BaseSHA, postBuildHEAD)
	if err != nil {
		return PostBuildResult{}, temporal.NewApplicationErrorWithCause("check base_sha ancestry", InfrastructureFailureType, err)
	}
	if !isAncestor {
		return PostBuildResult{}, temporal.NewApplicationError(
			fmt.Sprintf("workspace history no longer contains base_sha %s as an ancestor of HEAD %s — the workspace was rolled back or rewritten during this run", input.BaseSHA, postBuildHEAD),
			InfrastructureFailureType,
		)
	}

	result := PostBuildResult{ResultSHA: postBuildHEAD}
	if input.BuildExitCode != 0 {
		return result, nil
	}
	clean, err := runner.GitIsClean(input.WorkspacePath)
	if err != nil {
		return PostBuildResult{}, temporal.NewApplicationErrorWithCause("check workspace cleanliness", InfrastructureFailureType, err)
	}
	if clean {
		return result, nil
	}
	// N2: the ticket's own "## Goal" paragraph (the same helper
	// cmd/factoryd's pullRequestTitle/direct-run auto-commit use), so a
	// squash merge's own subject says what the ticket actually did instead
	// of the generic "ticket: apply verified change" every such merge used
	// to share -- falls back to the old generic subject when SpecPath has
	// no usable Goal section (or predates this field).
	subject := ticketspec.GoalTitle(input.SpecPath)
	if subject == "" {
		subject = "ticket: apply verified change"
	}
	msg := subject + "\n\nAuto-committed by the Temporal Worker: build_app.py reported success but\nthe agent left its diff uncommitted."
	if _, err := recordActivityIntent(ctx, checkpointDir, "post-build-commit", []string{"git", "-C", input.WorkspacePath, "-c", "core.hooksPath=/dev/null", "commit", "-m", msg}); err != nil {
		return PostBuildResult{}, temporal.NewApplicationErrorWithCause("record post-build-commit Activity intent", InfrastructureFailureType, err)
	}
	if err := runner.GitCommitAll(input.WorkspacePath, msg); err != nil {
		return PostBuildResult{}, temporal.NewApplicationErrorWithCause("commit verified diff", InfrastructureFailureType, err)
	}
	result.CommittedByWorker = true
	result.ResultSHA, err = runner.GitRevParseHEAD(input.WorkspacePath)
	if err != nil {
		// CommittedByWorker=true, not the zero-valued PostBuildResult{}
		// every other failure return above uses: the commit above already
		// landed by the time this specific step can fail, and discarding
		// that fact here is exactly the commit-then-checkpoint/error
		// window CollectEvidenceActivity's own analogous fix closed (see
		// its doc comment) — found via the same review round.
		return PostBuildResult{CommittedByWorker: true}, temporal.NewApplicationErrorWithCause("capture result SHA after safety-net commit", InfrastructureFailureType, err)
	}
	return result, nil
}
