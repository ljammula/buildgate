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

// combinedReviewFixtureInput is fixtureInput() plus the relay policy,
// spec, and criteria path RunReviewStepActivity needs for the Combined
// step -- mirrors specConformityReviewFixtureInput/codeReviewFixtureInput.
func combinedReviewFixtureInput(t *testing.T, workspacePath string) ReviewStepInput {
	t.Helper()
	input := fixtureInput()
	input.WorkspacePath = workspacePath
	input.RunID = "combined-review-run"
	input.SpecPath = "/fixture/spec.md"
	input.SpecAcceptanceCriteria = "/fixture/criteria.md"
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

// TestRunReviewStepActivityAcceptsCombinedStepDespiteItNotBeingInSteps
// proves RunReviewStepActivity resolves Step == reviewstep.Combined via
// reviewstep.CombinedStep instead of reviewstep.ByName -- which never
// finds Combined (it is deliberately absent from reviewstep.Steps, see
// that table's own doc comment) -- so a combined-review launch does not
// fail with "unknown review step" the way an unrecognized Step value
// correctly still does.
func TestRunReviewStepActivityAcceptsCombinedStepDespiteItNotBeingInSteps(t *testing.T) {
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
		// what splits it via reviewstep.GateExitCodes.
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

// TestReviewStepArgsForCombinedStepIncludesBothHostPaths proves
// reviewStepArgs builds combined_review.py's own argv (via
// reviewstep.CombinedArgs) with BOTH input.SpecPath and
// input.SpecAcceptanceCriteria verbatim -- a.runSandboxWithRetries
// always stages both host paths (specPath and extraRunInput; see its
// own doc comment on extraRunInput), so both must appear in args
// exactly as given for that staging's exact-string substitution to
// translate them to their in-container paths.
func TestReviewStepArgsForCombinedStepIncludesBothHostPaths(t *testing.T) {
	activities := &Activities{}
	input := fixtureInput()
	input.SpecPath = "/fixture/spec.md"
	input.SpecAcceptanceCriteria = "/fixture/criteria.md"
	input.CodeReviewPolicy = "required"
	stepInput := ReviewStepInput{RunWorkflowInput: input, Step: reviewstep.Combined}

	args := reviewStepArgs(activities, reviewstep.CombinedStep, stepInput, "/fixture/"+reviewstep.CombinedScriptName)

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
	assertArgValue("--spec-acceptance-criteria", input.SpecAcceptanceCriteria)
	assertArgValue("--review-policy", input.CodeReviewPolicy)
}

// TestReviewStepNoRelayErrorNamesTheCombinedStep proves the Combined
// step gets its own specific no-relay halt text (mirrors
// run_ticket.go's own runReviewPhase noRelayErr parameter for the
// Combined case), not one of the two standalone steps' text.
func TestReviewStepNoRelayErrorNamesTheCombinedStep(t *testing.T) {
	input := fixtureInput()
	input.SpecAcceptanceCriteria = "/fixture/criteria.md"
	input.CodeReviewPolicy = "required"
	stepInput := ReviewStepInput{RunWorkflowInput: input, Step: reviewstep.Combined}

	got := reviewStepNoRelayError(reviewstep.CombinedStep, stepInput)
	if got == "" {
		t.Fatal("reviewStepNoRelayError returned empty string for the Combined step")
	}
	for _, substr := range []string{"-spec-acceptance-criteria", "-code-review-policy", "combined review"} {
		if !strings.Contains(got, substr) {
			t.Errorf("reviewStepNoRelayError = %q, want it to mention %q", got, substr)
		}
	}
}
