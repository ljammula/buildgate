package main

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"testing"

	"buildgate/internal/request"
	"buildgate/internal/workflow"
)

// legacyOn is a stub whose legacy workflows of the named requests run on queue.
func legacyOn(queue string, requests ...string) stubWorkflowTerminator {
	s := stubWorkflowTerminator{running: map[string]bool{}, queues: map[string]string{}}
	for _, r := range requests {
		s.running[workflow.LegacyRequestWorkflowID(r)] = true
		s.queues[workflow.LegacyRequestWorkflowID(r)] = queue
	}
	return s
}

func TestRequestDriverWorkflowIDIsLegacyOnlyWhileItRunsOnThisQueue(t *testing.T) {
	id := workflow.LegacyRequestWorkflowID
	stub := legacyOn("light-q", "old")
	stub.running[id("other-dir")], stub.queues[id("other-dir")] = true, "light-other"
	stub.running[id("closed")], stub.queues[id("closed")] = false, "light-q"
	for req, want := range map[string]string{
		"old":       id("old"),
		"other-dir": workflow.RequestWorkflowID("qid", "other-dir"),
		"closed":    workflow.RequestWorkflowID("qid", "closed"),
		"new":       workflow.RequestWorkflowID("qid", "new"),
	} {
		got, err := requestDriverWorkflowID(context.Background(), &stub, "qid", "light-q", req)
		if err != nil || got != want {
			t.Errorf("driver of %s = %q, %v; want %q", req, got, err, want)
		}
	}
	stub.describeErr = map[string]error{id("sick"): errors.New("temporal is slow")}
	if got, err := requestDriverWorkflowID(context.Background(), &stub, "qid", "light-q", "sick"); err == nil {
		t.Errorf("a Describe failure must be an error, got %q", got)
	}
}

// Two data dirs with one request id on one server get distinct driver ids.
func TestRequestDriverWorkflowIDsDifferPerDataDir(t *testing.T) {
	stub := &stubWorkflowTerminator{}
	a, err := requestDriverWorkflowID(context.Background(), stub, "qa", "light-a", "req-1")
	if err != nil {
		t.Fatal(err)
	}
	b, err := requestDriverWorkflowID(context.Background(), stub, "qb", "light-b", "req-1")
	if err != nil || a == b {
		t.Fatalf("ids %q and %q (%v)", a, b, err)
	}
}

// A wake signals a legacy workflow that still drives the request, and
// signal-with-starts the new id otherwise.
func TestWakeSignalsTheLegacyDriverAndStartsTheNewIDOtherwise(t *testing.T) {
	s := &recordingStarter{stubWorkflowTerminator: legacyOn("light-q", "old")}
	for _, req := range []string{"old", "new"} {
		if err := wakeOrStartRequestWorkflow(context.Background(), s, "qid", req, "jobs-q", "light-q"); err != nil {
			t.Fatal(err)
		}
	}
	if want := []string{workflow.LegacyRequestWorkflowID("old")}; !slices.Equal(s.signaled, want) {
		t.Errorf("signalled %v, want %v", s.signaled, want)
	}
	if want := []string{workflow.RequestWorkflowID("qid", "new")}; !slices.Equal(s.ids, want) {
		t.Errorf("signal-with-start ids %v, want %v", s.ids, want)
	}
}

func TestWakeFailsWhenTheDriverCannotBeLookedUp(t *testing.T) {
	s := &recordingStarter{stubWorkflowTerminator: stubWorkflowTerminator{describeErr: map[string]error{workflow.LegacyRequestWorkflowID("r"): errors.New("down")}}}
	if err := wakeOrStartRequestWorkflow(context.Background(), s, "qid", "r", "jobs-q", "light-q"); err == nil || len(s.ids) != 0 {
		t.Fatalf("err = %v, started %v; want an error and no second driver", err, s.ids)
	}
}

// Worker start leaves a request whose legacy workflow drives it alone: no
// second workflow, no halt, no termination, even for a request a lost step
// already put in resume_review. The legacy workflow is only woken.
func TestStartRequestWorkflowsLeavesALegacyDriverRunning(t *testing.T) {
	dataDir := t.TempDir()
	saveRequestInState(t, dataDir, "legacy", request.StateSpecReview)
	saveRequestInState(t, dataDir, "reclaimed", request.StateResumeReview)
	saveRequestInState(t, dataDir, "fresh", request.StatePlanReview)
	s := &recordingStarter{stubWorkflowTerminator: legacyOn("light-q", "legacy", "reclaimed")}
	if err := startRequestWorkflows(context.Background(), s, dataDir, "qid", "jobs-q", "light-q"); err != nil {
		t.Fatal(err)
	}
	slices.Sort(s.signaled)
	if want := []string{workflow.LegacyRequestWorkflowID("legacy"), workflow.LegacyRequestWorkflowID("reclaimed")}; !slices.Equal(s.signaled, want) {
		t.Errorf("signalled %v, want %v", s.signaled, want)
	}
	if want := []string{workflow.RequestWorkflowID("qid", "fresh")}; !slices.Equal(s.ids, want) {
		t.Errorf("started %v, want only %v", s.ids, want)
	}
	if len(s.terminated) != 0 {
		t.Errorf("terminated %v, want none", s.terminated)
	}
	if r, _ := request.Load(dataDir, "reclaimed"); r.State != request.StateResumeReview || r.Resume != nil && r.Resume.Generation > 1 {
		t.Errorf("reclaimed request changed: %s %+v", r.State, r.Resume)
	}
}

func TestStartRequestWorkflowsFailsWhenADriverCannotBeLookedUp(t *testing.T) {
	dataDir := t.TempDir()
	saveRequestInState(t, dataDir, "sick", request.StateSpecReview)
	s := &recordingStarter{stubWorkflowTerminator: stubWorkflowTerminator{describeErr: map[string]error{workflow.LegacyRequestWorkflowID("sick"): errors.New("down")}}}
	if err := startRequestWorkflows(context.Background(), s, dataDir, "qid", "jobs-q", "light-q"); err == nil {
		t.Fatal("start must fail loudly on a Describe error")
	}
}

// Halted requests have their driver ended, whichever workflow that is.
func TestTerminateRequestWorkflowsEndsTheDriver(t *testing.T) {
	stub := legacyOn("light-q", "old")
	terminateRequestWorkflows(context.Background(), &stub, "qid", "light-q", []string{"old", "new"})
	want := []string{workflow.LegacyRequestWorkflowID("old"), workflow.RequestWorkflowID("qid", "new")}
	if !slices.Equal(stub.terminated, want) {
		t.Fatalf("terminated %v, want %v", stub.terminated, want)
	}
}

func TestEndRequestWorkflowsEndsTheLegacyDriver(t *testing.T) {
	dp := newTestDeps(t)
	dataDir := t.TempDir()
	queueID, err := requestQueueID(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	_, light := taskQueuesFor(queueID)
	stub := legacyOn(light, "old")
	fakeTemporalOf(dp).dialTerminatorFn = func(context.Context, string) (workflowTerminator, func(), error) { return &stub, func() {}, nil }
	var out bytes.Buffer
	endRequestWorkflows(dp, context.Background(), "stub:7233", dataDir, []string{"old"}, &out)
	if want := []string{workflow.LegacyRequestWorkflowID("old")}; !slices.Equal(stub.terminated, want) || out.Len() != 0 {
		t.Errorf("terminated %v (output %q), want %v", stub.terminated, out.String(), want)
	}
}
