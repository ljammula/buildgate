package main

import (
	"context"
	"testing"
	"time"

	"buildgate/internal/progress"
	wsisolation "buildgate/internal/workspace"
)

// TestDirectRunLockWaitIsReported: a build that waits for the repository lock
// inside its slot writes a repository_lock waiting line, then an acquired line
// once it holds the lock, to its run's progress feed, and tells the step's
// hook both times; a lock taken without a wait writes nothing.
func TestDirectRunLockWaitIsReported(t *testing.T) {
	ws := newFixtureRepo(t)
	dataDir := t.TempDir()
	report := repoLockReport{dataDir: dataDir, runID: "run-1"}
	var hooked []bool
	ctx := withRepoLockWaitHook(withWaitForRepoLock(context.Background()), func(waiting bool) { hooked = append(hooked, waiting) })

	holder, err := wsisolation.AcquireDirectLock(ws)
	if err != nil {
		t.Fatal(err)
	}
	released := make(chan struct{})
	go func() {
		defer close(released)
		time.Sleep(150 * time.Millisecond)
		_ = holder.Close()
	}()
	lock, err := acquireDirectRunLock(ctx, ws, report)
	<-released
	if err != nil {
		t.Fatalf("acquireDirectRunLock: %v", err)
	}
	defer lock.Close()

	events, err := progress.Read(progress.Path(dataDir, "run-1"))
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[0].Stage != "repository_lock" || events[0].Event != "waiting" || events[0].Detail == "" || events[1].Event != "acquired" {
		t.Errorf("feed = %+v, want repository_lock waiting (with a reason) then acquired", events)
	}
	if len(hooked) != 2 || !hooked[0] || hooked[1] {
		t.Errorf("hook calls = %v, want [true false]", hooked)
	}

	// An idle repository: no wait, no lines.
	idle := newFixtureRepo(t)
	free, err := acquireDirectRunLock(ctx, idle, repoLockReport{dataDir: dataDir, runID: "run-2"})
	if err != nil {
		t.Fatal(err)
	}
	defer free.Close()
	if events, _ := progress.Read(progress.Path(dataDir, "run-2")); len(events) != 0 {
		t.Errorf("uncontended lock wrote %+v, want nothing", events)
	}
}

// TestIsolatedRunLockWaitIsReported: an isolated run's wait for the shared
// lock behind an exclusive holder is reported the same way.
func TestIsolatedRunLockWaitIsReported(t *testing.T) {
	ws := newFixtureRepo(t)
	dataDir := t.TempDir()
	holder, err := wsisolation.AcquireDirectLock(ws)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(150 * time.Millisecond)
		_ = holder.Close()
	}()
	lock, err := acquireIsolatedRunLock(context.Background(), ws, func(*wsisolation.DirectLock) {}, repoLockReport{dataDir: dataDir, runID: "run-3"})
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	events, _ := progress.Read(progress.Path(dataDir, "run-3"))
	if len(events) != 2 || events[0].Event != "waiting" || events[1].Event != "acquired" {
		t.Errorf("feed = %+v, want waiting then acquired", events)
	}
}

// TestAFailedLockWaitIsClosedInTheFeed: a wait that ends without the lock (the
// context ended) writes a closing acquired line with outcome fail, so the feed
// no longer reads as waiting, and tells the hook the wait is over.
func TestAFailedLockWaitIsClosedInTheFeed(t *testing.T) {
	ws := newFixtureRepo(t)
	dataDir := t.TempDir()
	holder, err := wsisolation.AcquireDirectLock(ws)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	var hooked []bool
	ctx, cancel := context.WithCancel(withRepoLockWaitHook(withWaitForRepoLock(context.Background()), func(waiting bool) { hooked = append(hooked, waiting) }))
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()
	if _, err := acquireDirectRunLock(ctx, ws, repoLockReport{dataDir: dataDir, runID: "run-4"}); err == nil {
		t.Fatal("acquired a held lock")
	}
	sum, err := progress.Summary(progress.Path(dataDir, "run-4"))
	if err != nil || sum.WaitingStage != "" || sum.WaitingReason != "" {
		t.Errorf("summary after a failed wait = %+v, %v, want no wait", sum, err)
	}
	if len(hooked) != 2 || !hooked[0] || hooked[1] {
		t.Errorf("hook calls = %v, want [true false]", hooked)
	}
}
