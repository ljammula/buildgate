package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"buildgate/internal/request"
	"buildgate/internal/requestdriver"
	"buildgate/internal/run"
	"buildgate/internal/workflow"
)

func TestRequestStepForEveryState(t *testing.T) {
	cfg := requestdriver.WorkerConfig{PrPollInterval: 5 * time.Minute, HitlReminderInterval: time.Hour}
	cases := []struct {
		state request.State
		want  workflow.RequestStep
	}{
		{request.StateSubmitted, workflow.RequestStep{Advance: workflow.RequestAdvanceLight}},
		{request.StateSpecDrafting, workflow.RequestStep{Advance: workflow.RequestAdvanceJobs}},
		{request.StateSpecReview, workflow.RequestStep{Wait: time.Hour, Remind: true}},
		{request.StateOracleDrafting, workflow.RequestStep{Advance: workflow.RequestAdvanceJobs}},
		{request.StateOracleReview, workflow.RequestStep{Wait: time.Hour, Remind: true}},
		{request.StatePlanning, workflow.RequestStep{Advance: workflow.RequestAdvanceJobs}},
		{request.StatePlanReview, workflow.RequestStep{Wait: time.Hour, Remind: true}},
		{request.StateBuilding, workflow.RequestStep{Advance: workflow.RequestAdvanceJobs}},
		{request.StatePRReview, workflow.RequestStep{Advance: workflow.RequestAdvanceJobs, Wait: 5 * time.Minute}},
		{request.StateResumeReview, workflow.RequestStep{Wait: time.Hour, Remind: true}},
		{request.StateHalted, workflow.RequestStep{}},
		{request.StateQuarantined, workflow.RequestStep{}},
		{request.StateDone, workflow.RequestStep{Done: true}},
		{request.StateCancelled, workflow.RequestStep{Done: true}},
	}
	for _, c := range cases {
		want := c.want
		want.State = string(c.state)
		want.RetryAfter = requestStepRetryAfter
		if c.state == request.StateBuilding {
			want.RetryAfter = requestRepoBusyRetryAfter
		}
		if got := requestStepFor(c.state, cfg); got != want {
			t.Errorf("requestStepFor(%s) = %+v, want %+v", c.state, got, want)
		}
	}
}

func TestAdvanceRequestActivityRunsOneStep(t *testing.T) {
	dp := newTestDeps(t)
	dataDir := t.TempDir()
	saveRequestInState(t, dataDir, "req-1", request.StateSubmitted)
	acts := &requestActivities{dp: dp, dataDir: dataDir}
	after, err := acts.AdvanceRequest(context.Background(), "req-1")
	if err != nil {
		t.Fatal(err)
	}
	r, err := request.Load(dataDir, "req-1")
	if err != nil {
		t.Fatal(err)
	}
	if r.State != request.StateSpecDrafting || after != string(request.StateSpecDrafting) {
		t.Fatalf("state = %s, returned %q; want spec_drafting", r.State, after)
	}
}

func TestAdvanceRequestActivityLeavesHumanWaitsAlone(t *testing.T) {
	dp := newTestDeps(t)
	dataDir := t.TempDir()
	saveRequestInState(t, dataDir, "req-1", request.StatePlanReview)
	acts := &requestActivities{dp: dp, dataDir: dataDir}
	after, err := acts.AdvanceRequest(context.Background(), "req-1")
	if err != nil {
		t.Fatal(err)
	}
	if after != string(request.StatePlanReview) {
		t.Fatalf("returned %q, want plan_review", after)
	}
}

