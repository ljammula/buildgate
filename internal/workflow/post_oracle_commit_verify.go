package workflow

import (
	"context"
	"errors"
	"strings"
	"time"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"

	"buildgate/internal/evidence"
	"buildgate/internal/policy"
	"buildgate/internal/run"
	"buildgate/internal/runner"
	"buildgate/internal/sandbox"
)

const (
	// RunPostOracleCommitVerifyActivityName is the Temporal counterpart of
	// cmd/factoryd's "verify_after_oracle_commit" re-run. The host
	// oracle commit (CommitOraclesActivity) changes HEAD after every gate ran,
	// so canonical verification -- and the full suite when one is configured --
	// must run again against the resulting tree, and the workspace must still
	// be clean afterwards. It is its own Activity (its own ActivityID, hence its
	// own checkpoint, intent and attempt journal) because RunVerifyActivity
	// short-circuits on its own durable checkpoint: a second call would replay
	// the first result instead of re-verifying the committed tree.
	RunPostOracleCommitVerifyActivityName = "RunPostOracleCommitVerifyActivity"

	// The attempt kinds match cmd/factoryd's, so run.json reads the same. postOracleCommitJournalKind is the journal-level kind: one
	// journal covers both phases of this Activity.
	postOracleCommitVerifyKind    = "verify_after_oracle_commit"
	postOracleCommitFullSuiteKind = "full_suite_verify_after_oracle_commit"
	postOracleCommitJournalKind   = "post_oracle_commit_verify"

	// postOracleCommitGateSuffix turns a named gate's check into its attempt
	// kind here ("lint" -> "lint_after_oracle_commit"), as in cmd/factoryd.
	postOracleCommitGateSuffix = "_after_oracle_commit"
)

// postOracleCommitGate is one named gate re-run by the post-commit Activity.
type postOracleCommitGate struct {
	Check   string
	Command string
}

// postOracleCommitGates lists the configured named gates the Activity re-runs
// against the committed tree, in the registry's own gate order
// (policy.CommandGates). reference_oracle is excluded (its
// RerunAfterOracleCommit is false): it verifies the oracle bytes the host
// committed, not the tree.
func postOracleCommitGates(input RunWorkflowInput) []postOracleCommitGate {
	var out []postOracleCommitGate
	for _, g := range policy.CommandGates {
		if !g.RerunAfterOracleCommit {
			continue
		}
		if command := input.GateCommands[g.ID]; command != "" {
			out = append(out, postOracleCommitGate{Check: g.ID, Command: command})
		}
	}
	// A repository's own gates check the tree, as lint does, so they run
	// again against the committed one.
	for _, check := range policy.RepoGateChecks(input.GateCommands) {
		out = append(out, postOracleCommitGate{Check: check, Command: input.GateCommands[check]})
	}
	return out
}

// postOracleCommitPhaseCount is how many sequential commands the Activity runs:
// canonical verify, the full suite when declared, and each configured named
// gate. The workflow multiplies the single-command Activity timeout by it and
// the Activity divides its deadline by it, so every phase gets one
// single-command budget however many phases there are.
func postOracleCommitPhaseCount(input RunWorkflowInput) int {
	n := 1 + len(postOracleCommitGates(input))
	if input.FullSuiteCommand != "" {
		n++
	}
	return n
}

// PostOracleCommitVerifyResult is RunPostOracleCommitVerifyActivity's output.
// FullSuite is nil when the run declared no full-suite command. Attempts holds
// every attempt of both phases (also inside Verify/FullSuite individually).
// Clean reports runner.GitIsClean after both phases: a re-run that wrote files
// while exiting 0 leaves output no commit captures.
type PostOracleCommitVerifyResult struct {
	Verify    VerifyActivityResult  `json:"verify"`
	FullSuite *VerifyActivityResult `json:"full_suite,omitempty"`
	// NamedGates holds one entry per configured named gate other than
	// reference_oracle that was re-run, in run order.
	NamedGates []PostOracleCommitGateResult `json:"named_gates,omitempty"`
	Attempts   []run.Attempt                `json:"attempts"`
	Clean      bool                         `json:"clean"`
}

// PostOracleCommitGateResult is one named gate's re-run against the committed tree.
type PostOracleCommitGateResult struct {
	Check  string               `json:"check"`
	Result VerifyActivityResult `json:"result"`
}

