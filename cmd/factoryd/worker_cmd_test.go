package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"go.temporal.io/api/workflowservice/v1"
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

func TestQueueNamesAreStablePerDataDir(t *testing.T) {
	dataDir := t.TempDir()
	id, err := requestQueueID(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	jobs, light := taskQueuesFor(id)
	if jobs != "factoryd-jobs-"+id || light != "factoryd-light-"+id {
		t.Fatalf("queues = %q, %q", jobs, light)
	}
	if again, err := requestQueueID(dataDir); err != nil || again != id {
		t.Fatalf("second call = %q, %v; want %q", again, err, id)
	}
	if got, found, err := readRequestQueueID(dataDir); err != nil || !found || got != id {
		t.Fatalf("read = %q, %v, %v", got, found, err)
	}
	if other, err := requestQueueID(t.TempDir()); err != nil || other == id {
		t.Fatalf("two data dirs share queue id %q (%v)", id, err)
	}
	if _, found, err := readRequestQueueID(t.TempDir()); err != nil || found {
		t.Fatalf("a data dir with no worker has a queue id (%v, %v)", found, err)
	}
}

func TestQueueIDRejectsAnEmptyFile(t *testing.T) {
	dataDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dataDir, requestQueueIDFileName), []byte("\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := requestQueueID(dataDir); err == nil {
		t.Fatal("want an error for an empty queue id file")
	}
}

type recordingStarter struct {
	stubWorkflowTerminator // Describe: the legacy workflows that run
	signaled               []string
	signalErr              error
	ids                    []string
	options                []client.StartWorkflowOptions
	inputs                 []workflow.RequestWorkflowInput
	signals                []string
	startErrs              []error // consumed one per SignalWithStartWorkflow call
	describes              int
}

func (s *recordingStarter) DescribeWorkflowExecution(ctx context.Context, workflowID, runID string) (*workflowservice.DescribeWorkflowExecutionResponse, error) {
	s.describes++
	return s.stubWorkflowTerminator.DescribeWorkflowExecution(ctx, workflowID, runID)
}

func (s *recordingStarter) SignalWorkflow(_ context.Context, workflowID, _, _ string, _ interface{}) error {
	s.signaled = append(s.signaled, workflowID)
	return s.signalErr
}

func (s *recordingStarter) SignalWithStartWorkflow(_ context.Context, workflowID, signalName string, _ interface{}, options client.StartWorkflowOptions, _ interface{}, workflowArgs ...interface{}) (client.WorkflowRun, error) {
	s.ids = append(s.ids, workflowID)
	s.signals = append(s.signals, signalName)
	s.options = append(s.options, options)
	s.inputs = append(s.inputs, workflowArgs[0].(workflow.RequestWorkflowInput))
	if len(s.startErrs) > 0 {
		err := s.startErrs[0]
		s.startErrs = s.startErrs[1:]
		return nil, err
	}
	return nil, nil
}

