package workflow

import (
	"context"
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

// combinedReviewFixtureInput is fixtureInput() plus the relay policy and
// spec RunReviewStepActivity needs for the Combined step.
func combinedReviewFixtureInput(t *testing.T, workspacePath string) ReviewStepInput {
	t.Helper()
	input := fixtureInput()
	input.WorkspacePath = workspacePath
	input.RunID = "combined-review-run"
	input.SpecPath = "/fixture/spec.md"
	input.CodeReviewPolicy = codereview.PolicyRequired
	dataDir := t.TempDir()
	input.DataDir = dataDir
	input.LogDir = filepath.Join(dataDir, "logs")
	input.CheckpointDir = filepath.Join(dataDir, "checkpoints")
	policy := *testRelayPolicy()
	policy.AllowUnauthenticatedUpstream = true
	input.RoutePolicy = &policy
	return ReviewStepInput{RunWorkflowInput: input, Step: reviewstep.Combined}
}

// TestRunReviewStepActivityRunsTheCombinedStep proves RunReviewStepActivity
// launches Step == reviewstep.Combined and returns the launch's raw exit
// status: the workflow, not the Activity, decodes it.
func TestRunReviewStepActivityRunsTheCombinedStep(t *testing.T) {
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
		// CombinedExitBase+3: both bits set --
		// this test only checks the Activity resolves and returns this
		// raw exit code untouched; RunWorkflow (not this Activity) is
		// what decodes it via reviewstep.ReviewExitCode.
		result := runner.Result{Command: []string{"python3", "combined_review.py"}, StartedAt: started, FinishedAt: started.Add(time.Second), ExitCode: reviewstep.CombinedExitBase + 3, LogPath: log}
		if err := after(1, result, nil); err != nil {
			return result, err
		}
		return result, nil
	}
	input := combinedReviewFixtureInput(t, workspacePath)

	wrapper := func(ctx context.Context, in ReviewStepInput) (VerifyActivityResult, error) {
		return activities.RunReviewStepActivity(ctx, in)
	}
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestActivityEnvironment()
	env.RegisterActivity(wrapper)
	raw, err := env.ExecuteActivity(wrapper, input)
	if err != nil {
		t.Fatalf("execute combined-review Activity: %v", err)
	}
	var result VerifyActivityResult
	if err := raw.Get(&result); err != nil {
		t.Fatalf("decode combined-review Activity result: %v", err)
	}
	if result.Result.ExitCode != reviewstep.CombinedExitBase+3 {
		t.Errorf("Result.ExitCode = %d, want CombinedExitBase+3 (raw, unsplit -- RunWorkflow splits it)", result.Result.ExitCode)
	}
	if len(result.Attempts) != 1 || result.Attempts[0].Kind != reviewstep.Combined {
		t.Fatalf("Attempts = %+v, want exactly one %q attempt", result.Attempts, reviewstep.Combined)
	}
}

// TestReviewStepArgsForCombinedStepIncludesTheSpecPathAndNoConformityFlags proves
// reviewStepArgs builds combined_review.py's own argv (via
// reviewstep.CombinedArgs) with input.SpecPath verbatim -- a.runSandboxWithRetries
// always stages that host path (specPath), so it must appear in args
// exactly as given for that staging's exact-string substitution to
// translate it to its in-container path. No criteria and no conformity
// policy are passed: the conformity review is retired.
func TestReviewStepArgsForCombinedStepIncludesTheSpecPathAndNoConformityFlags(t *testing.T) {
	input := fixtureInput()
	input.SpecPath = "/fixture/spec.md"
	input.CodeReviewPolicy = "required"
	stepInput := ReviewStepInput{RunWorkflowInput: input, Step: reviewstep.Combined}

	args := reviewStepArgs(stepInput, "/fixture/"+reviewstep.CombinedScriptName)

	assertArgValue := func(flag, want string) {
		for i, a := range args {
			if a == flag {
				if i+1 >= len(args) || args[i+1] != want {
					t.Errorf("%s = %v, want followed by %q", flag, args[i:], want)
				}
				return
			}
		}
		t.Errorf("args %v carry no %s", args, flag)
	}
	assertArgValue("--spec", input.SpecPath)
	assertArgValue("--review-policy", input.CodeReviewPolicy)
	for _, gone := range []string{"--spec-acceptance-criteria", "--conformity-policy"} {
		for _, a := range args {
			if a == gone {
				t.Errorf("args %v carry the retired %s", args, gone)
			}
		}
	}
}

// TestReviewStepNoRelayErrorNamesTheCombinedStep proves the review
// step's no-relay halt text names the policy that needs the model.
func TestReviewStepNoRelayErrorNamesTheCombinedStep(t *testing.T) {
	input := fixtureInput()
	input.CodeReviewPolicy = "required"
	stepInput := ReviewStepInput{RunWorkflowInput: input, Step: reviewstep.Combined}

	got := reviewStepNoRelayError(stepInput)
	if got == "" {
		t.Fatal("reviewStepNoRelayError returned empty string for the Combined step")
	}
	for _, substr := range []string{"-code-review-policy", "model access"} {
		if !strings.Contains(got, substr) {
			t.Errorf("reviewStepNoRelayError = %q, want it to mention %q", got, substr)
		}
	}
}
