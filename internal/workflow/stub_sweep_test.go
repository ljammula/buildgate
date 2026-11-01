package workflow

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"go.temporal.io/sdk/testsuite"

	"buildgate/internal/runner"
	"buildgate/internal/sandbox"
	"buildgate/internal/testfixture"
)

// leftoverStub is a repository whose worktree holds an empty mount stub a
// dead worker left, with its manifest under runDataDir, a data directory other
// than the Activities' own (a daemon whose -data-dir differs from the run's),
// and one dirty file the safety-net commit will take.
func leftoverStub(t *testing.T) (workspace, base, runDataDir string) {
	t.Helper()
	workspace = testfixture.NewGitRepo(t)
	base, err := runner.GitRevParseHEAD(workspace)
	if err != nil {
		t.Fatal(err)
	}
	runDataDir = t.TempDir()
	masks := []sandbox.WorkspaceMask{{Target: "p/x_test.go", AbsentInWorktree: true}}
	if err := os.MkdirAll(filepath.Join(workspace, "p"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := createReviewStubs(workspace, reviewStubManifestPath(runDataDir, workspace), masks); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "content.txt"), []byte("dirty\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return workspace, base, runDataDir
}

func committedFiles(t *testing.T, workspace string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", workspace, "show", "--name-only", "--format=", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

// A stub left by a worker that died mid-round is removed before the safety-net
// commit of PostBuildActivity, using the run's data directory.
func TestPostBuildActivitySweepsAStubADeadWorkerLeft(t *testing.T) {
	workspace, base, runDataDir := leftoverStub(t)
	activities := &Activities{LogDir: t.TempDir(), DataDir: t.TempDir()}
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestActivityEnvironment()
	env.RegisterActivity(activities.PostBuildActivity)
	if _, err := env.ExecuteActivity(activities.PostBuildActivity, PostBuildInput{WorkspacePath: workspace, BaseSHA: base, DataDir: runDataDir}); err != nil {
		t.Fatalf("PostBuildActivity: %v", err)
	}
	files := committedFiles(t, workspace)
	if strings.Contains(files, "x_test.go") || !strings.Contains(files, "content.txt") {
		t.Errorf("committed %q, want content.txt and no stub", files)
	}
}

// The same, for the verification-output commit of CollectEvidenceActivity.
func TestCollectEvidenceActivitySweepsAStubADeadWorkerLeft(t *testing.T) {
	workspace, base, runDataDir := leftoverStub(t)
	result, err := callCollectEvidenceActivity(t, &Activities{LogDir: t.TempDir(), DataDir: t.TempDir()}, CollectEvidenceInput{WorkspacePath: workspace, BaseSHA: base, DataDir: runDataDir})
	if err != nil {
		t.Fatalf("CollectEvidenceActivity: %v", err)
	}
	if files := committedFiles(t, workspace); strings.Contains(files, "x_test.go") || !strings.Contains(files, "content.txt") {
		t.Errorf("committed %q, want content.txt and no stub", files)
	}
	for _, f := range result.ChangedFiles {
		if strings.Contains(f, "x_test.go") {
			t.Errorf("the stub is a changed file: %v", result.ChangedFiles)
		}
	}
}

// A failed build commits nothing, and the stub still must not show in the
// evidence as a changed file.
func TestCollectEvidenceActivityEvidenceOmitsAStubWhenTheBuildFailed(t *testing.T) {
	workspace, base, runDataDir := leftoverStub(t)
	result, err := callCollectEvidenceActivity(t, &Activities{LogDir: t.TempDir(), DataDir: t.TempDir()}, CollectEvidenceInput{WorkspacePath: workspace, BaseSHA: base, BuildExitCode: 1, DataDir: runDataDir})
	if err != nil {
		t.Fatalf("CollectEvidenceActivity: %v", err)
	}
	for _, f := range result.ChangedFiles {
		if strings.Contains(f, "x_test.go") {
			t.Errorf("the stub is a changed file: %v", result.ChangedFiles)
		}
	}
	if _, err := os.Stat(filepath.Join(workspace, "p", "x_test.go")); !os.IsNotExist(err) {
		t.Errorf("the stub was left: %v", err)
	}
}
