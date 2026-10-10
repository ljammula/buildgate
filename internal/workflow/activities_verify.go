package workflow

import (
	"buildgate/internal/baseline"
	"buildgate/internal/evidence"
	"buildgate/internal/oraclecanary"
	"buildgate/internal/run"
	"buildgate/internal/runner"
	"buildgate/internal/sandbox"
	"buildgate/internal/triage"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"
)

// RunVerifyActivity invokes the canonical verification command through
// runner.RunWithRetries. Verification can rewrite files, so redispatch also
// returns its completed durable checkpoint instead of running it again.
func (a *Activities) RunVerifyActivity(ctx context.Context, input RunWorkflowInput) (VerifyActivityResult, error) {
	return a.runVerifyCommand(ctx, input, verifyRun{kind: "verify", label: "verify"})
}

// verifyRun says which of the verify command's two runs runVerifyCommand
// performs: canonical verification after the build (kind "verify"), or the
// baseline on the base commit before it (run.BaselineVerifyAttemptKind).
// kind is the progress stage, the intent and journal kind, the Attempt.Kind,
// the log's file name and the compose evidence phase; label is the run's
// name in an error message.
type verifyRun struct {
	kind  string
	label string
	// endDetail is the detail of the progress feed's end mark, from the
	// run's result; nil for none.
	endDetail func(VerifyActivityResult) string
}

