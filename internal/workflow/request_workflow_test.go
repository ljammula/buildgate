package workflow

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/converter"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"
	temporalworkflow "go.temporal.io/sdk/workflow"
)

// fakeRequestActivities scripts LoadRequestStep and records every activity
// call as "<name>@<task queue>".
type fakeRequestActivities struct {
	mu       sync.Mutex
	steps    []RequestStep
	advance  []error
	after    []string
	calls    []string
	halted   []HaltLostRequestStepInput
	loadSeen int
}

func (f *fakeRequestActivities) record(ctx context.Context, name string) {
	f.calls = append(f.calls, name+"@"+activity.GetInfo(ctx).TaskQueue)
}

func (f *fakeRequestActivities) register(env *testsuite.TestWorkflowEnvironment) {
	env.RegisterActivityWithOptions(func(ctx context.Context, _ string) (RequestStep, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.record(ctx, LoadRequestStepActivityName)
		if f.loadSeen >= len(f.steps) {
			return RequestStep{Done: true}, nil
		}
		step := f.steps[f.loadSeen]
		f.loadSeen++
		return step, nil
	}, activity.RegisterOptions{Name: LoadRequestStepActivityName})
	env.RegisterActivityWithOptions(func(ctx context.Context, _ string) (string, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.record(ctx, AdvanceRequestActivityName)
		if len(f.advance) > 0 {
			err := f.advance[0]
			f.advance = f.advance[1:]
			if err != nil {
				return "", err
			}
		}
		// By default the step leaves the request in the state it was loaded
		// in; f.after scripts a state change.
		after := f.steps[f.loadSeen-1].State
		if len(f.after) > 0 {
			after, f.after = f.after[0], f.after[1:]
		}
		return after, nil
	}, activity.RegisterOptions{Name: AdvanceRequestActivityName})
	env.RegisterActivityWithOptions(func(ctx context.Context, _ string) error {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.record(ctx, RemindRequestActivityName)
		return nil
	}, activity.RegisterOptions{Name: RemindRequestActivityName})
	env.RegisterActivityWithOptions(func(ctx context.Context, in HaltLostRequestStepInput) error {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.record(ctx, HaltLostRequestStepActivityName)
		f.halted = append(f.halted, in)
		return nil
	}, activity.RegisterOptions{Name: HaltLostRequestStepActivityName})
}

var testRequestInput = RequestWorkflowInput{RequestID: "req-1", JobsTaskQueue: "jobs-q", LightTaskQueue: "light-q"}

func runRequestWorkflow(t *testing.T, f *fakeRequestActivities, setup func(*testsuite.TestWorkflowEnvironment)) *testsuite.TestWorkflowEnvironment {
	t.Helper()
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.SetTestTimeout(10 * time.Second)
	f.register(env)
	if setup != nil {
		setup(env)
	}
	env.ExecuteWorkflow(RequestWorkflow, testRequestInput)
	if !env.IsWorkflowCompleted() {
		t.Fatal("workflow did not complete")
	}
	return env
}

func assertCalls(t *testing.T, f *fakeRequestActivities, want ...string) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) != len(want) {
		t.Fatalf("calls = %v, want %v", f.calls, want)
	}
	for i := range want {
		if f.calls[i] != want[i] {
			t.Fatalf("calls = %v, want %v", f.calls, want)
		}
	}
}

func TestRequestWorkflowRunsJobStepsOnJobsQueueAndFinishes(t *testing.T) {
	f := &fakeRequestActivities{steps: []RequestStep{
		{State: "submitted", Advance: RequestAdvanceLight},
		{State: "spec_drafting", Advance: RequestAdvanceJobs},
	}}
	env := runRequestWorkflow(t, f, nil)
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error: %v", err)
	}
	assertCalls(t, f,
		"LoadRequestStep@light-q", "AdvanceRequest@light-q",
		"LoadRequestStep@light-q", "AdvanceRequest@jobs-q",
		"LoadRequestStep@light-q")
}

func TestRequestWorkflowRemindsWhenReviewWaitTimesOut(t *testing.T) {
	f := &fakeRequestActivities{steps: []RequestStep{
		{State: "spec_review", Wait: time.Hour, Remind: true},
	}}
	env := runRequestWorkflow(t, f, nil)
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error: %v", err)
	}
	assertCalls(t, f, "LoadRequestStep@light-q", "RemindRequest@light-q", "LoadRequestStep@light-q")
}

func TestRequestWorkflowWakeSignalEndsWaitWithoutReminder(t *testing.T) {
	f := &fakeRequestActivities{steps: []RequestStep{
		{State: "spec_review", Wait: time.Hour, Remind: true},
	}}
	env := runRequestWorkflow(t, f, func(env *testsuite.TestWorkflowEnvironment) {
		env.RegisterDelayedCallback(func() {
			env.SignalWorkflow(RequestWakeSignalName, nil)
		}, time.Minute)
	})
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error: %v", err)
	}
	assertCalls(t, f, "LoadRequestStep@light-q", "LoadRequestStep@light-q")
}

// A halted or quarantined request has no timer: only a decision ends the
// wait, so an idle request adds nothing to history.
func TestRequestWorkflowWaitsForSignalOnlyWhenNoStepAndNoWait(t *testing.T) {
	f := &fakeRequestActivities{steps: []RequestStep{{State: "halted"}}}
	env := runRequestWorkflow(t, f, func(env *testsuite.TestWorkflowEnvironment) {
		env.RegisterDelayedCallback(func() {
			env.SignalWorkflow(RequestWakeSignalName, nil)
		}, 30*24*time.Hour)
	})
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error: %v", err)
	}
	assertCalls(t, f, "LoadRequestStep@light-q", "LoadRequestStep@light-q")
}

