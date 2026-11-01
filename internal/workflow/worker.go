package workflow

import (
	"time"

	temporalworker "go.temporal.io/sdk/worker"
)

const (
	// DefaultMaxConcurrentActivityExecutionSize is intentionally one: a
	// repository worker must never run two side-effecting build/verify
	// Activities at once, and this is the small global cap used by every
	// factoryd Temporal Worker.
	DefaultMaxConcurrentActivityExecutionSize = 1
	// DefaultMaxConcurrentWorkflowTaskExecutionSize leaves room for the
	// owner and child Workflow tasks to make progress while Activities are
	// bounded independently.
	DefaultMaxConcurrentWorkflowTaskExecutionSize = 4
	// RunMaxConcurrentActivityExecutionSize is the cap of a worker whose task
	// queue serves one run only (RunWorkerOptions): two. The code
	// review and the challenger job use both at once (reviewWithChallenge); they
	// share no worktree: the challenger works on a scratch worktree of the
	// result commit, and its files are committed after both have finished. The
	// tests_red_on_base proof and a challenge that runs after the review launch
	// up to two reproductions at a time (parallelReproductionLaunchesChange); a
	// challenge beside the review launches one at a time, so the review keeps a
	// slot. Every other Activity of RunWorkflow starts alone.
	RunMaxConcurrentActivityExecutionSize = 2
)

// BoundedWorkerOptions centralizes the Temporal worker concurrency guard so
// the short-lived CLI workers and the long-lived repository daemon cannot
// silently drift to the SDK's very large defaults. The cap is per Worker;
// repository ownership still provides the cross-process one-run invariant.
func BoundedWorkerOptions(stopTimeout time.Duration) temporalworker.Options {
	return temporalworker.Options{
		WorkerStopTimeout:                      stopTimeout,
		MaxHeartbeatThrottleInterval:           HeartbeatThrottle,
		MaxConcurrentActivityExecutionSize:     DefaultMaxConcurrentActivityExecutionSize,
		MaxConcurrentWorkflowTaskExecutionSize: DefaultMaxConcurrentWorkflowTaskExecutionSize,
	}
}

// RunWorkerOptions are BoundedWorkerOptions for a worker whose task queue
// serves one run's RunWorkflow and nothing else, so the review and the
// challenger can run at once (RunMaxConcurrentActivityExecutionSize). A
// queue shared by several runs keeps BoundedWorkerOptions.
func RunWorkerOptions(stopTimeout time.Duration) temporalworker.Options {
	options := BoundedWorkerOptions(stopTimeout)
	options.MaxConcurrentActivityExecutionSize = RunMaxConcurrentActivityExecutionSize
	return options
}
