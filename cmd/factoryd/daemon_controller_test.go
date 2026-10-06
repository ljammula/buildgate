package main

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"buildgate/internal/api"
	"buildgate/internal/daemonheartbeat"
	"buildgate/internal/workflow"
)

func TestServeDaemonControllerStartListStopSupportsRepositoryIdentity(t *testing.T) {
	dp := newTestDeps(t)
	t.Parallel()
	config := testSuperviseConfig(t.TempDir())
	controller := newServeDaemonController(dp, config)
	process := newFakeSuperviseProcess(true)
	var gotArgs []string
	controller.factory = func(args []string) (superviseProcess, error) {
		gotArgs = append([]string(nil), args...)
		return process, nil
	}
	repository := "team/repo with spaces"
	status, err := controller.Start(context.Background(), repository)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if status.Repository != repository || status.State != "running" || status.PID != process.PID() {
		t.Fatalf("Start status = %+v", status)
	}
	if want := config.supervisorArgs(repository); !reflect.DeepEqual(gotArgs, want) {
		t.Fatalf("child args = %#v, want %#v", gotArgs, want)
	}
	statuses, err := controller.List(context.Background())
	if err != nil || len(statuses) != 1 || statuses[0].Repository != repository {
		t.Fatalf("List = %#v, %v", statuses, err)
	}
	if err := controller.Stop(context.Background(), repository); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if got := process.gotSignal(); got != syscall.SIGTERM {
		t.Fatalf("stop signal = %v, want SIGTERM", got)
	}
	if err := controller.Stop(context.Background(), repository); !errors.Is(err, api.ErrDaemonNotFound) {
		t.Fatalf("second Stop = %v, want ErrDaemonNotFound", err)
	}
}

func TestServeDaemonControllerSerializesConcurrentDuplicateStarts(t *testing.T) {
	dp := newTestDeps(t)
	t.Parallel()
	controller := newServeDaemonController(dp, testSuperviseConfig(t.TempDir()))
	controller.factory = func(_ []string) (superviseProcess, error) {
		return newFakeSuperviseProcess(false), nil
	}
	const callers = 12
	results := make(chan error, callers)
	var wg sync.WaitGroup
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := controller.Start(context.Background(), "same/repo")
			results <- err
		}()
	}
	wg.Wait()
	close(results)
	var successes, conflicts int
	for err := range results {
		switch {
		case err == nil:
			successes++
		case errors.Is(err, api.ErrDaemonConflict):
			conflicts++
		default:
			t.Fatalf("unexpected concurrent start error: %v", err)
		}
	}
	if successes != 1 || conflicts != callers-1 {
		t.Fatalf("concurrent starts: successes=%d conflicts=%d", successes, conflicts)
	}
}

