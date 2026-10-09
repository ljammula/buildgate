package release

import (
	"os"
	"os/exec"
	"strings"
	"testing"

	"buildgate/internal/testfixture"
	"buildgate/internal/workspace"
)

// newFixtureRepo is this package's name for the shared fixture, matching
// every other package's local alias for it (see cmd/factoryd/integration_test.go).
func newFixtureRepo(t *testing.T) string {
	t.Helper()
	return testfixture.NewGitRepo(t)
}

// TestRollbackDiscardsWorktreeAndBranch verifies that Rollback successfully
// discards the isolated worktree and its branch when a run is rejected.
func TestRollbackDiscardsWorktreeAndBranch(t *testing.T) {
	repoDir := newFixtureRepo(t)

	// Get the current HEAD SHA as our base.
	out, err := exec.Command("git", "-C", repoDir, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("get HEAD SHA: %v", err)
	}
	headSHA := string(out)
	if n := len(headSHA); n > 0 && headSHA[n-1] == '\n' {
		headSHA = headSHA[:n-1]
	}

	parentDir := t.TempDir()
	runID := "test-rollback"

	// Create a worktree using workspace.Prepare.
	worktreePath, branch, err := workspace.Prepare(repoDir, parentDir, runID, headSHA)
	if err != nil {
		t.Fatalf("Prepare failed: %v", err)
	}

	// Verify the worktree exists before rollback.
	if _, err := os.Stat(worktreePath); err != nil {
		t.Fatalf("worktree should exist before Rollback: %v", err)
	}

	// Call Rollback.
	if err := Rollback(repoDir, worktreePath, branch, false); err != nil {
		t.Fatalf("Rollback failed: %v", err)
	}

	// Verify the worktree no longer exists.
	if _, err := os.Stat(worktreePath); err == nil {
		t.Fatalf("worktree should not exist after Rollback")
	} else if !os.IsNotExist(err) {
		t.Fatalf("unexpected error checking worktree: %v", err)
	}

	// Verify the branch no longer exists.
	out, err = exec.Command("git", "-C", repoDir, "branch", "--list", branch).Output()
	if err != nil {
		t.Fatalf("git branch --list failed: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("branch should not exist after Rollback, but got output: %s", out)
	}
}

// TestRollbackOfAnOnBranchWorkspaceKeepsTheBranch verifies that a run on an
// existing branch loses its worktree but not the branch or its commits.
func TestRollbackOfAnOnBranchWorkspaceKeepsTheBranch(t *testing.T) {
	repoDir := newFixtureRepo(t)
	git := func(args ...string) string {
		t.Helper()
		out, err := exec.Command("git", append([]string{"-C", repoDir}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("branch", "feature")
	worktreePath, err := workspace.PrepareOnBranch(repoDir, t.TempDir(), "keep-run", "feature")
	if err != nil {
		t.Fatalf("PrepareOnBranch: %v", err)
	}
	if out, err := exec.Command("git", "-C", worktreePath, "-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "--allow-empty", "-m", "extra").CombinedOutput(); err != nil {
		t.Fatalf("commit: %v: %s", err, out)
	}
	tip := git("rev-parse", "refs/heads/feature")

	if err := Rollback(repoDir, worktreePath, "feature", true); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if _, err := os.Stat(worktreePath); !os.IsNotExist(err) {
		t.Fatalf("worktree still present after Rollback: %v", err)
	}
	if got := git("rev-parse", "refs/heads/feature"); got != tip {
		t.Fatalf("feature = %s after Rollback, want %s", got, tip)
	}
}
