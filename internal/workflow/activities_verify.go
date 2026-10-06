package workflow

import (
	"buildgate/internal/evidence"
	"buildgate/internal/oraclecanary"
	"buildgate/internal/run"
	"buildgate/internal/runner"
	"buildgate/internal/sandbox"
	"context"
	"errors"
	"os"
	"path/filepath"
	"time"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"
)

// RunVerifyActivity invokes the canonical verification command through
// runner.RunWithRetries. Verification can rewrite files, so redispatch also
// returns its completed durable checkpoint instead of running it again.
func (a *Activities) RunVerifyActivity(ctx context.Context, input RunWorkflowInput) (progressResult VerifyActivityResult, err error) {
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
		progressMark(ctx, a.logDirFor(input), "verify", "end", outcome, "")
	}()
	if err := a.fenceEarlierAttempts(ctx, input, true); err != nil {
		return VerifyActivityResult{}, err
	}
	checkpoint, path, found, err := loadRetriedActivityCheckpoint[VerifyActivityResult](ctx, a.checkpointDirFor(input))
	if err != nil {
		return VerifyActivityResult{}, checkpointLoadError("load verify Activity checkpoint", err)
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
	progressMark(ctx, a.logDirFor(input), "verify", "start", "", "")

	// Resolved before any intent record or Docker contact, as in
	// RunBuildActivity: verification gets the same registry proxy the build
	// had (see registryProxySpecFor).
	registrySpec, err := a.registryProxySpecFor(input)
	if err != nil {
		return VerifyActivityResult{}, temporal.NewApplicationError(err.Error(), InfrastructureFailureType)
	}
	composeSpec, err := a.composeServicesSpecFor(input, "verify")
	if err != nil {
		return VerifyActivityResult{}, temporal.NewApplicationError(err.Error(), InfrastructureFailureType)
	}

	// Same two-phase intent protocol as RunBuildActivity, closing the same
	// crash window for the verify subprocess: see its comment for why this
	// halts instead of re-running or trusting an unconfirmed result.
	intentFound, journalFound, err := priorAttemptRecords(ctx, a.checkpointDirFor(input))
	if err != nil {
		return VerifyActivityResult{}, priorRecordsFailure("verify", err)
	}
	if intentFound {
		return VerifyActivityResult{}, temporal.NewApplicationError(
			"a prior attempt of this verify Activity recorded intent to run canonical verification but crashed before reaching a durable checkpoint — halting rather than risk a duplicate invocation or trusting an unconfirmed result",
			AmbiguousPriorAttemptType,
		)
	}
	if journalFound {
		return VerifyActivityResult{}, temporal.NewApplicationError(
			"a verify Activity attempt journal exists without its intent or completed checkpoint — halting rather than risk a duplicate invocation",
			AmbiguousPriorAttemptType,
		)
	}
	if _, err := recordActivityIntent(ctx, a.checkpointDirFor(input), "verify", []string{"sh", "-c", a.verifyCommandFor(input)}); err != nil {
		return VerifyActivityResult{}, temporal.NewApplicationErrorWithCause("record verify Activity intent", InfrastructureFailureType, err)
	}

	logPath := activityLogPath(activityExecutionLogPath(ctx, a.logDirFor(input), "verify.log"))
	// Mirrors cmd/factoryd's — see RunBuildActivity's matching
	// comment for why this was previously always empty here too.
	// Earlier Temporal attempts' evidence reaches the result and checkpoint only;
	// attempts (and so this attempt's journal) holds this attempt's own.
	inherited := a.earlierAttemptsFor(ctx, a.checkpointDirFor(input), "verify")
	attempts := []run.Attempt{}
	command := []string{"sh", "-c", a.verifyCommandFor(input)}
	beforeAttempt := func(attempt int) error {
		// RFC3339, not RFC3339Nano — see RunBuildActivity's matching
		// beforeAttempt comment for why the precision must match
		// afterAttempt's completed-Attempt StartedAt format exactly.
		_, err := recordActivityAttemptIntent(ctx, a.checkpointDirFor(input), attempt, "verify", command, time.Now().UTC().Format(time.RFC3339))
		return err
	}
	beforeAttempt = a.leaseChecked(ctx, a.checkpointDirFor(input), beforeAttempt)
	afterAttempt := func(attempt int, res runner.Result, _ error) error {
		attempts = append(attempts, run.Attempt{
			Kind:        "verify",
			Command:     res.Command,
			StartedAt:   res.StartedAt.Format(time.RFC3339),
			FinishedAt:  res.FinishedAt.Format(time.RFC3339),
			ExitCode:    res.ExitCode,
			LogPath:     logPath(attempt),
			ImageDigest: res.ImageDigest,
		})
		return saveActivityAttemptJournal(ctx, a.checkpointDirFor(input), "verify", attempts)
	}
	verifyHeartbeatStart := time.Now()
	result, runErr := heartbeatWhileRunning(activityHeartbeatInterval, func() {
		activity.RecordHeartbeat(ctx, HeartbeatDetails{Stage: "verify", Elapsed: time.Since(verifyHeartbeatStart)})
	}, func() (runner.Result, error) {
		if a.hasFakeRunner() {
			return a.runWithRetriesFn()(
				ctx,
				input.WorkspacePath,
				logPath,
				a.verifyMaxAttemptsFor(input),
				beforeAttempt,
				afterAttempt,
				"sh", "-c", a.verifyCommandFor(input),
			)
		}
		return a.runSandboxWithRetries(ctx, input, logPath, a.verifyMaxAttemptsFor(input), beforeAttempt, afterAttempt,
			// nil: canonical verification never calls a model, so it
			// never gets the relay's network -- it launches with
			// Network "none" even on a relay-contained run (unless a
			// registry proxy gives it that proxy's network instead).
			nil, registrySpec, composeSpec, "", "", nil, nil, "sh", "-c", a.verifyCommandFor(input))
	})
	verifyResult := VerifyActivityResult{Result: result, Attempts: withInherited(inherited, attempts)}
	if runErr == nil {
		verifyResult.DurationMs = result.FinishedAt.Sub(result.StartedAt).Milliseconds()
		verifyResult.LogSHA256, err = evidence.SHA256File(result.LogPath)
	}
	if runErr != nil {
		checkpoint.Error = "verify subprocess infrastructure failure: " + runErr.Error()
		if errors.Is(runErr, sandbox.ErrCleanupUnconfirmed) {
			checkpoint.ErrorType = CleanupUnconfirmedFailureType
		}
	} else if err != nil {
		checkpoint.Error = "verify evidence infrastructure failure: " + err.Error()
	}
	checkpoint.Result = verifyResult
	// verifyResult.Attempts is attached as this error's Details on every
	// failure return below — see RunBuildActivity's matching comment for
	// why.
	if saveErr := saveActivityCheckpoint(path, checkpoint, activity.GetInfo(ctx).Attempt); saveErr != nil {
		errType := InfrastructureFailureType
		if errors.Is(runErr, sandbox.ErrCleanupUnconfirmed) {
			errType = CleanupUnconfirmedFailureType
		}
		return verifyResult, temporal.NewApplicationErrorWithCause("save verify Activity checkpoint", errType, saveErr, verifyResult.Attempts)
	}
	if runErr != nil {
		errType := InfrastructureFailureType
		if errors.Is(runErr, sandbox.ErrCleanupUnconfirmed) {
			errType = CleanupUnconfirmedFailureType
		}
		return verifyResult, temporal.NewApplicationErrorWithCause("verify subprocess infrastructure failure", errType, runErr, verifyResult.Attempts)
	}
	if err != nil {
		return verifyResult, temporal.NewApplicationErrorWithCause("verify evidence infrastructure failure", InfrastructureFailureType, err, verifyResult.Attempts)
	}
	return verifyResult, nil
}