// A wake signal sent while a step runs (a cancel during a build) ends the
// wait that follows it at once.
func TestRequestWorkflowWakeDuringStepEndsFollowingWait(t *testing.T) {
	f := &fakeRequestActivities{steps: []RequestStep{
		{State: "pr_review", Advance: RequestAdvanceLight, Wait: 5 * time.Minute},
	}}
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.SetTestTimeout(10 * time.Second)
	f.register(env)
	env.SetOnActivityStartedListener(func(info *activity.Info, _ context.Context, _ converter.EncodedValues) {
		if info.ActivityType.Name == AdvanceRequestActivityName {
			env.SignalWorkflow(RequestWakeSignalName, nil)
		}
	})
	env.ExecuteWorkflow(RequestWorkflow, testRequestInput)
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error: %v", err)
	}
	assertCalls(t, f, "LoadRequestStep@light-q", "AdvanceRequest@light-q", "LoadRequestStep@light-q")
}

func TestRequestWorkflowHaltsRequestWhenJobStepIsLost(t *testing.T) {
	f := &fakeRequestActivities{
		steps:   []RequestStep{{State: "building", Advance: RequestAdvanceJobs}},
		advance: []error{temporal.NewTimeoutError(enumspb.TIMEOUT_TYPE_HEARTBEAT, nil)},
	}
	env := runRequestWorkflow(t, f, nil)
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error: %v", err)
	}
	assertCalls(t, f,
		"LoadRequestStep@light-q", "AdvanceRequest@jobs-q",
		"HaltLostRequestStep@light-q", "LoadRequestStep@light-q")
	if len(f.halted) != 1 || f.halted[0].State != "building" || f.halted[0].RequestID != "req-1" {
		t.Fatalf("halted = %+v", f.halted)
	}
}

func TestRequestWorkflowRetriesFailedStepAfterRetryAfter(t *testing.T) {
	f := &fakeRequestActivities{
		steps: []RequestStep{
			{State: "planning", Advance: RequestAdvanceJobs, RetryAfter: 2 * time.Second},
			{State: "planning", Advance: RequestAdvanceJobs, RetryAfter: 2 * time.Second},
		},
		advance: []error{errors.New("write tickets: disk full")},
	}
	env := runRequestWorkflow(t, f, nil)
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error: %v", err)
	}
	assertCalls(t, f,
		"LoadRequestStep@light-q", "AdvanceRequest@jobs-q",
		"LoadRequestStep@light-q", "AdvanceRequest@jobs-q",
		"LoadRequestStep@light-q")
}

func TestRequestWorkflowContinuesAsNewWhenSuggested(t *testing.T) {
	f := &fakeRequestActivities{steps: []RequestStep{{State: "submitted", Advance: RequestAdvanceLight}}}
	env := runRequestWorkflow(t, f, func(env *testsuite.TestWorkflowEnvironment) {
		env.SetContinueAsNewSuggested(true)
	})
	var canErr *temporalworkflow.ContinueAsNewError
	if err := env.GetWorkflowError(); !errors.As(err, &canErr) {
		t.Fatalf("workflow error = %v, want ContinueAsNewError", err)
	}
	if canErr.WorkflowType.Name != RequestWorkflowName {
		t.Fatalf("continued as %q, want %q", canErr.WorkflowType.Name, RequestWorkflowName)
	}
}

func TestRequestWorkflowRejectsIncompleteInput(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.ExecuteWorkflow(RequestWorkflow, RequestWorkflowInput{RequestID: "req-1"})
	if env.GetWorkflowError() == nil {
		t.Fatal("want an error for missing task queues")
	}
}

func TestLostActivityClassifiesTimeouts(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{temporal.NewTimeoutError(enumspb.TIMEOUT_TYPE_HEARTBEAT, nil), true},
		{temporal.NewTimeoutError(enumspb.TIMEOUT_TYPE_START_TO_CLOSE, nil), true},
		{temporal.NewTimeoutError(enumspb.TIMEOUT_TYPE_SCHEDULE_TO_START, nil), false},
		{errors.New("boom"), false},
	}
	for _, c := range cases {
		if got := lostActivity(c.err); got != c.want {
			t.Errorf("lostActivity(%v) = %v, want %v", c.err, got, c.want)
		}
	}
}

// A step that moves the request on goes straight to the next step instead of
// waiting out the old state's timer (an approved PR starts the next ticket).
func TestRequestWorkflowSkipsWaitWhenStepChangesState(t *testing.T) {
	f := &fakeRequestActivities{
		steps: []RequestStep{{State: "pr_review", Advance: RequestAdvanceJobs, Wait: 5 * time.Minute}},
		after: []string{"building"},
	}
	var start time.Time
	env := runRequestWorkflow(t, f, func(env *testsuite.TestWorkflowEnvironment) { start = env.Now() })
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error: %v", err)
	}
	if waited := env.Now().Sub(start); waited >= 5*time.Minute {
		t.Fatalf("waited %s after a state change", waited)
	}
	assertCalls(t, f, "LoadRequestStep@light-q", "AdvanceRequest@jobs-q", "LoadRequestStep@light-q")
}
