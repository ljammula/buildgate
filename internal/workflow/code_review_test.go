package workflow

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.temporal.io/sdk/testsuite"

	"buildgate/internal/codereview"
	"buildgate/internal/reviewstep"
	"buildgate/internal/runner"
	"buildgate/internal/sandbox"
	"buildgate/internal/testfixture"
)

// codeReviewFixtureInput is fixtureInput() plus the relay policy and
// -code-review-policy RunCodeReviewActivity needs -- mirrors
// specConformityReviewFixtureInput (spec_conformity_review_test.go).
func codeReviewFixtureInput(t *testing.T, workspacePath string) ReviewStepInput {
	t.Helper()
	input := fixtureInput()
	input.WorkspacePath = workspacePath
	input.RunID = "code-review-run"
	input.CodeReviewPolicy = codereview.PolicyAdvisory
	dataDir := t.TempDir()
	input.DataDir = dataDir
	input.LogDir = filepath.Join(dataDir, "logs")
	input.CheckpointDir = filepath.Join(dataDir, "checkpoints")
	policy := *testRelayPolicy()
	policy.AllowUnauthenticatedUpstream = true
	input.RoutePolicy = &policy
	return ReviewStepInput{RunWorkflowInput: input, Step: reviewstep.CodeReview}
}

// TestRunCodeReviewActivityHaltsWhenNoRelayConfigured is
// TestRunSpecConformityReviewActivityHaltsWhenNoRelayConfigured's
// counterpart: an operator who explicitly set -code-review-policy to
// advisory/required is asking for a real review, not an optional one --
// silently skipping this Activity because no relay is configured would
// leave that declared policy never checked while the run could still be
// accepted. This must halt explicitly instead.
func TestRunCodeReviewActivityHaltsWhenNoRelayConfigured(t *testing.T) {
	workspacePath := testfixture.NewGitRepo(t)
	activities := &Activities{LogDir: t.TempDir()}
	input := codeReviewFixtureInput(t, workspacePath)
	input.RoutePolicy = nil // the case under test

	wrapper := func(ctx context.Context, in ReviewStepInput) (VerifyActivityResult, error) {
		return activities.RunReviewStepActivity(ctx, in)
	}
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestActivityEnvironment()
	env.RegisterActivity(wrapper)
	_, err := env.ExecuteActivity(wrapper, input)
	if err == nil {
		t.Fatal("RunCodeReviewActivity with no relay configured: want error, got nil")
	}
	if !hasApplicationErrorType(err, RelayConfigurationFailureType) {
		t.Fatalf("error does not carry application error type %q: %v", RelayConfigurationFailureType, err)
	}
}

// TestRunCodeReviewActivitySucceeds is the common case: the subprocess
// runs (through the runWithRetriesChecked fake seam, no real Docker
// needed) and leaves the workspace clean, so the Activity returns a
// populated VerifyActivityResult with no error.
func TestRunCodeReviewActivitySucceeds(t *testing.T) {
	workspacePath := testfixture.NewGitRepo(t)
	logDir := t.TempDir()
	activities := testRoutedActivities(sandbox.NewRouteSecret("worker-static-upstream-key"))
	activities.LogDir = logDir
	activities.runWithRetriesChecked = func(_ context.Context, _ string, logPath func(int) string, _ int, before func(int) error, after func(int, runner.Result, error) error, _ string, _ ...string) (runner.Result, error) {
		if err := before(1); err != nil {
			return runner.Result{}, err
		}
		started := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
		log := logPath(1)
		if err := os.MkdirAll(filepath.Dir(log), 0o750); err != nil {
			return runner.Result{}, err
		}
		if err := os.WriteFile(log, []byte("clean\n"), 0o600); err != nil {
			return runner.Result{}, err
		}
		result := runner.Result{Command: []string{"python3", "code_review.py"}, StartedAt: started, FinishedAt: started.Add(time.Second), ExitCode: 0, LogPath: log}
		if err := after(1, result, nil); err != nil {
			return result, err
		}
		return result, nil
	}
	input := codeReviewFixtureInput(t, workspacePath)

	wrapper := func(ctx context.Context, in ReviewStepInput) (VerifyActivityResult, error) {
		return activities.RunReviewStepActivity(ctx, in)
	}
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestActivityEnvironment()
	env.RegisterActivity(wrapper)
	raw, err := env.ExecuteActivity(wrapper, input)
	if err != nil {
		t.Fatalf("execute code-review Activity: %v", err)
	}
	var result VerifyActivityResult
	if err := raw.Get(&result); err != nil {
		t.Fatalf("decode code-review Activity result: %v", err)
	}
	if result.Result.ExitCode != 0 {
		t.Errorf("Result.ExitCode = %d, want 0", result.Result.ExitCode)
	}
	if result.LogSHA256 == "" {
		t.Error("LogSHA256 is empty, want a real hash of the code-review log")
	}
	if len(result.Attempts) != 1 || result.Attempts[0].Kind != "code_review" {
		t.Fatalf("Attempts = %+v, want exactly one code_review attempt", result.Attempts)
	}
}

