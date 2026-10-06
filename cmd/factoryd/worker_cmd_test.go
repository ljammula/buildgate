package main

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"go.temporal.io/sdk/client"

	"buildgate/internal/request"
	"buildgate/internal/workflow"
)

func saveRequestInState(t *testing.T, dataDir, id string, state request.State) {
	t.Helper()
	r := request.New(id, "/repos/app", "app", request.Source{Kind: request.SourceText}, time.Now())
	r.State = state
	if err := r.Save(dataDir); err != nil {
		t.Fatalf("save request %s: %v", id, err)
	}
}

func TestRequestTaskQueuesAreStablePerDataDir(t *testing.T) {
	dataDir := t.TempDir()
	jobs, light, err := requestTaskQueues(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(jobs, "factoryd-jobs-") || !strings.HasPrefix(light, "factoryd-light-") ||
		strings.TrimPrefix(jobs, "factoryd-jobs-") != strings.TrimPrefix(light, "factoryd-light-") {
		t.Fatalf("queues = %q, %q", jobs, light)
	}
	jobs2, light2, err := requestTaskQueues(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if jobs2 != jobs || light2 != light {
		t.Fatalf("second call = %q, %q; want %q, %q", jobs2, light2, jobs, light)
	}
	other, _, err := requestTaskQueues(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if other == jobs {
		t.Fatalf("two data dirs share queue %q", jobs)
	}
}

func TestRequestTaskQueuesRejectsEmptyID(t *testing.T) {
	dataDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dataDir, requestQueueIDFileName), []byte("\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := requestTaskQueues(dataDir); err == nil {
		t.Fatal("want an error for an empty queue id file")
	}
}

type recordingStarter struct {
	ids     []string
	options []client.StartWorkflowOptions
	inputs  []workflow.RequestWorkflowInput
	signals []string
}

func (s *recordingStarter) SignalWithStartWorkflow(_ context.Context, workflowID, signalName string, _ interface{}, options client.StartWorkflowOptions, _ interface{}, workflowArgs ...interface{}) (client.WorkflowRun, error) {
	s.ids = append(s.ids, workflowID)
	s.signals = append(s.signals, signalName)
	s.options = append(s.options, options)
	s.inputs = append(s.inputs, workflowArgs[0].(workflow.RequestWorkflowInput))
	return nil, nil
}

func TestStartRequestWorkflowsSkipsFinishedRequests(t *testing.T) {
	dataDir := t.TempDir()
	saveRequestInState(t, dataDir, "req-review", request.StateSpecReview)
	saveRequestInState(t, dataDir, "req-halted", request.StateHalted)
	saveRequestInState(t, dataDir, "req-done", request.StateDone)
	saveRequestInState(t, dataDir, "req-cancelled", request.StateCancelled)
	starter := &recordingStarter{}
	if err := startRequestWorkflows(context.Background(), starter, dataDir, "jobs-q", "light-q"); err != nil {
		t.Fatal(err)
	}
	got := slices.Clone(starter.ids)
	slices.Sort(got)
	want := []string{workflow.RequestWorkflowID("req-halted"), workflow.RequestWorkflowID("req-review")}
	if !slices.Equal(got, want) {
		t.Fatalf("started %v, want %v", got, want)
	}
	for i := range starter.ids {
		if starter.signals[i] != workflow.RequestWakeSignalName || starter.options[i].TaskQueue != "light-q" {
			t.Errorf("start %d: signal %q, queue %q", i, starter.signals[i], starter.options[i].TaskQueue)
		}
		if in := starter.inputs[i]; in.JobsTaskQueue != "jobs-q" || in.LightTaskQueue != "light-q" || workflow.RequestWorkflowID(in.RequestID) != starter.ids[i] {
			t.Errorf("start %d: input %+v", i, in)
		}
	}
}
