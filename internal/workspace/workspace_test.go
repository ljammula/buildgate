package workspace

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"buildgate/internal/testfixture"
)

// newFixtureRepo is this package's name for the shared fixture, matching
// every other package's local alias for it (see cmd/factoryd/integration_test.go).
func newFixtureRepo(t *testing.T) string {
	t.Helper()
	return testfixture.NewGitRepo(t)
}

// TestPrepareResolvesRelativeParentDir is the regression test for a real
// finding from codex review (round 2, 2026-08-28): Prepare's own contract
// promises an absolute worktreePath, but a relative parentDir used to
// produce a relative worktreePath too. Worse, `git -C repoDir worktree
// add <worktreePath>` resolves a relative path argument against repoDir
// (because of -C), while every Go-side consumer of the returned path
// (os.Stat here, a caller elsewhere) resolves it against this process's
// own working directory -- two different locations for the same nominal
// path. Prepare must resolve parentDir to absolute before using it either
// way.
func TestPrepareResolvesRelativeParentDir(t *testing.T) {
	repoDir := newFixtureRepo(t)
	out, err := exec.Command("git", "-C", repoDir, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("get HEAD SHA: %v", err)
	}
	baseSHA := string(out)
	if n := len(baseSHA); n > 0 && baseSHA[n-1] == '\n' {
		baseSHA = baseSHA[:n-1]
	}

	// A relative parentDir only means something once this process's cwd
	// is pinned to a known directory -- t.Chdir restores it automatically.
	cwd := t.TempDir()
	t.Chdir(cwd)

	worktreePath, _, err := Prepare(repoDir, "relative-parent", "test-run-relative", baseSHA)
	if err != nil {
		t.Fatalf("Prepare failed: %v", err)
	}
	if !filepath.IsAbs(worktreePath) {
		t.Fatalf("worktreePath = %q, want an absolute path", worktreePath)
	}
	wantPath := filepath.Join(cwd, "relative-parent", "test-run-relative")
	if worktreePath != wantPath {
		t.Fatalf("worktreePath = %q, want %q", worktreePath, wantPath)
	}
	if info, err := os.Stat(worktreePath); err != nil || !info.IsDir() {
		t.Fatalf("worktree does not exist at the returned absolute path: %v", err)
	}
}

// TestPrepareCreatesWorktreeAtBranch verifies that Prepare successfully
// creates a worktree at the returned path, and that the worktree is checked
// out to the correct branch at the correct base commit.
func TestPrepareCreatesWorktreeAtBranch(t *testing.T) {
	repoDir := newFixtureRepo(t)

	// Get the current HEAD SHA as our base.
	out, err := exec.Command("git", "-C", repoDir, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("get HEAD SHA: %v", err)
	}
	baseSHA := string(out)
	if n := len(baseSHA); n > 0 && baseSHA[n-1] == '\n' {
		baseSHA = baseSHA[:n-1]
	}

	parentDir := t.TempDir()
	runID := "test-run-1"

	worktreePath, branch, err := Prepare(repoDir, parentDir, runID, baseSHA)
	if err != nil {
		t.Fatalf("Prepare failed: %v", err)
	}

	// Verify the returned worktree path exists.
	if info, err := os.Stat(worktreePath); err != nil {
		t.Fatalf("worktree path does not exist: %v", err)
	} else if !info.IsDir() {
		t.Fatalf("worktree path is not a directory")
	}

	// Verify the branch name is correct.
	expectedBranch := "factoryd/" + runID
	if branch != expectedBranch {
		t.Fatalf("branch name mismatch: got %q, want %q", branch, expectedBranch)
	}

	// Verify the worktree is checked out to the correct branch.
	out, err = exec.Command("git", "-C", worktreePath, "rev-parse", "--abbrev-ref", "HEAD").Output()
	if err != nil {
		t.Fatalf("get current branch in worktree: %v", err)
	}
	currentBranch := string(out)
	if n := len(currentBranch); n > 0 && currentBranch[n-1] == '\n' {
		currentBranch = currentBranch[:n-1]
	}
	if currentBranch != expectedBranch {
		t.Fatalf("worktree checked out to wrong branch: got %q, want %q", currentBranch, expectedBranch)
	}

	// Verify the worktree HEAD is at the correct commit.
	out, err = exec.Command("git", "-C", worktreePath, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("get HEAD SHA in worktree: %v", err)
	}
	worktreeHEAD := string(out)
	if n := len(worktreeHEAD); n > 0 && worktreeHEAD[n-1] == '\n' {
		worktreeHEAD = worktreeHEAD[:n-1]
	}
	if worktreeHEAD != baseSHA {
		t.Fatalf("worktree HEAD mismatch: got %q, want %q", worktreeHEAD, baseSHA)
	}
}

