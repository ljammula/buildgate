package workflow

import (
	"context"
	"os"
	"path/filepath"
	"time"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"

	"buildgate/internal/oraclecanary"
	"buildgate/internal/run"
	"buildgate/internal/runner"
	"buildgate/internal/sandbox"
)

// runReferenceOracleCanary is the second half of RunNamedGateActivity's
// reference_oracle gate: it runs the SAME command, in the same sandbox and with
// the same mount path, against a canary snapshot of the oracle the real run
// just passed on, and judges the pair. It runs inside the gate Activity's own
// execution, so the gate's single checkpoint covers both runs; it has its own
// attempt intent (numbered after the real run's) and appends to the same
// attempt journal, so a crash mid-canary leaves an intent with no checkpoint.
// A Temporal retry of the gate (attempt > 1) expects the earlier attempt's
// intents (see retriedAttempt) and reruns the real run and the canary whole,
// with a fresh nonce.
//
// ctx is the Activity context (intent/journal/heartbeat identity). budget is the
// canary run's OWN timeout, a duration (never a shared absolute deadline: a slow
// real run must not eat the canary's time); 0 means the Activity deadline only. A returned error is an infrastructure failure. A
// snapshot that cannot be built is NOT an error: it is an UNSUPPORTED verdict
// that fails the gate.
func (a *Activities) runReferenceOracleCanary(ctx context.Context, budget time.Duration, input NamedGateActivityInput, attempts *[]run.Attempt, realSnapshotDir, mountPath string, registrySpec *sandbox.RegistryProxySpec, command []string) (run.OracleCanaryEvidence, bool, error) {
	canaryDir, err := filepath.Abs(filepath.Join(filepath.Dir(realSnapshotDir), "reference-oracle-canary-snapshot"))
	if err != nil {
		return run.OracleCanaryEvidence{}, false, temporal.NewApplicationErrorWithCause("resolve oracle canary snapshot path", InfrastructureFailureType, err)
	}
	defer os.RemoveAll(canaryDir)
	// Fresh per execution of this function, held only in memory. Retry cannot mix
	// nonces: a retried gate reruns the real run and the canary from the top
	// with a new nonce, and a completed checkpoint short-circuits it. The nonce
	// is therefore never persisted and never spans two snapshots.
	nonce, err := oraclecanary.NewNonce()
	if err != nil {
		return run.OracleCanaryEvidence{}, false, temporal.NewApplicationErrorWithCause("generate oracle canary nonce", InfrastructureFailureType, err)
	}
	eco, buildErr := oraclecanary.BuildMountSnapshot(realSnapshotDir, canaryDir, nonce)
	if buildErr != nil {
		return oraclecanary.Unsupported(0, buildErr), false, nil
	}
	composeSpec, err := a.composeServicesSpecFor(input.RunWorkflowInput, oraclecanary.AttemptKind)
	if err != nil {
		return run.OracleCanaryEvidence{}, false, temporal.NewApplicationError(err.Error(), InfrastructureFailureType)
	}

	checkpointDir := a.checkpointDirFor(input.RunWorkflowInput)
	logPath := activityLogPath(activityExecutionLogPath(ctx, a.logDirFor(input.RunWorkflowInput), oraclecanary.AttemptKind+".log"))
	prior := len(*attempts)
	if _, err := recordActivityIntent(ctx, checkpointDir, oraclecanary.AttemptKind, command); err != nil {
		return run.OracleCanaryEvidence{}, false, temporal.NewApplicationErrorWithCause("record oracle canary Activity intent", InfrastructureFailureType, err)
	}
	beforeAttempt := func(attempt int) error {
		_, err := recordActivityAttemptIntent(ctx, checkpointDir, prior+attempt, oraclecanary.AttemptKind, command, time.Now().UTC().Format(time.RFC3339))
		return err
	}
	beforeAttempt = a.leaseChecked(ctx, checkpointDir, beforeAttempt)
	afterAttempt := func(attempt int, res runner.Result, _ error) error {
		*attempts = append(*attempts, run.Attempt{
			Kind:        oraclecanary.AttemptKind,
			Command:     res.Command,
			StartedAt:   res.StartedAt.Format(time.RFC3339),
			FinishedAt:  res.FinishedAt.Format(time.RFC3339),
			ExitCode:    res.ExitCode,
			LogPath:     logPath(attempt),
			ImageDigest: res.ImageDigest,
		})
		return saveActivityAttemptJournal(ctx, checkpointDir, input.Check, *attempts)
	}
	runCtx := ctx
	if budget > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(ctx, budget)
		defer cancel()
	}
	start := time.Now()
	result, runErr := heartbeatWhileRunning(activityHeartbeatInterval, func() {
		activity.RecordHeartbeat(ctx, HeartbeatDetails{Stage: oraclecanary.AttemptKind, Elapsed: time.Since(start)})
	}, func() (runner.Result, error) {
		if a.hasFakeRunner() {
			return a.runWithRetriesFn()(runCtx, input.WorkspacePath, logPath, 1, beforeAttempt, afterAttempt, "sh", "-c", input.Command)
		}
		return a.runSandboxWithRetries(runCtx, input.RunWorkflowInput, logPath, 1, beforeAttempt, afterAttempt,
			nil, registrySpec, composeSpec, canaryDir, mountPath, nil, nil, "sh", "-c", input.Command)
	})
	if runErr != nil {
		return run.OracleCanaryEvidence{}, false, runErr
	}
	ev, trusted, judgeErr := oraclecanary.Judge(eco, 0, result.ExitCode, logPath(1), nonce)
	if judgeErr != nil {
		return run.OracleCanaryEvidence{}, false, judgeErr
	}
	return ev, trusted, nil
}