// runVerifyCommand runs the verify command once in a fresh sandbox, as v
// describes.
func (a *Activities) runVerifyCommand(ctx context.Context, input RunWorkflowInput, v verifyRun) (progressResult VerifyActivityResult, err error) {
	// The stage "start" mark is deferred until past the checkpoint
	// short-circuit below: a redispatch of an already-completed Activity
	// must not add a spurious near-zero start/end pair to the feed.
	progressStarted := false
	defer func() {
		if progressStarted {
			v.markEnd(ctx, a.logDirFor(input), progressResult, err)
		}
	}()
	if err := a.fenceEarlierAttempts(ctx, input, true); err != nil {
		return VerifyActivityResult{}, err
	}
	checkpoint, path, found, err := loadRetriedActivityCheckpoint[VerifyActivityResult](ctx, a.checkpointDirFor(input))
	if err != nil {
		return VerifyActivityResult{}, checkpointLoadError("load "+v.label+" Activity checkpoint", err)
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
	progressMark(ctx, a.logDirFor(input), v.kind, "start", "", "")

	// Resolved before any intent record or Docker contact, as in
	// RunBuildActivity: verification gets the same registry proxy the build
	// had (see registryProxySpecFor).
	registrySpec, err := a.registryProxySpecFor(input)
	if err != nil {
		return VerifyActivityResult{}, temporal.NewApplicationError(err.Error(), InfrastructureFailureType)
	}
	composeSpec, err := a.composeServicesSpecFor(input, v.kind)
	if err != nil {
		return VerifyActivityResult{}, temporal.NewApplicationError(err.Error(), InfrastructureFailureType)
	}

	// Same two-phase intent protocol as RunBuildActivity, closing the same
	// crash window for the verify subprocess: see its comment for why this
	// halts instead of re-running or trusting an unconfirmed result.
	intentFound, journalFound, err := priorAttemptRecords(ctx, a.checkpointDirFor(input))
	if err != nil {
		return VerifyActivityResult{}, priorRecordsFailure(v.label, err)
	}
	if intentFound {
		return VerifyActivityResult{}, temporal.NewApplicationError(
			"a prior attempt of this "+v.label+" Activity recorded intent to run "+v.what()+" but crashed before reaching a durable checkpoint — halting rather than risk a duplicate invocation or trusting an unconfirmed result",
			AmbiguousPriorAttemptType,
		)
	}
	if journalFound {
		return VerifyActivityResult{}, temporal.NewApplicationError(
			"a "+v.label+" Activity attempt journal exists without its intent or completed checkpoint — halting rather than risk a duplicate invocation",
			AmbiguousPriorAttemptType,
		)
	}
	command := stepCommand(input.SetupCommands, a.verifyCommandFor(input))
	if _, err := recordActivityIntent(ctx, a.checkpointDirFor(input), v.kind, command); err != nil {
		return VerifyActivityResult{}, temporal.NewApplicationErrorWithCause("record "+v.label+" Activity intent", InfrastructureFailureType, err)
	}

	logPath := activityLogPath(activityExecutionLogPath(ctx, a.logDirFor(input), v.kind+".log"))
	// Mirrors cmd/factoryd's — see RunBuildActivity's matching
	// comment for why this was previously always empty here too.
	// Earlier Temporal attempts' evidence reaches the result and checkpoint only;
	// attempts (and so this attempt's journal) holds this attempt's own.
	inherited := a.earlierAttemptsFor(ctx, a.checkpointDirFor(input), v.kind)
	attempts := []run.Attempt{}
	beforeAttempt := func(attempt int) error {
		// RFC3339, not RFC3339Nano — see RunBuildActivity's matching
		// beforeAttempt comment for why the precision must match
		// afterAttempt's completed-Attempt StartedAt format exactly.
		_, err := recordActivityAttemptIntent(ctx, a.checkpointDirFor(input), attempt, v.kind, command, time.Now().UTC().Format(time.RFC3339))
		return err
	}
	beforeAttempt = a.leaseChecked(ctx, a.checkpointDirFor(input), beforeAttempt)
	afterAttempt := func(attempt int, res runner.Result, _ error) error {
		attempts = append(attempts, run.Attempt{
			Kind:             v.kind,
			Command:          res.Command,
			SetupSHA256:      run.SetupDigest(input.SetupCommands),
			FactoryDirSHA256: res.FactoryDirSHA256, FactoryDirCommit: res.FactoryDirCommit, FactoryDirError: res.FactoryDirError,
			StartedAt:   res.StartedAt.Format(time.RFC3339),
			FinishedAt:  res.FinishedAt.Format(time.RFC3339),
			ExitCode:    res.ExitCode,
			LogPath:     logPath(attempt),
			ImageDigest: res.ImageDigest,
		})
		return saveActivityAttemptJournal(ctx, a.checkpointDirFor(input), v.kind, attempts)
	}
	verifyHeartbeatStart := time.Now()
	result, runErr := heartbeatWhileRunning(activityHeartbeatInterval, func() {
		activity.RecordHeartbeat(ctx, HeartbeatDetails{Stage: v.kind, Elapsed: time.Since(verifyHeartbeatStart)})
	}, func() (runner.Result, error) {
		if a.hasFakeRunner() {
			return a.runWithRetriesFn()(
				ctx,
				input.WorkspacePath,
				logPath,
				a.verifyMaxAttemptsFor(input),
				beforeAttempt,
				afterAttempt,
				command[0], command[1:]...,
			)
		}
		return a.runSandboxWithRetries(ctx, input, logPath, a.verifyMaxAttemptsFor(input), beforeAttempt, afterAttempt,
			// nil: canonical verification never calls a model, so it
			// never gets the relay's network -- it launches with
			// Network "none" even on a relay-contained run (unless a
			// registry proxy gives it that proxy's network instead).
			nil, registrySpec, composeSpec, "", "", nil, nil, command[0], command[1:]...)
	})
	verifyResult := VerifyActivityResult{Result: result, Attempts: withInherited(inherited, attempts)}
	if runErr == nil {
		verifyResult.DurationMs = result.FinishedAt.Sub(result.StartedAt).Milliseconds()
		verifyResult.LogSHA256, err = evidence.SHA256File(result.LogPath)
	}
	if runErr != nil {
		checkpoint.Error = v.label + " subprocess infrastructure failure: " + runErr.Error()
		checkpoint.ErrorType = checkpointErrorType(runErr)
	} else if err != nil {
		checkpoint.Error = v.label + " evidence infrastructure failure: " + err.Error()
	}
	checkpoint.Result = verifyResult
	// verifyResult.Attempts is attached as this error's Details on every
	// failure return below — see RunBuildActivity's matching comment for
	// why.
	if saveErr := saveActivityCheckpoint(path, checkpoint, activity.GetInfo(ctx).Attempt); saveErr != nil {
		errType := launchErrorType(runErr)
		return verifyResult, temporal.NewApplicationErrorWithCause("save "+v.label+" Activity checkpoint", errType, saveErr, verifyResult.Attempts)
	}
	if runErr != nil {
		errType := launchErrorType(runErr)
		return verifyResult, temporal.NewApplicationErrorWithCause(v.label+" subprocess infrastructure failure", errType, runErr, verifyResult.Attempts)
	}
	if err != nil {
		return verifyResult, temporal.NewApplicationErrorWithCause(v.label+" evidence infrastructure failure", InfrastructureFailureType, err, verifyResult.Attempts)
	}
	return verifyResult, nil
}

// markEnd writes the run's end mark to the progress feed.
func (v verifyRun) markEnd(ctx context.Context, logDir string, result VerifyActivityResult, err error) {
	outcome := "pass"
	if err != nil || result.Result.ExitCode != 0 {
		outcome = "fail"
	}
	detail := ""
	if v.endDetail != nil && err == nil {
		detail = v.endDetail(result)
	}
	progressMark(ctx, logDir, v.kind, "end", outcome, detail)
}

// what names the command v runs, for the ambiguous-prior-attempt message.
func (v verifyRun) what() string {
	if v.kind == "verify" {
		return "canonical verification"
	}
	return "the verify command on the base commit"
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
	command := stepCommand(input.SetupCommands, input.FullSuiteCommand)
	if _, err := recordActivityIntent(ctx, a.checkpointDirFor(input), "full_suite_verify", command); err != nil {
		return VerifyActivityResult{}, temporal.NewApplicationErrorWithCause("record full-suite verify Activity intent", InfrastructureFailureType, err)
	}

	logPath := activityLogPath(activityExecutionLogPath(ctx, a.logDirFor(input), "full_suite_verify.log"))
	// Earlier Temporal attempts' evidence reaches the result and checkpoint only;
	// attempts (and so this attempt's journal) holds this attempt's own.
	inherited := a.earlierAttemptsFor(ctx, a.checkpointDirFor(input), "full_suite_verify")
	attempts := []run.Attempt{}
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
			Kind:             "full_suite_verify",
			Command:          res.Command,
			SetupSHA256:      run.SetupDigest(input.SetupCommands),
			FactoryDirSHA256: res.FactoryDirSHA256, FactoryDirCommit: res.FactoryDirCommit, FactoryDirError: res.FactoryDirError,
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
				command[0], command[1:]...,
			)
		}
		return a.runSandboxWithRetries(ctx, input, logPath, 1, beforeAttempt, afterAttempt,
			// nil, for the same reason canonical verification passes
			// nil: the full-suite gate never calls a model.
			nil, registrySpec, composeSpec, "", "", nil, nil, command[0], command[1:]...)
	})
	fullSuiteResult := VerifyActivityResult{Result: result, Attempts: withInherited(inherited, attempts)}
	if runErr == nil {
		fullSuiteResult.DurationMs = result.FinishedAt.Sub(result.StartedAt).Milliseconds()
		fullSuiteResult.LogSHA256, err = evidence.SHA256File(result.LogPath)
	}
	if runErr != nil {
		checkpoint.Error = "full-suite verify subprocess infrastructure failure: " + runErr.Error()
		checkpoint.ErrorType = checkpointErrorType(runErr)
	} else if err != nil {
		checkpoint.Error = "full-suite verify evidence infrastructure failure: " + err.Error()
	}
	checkpoint.Result = fullSuiteResult
	if saveErr := saveActivityCheckpoint(path, checkpoint, activity.GetInfo(ctx).Attempt); saveErr != nil {
		errType := launchErrorType(runErr)
		return fullSuiteResult, temporal.NewApplicationErrorWithCause("save full-suite verify Activity checkpoint", errType, saveErr, fullSuiteResult.Attempts)
	}
	if runErr != nil {
		errType := launchErrorType(runErr)
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
	// Before the checkpoint's early return: an attempt whose worker died
	// during the rerun on the base commit left a completed checkpoint and
	// the rerun's scratch worktree.
	a.sweepGateBaseWorktree(ctx, input)
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
	command := stepCommand(input.SetupCommands, input.Command)
	if _, err := recordActivityIntent(ctx, a.checkpointDirFor(input.RunWorkflowInput), input.Check, command); err != nil {
		return VerifyActivityResult{}, temporal.NewApplicationErrorWithCause("record "+input.Check+" gate Activity intent", InfrastructureFailureType, err)
	}

	logPath := activityLogPath(activityExecutionLogPath(ctx, a.logDirFor(input.RunWorkflowInput), input.Check+".log"))
	// Earlier Temporal attempts' evidence reaches the result and checkpoint only;
	// attempts (and so this attempt's journal) holds this attempt's own.
	inherited := a.earlierAttemptsFor(ctx, a.checkpointDirFor(input.RunWorkflowInput), input.Check)
	attempts := []run.Attempt{}

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
			Kind:             input.Check,
			Command:          res.Command,
			SetupSHA256:      run.SetupDigest(input.SetupCommands),
			FactoryDirSHA256: res.FactoryDirSHA256, FactoryDirCommit: res.FactoryDirCommit, FactoryDirError: res.FactoryDirError,
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
				command[0], command[1:]...,
			)
		}
		return a.runSandboxWithRetries(gateCtx, input.RunWorkflowInput, logPath, 1, beforeAttempt, afterAttempt,
			// nil, for the same reason canonical verification passes
			// nil: a named gate never calls a model.
			nil, registrySpec, composeSpec, referenceOracleDir, referenceOracleMountPath, nil, nil, command[0], command[1:]...)
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
		checkpoint.ErrorType = checkpointErrorType(runErr)
	} else if err != nil {
		checkpoint.Error = input.Check + " gate evidence infrastructure failure: " + err.Error()
	}
	// A gate that failed is rerun on the base commit below, after this
	// result is checkpointed: until then it is recorded as interrupted.
	gateResult.BaseCheck = pendingGateBaseCheck(input, result, runErr, err)
	checkpoint.Result = gateResult
	if saveErr := saveActivityCheckpoint(path, checkpoint, activity.GetInfo(ctx).Attempt); saveErr != nil {
		errType := launchErrorType(runErr)
		return gateResult, temporal.NewApplicationErrorWithCause("save "+input.Check+" gate Activity checkpoint", errType, saveErr, gateResult.Attempts)
	}
	if runErr != nil {
		errType := launchErrorType(runErr)
		return gateResult, temporal.NewApplicationErrorWithCause(input.Check+" gate subprocess infrastructure failure", errType, runErr, gateResult.Attempts)
	}
	if err != nil {
		return gateResult, temporal.NewApplicationErrorWithCause(input.Check+" gate evidence infrastructure failure", InfrastructureFailureType, err, gateResult.Attempts)
	}
	return a.finishGateBaseCheck(ctx, input, command, registrySpec, path, checkpoint, gateResult), nil
}

