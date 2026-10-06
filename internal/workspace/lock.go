package workspace

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// ErrBusy means another direct factoryd run currently owns the repository
// lock. Callers should fail rather than wait, so an operator can see which
// invocation needs attention instead of accumulating an unbounded queue.
var ErrBusy = errors.New("repository is busy")

// DirectLock is the inter-process ownership lock for a plain run. The lock
// file is intentionally kept in Git's common metadata directory; the kernel
// lock, rather than file removal, is the ownership mechanism, so a crashed
// process cannot leave a stale lock that blocks future work.
//
// The lock has two modes on one kernel flock. An exclusive holder is the only
// execution in the repository: it may mutate the shared checkout and run the
// stranded-worktree reconcile. Shared holders are isolated runs, each mutating
// only its own worktree and branch, so any number of them coexist; an
// exclusive attempt waits for all of them to finish.
type DirectLock struct {
	file      *os.File
	exclusive bool
}

// HeldExclusive reports whether this handle still owns the lock exclusively.
// Recovery helpers use it to enforce their caller-held ownership contract:
// reconciling stranded worktrees is safe only when no other run is live.
func (l *DirectLock) HeldExclusive() bool {
	return l != nil && l.file != nil && l.exclusive
}

// GitCommonDir returns the canonical Git common directory for repoDir. Git
// worktrees report the same common directory as their main checkout, while
// unrelated repositories report different directories.
func GitCommonDir(repoDir string) (string, error) {
	out, err := exec.Command("git", "-C", repoDir, "rev-parse", "--git-common-dir").Output()
	if err != nil {
		return "", fmt.Errorf("git rev-parse --git-common-dir: %w", err)
	}
	commonDir := strings.TrimSpace(string(out))
	if commonDir == "" {
		return "", errors.New("git rev-parse --git-common-dir returned an empty path")
	}
	if !filepath.IsAbs(commonDir) {
		commonDir = filepath.Join(repoDir, commonDir)
	}
	canonical, err := filepath.EvalSymlinks(commonDir)
	if err != nil {
		return "", fmt.Errorf("resolve Git common directory %q: %w", commonDir, err)
	}
	info, err := os.Stat(canonical)
	if err != nil {
		return "", fmt.Errorf("stat Git common directory %q: %w", canonical, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("git common directory %q is not a directory", canonical)
	}
	return filepath.Clean(canonical), nil
}

// AcquireDirectLock takes the repository-scoped direct-run lock without
// waiting. The lock identity is the Git common directory, not the checkout
// path, so all worktrees of one repository contend with their main checkout.
func AcquireDirectLock(repoDir string) (*DirectLock, error) {
	return acquireDirectLock(context.Background(), repoDir, false, syscall.LOCK_EX)
}

// AcquireDirectLockShared takes the repository lock in shared mode without
// waiting. It fails with ErrBusy only while another process holds the lock
// exclusively; other shared holders do not block it.
func AcquireDirectLockShared(repoDir string) (*DirectLock, error) {
	return acquireDirectLock(context.Background(), repoDir, false, syscall.LOCK_SH)
}

// AcquireDirectLockSharedContext is AcquireDirectLockShared that waits, in
// the same bounded polling increments as AcquireDirectLockContext, for an
// exclusive holder to release.
func AcquireDirectLockSharedContext(ctx context.Context, repoDir string) (*DirectLock, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	return acquireDirectLock(ctx, repoDir, true, syscall.LOCK_SH)
}

// AcquireDirectLockContext takes the same repository lock as
// AcquireDirectLock, but waits in bounded polling increments until ctx is
// canceled or the lock becomes available. This is for repository-owner
// submissions, whose caller already has a finite run/lifecycle deadline.
func AcquireDirectLockContext(ctx context.Context, repoDir string) (*DirectLock, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	return acquireDirectLock(ctx, repoDir, true, syscall.LOCK_EX)
}

func acquireDirectLock(ctx context.Context, repoDir string, wait bool, how int) (*DirectLock, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	commonDir, err := GitCommonDir(repoDir)
	if err != nil {
		return nil, err
	}
	file, err := flockFile(ctx, filepath.Join(commonDir, "factoryd-direct.lock"), how, wait, "direct-run lock")
	if err != nil {
		if errors.Is(err, errLockHeld) {
			return nil, fmt.Errorf("%w: %s", ErrBusy, repoDir)
		}
		return nil, err
	}
	return &DirectLock{file: file, exclusive: how == syscall.LOCK_EX}, nil
}

// errLockHeld is flockFile's non-waiting "someone else holds it" outcome.
var errLockHeld = errors.New("lock is held")

// flockFile opens path (creating it) and takes a flock of mode how
// (LOCK_SH or LOCK_EX) on it. Without wait it fails at once with errLockHeld
// when another process holds a conflicting lock. With wait it polls every
// 25 ms, so ctx can cancel it (a blocking flock(2) cannot be interrupted),
// until the lock is free or ctx ends. what names the lock in errors.
func flockFile(ctx context.Context, path string, how int, wait bool, what string) (*os.File, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open %s %q: %w", what, path, err)
	}
	for {
		err = syscall.Flock(int(file.Fd()), how|syscall.LOCK_NB)
		if err == nil {
			return file, nil
		}
		busy := errors.Is(err, syscall.EAGAIN) || errors.Is(err, syscall.EWOULDBLOCK)
		if !busy {
			_ = file.Close()
			return nil, fmt.Errorf("acquire %s %q: %w", what, path, err)
		}
		if !wait {
			_ = file.Close()
			return nil, errLockHeld
		}
		select {
		case <-ctx.Done():
			_ = file.Close()
			return nil, fmt.Errorf("wait for %s %q: %w", what, path, ctx.Err())
		case <-time.After(25 * time.Millisecond):
		}
	}
}

// Close releases the lock and closes its file descriptor. It is safe to call
// once; a second call is a no-op.
func (l *DirectLock) Close() error {
	if l == nil || l.file == nil {
		return nil
	}
	file := l.file
	l.file = nil
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_UN); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}
