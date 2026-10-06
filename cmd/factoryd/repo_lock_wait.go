package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	wsisolation "buildgate/internal/workspace"
)

// waitForRepoLockKey marks a context whose build waits for a busy repository
// lock instead of failing: a worker runs several requests at once, and two
// tickets of one repository must queue, not halt the second request.
// worker runs one build at a time and keeps the immediate failure.
type waitForRepoLockKey struct{}

func withWaitForRepoLock(ctx context.Context) context.Context {
	return context.WithValue(ctx, waitForRepoLockKey{}, true)
}

func waitsForRepoLock(ctx context.Context) bool {
	wait, _ := ctx.Value(waitForRepoLockKey{}).(bool)
	return wait
}

// repoBusy reports whether a build of workspace, with durable state in
// dataDir, cannot take its direct-run lock now, probing without blocking and
// releasing at once. A build that will be isolated (dataDir outside the
// workspace, as run_ticket.go decides) holds the lock shared, so the probe is
// shared: other isolated builds holding it do not make the repository busy,
// only a run that owns the shared checkout does. A build that will be
// non-isolated (dataDir inside the workspace) needs the lock exclusively, so
// any holder makes the repository busy. Any other failure (not a git
// repository, say) is not "busy": the build itself reports it.
func repoBusy(workspace, dataDir string) bool {
	probe := wsisolation.AcquireDirectLockShared
	if inside, _, _, err := dataDirInsideWorkspace(workspace, dataDir); err == nil && inside {
		probe = wsisolation.AcquireDirectLock
	}
	lock, err := probe(workspace)
	if err != nil {
		return errors.Is(err, wsisolation.ErrBusy)
	}
	if err := lock.Close(); err != nil {
		log.Printf("release repository probe lock: %v", err)
	}
	return false
}

// acquireDirectRunLock takes workspace's direct-run lock. A ctx marked by
// withWaitForRepoLock waits (logging once) until the holder releases it or
// ctx ends; any other ctx fails at once with wsisolation.ErrBusy.
func acquireDirectRunLock(ctx context.Context, workspace string) (*wsisolation.DirectLock, error) {
	lock, err := wsisolation.AcquireDirectLock(workspace)
	if err == nil || !waitsForRepoLock(ctx) || !errors.Is(err, wsisolation.ErrBusy) {
		return lock, err
	}
	log.Printf("waiting for the repository lock held by another build: %s", workspace)
	return wsisolation.AcquireDirectLockContext(ctx, workspace)
}

// isolatedRunLockWait bounds how long an isolated run waits for the shared
// lock while another process holds it exclusively (a run reconciling stranded
// worktrees, a non-isolated build, a reclaim). A variable for tests.
var isolatedRunLockWait = 2 * time.Minute

// acquireIsolatedRunLock takes workspace's direct-run lock, shared, for a run
// in its own worktree. If the repository is idle (an exclusive, non-blocking
// try succeeds) it first calls reconcile under that exclusive hold, then
// releases it: flock conversion is not atomic, so there is no downgrade.
// It then takes the lock shared, polling so ctx can cancel, for up to
// isolatedRunLockWait; two isolated runs starting together thus never fail
// because the other briefly holds the lock exclusively for its reconcile.
// Past the bound it returns wsisolation.ErrBusy, in every mode.
func acquireIsolatedRunLock(ctx context.Context, workspace string, reconcile func(*wsisolation.DirectLock)) (*wsisolation.DirectLock, error) {
	exclusive, err := wsisolation.AcquireDirectLock(workspace)
	switch {
	case err == nil:
		reconcile(exclusive)
		if err := exclusive.Close(); err != nil {
			return nil, fmt.Errorf("release repository reconcile lock: %w", err)
		}
	case !errors.Is(err, wsisolation.ErrBusy):
		return nil, err
	}
	waitCtx, cancel := context.WithTimeout(ctx, isolatedRunLockWait)
	defer cancel()
	lock, err := wsisolation.AcquireDirectLockSharedContext(waitCtx, workspace)
	if err != nil && ctx.Err() == nil && errors.Is(err, context.DeadlineExceeded) {
		return nil, fmt.Errorf("%w: %s (an exclusive holder kept it for %s)", wsisolation.ErrBusy, workspace, isolatedRunLockWait)
	}
	return lock, err
}