// baselineBuildNoteFileName is the file, in the run's log directory, that
// holds what the build is told about a baseline that failed as the ticket
// expects (baseline.BuildNote).
const baselineBuildNoteFileName = "baseline_failure.md"

// baselineLogTailBytes bounds how much of the baseline's log is read for
// failing test names. A failure printed before the last 8 MiB is not seen.
const baselineLogTailBytes = 8 << 20

// RunBaselineVerifyActivity runs the ticket's verify command on the base
// commit, in a fresh sandbox, before the build's first round, and judges
// the result (baseline.Evaluate). Once the command has run to an exit code
// it writes the run's baseline record (run.BaselineVerifyFileName) before
// returning, so the result reaches the run record even when the run halts
// here.
//
//   - passed: the build runs.
//   - failed, and the ticket names every failing test: the build runs and is
//     given the list (BaselineVerifyResult.BuildNotePath).
//   - passed or failed as the ticket expects, but the command left paths
//     outside the ticket's Allowed-Files that its repository does not
//     ignore: a BaselineVerifyFailureType error too (every build would be
//     quarantined by diff_scope, which no build can fix).
//   - failed any other way: a BaselineVerifyFailureType error. No build of
//     this ticket could pass the same command in canonical verification, so
//     the run halts before a model call.
//
// The command runs in the run's own worktree, which RunWorkflow has just
// created at the base commit; what the command wrote there is removed
// afterwards (restoreBaseCommit), so the build starts from the commit, not
// from the command's leftovers.
//
// Two kinds of run do not start from the untouched repository, launch
// nothing and carry the record of the run they follow (inheritBaselineVerify):
//
//   - a resumed run, whose worktree holds the halted run's work. The one
//     resumed run that does launch is one whose halted run never finished its
//     own baseline and never started a build: its worktree holds no work, and
//     without this the build would run with no baseline at all;
//   - a run that continues an earlier run of the same ticket from the commit
//     that run produced (a retry on the failed attempt's branch, a corrective
//     round, a PR-review round: DiffBaseSHA names the ticket's real base).
//     The verify command may fail there for the very reason the run exists,
//     and the ticket's baseline was taken by its first run. When no run of
//     this data directory produced the commit, the baseline runs.
//
// Like canonical verification, the launch gets no model route, and the
// checkpoint, intent and journal protocol is runVerifyCommand's.
func (a *Activities) RunBaselineVerifyActivity(ctx context.Context, input RunWorkflowInput) (BaselineVerifyResult, error) {
	logDir := a.logDirFor(input)
	if from, resumed := a.baselineInheritedFrom(input); from != "" {
		if result, inherited, err := a.inheritBaselineVerify(input, logDir, from, resumed); inherited || err != nil {
			return result, err
		}
		if resumed {
			// The halted run never got past its baseline, so the adopted
			// worktree holds no build work: only what that run's command
			// left behind, which must not be this baseline's starting state.
			if err := restoreBaseCommit(ctx, input.WorkspacePath, nil); err != nil {
				return BaselineVerifyResult{}, temporal.NewApplicationErrorWithCause("clear what the lost baseline verify left in the resumed workspace", InfrastructureFailureType, err)
			}
		}
	}
	before, err := gitStatusAllPaths(ctx, input.WorkspacePath)
	if err != nil {
		return BaselineVerifyResult{}, temporal.NewApplicationErrorWithCause("read the workspace's state before the baseline verify", InfrastructureFailureType, err)
	}
	// What the ticket is to create, read before the command can write
	// anything: the worktree is the base commit here.
	created := baseline.CreatedPaths(append(append([]string{}, input.AllowedFiles...), input.RequiredChangedFiles...), func(path string) bool {
		_, err := os.Lstat(filepath.Join(input.WorkspacePath, filepath.FromSlash(path)))
		return err == nil
	})
	var record *run.BaselineVerify
	var leftErr error
	judge := func(res VerifyActivityResult) *run.BaselineVerify {
		if record == nil {
			record = a.judgeBaseline(input, res, created)
			// The command has run and nothing has been restored yet: what
			// it left outside the ticket's Allowed-Files is read now.
			leftErr = recordLeftovers(ctx, input, before, record)
		}
		return record
	}
	res, err := a.runVerifyCommand(ctx, input, verifyRun{
		kind:      run.BaselineVerifyAttemptKind,
		label:     "baseline verify",
		endDetail: func(res VerifyActivityResult) string { return judge(res).Summary() },
	})
	if errors.Is(err, sandbox.ErrComposeServicesRejected) {
		// A rejected compose file halts a run with its own reason code
		// (see buildActivityErrorType); the baseline is now the first
		// launch to load the file, so it says the same.
		return BaselineVerifyResult{Attempts: res.Attempts}, temporal.NewApplicationErrorWithCause(err.Error(), ComposeServicesRejectedFailureType, err, res.Attempts)
	}
	if err != nil {
		return BaselineVerifyResult{Attempts: res.Attempts}, err
	}
	if err := restoreBaseCommit(ctx, input.WorkspacePath, before); err != nil {
		return BaselineVerifyResult{Attempts: res.Attempts}, temporal.NewApplicationErrorWithCause("restore the workspace to the base commit after the baseline verify", InfrastructureFailureType, err, res.Attempts)
	}
	result := BaselineVerifyResult{Record: *judge(res), Attempts: res.Attempts}
	if leftErr != nil {
		return result, temporal.NewApplicationErrorWithCause("read the workspace's state after the baseline verify", InfrastructureFailureType, leftErr, res.Attempts)
	}
	if err := run.SaveBaselineVerify(logDir, &result.Record); err != nil {
		return result, temporal.NewApplicationErrorWithCause("record the baseline verify", InfrastructureFailureType, err, res.Attempts)
	}
	if result.Record.Halts() {
		return result, temporal.NewNonRetryableApplicationError(result.Record.HaltMessage(), BaselineVerifyFailureType, nil, res.Attempts)
	}
	result.BuildNotePath, err = writeBaselineBuildNote(logDir, &result.Record)
	if err != nil {
		return result, temporal.NewApplicationErrorWithCause("write the baseline note for the build", InfrastructureFailureType, err, res.Attempts)
	}
	return result, nil
}