// TestPrepareWorktreePathIsIsolatedFromRepoDir verifies that changes made
// in the worktree do not appear in the original repository's working tree.
func TestPrepareWorktreePathIsIsolatedFromRepoDir(t *testing.T) {
	repoDir := newFixtureRepo(t)

	// Get the current HEAD SHA as our base.
	out, err := exec.Command("git", "-C", repoDir, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("get HEAD SHA: %v", err)
	}
	baseSHA := string(out)
	if n := len(baseSHA); n > 0 && baseSHA[n-1] == '\n' {
		baseSHA = baseSHA[:n-1]
	}

	parentDir := t.TempDir()
	runID := "test-run-2"

	worktreePath, _, err := Prepare(repoDir, parentDir, runID, baseSHA)
	if err != nil {
		t.Fatalf("Prepare failed: %v", err)
	}

	// Write a file in the worktree.
	testFilePath := filepath.Join(worktreePath, "worktree-file.txt")
	if err := os.WriteFile(testFilePath, []byte("worktree content\n"), 0o644); err != nil {
		t.Fatalf("write test file in worktree: %v", err)
	}

	// Verify the file does not exist in the original repo.
	repoFilePath := filepath.Join(repoDir, "worktree-file.txt")
	if _, err := os.Stat(repoFilePath); err == nil {
		t.Fatalf("file should not exist in original repo, but it does")
	} else if !os.IsNotExist(err) {
		t.Fatalf("unexpected error checking repo file: %v", err)
	}
}