// isPostOracleCommitAttemptKind reports whether kind is an attempt kind this
// Activity's journal may hold (its journal kind differs from its attempts').
func isPostOracleCommitAttemptKind(kind string) bool {
	switch kind {
	case postOracleCommitVerifyKind, postOracleCommitFullSuiteKind:
		return true
	}
	check, ok := strings.CutSuffix(kind, postOracleCommitGateSuffix)
	if !ok {
		return false
	}
	for _, g := range policy.CommandGates {
		if g.RerunAfterOracleCommit && g.ID == check {
			return true
		}
	}
	return policy.IsRepoGate(check)
}

// attemptKindFitsJournal is the journal validity rule shared by the loader and
// RecoverAttemptsFromCheckpointDir: an attempt's kind equals its journal's,
// except in the post-oracle-commit journal that holds both phases.
func attemptKindFitsJournal(journalKind, attemptKind string) bool {
	if journalKind == postOracleCommitJournalKind {
		return isPostOracleCommitAttemptKind(attemptKind)
	}
	return attemptKind == journalKind
}

// RunPostOracleCommitVerifyActivity re-runs canonical verification and, when
// input.FullSuiteCommand is declared, the full suite against the committed
// workspace, then reports whether the workspace is still clean. Same
// checkpoint / two-phase intent / attempt-journal crash-safety protocol as
// RunVerifyActivity and RunFullSuiteVerifyActivity (see RunVerifyActivity),
// with one checkpoint for the whole Activity. The configured named gates other
// than reference_oracle (lint, security_audit, unit_tests, integration_tests)
// re-run after the full suite, matching cmd/factoryd.
func (a *Activities) RunPostOracleCommitVerifyActivity(ctx context.Context, input RunWorkflowInput) (progressResult PostOracleCommitVerifyResult, err error) {
	progressStarted := false
	defer func() {
		if !progressStarted {
			return
		}
		outcome := "pass"
		if err != nil || progressResult.Verify.Result.ExitCode != 0 || !progressResult.Clean ||
			(progressResult.FullSuite != nil && progressResult.FullSuite.Result.ExitCode != 0) {
			outcome = "fail"
		}
		for _, g := range progressResult.NamedGates {
			if g.Result.Result.ExitCode != 0 {
				outcome = "fail"
			}
		}
		progressMark(ctx, a.logDirFor(input), "post_oracle_commit_verify", "end", outcome, "")
	}()
	checkpointDir := a.checkpointDirFor(input)
	if err := a.fenceEarlierAttempts(ctx, input, true); err != nil {
		return PostOracleCommitVerifyResult{}, err
	}
	checkpoint, path, found, err := loadRetriedActivityCheckpoint[PostOracleCommitVerifyResult](ctx, checkpointDir)
	if err != nil {
		return PostOracleCommitVerifyResult{}, checkpointLoadError("load post-oracle-commit verify Activity checkpoint", err)
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
	progressMark(ctx, a.logDirFor(input), "post_oracle_commit_verify", "start", "", "")

	registrySpec, err := a.registryProxySpecFor(input)
	if err != nil {
		return PostOracleCommitVerifyResult{}, temporal.NewApplicationError(err.Error(), InfrastructureFailureType)
	}
	verifyComposeSpec, err := a.composeServicesSpecFor(input, postOracleCommitVerifyKind)
	if err != nil {
		return PostOracleCommitVerifyResult{}, temporal.NewApplicationError(err.Error(), InfrastructureFailureType)
	}
	runFullSuite := input.FullSuiteCommand != ""
	gates := postOracleCommitGates(input)
	gateComposeSpecs := make([]*sandbox.ComposeServicesSpec, len(gates))
	for i, g := range gates {
		gateComposeSpecs[i], err = a.composeServicesSpecFor(input, g.Check+postOracleCommitGateSuffix)
		if err != nil {
			return PostOracleCommitVerifyResult{}, temporal.NewApplicationError(err.Error(), InfrastructureFailureType)
		}
	}
	var fullSuiteComposeSpec *sandbox.ComposeServicesSpec
	if runFullSuite {
		fullSuiteComposeSpec, err = a.composeServicesSpecFor(input, postOracleCommitFullSuiteKind)
		if err != nil {
			return PostOracleCommitVerifyResult{}, temporal.NewApplicationError(err.Error(), InfrastructureFailureType)
		}
	}

	intentFound, journalFound, err := priorAttemptRecords(ctx, checkpointDir)
	if err != nil {
		return PostOracleCommitVerifyResult{}, priorRecordsFailure("post-oracle-commit verify", err)
	}
	if intentFound {
		return PostOracleCommitVerifyResult{}, temporal.NewApplicationError(
			"a prior attempt of this post-oracle-commit verify Activity recorded intent to re-run verification but crashed before reaching a durable checkpoint — halting rather than risk a duplicate invocation or trusting an unconfirmed result",
			AmbiguousPriorAttemptType,
		)
	}
	if journalFound {
		return PostOracleCommitVerifyResult{}, temporal.NewApplicationError(
			"a post-oracle-commit verify Activity attempt journal exists without its intent or completed checkpoint — halting rather than risk a duplicate invocation",
			AmbiguousPriorAttemptType,
		)
	}

	// One journal covers both phases, so attempts is cumulative and the attempt
	// numbers recorded in intents continue across phases: otherwise the second
	// phase's in-flight attempt (numbered 1) would look already-journaled to
	// RecoverAttemptsFromCheckpointDir and vanish from run.json.
	inherited := a.earlierAttemptsFor(ctx, checkpointDir, postOracleCommitJournalKind)
	attempts := []run.Attempt{}
	var phaseBudget time.Duration
	if deadline, ok := ctx.Deadline(); ok {
		phaseBudget = time.Until(deadline) / time.Duration(postOracleCommitPhaseCount(input))
	}
	runPhase := func(kind, shellCommand, logName string, maxAttempts int, composeSpec *sandbox.ComposeServicesSpec) (VerifyActivityResult, error, error) {
		command := stepCommand(input.SetupCommands, shellCommand)
		if _, err := recordActivityIntent(ctx, checkpointDir, kind, command); err != nil {
			return VerifyActivityResult{}, nil, temporal.NewApplicationErrorWithCause("record post-oracle-commit verify Activity intent", InfrastructureFailureType, err)
		}
		logPath := activityLogPath(activityExecutionLogPath(ctx, a.logDirFor(input), logName))
		priorAttempts := len(attempts)
		var phaseAttempts []run.Attempt
		beforeAttempt := func(attempt int) error {
			_, err := recordActivityAttemptIntent(ctx, checkpointDir, priorAttempts+attempt, kind, command, time.Now().UTC().Format(time.RFC3339))
			return err
		}
		beforeAttempt = a.leaseChecked(ctx, checkpointDir, beforeAttempt)
		afterAttempt := func(attempt int, res runner.Result, _ error) error {
			recorded := run.Attempt{
				Kind:        kind,
				Command:     res.Command,
				SetupSHA256: run.SetupDigest(input.SetupCommands),
				StartedAt:   res.StartedAt.Format(time.RFC3339),
				FinishedAt:  res.FinishedAt.Format(time.RFC3339),
				ExitCode:    res.ExitCode,
				LogPath:     logPath(attempt),
				ImageDigest: res.ImageDigest,
			}
			phaseAttempts = append(phaseAttempts, recorded)
			attempts = append(attempts, recorded)
			return saveActivityAttemptJournal(ctx, checkpointDir, postOracleCommitJournalKind, attempts)
		}
		// Each phase gets at most the single-command budget (this Activity's
		// scaled deadline divided by its phase count, fixed at Activity start),
		// so a hung phase cannot consume the next phase's time.
		phaseCtx := ctx
		if phaseBudget > 0 {
			var cancel context.CancelFunc
			phaseCtx, cancel = context.WithTimeout(ctx, phaseBudget)
			defer cancel()
		}
		heartbeatStart := time.Now()
		result, runErr := heartbeatWhileRunning(activityHeartbeatInterval, func() {
			activity.RecordHeartbeat(ctx, HeartbeatDetails{Stage: kind, Elapsed: time.Since(heartbeatStart)})
		}, func() (runner.Result, error) {
			if a.hasFakeRunner() {
				return a.runWithRetriesFn()(phaseCtx, input.WorkspacePath, logPath, maxAttempts, beforeAttempt, afterAttempt, command[0], command[1:]...)
			}
			// nil relay: neither phase calls a model, exactly as for the
			// original verify and full-suite gates.
			return a.runSandboxWithRetries(phaseCtx, input, logPath, maxAttempts, beforeAttempt, afterAttempt,
				nil, registrySpec, composeSpec, "", "", nil, nil, command[0], command[1:]...)
		})
		phaseResult := VerifyActivityResult{Result: result, Attempts: phaseAttempts}
		if runErr != nil {
			return phaseResult, runErr, nil
		}
		phaseResult.DurationMs = result.FinishedAt.Sub(result.StartedAt).Milliseconds()
		var evidenceErr error
		phaseResult.LogSHA256, evidenceErr = evidence.SHA256File(result.LogPath)
		return phaseResult, nil, evidenceErr
	}

	var out PostOracleCommitVerifyResult
	var runErr, evidenceErr error
	var stage string
	out.Verify, runErr, evidenceErr = runPhase(postOracleCommitVerifyKind, a.verifyCommandFor(input), "verify_after_oracle_commit.log", 1, verifyComposeSpec)
	stage = "post-oracle-commit verify"
	// Unlike the original gates, the full suite runs even when this verify
	// failed, as in cmd/factoryd: a failing re-run replaces the run's verify
	// result either way, and the full suite's own result is reported too.
	if runErr == nil && evidenceErr == nil && runFullSuite {
		var fullSuite VerifyActivityResult
		fullSuite, runErr, evidenceErr = runPhase(postOracleCommitFullSuiteKind, input.FullSuiteCommand, "full_suite_verify_after_oracle_commit.log", 1, fullSuiteComposeSpec)
		out.FullSuite = &fullSuite
		stage = "post-oracle-commit full-suite verify"
	}
	// The named gates ran against the tree before the host commit too. Like the
	// full suite they run even after a failed earlier phase (a failing re-run
	// replaces that gate's recorded result); an infrastructure error stops them.
	for i, g := range gates {
		if runErr != nil || evidenceErr != nil {
			break
		}
		var gateResult VerifyActivityResult
		kind := g.Check + postOracleCommitGateSuffix
		gateResult, runErr, evidenceErr = runPhase(kind, g.Command, kind+".log", 1, gateComposeSpecs[i])
		out.NamedGates = append(out.NamedGates, PostOracleCommitGateResult{Check: g.Check, Result: gateResult})
		stage = "post-oracle-commit " + g.Check + " gate"
	}
	out.Attempts = withInherited(inherited, attempts)
	if runErr == nil && evidenceErr == nil {
		// A re-run that writes files while exiting 0 leaves output no commit
		// captures, so the accepted tree could differ from what passed. A git
		// error is reported as not clean (fail closed).
		clean, cleanErr := runner.GitIsClean(input.WorkspacePath)
		out.Clean = cleanErr == nil && clean
		if cleanErr != nil {
			evidenceErr = cleanErr
			stage = "post-oracle-commit cleanliness check"
		}
	}

	if runErr != nil {
		checkpoint.Error = stage + " subprocess infrastructure failure: " + runErr.Error()
		if errors.Is(runErr, sandbox.ErrCleanupUnconfirmed) {
			checkpoint.ErrorType = CleanupUnconfirmedFailureType
		}
	} else if evidenceErr != nil {
		checkpoint.Error = stage + " evidence infrastructure failure: " + evidenceErr.Error()
	}
	checkpoint.Result = out
	if saveErr := saveActivityCheckpoint(path, checkpoint, activity.GetInfo(ctx).Attempt); saveErr != nil {
		errType := InfrastructureFailureType
		if errors.Is(runErr, sandbox.ErrCleanupUnconfirmed) {
			errType = CleanupUnconfirmedFailureType
		}
		return out, temporal.NewApplicationErrorWithCause("save post-oracle-commit verify Activity checkpoint", errType, saveErr, out.Attempts)
	}
	if runErr != nil {
		errType := InfrastructureFailureType
		if errors.Is(runErr, sandbox.ErrCleanupUnconfirmed) {
			errType = CleanupUnconfirmedFailureType
		}
		return out, temporal.NewApplicationErrorWithCause(stage+" subprocess infrastructure failure", errType, runErr, out.Attempts)
	}
	if evidenceErr != nil {
		return out, temporal.NewApplicationErrorWithCause(stage+" evidence infrastructure failure", InfrastructureFailureType, evidenceErr, out.Attempts)
	}
	return out, nil
}
