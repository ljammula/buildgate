package main

import (
	"bytes"
	"log"
	"os"
	"os/exec"
	"strings"
	"testing"

	"buildgate/internal/run"
	wsisolation "buildgate/internal/workspace"
)

// TestRollbackIsolatedWorkspaceOnSaveFailureDiscardsWorktreeAndBranch is
// the first real test for rollbackIsolatedWorkspaceOnSaveFailure (see
// apply_run_result.go's own two call sites) — before this, the function
// had no dedicated test at all, unlike its sibling
// rollbackIsolatedWorkspaceIfTerminated (covered end to end by
// TestIntegrationIsolateWorkspaceRollsBackOnHardTerminationViaTemporal,
// which drives it via a real compiled `factoryd` subprocess — real
// coverage, just invisible to `go test -cover` since that subprocess is
// never built with coverage instrumentation, and the reason both showed
// as 0% in a statement-coverage report despite one already being
// integration-tested).
//
// This one is a pure function over a *run.Run plus a real isolated git
// worktree (internal/workspace.Prepare, the same fixture
// TestRollbackDiscardsWorktreeAndBranch in internal/release uses for
// release.Rollback itself) — no Temporal server needed, since the
// save-failure path this mirrors is a plain run.Save error, not anything
// Temporal-specific.
func TestRollbackIsolatedWorkspaceOnSaveFailureDiscardsWorktreeAndBranch(t *testing.T) {
	repoDir := newFixtureRepo(t)
	headSHA, err := exec.Command("git", "-C", repoDir, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("get HEAD SHA: %v", err)
	}
	base := strings.TrimSpace(string(headSHA))

	parentDir := t.TempDir()
	worktreePath, branch, err := wsisolation.Prepare(repoDir, parentDir, "test-save-failure-rollback", base)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if _, err := os.Stat(worktreePath); err != nil {
		t.Fatalf("worktree should exist before rollback: %v", err)
	}

	r := &run.Run{
		ID:            "test-save-failure-rollback",
		ProjectPath:   repoDir,
		WorkspacePath: worktreePath,
		Branch:        branch,
	}
	rollbackIsolatedWorkspaceOnSaveFailure(r)

	if _, err := os.Stat(worktreePath); err == nil {
		t.Error("worktree should not exist after rollbackIsolatedWorkspaceOnSaveFailure")
	} else if !os.IsNotExist(err) {
		t.Errorf("unexpected error checking worktree: %v", err)
	}
	branchList, err := exec.Command("git", "-C", repoDir, "branch", "--list", branch).Output()
	if err != nil {
		t.Fatalf("git branch --list: %v", err)
	}
	if len(strings.TrimSpace(string(branchList))) != 0 {
		t.Errorf("branch %q should not exist after rollbackIsolatedWorkspaceOnSaveFailure: %s", branch, branchList)
	}
}

// TestRollbackIsolatedWorkspaceOnSaveFailureSkipsNonIsolatedRun covers the
// function's own early-return guard: a run whose WorkspacePath is empty,
// or equals ProjectPath, was never isolated in the first place (see
// run.Run.Branch's own doc comment — WorkspacePath equals ProjectPath for
// a non-isolated run), so calling release.Rollback against it would
// either no-op on an empty path or, worse, attempt to discard the shared
// checkout itself.
//
// Neither of those release.Rollback outcomes is actually observable via
// repo state, though (found via Codex review of this PR): workspace.Remove
// treats an empty worktreePath the same as "already gone" and merely
// tries (and fails, harmlessly logged) to delete a branch named "", and
// git itself refuses to `worktree remove` the primary worktree
// (WorkspacePath == ProjectPath) without ever touching it -- so a
// deleted guard would still leave the fixture repo and its worktree list
// completely unchanged, and the original version of this test would
// still pass even with the guard removed entirely. Capturing log output
// instead makes the guard's own effect observable: reaching
// release.Rollback for either input logs "rollback of isolated
// workspace after a terminal-state save failure also failed" (both
// inputs make the underlying git commands fail, per the analysis above);
// the guard firing means that call, and therefore that log line, never
// happens.
// not parallel-safe: redirects the global log package's output (log.SetOutput)
func TestRollbackIsolatedWorkspaceOnSaveFailureSkipsNonIsolatedRun(t *testing.T) {
	repoDir := newFixtureRepo(t)

	var logBuf bytes.Buffer
	previousOutput := log.Writer()
	log.SetOutput(&logBuf)
	t.Cleanup(func() { log.SetOutput(previousOutput) })

	rollbackIsolatedWorkspaceOnSaveFailure(&run.Run{ID: "empty-workspace-path", ProjectPath: repoDir, WorkspacePath: ""})
	rollbackIsolatedWorkspaceOnSaveFailure(&run.Run{ID: "workspace-equals-project", ProjectPath: repoDir, WorkspacePath: repoDir, Branch: "should-never-be-touched"})

	if logs := logBuf.String(); logs != "" {
		t.Errorf("logs = %q, want no rollback-failure log line -- the guard should have skipped release.Rollback entirely for both inputs", logs)
	}
	if _, err := os.Stat(repoDir); err != nil {
		t.Fatalf("fixture repo should still exist: %v", err)
	}
	out, err := exec.Command("git", "-C", repoDir, "worktree", "list").Output()
	if err != nil {
		t.Fatalf("git worktree list: %v", err)
	}
	// Exactly one line: the repo's own primary worktree, nothing removed
	// or added by either no-op call above.
	if lines := strings.Count(strings.TrimSpace(string(out)), "\n") + 1; lines != 1 {
		t.Errorf("git worktree list = %q, want exactly the primary worktree untouched", out)
	}
}