// TestRunCodeReviewActivityHaltsWhenWorkspaceLeftDirty mirrors
// TestRunSpecConformityReviewActivityHaltsWhenWorkspaceLeftDirty:
// code_review.py's job is to read and report, never to edit -- if it
// somehow wrote something, silently folding that into a further
// safety-net commit would accept unreviewed, unknown output onto this
// run's own result_sha. This must halt instead.
func TestRunCodeReviewActivityHaltsWhenWorkspaceLeftDirty(t *testing.T) {
	workspacePath := testfixture.NewGitRepo(t)
	logDir := t.TempDir()
	activities := testRoutedActivities(sandbox.NewRouteSecret("worker-static-upstream-key"))
	activities.LogDir = logDir
	activities.runWithRetriesChecked = func(_ context.Context, dir string, logPath func(int) string, _ int, before func(int) error, after func(int, runner.Result, error) error, _ string, _ ...string) (runner.Result, error) {
		if err := before(1); err != nil {
			return runner.Result{}, err
		}
		if err := os.WriteFile(filepath.Join(dir, "stray-reviewer-output.txt"), []byte("should never be here\n"), 0o600); err != nil {
			return runner.Result{}, err
		}
		started := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
		log := logPath(1)
		if err := os.MkdirAll(filepath.Dir(log), 0o750); err != nil {
			return runner.Result{}, err
		}
		if err := os.WriteFile(log, []byte("dirty\n"), 0o600); err != nil {
			return runner.Result{}, err
		}
		result := runner.Result{Command: []string{"python3", "code_review.py"}, StartedAt: started, FinishedAt: started.Add(time.Second), ExitCode: 0, LogPath: log}
		if err := after(1, result, nil); err != nil {
			return result, err
		}
		return result, nil
	}
	input := codeReviewFixtureInput(t, workspacePath)

	wrapper := func(ctx context.Context, in ReviewStepInput) (VerifyActivityResult, error) {
		return activities.RunReviewStepActivity(ctx, in)
	}
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestActivityEnvironment()
	env.RegisterActivity(wrapper)
	_, err := env.ExecuteActivity(wrapper, input)
	if err == nil {
		t.Fatal("RunCodeReviewActivity with a dirty workspace afterward: want error, got nil")
	}
	if !hasApplicationErrorType(err, InfrastructureFailureType) {
		t.Fatalf("error does not carry application error type %q: %v", InfrastructureFailureType, err)
	}
	statusOut, statusErr := runner.GitIsClean(workspacePath)
	if statusErr != nil {
		t.Fatalf("GitIsClean: %v", statusErr)
	}
	if statusOut {
		t.Fatal("workspace reads as clean, want it to still be dirty (the stray file must not have been auto-committed)")
	}
}