// TestRemoveDeletesWorktreeAndBranch verifies that Remove successfully
// deletes both the worktree directory and the branch from the repository.
func TestRemoveDeletesWorktreeAndBranch(t *testing.T) {
	repoDir := newFixtureRepo(t)

	// Get the current HEAD SHA as our base.
	out, err := exec.Command("git", "-C", repoDir, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("get HEAD SHA: %v", err)
	}
	baseSHA := string(out)
	if n := len(baseSHA); n > 0 && baseSHA[n-1] == '\n' {
		baseSHA = baseSHA[:n-1]
	}

	parentDir := t.TempDir()
	runID := "test-run-3"

	worktreePath, branch, err := Prepare(repoDir, parentDir, runID, baseSHA)
	if err != nil {
		t.Fatalf("Prepare failed: %v", err)
	}

	// Verify the worktree exists.
	if _, err := os.Stat(worktreePath); err != nil {
		t.Fatalf("worktree should exist before Remove: %v", err)
	}

	// Call Remove.
	if err := Remove(repoDir, worktreePath, branch); err != nil {
		t.Fatalf("Remove failed: %v", err)
	}

	// Verify the worktree no longer exists.
	if _, err := os.Stat(worktreePath); err == nil {
		t.Fatalf("worktree should not exist after Remove")
	} else if !os.IsNotExist(err) {
		t.Fatalf("unexpected error checking worktree: %v", err)
	}

	// Verify the branch no longer exists.
	out, err = exec.Command("git", "-C", repoDir, "branch", "--list", branch).Output()
	if err != nil {
		t.Fatalf("git branch --list failed: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("branch should not exist after Remove, but got output: %s", out)
	}
}

// TestRemoveSucceedsWhenWorktreePathAlreadyGone verifies that Remove still
// succeeds and deletes the branch even if the worktree directory has already
// been deleted manually.
func TestRemoveSucceedsWhenWorktreePathAlreadyGone(t *testing.T) {
	repoDir := newFixtureRepo(t)

	// Get the current HEAD SHA as our base.
	out, err := exec.Command("git", "-C", repoDir, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("get HEAD SHA: %v", err)
	}
	baseSHA := string(out)
	if n := len(baseSHA); n > 0 && baseSHA[n-1] == '\n' {
		baseSHA = baseSHA[:n-1]
	}

	parentDir := t.TempDir()
	runID := "test-run-4"

	worktreePath, branch, err := Prepare(repoDir, parentDir, runID, baseSHA)
	if err != nil {
		t.Fatalf("Prepare failed: %v", err)
	}

	// Manually delete the worktree directory.
	if err := os.RemoveAll(worktreePath); err != nil {
		t.Fatalf("remove worktree directory: %v", err)
	}

	// Call Remove — it should still succeed.
	if err := Remove(repoDir, worktreePath, branch); err != nil {
		t.Fatalf("Remove failed when worktree path already gone: %v", err)
	}

	// Verify the branch was still deleted.
	out, err = exec.Command("git", "-C", repoDir, "branch", "--list", branch).Output()
	if err != nil {
		t.Fatalf("git branch --list failed: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("branch should not exist after Remove, but got output: %s", out)
	}
}

// TestPrepareRejectsUnknownBaseSHA verifies that Prepare fails when given
// a non-existent base SHA, and does not leave behind a worktree directory.
func TestPrepareRejectsUnknownBaseSHA(t *testing.T) {
	repoDir := newFixtureRepo(t)

	parentDir := t.TempDir()
	runID := "test-run-5"
	unknownSHA := "0000000000000000000000000000000000000000"

	worktreePath, _, err := Prepare(repoDir, parentDir, runID, unknownSHA)
	if err == nil {
		t.Fatalf("Prepare should have failed with unknown SHA, but did not")
	}

	// Verify no worktree directory was left behind.
	if _, err := os.Stat(worktreePath); err == nil {
		t.Fatalf("worktree should not exist after failed Prepare")
	} else if !os.IsNotExist(err) {
		t.Fatalf("unexpected error checking worktree: %v", err)
	}
}

// TestPrepareRejectsGitUnsafeRunID is the regression test for a real
// finding from codex review (round 2, 2026-08-28): a runID that is a
// perfectly safe path component can still be an illegal git ref name
// (e.g. two consecutive dots anywhere in the name, not just as a whole
// path segment) -- Prepare must catch this with a clear error before
// `git worktree add -b` ever runs, not let it fail as an opaque
// subprocess error deep inside that call.
func TestPrepareRejectsGitUnsafeRunID(t *testing.T) {
	repoDir := newFixtureRepo(t)
	headSHA, err := exec.Command("git", "-C", repoDir, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("get HEAD SHA: %v", err)
	}
	baseSHA := string(headSHA)
	if n := len(baseSHA); n > 0 && baseSHA[n-1] == '\n' {
		baseSHA = baseSHA[:n-1]
	}

	_, _, err = Prepare(repoDir, t.TempDir(), "foo..bar", baseSHA)
	if err == nil {
		t.Fatal("Prepare should have failed for a git-ref-unsafe runID, but did not")
	}
}

// TestPrepareCleansUpBranchOnFailedWorktreeAdd is the regression test for
// a real finding from a GitHub Codex App review comment (2026-08-29):
// `git worktree add -b <branch>` creates the branch ref before it checks
// whether the destination path is already occupied, so a failure there
// still leaves the just-created branch behind -- confirmed with a raw
// git repro before this fix (a second `worktree add` at an
// already-occupied path exits nonzero but still creates its own new
// branch). Left uncleaned, a retry with the same runID would fail
// forever with "branch already exists", worse than the original error.
// Reproduced here by occupying the destination path with an unrelated
// worktree first (a different branch, not created via Prepare), then
// calling Prepare with a runID chosen so its own path collides with it.
func TestPrepareCleansUpBranchOnFailedWorktreeAdd(t *testing.T) {
	repoDir := newFixtureRepo(t)
	headSHA, err := exec.Command("git", "-C", repoDir, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("get HEAD SHA: %v", err)
	}
	baseSHA := string(headSHA)
	if n := len(baseSHA); n > 0 && baseSHA[n-1] == '\n' {
		baseSHA = baseSHA[:n-1]
	}

	parentDir := t.TempDir()
	runID := "colliding-run"
	occupiedPath := filepath.Join(parentDir, runID)

	// Occupy the destination with an unrelated worktree first, bypassing
	// Prepare so its branch name doesn't match what Prepare will try.
	if out, err := exec.Command("git", "-C", repoDir, "worktree", "add", "-b", "manual-blocker-branch", occupiedPath, baseSHA).CombinedOutput(); err != nil {
		t.Fatalf("set up colliding worktree: %v: %s", err, out)
	}
	// Uncommitted content in the pre-existing worktree, so a real GitHub
	// Codex App finding (third review round, 2026-08-29) has something
	// concrete to lose: an unconditional `worktree remove --force` on any
	// worktree-add failure would force-remove this pre-existing,
	// unrelated worktree too, discarding it.
	blockerFile := filepath.Join(occupiedPath, "uncommitted-work.txt")
	if err := os.WriteFile(blockerFile, []byte("do not delete me\n"), 0o644); err != nil {
		t.Fatalf("write uncommitted content in the blocking worktree: %v", err)
	}

	_, _, err = Prepare(repoDir, parentDir, runID, baseSHA)
	if err == nil {
		t.Fatal("Prepare should have failed on the already-occupied destination path, but did not")
	}

	branchList, err := exec.Command("git", "-C", repoDir, "branch", "--list", "factoryd/"+runID).Output()
	if err != nil {
		t.Fatalf("git branch --list: %v", err)
	}
	if len(branchList) != 0 {
		t.Errorf("branch %q still exists after Prepare's own worktree add failed, want it cleaned up", "factoryd/"+runID)
	}
	// The pre-existing, unrelated worktree (and its uncommitted content)
	// must survive: Prepare's own cleanup must never force-remove a
	// worktree it didn't create itself.
	if _, err := os.Stat(blockerFile); err != nil {
		t.Errorf("pre-existing worktree's uncommitted file was removed by Prepare's own cleanup, want it preserved: %v", err)
	}
	blockerList, err := exec.Command("git", "-C", repoDir, "worktree", "list").Output()
	if err != nil {
		t.Fatalf("git worktree list: %v", err)
	}
	if !strings.Contains(string(blockerList), "manual-blocker-branch") {
		t.Errorf("pre-existing worktree was deregistered by Prepare's own cleanup, want it preserved:\n%s", blockerList)
	}
}

// TestPrepareDoesNotDeletePreexistingBranchOnFailure is the regression
// test for a real finding from a second GitHub Codex App review round
// (2026-08-29) on the fix directly above: if `factoryd/<runID>` already
// existed *before* this call -- e.g. it's the durable, preserved result
// of an earlier accepted isolated run whose worktree has already been
// removed -- `git worktree add -b` fails immediately without ever
// touching that branch (confirmed with a raw git repro: only the branch
// collides, no worktree path is involved). The unconditional cleanup in
// the fix above would still have deleted it, destroying that run's only
// remaining record. Only a branch this call itself creates may be
// cleaned up by this call.
func TestPrepareDoesNotDeletePreexistingBranchOnFailure(t *testing.T) {
	repoDir := newFixtureRepo(t)
	headSHA, err := exec.Command("git", "-C", repoDir, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("get HEAD SHA: %v", err)
	}
	baseSHA := string(headSHA)
	if n := len(baseSHA); n > 0 && baseSHA[n-1] == '\n' {
		baseSHA = baseSHA[:n-1]
	}

	runID := "preexisting-run"
	// The branch already exists, with no worktree at all -- exactly the
	// durable-result-of-an-accepted-run shape this test targets.
	if out, err := exec.Command("git", "-C", repoDir, "branch", "factoryd/"+runID).CombinedOutput(); err != nil {
		t.Fatalf("create pre-existing branch: %v: %s", err, out)
	}

	_, _, err = Prepare(repoDir, t.TempDir(), runID, baseSHA)
	if err == nil {
		t.Fatal("Prepare should have failed because the branch already exists, but did not")
	}

	branchList, err := exec.Command("git", "-C", repoDir, "branch", "--list", "factoryd/"+runID).Output()
	if err != nil {
		t.Fatalf("git branch --list: %v", err)
	}
	if len(branchList) == 0 {
		t.Error("pre-existing branch was deleted by Prepare's own failure cleanup, want it preserved")
	}
}

// TestPrepareRemovesPartiallyRegisteredWorktreeOnHookFailure is the
// regression test for a real finding from a second GitHub Codex App
// review round (2026-08-29): with a failing post-checkout hook, `git
// worktree add -b` can fully register the worktree and check out the
// branch before reporting failure (confirmed with a raw git repro) --
// git then refuses to `branch -D` a branch checked out in that
// still-registered worktree, silently failing the cleanup and leaving
// both the worktree and branch stranded with no path back to them
// (Prepare returns no paths on error). Prepare must clear the worktree
// registration first so the branch is actually free to delete.
func TestPrepareRemovesPartiallyRegisteredWorktreeOnHookFailure(t *testing.T) {
	repoDir := newFixtureRepo(t)
	headSHA, err := exec.Command("git", "-C", repoDir, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("get HEAD SHA: %v", err)
	}
	baseSHA := string(headSHA)
	if n := len(baseSHA); n > 0 && baseSHA[n-1] == '\n' {
		baseSHA = baseSHA[:n-1]
	}

	hooksDir := filepath.Join(repoDir, ".git", "hooks")
	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		t.Fatalf("mkdir hooks dir: %v", err)
	}
	hookPath := filepath.Join(hooksDir, "post-checkout")
	if err := os.WriteFile(hookPath, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatalf("write post-checkout hook: %v", err)
	}

	runID := "hook-failure-run"
	parentDir := t.TempDir()
	worktreePath, branch, err := Prepare(repoDir, parentDir, runID, baseSHA)
	if err == nil {
		t.Fatal("Prepare should have failed because the post-checkout hook exits nonzero, but did not")
	}

	listOut, err := exec.Command("git", "-C", repoDir, "worktree", "list").Output()
	if err != nil {
		t.Fatalf("git worktree list: %v", err)
	}
	if strings.Contains(string(listOut), runID) {
		t.Errorf("git worktree list still references the failed run's worktree, want it cleared:\n%s", listOut)
	}
	branchList, err := exec.Command("git", "-C", repoDir, "branch", "--list", "factoryd/"+runID).Output()
	if err != nil {
		t.Fatalf("git branch --list: %v", err)
	}
	if len(branchList) != 0 {
		t.Errorf("branch %q still exists after the hook-triggered failure, want it cleaned up", "factoryd/"+runID)
	}
	// Prepare's own recorded worktreePath/branch return values are empty
	// on error, so this also confirms nothing is left for a caller to
	// even attempt cleaning up via Remove separately.
	if worktreePath != "" || branch != "" {
		t.Errorf("Prepare returned non-empty paths on failure: worktreePath=%q branch=%q", worktreePath, branch)
	}
}

// TestPrepareRejectsConcurrentCallForSameWorktreePath is the regression test
// for a real GitHub Codex App review finding, 2026-08-29: two concurrent
// Prepare calls for the same repoDir/parentDir/runID both observe
// worktreeExistedBefore/branchExistedBefore == false before either has
// created anything, so whichever call's own `worktree add` loses the race
// saw a failure and force-removed the *winner's* now-active worktree --
// indistinguishable, under an identical runID, from its own leftover from a
// failing post-checkout hook (same branch name either way). That ambiguity
// can only be prevented, not resolved after the fact, so Prepare now claims
// an os.O_EXCL lock file for worktreePath before touching anything else;
// this test pre-holds that lock (deterministic instead of relying on a real
// goroutine race window) and proves the blocked call fails cleanly with no
// worktree or branch mutation of its own, then that releasing the lock lets
// a normal Prepare call for the same runID succeed afterward.
func TestPrepareRejectsConcurrentCallForSameWorktreePath(t *testing.T) {
	repoDir := newFixtureRepo(t)
	headSHA, err := exec.Command("git", "-C", repoDir, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("get HEAD SHA: %v", err)
	}
	baseSHA := strings.TrimSpace(string(headSHA))

	runID := "concurrent-run"
	parentDir := t.TempDir()

	lockPath := filepath.Join(parentDir, runID) + ".prepare.lock"
	lockFile, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("pre-create lock file: %v", err)
	}
	lockFile.Close()

	if _, _, err := Prepare(repoDir, parentDir, runID, baseSHA); err == nil {
		t.Fatal("Prepare should fail while another call's lock file is held, but succeeded")
	} else if !strings.Contains(err.Error(), "already in progress") {
		t.Errorf("Prepare error = %q, want it to mention a concurrent call in progress", err)
	}

	listOut, err := exec.Command("git", "-C", repoDir, "worktree", "list").Output()
	if err != nil {
		t.Fatalf("git worktree list: %v", err)
	}
	if strings.Contains(string(listOut), runID) {
		t.Errorf("blocked Prepare call registered a worktree anyway:\n%s", listOut)
	}
	branchList, err := exec.Command("git", "-C", repoDir, "branch", "--list", "factoryd/"+runID).Output()
	if err != nil {
		t.Fatalf("git branch --list: %v", err)
	}
	if len(branchList) != 0 {
		t.Errorf("blocked Prepare call created branch %q anyway", "factoryd/"+runID)
	}

	if err := os.Remove(lockPath); err != nil {
		t.Fatalf("remove test lock file: %v", err)
	}
	if _, _, err := Prepare(repoDir, parentDir, runID, baseSHA); err != nil {
		t.Fatalf("Prepare after lock release: %v", err)
	}
}

