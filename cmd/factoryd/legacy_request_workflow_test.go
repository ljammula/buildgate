package main

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"buildgate/internal/request"
	"buildgate/internal/unresolved"
	"buildgate/internal/workflow"
)

func noHalt(string, string) error { return nil }

// A request in flight at an upgrade is still running under its legacy id on
// this data dir's light queue; the worker ends it. The same legacy id on
// another data dir's queue, a finished request and a closed workflow are left.
func TestRetireLegacyRequestWorkflowsEndsOnlyThisDataDirsRunningOnes(t *testing.T) {
	dataDir := t.TempDir()
	saveRequestInState(t, dataDir, "mine", request.StateSpecReview)
	saveRequestInState(t, dataDir, "theirs", request.StateSpecReview)
	saveRequestInState(t, dataDir, "closed-wf", request.StateSpecReview)
	saveRequestInState(t, dataDir, "fresh", request.StateSpecReview)
	saveRequestInState(t, dataDir, "finished", request.StateDone)
	id := workflow.LegacyRequestWorkflowID
	stub := &stubWorkflowTerminator{
		running: map[string]bool{id("mine"): true, id("theirs"): true, id("closed-wf"): false, id("finished"): true},
		queues:  map[string]string{id("mine"): "light-q", id("theirs"): "light-other", id("closed-wf"): "light-q", id("finished"): "light-q"},
	}
	skip, err := retireLegacyRequestWorkflows(context.Background(), stub, dataDir, "light-q", noHalt)
	if err != nil || len(skip) != 0 {
		t.Fatalf("skip = %v, err = %v; want none", skip, err)
	}
	if want := []string{id("mine")}; !slices.Equal(stub.terminated, want) {
		t.Fatalf("terminated %v, want %v", stub.terminated, want)
	}
}

// A drafting job has no run record, so only its workflow's lost-step halt
// stands between a stopped worker and a rerun with no human decision: retire
// puts the request in resume_review before it ends the workflow.
func TestRetireLegacyRequestWorkflowsHaltsAStepInFlight(t *testing.T) {
	dp := newTestDeps(t)
	dataDir := t.TempDir()
	saveRequestInState(t, dataDir, "drafting", request.StatePlanning)
	saveRequestInState(t, dataDir, "waiting", request.StateSpecReview)
	id := workflow.LegacyRequestWorkflowID
	stub := &stubWorkflowTerminator{
		running: map[string]bool{id("drafting"): true, id("waiting"): true},
		queues:  map[string]string{id("drafting"): "light-q", id("waiting"): "light-q"},
		started: map[string]bool{id("drafting"): true},
	}
	acts := &requestActivities{dp: dp, dataDir: dataDir}
	skip, err := retireLegacyRequestWorkflows(context.Background(), stub, dataDir, "light-q", acts.HaltLostRequestStepOf)
	if err != nil || len(skip) != 0 {
		t.Fatalf("skip = %v, err = %v", skip, err)
	}
	slices.Sort(stub.terminated)
	if want := []string{id("drafting"), id("waiting")}; !slices.Equal(stub.terminated, want) {
		t.Fatalf("terminated %v, want %v", stub.terminated, want)
	}
	drafting, err := request.Load(dataDir, "drafting")
	if err != nil {
		t.Fatal(err)
	}
	if want := request.ResumePrompt("drafting", request.StatePlanning, request.LostStep{Cause: unresolved.WorkerLost}); drafting.State != request.StateResumeReview || drafting.Error != want {
		t.Fatalf("drafting request = %s %q, want resume_review %q", drafting.State, drafting.Error, want)
	}
	if waiting, _ := request.Load(dataDir, "waiting"); waiting.State != request.StateSpecReview {
		t.Fatalf("a request with no step in flight moved to %s", waiting.State)
	}
}

