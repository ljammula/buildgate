package workflow

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"buildgate/internal/modelhost"
	"buildgate/internal/progress"
	"buildgate/internal/sandbox"
)

// TestRunSandboxWithRetriesAcquiresAndReleasesModelHostLock is the
// direct/Temporal parity's own Temporal-path proof: the same
// internal/modelhost lock cmd/factoryd/sandbox_exec.go's
// runSandboxWithRetries acquires must also serialize
// the Temporal Activity path against a concurrent holder of the same
// private/single-instance upstream (this closes a real direct/Temporal
// drift, not a hypothetical one).
//
// A private-address upstream is used deliberately (modelhost.ShouldLock
// only locks a non-TLS or private/loopback/Tailscale/.lan/.local
// upstream by default; testRelayPolicy's own default
// "https://models.example/v1" is a public host and would never engage
// the lock at all). An external holder acquires the lock first, so this
// Activity's own call must wait -- proven by the fake docker never being
// invoked before the external holder releases, and by progress.jsonl carrying a "queued behind
// external-holder" model_host_lock event while it waits.
func TestRunSandboxWithRetriesAcquiresAndReleasesModelHostLock(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	upstream := "https://10.55.0.9:8080"

	externalHandle, err := modelhost.Acquire(context.Background(), upstream, "external-holder", 1, nil)
	if err != nil || externalHandle == nil {
		t.Fatalf("pre-acquire external model-host lock: handle=%v err=%v", externalHandle, err)
	}
	released := false
	defer func() {
		if !released {
			_ = externalHandle.Release()
		}
	}()

	runID := "run-temporal-lock"
	runDataDir := t.TempDir()
	policy := testRelayPolicy()
	policy.Upstream = upstream
	relaySpec := policy.Spec(sandbox.NewRouteSecret("worker-static-upstream-key"), sandbox.RouteSecret{}, sandbox.RouteSecret{}, sandbox.RouteSecret{}, runID, runDataDir)

	logDir := t.TempDir()
	// The worker launch fails at once: the point is proving it was reached
	// (and only after the lock released), not driving a real container.
	launched := filepath.Join(t.TempDir(), "launched")
	docker := fakeDockerBinary(t, "touch "+launched+"\nexit 125\n")
	activities := &Activities{
		LogDir:               t.TempDir(),
		ModelHostConcurrency: 1,
		SandboxDocker:        docker,
	}
	input := RunWorkflowInput{
		Ticket:        "fixture-ticket",
		WorkspacePath: t.TempDir(),
		SandboxDocker: docker,
		SandboxImage:  "factory-worker:test@sha256:deadbeef",
		RunID:         runID,
		DataDir:       runDataDir,
		LogDir:        logDir,
	}
	logPath := func(int) string { return filepath.Join(t.TempDir(), "build.log") }

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = activities.runSandboxWithRetries(context.Background(), input, logPath, 1, nil, nil, &relaySpec, nil, nil, "", "", nil, nil, "sh", "-c", "true")
	}()

	deadline := time.Now().Add(10 * time.Second)
	sawQueued := false
	for time.Now().Before(deadline) {
		b, _ := os.ReadFile(progress.PathInDir(logDir))
		if strings.Contains(string(b), "queued behind external-holder") {
			sawQueued = true
			break
		}
		select {
		case <-done:
			t.Fatal("Activity finished before the external model-host lock was ever released")
		case <-time.After(50 * time.Millisecond):
		}
		if _, err := os.Stat(launched); err == nil {
			t.Fatal("the worker was launched while the external model-host lock was still held")
		}
	}
	if !sawQueued {
		t.Fatal("progress.jsonl never recorded a \"queued behind external-holder\" model_host_lock event")
	}

	if err := externalHandle.Release(); err != nil {
		t.Fatalf("release external lock: %v", err)
	}
	released = true

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Activity did not complete after the external model-host lock was released")
	}

	// The Activity's own lock must be released too, once it finishes --
	// otherwise a second Acquire for the same upstream would still block.
	reacquired, err := modelhost.Acquire(context.Background(), upstream, "post-test-check", 1, nil)
	if err != nil {
		t.Fatalf("re-acquire model-host lock after Activity finished: %v", err)
	}
	if reacquired == nil {
		t.Fatal("model-host lock still held after the Activity finished -- the Activity's own Release never ran")
	}
	_ = reacquired.Release()
}

// TestRunSandboxWithRetriesSkipsModelHostLockWhenConcurrencyZero proves
// the model_host_concurrency: 0 escape hatch reaches the Temporal path
// too: with ModelHostConcurrency unset (0, the Activities zero value --
// see that field's own doc comment on why 0 means disabled here, unlike
// every other Sandbox* field), an external holder of the same private
// upstream must never block this Activity at all.
func TestRunSandboxWithRetriesSkipsModelHostLockWhenConcurrencyZero(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	upstream := "https://10.55.0.10:8080"

	externalHandle, err := modelhost.Acquire(context.Background(), upstream, "external-holder", 1, nil)
	if err != nil || externalHandle == nil {
		t.Fatalf("pre-acquire external model-host lock: handle=%v err=%v", externalHandle, err)
	}
	defer externalHandle.Release()

	runID := "run-temporal-nolock"
	runDataDir := t.TempDir()
	policy := testRelayPolicy()
	policy.Upstream = upstream
	relaySpec := policy.Spec(sandbox.NewRouteSecret("worker-static-upstream-key"), sandbox.RouteSecret{}, sandbox.RouteSecret{}, sandbox.RouteSecret{}, runID, runDataDir)

	docker := fakeDockerBinary(t, "exit 125\n")
	activities := &Activities{
		LogDir: t.TempDir(),
		// ModelHostConcurrency deliberately left at its zero value.
		SandboxDocker: docker,
	}
	input := RunWorkflowInput{
		Ticket:        "fixture-ticket",
		WorkspacePath: t.TempDir(),
		SandboxDocker: docker,
		SandboxImage:  "factory-worker:test@sha256:deadbeef",
		RunID:         runID,
		DataDir:       runDataDir,
	}
	logPath := func(int) string { return filepath.Join(t.TempDir(), "build.log") }

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = activities.runSandboxWithRetries(context.Background(), input, logPath, 1, nil, nil, &relaySpec, nil, nil, "", "", nil, nil, "sh", "-c", "true")
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the launch never ran despite ModelHostConcurrency == 0 (disabled) and an external holder of the same upstream")
	}
}