// RunFullSuiteVerifyActivity invokes input.FullSuiteCommand — cmd/factoryd's
// -full-suite-command ported to the Temporal path (gap 3 of the plan's
// 2026-08-28 readiness review, the regression oracle) — through
// runner.RunWithRetries with a single attempt, no per-execution/Worker-static
// override split like verifyCommandFor: RunWorkflow only ever calls this
// Activity when RunWorkflowInput.FullSuiteCommand is itself declared (see
// that field's own doc comment for why the decision to call this Activity at
// all must come from workflow input, not Worker config), so input.
// FullSuiteCommand is always the value to use. Same checkpoint/two-phase
// intent protocol as RunVerifyActivity, keyed by this Activity's own
// ActivityID so it cannot collide with RunVerifyActivity's checkpoint for
// the same execution.
func (a *Activities) RunFullSuiteVerifyActivity(ctx context.Context, input RunWorkflowInput) (progressResult VerifyActivityResult, err error) {
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
		progressMark(ctx, a.logDirFor(input), "full_suite", "end", outcome, "")
	}()
	if err := a.fenceEarlierAttempts(ctx, input, true); err != nil {
		return VerifyActivityResult{}, err
	}
	checkpoint, path, found, err := loadRetriedActivityCheckpoint[VerifyActivityResult](ctx, a.checkpointDirFor(input))
	if err != nil {
		return VerifyActivityResult{}, checkpointLoadError("load full-suite verify Activity checkpoint", err)
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
	progressMark(ctx, a.logDirFor(input), "full_suite", "start", "", "")

	registrySpec, err := a.registryProxySpecFor(input)
	if err != nil {
		return VerifyActivityResult{}, temporal.NewApplicationError(err.Error(), InfrastructureFailureType)
	}
	composeSpec, err := a.composeServicesSpecFor(input, "full_suite_verify")
	if err != nil {
		return VerifyActivityResult{}, temporal.NewApplicationError(err.Error(), InfrastructureFailureType)
	}

	// Same two-phase intent protocol as RunVerifyActivity, closing the same
	// crash window for the full-suite subprocess.
	intentFound, journalFound, err := priorAttemptRecords(ctx, a.checkpointDirFor(input))
	if err != nil {
		return VerifyActivityResult{}, priorRecordsFailure("full-suite verify", err)
	}
	if intentFound {
		return VerifyActivityResult{}, temporal.NewApplicationError(
			"a prior attempt of this full-suite verify Activity recorded intent to run the full-suite command but crashed before reaching a durable checkpoint — halting rather than risk a duplicate invocation or trusting an unconfirmed result",
			AmbiguousPriorAttemptType,
		)
	}
	if journalFound {
		return VerifyActivityResult{}, temporal.NewApplicationError(
			"a full-suite verify Activity attempt journal exists without its intent or completed checkpoint — halting rather than risk a duplicate invocation",
			AmbiguousPriorAttemptType,
		)
	}
	if _, err := recordActivityIntent(ctx, a.checkpointDirFor(input), "full_suite_verify", []string{"sh", "-c", input.FullSuiteCommand}); err != nil {
		return VerifyActivityResult{}, temporal.NewApplicationErrorWithCause("record full-suite verify Activity intent", InfrastructureFailureType, err)
	}

	logPath := activityLogPath(activityExecutionLogPath(ctx, a.logDirFor(input), "full_suite_verify.log"))
	// Earlier Temporal attempts' evidence reaches the result and checkpoint only;
	// attempts (and so this attempt's journal) holds this attempt's own.
	inherited := a.earlierAttemptsFor(ctx, a.checkpointDirFor(input), "full_suite_verify")
	attempts := []run.Attempt{}
	command := []string{"sh", "-c", input.FullSuiteCommand}
	beforeAttempt := func(attempt int) error {
		// RFC3339, not RFC3339Nano — see RunBuildActivity's matching
		// beforeAttempt comment for why the precision must match
		// afterAttempt's completed-Attempt StartedAt format exactly.
		_, err := recordActivityAttemptIntent(ctx, a.checkpointDirFor(input), attempt, "full_suite_verify", command, time.Now().UTC().Format(time.RFC3339))
		return err
	}
	beforeAttempt = a.leaseChecked(ctx, a.checkpointDirFor(input), beforeAttempt)
	afterAttempt := func(attempt int, res runner.Result, _ error) error {
		attempts = append(attempts, run.Attempt{
			Kind:        "full_suite_verify",
			Command:     res.Command,
			StartedAt:   res.StartedAt.Format(time.RFC3339),
			FinishedAt:  res.FinishedAt.Format(time.RFC3339),
			ExitCode:    res.ExitCode,
			LogPath:     logPath(attempt),
			ImageDigest: res.ImageDigest,
		})
		return saveActivityAttemptJournal(ctx, a.checkpointDirFor(input), "full_suite_verify", attempts)
	}
	fullSuiteHeartbeatStart := time.Now()
	result, runErr := heartbeatWhileRunning(activityHeartbeatInterval, func() {
		activity.RecordHeartbeat(ctx, HeartbeatDetails{Stage: "full_suite_verify", Elapsed: time.Since(fullSuiteHeartbeatStart)})
	}, func() (runner.Result, error) {
		if a.hasFakeRunner() {
			return a.runWithRetriesFn()(
				ctx,
				input.WorkspacePath,
				logPath,
				1,
				beforeAttempt,
				afterAttempt,
				"sh", "-c", input.FullSuiteCommand,
			)
		}
		return a.runSandboxWithRetries(ctx, input, logPath, 1, beforeAttempt, afterAttempt,
			// nil, for the same reason canonical verification passes
			// nil: the full-suite gate never calls a model.
			nil, registrySpec, composeSpec, "", "", nil, nil, "sh", "-c", input.FullSuiteCommand)
	})
	fullSuiteResult := VerifyActivityResult{Result: result, Attempts: withInherited(inherited, attempts)}
	if runErr == nil {
		fullSuiteResult.DurationMs = result.FinishedAt.Sub(result.StartedAt).Milliseconds()
		fullSuiteResult.LogSHA256, err = evidence.SHA256File(result.LogPath)
	}
	if runErr != nil {
		checkpoint.Error = "full-suite verify subprocess infrastructure failure: " + runErr.Error()
		if errors.Is(runErr, sandbox.ErrCleanupUnconfirmed) {
			checkpoint.ErrorType = CleanupUnconfirmedFailureType
		}
	} else if err != nil {
		checkpoint.Error = "full-suite verify evidence infrastructure failure: " + err.Error()
	}
	checkpoint.Result = fullSuiteResult
	if saveErr := saveActivityCheckpoint(path, checkpoint, activity.GetInfo(ctx).Attempt); saveErr != nil {
		errType := InfrastructureFailureType
		if errors.Is(runErr, sandbox.ErrCleanupUnconfirmed) {
			errType = CleanupUnconfirmedFailureType
		}
		return fullSuiteResult, temporal.NewApplicationErrorWithCause("save full-suite verify Activity checkpoint", errType, saveErr, fullSuiteResult.Attempts)
	}
	if runErr != nil {
		errType := InfrastructureFailureType
		if errors.Is(runErr, sandbox.ErrCleanupUnconfirmed) {
			errType = CleanupUnconfirmedFailureType
		}
		return fullSuiteResult, temporal.NewApplicationErrorWithCause("full-suite verify subprocess infrastructure failure", errType, runErr, fullSuiteResult.Attempts)
	}
	if err != nil {
		return fullSuiteResult, temporal.NewApplicationErrorWithCause("full-suite verify evidence infrastructure failure", InfrastructureFailureType, err, fullSuiteResult.Attempts)
	}
	return fullSuiteResult, nil
}

