package workspace

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"buildgate/internal/testfixture"
)

func TestDirectLockBlocksSameRepositoryAndReleases(t *testing.T) {
	repoDir := testfixture.NewGitRepo(t)
	first, err := AcquireDirectLock(repoDir)
	if err != nil {
		t.Fatalf("AcquireDirectLock(first): %v", err)
	}
	second, err := AcquireDirectLock(repoDir)
	if !errors.Is(err, ErrBusy) {
		t.Fatalf("AcquireDirectLock(second) error = %v, want ErrBusy", err)
	}
	if second != nil {
		t.Fatal("AcquireDirectLock(second) returned a lock on a busy repository")
	}
	if err := first.Close(); err != nil {
		t.Fatalf("release first lock: %v", err)
	}
	third, err := AcquireDirectLock(repoDir)
	if err != nil {
		t.Fatalf("AcquireDirectLock after release: %v", err)
	}
	if err := third.Close(); err != nil {
		t.Fatalf("release third lock: %v", err)
	}
}

func TestDirectLockContextStopsWaitingWhenCanceled(t *testing.T) {
	repoDir := testfixture.NewGitRepo(t)
	owner, err := AcquireDirectLock(repoDir)
	if err != nil {
		t.Fatalf("AcquireDirectLock(owner): %v", err)
	}
	defer owner.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	lock, err := AcquireDirectLockContext(ctx, repoDir)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("AcquireDirectLockContext error = %v, want context.Canceled", err)
	}
	if lock != nil {
		t.Fatal("AcquireDirectLockContext returned a lock after cancellation")
	}
}

func TestDirectLockDistinctRepositoriesProceed(t *testing.T) {
	firstRepo := testfixture.NewGitRepo(t)
	secondRepo := testfixture.NewGitRepo(t)
	first, err := AcquireDirectLock(firstRepo)
	if err != nil {
		t.Fatalf("AcquireDirectLock(first): %v", err)
	}
	defer first.Close()
	second, err := AcquireDirectLock(secondRepo)
	if err != nil {
		t.Fatalf("AcquireDirectLock(distinct repository): %v", err)
	}
	if err := second.Close(); err != nil {
		t.Fatalf("release second lock: %v", err)
	}
}

func TestDirectLockReleasedWhenOwnerExits(t *testing.T) {
	repoDir := testfixture.NewGitRepo(t)
	cmd := exec.Command(os.Args[0], "-test.run", "^TestDirectLockOwnerHelper$")
	cmd.Env = append(os.Environ(), "FACTORYD_LOCK_OWNER_HELPER=1", "FACTORYD_LOCK_REPO="+repoDir)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("owner stdout pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start owner helper: %v", err)
	}
	ready, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil || ready != "ready\n" {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatalf("owner helper readiness = %q, err=%v", ready, err)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("kill owner helper: %v", err)
	}
	if err := cmd.Wait(); err == nil {
		t.Fatal("owner helper unexpectedly exited successfully after kill")
	}
	lock, err := AcquireDirectLock(repoDir)
	if err != nil {
		t.Fatalf("AcquireDirectLock after owner exit: %v", err)
	}
	if err := lock.Close(); err != nil {
		t.Fatalf("release post-exit lock: %v", err)
	}
}

func TestDirectLockOwnerHelper(t *testing.T) {
	if os.Getenv("FACTORYD_LOCK_OWNER_HELPER") != "1" {
		return
	}
	lock, err := AcquireDirectLock(os.Getenv("FACTORYD_LOCK_REPO"))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer lock.Close()
	fmt.Fprintln(os.Stdout, "ready")
	select {}
}

func TestGitCommonDirIsSharedByWorktree(t *testing.T) {
	repoDir := testfixture.NewGitRepo(t)
	out, err := exec.Command("git", "-C", repoDir, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("get HEAD: %v", err)
	}
	baseSHA := strings.TrimSpace(string(out))
	worktreeParent := t.TempDir()
	worktreePath, _, err := Prepare(repoDir, worktreeParent, "lock-worktree", baseSHA)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	mainCommon, err := GitCommonDir(repoDir)
	if err != nil {
		t.Fatalf("GitCommonDir(main): %v", err)
	}
	worktreeCommon, err := GitCommonDir(worktreePath)
	if err != nil {
		t.Fatalf("GitCommonDir(worktree): %v", err)
	}
	if mainCommon != worktreeCommon {
		t.Fatalf("common dirs differ: main=%q worktree=%q", mainCommon, worktreeCommon)
	}
	lockPath := filepath.Join(mainCommon, "factoryd-direct.lock")
	if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
		t.Fatalf("lock file should not exist before acquisition, stat error=%v", err)
	}
	lock, err := AcquireDirectLock(worktreePath)
	if err != nil {
		t.Fatalf("AcquireDirectLock(worktree): %v", err)
	}
	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("lock file was not created in Git metadata: %v", err)
	}
	if err := lock.Close(); err != nil {
		t.Fatalf("release worktree lock: %v", err)
	}
}
