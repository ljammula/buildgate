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
)

// BoundedWorkerOptions centralizes the Temporal worker concurrency guard so
// the short-lived CLI workers and the long-lived repository daemon cannot
// silently drift to the SDK's very large defaults. The cap is per Worker;
// repository ownership still provides the cross-process one-run invariant.
func BoundedWorkerOptions(stopTimeout time.Duration) temporalworker.Options {
	return temporalworker.Options{
		WorkerStopTimeout:                      stopTimeout,
		MaxConcurrentActivityExecutionSize:     DefaultMaxConcurrentActivityExecutionSize,
		MaxConcurrentWorkflowTaskExecutionSize: DefaultMaxConcurrentWorkflowTaskExecutionSize,
	}
}
