package workflow

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.temporal.io/sdk/testsuite"

	"buildgate/internal/reviewstep"
	"buildgate/internal/runner"
	"buildgate/internal/sandbox"
	"buildgate/internal/sandbox/sandboxtest"
)

const roundNotesMarker = "MARKER-the-build-agents-last-message-of-a-failed-round"

// leaveRoundNotes writes what build_app.py leaves at the worktree root when
// a round failed: the evidence file and the round-state file, each holding
// the agent's last message of that round.
func leaveRoundNotes(t *testing.T, worktree string) {
	t.Helper()
	writeFile(t, filepath.Join(worktree, buildSessionDir, "session.jsonl"), "the first prompt\n")
	writeFile(t, filepath.Join(worktree, "BUILD_EVIDENCE.json"),
		`{"schema_version":2,"succeeded":true,"rounds":[{"index":1,"agent":"pi","agent_notes":"Your final message was:\n\n`+roundNotesMarker+`"},{"index":2,"agent":"pi"}]}`+"\n")
	writeFile(t, filepath.Join(worktree, RoundStateFileName),
		`{"version":1,"last_completed_round":2,"next_prompt":"Round 1 failed. `+roundNotesMarker+`","rounds":[{"index":1,"agent_notes":"`+roundNotesMarker+`"},{"index":2}],"head":null}`+"\n")
}

// filesHolding lists the files under root, links not followed, whose content
// holds marker.
func filesHolding(t *testing.T, root, marker string) []string {
	t.Helper()
	var found []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || d.Type()&fs.ModeSymlink != 0 {
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

// The per-round notes in the build's evidence are the build agent's own
// last message. A review mounts the worktree and explores it with tools, so
// by the time one launches no file in the worktree holds that text; the
// host's copy of the evidence, which the run record is read from, does.
func TestNoReviewLaunchFindsTheBuildAgentsRoundNotesInTheWorktree(t *testing.T) {
	rt := &sandboxtest.WorkerRuntime{Lines: []string{"ok"}}
	activities, input, _ := runtimeActivities(t, rt)
	build := &Activities{
		LogDir: activities.LogDir,
		runWithRetries: func(_ context.Context, _ string, _ func(int) string, _ int, _ func(int, runner.Result, error), _ string, _ ...string) (runner.Result, error) {
			leaveRoundNotes(t, input.WorkspacePath)
			return runner.Result{}, nil
		},
	}
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestActivityEnvironment()
	env.RegisterActivity(build.RunBuildActivity)
	// Its own checkpoint folder: both Activities run under the test
	// environment's one Activity ID.
	buildInput := input
	buildInput.CheckpointDir = t.TempDir()
	if _, err := env.ExecuteActivity(build.RunBuildActivity, buildInput); err != nil {
		t.Fatalf("RunBuildActivity: %v", err)
	}

	routed := testRoutedActivities(sandbox.RouteSecret{})
	routed.LogDir, routed.DataDir, routed.Sandboxes = activities.LogDir, activities.DataDir, rt
	routed.MeterLedgerRoot, routed.SandboxDocker = activities.MeterLedgerRoot, activities.SandboxDocker
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
	env = suite.NewTestActivityEnvironment()
	env.RegisterActivity(routed.RunReviewStepActivity)
	_, err := env.ExecuteActivity(routed.RunReviewStepActivity, ReviewStepInput{RunWorkflowInput: input, Step: reviewstep.Combined})
	reqs := rt.Requests()
	if len(reqs) != 1 {
		t.Fatalf("the review did not launch (%v)", err)
	}
	// What the review's sandbox mounts: the worktree, and every other
	// directory or file the launch binds.
	mounted := []string{input.WorkspacePath}
	for _, m := range reqs[0].Mounts {
		mounted = append(mounted, m.Source)
	}
	for _, root := range mounted {
		for _, path := range filesHolding(t, root, roundNotesMarker) {
			t.Errorf("%s, which the review launch mounts, holds the build agent's round notes", path)
		}
	}
	kept, readErr := os.ReadFile(filepath.Join(activities.LogDir, "BUILD_EVIDENCE.json"))
	if readErr != nil || !strings.Contains(string(kept), roundNotesMarker) {
		t.Errorf("the host's copy of the build evidence = %.80q, %v, want the file the build wrote", kept, readErr)
	}
	if info, statErr := os.Stat(filepath.Join(activities.LogDir, "BUILD_EVIDENCE.json")); statErr == nil && info.Mode().Perm() != 0o600 {
		t.Errorf("the host's copy has mode %v, want 0600", info.Mode().Perm())
	}
}

// A build that was lost keeps its round state where a resume reads it, and
// no review follows a lost build.
func TestALostBuildKeepsItsRoundStateInTheWorktree(t *testing.T) {
	rt := &sandboxtest.WorkerRuntime{Lines: []string{"ok"}}
	activities, input, _ := runtimeActivities(t, rt)
	build := &Activities{
		LogDir: activities.LogDir,
		runWithRetries: func(_ context.Context, _ string, _ func(int) string, _ int, _ func(int, runner.Result, error), _ string, _ ...string) (runner.Result, error) {
			leaveRoundNotes(t, input.WorkspacePath)
			return runner.Result{}, errors.New("sandbox lost")
		},
	}
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestActivityEnvironment()
	env.RegisterActivity(build.RunBuildActivity)
	if _, err := env.ExecuteActivity(build.RunBuildActivity, input); err == nil {
		t.Fatal("a lost build returned no error")
	}
	if _, err := os.Stat(filepath.Join(input.WorkspacePath, RoundStateFileName)); err != nil {
		t.Errorf("a lost build's round state: %v, want it kept for a resume", err)
	}
}

// An evidence file the host cannot remove (here a directory with content in
// its place) fails the build step: no review is launched beside it.
func TestRunBuildActivityFailsWhenTheEvidenceFileCannotLeaveTheWorktree(t *testing.T) {
	rt := &sandboxtest.WorkerRuntime{Lines: []string{"ok"}}
	activities, input, _ := runtimeActivities(t, rt)
	build := &Activities{
		LogDir: activities.LogDir,
		runWithRetries: func(_ context.Context, _ string, _ func(int) string, _ int, _ func(int, runner.Result, error), _ string, _ ...string) (runner.Result, error) {
			writeFile(t, filepath.Join(input.WorkspacePath, "BUILD_EVIDENCE.json", "inner.json"), roundNotesMarker)
			return runner.Result{}, nil
		},
	}
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestActivityEnvironment()
	env.RegisterActivity(build.RunBuildActivity)
	_, err := env.ExecuteActivity(build.RunBuildActivity, input)
	if err == nil || !strings.Contains(err.Error(), "BUILD_EVIDENCE.json from the worktree") {
		t.Errorf("RunBuildActivity err = %v, want the step to fail on the evidence file it could not remove", err)
	}
	if _, statErr := os.Stat(filepath.Join(activities.LogDir, "BUILD_EVIDENCE.json")); !os.IsNotExist(statErr) {
		t.Errorf("a directory in the evidence file's place was kept (%v)", statErr)
	}
}