// recordLeftovers sets on record the paths the command left in the workspace
// that the ticket's diff_scope gate would flag (baseline.NoteLeftovers): the
// factory commits what a build leaves, so such a path quarantines every build
// of the ticket.
func recordLeftovers(ctx context.Context, input RunWorkflowInput, before []string, record *run.BaselineVerify) error {
	left, err := pathsAddedSince(ctx, input.WorkspacePath, before)
	if err != nil {
		return err
	}
	baseline.NoteLeftovers(record, left, input.AllowedFiles)
	return nil
}

// writeBaselineBuildNote writes what the build is told about a baseline
// that failed as its ticket expects, and returns the file's path; "" when
// there is nothing to tell.
func writeBaselineBuildNote(logDir string, record *run.BaselineVerify) (string, error) {
	note := baseline.BuildNote(record)
	if note == "" {
		return "", nil
	}
	path := filepath.Join(logDir, baselineBuildNoteFileName)
	return path, os.WriteFile(path, []byte(note), 0o600)
}

// judgeBaseline builds the baseline record of a finished launch from its
// log and the ticket's text.
func (a *Activities) judgeBaseline(input RunWorkflowInput, res VerifyActivityResult, created []string) *run.BaselineVerify {
	evaluate := baseline.Evaluate
	if len(input.SetupCommands) > 0 {
		evaluate = baseline.EvaluateWithSetup
	}
	record := evaluate(a.verifyCommandFor(input), res.Result.ExitCode, readFileTail(res.Result.LogPath, baselineLogTailBytes), triage.FirstFailureLine(res.Result.LogPath), a.ticketTextFor(input), created)
	record.BaseSHA = input.BaseSHA
	record.LogPath = res.Result.LogPath
	record.DurationMs = res.DurationMs
	return record
}

