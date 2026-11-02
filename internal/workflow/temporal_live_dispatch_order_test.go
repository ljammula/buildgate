package workflow

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"
	temporalworkflow "go.temporal.io/sdk/workflow"
)

// TestTemporalLiveOneSlotQueueServesWaitingStepsInOrder pins the server
// setting that makes the worker's one-slot jobs queue first come, first
// served (matching.numTaskqueueWritePartitions=1, in
// docker-compose.temporal.yml and scripts/temporal-test-server.sh): steps
// queued while the slot is busy run in the order they were scheduled, and a
// busy workflow's next step, scheduled after them, waits its turn. With the
// default four write partitions the waiting steps run in random order and the
// busy workflow's steps cut in, which kept one request's pr_review pass
// waiting for hours behind another request's builds.
func TestTemporalLiveOneSlotQueueServesWaitingStepsInOrder(t *testing.T) {
	temporalClient := dialTemporal(t)
	defer temporalClient.Close()

	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	workflowQueue := "dispatch-order-wf-" + suffix
	jobsQueue := "dispatch-order-jobs-" + suffix

	var mu sync.Mutex
	var started []string
	step := func(_ context.Context, name string) error {
		mu.Lock()
		started = append(started, name)
		mu.Unlock()
		time.Sleep(time.Second)
		return nil
	}
	stepOptions := func(ctx temporalworkflow.Context) temporalworkflow.Context {
		return temporalworkflow.WithActivityOptions(ctx, temporalworkflow.ActivityOptions{TaskQueue: jobsQueue, StartToCloseTimeout: time.Minute})
	}
	busy := func(ctx temporalworkflow.Context, n int) error {
		for i := range n {
			if err := temporalworkflow.ExecuteActivity(stepOptions(ctx), "Step", fmt.Sprintf("busy%d", i)).Get(ctx, nil); err != nil {
				return err
			}
		}
		return nil
	}
	waiting := func(ctx temporalworkflow.Context, name string) error {
		return temporalworkflow.ExecuteActivity(stepOptions(ctx), "Step", name).Get(ctx, nil)
	}

	workflows := worker.New(temporalClient, workflowQueue, worker.Options{})
	workflows.RegisterWorkflowWithOptions(busy, temporalworkflow.RegisterOptions{Name: "Busy"})
	workflows.RegisterWorkflowWithOptions(waiting, temporalworkflow.RegisterOptions{Name: "Waiting"})
	jobs := worker.New(temporalClient, jobsQueue, worker.Options{MaxConcurrentActivityExecutionSize: 1})
	jobs.RegisterActivityWithOptions(step, activity.RegisterOptions{Name: "Step"})
	for _, w := range []worker.Worker{workflows, jobs} {
		if err := w.Start(); err != nil {
			t.Fatalf("start worker: %v", err)
		}
		defer w.Stop()
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	start := func(name string, args ...any) client.WorkflowRun {
		run, err := temporalClient.ExecuteWorkflow(ctx, client.StartWorkflowOptions{TaskQueue: workflowQueue}, name, args...)
		if err != nil {
			t.Fatalf("start %s: %v", name, err)
		}
		return run
	}
	runs := []client.WorkflowRun{start("Busy", 6)}
	// The eight wait only if busy0 already holds the slot.
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		mu.Lock()
		holding := len(started) > 0
		mu.Unlock()
		if holding {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("busy0 never started")
		}
	}
	var want []string
	for i := range 8 {
		name := fmt.Sprintf("waiting%d", i)
		want = append(want, name)
		runs = append(runs, start("Waiting", name))
	}
	for _, run := range runs {
		if err := run.Get(ctx, nil); err != nil {
			t.Fatalf("workflow %s: %v", run.GetID(), err)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	var got []string
	for _, name := range started {
		if !strings.HasPrefix(name, "busy") {
			got = append(got, name)
		}
	}
	if !slices.Equal(got, want) {
		t.Errorf("waiting steps started in order %v, want %v (all steps: %v)", got, want, started)
	}
	// busy0 held the slot while all eight were queued; busy1 was scheduled
	// after them, so it starts only once they have all started.
	if i := slices.Index(started, "busy1"); i != len(want)+1 {
		t.Errorf("busy1 started at position %d, want %d, after every waiting step (all steps: %v)", i, len(want)+1, started)
	}
}
