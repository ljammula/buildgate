package main

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/temporal"

	"buildgate/internal/request"
	"buildgate/internal/requestdriver"
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
	if r, _ := request.Load(dataDir, "reclaimed"); r.State != request.StateResumeReview || r.Resume != nil {
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

// The legacy driver can finish between the lookup and the signal; the wake
// then starts the new id, whose workflow finds the request done and returns.
func TestWakeFallsThroughToTheNewIDWhenTheLegacyDriverIsGone(t *testing.T) {
	s := &recordingStarter{stubWorkflowTerminator: legacyOn("light-q", "r"), signalErr: serviceerror.NewNotFound("gone")}
	if err := wakeOrStartRequestWorkflow(context.Background(), s, "qid", "r", "jobs-q", "light-q"); err != nil {
		t.Fatal(err)
	}
	if want := []string{workflow.RequestWorkflowID("qid", "r")}; !slices.Equal(s.ids, want) {
		t.Fatalf("started %v, want %v", s.ids, want)
	}
	s = &recordingStarter{stubWorkflowTerminator: legacyOn("light-q", "r"), signalErr: errors.New("down")}
	if err := wakeOrStartRequestWorkflow(context.Background(), s, "qid", "r", "jobs-q", "light-q"); err == nil || len(s.ids) != 0 {
		t.Fatalf("another signal failure must fail the wake without a second driver: %v, %v", err, s.ids)
	}
}

// A workflow that closed between the lookup and the terminate is fine; any
// other failure is returned, and doctor prints both ids.
func TestTerminateRequestWorkflowsReportsWhatItCannotEnd(t *testing.T) {
	stub := legacyOn("light-q", "gone")
	stub.terminateErr = serviceerror.NewNotFound("closed")
	if failed := terminateRequestWorkflows(context.Background(), &stub, "qid", "light-q", []string{"gone"}); len(failed) != 0 {
		t.Fatalf("a NotFound terminate is not a failure: %v", failed)
	}
	stub = legacyOn("light-q", "r")
	stub.terminateErr = errors.New("denied")
	stub.describeErr = map[string]error{workflow.LegacyRequestWorkflowID("sick"): errors.New("slow")}
	failed := terminateRequestWorkflows(context.Background(), &stub, "qid", "light-q", []string{"r", "sick"})
	if failed["r"] == nil || failed["sick"] == nil || len(failed) != 2 {
		t.Fatalf("failed = %v, want r and sick", failed)
	}
}

func TestEndRequestWorkflowsNamesBothIDsWhenTemporalIsUnreachable(t *testing.T) {
	dp := newTestDeps(t)
	dataDir := t.TempDir()
	queueID, err := requestQueueID(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	fakeTemporalOf(dp).dialTerminatorFn = func(context.Context, string) (workflowTerminator, func(), error) {
		return nil, nil, errors.New("temporal is down")
	}
	var out bytes.Buffer
	endRequestWorkflows(dp, context.Background(), "stub:7233", dataDir, []string{"r"}, &out)
	for _, id := range []string{workflow.RequestWorkflowID(queueID, "r"), workflow.LegacyRequestWorkflowID("r")} {
		if !strings.Contains(out.String(), "--workflow-id "+id) {
			t.Errorf("output %q lacks %s", out.String(), id)
		}
	}
}

// A Describe that fails in doctor reaches the operator, not only the log.
func TestEndRequestWorkflowsTellsTheOperatorWhenALookupFails(t *testing.T) {
	dp := newTestDeps(t)
	dataDir := t.TempDir()
	if _, err := requestQueueID(dataDir); err != nil {
		t.Fatal(err)
	}
	stub := &stubWorkflowTerminator{describeErr: map[string]error{workflow.LegacyRequestWorkflowID("r"): errors.New("slow")}}
	fakeTemporalOf(dp).dialTerminatorFn = func(context.Context, string) (workflowTerminator, func(), error) { return stub, func() {}, nil }
	var out bytes.Buffer
	endRequestWorkflows(dp, context.Background(), "stub:7233", dataDir, []string{"r"}, &out)
	if !strings.Contains(out.String(), "slow") || len(stub.terminated) != 0 {
		t.Errorf("output %q, terminated %v", out.String(), stub.terminated)
	}
}

// DescribeWorkflowExecution must fill the task queue of a running workflow:
// every legacy decision depends on it. Against the shared test Temporal server.
func TestDescribeFillsTheTaskQueueOfARunningWorkflow(t *testing.T) {
	c, err := client.Dial(client.Options{HostPort: sharedTemporalAddress(t)})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	queue := "factoryd-light-describe-" + t.Name()
	legacyID := workflow.LegacyRequestWorkflowID("describe-" + t.Name())
	run, err := c.ExecuteWorkflow(context.Background(), client.StartWorkflowOptions{ID: legacyID, TaskQueue: queue}, "NoSuchWorkflow")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.TerminateWorkflow(context.Background(), legacyID, run.GetRunID(), "test over") })
	desc, err := c.DescribeWorkflowExecution(context.Background(), legacyID, "")
	if err != nil {
		t.Fatal(err)
	}
	if got := desc.GetWorkflowExecutionInfo().GetTaskQueue(); got != queue {
		t.Fatalf("task queue = %q, want %q", got, queue)
	}
	for q, want := range map[string]string{queue: legacyID, "factoryd-light-elsewhere": workflow.RequestWorkflowID("qid", "describe-"+t.Name())} {
		got, err := requestDriverWorkflowID(context.Background(), c, "qid", q, "describe-"+t.Name())
		if err != nil || got != want {
			t.Errorf("driver on %s = %q, %v; want %q", q, got, err, want)
		}
	}
}