// TestPrepareRemovesOwnLockFileOnSuccess proves a successful Prepare call
// does not leak its own lock file -- a caller retrying the exact same runID
// later (e.g. after Remove) must not be permanently blocked by a stale lock
// from a Prepare call that already completed cleanly.
func TestPrepareRemovesOwnLockFileOnSuccess(t *testing.T) {
	repoDir := newFixtureRepo(t)
	headSHA, err := exec.Command("git", "-C", repoDir, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("get HEAD SHA: %v", err)
	}
	baseSHA := strings.TrimSpace(string(headSHA))

	runID := "lock-cleanup-run"
	parentDir := t.TempDir()
	worktreePath, _, err := Prepare(repoDir, parentDir, runID, baseSHA)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	lockPath := worktreePath + ".prepare.lock"
	if _, statErr := os.Stat(lockPath); !os.IsNotExist(statErr) {
		t.Errorf("lock file %q still exists after a successful Prepare, want it removed: err=%v", lockPath, statErr)
	}
}

// TestPrepareOnBranchChecksOutExistingBranch proves PrepareOnBranch checks
// out an already-existing branch into a fresh worktree, rather than
// creating a new one -- the PR-review poll's own corrective-review-round mechanism.
func TestPrepareOnBranchChecksOutExistingBranch(t *testing.T) {
	repoDir := newFixtureRepo(t)
	if out, err := exec.Command("git", "-C", repoDir, "branch", "existing-pr-branch").CombinedOutput(); err != nil {
		t.Fatalf("create existing-pr-branch: %v: %s", err, out)
	}
	headSHA, err := exec.Command("git", "-C", repoDir, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("get HEAD SHA: %v", err)
	}
	baseSHA := strings.TrimSpace(string(headSHA))

	parentDir := t.TempDir()
	worktreePath, err := PrepareOnBranch(repoDir, parentDir, "round-run-1", "existing-pr-branch")
	if err != nil {
		t.Fatalf("PrepareOnBranch: %v", err)
	}
	if info, err := os.Stat(worktreePath); err != nil || !info.IsDir() {
		t.Fatalf("worktree path does not exist: %v", err)
	}
	out, err := exec.Command("git", "-C", worktreePath, "rev-parse", "--abbrev-ref", "HEAD").Output()
	if err != nil {
		t.Fatalf("get current branch: %v", err)
	}
	if got := strings.TrimSpace(string(out)); got != "existing-pr-branch" {
		t.Fatalf("checked out branch = %q, want %q", got, "existing-pr-branch")
	}
	worktreeHEAD, err := exec.Command("git", "-C", worktreePath, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("get worktree HEAD: %v", err)
	}
	if got := strings.TrimSpace(string(worktreeHEAD)); got != baseSHA {
		t.Fatalf("worktree HEAD = %q, want %q", got, baseSHA)
	}
}

