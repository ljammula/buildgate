package main

import (
	"bytes"
	"context"
	"errors"
	"log"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	workflowpb "go.temporal.io/api/workflow/v1"
	"go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/sdk/client"

	"buildgate/internal/run"
)

// runningWorkflowClient answers Describe with a Running workflow and counts
// Terminate calls; every other client.Client method would panic on the nil
// embed.
type runningWorkflowClient struct {
	client.Client
	terminated int
	order      *[]string // when set, Terminate appends "terminate"
}

func (f *runningWorkflowClient) DescribeWorkflowExecution(context.Context, string, string) (*workflowservice.DescribeWorkflowExecutionResponse, error) {
	return &workflowservice.DescribeWorkflowExecutionResponse{
		WorkflowExecutionInfo: &workflowpb.WorkflowExecutionInfo{Status: enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING},
	}, nil
}

func (f *runningWorkflowClient) TerminateWorkflow(context.Context, string, string, string, ...interface{}) error {
	f.terminated++
	if f.order != nil {
		*f.order = append(*f.order, "terminate")
	}
	return nil
}

// blockedExecution is a workflow run whose Get only returns when the wait's
// context ends.
type blockedExecution struct{ client.WorkflowRun }

func (blockedExecution) GetID() string    { return "wf" }
func (blockedExecution) GetRunID() string { return "tr" }
func (blockedExecution) Get(ctx context.Context, _ interface{}) error {
	<-ctx.Done()
	return ctx.Err()
}

// TestAwaitRunWorkflowTerminatesAndHaltsWhenTheWaitEnds proves a wait that
// ends before the workflow does, whether by the process's signal context (stop,
// SIGTERM) or by the overall timeout, terminates the Running workflow and
// records the run halted: a stopped process never leaves a workflow running
// for a later start to re-attach to.
func TestAwaitRunWorkflowTerminatesAndHaltsWhenTheWaitEnds(t *testing.T) {
	dp := newTestDeps(t)
	for _, tc := range []struct {
		name   string
		signal bool
	}{{"signal", true}, {"timeout", false}} {
		t.Run(tc.name, func(t *testing.T) {
			dataDir := t.TempDir()
			seedOwnedRun(t, dataDir, "run-s", run.StateSliceRunning, os.Getpid(), "wf")
			var order []string
			fake := &runningWorkflowClient{order: &order}
			var logs bytes.Buffer
			log.SetOutput(&logs)
			defer log.SetOutput(os.Stderr)
			lifecycleCtx, cancelLifecycle := context.WithCancel(context.Background())
			defer cancelLifecycle()
			timeout := time.Hour
			if !tc.signal {
				timeout = time.Millisecond
			}
			runCtx, cancelRun := context.WithTimeout(lifecycleCtx, timeout)
			defer cancelRun()
			if tc.signal {
				cancelLifecycle()
			}
			r, err := run.Load(dataDir, "run-s")
			if err != nil {
				t.Fatal(err)
			}

			awaitErr := awaitRunWorkflow(dp, runCtx, fake, blockedExecution{}, r, runOptions{DataDir: dataDir, ID: "run-s", Ticket: "ticket", BaseSHA: "base"}, "factoryd-run-s",
				func() { order = append(order, "cancel-activities") })

			if awaitErr == nil {
				t.Fatal("awaitRunWorkflow returned nil for an interrupted wait")
			}
			if tc.signal && !errors.Is(awaitErr, context.Canceled) {
				t.Errorf("err = %v, want it to wrap context.Canceled", awaitErr)
			}
			got, err := run.Load(dataDir, "run-s")
			if err != nil {
				t.Fatal(err)
			}
			if got.State != run.StateHalted || !got.HaltConfirmed {
				t.Errorf("run = state %s halt_confirmed %v, want halted and confirmed", got.State, got.HaltConfirmed)
			}
			if fake.terminated != 1 {
				t.Errorf("terminated = %d, want 1", fake.terminated)
			}
			// The workflow is terminated before the build Activity is
			// cancelled: the reverse would let the workflow see a failed
			// Activity and roll back a worktree kept for resume.
			want := []string{"terminate", "cancel-activities"}
			if !tc.signal {
				want = []string{"terminate"} // a timeout lets the Activity run on, as before
			}
			if !slices.Equal(order, want) {
				t.Errorf("call order = %v, want %v", order, want)
			}
			const operatorLine = "run run-s: operator cancelled this run (SIGINT/SIGTERM)"
			if n := strings.Count(logs.String(), operatorLine); n != map[bool]int{true: 1, false: 0}[tc.signal] {
				t.Errorf("operator line printed %d times (signal=%v), want once for a signal and never for a timeout; log:\n%s", n, tc.signal, logs.String())
			}
		})
	}
}
