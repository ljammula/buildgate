package workflow

import (
	"context"
	"errors"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/converter"
	"go.temporal.io/sdk/testsuite"

	"buildgate/internal/sandbox"
)

// runWithHeartbeatCount runs the review step and counts the heartbeats it
// records.
func (f *reviewFixture) runWithHeartbeatCount(heartbeats *atomic.Int32) error {
	f.t.Helper()
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestActivityEnvironment()
	env.SetOnActivityHeartbeatListener(func(*activity.Info, converter.EncodedValues) { heartbeats.Add(1) })
	env.RegisterActivity(f.acts.RunReviewStepActivity)
	_, err := env.ExecuteActivity(f.acts.RunReviewStepActivity, f.input)
	return err
}

// The review step heartbeats while the instruction snapshot runs: RunWorkflow
// gives the step a heartbeat timeout of twice activityHeartbeatInterval, so a
// snapshot that takes longer than one interval must have been heard from
// before it returns.
func TestReviewStepHeartbeatsWhileTheInstructionSnapshotRuns(t *testing.T) {
	f := newReviewFixture(t, map[string]string{"main.go": "package main\n"}, func(repo string) {})
	var heartbeats atomic.Int32
	during := int32(-1)
	f.acts.snapshotReviewInstructions = func(ctx context.Context, _, _, _ string) (sandbox.ReviewInstructionSnapshot, error) {
		select {
		case <-time.After(activityHeartbeatInterval + 2*time.Second):
		case <-ctx.Done():
			return sandbox.ReviewInstructionSnapshot{}, ctx.Err()
		}
		during = heartbeats.Load()
		return sandbox.ReviewInstructionSnapshot{}, nil
	}
	f.fakeLaunch(nil, nil)
	if err := f.runWithHeartbeatCount(&heartbeats); err != nil {
		t.Fatal(err)
	}
	if during < 1 {
		t.Errorf("%d heartbeats while a snapshot of %v ran, want at least one", during, activityHeartbeatInterval+2*time.Second)
	}
}

// A heartbeat is recorded as the snapshot returns, however short it was: the
// launch that follows first heartbeats a whole interval after it begins, and
// a snapshot that took most of an interval would leave the step unheard from
// for close to its heartbeat timeout.
func TestReviewStepHeartbeatsWhenTheInstructionSnapshotReturns(t *testing.T) {
	f := newReviewFixture(t, map[string]string{"main.go": "package main\n"}, func(repo string) {})
	var heartbeats atomic.Int32
	atLaunch := int32(-1)
	f.acts.snapshotReviewInstructions = noReviewInstructions
	f.fakeLaunch(func([]string) { atLaunch = heartbeats.Load() }, nil)
	if err := f.runWithHeartbeatCount(&heartbeats); err != nil {
		t.Fatal(err)
	}
	if atLaunch < 1 {
		t.Errorf("%d heartbeats when the launch began, want the one recorded as the snapshot returned", atLaunch)
	}
}

// A snapshot that failed after it removed paths from the worktree returns
// them with its error, and the halted attempt records them beside the cause.
func TestSnapshotFailureAfterRemovalsRecordsTheRemovedPaths(t *testing.T) {
	f := newReviewFixture(t, map[string]string{"main.go": "package main\n"}, func(repo string) {})
	f.acts.snapshotReviewInstructions = func(context.Context, string, string, string) (sandbox.ReviewInstructionSnapshot, error) {
		return sandbox.ReviewInstructionSnapshot{Removed: []string{"a/AGENTS.md"}}, errors.New("after removing 1 of 2 untracked instruction paths: remove \"zz/AGENTS.md\": permission denied")
	}
	f.fakeLaunch(nil, nil)
	_, err := f.run()
	if err == nil || f.launches != 0 {
		t.Fatalf("err = %v, launches = %d: the review ran although the snapshot failed", err, f.launches)
	}
	attempts := AttemptsFromError(err)
	if len(attempts) != 1 || !reflect.DeepEqual(attempts[0].ReviewRemovedPaths, []string{"a/AGENTS.md"}) || attempts[0].ReviewInstructionsError == "" {
		t.Fatalf("attempts = %+v, want one with the removed path and the cause", attempts)
	}
}

// A snapshot that runs to its own deadline returns its refusal well inside
// the review step: the step's start-to-close limit is many times the
// deadline, and the step heartbeats throughout (the test above), so neither
// limit of the Activity ends it before the refusal reaches the operator.
func TestReviewInstructionSnapshotDeadlineIsWellInsideTheReviewStepsLimit(t *testing.T) {
	if 10*sandbox.ReviewInstructionTimeout > ActivityStartToCloseTimeout {
		t.Fatalf("a snapshot deadline of %v is not well inside the review step's start-to-close limit of %v", sandbox.ReviewInstructionTimeout, ActivityStartToCloseTimeout)
	}
}
