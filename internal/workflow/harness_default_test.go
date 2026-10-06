package workflow

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.temporal.io/sdk/testsuite"

	"buildgate/internal/reviewstep"
	"buildgate/internal/run"
	"buildgate/internal/runner"
	"buildgate/internal/sandbox"
	"buildgate/internal/testfixture"
)

// TestRunBuildActivityEmptyHarnessResolvesToPi drives RunBuildActivity itself
// with an empty Harness: the argv it launches carries --harness pi and the
// recorded Attempt.Harness is pi, so the command, the record and the worker
// environment (which resolves "" to pi) cannot disagree.
func TestRunBuildActivityEmptyHarnessResolvesToPi(t *testing.T) {
	var gotArgs []string
	activities := &Activities{
		LogDir: t.TempDir(),
		runWithRetries: func(_ context.Context, _ string, _ func(int) string, _ int, onAttempt func(int, runner.Result, error), _ string, args ...string) (runner.Result, error) {
			gotArgs = args
			res := runner.Result{ExitCode: 0, Command: args}
			if onAttempt != nil {
				onAttempt(1, res, nil)
			}
			return res, nil
		},
	}
	input := fixtureInput() // Harness deliberately empty
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestActivityEnvironment()
	env.RegisterActivity(activities.RunBuildActivity)
	raw, err := env.ExecuteActivity(activities.RunBuildActivity, input)
	if err != nil {
		t.Fatalf("RunBuildActivity: %v", err)
	}
	var result BuildActivityResult
	if err := raw.Get(&result); err != nil {
		t.Fatal(err)
	}
	if n := len(gotArgs); n < 2 || gotArgs[n-2] != "--harness" || gotArgs[n-1] != "pi" {
		t.Errorf("build argv = %v, want it to end with --harness pi", gotArgs)
	}
	if len(result.Attempts) == 0 || result.Attempts[0].Harness != "pi" || result.Attempts[0].Role != run.AttemptRoleExecution {
		t.Errorf("recorded attempts = %+v, want an execution attempt with Harness pi", result.Attempts)
	}
}

// TestRunReviewStepActivityEmptyReviewHarnessResolvesToPi is the review-side
// counterpart: an empty ReviewHarness launches --harness pi and records pi.
func TestRunReviewStepActivityEmptyReviewHarnessResolvesToPi(t *testing.T) {
	workspacePath := testfixture.NewGitRepo(t)
	var gotArgs []string
	activities := testRoutedActivities(sandbox.NewRouteSecret("worker-static-upstream-key"))
	activities.LogDir = t.TempDir()
	activities.runWithRetriesChecked = func(_ context.Context, _ string, logPath func(int) string, _ int, before func(int) error, after func(int, runner.Result, error) error, _ string, args ...string) (runner.Result, error) {
		gotArgs = args
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
		result := runner.Result{Command: args, StartedAt: started, FinishedAt: started.Add(time.Second), ExitCode: 0, LogPath: log}
		return result, after(1, result, nil)
	}
	input := codeReviewFixtureInput(t, workspacePath) // ReviewHarness deliberately empty
	wrapper := func(ctx context.Context, in ReviewStepInput) (VerifyActivityResult, error) {
		return activities.RunReviewStepActivity(ctx, in)
	}
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestActivityEnvironment()
	env.RegisterActivity(wrapper)
	raw, err := env.ExecuteActivity(wrapper, input)
	if err != nil {
		t.Fatalf("review step: %v", err)
	}
	var result VerifyActivityResult
	if err := raw.Get(&result); err != nil {
		t.Fatal(err)
	}
	if n := len(gotArgs); n < 2 || gotArgs[n-2] != "--harness" || gotArgs[n-1] != "pi" {
		t.Errorf("review argv = %v, want it to end with --harness pi", gotArgs)
	}
	if len(result.Attempts) == 0 || result.Attempts[0].Harness != "pi" || result.Attempts[0].Role != run.AttemptRoleReview {
		t.Errorf("recorded attempts = %+v, want a review attempt with Harness pi", result.Attempts)
	}
}

// TestEmptyHarnessResolvesToPiInCommandAndEnvironment: the review-step argv and
// worker environment for an execution whose
// Harness/ReviewHarness are empty runs pi in the command (--harness pi) exactly
// as the worker environment resolves it, so the two can never disagree.
func TestEmptyHarnessResolvesToPiInCommandAndEnvironment(t *testing.T) {
	in := RunWorkflowInput{SpecAcceptanceCriteria: "/ws/criteria.md"}
	a := &Activities{}
	for _, step := range append(append([]reviewstep.Step{}, reviewstep.Steps...), reviewstep.CombinedStep) {
		args := reviewStepArgs(a, step, ReviewStepInput{RunWorkflowInput: in}, "/s.py")
		if n := len(args); args[n-2] != "--harness" || args[n-1] != "pi" {
			t.Errorf("reviewStepArgs(%s) = %v, want --harness pi for an empty ReviewHarness", step.Name, args)
		}
	}
	if len(executionHarnessEnv(in)) != 0 || len(reviewHarnessEnv(in)) != 0 {
		t.Error("an empty harness got a worker environment, but resolves to pi")
	}
	if harnessArg(" pifork ") != "pifork" || harnessArg("bogus") != "bogus" {
		t.Errorf("harnessArg canonicalisation wrong: %q %q", harnessArg(" pifork "), harnessArg("bogus"))
	}
}
