package workflow

import (
	"testing"
	"time"
)

func TestBoundedWorkerOptionsUsesSmallConcurrencyCaps(t *testing.T) {
	const stopTimeout = 17 * time.Second
	options := BoundedWorkerOptions(stopTimeout)
	if options.WorkerStopTimeout != stopTimeout {
		t.Fatalf("WorkerStopTimeout = %v, want %v", options.WorkerStopTimeout, stopTimeout)
	}
	if options.MaxConcurrentActivityExecutionSize != DefaultMaxConcurrentActivityExecutionSize {
		t.Fatalf("MaxConcurrentActivityExecutionSize = %d, want %d", options.MaxConcurrentActivityExecutionSize, DefaultMaxConcurrentActivityExecutionSize)
	}
	if options.MaxConcurrentWorkflowTaskExecutionSize != DefaultMaxConcurrentWorkflowTaskExecutionSize {
		t.Fatalf("MaxConcurrentWorkflowTaskExecutionSize = %d, want %d", options.MaxConcurrentWorkflowTaskExecutionSize, DefaultMaxConcurrentWorkflowTaskExecutionSize)
	}
}
