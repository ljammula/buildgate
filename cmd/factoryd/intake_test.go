package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestIntakeRefusesConcurrentInvocationOnSamePilotDir proves
// withIntakeLock fails fast, in-process, when a second invocation targets
// a pilot dir another invocation is already holding: it must return
// immediately with an error naming the pilot dir, rather than blocking
// until the first releases the lock. Unlike
// TestIntegrationIntakeFailsFastAcrossProcesses (which drives a real
// `factoryd intake` subprocess end to end), this exercises withIntakeLock
// itself directly, the same way TestDrainQueueRefusesToStartWhileAnotherDrainerHoldsTheLock
// exercises acquireWorkerLock directly.
func TestIntakeRefusesConcurrentInvocationOnSamePilotDir(t *testing.T) {
	t.Parallel()
	pilotDir := filepath.Join(t.TempDir(), "pilot")
	if err := os.MkdirAll(pilotDir, 0o750); err != nil {
		t.Fatalf("mkdir pilot dir: %v", err)
	}

	firstHolding := make(chan struct{})
	release := make(chan struct{})
	firstDone := make(chan error, 1)
	go func() {
		firstDone <- withIntakeLock(pilotDir, func() error {
			close(firstHolding)
			<-release
			return nil
		})
	}()

	select {
	case <-firstHolding:
	case <-time.After(5 * time.Second):
		t.Fatal("first withIntakeLock invocation never started")
	}

	err := withIntakeLock(pilotDir, func() error {
		t.Fatal("second withIntakeLock invocation ran fn while the first still held the lock")
		return nil
	})

	close(release)
	if firstErr := <-firstDone; firstErr != nil {
		t.Fatalf("first withIntakeLock invocation: %v", firstErr)
	}

	if err == nil {
		t.Fatal("second withIntakeLock invocation while the first still holds the lock = nil, want a busy error")
	}
	if !strings.Contains(err.Error(), "already running against pilot dir") {
		t.Fatalf("err = %v, want it to say another intake is already running against the pilot dir", err)
	}
	resolvedPilotDir, resolveErr := resolveExistingAncestor(pilotDir)
	if resolveErr != nil {
		t.Fatalf("resolve pilot dir for comparison: %v", resolveErr)
	}
	if !strings.Contains(err.Error(), resolvedPilotDir) {
		t.Fatalf("err = %v, want it to name the pilot dir %s", err, resolvedPilotDir)
	}
}
