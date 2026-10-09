package workflow

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.temporal.io/sdk/testsuite"

	"buildgate/internal/evidence"
	"buildgate/internal/reviewstep"
	"buildgate/internal/runner"
	"buildgate/internal/sandbox"
	"buildgate/internal/sandbox/sandboxtest"
	"buildgate/internal/testfixture"
)

const promptMarker = "PROMPT-MARKER-the-earlier-attempt-record"

// holdingMarker lists the regular files under root whose text has marker.
func holdingMarker(t *testing.T, root, marker string) []string {
	t.Helper()
	var found []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !d.Type().IsRegular() {
			return nil
		}
		if data, readErr := os.ReadFile(path); readErr == nil && strings.Contains(string(data), marker) {
			found = append(found, path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return found
}

func TestRunBuildActivityCopiesTheSavedPromptsOutBeforeAnyLaterStep(t *testing.T) {
	repo, logDir := runBuildLeavingNotes(t, func(session string) {
		writeFile(t, filepath.Join(session, "prompts", "build-round-1.md"), "Fix it.\n"+promptMarker+"\n")
		writeFile(t, filepath.Join(session, "prompts", "build-notes.md"), "Write your notes.\n")
		// The conformity review a build may run in its own process.
		writeFile(t, filepath.Join(repo0(session), conformitySessionDir, "prompts", "review-conformity.md"), "Review "+promptMarker+"\n")
	})
	dst := filepath.Join(logDir, evidence.PromptsDirName, "build-1")
	for _, name := range []string{"build-round-1.md", "build-notes.md", "review-conformity.md"} {
		if _, err := os.Stat(filepath.Join(dst, name)); err != nil {
			t.Errorf("%s was not kept for the operator: %v", name, err)
		}
	}
	if info, _ := os.Stat(filepath.Join(dst, "build-round-1.md")); info == nil || info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600", info)
	}
	// What the review launches share: the worktree. Nothing in it keeps the marker.
	if found := holdingMarker(t, repo, promptMarker); len(found) != 0 {
		t.Errorf("the worktree still holds the saved prompt: %v", found)
	}
	if got := evidence.ListSavedPrompts(logDir); len(got) != 3 {
		t.Errorf("ListSavedPrompts = %+v, want the three prompts", got)
	}
}

// repo0 is the worktree a session folder sits in.
func repo0(session string) string { return filepath.Dir(session) }

func TestRunBuildActivityKeepsNoPromptThroughALinkedSessionFolder(t *testing.T) {
	repo := testfixture.NewGitRepo(t)
	logDir := t.TempDir()
	writeFile(t, filepath.Join(repo, "docs", "prompts", "build-round-1.md"), promptMarker)
	activities := &Activities{
		LogDir: logDir,
		runWithRetries: func(_ context.Context, _ string, _ func(int) string, _ int, _ func(int, runner.Result, error), _ string, _ ...string) (runner.Result, error) {
			return runner.Result{ExitCode: 1}, os.Symlink("docs", filepath.Join(repo, buildSessionDir))
		},
	}
	input := fixtureInput()
	input.WorkspacePath = repo
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestActivityEnvironment()
	env.RegisterActivity(activities.RunBuildActivity)
	_, _ = env.ExecuteActivity(activities.RunBuildActivity, input)
	if found := holdingMarker(t, logDir, promptMarker); len(found) != 0 {
		t.Errorf("a prompt was kept through a linked session folder: %v", found)
	}
}

func reviewFixtureWithRunner(t *testing.T, plant func(repo string)) (repo, logDir string, run func() error) {
	t.Helper()
	repo = testfixture.NewGitRepo(t)
	activities := testRoutedActivities(sandbox.NewRouteSecret("worker-static-upstream-key"))
	input := combinedReviewFixtureInput(t, repo)
	logDir = input.LogDir
	activities.LogDir = logDir
	activities.runWithRetriesChecked = func(_ context.Context, _ string, logPath func(int) string, _ int, before func(int) error, after func(int, runner.Result, error) error, _ string, _ ...string) (runner.Result, error) {
		if err := before(1); err != nil {
			return runner.Result{}, err
		}
		log := logPath(1)
		if err := os.MkdirAll(filepath.Dir(log), 0o750); err != nil {
			return runner.Result{}, err
		}
		if err := os.WriteFile(log, []byte("clean\n"), 0o600); err != nil {
			return runner.Result{}, err
		}
		plant(repo)
		result := runner.Result{Command: []string{"python3", "combined_review.py"}, ExitCode: reviewstep.CombinedExitBase + 3, LogPath: log}
		return result, after(1, result, nil)
	}
	return repo, logDir, func() error {
		wrapper := func(ctx context.Context, in ReviewStepInput) (VerifyActivityResult, error) {
			return activities.RunReviewStepActivity(ctx, in)
		}
		var suite testsuite.WorkflowTestSuite
		env := suite.NewTestActivityEnvironment()
		env.RegisterActivity(wrapper)
		_, err := env.ExecuteActivity(wrapper, input)
		return err
	}
}

func TestAReviewKeepsTheLaunchesPromptAndNothingElseInItsSessionFolder(t *testing.T) {
	var repoDir string
	repo, logDir, runStep := reviewFixtureWithRunner(t, func(repo string) {
		repoDir = repo
		writeFile(t, filepath.Join(repo, ".pi-combined-review-session", "prompts", "review-combined.md"), "Review the diff.\n")
	})
	// A file the build agent left there before the review launched is not a
	// prompt the review sent.
	writeFile(t, filepath.Join(repo, ".pi-combined-review-session", "prompts", "forged.md"), promptMarker)
	// As the run's own worktree does: the harness folders are not part of the tree.
	writeFile(t, filepath.Join(repo, ".git", "info", "exclude"), ".pi-combined-review-session/\n")
	if err := runStep(); err != nil {
		t.Fatal(err)
	}
	got := evidence.ListSavedPrompts(logDir)
	if len(got) != 1 || got[0].Attempt != "review-1" || got[0].Name != "review-combined" {
		t.Fatalf("saved prompts = %+v, want the one the launch wrote", got)
	}
	if found := holdingMarker(t, logDir, promptMarker); len(found) != 0 {
		t.Errorf("a forged prompt was kept: %v", found)
	}
	if _, err := os.Stat(filepath.Join(repoDir, ".pi-combined-review-session", "prompts")); !os.IsNotExist(err) {
		t.Errorf("the review's prompts folder remains in the worktree (%v)", err)
	}
}

// A review sandbox mounts the worktree and a few staged inputs; the run's own
// directory, where the build's saved prompts are kept, is none of them.
func TestAReviewLaunchCannotSeeTheRunsSavedPrompts(t *testing.T) {
	rt := &sandboxtest.WorkerRuntime{Lines: []string{"ok"}}
	activities, input, _ := runtimeActivities(t, rt)
	routed := testRoutedActivities(sandbox.RouteSecret{})
	routed.LogDir, routed.DataDir, routed.Sandboxes = activities.LogDir, activities.DataDir, rt
	routed.MeterLedgerRoot, routed.SandboxDocker = activities.MeterLedgerRoot, activities.SandboxDocker
	input.SpecPath = ""
	input.BuildAppScript = realScripts(t)
	input.BuildAppInterpreter = "python3"
	input.RoutePolicy = testRelayPolicy()
	input.RoutePolicy.AllowUnauthenticatedUpstream = true
	input.RoutePolicy.Upstream = "http://127.0.0.1:8080"
	input.RoutePolicy.AllowPlaintextUpstream = true
	input.RoutePolicy.AllowedPathPrefix = "/v1/chat/completions"
	input.RoutePolicy.UsageFormat = "openai"
	input.RoutePolicy.WorkerModelID = "qwen"
	input.RoutePolicy.WorkerBasePath = "/v1"
	// The build's prompt is where the host copy puts it.
	saved := filepath.Join(routed.LogDir, evidence.PromptsDirName, "build-1", "build-round-1.md")
	writeFile(t, saved, promptMarker)
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestActivityEnvironment()
	env.RegisterActivity(routed.RunReviewStepActivity)
	_, err := env.ExecuteActivity(routed.RunReviewStepActivity, ReviewStepInput{RunWorkflowInput: input, Step: reviewstep.Combined})
	if len(rt.Requests()) != 1 {
		t.Fatalf("the review did not launch (%v)", err)
	}
	if rel, err := filepath.Rel(input.WorkspacePath, routed.LogDir); err == nil && !strings.HasPrefix(rel, "..") {
		t.Fatalf("the run directory %s is inside the workspace %s: the mounts would carry it", routed.LogDir, input.WorkspacePath)
	}
	req := rt.Requests()[0]
	paths := append([]string{}, req.ReadOnlyPaths...)
	paths = append(paths, req.ReadWritePaths...)
	for _, m := range req.Mounts {
		paths = append(paths, m.Source)
	}
	for _, p := range paths {
		if p == "" {
			continue
		}
		abs, err := filepath.Abs(p)
		if err != nil {
			continue
		}
		// Neither the prompts folder, nor something inside it, nor a folder
		// that holds it (the run directory, the data directory).
		promptsDir := filepath.Join(routed.LogDir, evidence.PromptsDirName)
		inside, _ := filepath.Rel(promptsDir, abs)
		holds, _ := filepath.Rel(abs, promptsDir)
		if !strings.HasPrefix(inside, "..") || !strings.HasPrefix(holds, "..") {
			t.Errorf("the review launch mounts %s, which is or holds the saved prompts", p)
		}
		if found := holdingMarker(t, abs, promptMarker); len(found) != 0 {
			t.Errorf("the review launch mounts %s, which holds the build's saved prompt: %v", p, found)
		}
	}
}
