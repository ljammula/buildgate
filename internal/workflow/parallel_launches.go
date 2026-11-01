package workflow

import (
	temporalworkflow "go.temporal.io/sdk/workflow"
)

// parallelReproductionLaunchesChange is the GetVersion change ID under which
// the independent reproduction launches of a run start at once: the
// tests_red_on_base proof (every file's result launch together, then the base
// launches that are needed) and the challenge (every unit's first pass
// together, then the second pass of each red one). Each launch is still its
// own Activity, with its own ID, sandbox and verdict; only the start times
// change. It is asked once per phase call, before the phase's first launch,
// at a point every execution reaches the same way. DefaultVersion keeps the
// serial order, so a history recorded before it replays unchanged.
const parallelReproductionLaunchesChange = "parallel-reproduction-launches"

// launchesInParallel asks parallelReproductionLaunchesChange. A run with
// compose services launches one at a time even in the new version: the
// services' project and network are named by the run, so two launches of it
// would share and tear down each other's sidecars.
func launchesInParallel(ctx temporalworkflow.Context, input RunWorkflowInput) bool {
	v := temporalworkflow.GetVersion(ctx, parallelReproductionLaunchesChange, temporalworkflow.DefaultVersion, 1)
	return v == 1 && !input.ComposeServicesEnabled
}

// waveLaunch is one launch of a wave: its Activity ID and input.
type waveLaunch struct {
	id string
	in ReproductionRunInput
}

// waveOutcome is what one launch of a wave returned.
type waveOutcome struct {
	res ReproductionRunResult
	err error
}

// runReproductionWave schedules every launch before it waits on any, then
// waits for every one, in order, so no Activity is left running when it
// returns. Only the workflow goroutine calls it; outcomes are in launch order.
func runReproductionWave(ctx temporalworkflow.Context, ids *activityIDs, rctx temporalworkflow.Context, launches []waveLaunch) []waveOutcome {
	futures := make([]temporalworkflow.Future, len(launches))
	for i, l := range launches {
		futures[i] = temporalworkflow.ExecuteActivity(ids.withRetries(rctx, l.id), RunHeldTestActivityName, l.in)
	}
	out := make([]waveOutcome, len(launches))
	for i, f := range futures {
		out[i].err = f.Get(ctx, &out[i].res)
	}
	return out
}

// failWave is the run's failure when a launch of the wave failed: the first
// error in launch order, wrapped as runReproductionPass wraps it, with the
// attempts of the launches that completed recorded first. nil when none did.
func failWave(result *RunWorkflowResult, outs []waveOutcome, worktreePath, branch string) error {
	for _, o := range outs {
		if o.err == nil {
			continue
		}
		appendWaveAttempts(result, outs)
		return wrapActivityFailure("run review reproduction activity", o.err, result.Attempts, result.BaseSHA, result.CommittedByWorker, worktreePath, branch)
	}
	return nil
}

// waveFailed reports whether any launch of the wave returned an error.
func waveFailed(outs []waveOutcome) bool {
	for _, o := range outs {
		if o.err != nil {
			return true
		}
	}
	return false
}

// appendWaveAttempts records the attempts of the wave's completed launches, in
// launch order.
func appendWaveAttempts(result *RunWorkflowResult, outs []waveOutcome) {
	for _, o := range outs {
		if o.err == nil {
			result.Attempts = append(result.Attempts, o.res.Attempts...)
		}
	}
}