// NamedGateActivityInput is RunNamedGateActivity's input: the whole
// RunWorkflowInput (workspace, sandbox, checkpoint/log directories) plus
// which named gate this particular Activity execution is for. Check
// names the gate ("lint", "security_audit", "unit_tests",
// "integration_tests") and doubles as this Activity's checkpoint/journal
// kind, log-file name, and Attempt.Kind; Command is the shell command to
// run. RunWorkflow calls this Activity once per configured gate, each its
// own ActivityID, so their checkpoints/journals never collide even though
// they share this one Activity implementation.
type NamedGateActivityInput struct {
	RunWorkflowInput
	Check   string `json:"check"`
	Command string `json:"command"`
	// OracleCanary is set by the workflow only for the reference_oracle gate when
	// its "reference-oracle-canary-timeout" marker resolved to >= 1. Only then does
	// the Activity run the runtime canary and halve its deadline between the two
	// runs; unset (an Activity scheduled before the canary existed, with the old
	// single-command timeout) it behaves exactly as it did: one run, full budget.
	OracleCanary bool `json:"oracle_canary,omitempty"`
}

// RunNamedGateActivity invokes input.Command through runner.RunWithRetries
// with a single attempt -- the lint/security_audit/unit_tests/
// integration_tests named gates ported to the Temporal path, one Activity
// implementation shared by all four (see NamedGateActivityInput's own doc
// comment). Same checkpoint/two-phase intent protocol as
// RunFullSuiteVerifyActivity, keyed by this Activity's own ActivityID so
// it cannot collide with any sibling gate's checkpoint for the same
// execution.
func (a *Activities) RunNamedGateActivity(ctx context.Context, input NamedGateActivityInput) (progressResult VerifyActivityResult, err error) {
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
		progressMark(ctx, a.logDirFor(input.RunWorkflowInput), "gate", "end", outcome, input.Check)
	}()
	if err := a.fenceEarlierAttempts(ctx, input.RunWorkflowInput, true); err != nil {
		return VerifyActivityResult{}, err
	}
	checkpoint, path, found, err := loadRetriedActivityCheckpoint[VerifyActivityResult](ctx, a.checkpointDirFor(input.RunWorkflowInput))
	if err != nil {
		return VerifyActivityResult{}, checkpointLoadError("load "+input.Check+" gate Activity checkpoint", err)
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
	progressMark(ctx, a.logDirFor(input.RunWorkflowInput), "gate", "start", "", input.Check)

	registrySpec, err := a.registryProxySpecFor(input.RunWorkflowInput)
	if err != nil {
		return VerifyActivityResult{}, temporal.NewApplicationError(err.Error(), InfrastructureFailureType)
	}
	composeSpec, err := a.composeServicesSpecFor(input.RunWorkflowInput, input.Check)
	if err != nil {
		return VerifyActivityResult{}, temporal.NewApplicationError(err.Error(), InfrastructureFailureType)
	}

	intentFound, journalFound, err := priorAttemptRecords(ctx, a.checkpointDirFor(input.RunWorkflowInput))
	if err != nil {
		return VerifyActivityResult{}, priorRecordsFailure(input.Check+" gate", err)
	}
	if intentFound {
		return VerifyActivityResult{}, temporal.NewApplicationError(
			"a prior attempt of this "+input.Check+" gate Activity recorded intent to run the gate command but crashed before reaching a durable checkpoint — halting rather than risk a duplicate invocation or trusting an unconfirmed result",
			AmbiguousPriorAttemptType,
		)
	}
	if journalFound {
		return VerifyActivityResult{}, temporal.NewApplicationError(
			"a "+input.Check+" gate Activity attempt journal exists without its intent or completed checkpoint — halting rather than risk a duplicate invocation",
			AmbiguousPriorAttemptType,
		)
	}
	if _, err := recordActivityIntent(ctx, a.checkpointDirFor(input.RunWorkflowInput), input.Check, []string{"sh", "-c", input.Command}); err != nil {
		return VerifyActivityResult{}, temporal.NewApplicationErrorWithCause("record "+input.Check+" gate Activity intent", InfrastructureFailureType, err)
	}

	logPath := activityLogPath(activityExecutionLogPath(ctx, a.logDirFor(input.RunWorkflowInput), input.Check+".log"))
	// Earlier Temporal attempts' evidence reaches the result and checkpoint only;
	// attempts (and so this attempt's journal) holds this attempt's own.
	inherited := a.earlierAttemptsFor(ctx, a.checkpointDirFor(input.RunWorkflowInput), input.Check)
	attempts := []run.Attempt{}
	command := []string{"sh", "-c", input.Command}

	// Temporal-path counterpart to cmd/factoryd's runGate (PR #151/#152's
	// review-driven fixes): only for the "reference_oracle" check, and
	// only when ReferenceOracleDir is configured, snapshot it into an
	// immutable, Activity-owned copy before either hashing or mounting --
	// see evidence.SnapshotTree's own doc comment for the TOCTOU gap this
	// closes. Whichever Worker actually executes this Activity needs
	// ReferenceOracleDir reachable on its own filesystem (inherent to a
	// shared task queue, same as -workspace itself), which is exactly
	// what's true here: this Worker is the one about to launch the
	// container that mounts it.
	// referenceOracleDir and referenceOracleMountPath are both local,
	// gated together on the SAME condition, and never read from
	// input.ReferenceOracleMountPath directly anywhere below -- found via
	// review: an earlier version passed input.ReferenceOracleMountPath
	// unconditionally into runSandboxWithRetries while only
	// referenceOracleDir was check-gated, so every OTHER named gate
	// (lint, security_audit, unit_tests, integration_tests) got a
	// mismatched ReferenceOracleDir=""/ReferenceOracleMountPath=<set>
	// pair whenever the oracle-mount feature was configured at all --
	// LaunchSpec.Validate's "must be set together" rule then rejected
	// every one of them, halting the run. cmd/factoryd
	// never had this risk: it dispatches to an entirely separate
	// closure (sandboxedForReferenceOracle) only for the reference_oracle
	// check, rather than sharing one call for every gate the way this
	// Activity does.
	var referenceOracleDir, referenceOracleMountPath, referenceOracleSHA256 string
	if input.Check == "reference_oracle" {
		// Set for this check UNCONDITIONALLY, not gated on
		// input.ReferenceOracleDir != "" -- found via review, round 2:
		// the previous version skipped this whole assignment whenever
		// ReferenceOracleDir was empty, so -reference-oracle-mount-path
		// configured WITHOUT -reference-oracle-dir passed BOTH fields
		// through as empty, which LaunchSpec.Validate reads as plain
		// "not configured" rather than the actual misconfiguration --
		// silently running with no mount at all instead of failing
		// loudly. Setting it here unconditionally lets a mismatched
		// pair reach Validate's "must be set together" rule for real.
		referenceOracleMountPath = input.ReferenceOracleMountPath
	}
	if input.Check == "reference_oracle" && input.ReferenceOracleDir != "" {
		snapshotDir, absErr := filepath.Abs(filepath.Join(a.logDirFor(input.RunWorkflowInput), "reference-oracle-snapshot"))
		if absErr != nil {
			return VerifyActivityResult{}, temporal.NewApplicationErrorWithCause("resolve reference-oracle snapshot path", InfrastructureFailureType, absErr)
		}
		// Containment check, stale-snapshot clearing (a Temporal Activity
		// attempt can be retried after a crash between snapshotting and
		// its own durable checkpoint), snapshot, and hash all live in
		// sandbox.SnapshotReferenceOracle -- shared with this Activity file's own build Activity, see its doc comment.
		referenceOracleSHA256, err = sandbox.SnapshotReferenceOracle(input.WorkspacePath, input.ReferenceOracleDir, snapshotDir)
		if err != nil {
			return VerifyActivityResult{}, temporal.NewApplicationErrorWithCause("reference-oracle snapshot", InfrastructureFailureType, err)
		}
		defer os.RemoveAll(snapshotDir)
		referenceOracleDir = snapshotDir
	}

	// A run with an oracle also runs the runtime canary (see runCanary below)
	// inside this same Activity execution, so its single checkpoint covers both
	// runs. Each of the two runs gets at most half of this Activity's deadline
	// (the workflow doubles the gate's StartToCloseTimeout for exactly this
	// check), so a hung real run cannot consume the canary's time.
	canaryEligible := input.OracleCanary && input.Check == "reference_oracle" && referenceOracleDir != ""
	// half is a DURATION fixed once at Activity start; each run gets its own
	// fresh context.WithTimeout(ctx, half). Sharing one absolute deadline would
	// leave the canary only what the real run did not use and time it out.
	var half time.Duration
	gateCtx := ctx
	if canaryEligible {
		if deadline, ok := ctx.Deadline(); ok {
			half = time.Until(deadline) / 2
			var cancel context.CancelFunc
			gateCtx, cancel = context.WithTimeout(ctx, half)
			defer cancel()
		}
	}

	beforeAttempt := func(attempt int) error {
		_, err := recordActivityAttemptIntent(ctx, a.checkpointDirFor(input.RunWorkflowInput), attempt, input.Check, command, time.Now().UTC().Format(time.RFC3339))
		return err
	}
	beforeAttempt = a.leaseChecked(ctx, a.checkpointDirFor(input.RunWorkflowInput), beforeAttempt)
	afterAttempt := func(attempt int, res runner.Result, _ error) error {
		attempts = append(attempts, run.Attempt{
			Kind:                  input.Check,
			Command:               res.Command,
			StartedAt:             res.StartedAt.Format(time.RFC3339),
			FinishedAt:            res.FinishedAt.Format(time.RFC3339),
			ExitCode:              res.ExitCode,
			LogPath:               logPath(attempt),
			ImageDigest:           res.ImageDigest,
			ReferenceOracleSHA256: referenceOracleSHA256,
		})
		return saveActivityAttemptJournal(ctx, a.checkpointDirFor(input.RunWorkflowInput), input.Check, attempts)
	}
	namedGateHeartbeatStart := time.Now()
	result, runErr := heartbeatWhileRunning(activityHeartbeatInterval, func() {
		activity.RecordHeartbeat(ctx, HeartbeatDetails{Stage: input.Check, Elapsed: time.Since(namedGateHeartbeatStart)})
	}, func() (runner.Result, error) {
		if a.hasFakeRunner() {
			return a.runWithRetriesFn()(
				gateCtx,
				input.WorkspacePath,
				logPath,
				1,
				beforeAttempt,
				afterAttempt,
				"sh", "-c", input.Command,
			)
		}
		return a.runSandboxWithRetries(gateCtx, input.RunWorkflowInput, logPath, 1, beforeAttempt, afterAttempt,
			// nil, for the same reason canonical verification passes
			// nil: a named gate never calls a model.
			nil, registrySpec, composeSpec, referenceOracleDir, referenceOracleMountPath, nil, nil, "sh", "-c", input.Command)
	})

	// Runtime canary: a passing oracle command is only believed when the SAME
	// command, same sandbox and mount path, fails against a known-failing
	// snapshot of the oracle. Anything else (the command never executed the
	// oracle, the canary died of an environment error, no canary could be
	// built) fails the gate, closed.
	var canary *run.OracleCanaryEvidence
	if runErr == nil && canaryEligible && result.ExitCode == 0 {
		var ev run.OracleCanaryEvidence
		var trusted bool
		ev, trusted, runErr = a.runReferenceOracleCanary(ctx, half, input, &attempts, referenceOracleDir, referenceOracleMountPath, registrySpec, command)
		if runErr == nil {
			canary = &ev
			if err = oraclecanary.AppendNote(result.LogPath, ev); err == nil && !trusted {
				// A distinct non-zero code: policy sees a failed gate.
				result.ExitCode = 1
			}
		}
	}
	gateResult := VerifyActivityResult{Result: result, Attempts: withInherited(inherited, attempts), ReferenceOracleSHA256: referenceOracleSHA256, OracleCanary: canary}
	if runErr == nil && err == nil {
		gateResult.DurationMs = result.FinishedAt.Sub(result.StartedAt).Milliseconds()
		gateResult.LogSHA256, err = evidence.SHA256File(result.LogPath)
	}
	if runErr != nil {
		checkpoint.Error = input.Check + " gate subprocess infrastructure failure: " + runErr.Error()
		if errors.Is(runErr, sandbox.ErrCleanupUnconfirmed) {
			checkpoint.ErrorType = CleanupUnconfirmedFailureType
		}
	} else if err != nil {
		checkpoint.Error = input.Check + " gate evidence infrastructure failure: " + err.Error()
	}
	checkpoint.Result = gateResult
	if saveErr := saveActivityCheckpoint(path, checkpoint, activity.GetInfo(ctx).Attempt); saveErr != nil {
		errType := InfrastructureFailureType
		if errors.Is(runErr, sandbox.ErrCleanupUnconfirmed) {
			errType = CleanupUnconfirmedFailureType
		}
		return gateResult, temporal.NewApplicationErrorWithCause("save "+input.Check+" gate Activity checkpoint", errType, saveErr, gateResult.Attempts)
	}
	if runErr != nil {
		errType := InfrastructureFailureType
		if errors.Is(runErr, sandbox.ErrCleanupUnconfirmed) {
			errType = CleanupUnconfirmedFailureType
		}
		return gateResult, temporal.NewApplicationErrorWithCause(input.Check+" gate subprocess infrastructure failure", errType, runErr, gateResult.Attempts)
	}
	if err != nil {
		return gateResult, temporal.NewApplicationErrorWithCause(input.Check+" gate evidence infrastructure failure", InfrastructureFailureType, err, gateResult.Attempts)
	}
	return gateResult, nil
}