// TestPrepareOnBranchSucceedsWhenBranchIsAlreadyCheckedOutElsewhere pins
// the live bug this guards against: an accepted run's preserved evidence
// worktree keeps the ticket's branch checked out indefinitely (it is
// never built in again, but is never removed either -- see
// PrepareOnBranch's own doc comment), so a later corrective round's own
// `git worktree add` for that same branch used to fail outright with
// "is already checked out" before a single command of that round ever
// ran. PrepareOnBranch must still succeed, and a commit made in the new
// worktree must actually advance the branch both worktrees share.
func TestPrepareOnBranchSucceedsWhenBranchIsAlreadyCheckedOutElsewhere(t *testing.T) {
	repoDir := newFixtureRepo(t)
	if out, err := exec.Command("git", "-C", repoDir, "branch", "existing-pr-branch").CombinedOutput(); err != nil {
		t.Fatalf("create existing-pr-branch: %v: %s", err, out)
	}
	// The stand-in for a preserved evidence worktree: an ordinary
	// `git worktree add` (no --force needed here -- nothing else has
	// this branch checked out yet) that is never cleaned up afterward.
	evidenceParentDir := t.TempDir()
	evidenceWorktreePath := filepath.Join(evidenceParentDir, "evidence-run")
	if out, err := exec.Command("git", "-C", repoDir, "worktree", "add", evidenceWorktreePath, "existing-pr-branch").CombinedOutput(); err != nil {
		t.Fatalf("create evidence worktree: %v: %s", err, out)
	}

	parentDir := t.TempDir()
	worktreePath, err := PrepareOnBranch(repoDir, parentDir, "round-run-elsewhere", "existing-pr-branch")
	if err != nil {
		t.Fatalf("PrepareOnBranch with the branch already checked out elsewhere: %v", err)
	}

	if err := os.WriteFile(filepath.Join(worktreePath, "round.txt"), []byte("round commit\n"), 0o600); err != nil {
		t.Fatalf("write round file: %v", err)
	}
	if out, err := exec.Command("git", "-C", worktreePath, "add", "-A").CombinedOutput(); err != nil {
		t.Fatalf("git add: %v: %s", err, out)
	}
	if out, err := exec.Command("git", "-C", worktreePath, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "-m", "round commit").CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v: %s", err, out)
	}

	branchHEAD, err := exec.Command("git", "-C", repoDir, "rev-parse", "existing-pr-branch").Output()
	if err != nil {
		t.Fatalf("rev-parse existing-pr-branch: %v", err)
	}
	worktreeHEAD, err := exec.Command("git", "-C", worktreePath, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("rev-parse worktree HEAD: %v", err)
	}
	if strings.TrimSpace(string(branchHEAD)) != strings.TrimSpace(string(worktreeHEAD)) {
		t.Fatalf("existing-pr-branch = %q, want it to match the round worktree's own HEAD %q", branchHEAD, worktreeHEAD)
	}
}

