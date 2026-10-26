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
	stubWorkflowTerminator // Describe: the legacy workflows that run
	signaled               []string
	ids                    []string
	options                []client.StartWorkflowOptions
	inputs                 []workflow.RequestWorkflowInput
	signals                []string
}

func (s *recordingStarter) SignalWorkflow(_ context.Context, workflowID, _, _ string, _ interface{}) error {
	s.signaled = append(s.signaled, workflowID)
	return nil
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
	saveRequestInState(t, dataDir, "req-skipped", request.StateSpecReview)
	saveRequestInState(t, dataDir, "req-done", request.StateDone)
	saveRequestInState(t, dataDir, "req-cancelled", request.StateCancelled)
	saveRequestInState(t, dataDir, "req-closed", request.StateClosed)
	starter := &recordingStarter{}
	if err := startRequestWorkflows(context.Background(), starter, dataDir, "qid", "jobs-q", "light-q", map[string]bool{"req-skipped": true}); err != nil {
		t.Fatal(err)
	}
	got := slices.Clone(starter.ids)
	slices.Sort(got)
	want := []string{workflow.RequestWorkflowID("qid", "req-halted"), workflow.RequestWorkflowID("qid", "req-review")}
	if !slices.Equal(got, want) {
		t.Fatalf("started %v, want %v", got, want)
	}
	for i := range starter.ids {
		if starter.signals[i] != workflow.RequestWakeSignalName || starter.options[i].TaskQueue != "light-q" {
			t.Errorf("start %d: signal %q, queue %q", i, starter.signals[i], starter.options[i].TaskQueue)
		}
		if in := starter.inputs[i]; in.JobsTaskQueue != "jobs-q" || in.LightTaskQueue != "light-q" || workflow.RequestWorkflowID("qid", in.RequestID) != starter.ids[i] {
			t.Errorf("start %d: input %+v", i, in)
		}
	}
}

// Two data dirs on one Temporal server that hold a request with the same id
// must not share a workflow id.
func TestRequestWorkflowIDsDifferPerDataDir(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	qa, err := requestQueueID(a)
	if err != nil {
		t.Fatal(err)
	}
	qb, err := requestQueueID(b)
	if err != nil {
		t.Fatal(err)
	}
	if workflow.RequestWorkflowID(qa, "req-1") == workflow.RequestWorkflowID(qb, "req-1") {
		t.Fatalf("data dirs %s and %s share a workflow id for one request id", a, b)
	}
	again, err := requestQueueID(a)
	if err != nil || again != qa {
		t.Fatalf("queue id of one data dir changed: %q then %q (%v)", qa, again, err)
	}
	if workflow.RequestWorkflowID(qa, "req-1") == workflow.LegacyRequestWorkflowID("req-1") {
		t.Fatal("the new id equals the legacy id")
	}
}