// TestRunCodeReviewActivityStagesBuildAppSiblingAndArgv is
// TestRunSpecConformityReviewActivityStagesBuildAppSibling's counterpart,
// plus the argv assertion M2-C's own checklist asks for: code_review.py's
// argv must carry --review-policy and --thinking.
func TestRunCodeReviewActivityStagesBuildAppSiblingAndArgv(t *testing.T) {
	harnessDir := t.TempDir()
	for _, name := range []string{"build_app.py", codereview.ScriptName} {
		if err := os.WriteFile(filepath.Join(harnessDir, name), []byte("# "+name+"\n"), 0o644); err != nil {
			t.Fatalf("write fixture harness file %s: %v", name, err)
		}
	}
	markerPath := filepath.Join(t.TempDir(), "sibling-marker")
	argvPath := filepath.Join(t.TempDir(), "argv")
	dockerScript := `
args=""
prev=""
for arg in "$@"; do
	if [ "$prev" = "--volume" ]; then
		case "$arg" in
		*:/inputs/script:ro)
			src="${arg%:/inputs/script:ro}"
			if [ -f "$src/build_app.py" ]; then
				echo present > ` + markerPath + `
			else
				echo missing > ` + markerPath + `
			fi
			;;
		esac
	fi
	prev="$arg"
done
echo "$@" > ` + argvPath + `
exit 0
`
	docker := fakeDockerBinary(t, dockerScript)

	workspacePath := testfixture.NewGitRepo(t)
	specPath := filepath.Join(t.TempDir(), "spec.md")
	if err := os.WriteFile(specPath, []byte("# Spec\n"), 0o644); err != nil {
		t.Fatalf("write fixture spec file: %v", err)
	}
	activities := testRoutedActivities(sandbox.NewRouteSecret("worker-static-upstream-key"))
	input := codeReviewFixtureInput(t, workspacePath)
	input.SpecPath = specPath
	input.CodeReviewPolicy = codereview.PolicyRequired
	input.ReviewThinking = "high"
	input.SandboxDocker = docker
	input.SandboxImage = "worker@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	input.SandboxUser = fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid())
	input.BuildAppScript = filepath.Join(harnessDir, "build_app.py")
	input.BuildAppInterpreter = "python3"

	wrapper := func(ctx context.Context, in ReviewStepInput) (VerifyActivityResult, error) {
		return activities.RunReviewStepActivity(ctx, in)
	}
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestActivityEnvironment()
	env.RegisterActivity(wrapper)
	raw, err := env.ExecuteActivity(wrapper, input)
	if err != nil {
		t.Fatalf("RunCodeReviewActivity: %v", err)
	}
	var result VerifyActivityResult
	if err := raw.Get(&result); err != nil {
		t.Fatalf("decode code-review Activity result: %v", err)
	}
	if len(result.Attempts) != 1 {
		t.Fatalf("Attempts = %+v, want exactly one code_review attempt", result.Attempts)
	}

	marker, err := os.ReadFile(markerPath)
	if err != nil {
		t.Fatalf("read sibling-staging marker (fake docker never saw the /inputs/script mount?): %v", err)
	}
	if got := string(marker); got != "present\n" {
		t.Fatalf("build_app.py beside the staged code_review.py = %q, want \"present\\n\" -- code_review.py's own `import build_app` would die with ModuleNotFoundError inside the real sandbox", got)
	}

	argv, err := os.ReadFile(argvPath)
	if err != nil {
		t.Fatalf("read recorded argv: %v", err)
	}
	if !strings.Contains(string(argv), "--review-policy required") {
		t.Fatalf("argv = %q, want it to contain %q", string(argv), "--review-policy required")
	}
	if !strings.Contains(string(argv), "--thinking high") {
		t.Fatalf("argv = %q, want it to contain %q", string(argv), "--thinking high")
	}
}