// TestPrepareOnBranchRejectsMissingBranch proves PrepareOnBranch never
// creates a branch of its own -- an existing PR branch is a precondition,
// not something this mechanism may substitute a fresh one for.
func TestPrepareOnBranchRejectsMissingBranch(t *testing.T) {
	repoDir := newFixtureRepo(t)
	parentDir := t.TempDir()
	if _, err := PrepareOnBranch(repoDir, parentDir, "round-run-2", "no-such-branch"); err == nil {
		t.Fatal("PrepareOnBranch(missing branch) = nil error, want a failure naming the missing branch")
	}
	branchList, err := exec.Command("git", "-C", repoDir, "branch", "--list", "no-such-branch").Output()
	if err != nil {
		t.Fatalf("git branch --list: %v", err)
	}
	if len(branchList) != 0 {
		t.Error("PrepareOnBranch created the branch it was supposed to require pre-existing")
	}
}

// TestPrepareOnBranchNeverDeletesBranchOnFailure mirrors
// TestPrepareDoesNotDeletePreexistingBranchOnFailure for PrepareOnBranch:
// a failed worktree add (destination already occupied by an unrelated
// worktree) must never delete the existing PR branch it was given.
func TestPrepareOnBranchNeverDeletesBranchOnFailure(t *testing.T) {
	repoDir := newFixtureRepo(t)
	if out, err := exec.Command("git", "-C", repoDir, "branch", "existing-pr-branch").CombinedOutput(); err != nil {
		t.Fatalf("create existing-pr-branch: %v: %s", err, out)
	}
	parentDir := t.TempDir()
	runID := "round-run-3"
	occupiedPath := filepath.Join(parentDir, runID)
	if err := os.MkdirAll(occupiedPath, 0o755); err != nil {
		t.Fatalf("mkdir occupied path: %v", err)
	}
	if err := os.WriteFile(filepath.Join(occupiedPath, "unrelated.txt"), []byte("keep me"), 0o600); err != nil {
		t.Fatalf("write unrelated file: %v", err)
	}

	if _, err := PrepareOnBranch(repoDir, parentDir, runID, "existing-pr-branch"); err == nil {
		t.Fatal("PrepareOnBranch(occupied destination) = nil error, want a failure")
	}

	branchList, err := exec.Command("git", "-C", repoDir, "branch", "--list", "existing-pr-branch").Output()
	if err != nil {
		t.Fatalf("git branch --list: %v", err)
	}
	if len(branchList) == 0 {
		t.Error("PrepareOnBranch deleted the pre-existing branch on a failed worktree add")
	}
	if _, err := os.Stat(filepath.Join(occupiedPath, "unrelated.txt")); err != nil {
		t.Errorf("unrelated pre-existing file was removed: %v", err)
	}
}

