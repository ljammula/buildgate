package requestdriver_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"buildgate/internal/release"
	"buildgate/internal/request"
	"buildgate/internal/requestdriver"
	"buildgate/internal/requestdriver/requestdrivertest"
	"buildgate/internal/run"
)

func oversizedTicketRunner(t *testing.T, dataDir, requestID, runID string) requestdriver.TicketRunner {
	t.Helper()
	return oversizedTicketRunnerForSize(t, dataDir, requestID, runID, run.ChangeSize{SourceLines: 1001, SourceFiles: 3, Functions: 4})
}

func oversizedTicketRunnerForSize(t *testing.T, dataDir, requestID, runID string, size run.ChangeSize) requestdriver.TicketRunner {
	t.Helper()
	return func(_ context.Context, _ []string, onReady func(*run.Run)) error {
		onReady(&run.Run{ID: runID, State: run.StateReady})
		record := &run.Run{
			ID: runID, RequestID: requestID, Project: "app", State: run.StateAccepted,
			BaseSHA: strings.Repeat("1", 40), ResultSHA: strings.Repeat("2", 40),
			ChangeSize:    &size,
			ReleasePolicy: &run.ReleasePolicy{MaxChangedLines: 1000, MaxFilesChanged: 25},
		}
		if err := record.Save(dataDir); err != nil {
			t.Fatal(err)
		}
		if err := release.SaveDecision(dataDir, release.Decision{RunID: runID, Project: "app", Allowed: false, Reasons: []string{fmt.Sprintf("diff changes %d source lines and %d source files", size.SourceLines, size.SourceFiles)}}); err != nil {
			t.Fatal(err)
		}
		return nil
	}
}

func TestTooManySourceFilesReturnsToPlanning(t *testing.T) {
	dataDir, id := requestdrivertest.ApprovedPlanningFixture(t, requestdrivertest.TwoCriteriaSpec, "make verify")
	requestdrivertest.PlanReviewOff(t, dataDir, id)
	driveWithSpecPlanner(t, dataDir, requestdrivertest.FailingPlanTicketsRunner(t))
	r := requestdrivertest.LoadRequest(t, dataDir, id)
	runner := oversizedTicketRunnerForSize(t, dataDir, id, "too-many-files", run.ChangeSize{SourceLines: 99, SourceFiles: 26})
	if err := requestdriver.AdvanceBuilding(newFakeDeps(t), context.Background(), dataDir, r, requestdriver.WorkerConfig{}, runner, time.Now()); err != nil {
		t.Fatal(err)
	}
	r = requestdrivertest.LoadRequest(t, dataDir, id)
	if r.State != request.StatePlanning || r.SizeReplan == nil || r.SizeReplan.Measurement.SourceFiles != 26 {
		t.Fatalf("request = %+v, want planning with the source-file measurement", r)
	}
}

func TestOversizedTicketReturnsToPlanningOnceAndThenHalts(t *testing.T) {
	dp := newFakeDeps(t)
	dataDir, id := requestdrivertest.ApprovedPlanningFixture(t, requestdrivertest.TwoCriteriaSpec, "make verify")
	requestdrivertest.PlanReviewOff(t, dataDir, id)
	driveWithSpecPlanner(t, dataDir, requestdrivertest.FailingPlanTicketsRunner(t))
	r := requestdrivertest.LoadRequest(t, dataDir, id)
	if err := requestdriver.AdvanceBuilding(dp, context.Background(), dataDir, r, requestdriver.WorkerConfig{}, oversizedTicketRunner(t, dataDir, id, "oversized-1"), time.Now()); err != nil {
		t.Fatal(err)
	}
	r = requestdrivertest.LoadRequest(t, dataDir, id)
	if r.State != request.StatePlanning || r.SizeReplan == nil || r.SizeReplan.RunID != "oversized-1" || r.SizeReplan.Measurement.SourceLines != 1001 {
		t.Fatalf("request = %+v, want planning with the measurement recorded", r)
	}
	if _, pinned := r.ApprovedSHA256["tickets/001.spec.md"]; pinned {
		t.Fatal("the previous ticket's approval remained pinned during replanning")
	}
	called := false
	job := func(_ context.Context, _ string, r *request.Request, _ requestdriver.WorkerConfig, _ string) ([]requestdriver.DraftedTicket, *request.PlanEvidence, error) {
		called = r.SizeReplan != nil && r.SizeReplan.Measurement.SourceFiles == 3
		return []requestdriver.DraftedTicket{{Filename: "001.spec.md", Content: requestdrivertest.ValidBrownfieldTicket("make verify", 1, 2)}}, nil, nil
	}
	if err := requestdriver.AdvancePlanning(dp, context.Background(), dataDir, r, requestdriver.WorkerConfig{}, job, time.Now()); err != nil {
		t.Fatal(err)
	}
	r = requestdrivertest.LoadRequest(t, dataDir, id)
	if !called || r.State != request.StatePlanReview {
		t.Fatalf("planning called %v, state %s; want the plan to wait for review", called, r.State)
	}
	if _, err := request.Approve(dataDir, id, "alice", time.Now(), nil); err != nil {
		t.Fatal(err)
	}
	r = requestdrivertest.LoadRequest(t, dataDir, id)
	if err := requestdriver.AdvanceBuilding(dp, context.Background(), dataDir, r, requestdriver.WorkerConfig{}, oversizedTicketRunner(t, dataDir, id, "oversized-2"), time.Now()); err != nil {
		t.Fatal(err)
	}
	r = requestdrivertest.LoadRequest(t, dataDir, id)
	if r.State != request.StateHalted || !strings.Contains(r.Error, "1001 source lines") {
		t.Fatalf("state %s, error %q; want a halt naming the measurement", r.State, r.Error)
	}
}

func TestOversizedHandedOverPlanHalts(t *testing.T) {
	dp := newFakeDeps(t)
	dataDir, id := requestdrivertest.BuildingFixture(dp, t, 1)
	r := requestdrivertest.LoadRequest(t, dataDir, id)
	r.PlanImported = true
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	if err := requestdriver.AdvanceBuilding(dp, context.Background(), dataDir, r, requestdriver.WorkerConfig{}, oversizedTicketRunner(t, dataDir, id, "oversized-import"), time.Now()); err != nil {
		t.Fatal(err)
	}
	r = requestdrivertest.LoadRequest(t, dataDir, id)
	if r.State != request.StateHalted || r.SizeReplan != nil || !strings.Contains(r.Error, fmt.Sprint(1001)) {
		t.Fatalf("state %s, replan %+v, error %q; want a halt with the measurement", r.State, r.SizeReplan, r.Error)
	}
}
