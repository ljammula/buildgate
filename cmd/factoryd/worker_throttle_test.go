package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"buildgate/internal/workflow"
)

// Every Temporal worker factoryd builds bounds the SDK's heartbeat throttle,
// so a stall cancels the SDK's context well inside the heartbeat timeout and
// the step guard's own heartbeats begin in time (workflow.HeartbeatThrottle).
func TestEveryTemporalWorkerBoundsTheHeartbeatThrottle(t *testing.T) {
	if got := workflow.BoundedWorkerOptions(0).MaxHeartbeatThrottleInterval; got != workflow.HeartbeatThrottle {
		t.Errorf("BoundedWorkerOptions throttle = %v, want %v", got, workflow.HeartbeatThrottle)
	}
	if got := jobsWorkerOptions(3).MaxHeartbeatThrottleInterval; got != workflow.HeartbeatThrottle {
		t.Errorf("jobs worker throttle = %v, want %v", got, workflow.HeartbeatThrottle)
	}
	if got := lightWorkerOptions().MaxHeartbeatThrottleInterval; got != workflow.HeartbeatThrottle {
		t.Errorf("light worker throttle = %v, want %v", got, workflow.HeartbeatThrottle)
	}

	// No worker is built from options that skip them.
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	allowed := regexp.MustCompile(`temporalworker\.New\([^,]+,[^,]+, (workflow\.BoundedWorkerOptions\(|workflow\.RunWorkerOptions\(|jobsWorkerOptions\(|lightWorkerOptions\(|workerOptions\)|workflow\.BoundedWorkerOptions\(\s*$)`)
	found := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(string(b), "\n") {
			if !strings.Contains(line, "temporalworker.New(") {
				continue
			}
			found++
			if !allowed.MatchString(line) {
				t.Errorf("%s builds a worker with options that do not bound the heartbeat throttle: %s", f, strings.TrimSpace(line))
			}
		}
	}
	if found < 7 {
		t.Errorf("found %d worker constructions, want at least the 7 that exist", found)
	}
}
