package main

import (
	"context"
	"testing"
	"time"

	"buildgate/internal/progress"
	wsisolation "buildgate/internal/workspace"
)

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
	var hooked []bool
	ctx := withRepoLockWaitHook(withWaitForRepoLock(context.Background()), func(waiting bool) { hooked = append(hooked, waiting) })
	lock, err := acquireIsolatedRunLock(ctx, ws, func(*wsisolation.DirectLock) {}, repoLockReport{dataDir: dataDir, runID: "run-3"})
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	events, _ := progress.Read(progress.Path(dataDir, "run-3"))
	if len(events) != 2 || events[0].Stage != "repository_lock" || events[0].Event != "waiting" || events[0].Detail == "" || events[1].Event != "acquired" {
		t.Errorf("feed = %+v, want repository_lock waiting (with a reason) then acquired", events)
	}
	if len(hooked) != 2 || !hooked[0] || hooked[1] {
		t.Errorf("hook calls = %v, want [true false]", hooked)
	}

	// An idle repository: no wait, no lines.
	idle := newFixtureRepo(t)
	free, err := acquireIsolatedRunLock(ctx, idle, func(*wsisolation.DirectLock) {}, repoLockReport{dataDir: dataDir, runID: "run-2"})
	if err != nil {
		t.Fatal(err)
	}
	defer free.Close()
	if events, _ := progress.Read(progress.Path(dataDir, "run-2")); len(events) != 0 {
		t.Errorf("uncontended lock wrote %+v, want nothing", events)
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
	if _, err := acquireIsolatedRunLock(ctx, ws, func(*wsisolation.DirectLock) {}, repoLockReport{dataDir: dataDir, runID: "run-4"}); err == nil {
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

// A wait for a branch's lock is its own progress step, naming the branch.
func TestBranchLockWaitIsReportedAsBranchLock(t *testing.T) {
	ws := newFixtureRepo(t)
	dataDir := t.TempDir()
	ctx := withWaitForRepoLock(context.Background())
	holder, err := wsisolation.AcquireBranchLock(ws, "pr-a")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(150 * time.Millisecond)
		_ = holder.Close()
	}()
	lock, err := acquireBranchRunLock(ctx, ws, "pr-a", repoLockReport{dataDir: dataDir, runID: "run-1"}.forBranch("pr-a"))
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	events, err := progress.Read(progress.Path(dataDir, "run-1"))
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[0].Stage != "branch_lock" || events[0].Event != "waiting" || events[0].Detail != "branch pr-a" || events[1].Stage != "branch_lock" || events[1].Event != "acquired" {
		t.Errorf("feed = %+v, want branch_lock waiting (branch pr-a) then acquired", events)
	}
}
