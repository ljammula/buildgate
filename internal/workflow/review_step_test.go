package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"

	"buildgate/internal/codereview"
	"buildgate/internal/reviewstep"
	"buildgate/internal/runner"
	"buildgate/internal/sandbox"
	"buildgate/internal/testfixture"
)

// TestReviewStepsOrderIsSpecConformityThenCodeReview pins reviewstep.Steps'
// own order (M4-K3): both cmd/factoryd's and RunWorkflow read
// this same table and rely on the spec-conformity review running before
// the code review -- see reviewstep's own package doc comment.
func TestReviewStepsOrderIsSpecConformityThenCodeReview(t *testing.T) {
	if len(reviewstep.Steps) != 2 {
		t.Fatalf("len(reviewstep.Steps) = %d, want 2", len(reviewstep.Steps))
	}
	if reviewstep.Steps[0].Name != reviewstep.SpecConformity {
		t.Errorf("Steps[0].Name = %q, want %q", reviewstep.Steps[0].Name, reviewstep.SpecConformity)
	}
	if reviewstep.Steps[1].Name != reviewstep.CodeReview {
		t.Errorf("Steps[1].Name = %q, want %q", reviewstep.Steps[1].Name, reviewstep.CodeReview)
	}
}

// TestRunReviewStepActivityChecksInIndependentlyPerStep proves the
// unification of RunSpecConformityReviewActivity/RunCodeReviewActivity
// onto one RunReviewStepActivity implementation (M4-K3) did not collapse
// the two steps' own durable checkpoints into one: two separate
// ExecuteActivity calls against the SAME run's checkpoint/log directories
// -- one per reviewstep.Steps entry, exactly as RunWorkflow issues them --
// must each get their own checkpoint file, keyed by their own distinct
// ActivityID (Temporal assigns each ExecuteActivity call its own), not
// share or overwrite one another's.
func TestRunReviewStepActivityChecksInIndependentlyPerStep(t *testing.T) {
	workspacePath := testfixture.NewGitRepo(t)
	dataDir := t.TempDir()
	logDir := filepath.Join(dataDir, "logs")
	checkpointDir := filepath.Join(dataDir, "checkpoints")

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
		result := runner.Result{Command: []string{"python3", "review.py"}, StartedAt: started, FinishedAt: started.Add(time.Second), ExitCode: 0, LogPath: log}
		if err := after(1, result, nil); err != nil {
			return result, err
		}
		return result, nil
	}

	baseInput := fixtureInput()
	baseInput.WorkspacePath = workspacePath
	baseInput.RunID = "review-step-checkpoint-run"
	baseInput.DataDir = dataDir
	baseInput.LogDir = logDir
	baseInput.CheckpointDir = checkpointDir
	baseInput.SpecAcceptanceCriteria = "/fixture/criteria.md"
	baseInput.CodeReviewPolicy = codereview.PolicyAdvisory
	policy := *testRelayPolicy()
	policy.AllowUnauthenticatedUpstream = true
	baseInput.RoutePolicy = &policy

	wrapper := func(ctx context.Context, in ReviewStepInput) (VerifyActivityResult, error) {
		return activities.RunReviewStepActivity(ctx, in)
	}
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestActivityEnvironment()
	env.RegisterActivity(wrapper)

	for _, step := range reviewstep.Steps {
		in := ReviewStepInput{RunWorkflowInput: baseInput, Step: step.Name}
		if _, err := env.ExecuteActivity(wrapper, in); err != nil {
			t.Fatalf("execute %s review step: %v", step.Name, err)
		}
	}

	entries, err := os.ReadDir(filepath.Join(checkpointDir, "activity-checkpoints"))
	if err != nil {
		t.Fatalf("read activity-checkpoints dir: %v", err)
	}
	// Each ExecuteActivity call writes its own completed-checkpoint file
	// (named by its own ActivityID) plus a sibling ".intent.json" attempt-
	// intent file -- count only the completed checkpoints (excluding the
	// intent and spend-start sidecars) to check for one per review step.
	checkpoints := 0
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
		if strings.HasSuffix(e.Name(), ".json") && !strings.HasSuffix(e.Name(), ".intent.json") && !strings.HasSuffix(e.Name(), ".spend-start.json") && !strings.HasSuffix(e.Name(), ".lease.json") {
			checkpoints++
		}
	}
	if checkpoints != 2 {
		t.Fatalf("activity-checkpoints entries = %v, want exactly 2 completed checkpoints (one per review step, keyed by its own ActivityID)", names)
	}
}