// A Describe that fails, or a halt that fails, never stops the worker: the
// request's old workflow is left running and the request is not started.
func TestRetireLegacyRequestWorkflowsSkipsWhatItCannotCheck(t *testing.T) {
	dataDir := t.TempDir()
	saveRequestInState(t, dataDir, "unknown", request.StateSpecReview)
	saveRequestInState(t, dataDir, "unhaltable", request.StatePlanning)
	saveRequestInState(t, dataDir, "ok", request.StateSpecReview)
	id := workflow.LegacyRequestWorkflowID
	stub := &stubWorkflowTerminator{
		running:     map[string]bool{id("unhaltable"): true, id("ok"): true},
		queues:      map[string]string{id("unhaltable"): "light-q", id("ok"): "light-q"},
		started:     map[string]bool{id("unhaltable"): true},
		describeErr: map[string]error{id("unknown"): errors.New("temporal is slow")},
	}
	halt := func(string, string) error { return errors.New("disk full") }
	skip, err := retireLegacyRequestWorkflows(context.Background(), stub, dataDir, "light-q", halt)
	if err != nil {
		t.Fatalf("a lookup failure must not stop the worker: %v", err)
	}
	if !skip["unknown"] || !skip["unhaltable"] || skip["ok"] || len(skip) != 2 {
		t.Fatalf("skip = %v, want unknown and unhaltable", skip)
	}
	if want := []string{id("ok")}; !slices.Equal(stub.terminated, want) {
		t.Fatalf("terminated %v, want only %v", stub.terminated, want)
	}
}

// A wake signals a legacy workflow that still runs on this data dir's queue
// (an old worker is up), and starts the new id otherwise.
func TestWakeSignalsTheLegacyWorkflowWhileItRunsOnThisQueue(t *testing.T) {
	id := workflow.LegacyRequestWorkflowID
	s := &recordingStarter{stubWorkflowTerminator: stubWorkflowTerminator{
		running: map[string]bool{id("old"): true, id("other-dir"): true, id("closed"): false},
		queues:  map[string]string{id("old"): "light-q", id("other-dir"): "light-other", id("closed"): "light-q"},
	}}
	ctx := context.Background()
	for _, req := range []string{"old", "other-dir", "closed", "new"} {
		if err := wakeOrStartRequestWorkflow(ctx, s, "qid", req, "jobs-q", "light-q"); err != nil {
			t.Fatal(err)
		}
	}
	if want := []string{id("old")}; !slices.Equal(s.signaled, want) {
		t.Errorf("signalled %v, want %v", s.signaled, want)
	}
	want := []string{workflow.RequestWorkflowID("qid", "other-dir"), workflow.RequestWorkflowID("qid", "closed"), workflow.RequestWorkflowID("qid", "new")}
	if !slices.Equal(s.ids, want) {
		t.Errorf("signal-with-start ids %v, want %v", s.ids, want)
	}
}

func TestWakeFailsWhenTheLegacyWorkflowCannotBeLookedUp(t *testing.T) {
	id := workflow.LegacyRequestWorkflowID("r")
	s := &recordingStarter{stubWorkflowTerminator: stubWorkflowTerminator{describeErr: map[string]error{id: errors.New("down")}}}
	if err := wakeOrStartRequestWorkflow(context.Background(), s, "qid", "r", "jobs-q", "light-q"); err == nil || len(s.ids) != 0 {
		t.Fatalf("err = %v, started %v; want an error and no second driver", err, s.ids)
	}
}

// A data dir with no queue id never had a worker under ids that name it, so
// doctor ends the legacy id there, and does not create the queue id file.
func TestEndRequestWorkflowsWithNoQueueIDEndsTheLegacyID(t *testing.T) {
	dp := newTestDeps(t)
	stub := &stubWorkflowTerminator{}
	fakeTemporalOf(dp).dialTerminatorFn = func(context.Context, string) (workflowTerminator, func(), error) { return stub, func() {}, nil }
	dataDir := t.TempDir()
	var out strings.Builder
	endRequestWorkflows(dp, context.Background(), "stub:7233", dataDir, []string{"req-1"}, &out)
	if len(stub.terminated) != 1 || stub.terminated[0] != workflow.LegacyRequestWorkflowID("req-1") {
		t.Errorf("terminated = %v, want the legacy id", stub.terminated)
	}
	if _, found, err := readRequestQueueID(dataDir); err != nil || found {
		t.Errorf("doctor created a queue id (found=%v, err=%v)", found, err)
	}
}
