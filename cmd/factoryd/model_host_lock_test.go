package main

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"buildgate/internal/progress"
	"buildgate/internal/sandbox"
	"buildgate/internal/sessionconfig"
)

// TestAcquireModelHostLockNilRelaySpecIsNoOp proves canonical verification
// and the full-suite gate -- neither of which is ever given a relaySpec --
// never touch the filesystem-backed lock at all.
func TestAcquireModelHostLockNilRelaySpecIsNoOp(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	handle, err := acquireModelHostLock(context.Background(), t.TempDir(), "run-1", nil)
	if err != nil || handle != nil {
		t.Fatalf("acquireModelHostLock(nil relaySpec) = %v, %v, want nil, nil", handle, err)
	}
}

// TestAcquireModelHostLockSerializesJobsAndEmitsProgress is the
// model-host-lock end-to-end proof at the actual integration point: two
// model-bound jobs
// targeting the same private upstream serialize through
// runSandboxWithRetries's own acquireModelHostLock call, and the waiting
// job's progress.jsonl carries a "queued behind" model_host_lock event --
// the operator-visibility requirement, not just the lock mechanics
// internal/modelhost's own tests already cover.
func TestAcquireModelHostLockSerializesJobsAndEmitsProgress(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dataDir := t.TempDir()

	settings := sessionconfig.DefaultSettings()
	settings.ModelHostConcurrency = 1
	tier2SettingsOverride = &settings
	t.Cleanup(func() { tier2SettingsOverride = nil })

	relaySpec := &sandbox.RouteSpec{RoutePolicy: sandbox.RoutePolicy{Upstream: "https://10.99.0.1:8080"}}

	first, err := acquireModelHostLock(context.Background(), dataDir, "run-first", relaySpec)
	if err != nil {
		t.Fatalf("acquireModelHostLock(first): %v", err)
	}
	if first == nil {
		t.Fatal("acquireModelHostLock(first) returned a nil handle for a private upstream with concurrency 1")
	}

	secondDone := make(chan struct{})
	var secondErr error
	go func() {
		defer close(secondDone)
		handle, err := acquireModelHostLock(context.Background(), dataDir, "run-second", relaySpec)
		secondErr = err
		if handle != nil {
			_ = handle.Release()
		}
	}()

	deadline := time.Now().Add(5 * time.Second)
	var sawQueued bool
	for time.Now().Before(deadline) {
		b, _ := os.ReadFile(progress.Path(dataDir, "run-second"))
		if strings.Contains(string(b), "queued behind run-first") {
			sawQueued = true
			break
		}
		select {
		case <-secondDone:
			t.Fatal("second acquireModelHostLock completed before the first lock was released")
		case <-time.After(50 * time.Millisecond):
		}
	}
	if !sawQueued {
		t.Fatal("run-second's progress.jsonl never recorded a \"queued behind run-first\" model_host_lock event")
	}

	if err := first.Release(); err != nil {
		t.Fatalf("Release(first): %v", err)
	}
	select {
	case <-secondDone:
	case <-time.After(5 * time.Second):
		t.Fatal("second acquireModelHostLock did not complete after the first was released")
	}
	if secondErr != nil {
		t.Fatalf("acquireModelHostLock(second): %v", secondErr)
	}
}

// TestAcquireModelHostLockDisabledByZeroConcurrency proves the
// model_host_concurrency: 0 escape hatch reaches acquireModelHostLock.
func TestAcquireModelHostLockDisabledByZeroConcurrency(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	settings := sessionconfig.DefaultSettings()
	settings.ModelHostConcurrency = 0
	tier2SettingsOverride = &settings
	t.Cleanup(func() { tier2SettingsOverride = nil })

	relaySpec := &sandbox.RouteSpec{RoutePolicy: sandbox.RoutePolicy{Upstream: "https://10.99.0.2:8080"}}
	handle, err := acquireModelHostLock(context.Background(), t.TempDir(), "run-1", relaySpec)
	if err != nil || handle != nil {
		t.Fatalf("acquireModelHostLock(concurrency 0) = %v, %v, want nil, nil", handle, err)
	}
}

// TestAcquireModelHostLockSkipsRemoteSaaSUpstream proves a public HTTPS
// upstream (the chatgpt.com/api.githubcopilot.com/api.anthropic.com
// shape) is unlocked even with the default concurrency, per
// modelhost.ShouldLock's own decision.
func TestAcquireModelHostLockSkipsRemoteSaaSUpstream(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	settings := sessionconfig.DefaultSettings()
	tier2SettingsOverride = &settings
	t.Cleanup(func() { tier2SettingsOverride = nil })

	relaySpec := &sandbox.RouteSpec{RoutePolicy: sandbox.RoutePolicy{Upstream: "https://api.anthropic.com"}}
	handle, err := acquireModelHostLock(context.Background(), t.TempDir(), "run-1", relaySpec)
	if err != nil || handle != nil {
		t.Fatalf("acquireModelHostLock(remote SaaS upstream) = %v, %v, want nil, nil", handle, err)
	}
}
