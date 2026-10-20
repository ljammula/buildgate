package workflow

import (
	"buildgate/internal/run"
	"buildgate/internal/runner"
	"buildgate/internal/sandbox"
	"context"
	"errors"
	"fmt"
)

// A repository command (canonical verify, the baseline verify, the full
// suite, a named gate, and the setup commands that share its launch) that
// fails after Docker's disk ran out of room, or after a process of its
// sandbox was killed for want of memory, has not judged the work. Both facts
// are read by the factory from outside the sandbox (sandbox.Resources),
// never from what the command printed; the decision is made from the
// command's exit code and those readings only:
//
//   - failed, and Docker's free space after the exit is under
//     sandbox.ExhaustedDiskFloor while the space the launch itself released
//     is under half of what was free before it (it did not fill the disk
//     itself): the step is unresolved (errSandboxDisk, SandboxDiskFailureType),
//     with no second run, since a second run finds the same disk. The
//     baseline verify, which runs on the base commit the work has not
//     touched, needs only the low free space.
//   - failed, and Docker reports the worker container OOM-killed: the
//     command is run once more, in the same Activity, on the worktree as the
//     first run left it; a second run that fails and is OOM-killed again is
//     unresolved (errSandboxMemory, SandboxMemoryFailureType). The baseline
//     verify is unresolved on the first.
//   - a pass is a pass whatever the readings; a reading that could not be
//     taken is judged as if the rule did not exist.
//
// The build step records the readings and is never unresolved by them: only
// a launch refused for want of disk before its command was released
// (sandbox.ErrSandboxDiskLow) is, for every step.

// errSandboxDisk is the launch error of a command that failed with Docker's
// disk exhausted by something other than the launch.
var errSandboxDisk = errors.New("the command failed with Docker's disk out of room, which the launch did not fill")

// errSandboxMemory is the launch error of a command that failed in a sandbox
// whose process the kernel killed for want of memory.
var errSandboxMemory = errors.New("the command failed with a sandbox process killed for want of memory")

type sandboxResourcesKey struct{}

// withSandboxResources returns ctx marked so that runSandboxWithRetries
// leaves the readings of each launch in the returned value, clearing what the
// launch before left.
func withSandboxResources(ctx context.Context) (context.Context, *sandbox.Resources) {
	seen := &sandbox.Resources{}
	return context.WithValue(ctx, sandboxResourcesKey{}, seen), seen
}

// watchSandboxResources records the readings of a finished launch on a
// context marked by withSandboxResources; a launch that read nothing leaves
// them cleared.
func watchSandboxResources(ctx context.Context, res sandbox.Resources) {
	if seen, _ := ctx.Value(sandboxResourcesKey{}).(*sandbox.Resources); seen != nil {
		*seen = res
	}
}

// attemptResources is the evidence of a launch's readings, nil when it read
// none.
func attemptResources(res runner.Result) *run.AttemptResources {
	r := res.Resources
	if !r.DiskKnown && !r.OOMKnown {
		return nil
	}
	evidence := &run.AttemptResources{OOMKilled: r.OOMKnown && r.OOMKilled}
	if r.DiskKnown {
		evidence.DockerDiskFreeBefore, evidence.DockerDiskFreeAfterExit = r.DiskFree0, r.DiskFree1
		if r.ReleasedKnown {
			evidence.DockerDiskFreeAfterDelete = r.DiskFree2
		}
	}
	return evidence
}

// diskRanOut reports a failed launch to be read as out of disk.
func diskRanOut(r sandbox.Resources, baseline bool) bool {
	return r.DiskLow() && (baseline || r.NotFilledByItself())
}

// rerunAfterEnvironmentFailure makes launch like rerunAfterFetchFailure
// does, and then judges its failure by the disk and memory rules at the top
// of this file. baseline is the baseline verify. The second run's attempts
// are numbered after the first's, as rerunAfterFetchFailure numbers its own;
// before and after must not be nil.
//
// As there, the second run is not made when less of the Activity's time is
// left than twice what the first took, and one the deadline left no room to
// start is as if it had not been made.
func rerunAfterEnvironmentFailure(ctx context.Context, logPath func(int) string, before func(int) error, after func(int, runner.Result, error) error, baseline bool, launch commandLaunch) (runner.Result, error) {
	ctx, seen := withSandboxResources(ctx)
	made := 0
	first, err := rerunAfterFetchFailure(ctx, logPath, before, func(attempt int, res runner.Result, runErr error) error {
		made = max(made, attempt)
		return after(attempt, res, runErr)
	}, launch)
	if err != nil || first.ExitCode == 0 {
		return first, err
	}
	if diskRanOut(*seen, baseline) {
		return first, fmt.Errorf("%w (exit code %d)", errSandboxDisk, first.ExitCode)
	}
	if !killedForMemory(*seen) {
		return first, nil
	}
	if baseline {
		return first, fmt.Errorf("%w (exit code %d)", errSandboxMemory, first.ExitCode)
	}
	if !timeToRunAgain(ctx, first) {
		return first, nil
	}
	second, err := rerunAfterFetchFailure(ctx,
		func(attempt int) string { return logPath(made + attempt) },
		func(attempt int) error { return before(made + attempt) },
		func(attempt int, res runner.Result, runErr error) error { return after(made+attempt, res, runErr) },
		launch)
	switch {
	case errors.Is(err, errOutOfTime) && !errors.Is(err, sandbox.ErrCleanupUnconfirmed):
		return first, nil
	case err != nil:
		return second, err
	case second.ExitCode != 0 && killedForMemory(*seen):
		return second, fmt.Errorf("%w (exit code %d)", errSandboxMemory, second.ExitCode)
	case second.ExitCode != 0 && diskRanOut(*seen, baseline):
		return second, fmt.Errorf("%w (exit code %d)", errSandboxDisk, second.ExitCode)
	}
	return second, nil
}

// killedForMemory reports a launch whose worker container Docker reports as
// OOM-killed.
func killedForMemory(r sandbox.Resources) bool {
	return r.OOMKnown && r.OOMKilled
}
