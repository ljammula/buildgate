package workflow

import (
	"buildgate/internal/testfixture"
	wsisolation "buildgate/internal/workspace"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"go.temporal.io/sdk/testsuite"
)

// TestRollbackIsolatedWorkspaceActivityRetainsAgentReport is the
// regression test for a real GitHub Codex App review finding on this PR:
// a run that halts partway through a Temporal-routed slice, after
// build_app.py wrote BUILD_REPORT.md but before applyRunWorkflowResult's
// own loadAgentEvidence call is ever reached, previously lost that report
// outright -- RunWorkflow's own deferred rollback deleted the one
// worktree it existed in with nothing having retained it first.
// RollbackIsolatedWorkspaceActivity must retain it into LogDir before
// removing the worktree.
func TestRollbackIsolatedWorkspaceActivityRetainsAgentReport(t *testing.T) {
	repoDir := testfixture.NewGitRepo(t)
	baseSHABytes, err := exec.Command("git", "-C", repoDir, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("git rev-parse HEAD: %v", err)
	}
	baseSHA := strings.TrimSpace(string(baseSHABytes))
	parentDir := t.TempDir()
	worktreePath, branch, err := wsisolation.Prepare(repoDir, parentDir, "fixture-run", baseSHA)
	if err != nil {
		t.Fatalf("prepare isolated workspace: %v", err)
	}
	const report = "# Build report\n\nDid the thing before halting.\n"
	if err := os.WriteFile(filepath.Join(worktreePath, "BUILD_REPORT.md"), []byte(report), 0o644); err != nil {
		t.Fatalf("write fixture BUILD_REPORT.md: %v", err)
	}

	logDir := t.TempDir()
	activities := &Activities{}
	input := RollbackIsolatedWorkspaceInput{RepoDir: repoDir, WorktreePath: worktreePath, Branch: branch, LogDir: logDir}

	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestActivityEnvironment()
	env.RegisterActivity(activities.RollbackIsolatedWorkspaceActivity)
	if _, err := env.ExecuteActivity(activities.RollbackIsolatedWorkspaceActivity, input); err != nil {
		t.Fatalf("RollbackIsolatedWorkspaceActivity: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(logDir, "BUILD_REPORT.md"))
	if err != nil {
		t.Fatalf("read retained BUILD_REPORT.md from LogDir: %v", err)
	}
	if string(got) != report {
		t.Errorf("retained BUILD_REPORT.md = %q, want %q", got, report)
	}
	if _, err := os.Stat(worktreePath); !os.IsNotExist(err) {
		t.Errorf("Stat(worktreePath) error = %v, want IsNotExist -- the rollback itself must still have removed the worktree", err)
	}
}

// TestRollbackIsolatedWorkspaceActivityToleratesMissingAgentReport covers
// a halt before build_app.py ever wrote a report at all: absence must
// not be an error, and must not prevent the rollback itself from
// proceeding.
func TestRollbackIsolatedWorkspaceActivityToleratesMissingAgentReport(t *testing.T) {
	repoDir := testfixture.NewGitRepo(t)
	baseSHABytes, err := exec.Command("git", "-C", repoDir, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("git rev-parse HEAD: %v", err)
	}
	baseSHA := strings.TrimSpace(string(baseSHABytes))
	parentDir := t.TempDir()
	worktreePath, branch, err := wsisolation.Prepare(repoDir, parentDir, "fixture-run", baseSHA)
	if err != nil {
		t.Fatalf("prepare isolated workspace: %v", err)
	}

	logDir := t.TempDir()
	activities := &Activities{}
	input := RollbackIsolatedWorkspaceInput{RepoDir: repoDir, WorktreePath: worktreePath, Branch: branch, LogDir: logDir}

	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestActivityEnvironment()
	env.RegisterActivity(activities.RollbackIsolatedWorkspaceActivity)
	if _, err := env.ExecuteActivity(activities.RollbackIsolatedWorkspaceActivity, input); err != nil {
		t.Fatalf("RollbackIsolatedWorkspaceActivity: %v", err)
	}

	if _, err := os.Stat(filepath.Join(logDir, "BUILD_REPORT.md")); !os.IsNotExist(err) {
		t.Errorf("Stat(retained BUILD_REPORT.md) error = %v, want IsNotExist", err)
	}
	if _, err := os.Stat(worktreePath); !os.IsNotExist(err) {
		t.Errorf("Stat(worktreePath) error = %v, want IsNotExist -- the rollback itself must still have removed the worktree", err)
	}
}
