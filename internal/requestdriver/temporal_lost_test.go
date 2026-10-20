package requestdriver_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"buildgate/internal/request"
	"buildgate/internal/requestdriver"
	"buildgate/internal/requestdriver/requestdrivertest"
	"buildgate/internal/run"
	"buildgate/internal/unresolved"
)

// A build whose step lost Temporal: the step's context is cancelled, the run
// is saved halted with its worktree kept and the cause, and the runner
// returns the cancellation. The request waits in resume_review, once, and a
// second step finds nothing to run.
func TestABuildLostToTemporalEntersResumeReviewOnceAndNeverRebuilds(t *testing.T) {
	dp := newFakeDeps(t)
	dataDir, id := requestdrivertest.BuildingFixture(dp, t, 1)
	ctx, cancel := context.WithCancelCause(context.Background())
	runnerCalls := 0
	lost := func(_ context.Context, args []string, onReady func(*run.Run)) error {
		runnerCalls++
		rid := requestdrivertest.ArgValue(args, "-ticket")
		onReady(&run.Run{ID: rid, State: run.StateReady})
		cancel(errors.New("no heartbeat reached the Temporal server"))
		saved := &run.Run{ID: rid, State: run.StateHalted, KeptForResume: true, RequestID: id,
			HaltReasonCode: string(unresolved.TemporalUnreachable), HaltError: "lost contact with the Temporal server"}
		if err := saved.Save(dataDir); err != nil {
			t.Fatal(err)
		}
		return context.Cause(ctx)
	}

	step := func(runner requestdriver.TicketRunner) *request.Request {
		t.Helper()
		r, err := request.Load(dataDir, id)
		if err != nil {
			t.Fatal(err)
		}
		if requestdriver.RequestDriverOwnsState(r.State) {
			if err := requestdriver.AdvanceRequest(dp, ctx, dataDir, r, requestdriver.WorkerConfig{}, nil, nil, nil, runner); err != nil {
				t.Fatalf("AdvanceRequest: %v", err)
			}
		}
		got, err := request.Load(dataDir, id)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}

	got := step(lost)
	if got.State != request.StateResumeReview || got.Resume == nil || got.Resume.Cause != unresolved.TemporalUnreachable {
		t.Fatalf("state %s resume %+v, want resume_review with cause temporal_unreachable", got.State, got.Resume)
	}
	if got.Resume.LostRunID != requestdrivertest.TicketRunID(id, 1) {
		t.Errorf("LostRunID = %q, want the kept run", got.Resume.LostRunID)
	}
	if got.NotifyCount != 1 {
		t.Errorf("NotifyCount = %d, want the one reminder", got.NotifyCount)
	}

	again := step(requestdrivertest.FailingBuildRunner(t))
	if again.State != request.StateResumeReview || again.NotifyCount != 1 || again.Resume.Generation != got.Resume.Generation {
		t.Errorf("a second step changed the request: state %s notify %d generation %d", again.State, again.NotifyCount, again.Resume.Generation)
	}
	if runnerCalls != 1 {
		t.Errorf("the runner was called %d times, want once", runnerCalls)
	}
}

// A request still in building whose ticket's run halted with no verdict
// (the step that found it failed before it could move the request, and
// Temporal ran it again) is moved to resume_review, not built over.
func TestAHaltedUnresolvedTicketIsNeverRebuiltWithoutADecision(t *testing.T) {
	for _, cause := range []unresolved.Cause{unresolved.TemporalUnreachable, unresolved.WorkerLost, unresolved.SandboxLaunch} {
		for _, kept := range []bool{true, false} {
			dp := newFakeDeps(t)
			dataDir, id := requestdrivertest.BuildingFixture(dp, t, 1)
			r, err := request.Load(dataDir, id)
			if err != nil {
				t.Fatal(err)
			}
			r.TicketIndex = 1
			r.Tickets[0].RunID = "halted-run"
			if err := r.Save(dataDir); err != nil {
				t.Fatal(err)
			}
			if err := (&run.Run{ID: "halted-run", State: run.StateHalted, KeptForResume: kept, RequestID: id, HaltReasonCode: string(cause)}).Save(dataDir); err != nil {
				t.Fatal(err)
			}

			got := requestdrivertest.AdvanceBuildingOnce(dp, t, dataDir, id, requestdriver.ResumeGate{}, requestdrivertest.FailingBuildRunner(t))
			if got.State != request.StateResumeReview || got.Resume == nil || got.Resume.Cause != cause {
				t.Errorf("%s kept=%v: state %s resume %+v, want resume_review with the run's cause", cause, kept, got.State, got.Resume)
			}
			if rec, err := run.Load(dataDir, "halted-run"); err != nil || rec.KeptForResume != kept {
				t.Errorf("%s kept=%v: the halted run was touched: %+v (%v)", cause, kept, rec, err)
			}
		}
	}
}