// TestReviewStepArgsUsesDiffBaseNotBaseSHA proves both review steps'
// --review-base-sha comes from RunWorkflowInput.DiffBaseSHA (the
// cumulative ticket diff a -diff-base override names) when it is set,
// not from BaseSHA (a corrective round's own checkout point) -- see
// DiffBaseSHA's own doc comment (workflow_types.go) for the incident this
// guards against: before DiffBaseSHA existed, a Temporal-routed
// corrective round's spec-conformity/code review always saw only the
// round's own tiny delta.
func TestReviewStepArgsUsesDiffBaseNotBaseSHA(t *testing.T) {
	activities := &Activities{}
	input := fixtureInput()
	input.BaseSHA = "round-own-checkout-point"
	input.DiffBaseSHA = "ticket-original-base"
	input.SpecAcceptanceCriteria = "/fixture/criteria.md"
	input.CodeReviewPolicy = "advisory"

	for _, step := range reviewstep.Steps {
		stepInput := ReviewStepInput{RunWorkflowInput: input, Step: step.Name}
		args := reviewStepArgs(activities, step, stepInput, "/fixture/"+step.ScriptName)
		found := false
		for i, a := range args {
			if a != "--review-base-sha" {
				continue
			}
			found = true
			if i+1 >= len(args) || args[i+1] != input.DiffBaseSHA {
				t.Errorf("%s: --review-base-sha = %v, want %q (DiffBaseSHA), not %q (BaseSHA)", step.Name, args[i:], input.DiffBaseSHA, input.BaseSHA)
			}
		}
		if !found {
			t.Errorf("%s: args %v carry no --review-base-sha at all", step.Name, args)
		}
	}
}

// TestRunReviewStepActivityRecordsSpendStartAndRefusesSupersededLaunch: the
// spend-start record is written at the top of attempt 1 (literal: the 100
// tokens already in the run ledger), and when a later attempt takes the
// lease before the runner's before hook, the hook refuses and nothing
// launches.
func TestRunReviewStepActivityRecordsSpendStartAndRefusesSupersededLaunch(t *testing.T) {
	workspacePath := testfixture.NewGitRepo(t)
	dataDir := t.TempDir()
	logDir := filepath.Join(dataDir, "logs")
	checkpointDir := filepath.Join(dataDir, "checkpoints")
	appendLedger(t, dataDir, "review-lease-run", 100, 7)

	activities := testRoutedActivities(sandbox.NewRouteSecret("worker-static-upstream-key"))
	activities.LogDir = logDir
	launched := false
	var hookErr error
	activities.runWithRetriesChecked = func(ctx context.Context, _ string, _ func(int) string, _ int, before func(int) error, _ func(int, runner.Result, error) error, _ string, _ ...string) (runner.Result, error) {
		info := activity.GetInfo(ctx)
		if err := takeActivityLease(checkpointDir, info.WorkflowExecution.ID, info.WorkflowExecution.RunID, info.ActivityID, 2); err != nil {
			return runner.Result{}, err
		}
		if hookErr = before(1); hookErr != nil {
			return runner.Result{}, hookErr
		}
		launched = true
		return runner.Result{}, nil
	}
	input := fixtureInput()
	input.WorkspacePath = workspacePath
	input.RunID = "review-lease-run"
	input.DataDir = dataDir
	input.LogDir = logDir
	input.CheckpointDir = checkpointDir
	input.SpecAcceptanceCriteria = "/fixture/criteria.md"
	input.CodeReviewPolicy = codereview.PolicyAdvisory
	policy := *testRelayPolicy()
	policy.AllowUnauthenticatedUpstream = true
	input.RoutePolicy = &policy

	wrapper := func(ctx context.Context, in ReviewStepInput) (VerifyActivityResult, error) {
		return activities.RunReviewStepActivity(ctx, in)
	}
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestActivityEnvironment()
	env.RegisterActivity(wrapper)
	_, err := env.ExecuteActivity(wrapper, ReviewStepInput{RunWorkflowInput: input, Step: reviewstep.Steps[0].Name})
	if err == nil || hookErr == nil {
		t.Fatalf("err = %v, hookErr = %v; want the superseded refusal", err, hookErr)
	}
	var appErr *temporal.ApplicationError
	if !errors.As(hookErr, &appErr) || appErr.Type() != ActivitySupersededType {
		t.Errorf("hook err = %v, want %s", hookErr, ActivitySupersededType)
	}
	if launched {
		t.Error("a superseded attempt launched")
	}
	entries, _ := os.ReadDir(filepath.Join(checkpointDir, "activity-checkpoints"))
	var startFile string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".spend-start.json") {
			startFile = filepath.Join(checkpointDir, "activity-checkpoints", e.Name())
		}
	}
	if startFile == "" {
		t.Fatalf("no spend-start record among %v", entries)
	}
	b, _ := os.ReadFile(startFile)
	var start activitySpendStart
	if err := json.Unmarshal(b, &start); err != nil || start.Tokens != 100 || start.CostMicroUSD != 7 {
		t.Errorf("spend-start = %+v err=%v, want 100 tokens, 7 cost", start, err)
	}
}