// Two AdvanceRequest calls for one request, as when an older binary started a
// second RequestWorkflow for it: one step runs, the other is refused with an
// error RequestWorkflow retries after a pause.
func TestAdvanceRequestRunsOneStepPerRequestAtATime(t *testing.T) {
	dp := newTestDeps(t)
	dataDir := t.TempDir()
	saveRequestInState(t, dataDir, "req-1", request.StateSpecDrafting)
	started := make(chan struct{})
	release := make(chan struct{})
	var runs atomic.Int32
	acts := &requestActivities{dp: dp, dataDir: dataDir,
		specRunner: func(context.Context, string, *request.Request, requestdriver.WorkerConfig) (string, *request.SpecEvidence, error) {
			if runs.Add(1) == 1 {
				close(started)
			}
			<-release
			return "", nil, errors.New("stop here")
		},
	}
	done := make(chan error, 1)
	go func() { _, err := acts.AdvanceRequest(context.Background(), "req-1"); done <- err }()
	<-started
	_, err := acts.AdvanceRequest(context.Background(), "req-1")
	var appErr *temporal.ApplicationError
	if !errors.As(err, &appErr) || appErr.Type() != stepInProgressErrorType {
		t.Fatalf("second step err = %v, want %s", err, stepInProgressErrorType)
	}
	close(release)
	<-done
	if n := runs.Load(); n != 1 {
		t.Fatalf("step ran %d times, want 1", n)
	}
	if !acts.steps.begin("req-1") {
		t.Fatal("the guard was not released when the step ended")
	}
	acts.steps.end("req-1")
}

// The guard is per request, and a step that panics still releases it.
func TestStepGuardIsPerRequestAndSurvivesAPanic(t *testing.T) {
	var g stepGuard
	if !g.begin("a") || !g.begin("b") {
		t.Fatal("a different request must run concurrently")
	}
	if g.begin("a") || !g.running("a") {
		t.Fatal("the same request must not")
	}
	g.end("a")
	g.end("b")
	func() {
		defer func() { _ = recover() }()
		defer g.end("p")
		g.begin("p")
		panic("step failed")
	}()
	if g.running("p") || !g.begin("p") {
		t.Fatal("a panic left the guard held")
	}
}

// HaltLostRequestStep leaves a request alone while its step still runs in this
// process (the step would save a stale request over the operator's resume),
// and halts it once the step has ended.
func TestHaltLostRequestStepWaitsForARunningStep(t *testing.T) {
	dp := newTestDeps(t)
	dataDir := t.TempDir()
	saveRequestInState(t, dataDir, "req-1", request.StatePlanning)
	acts := &requestActivities{dp: dp, dataDir: dataDir}
	in := workflow.HaltLostRequestStepInput{RequestID: "req-1", State: string(request.StatePlanning)}
	acts.steps.begin("req-1")
	if err := acts.HaltLostRequestStep(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	if r, _ := request.Load(dataDir, "req-1"); r.State != request.StatePlanning {
		t.Fatalf("halted while its step ran: %s", r.State)
	}
	acts.steps.end("req-1")
	if err := acts.HaltLostRequestStep(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	if r, _ := request.Load(dataDir, "req-1"); r.State != request.StateResumeReview {
		t.Fatalf("state = %s, want resume_review once the step ended", r.State)
	}
}
