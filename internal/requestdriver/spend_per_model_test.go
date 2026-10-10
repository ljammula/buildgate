package requestdriver_test

import (
	"context"
	"reflect"
	"testing"
	"time"

	"buildgate/internal/api"
	"buildgate/internal/request"
	"buildgate/internal/requestdriver"
	"buildgate/internal/requestdriver/requestdrivertest"
)

// TestPlanRedraftOnAnotherModelKeepsEachModelsSpend covers a planning role
// whose model changes between drafts: a plan drafted on one model and
// rejected at plan_review, then redrafted on another. The cost rollup must
// show each model with what it spent, and the record's totals must still be
// the sum of both drafts.
func TestPlanRedraftOnAnotherModelKeepsEachModelsSpend(t *testing.T) {
	dp := newFakeDeps(t)
	dataDir, id := requestdrivertest.ApprovedPlanningFixture(t, requestdrivertest.TwoCriteriaSpec, "make verify")
	plan := []requestdriver.DraftedTicket{
		{Filename: "001.spec.md", Content: requestdrivertest.ValidBrownfieldTicket("make verify", 1, 2)},
	}
	drive := func(spend *request.JobSpend) {
		t.Helper()
		runner, _, _ := sequencedPlanTicketsRunner(t, dataDir,
			func() ([]requestdriver.DraftedTicket, *request.PlanEvidence, error) {
				return plan, &request.PlanEvidence{Model: spend.Model, Spend: spend}, nil
			},
		)
		if err := requestdrivertest.DriveRequests(dp, context.Background(), dataDir, requestdriver.WorkerConfig{}, requestdrivertest.FailingSpecDraftRunner(t), runner, requestdrivertest.FailingOracleDraftRunner(t), requestdrivertest.FailingBuildRunner(t)); err != nil {
			t.Fatalf("driveRequests: %v", err)
		}
	}

	drive(&request.JobSpend{Role: "planning", Model: "model-local", InputTokens: 9000, OutputTokens: 3000, At: time.Unix(1000, 0)})
	if _, err := request.Reject(dataDir, id, "alice", "plan it on the other model", time.Now()); err != nil {
		t.Fatalf("reject plan: %v", err)
	}
	drive(&request.JobSpend{Role: "planning", Model: "model-hosted", InputTokens: 30000, OutputTokens: 5000, CostMicroUSD: 14000, SpendPartial: true, At: time.Unix(2000, 0)})

	loaded, err := request.Load(dataDir, id)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.State != request.StatePlanReview {
		t.Fatalf("State = %q, want %q", loaded.State, request.StatePlanReview)
	}
	spend := loaded.PlanEvidence.Spend
	if spend.InputTokens != 39000 || spend.OutputTokens != 8000 || spend.CostMicroUSD != 14000 || !spend.SpendPartial {
		t.Errorf("PlanEvidence.Spend totals = %+v, want both drafts summed and partial", *spend)
	}

	cs := api.NewServer(dataDir).ComputeCostSummary(loaded)
	want := []api.ModelUsage{
		{Role: "planning", Model: "model-hosted", Tokens: 35000, CostMicroUSD: 14000},
		{Role: "planning", Model: "model-local", Tokens: 12000},
	}
	if !reflect.DeepEqual(cs.ByModel, want) {
		t.Errorf("ByModel = %+v, want one row per model that spent: %+v", cs.ByModel, want)
	}
	if cs.Plan != 0.014 {
		t.Errorf("Plan = %v, want 0.014 (both drafts' cost)", cs.Plan)
	}
}