func TestHaltLostRequestStepEntersResumeReviewOnlyInTheLostState(t *testing.T) {
	dp := newTestDeps(t)
	dataDir := t.TempDir()
	saveRequestInState(t, dataDir, "req-moved", request.StatePlanReview)
	saveRequestInState(t, dataDir, "req-lost", request.StatePlanning)
	acts := &requestActivities{dp: dp, dataDir: dataDir}
	for _, id := range []string{"req-moved", "req-lost"} {
		if err := acts.HaltLostRequestStep(context.Background(), workflow.HaltLostRequestStepInput{
			RequestID: id, State: string(request.StatePlanning),
		}); err != nil {
			t.Fatal(err)
		}
	}
	moved, err := request.Load(dataDir, "req-moved")
	if err != nil {
		t.Fatal(err)
	}
	if moved.State != request.StatePlanReview {
		t.Fatalf("moved request state = %s, want plan_review", moved.State)
	}
	lost, err := request.Load(dataDir, "req-lost")
	if err != nil {
		t.Fatal(err)
	}
	if want := request.ResumePrompt("req-lost", request.StatePlanning); lost.State != request.StateResumeReview || lost.Error != want {
		t.Fatalf("lost request = %s %q, want resume_review %q", lost.State, lost.Error, want)
	}
	if lost.Resume == nil || lost.Resume.FromState != request.StatePlanning || lost.Resume.Generation != 1 || lost.Resume.LostRunID != "" {
		t.Fatalf("Resume = %+v, want the lost planning step, generation 1, no run", lost.Resume)
	}
	if lost.NotifyCount != 1 || lost.LastNotifiedAt == "" {
		t.Errorf("reminder state = %d/%q, want the first reminder sent on entering", lost.NotifyCount, lost.LastNotifiedAt)
	}
	for _, want := range []string{"planning", "factoryd resume req-lost", "-from scratch", "factoryd cancel req-lost"} {
		if !strings.Contains(lost.Error, want) {
			t.Errorf("prompt %q lacks %q", lost.Error, want)
		}
	}
	if strings.Contains(lost.Error, "retry") {
		t.Errorf("prompt %q still offers retry", lost.Error)
	}
}

// A lost build records the run it was running, which a resume continues.
func TestHaltLostRequestStepRecordsTheLostBuildRun(t *testing.T) {
	dp := newTestDeps(t)
	dataDir, reqID := buildingFixture(dp, t, 1)
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
	acts := &requestActivities{dp: dp, dataDir: dataDir}
	if err := acts.HaltLostRequestStep(context.Background(), workflow.HaltLostRequestStepInput{
		RequestID: reqID, State: string(request.StateBuilding),
	}); err != nil {
		t.Fatal(err)
	}
	got, err := request.Load(dataDir, reqID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != request.StateResumeReview || got.Resume == nil || got.Resume.LostRunID != "run-lost" || got.Resume.FromState != request.StateBuilding {
		t.Fatalf("request = %s %+v, want resume_review of run-lost", got.State, got.Resume)
	}
}

func TestLoadRequestStepEndsWorkflowForMissingRequest(t *testing.T) {
	dp := newTestDeps(t)
	acts := &requestActivities{dp: dp, dataDir: t.TempDir()}
	step, err := acts.LoadRequestStep(context.Background(), "req-gone")
	if err != nil {
		t.Fatal(err)
	}
	if !step.Done {
		t.Fatalf("step = %+v, want Done", step)
	}
}

// A lost PR poll leaves the request in pr_review to poll again.
func TestHaltLostRequestStepLeavesPRReviewPolling(t *testing.T) {
	dp := newTestDeps(t)
	dataDir := t.TempDir()
	saveRequestInState(t, dataDir, "req-pr", request.StatePRReview)
	acts := &requestActivities{dp: dp, dataDir: dataDir}
	if err := acts.HaltLostRequestStep(context.Background(), workflow.HaltLostRequestStepInput{
		RequestID: "req-pr", State: string(request.StatePRReview),
	}); err != nil {
		t.Fatal(err)
	}
	if r, _ := request.Load(dataDir, "req-pr"); r.State != request.StatePRReview {
		t.Fatalf("state = %s, want pr_review", r.State)
	}
}