// TestRemoveWorktreeOnlyLeavesBranchIntact proves RemoveWorktreeOnly
// discards only the worktree, never the branch -- the cleanup
// PrepareOnBranch's own worktree gets once a caller is done with it.
func TestRemoveWorktreeOnlyLeavesBranchIntact(t *testing.T) {
	repoDir := newFixtureRepo(t)
	if out, err := exec.Command("git", "-C", repoDir, "branch", "existing-pr-branch").CombinedOutput(); err != nil {
		t.Fatalf("create existing-pr-branch: %v: %s", err, out)
	}
	parentDir := t.TempDir()
	worktreePath, err := PrepareOnBranch(repoDir, parentDir, "round-run-4", "existing-pr-branch")
	if err != nil {
		t.Fatalf("PrepareOnBranch: %v", err)
	}

	if err := RemoveWorktreeOnly(repoDir, worktreePath); err != nil {
		t.Fatalf("RemoveWorktreeOnly: %v", err)
	}
	if _, err := os.Stat(worktreePath); !os.IsNotExist(err) {
		t.Errorf("worktree path still exists after RemoveWorktreeOnly: err=%v", err)
	}
	branchList, err := exec.Command("git", "-C", repoDir, "branch", "--list", "existing-pr-branch").Output()
	if err != nil {
		t.Fatalf("git branch --list: %v", err)
	}
	if len(branchList) == 0 {
		t.Error("RemoveWorktreeOnly deleted the branch, want it left intact")
	}
}
