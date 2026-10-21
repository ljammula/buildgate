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

	"buildgate/internal/request"
	"buildgate/internal/requestdriver/requestdrivertest"
	"buildgate/internal/run"
	"buildgate/internal/triage"
)

// A build in flight is stopped by `factoryd cancel`: the step's context ends
// with cause request.ErrCancelled, the build records its run halted as
// request_cancelled with nothing kept, and the step's result never revives the
// cancelled request.
func TestCancelStopsTheInFlightBuildAndReapsItsWorktree(t *testing.T) {
	dp := newTestDeps(t)
	dataDir, id := requestdrivertest.BuildingFixture(dp, t, 1)
	started := make(chan string, 1)
	cause := make(chan error, 1)
	acts := &requestActivities{dp: dp, dataDir: dataDir, cancelPoll: 10 * time.Millisecond,
		buildRunner: func(ctx context.Context, args []string, onReady func(*run.Run)) error {
			runID := requestdrivertest.ArgValue(args, "-ticket")
			onReady(&run.Run{ID: runID, State: run.StateReady})
			if err := (&run.Run{ID: runID, State: run.StateSliceRunning, RequestID: id}).Save(dataDir); err != nil {
				t.Error(err)
			}
			started <- runID
			<-ctx.Done()
			cause <- context.Cause(ctx)
			// What runViaTemporal records when its wait ends this way.
			halted := &run.Run{ID: runID, State: run.StateHalted, RequestID: id, HaltConfirmed: true}
			recordRequestCancelled(halted, cancelledByOperator(ctx))
			if err := halted.Save(dataDir); err != nil {
				t.Error(err)
			}
			return ctx.Err()
		},
	}
	stepErr := make(chan error, 1)
	go func() {
		_, err := acts.AdvanceRequest(context.Background(), id)
		stepErr <- err
	}()
	var runID string
	select {
	case runID = <-started:
	case <-time.After(30 * time.Second):
		t.Fatal("the build never started")
	}

	unlock, err := request.Lock(dataDir, id)
	if err != nil {
		t.Fatal(err)
	}
	r, err := request.Load(dataDir, id)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Cancel("alice", "no longer wanted", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	unlock()

	select {
	case got := <-cause:
		if !errors.Is(got, request.ErrCancelled) {
			t.Errorf("the build's context cause = %v, want request.ErrCancelled", got)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the build was not stopped within 10 s of the cancel")
	}
	select {
	case <-stepErr:
	case <-time.After(30 * time.Second):
		t.Fatal("the step did not return after the build stopped")
	}

	after := requestdrivertest.LoadRequest(t, dataDir, id)
	if after.State != request.StateCancelled {
		t.Errorf("request state = %q after the stopped step, want cancelled: the result must not revive it", after.State)
	}
	halted, err := run.Load(dataDir, runID)
	if err != nil {
		t.Fatal(err)
	}
	if halted.State != run.StateHalted || halted.HaltReasonCode != run.HaltReasonRequestCancelled || halted.KeptForResume {
		t.Errorf("run = state %s code %q kept %v, want halted, request_cancelled, nothing kept", halted.State, halted.HaltReasonCode, halted.KeptForResume)
	}
	if got := triage.Run(halted, dataDir); !strings.Contains(got, "request was cancelled") {
		t.Errorf("triage = %q, want the cancel named as an operator's decision", got)
	}
}

func TestCancelWatcherEndsWithTheStepAndLeavesALiveRequestAlone(t *testing.T) {
	dataDir := t.TempDir()
	reviewWaitingSince(t, dataDir, request.StateBuilding, time.Minute)
	ctx, stop := cancelWhenRequestCancelled(context.Background(), dataDir, "req-1", 5*time.Millisecond)
	time.Sleep(60 * time.Millisecond)
	if ctx.Err() != nil {
		t.Fatalf("the step's context ended (%v) though its request is live", context.Cause(ctx))
	}
	stop()
	if ctx.Err() == nil {
		t.Error("the context outlives the step")
	}
	if cancelledByOperator(ctx) {
		t.Error("a step that ended on its own reads as cancelled by the operator")
	}
}

// awaitRunWorkflow on a run whose request was cancelled: the workflow is
// terminated and the build's Activity cancelled at once (its sandbox goes with
// it), the run is recorded halted with request_cancelled, and nothing is kept
// for a resume even though it is a request's run.
func TestAwaitRunWorkflowOfACancelledRequestRollsBackAndRecordsTheCode(t *testing.T) {
	dp := newTestDeps(t)
	dataDir := t.TempDir()
	seedOwnedRun(t, dataDir, "run-c", run.StateSliceRunning, os.Getpid(), "wf")
	seeded, err := run.Load(dataDir, "run-c")
	if err != nil {
		t.Fatal(err)
	}
	seeded.RequestID = "req-1"
	if err := seeded.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	var order []string
	fake := &runningWorkflowClient{order: &order}
	var logs bytes.Buffer
	log.SetOutput(&logs)
	defer log.SetOutput(os.Stderr)
	lifecycleCtx, cancelLifecycle := context.WithCancelCause(context.Background())
	defer cancelLifecycle(nil)
	runCtx, cancelRun := context.WithTimeout(lifecycleCtx, time.Hour)
	defer cancelRun()
	cancelLifecycle(request.ErrCancelled)
	r, err := run.Load(dataDir, "run-c")
	if err != nil {
		t.Fatal(err)
	}

	awaitErr := awaitRunWorkflow(dp, runCtx, fake, blockedExecution{}, r, runOptions{DataDir: dataDir, ID: "run-c", Ticket: "ticket", BaseSHA: "base"}, "factoryd-run-c",
		func() { order = append(order, "cancel-activities") })
	if awaitErr == nil {
		t.Fatal("awaitRunWorkflow returned nil for a cancelled wait")
	}
	got, err := run.Load(dataDir, "run-c")
	if err != nil {
		t.Fatal(err)
	}
	if got.State != run.StateHalted || got.HaltReasonCode != run.HaltReasonRequestCancelled || got.KeptForResume || !strings.Contains(got.HaltError, "request was cancelled") {
		t.Errorf("run = state %s code %q kept %v error %q, want halted as request_cancelled, nothing kept", got.State, got.HaltReasonCode, got.KeptForResume, got.HaltError)
	}
	if want := []string{"terminate", "cancel-activities"}; !slices.Equal(order, want) {
		t.Errorf("call order = %v, want %v", order, want)
	}
	if !strings.Contains(logs.String(), "its request was cancelled") || strings.Contains(logs.String(), "operator cancelled this run (SIGINT/SIGTERM)") {
		t.Errorf("log = %q, want the request's cancel named, not a signal", logs.String())
	}
}

func TestAWorktreeIsKeptOnlyWhenTheBuildWasLostNotCancelled(t *testing.T) {
	existing := t.TempDir()
	lost := errors.New("temporal contact lost")
	for _, c := range []struct {
		name                                          string
		keepEligible, cancelled, processLost, resumed bool
		contactLost                                   error
		want                                          bool
	}{
		{"a request's run lost with its process", true, false, true, false, nil, true},
		{"a resumed run", true, false, false, true, nil, true},
		{"temporal contact lost", true, false, false, false, lost, true},
		{"a supervisor timeout", true, false, false, false, nil, false},
		{"a run no human decides about", false, false, true, false, nil, false},
		{"its request was cancelled, though the process was stopping", true, true, true, false, nil, false},
		{"its request was cancelled while it resumed a kept worktree", true, true, false, true, nil, false},
	} {
		if got := keepsWorktree(c.keepEligible, c.cancelled, c.processLost, c.resumed, c.contactLost); got != c.want {
			t.Errorf("%s: keepsWorktree = %v, want %v", c.name, got, c.want)
		}
	}
	if !keptAtHalt(true, false, existing, true, false, false) {
		t.Error("a lost build's worktree is not flagged kept")
	}
	if keptAtHalt(true, true, existing, true, true, true) {
		t.Error("a cancelled request's worktree is flagged kept for a resume")
	}
	if keptAtHalt(true, false, "", true, false, false) || keptAtHalt(true, false, existing+"/gone", true, false, false) {
		t.Error("a worktree that does not exist is flagged kept")
	}
	if !stopsBuildNow(false, true) || !stopsBuildNow(true, false) || stopsBuildNow(false, false) {
		t.Error("stopsBuildNow: a cancelled request or a stopping process cancels the build's Activity, a timeout does not")
	}
}