// ticketTextFor is what a baseline failure is looked up in: the ticket the
// run builds, which is the text its build is given. The acceptance
// criteria a review reads are not part of it: the build's note repeats the
// words a test was named with, and must hold nothing the build was not
// given. A file that cannot be read adds nothing, which can only make a
// failure unnamed.
func (a *Activities) ticketTextFor(input RunWorkflowInput) string {
	var text strings.Builder
	for _, path := range dedupStrings([]string{input.SpecPath, input.TicketPath}) {
		if path == "" {
			continue
		}
		if content, err := os.ReadFile(path); err == nil {
			text.Write(content)
			text.WriteByte('\n')
		}
	}
	return text.String()
}

// baselineInheritedFrom names the run whose baseline record input's run
// carries instead of taking its own, "" when it takes its own: the halted
// run it resumes (resumed true), or the earlier run of the same ticket that
// produced the commit it starts from.
func (a *Activities) baselineInheritedFrom(input RunWorkflowInput) (runID string, resumed bool) {
	if input.ResumeFrom != nil {
		return input.ResumeFrom.RunID, true
	}
	if input.DiffBaseSHA == "" || input.DiffBaseSHA == input.BaseSHA || input.BaseSHA == "" {
		return "", false
	}
	byRequest, err := run.ListByRequestID(a.dataDirFor(input))
	if err != nil {
		return "", false
	}
	// The newest such run: a commit is produced once, but a run that
	// committed nothing of its own records the commit it started from.
	var newest *run.Run
	for _, runs := range byRequest {
		for _, r := range runs {
			if r.ResultSHA == input.BaseSHA && r.ID != input.RunID && (newest == nil || r.CreatedAt > newest.CreatedAt) {
				newest = r
			}
		}
	}
	if newest == nil {
		return "", false
	}
	return newest.ID, false
}

