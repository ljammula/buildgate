package main

import (
	"testing"
	"time"
)

// TestWorkerStopTimeoutIsThirtySeconds pins the production Worker stop
// timeout. Tests that stop an in-process Worker holding a blocked Activity
// pass their own short timeout to workflow.BoundedWorkerOptions instead of
// waiting this out (see reconcileAfterFailedBuild); this keeps that choice
// from drifting into the production value.
func TestWorkerStopTimeoutIsThirtySeconds(t *testing.T) {
	t.Parallel()
	if workerStopTimeout != 30*time.Second {
		t.Fatalf("workerStopTimeout = %v, want 30s", workerStopTimeout)
	}
}
