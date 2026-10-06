package workspace

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"buildgate/internal/testfixture"
)

func TestDirectLockSharedHoldersCoexist(t *testing.T) {
	repoDir := testfixture.NewGitRepo(t)
	first, err := AcquireDirectLockShared(repoDir)
	if err != nil {
		t.Fatalf("AcquireDirectLockShared(first): %v", err)
	}
	defer first.Close()
	second, err := AcquireDirectLockShared(repoDir)
	if err != nil {
		t.Fatalf("AcquireDirectLockShared(second) beside a shared holder: %v", err)
	}
	defer second.Close()
	if first.HeldExclusive() || second.HeldExclusive() {
		t.Fatalf("shared holders report exclusive: first=%v second=%v", first.HeldExclusive(), second.HeldExclusive())
	}
}

func TestDirectLockExclusiveWaitsForSharedHolders(t *testing.T) {
	repoDir := testfixture.NewGitRepo(t)
	shared, err := AcquireDirectLockShared(repoDir)
	if err != nil {
		t.Fatalf("AcquireDirectLockShared: %v", err)
	}
	if _, err := AcquireDirectLock(repoDir); !errors.Is(err, ErrBusy) {
		t.Fatalf("exclusive beside a shared holder: error = %v, want ErrBusy", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := AcquireDirectLockContext(ctx, repoDir); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waiting exclusive beside a shared holder: error = %v, want deadline exceeded", err)
	}
	if err := shared.Close(); err != nil {
		t.Fatalf("release shared: %v", err)
	}
	exclusive, err := AcquireDirectLock(repoDir)
	if err != nil {
		t.Fatalf("exclusive after the shared holder released: %v", err)
	}
	if !exclusive.HeldExclusive() {
		t.Fatal("exclusive lock does not report exclusive mode")
	}
	exclusive.Close()
}

func TestDirectLockExclusiveHolderBlocksShared(t *testing.T) {
	repoDir := testfixture.NewGitRepo(t)
	exclusive, err := AcquireDirectLock(repoDir)
	if err != nil {
		t.Fatalf("AcquireDirectLock: %v", err)
	}
	defer exclusive.Close()
	if _, err := AcquireDirectLockShared(repoDir); !errors.Is(err, ErrBusy) {
		t.Fatalf("shared beside an exclusive holder: error = %v, want ErrBusy", err)
	}
}

func TestDirectLockSharedWaitsOutShortExclusiveHold(t *testing.T) {
	repoDir := testfixture.NewGitRepo(t)
	exclusive, err := AcquireDirectLock(repoDir)
	if err != nil {
		t.Fatalf("AcquireDirectLock: %v", err)
	}
	go func() {
		time.Sleep(150 * time.Millisecond)
		exclusive.Close()
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	shared, err := AcquireDirectLockSharedContext(ctx, repoDir)
	if err != nil {
		t.Fatalf("shared attempt did not wait out the exclusive hold: %v", err)
	}
	defer shared.Close()
	if shared.HeldExclusive() {
		t.Fatal("shared lock reports exclusive mode")
	}
}

func TestDirectLockSharedWaitHonoursContextCancel(t *testing.T) {
	repoDir := testfixture.NewGitRepo(t)
	exclusive, err := AcquireDirectLock(repoDir)
	if err != nil {
		t.Fatalf("AcquireDirectLock: %v", err)
	}
	defer exclusive.Close()
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()
	lock, err := AcquireDirectLockSharedContext(ctx, repoDir)
	if !errors.Is(err, context.Canceled) || lock != nil {
		t.Fatalf("shared wait after cancel: lock=%v err=%v, want context.Canceled", lock, err)
	}
}

func TestAddDetachedWorktreeWaitsForGitMetadataLock(t *testing.T) {
	repoDir := testfixture.NewGitRepo(t)
	wt := filepath.Join(t.TempDir(), "draft")
	unlock, err := lockGitMetadata(repoDir)
	if err != nil {
		t.Fatalf("lockGitMetadata: %v", err)
	}
	var done atomic.Bool
	result := make(chan error, 1)
	go func() {
		err := AddDetachedWorktree(repoDir, wt, "HEAD")
		done.Store(true)
		result <- err
	}()
	time.Sleep(300 * time.Millisecond)
	if done.Load() {
		t.Fatal("AddDetachedWorktree ran while another holder had the git metadata lock")
	}
	unlock()
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("AddDetachedWorktree after release: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("AddDetachedWorktree did not proceed after the lock was released")
	}
	if _, err := os.Stat(filepath.Join(wt, ".git")); err != nil {
		t.Fatalf("detached worktree not created: %v", err)
	}
	if err := RemoveWorktreeOnly(repoDir, wt); err != nil {
		t.Fatalf("RemoveWorktreeOnly: %v", err)
	}
}

func headSHA(t *testing.T, repoDir string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", repoDir, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("rev-parse HEAD: %v", err)
	}
	return strings.TrimSpace(string(out))
}

func TestGitMetadataLockSerializesConcurrentPrepare(t *testing.T) {
	repoDir := testfixture.NewGitRepo(t)
	base := headSHA(t, repoDir)
	parent := t.TempDir()
	unlock, err := lockGitMetadata(repoDir)
	if err != nil {
		t.Fatalf("lockGitMetadata: %v", err)
	}
	var done atomic.Bool
	result := make(chan error, 1)
	go func() {
		_, _, err := Prepare(repoDir, parent, "serial-1", base)
		done.Store(true)
		result <- err
	}()
	time.Sleep(300 * time.Millisecond)
	if done.Load() {
		t.Fatal("Prepare completed while another holder had the git metadata lock")
	}
	unlock()
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("Prepare after the lock was released: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Prepare did not proceed after the git metadata lock was released")
	}
	common, err := GitCommonDir(repoDir)
	if err != nil {
		t.Fatalf("GitCommonDir: %v", err)
	}
	if _, err := os.Stat(filepath.Join(common, "factoryd-git.lock")); err != nil {
		t.Fatalf("git metadata lock file missing: %v", err)
	}
}

