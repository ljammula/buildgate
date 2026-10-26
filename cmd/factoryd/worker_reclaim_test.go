package main

import (
	"buildgate/internal/unresolved"
	"context"
	"slices"
	"testing"

	"go.temporal.io/api/serviceerror"

	"buildgate/internal/request"
	"buildgate/internal/requestdriver/requestdrivertest"
	"buildgate/internal/run"
	"buildgate/internal/workflow"
)

// A run halted at worker start halts the request that was building it, so the
// request's workflow does not start that ticket over on its own.
func TestHaltRequestsOfLostRunsEntersResumeReviewOnlyForTheBuildingTicketsRequest(t *testing.T) {
	dp := newTestDeps(t)
	dataDir, reqID := requestdrivertest.BuildingFixture(dp, t, 1)
	r, err := request.Load(dataDir, reqID)
	if err != nil {
		t.Fatal(err)
	}
	r.Tickets[0].RunID = "run-lost"
	if err := (&run.Run{ID: "run-lost", State: run.StateHalted, KeptForResume: true}).Save(dataDir); err != nil {
		t.Fatal(err)
	}
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	haltRequestsOfLostRuns(dataDir, []*run.Run{
		{ID: "run-other", RequestID: reqID},
		{ID: "run-unowned"},
	})
	if got, _ := request.Load(dataDir, reqID); got.State != request.StateBuilding {
		t.Fatalf("state = %s after an unrelated run, want building", got.State)
	}
	if halted := haltRequestsOfLostRuns(dataDir, []*run.Run{{ID: "run-lost", RequestID: reqID}}); len(halted) != 1 || halted[0] != reqID {
		t.Fatalf("halted = %v, want [%s]", halted, reqID)
	}
	got, err := request.Load(dataDir, reqID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != request.StateResumeReview || got.Error != request.ResumePrompt(reqID, request.StateBuilding, request.LostStep{RunID: got.Resume.LostRunID, Cause: unresolved.WorkerLost}) {
		t.Fatalf("request = %s %q, want resume_review with the resume prompt", got.State, got.Error)
	}
	if got.Resume == nil || got.Resume.LostRunID != "run-lost" || got.Resume.Generation != 1 {
		t.Fatalf("Resume = %+v, want the lost run recorded", got.Resume)
	}
}

// A corrective round's run halted at worker start halts its pr_review request.
func TestHaltRequestsOfLostRunsEntersResumeReviewForAPRReviewRequest(t *testing.T) {
	dataDir := t.TempDir()
	saveRequestInState(t, dataDir, "req-pr", request.StatePRReview)
	halted := haltRequestsOfLostRuns(dataDir, []*run.Run{{ID: "run-round", RequestID: "req-pr"}})
	if len(halted) != 1 {
		t.Fatalf("halted = %v, want [req-pr]", halted)
	}
	if r, _ := request.Load(dataDir, "req-pr"); r.State != request.StateResumeReview || r.Resume.FromState != request.StatePRReview || r.Resume.LostRunID != "" {
		t.Fatalf("state = %s %+v, want resume_review from pr_review", r.State, r.Resume)
	}
}

// A run created before its request entered pr_review is a superseded attempt
// (a build halted before the ticket was rebuilt), not a lost round: reclaiming
// it at worker start leaves the request polling its pull request.
func TestHaltRequestsOfLostRunsLeavesAPRReviewRequestForARunFromBeforeIt(t *testing.T) {
	dataDir := t.TempDir()
	saveRequestInState(t, dataDir, "req-pr", request.StatePRReview)
	r, err := request.Load(dataDir, "req-pr")
	if err != nil {
		t.Fatal(err)
	}
	r.EnteredAt = "2026-10-20T03:50:00Z"
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	old := &run.Run{ID: "run-old", RequestID: "req-pr", CreatedAt: "2026-10-20T03:42:04Z"}
	if halted := haltRequestsOfLostRuns(dataDir, []*run.Run{old}); len(halted) != 0 {
		t.Fatalf("halted = %v, want none for a run from before pr_review", halted)
	}
	if got, _ := request.Load(dataDir, "req-pr"); got.State != request.StatePRReview {
		t.Fatalf("state = %s, want pr_review", got.State)
	}
	round := &run.Run{ID: "run-round", RequestID: "req-pr", CreatedAt: "2026-10-20T04:10:00Z"}
	if halted := haltRequestsOfLostRuns(dataDir, []*run.Run{round}); len(halted) != 1 {
		t.Fatalf("halted = %v, want [req-pr] for a round started in pr_review", halted)
	}
}

type recordingTerminator struct {
	ids []string
	err error
}

func (r *recordingTerminator) TerminateWorkflow(_ context.Context, workflowID, _, _ string, _ ...interface{}) error {
	r.ids = append(r.ids, workflowID)
	return r.err
}

func TestTerminateRequestWorkflowsEndsEachHaltedRequestsWorkflow(t *testing.T) {
	term := &recordingTerminator{err: serviceerror.NewNotFound("gone")}
	terminateRequestWorkflows(context.Background(), term, "q1", []string{"a", "b"})
	want := []string{workflow.RequestWorkflowID("q1", "a"), workflow.RequestWorkflowID("q1", "b")}
	if !slices.Equal(term.ids, want) {
		t.Fatalf("terminated %v, want %v", term.ids, want)
	}
}
