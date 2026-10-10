package runner

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A .gitignore that is a FIFO blocks git status for good; the check a caller
// gives a deadline must return by it, and the plain check must still answer
// for an ordinary worktree.
func TestGitIsCleanContextReturnsByItsDeadline(t *testing.T) {
	dir := t.TempDir()
	if out, err := exec.Command("git", "-C", dir, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	if clean, err := GitIsClean(dir); err != nil || !clean {
		t.Fatalf("GitIsClean of an empty repository = %v, %v, want true", clean, err)
	}
	if err := syscall.Mkfifo(filepath.Join(dir, ".gitignore"), 0o600); err != nil {
		t.Skipf("no FIFO on this filesystem: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := GitIsCleanContext(ctx, dir)
	if err == nil || !strings.Contains(err.Error(), "deadline") {
		t.Fatalf("GitIsCleanContext on a worktree whose .gitignore is a FIFO = %v, want a deadline error", err)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("returned %v after a deadline of 500ms", took)
	}
}