func TestStartRequestWorkflowsSkipsFinishedRequests(t *testing.T) {
	dataDir := t.TempDir()
	saveRequestInState(t, dataDir, "req-review", request.StateSpecReview)
	saveRequestInState(t, dataDir, "req-halted", request.StateHalted)
	saveRequestInState(t, dataDir, "req-done", request.StateDone)
	saveRequestInState(t, dataDir, "req-cancelled", request.StateCancelled)
	saveRequestInState(t, dataDir, "req-closed", request.StateClosed)
	starter := &recordingStarter{}
	if err := startRequestWorkflows(context.Background(), starter, dataDir, "qid", "jobs-q", "light-q"); err != nil {
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

// tickLoop runs retryOnTicks over n ticks and returns once they are consumed.
func tickLoop(t *testing.T, c requestWorkflowStarter, dataDir string, n int) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	ticks := make(chan time.Time)
	done := make(chan struct{})
	go func() {
		retryOnTicks(ctx, c, ticks, dataDir, "qid", "jobs-q", "light-q")
		close(done)
	}()
	for range n {
		ticks <- time.Now()
	}
	// A sweep finishes before the loop takes another tick or sees ctx.Done.
	cancel()
	<-done
}

func markWake(t *testing.T, dataDir, id string) {
	t.Helper()
	if err := request.MarkWakePending(dataDir, id); err != nil {
		t.Fatal(err)
	}
}

func wakeMarkers(t *testing.T, dataDir string) []string {
	t.Helper()
	marks, err := request.ListWakePending(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, m := range marks {
		ids = append(ids, m.ID)
	}
	return ids
}

func TestFailedWakeAfterSaveLeavesAMarker(t *testing.T) {
	dp := newTestDeps(t)
	dataDir := t.TempDir()
	saveRequestInState(t, dataDir, "req-1", request.StateSubmitted)
	stubWake(dp, t, errors.New("temporal down"))
	var out bytes.Buffer
	wakeAfterSave(dp, context.Background(), &out, dataDir, "req-1")
	if got := wakeMarkers(t, dataDir); !slices.Equal(got, []string{"req-1"}) {
		t.Fatalf("markers %v, want req-1", got)
	}
	if !strings.Contains(out.String(), "temporal down") || !strings.Contains(out.String(), "within a minute") {
		t.Errorf("warning %q", out.String())
	}
	stubWake(dp, t, nil)
	if err := wakeAndMark(dp, context.Background(), dataDir, "req-2"); err != nil || len(wakeMarkers(t, dataDir)) != 1 {
		t.Fatalf("a successful wake must not mark: %v %v", err, wakeMarkers(t, dataDir))
	}
}

func TestTickWakesAMarkedRequestAndClearsItsMarker(t *testing.T) {
	dataDir := t.TempDir()
	saveRequestInState(t, dataDir, "req-late", request.StateSubmitted)
	markWake(t, dataDir, "req-late")
	starter := &recordingStarter{}
	tickLoop(t, starter, dataDir, 2)
	id := workflow.RequestWorkflowID("qid", "req-late")
	if !slices.Equal(starter.ids, []string{id}) || starter.signals[0] != workflow.RequestWakeSignalName {
		t.Fatalf("signal-with-start %v %v, want one for %q", starter.ids, starter.signals, id)
	}
	if in := starter.inputs[0]; in != (workflow.RequestWorkflowInput{RequestID: "req-late", JobsTaskQueue: "jobs-q", LightTaskQueue: "light-q"}) {
		t.Errorf("input %+v", in)
	}
	if got := wakeMarkers(t, dataDir); len(got) != 0 {
		t.Errorf("markers %v, want none", got)
	}
}

func TestTickSignalsAMarkedRequestAtAReviewGate(t *testing.T) {
	dataDir := t.TempDir()
	saveRequestInState(t, dataDir, "req-gate", request.StateSpecReview)
	markWake(t, dataDir, "req-gate")
	starter := &recordingStarter{}
	tickLoop(t, starter, dataDir, 1)
	if !slices.Equal(starter.ids, []string{workflow.RequestWorkflowID("qid", "req-gate")}) || starter.signals[0] != workflow.RequestWakeSignalName {
		t.Fatalf("signal-with-start %v %v", starter.ids, starter.signals)
	}
}

func TestTickKeepsTheMarkerWhenTheWakeFailsAndRetriesNextTick(t *testing.T) {
	dataDir := t.TempDir()
	saveRequestInState(t, dataDir, "req-a", request.StateSubmitted)
	markWake(t, dataDir, "req-a")
	starter := &recordingStarter{startErrs: []error{context.DeadlineExceeded}}
	tickLoop(t, starter, dataDir, 1)
	if got := wakeMarkers(t, dataDir); !slices.Equal(got, []string{"req-a"}) {
		t.Fatalf("markers %v, want the failed wake kept", got)
	}
	tickLoop(t, starter, dataDir, 1)
	if len(starter.ids) != 2 || len(wakeMarkers(t, dataDir)) != 0 {
		t.Fatalf("starts %v, markers %v; want a retry that cleared it", starter.ids, wakeMarkers(t, dataDir))
	}
}

func TestTickTouchesNothingWithoutAMarker(t *testing.T) {
	dataDir := t.TempDir()
	saveRequestInState(t, dataDir, "req-review", request.StateSpecReview)
	saveRequestInState(t, dataDir, "req-new", request.StateSubmitted)
	starter := &recordingStarter{}
	tickLoop(t, starter, dataDir, 3)
	if len(starter.ids) != 0 || len(starter.signaled) != 0 || starter.describes != 0 {
		t.Fatalf("starts %v, signals %v, describes %d; want none", starter.ids, starter.signaled, starter.describes)
	}
}

// A cancel at a review gate finishes the request; its workflow sleeps until it
// is woken, so the retry wakes a finished request too (seen live: the workflow
// slept out its reminder timer when the retry only cleared the marker).
func TestTickWakesAMarkedFinishedRequestAndClearsItsMarker(t *testing.T) {
	dataDir := t.TempDir()
	saveRequestInState(t, dataDir, "req-cancelled", request.StateCancelled)
	markWake(t, dataDir, "req-cancelled")
	starter := &recordingStarter{}
	tickLoop(t, starter, dataDir, 2)
	want := []string{workflow.RequestWorkflowID("qid", "req-cancelled")}
	if !slices.Equal(starter.ids, want) || len(wakeMarkers(t, dataDir)) != 0 {
		t.Fatalf("signal-with-start %v, want %v once; markers %v", starter.ids, want, wakeMarkers(t, dataDir))
	}
}

func TestStartupClearsTheMarkersItWoke(t *testing.T) {
	dataDir := t.TempDir()
	saveRequestInState(t, dataDir, "req-a", request.StateSubmitted)
	saveRequestInState(t, dataDir, "req-done", request.StateDone)
	markWake(t, dataDir, "req-a")
	markWake(t, dataDir, "req-done")
	starter := &recordingStarter{}
	if err := startRequestWorkflows(context.Background(), starter, dataDir, "qid", "jobs-q", "light-q"); err != nil {
		t.Fatal(err)
	}
	// req-a is unfinished; req-done is finished but marked, so it is woken too.
	if len(starter.ids) != 2 || len(wakeMarkers(t, dataDir)) != 0 {
		t.Fatalf("starts %v, markers %v", starter.ids, wakeMarkers(t, dataDir))
	}
	// A failed startup wake keeps the marker.
	markWake(t, dataDir, "req-a")
	starter.startErrs = []error{context.DeadlineExceeded}
	if err := startRequestWorkflows(context.Background(), starter, dataDir, "qid", "jobs-q", "light-q"); err == nil {
		t.Fatal("want the start error")
	}
	if got := wakeMarkers(t, dataDir); !slices.Equal(got, []string{"req-a"}) {
		t.Fatalf("markers %v, want req-a kept", got)
	}
}
