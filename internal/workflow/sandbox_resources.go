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
// fails after Docker's disk ran out of room has not judged the work, and
// neither has a baseline verify whose sandbox had a process killed for want
// of memory. Both facts are read by the factory from outside the sandbox
// (sandbox.Resources), never from what the command printed; the decision is
// made from the command's exit code and those readings only:
//
//   - failed, and Docker's free space after the exit is under
//     sandbox.ExhaustedDiskFloor while the space the launch itself released
//     (its sandbox, scratch volume, compose services and registry proxy) is
//     under half of what was free before it, so it did not fill the disk
//     itself: the step is unresolved (errSandboxDisk, SandboxDiskFailureType),
//     with no second run, since a second run finds the same disk. The
//     baseline verify, which runs on the base commit the work has not
//     touched, needs only the low free space.
//   - the baseline verify failed, and Docker reports the worker container
//     OOM-killed: unresolved (errSandboxMemory, SandboxMemoryFailureType).
//     Any other step that failed with the worker OOM-killed is an ordinary
//     failure, judged as it was before the readings existed: a build can
//     cause its own OOM, so it is told the fact instead (the attempt records
//     it, and the handoff to the next build names it).
//   - a pass is a pass whatever the readings; a reading that could not be
//     taken is judged as if the rule did not exist.
//
// The build step records the readings and is never unresolved by them: only
// a launch refused for want of disk before its command was released
// (sandbox.ErrSandboxDiskLow) is, for every step.

// errSandboxDisk is the launch error of a command that failed with Docker's
// disk exhausted by something other than the launch.
var errSandboxDisk = errors.New("the command failed with Docker's disk out of room, which the launch did not fill")

// errSandboxMemory is the launch error of a baseline verify that failed in a
// sandbox whose process the kernel killed for want of memory.
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

// failOnEnvironment makes launch like rerunAfterFetchFailure does, and then
// judges its failure by the rules at the top of this file. baseline is the
// baseline verify; before and after must not be nil. The disk is judged
// before the memory.
func failOnEnvironment(ctx context.Context, logPath func(int) string, before func(int) error, after func(int, runner.Result, error) error, baseline bool, launch commandLaunch) (runner.Result, error) {
	ctx, seen := withSandboxResources(ctx)
	res, err := rerunAfterFetchFailure(ctx, logPath, before, after, launch)
	if err != nil || res.ExitCode == 0 {
		return res, err
	}
	switch {
	case diskRanOut(*seen, baseline):
		return res, fmt.Errorf("%w (exit code %d)", errSandboxDisk, res.ExitCode)
	case baseline && killedForMemory(*seen):
		return res, fmt.Errorf("%w (exit code %d)", errSandboxMemory, res.ExitCode)
	}
	return res, nil
}

// killedForMemory reports a launch whose worker container Docker reports as
// OOM-killed.
func killedForMemory(r sandbox.Resources) bool {
	return r.OOMKnown && r.OOMKilled
}