// inheritBaselineVerify gives a run the baseline record of the run it
// follows (from), and its build the same note. inherited is false when the
// caller must run the baseline itself: from has no record that let a build
// run (none, or one that halted it) and, for a resumed run, never started a
// build either, so it stopped at or during its own baseline. A resumed run whose halted run built with no record follows a
// run from before the check existed, and has none either.
func (a *Activities) inheritBaselineVerify(input RunWorkflowInput, logDir, from string, resumed bool) (result BaselineVerifyResult, inherited bool, err error) {
	dataDir := a.dataDirFor(input)
	earlier, err := run.LoadBaselineVerify(run.Dir(dataDir, from))
	if err != nil {
		return BaselineVerifyResult{}, true, temporal.NewApplicationErrorWithCause("read the baseline verify of the run this one follows", InfrastructureFailureType, err)
	}
	if earlier != nil && earlier.Halts() {
		// A run that halted on its baseline never built. Its result is
		// not carried: whoever follows it takes the baseline again.
		earlier = nil
	}
	if earlier == nil && !resumed {
		return BaselineVerifyResult{}, false, nil
	}
	if earlier == nil {
		halted, loadErr := run.Load(dataDir, from)
		if loadErr != nil {
			return BaselineVerifyResult{}, true, temporal.NewApplicationErrorWithCause("read the resumed run", InfrastructureFailureType, loadErr)
		}
		for _, attempt := range halted.Attempts {
			if attempt.Kind == "build" {
				return BaselineVerifyResult{}, true, nil
			}
		}
		return BaselineVerifyResult{}, false, nil
	}
	if earlier.InheritedFrom == "" {
		earlier.InheritedFrom = from
	}
	if err := run.SaveBaselineVerify(logDir, earlier); err != nil {
		return BaselineVerifyResult{}, true, temporal.NewApplicationErrorWithCause("record the baseline verify", InfrastructureFailureType, err)
	}
	result = BaselineVerifyResult{Record: *earlier}
	// The build is a fresh session, which is given the note again.
	if result.BuildNotePath, err = writeBaselineBuildNote(logDir, earlier); err != nil {
		return result, true, temporal.NewApplicationErrorWithCause("write the baseline note for the build", InfrastructureFailureType, err)
	}
	return result, true, nil
}

