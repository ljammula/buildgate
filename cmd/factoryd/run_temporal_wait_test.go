package main

import (
	"context"
	"errors"
	"testing"

	"go.temporal.io/sdk/client"

	"buildgate/internal/workflow"
)

// fakeWorkflowRun returns errs[i] from its i-th Get, then nil.
type fakeWorkflowRun struct {
	errs  []error
	calls int
}

func (f *fakeWorkflowRun) GetID() string                  { return "wf" }
func (f *fakeWorkflowRun) GetRunID() string               { return "run" }
func (f *fakeWorkflowRun) GetFirstExecutionRunID() string { return "run" }
func (f *fakeWorkflowRun) Get(ctx context.Context, valuePtr interface{}) error {
	f.calls++
	if f.calls <= len(f.errs) {
		return f.errs[f.calls-1]
	}
	return nil
}
func (f *fakeWorkflowRun) GetWithOptions(ctx context.Context, valuePtr interface{}, _ client.WorkflowRunGetOptions) error {
	return f.Get(ctx, valuePtr)
}

func stubWorkflowStillRunning(dp *deps, t *testing.T, running bool) *int {
	t.Helper()
	oldRunning, oldDelay := fakeTemporalOf(dp).stillRunningFn, waitRetryDelay
	describes := 0
	fakeTemporalOf(dp).stillRunningFn = func(client.Client, client.WorkflowRun) bool { describes++; return running }
	waitRetryDelay = 0
	t.Cleanup(func() { fakeTemporalOf(dp).stillRunningFn, waitRetryDelay = oldRunning, oldDelay })
	return &describes
}

func TestWaitForRunWorkflowWaitsAgainWhileTheWorkflowIsStillRunning(t *testing.T) {
	dp := newTestDeps(t)
	stubWorkflowStillRunning(dp, t, true)
	run := &fakeWorkflowRun{errs: []error{context.DeadlineExceeded}}
	var result workflow.RunWorkflowResult
	if err := waitForRunWorkflowWithProgress(dp, context.Background(), nil, run, "r", &result); err != nil {
		t.Fatalf("wait = %v, want nil after the second Get", err)
	}
	if run.calls != 2 {
		t.Fatalf("Get calls = %d, want 2", run.calls)
	}
}

func TestWaitForRunWorkflowReturnsTheErrorOnceTheWorkflowIsNotRunning(t *testing.T) {
	dp := newTestDeps(t)
	stubWorkflowStillRunning(dp, t, false)
	want := errors.New("workflow failed")
	run := &fakeWorkflowRun{errs: []error{want}}
	var result workflow.RunWorkflowResult
	if err := waitForRunWorkflowWithProgress(dp, context.Background(), nil, run, "r", &result); !errors.Is(err, want) {
		t.Fatalf("wait = %v, want %v", err, want)
	}
	if run.calls != 1 {
		t.Fatalf("Get calls = %d, want 1", run.calls)
	}
}

func TestWaitForRunWorkflowStopsWhenItsOwnContextEnds(t *testing.T) {
	dp := newTestDeps(t)
	describes := stubWorkflowStillRunning(dp, t, true)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	run := &fakeWorkflowRun{errs: []error{context.Canceled}}
	var result workflow.RunWorkflowResult
	if err := waitForRunWorkflowWithProgress(dp, ctx, nil, run, "r", &result); !errors.Is(err, context.Canceled) {
		t.Fatalf("wait = %v, want context.Canceled", err)
	}
	if run.calls != 1 || *describes != 0 {
		t.Fatalf("Get calls = %d, Describe calls = %d; want 1 and 0", run.calls, *describes)
	}
}