// A decision about the halted run is what lets a build start: from scratch
// it rebuilds.
func TestAHaltedUnresolvedTicketIsRebuiltOnceAHumanDecidesFromScratch(t *testing.T) {
	dp := newFakeDeps(t)
	dataDir, id := requestdrivertest.BuildingFixture(dp, t, 1)
	r, err := request.Load(dataDir, id)
	if err != nil {
		t.Fatal(err)
	}
	r.TicketIndex = 1
	r.Tickets[0].RunID = "halted-run"
	if err := (&run.Run{ID: "halted-run", State: run.StateHalted, RequestID: id, HaltReasonCode: string(unresolved.TemporalUnreachable)}).Save(dataDir); err != nil {
		t.Fatal(err)
	}
	if err := r.EnterResumeReview(request.StateBuilding, request.LostStep{RunID: "halted-run", Cause: unresolved.TemporalUnreachable}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ResumeDecide(request.ResumeScratch, "alice", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	calls := 0
	runner := func(ctx context.Context, args []string, onReady func(*run.Run)) error {
		calls++
		return requestdrivertest.AcceptingBuildRunner(t, dataDir)(ctx, args, onReady)
	}

	requestdrivertest.AdvanceBuildingOnce(dp, t, dataDir, id, requestdriver.ResumeGate{}, runner)
	if calls != 1 {
		t.Fatalf("the runner was called %d times after a from-scratch decision, want once", calls)
	}
}

// The server timed the step out while its process lived on, so the request
// was moved to resume_review as a lost worker with nothing kept; the run then
// halts for losing Temporal. The request keeps its place, and its cause and
// kept run become the run's own.
func TestARequestParkedAsALostWorkerEndsWithTheCauseOfItsRun(t *testing.T) {
	dp := newFakeDeps(t)
	dataDir, id := requestdrivertest.BuildingFixture(dp, t, 1)
	runner := func(_ context.Context, args []string, onReady func(*run.Run)) error {
		rid := requestdrivertest.ArgValue(args, "-ticket")
		onReady(&run.Run{ID: rid, State: run.StateReady})
		// HaltLostRequestStep, while the build still ran.
		r, err := request.Load(dataDir, id)
		if err != nil {
			t.Fatal(err)
		}
		if err := r.EnterResumeReview(request.StateBuilding, request.LostStep{Cause: unresolved.WorkerLost}, time.Now()); err != nil {
			t.Fatal(err)
		}
		if err := r.Save(dataDir); err != nil {
			t.Fatal(err)
		}
		if err := (&run.Run{ID: rid, State: run.StateHalted, KeptForResume: true, RequestID: id,
			HaltReasonCode: string(unresolved.TemporalUnreachable)}).Save(dataDir); err != nil {
			t.Fatal(err)
		}
		return errors.New("lost contact with the Temporal server")
	}

	got := requestdrivertest.AdvanceBuildingOnce(dp, t, dataDir, id, requestdriver.ResumeGate{}, runner)
	if got.State != request.StateResumeReview || got.Resume == nil || got.Resume.Cause != unresolved.TemporalUnreachable {
		t.Fatalf("state %s resume %+v, want resume_review with cause temporal_unreachable", got.State, got.Resume)
	}
	if got.Resume.LostRunID != requestdrivertest.TicketRunID(id, 1) || got.Resume.Generation != 2 {
		t.Errorf("resume %+v, want the kept run and the same generation", got.Resume)
	}
	if want := request.ResumePrompt(id, request.StateBuilding, request.LostStep{RunID: got.Resume.LostRunID, Cause: unresolved.TemporalUnreachable}); got.Error != want {
		t.Errorf("prompt %q, want %q", got.Error, want)
	}
}

// A decision already pending on the entry is never rewritten under the human:
// the cause, the kept run and the generation stay as the decision saw them.
func TestAParkedRequestWithAPendingDecisionKeepsItsEntryWhenItsRunHalts(t *testing.T) {
	dp := newFakeDeps(t)
	dataDir, id := requestdrivertest.BuildingFixture(dp, t, 1)
	runner := func(_ context.Context, args []string, onReady func(*run.Run)) error {
		rid := requestdrivertest.ArgValue(args, "-ticket")
		onReady(&run.Run{ID: rid, State: run.StateReady})
		r, err := request.Load(dataDir, id)
		if err != nil {
			t.Fatal(err)
		}
		if err := r.EnterResumeReview(request.StateBuilding, request.LostStep{Cause: unresolved.WorkerLost}, time.Now()); err != nil {
			t.Fatal(err)
		}
		r.ResumeDecision = &request.ResumeDecision{Verb: request.ResumeScratch, Generation: r.Resume.Generation, By: "alice"}
		if err := r.Save(dataDir); err != nil {
			t.Fatal(err)
		}
		if err := (&run.Run{ID: rid, State: run.StateHalted, KeptForResume: true, RequestID: id,
			HaltReasonCode: string(unresolved.TemporalUnreachable)}).Save(dataDir); err != nil {
			t.Fatal(err)
		}
		return errors.New("lost contact with the Temporal server")
	}

	got := requestdrivertest.AdvanceBuildingOnce(dp, t, dataDir, id, requestdriver.ResumeGate{}, runner)
	if got.Resume == nil || got.Resume.Cause != unresolved.WorkerLost || got.Resume.LostRunID != "" || got.Resume.Generation != 1 {
		t.Errorf("resume %+v, want the entry the decision answers, untouched", got.Resume)
	}
	if got.PendingResumeVerb() != request.ResumeScratch {
		t.Errorf("the pending decision was lost: %+v", got.ResumeDecision)
	}
}