// hostGitAfterSandbox is the start of a host git command line run in dir
// after a sandboxed command wrote there. The repository's own configuration
// can name a hooks directory or a file-system monitor inside the writable
// workspace, and checkout and status would run it on the host: both are
// turned off, as runner.GitCommitAll turns hooks off for its commit.
func hostGitAfterSandbox(dir string) []string {
	return []string{"-C", dir, "-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false"}
}

// gitStatusAllPaths lists every path git reports as changed or untracked
// in dir, each untracked file by its own path (a new file inside a
// directory that was already untracked is then a new entry).
func gitStatusAllPaths(ctx context.Context, dir string) ([]string, error) {
	out, err := exec.CommandContext(ctx, "git", append(hostGitAfterSandbox(dir), "status", "--porcelain", "-z", "--untracked-files=all", "--no-renames")...).Output()
	if err != nil {
		return nil, fmt.Errorf("git status: %w", err)
	}
	var paths []string
	for _, entry := range strings.Split(string(out), "\x00") {
		if len(entry) > 3 {
			paths = append(paths, entry[3:])
		}
	}
	return paths, nil
}

// pathsAddedSince lists the paths git reports as changed or untracked in dir
// now that before (gitStatusAllPaths at an earlier time) did not report.
func pathsAddedSince(ctx context.Context, dir string, before []string) ([]string, error) {
	after, err := gitStatusAllPaths(ctx, dir)
	if err != nil {
		return nil, err
	}
	was := make(map[string]bool, len(before))
	for _, p := range before {
		was[p] = true
	}
	var added []string
	for _, p := range after {
		if !was[p] {
			added = append(added, p)
		}
	}
	return added, nil
}

// restoreBaseCommit removes what the baseline's command left in dir: every
// path that is dirty now and was not in before (gitStatusAllPaths before
// the launch) is put back to HEAD, or deleted if HEAD does not have it.
// Files git ignores are left, as canonical verification leaves them. Each
// path is passed to git literally: a file named "*" names only itself.
func restoreBaseCommit(ctx context.Context, dir string, before []string) error {
	added, err := pathsAddedSince(ctx, dir, before)
	if err != nil || len(added) == 0 {
		return err
	}
	git := func(args ...string) {
		_ = exec.CommandContext(ctx, "git", append(append(hostGitAfterSandbox(dir), "--literal-pathspecs"), args...)...).Run()
	}
	for _, p := range added {
		// Each of the three is a no-op for a path it does not apply to
		// (unstaged, not in HEAD, tracked), so only the final check decides.
		// -ff: a directory the command left with its own .git is removed too.
		git("reset", "-q", "HEAD", "--", p)
		git("checkout", "-q", "HEAD", "--", p)
		git("clean", "-ffdq", "--", p)
	}
	added, err = pathsAddedSince(ctx, dir, before)
	if err != nil {
		return err
	}
	if len(added) > 0 {
		return fmt.Errorf("the baseline verify left %d path(s) that could not be removed, first %q", len(added), added[0])
	}
	return nil
}

// readFileTail returns up to the last max bytes of path, "" if it cannot be
// read.
func readFileTail(path string, max int64) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	if info, err := f.Stat(); err == nil && info.Size() > max {
		if _, err := f.Seek(info.Size()-max, io.SeekStart); err != nil {
			return ""
		}
	}
	content, err := io.ReadAll(f)
	if err != nil {
		return ""
	}
	return string(content)
}