func TestConcurrentPrepareOfDistinctRunsAllSucceed(t *testing.T) {
	repoDir := testfixture.NewGitRepo(t)
	base := headSHA(t, repoDir)
	parent := t.TempDir()
	const n = 6
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		id := "concurrent-" + string(rune('a'+i))
		go func() {
			path, branch, err := Prepare(repoDir, parent, id, base)
			if err == nil {
				err = Remove(repoDir, path, branch)
			}
			errs <- err
		}()
	}
	for i := 0; i < n; i++ {
		if err := <-errs; err != nil {
			t.Errorf("concurrent Prepare/Remove: %v", err)
		}
	}
}

func TestRetryOnGitLockRecoversFromLockFileError(t *testing.T) {
	old := gitLockRetryBackoff
	gitLockRetryBackoff = time.Millisecond
	defer func() { gitLockRetryBackoff = old }()
	calls := 0
	out, err := retryOnGitLock(func() ([]byte, error) {
		calls++
		if calls < 3 {
			return []byte("fatal: Unable to create '/r/.git/worktrees/x/index.lock': File exists."), errors.New("exit status 128")
		}
		return []byte("ok"), nil
	})
	if err != nil || string(out) != "ok" || calls != 3 {
		t.Fatalf("retry: out=%q err=%v calls=%d, want ok after 3 calls", out, err, calls)
	}
}

func TestRetryOnGitLockGivesUpAndIgnoresOtherErrors(t *testing.T) {
	old := gitLockRetryBackoff
	gitLockRetryBackoff = time.Millisecond
	defer func() { gitLockRetryBackoff = old }()
	calls := 0
	_, err := retryOnGitLock(func() ([]byte, error) {
		calls++
		return []byte("Unable to create 'x.lock': File exists."), errors.New("exit status 128")
	})
	if err == nil || calls != gitLockRetryAttempts {
		t.Fatalf("persistent lock error: err=%v calls=%d, want failure after %d calls", err, calls, gitLockRetryAttempts)
	}
	calls = 0
	_, err = retryOnGitLock(func() ([]byte, error) {
		calls++
		return []byte("fatal: not a branch"), errors.New("exit status 1")
	})
	if err == nil || calls != 1 {
		t.Fatalf("unrelated error: err=%v calls=%d, want one call", err, calls)
	}
}