func TestServeDaemonControllerRejectsConcurrentStops(t *testing.T) {
	dp := newTestDeps(t)
	t.Parallel()
	config := testSuperviseConfig(t.TempDir(), "repo")
	config.stopTimeout = 10 * time.Millisecond
	controller := newServeDaemonController(dp, config)
	process := newFakeSuperviseProcess(false)
	process.signalCall = make(chan struct{}, 1)
	controller.factory = func(_ []string) (superviseProcess, error) { return process, nil }
	if _, err := controller.Start(context.Background(), "repo"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	firstDone := make(chan error, 1)
	go func() { firstDone <- controller.Stop(context.Background(), "repo") }()
	select {
	case <-process.signalCall:
	case <-time.After(time.Second):
		t.Fatal("first Stop did not signal child")
	}
	if err := controller.Stop(context.Background(), "repo"); !errors.Is(err, api.ErrDaemonConflict) {
		t.Fatalf("concurrent Stop = %v, want ErrDaemonConflict", err)
	}
	if err := <-firstDone; err != nil {
		t.Fatalf("first Stop: %v", err)
	}
}

func TestServeDaemonControllerFailedStopCanBeRetried(t *testing.T) {
	dp := newTestDeps(t)
	t.Parallel()
	controller := newServeDaemonController(dp, testSuperviseConfig(t.TempDir()))
	process := newFakeSuperviseProcess(false)
	process.signalErr = errors.New("transient signal failure")
	process.killErr = errors.New("transient kill failure")
	controller.factory = func(_ []string) (superviseProcess, error) { return process, nil }
	if _, err := controller.Start(context.Background(), "repo"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := controller.Stop(context.Background(), "repo"); err == nil {
		t.Fatal("first Stop succeeded despite signal and kill failures")
	}
	process.mu.Lock()
	process.signalErr = nil
	process.killErr = nil
	process.autoStop = true
	process.mu.Unlock()
	if err := controller.Stop(context.Background(), "repo"); err != nil {
		t.Fatalf("retry Stop: %v", err)
	}
}

// TestStopManagedDaemonSucceedsWhenAlreadyExited is the regression test for
// a real finding from adversarial review (2026-09-01): a managed daemon
// that crashes or exits naturally at the same moment an operator calls
// Stop can make both Signal and Kill fail (a real "process already
// finished" style OS error), which stopManagedDaemon used to surface as a
// hard error even though the daemon is actually gone -- unlike
// stopSuperviseChild in supervisor.go, its structurally parallel
// counterpart, which already checked for this race.
func TestStopManagedDaemonSucceedsWhenAlreadyExited(t *testing.T) {
	t.Parallel()
	process := newFakeSuperviseProcess(false)
	process.signalErr = errors.New("os: process already finished")
	process.killErr = errors.New("os: process already finished")
	child := &managedDaemon{process: process, done: make(chan error, 1)}
	// Simulate the process having already exited on its own, independent
	// of this Stop call, before Signal is ever sent -- child.done is what
	// stopManagedDaemon actually reads, delivered in production by the
	// exit-watcher goroutine Start spawns (see child.done's own doc
	// comment); reproduced directly here since this test constructs child
	// by hand rather than going through the controller.
	child.done <- nil

	if err := stopManagedDaemon(context.Background(), child, time.Second); err != nil {
		t.Fatalf("stopManagedDaemon = %v, want nil (process had already exited)", err)
	}
	if process.killCalled {
		t.Error("Kill was called despite the process already having exited")
	}
}

func TestServeDaemonControllerStopAllWaitsForChildren(t *testing.T) {
	dp := newTestDeps(t)
	t.Parallel()
	controller := newServeDaemonController(dp, testSuperviseConfig(t.TempDir(), "a", "b"))
	processes := make([]*fakeSuperviseProcess, 0, 2)
	controller.factory = func(_ []string) (superviseProcess, error) {
		process := newFakeSuperviseProcess(true)
		processes = append(processes, process)
		return process, nil
	}
	if _, err := controller.Start(context.Background(), "a"); err != nil {
		t.Fatalf("start a: %v", err)
	}
	if _, err := controller.Start(context.Background(), "b"); err != nil {
		t.Fatalf("start b: %v", err)
	}
	if err := controller.StopAll(context.Background()); err != nil {
		t.Fatalf("StopAll: %v", err)
	}
	if statuses, err := controller.List(context.Background()); err != nil || len(statuses) != 0 {
		t.Fatalf("List after StopAll = %#v, %v", statuses, err)
	}
	for _, process := range processes {
		if got := process.gotSignal(); got != syscall.SIGTERM {
			t.Errorf("shutdown signal = %v, want SIGTERM", got)
		}
	}
}

func TestServeDaemonControllerStopAllSkipsExitedChildren(t *testing.T) {
	dp := newTestDeps(t)
	t.Parallel()
	controller := newServeDaemonController(dp, testSuperviseConfig(t.TempDir()))
	process := newFakeSuperviseProcess(false)
	controller.factory = func(_ []string) (superviseProcess, error) { return process, nil }
	if _, err := controller.Start(context.Background(), "repo"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	controller.mu.Lock()
	child := controller.children["repo"]
	controller.mu.Unlock()
	process.finish(errors.New("crashed"))
	<-child.done
	if err := controller.StopAll(context.Background()); err != nil {
		t.Fatalf("StopAll: %v", err)
	}
	if got := process.gotSignal(); got != nil {
		t.Fatalf("StopAll signaled exited child with %v", got)
	}
	if statuses, err := controller.List(context.Background()); err != nil || len(statuses) != 0 {
		t.Fatalf("List after exited StopAll = %#v, %v", statuses, err)
	}
}

func TestServeDaemonControllerRejectsFreshExistingHeartbeatButAllowsStale(t *testing.T) {
	dp := newTestDeps(t)
	t.Parallel()
	dataDir := t.TempDir()
	config := testSuperviseConfig(dataDir)
	controller := newServeDaemonController(dp, config)
	var starts int
	controller.factory = func(_ []string) (superviseProcess, error) {
		starts++
		return newFakeSuperviseProcess(true), nil
	}
	repository := "repo/with space"
	ownerID := workflow.RepositoryOwnerWorkflowID(repository)
	write := func(updatedAt time.Time) {
		t.Helper()
		if err := daemonheartbeat.Write(daemonheartbeat.Path(dataDir, ownerID), daemonheartbeat.Heartbeat{
			Repository: repository,
			TaskQueue:  "factoryd-repo-" + ownerID,
			PID:        123,
			UpdatedAt:  updatedAt.Format(time.RFC3339Nano),
		}); err != nil {
			t.Fatalf("write heartbeat: %v", err)
		}
	}
	write(time.Now())
	if _, err := controller.Start(context.Background(), repository); !errors.Is(err, api.ErrDaemonConflict) {
		t.Fatalf("Start with fresh heartbeat = %v, want ErrDaemonConflict", err)
	}
	write(time.Now().Add(-2 * daemonheartbeat.SandboxStaleAfter))
	if _, err := controller.Start(context.Background(), repository); err != nil {
		t.Fatalf("Start with stale heartbeat: %v", err)
	}
	if starts != 1 {
		t.Fatalf("factory starts = %d, want 1", starts)
	}
	if err := controller.StopAll(context.Background()); err != nil {
		t.Fatalf("StopAll: %v", err)
	}
}

func TestDrainServeComponentsStopsDaemonsAfterHTTPFailure(t *testing.T) {
	t.Parallel()
	var httpCalled, daemonCalled bool
	shutdownErr := errors.New("listener failed")
	daemonErr := errors.New("daemon stop failed")
	err := drainServeComponents(context.Background(), func(context.Context) error {
		httpCalled = true
		return shutdownErr
	}, func(context.Context) error {
		daemonCalled = true
		return daemonErr
	}, &sync.WaitGroup{})
	if !httpCalled || !daemonCalled {
		t.Fatalf("shutdown calls = http:%v daemon:%v, want both", httpCalled, daemonCalled)
	}
	if !errors.Is(err, shutdownErr) || !errors.Is(err, daemonErr) {
		t.Fatalf("drainServeComponents error = %v, want both errors", err)
	}
}

func TestSanitizedSuperviseEnvironmentRemovesControlTokens(t *testing.T) {
	t.Parallel()
	environment := sanitizedSuperviseEnvironment([]string{
		"PATH=/bin",
		startTokenEnvironmentVariable + "=start-secret",
		overrideTokenEnvironmentVariable + "=override-secret",
		"FACTORYD_TEST=value",
	})
	joined := strings.Join(environment, "\n")
	if strings.Contains(joined, "start-secret") || strings.Contains(joined, "override-secret") {
		t.Fatalf("sanitized environment leaked control token: %q", joined)
	}
	if !strings.Contains(joined, "PATH=/bin") || !strings.Contains(joined, "FACTORYD_TEST=value") {
		t.Fatalf("sanitized environment dropped unrelated values: %q", joined)
	}
}

func TestServeDaemonControllerRejectsInvalidRepository(t *testing.T) {
	dp := newTestDeps(t)
	t.Parallel()
	controller := newServeDaemonController(dp, testSuperviseConfig(t.TempDir()))
	for _, repository := range []string{"", "  "} {
		if _, err := controller.Start(context.Background(), repository); !errors.Is(err, api.ErrInvalidDaemonRequest) {
			t.Errorf("Start(%q) = %v, want ErrInvalidDaemonRequest", repository, err)
		}
		if err := controller.Stop(context.Background(), repository); !errors.Is(err, api.ErrInvalidDaemonRequest) {
			t.Errorf("Stop(%q) = %v, want ErrInvalidDaemonRequest", repository, err)
		}
	}
}
