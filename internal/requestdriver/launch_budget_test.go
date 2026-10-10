package requestdriver_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"buildgate/internal/forge"
	"buildgate/internal/request"
	"buildgate/internal/requestdriver"
	"buildgate/internal/requestdriver/requestdrivertest"
	"buildgate/internal/run"
	"buildgate/internal/sessionconfig"
)

// TestCheckLaunchBudgetNoBudgetsSkipsScan covers checkLaunchBudget's own
// fast path: every one of the four keys at its 0/absent default must
// return immediately with no error and no check, even against a dataDir
// with no requests/runs directories at all (a scan would otherwise error
// on the missing "runs" directory the way monthToDateSpend's own
// os.ReadDir does for anything other than the not-exist case).
func TestCheckLaunchBudgetNoBudgetsSkipsScan(t *testing.T) {
	dataDir := t.TempDir()
	r := &request.Request{ID: "req-1"}
	check, reason, err := requestdriver.CheckLaunchBudget(dataDir, r, sessionconfig.Settings{}, time.Now())
	if err != nil {
		t.Fatalf("checkLaunchBudget: %v", err)
	}
	if check != "" || reason != "" {
		t.Fatalf("checkLaunchBudget = (%q, %q), want (\"\", \"\")", check, reason)
	}
}

// TestCheckLaunchBudgetRequestTokenExceeded covers the request-level
// token budget: a request whose own SpecEvidence.Spend already reached
// the configured request_token_budget must refuse the launch, naming
// that exact key.
func TestCheckLaunchBudgetRequestTokenExceeded(t *testing.T) {
	dataDir := t.TempDir()
	r := &request.Request{
		ID:           "req-1",
		SpecEvidence: &request.SpecEvidence{Spend: &request.JobSpend{InputTokens: 800, OutputTokens: 300, CostMicroUSD: 1_000_000}},
	}
	settings := sessionconfig.Settings{RequestTokenBudget: 1000}
	check, reason, err := requestdriver.CheckLaunchBudget(dataDir, r, settings, time.Now())
	if err != nil {
		t.Fatalf("checkLaunchBudget: %v", err)
	}
	if check != request.QuarantineCheckBudgetRequest {
		t.Fatalf("check = %q, want %q", check, request.QuarantineCheckBudgetRequest)
	}
	if !strings.Contains(reason, "request_token_budget") || !strings.Contains(reason, "1000") {
		t.Errorf("reason = %q, want it to name request_token_budget and its limit", reason)
	}
}

// TestCheckLaunchBudgetRequestCostExceeded is the cost-budget sibling of
// TestCheckLaunchBudgetRequestTokenExceeded.
func TestCheckLaunchBudgetRequestCostExceeded(t *testing.T) {
	dataDir := t.TempDir()
	r := &request.Request{
		ID:           "req-1",
		SpecEvidence: &request.SpecEvidence{Spend: &request.JobSpend{InputTokens: 10, OutputTokens: 10, CostMicroUSD: 5_000_000}},
	}
	settings := sessionconfig.Settings{RequestCostBudgetMicroUSD: 4_000_000}
	check, reason, err := requestdriver.CheckLaunchBudget(dataDir, r, settings, time.Now())
	if err != nil {
		t.Fatalf("checkLaunchBudget: %v", err)
	}
	if check != request.QuarantineCheckBudgetRequest {
		t.Fatalf("check = %q, want %q", check, request.QuarantineCheckBudgetRequest)
	}
	if !strings.Contains(reason, "request_cost_budget_micro_usd") || !strings.Contains(reason, "4000000") {
		t.Errorf("reason = %q, want it to name request_cost_budget_micro_usd and its limit", reason)
	}
}

// TestCheckLaunchBudgetUnderBudgetAllowsLaunch covers the ordinary case:
// spend below every configured budget returns no check at all.
func TestCheckLaunchBudgetUnderBudgetAllowsLaunch(t *testing.T) {
	dataDir := t.TempDir()
	r := &request.Request{
		ID:           "req-1",
		SpecEvidence: &request.SpecEvidence{Spend: &request.JobSpend{InputTokens: 10, OutputTokens: 10, CostMicroUSD: 100}},
	}
	settings := sessionconfig.Settings{RequestTokenBudget: 1000, RequestCostBudgetMicroUSD: 1_000_000}
	check, _, err := requestdriver.CheckLaunchBudget(dataDir, r, settings, time.Now())
	if err != nil {
		t.Fatalf("checkLaunchBudget: %v", err)
	}
	if check != "" {
		t.Fatalf("check = %q, want \"\" (spend is under every configured budget)", check)
	}
}

// TestCheckLaunchBudgetRequestCheckedBeforeMonthly covers the documented
// precedence: when both the request's own spend and the month's total
// spend already exceed their respective budgets, the request check wins.
func TestCheckLaunchBudgetRequestCheckedBeforeMonthly(t *testing.T) {
	dataDir := t.TempDir()
	r := &request.Request{
		ID:           "req-1",
		SubmittedAt:  time.Now().UTC().Format(time.RFC3339Nano),
		SpecEvidence: &request.SpecEvidence{Spend: &request.JobSpend{InputTokens: 2000, CostMicroUSD: 1, At: time.Now()}},
	}
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	settings := sessionconfig.Settings{RequestTokenBudget: 1000, MonthlyTokenBudget: 1}
	check, reason, err := requestdriver.CheckLaunchBudget(dataDir, r, settings, time.Now())
	if err != nil {
		t.Fatalf("checkLaunchBudget: %v", err)
	}
	if check != request.QuarantineCheckBudgetRequest {
		t.Fatalf("check = %q, want %q (request checked first)", check, request.QuarantineCheckBudgetRequest)
	}
	if !strings.Contains(reason, "request_token_budget") {
		t.Errorf("reason = %q, want it to name request_token_budget", reason)
	}
}

// TestAdvanceSpecDraftingQuarantinesWhenRequestBudgetExhausted covers the
// spec-drafting launch point directly: a request whose prior spend (a
// re-draft's already-recorded SpecEvidence.Spend) already reached
// request_token_budget must quarantine instead of running the drafting
// job again.
func TestAdvanceSpecDraftingQuarantinesWhenRequestBudgetExhausted(t *testing.T) {
	dataDir := t.TempDir()
	if err := request.SaveText(dataDir, "req-1", "text"); err != nil {
		t.Fatal(err)
	}
	r := request.New("req-1", "/repos/app", "app", request.Source{Kind: request.SourceText}, time.Now())
	r.State = request.StateSpecDrafting
	r.SpecEvidence = &request.SpecEvidence{Spend: &request.JobSpend{InputTokens: 1000, At: time.Now()}}
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}

	cfg := requestdriver.WorkerConfig{Settings: sessionconfig.Settings{RequestTokenBudget: 500}}
	if err := requestdriver.AdvanceSpecDrafting(context.Background(), dataDir, r, cfg, requestdrivertest.FailingSpecDraftRunner(t), time.Now()); err != nil {
		t.Fatalf("advanceSpecDrafting: %v", err)
	}
	if r.State != request.StateQuarantined {
		t.Fatalf("State = %q, want %q", r.State, request.StateQuarantined)
	}
	if r.QuarantineCheck != request.QuarantineCheckBudgetRequest {
		t.Errorf("QuarantineCheck = %q, want %q", r.QuarantineCheck, request.QuarantineCheckBudgetRequest)
	}
	if !strings.Contains(r.Error, "request_token_budget") {
		t.Errorf("Error = %q, want it to name request_token_budget", r.Error)
	}
}

// TestRunCorrectiveRoundQuarantinesWhenRequestBudgetExhausted covers the
// PR-review corrective-round launch point (pr_review_driver.go): a
// request already at its own budget must quarantine before
// buildTicketRunArgs/the corrective runner ever runs, even though
// maxReviewRounds has budget left.
func TestRunCorrectiveRoundQuarantinesWhenRequestBudgetExhausted(t *testing.T) {
	dp := newFakeDeps(t)
	r, dataDir := requestdrivertest.StubPRReviewTestFixture(t, 1)
	r.PlanEvidence = &request.PlanEvidence{Spend: &request.JobSpend{InputTokens: 5000, At: time.Now()}}
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	ticket := &r.Tickets[0]
	threads := []forge.Thread{{ID: "thread-1", Path: "a.go", Line: 1, Author: "dave", Body: "needs work", CommentID: 444}}

	origRunner := requestdriver.PrReviewCorrectiveRunner
	t.Cleanup(func() { requestdriver.PrReviewCorrectiveRunner = origRunner })
	calls := 0
	requestdriver.PrReviewCorrectiveRunner = func(ctx context.Context, args []string, onReady func(*run.Run)) error {
		calls++
		t.Fatal("prReviewCorrectiveRunner must not be called once the request budget is already exhausted")
		return nil
	}

	cfg := requestdriver.WorkerConfig{MaxReviewRounds: 1, Settings: sessionconfig.Settings{RequestTokenBudget: 1000}}
	if err := requestdriver.RunCorrectiveRound(dp, context.Background(), dataDir, r, ticket, threads, cfg, time.Now()); err != nil {
		t.Fatalf("runCorrectiveRound: %v", err)
	}
	if calls != 0 {
		t.Fatalf("prReviewCorrectiveRunner calls = %d, want 0", calls)
	}
	if r.State != request.StateQuarantined {
		t.Fatalf("State = %q, want %q", r.State, request.StateQuarantined)
	}
	if r.QuarantineCheck != request.QuarantineCheckBudgetRequest {
		t.Errorf("QuarantineCheck = %q, want %q", r.QuarantineCheck, request.QuarantineCheckBudgetRequest)
	}
}

// TestMonthToDateSpendSumsThisMonthOnlyAcrossRequestsAndDirectRuns covers
// monthToDateSpend's own contract: only this UTC calendar month's records
// count, drawn from both drafting JobSpend (keyed by JobSpend.At) and
// every run's own attempts (keyed by Attempt.StartedAt) -- including a
// bare run with no RequestID at all, since the monthly cap bounds total
// spend regardless of whether a request pipeline was involved.
func TestMonthToDateSpendSumsThisMonthOnlyAcrossRequestsAndDirectRuns(t *testing.T) {
	dataDir := t.TempDir()
	now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)

	// A request whose spec-drafting spend falls inside this month.
	inMonth := &request.Request{
		ID:          "req-in-month",
		SubmittedAt: now.Format(time.RFC3339Nano),
		SpecEvidence: &request.SpecEvidence{Spend: &request.JobSpend{
			InputTokens: 1000, OutputTokens: 500, CostMicroUSD: 2_000_000,
			At: now.Add(-24 * time.Hour),
		}},
	}
	if err := inMonth.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	// A request whose spend falls in the PRIOR month -- must be excluded.
	outOfMonth := &request.Request{
		ID:          "req-out-of-month",
		SubmittedAt: now.Format(time.RFC3339Nano),
		SpecEvidence: &request.SpecEvidence{Spend: &request.JobSpend{
			InputTokens: 9999, OutputTokens: 9999, CostMicroUSD: 99_000_000,
			At: now.AddDate(0, -1, 0),
		}},
	}
	if err := outOfMonth.Save(dataDir); err != nil {
		t.Fatal(err)
	}

	// A ticket-build run belonging to inMonth's own request, started this
	// month.
	requestdrivertest.SaveRunFixture(t, dataDir, "run-ticket", "req-in-month", now.Add(-time.Hour).Format(time.RFC3339), 300, 500_000)
	// A bare run with no RequestID at all, started this month -- must
	// still count toward the monthly total.
	requestdrivertest.SaveRunFixture(t, dataDir, "run-direct", "", now.Add(-2*time.Hour).Format(time.RFC3339), 200, 250_000)
	// A run started last month -- must be excluded.
	requestdrivertest.SaveRunFixture(t, dataDir, "run-last-month", "", now.AddDate(0, -1, 0).Format(time.RFC3339), 5000, 5_000_000)

	tokens, costMicroUSD, err := requestdriver.MonthToDateSpend(dataDir, now)
	if err != nil {
		t.Fatalf("monthToDateSpend: %v", err)
	}
	wantTokens := int64(1000+500) + int64(300) + int64(200)
	wantCostMicroUSD := int64(2_000_000) + int64(500_000) + int64(250_000)
	if tokens != wantTokens {
		t.Errorf("tokens = %d, want %d", tokens, wantTokens)
	}
	if costMicroUSD != wantCostMicroUSD {
		t.Errorf("costMicroUSD = %d, want %d", costMicroUSD, wantCostMicroUSD)
	}
}

// TestCheckLaunchBudgetMonthlyExceeded wires monthToDateSpend into
// checkLaunchBudget itself: a monthly budget already reached by prior
// runs (unrelated to r, the request about to launch) must quarantine
// with the monthly check even though r's own spend is 0.
func TestCheckLaunchBudgetMonthlyExceeded(t *testing.T) {
	dataDir := t.TempDir()
	// Mid-month, not time.Now(): in the first hour of a UTC month the
	// fixture's now-1h falls in the previous month and drops out of the
	// month-to-date sum.
	now := time.Date(2026, time.September, 15, 12, 0, 0, 0, time.UTC)
	requestdrivertest.SaveRunFixture(t, dataDir, "run-direct", "", now.Add(-time.Hour).Format(time.RFC3339), 5000, 5_000_000)

	r := &request.Request{ID: "req-1"}
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	settings := sessionconfig.Settings{MonthlyCostBudgetMicroUSD: 1_000_000}
	check, reason, err := requestdriver.CheckLaunchBudget(dataDir, r, settings, now)
	if err != nil {
		t.Fatalf("checkLaunchBudget: %v", err)
	}
	if check != request.QuarantineCheckBudgetMonthly {
		t.Fatalf("check = %q, want %q", check, request.QuarantineCheckBudgetMonthly)
	}
	if !strings.Contains(reason, "monthly_cost_budget_micro_usd") {
		t.Errorf("reason = %q, want it to name monthly_cost_budget_micro_usd", reason)
	}
}

// TestAdvancePlanningQuarantinesWhenRequestBudgetExhausted covers the
// planning launch point.
func TestAdvancePlanningQuarantinesWhenRequestBudgetExhausted(t *testing.T) {
	dataDir, id := requestdrivertest.ApprovedPlanningFixture(t, requestdrivertest.TwoCriteriaSpec, "make verify")
	r := requestdrivertest.LoadRequest(t, dataDir, id)
	r.SpecEvidence = &request.SpecEvidence{Spend: &request.JobSpend{CostMicroUSD: 10_000_000, At: time.Now()}}
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}

	cfg := requestdriver.WorkerConfig{Settings: sessionconfig.Settings{RequestCostBudgetMicroUSD: 1_000_000}}
	if err := requestdriver.AdvancePlanning(context.Background(), dataDir, r, cfg, requestdrivertest.FailingPlanTicketsRunner(t), time.Now()); err != nil {
		t.Fatalf("advancePlanning: %v", err)
	}
	if r.State != request.StateQuarantined {
		t.Fatalf("State = %q, want %q", r.State, request.StateQuarantined)
	}
	if r.QuarantineCheck != request.QuarantineCheckBudgetRequest {
		t.Errorf("QuarantineCheck = %q, want %q", r.QuarantineCheck, request.QuarantineCheckBudgetRequest)
	}
}

// TestAdvanceOracleDraftingQuarantinesWhenRequestBudgetExhausted covers
// the oracle-drafting launch point.
func TestAdvanceOracleDraftingQuarantinesWhenRequestBudgetExhausted(t *testing.T) {
	dataDir, id := requestdrivertest.OracleStageFixture(t, true)
	r := requestdrivertest.LoadRequest(t, dataDir, id)
	r.SpecEvidence = &request.SpecEvidence{Spend: &request.JobSpend{InputTokens: 1000, At: time.Now()}}
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}

	cfg := requestdrivertest.NoOracleScriptCfg
	cfg.Settings = sessionconfig.Settings{RequestTokenBudget: 1}
	runner := func(ctx context.Context, in requestdriver.OracleDraftInput) (request.OracleDraft, error) {
		t.Fatal("oracleDraftRunner must not be called once the request budget is already exhausted")
		return request.OracleDraft{}, nil
	}
	if err := requestdriver.AdvanceOracleDrafting(context.Background(), dataDir, r, cfg, runner, time.Now()); err != nil {
		t.Fatalf("advanceOracleDrafting: %v", err)
	}
	if r.State != request.StateQuarantined {
		t.Fatalf("State = %q, want %q", r.State, request.StateQuarantined)
	}
	if r.QuarantineCheck != request.QuarantineCheckBudgetRequest {
		t.Errorf("QuarantineCheck = %q, want %q", r.QuarantineCheck, request.QuarantineCheckBudgetRequest)
	}
}

// TestAdvanceBuildingQuarantinesWhenRequestBudgetExhausted covers the
// per-ticket build launch point: no build runner call at all once the
// request's own spend already reached its budget.
func TestAdvanceBuildingQuarantinesWhenRequestBudgetExhausted(t *testing.T) {
	dp := newFakeDeps(t)
	dataDir, id := requestdrivertest.BuildingFixture(dp, t, 1)
	r := requestdrivertest.LoadRequest(t, dataDir, id)
	r.PlanEvidence = &request.PlanEvidence{Spend: &request.JobSpend{InputTokens: 5000, At: time.Now()}}
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}

	cfg := requestdriver.WorkerConfig{Settings: sessionconfig.Settings{RequestTokenBudget: 1000}}
	if err := requestdrivertest.DriveRequests(dp, context.Background(), dataDir, cfg, requestdrivertest.FailingSpecDraftRunner(t), requestdrivertest.FailingPlanTicketsRunner(t), requestdrivertest.FailingOracleDraftRunner(t), requestdrivertest.FailingBuildRunner(t)); err != nil {
		t.Fatalf("driveRequests: %v", err)
	}

	loaded := requestdrivertest.LoadRequest(t, dataDir, id)
	if loaded.State != request.StateQuarantined {
		t.Fatalf("State = %q, want %q", loaded.State, request.StateQuarantined)
	}
	if loaded.QuarantineCheck != request.QuarantineCheckBudgetRequest {
		t.Errorf("QuarantineCheck = %q, want %q", loaded.QuarantineCheck, request.QuarantineCheckBudgetRequest)
	}
	if next := loaded.NextAction(); !strings.Contains(next, "request_token_budget") {
		t.Errorf("NextAction() = %q, want it to name request_token_budget", next)
	}
}

// TestAdvanceBuildingLaunchesWhenUnderBudget is the positive-path sibling:
// a configured but not-yet-reached budget must not block the launch.
func TestAdvanceBuildingLaunchesWhenUnderBudget(t *testing.T) {
	dp := newFakeDeps(t)
	dataDir, id := requestdrivertest.BuildingFixture(dp, t, 1)
	runner := requestdrivertest.AcceptingBuildRunner(t, dataDir)

	cfg := requestdriver.WorkerConfig{Settings: sessionconfig.Settings{RequestTokenBudget: 1_000_000, RequestCostBudgetMicroUSD: 1_000_000}}
	if err := requestdrivertest.DriveRequests(dp, context.Background(), dataDir, cfg, requestdrivertest.FailingSpecDraftRunner(t), requestdrivertest.FailingPlanTicketsRunner(t), requestdrivertest.FailingOracleDraftRunner(t), runner); err != nil {
		t.Fatalf("driveRequests: %v", err)
	}

	loaded := requestdrivertest.LoadRequest(t, dataDir, id)
	if loaded.State != request.StatePRReview {
		t.Fatalf("State = %q, want %q (an unreached budget must not block the launch)", loaded.State, request.StatePRReview)
	}
}

// TestTryReviewCorrectiveRoundQuarantinesWhenRequestBudgetExhausted
// covers the automatic spec_conformity corrective-round launch point
// (request_driver.go): a request already at its own budget must
// quarantine on the very first round attempt, before the corrective
// runner is ever called -- even though reviewCorrectiveRounds has budget
// left.
func TestTryReviewCorrectiveRoundQuarantinesWhenRequestBudgetExhausted(t *testing.T) {
	dp := newFakeDeps(t)
	dataDir, id := requestdrivertest.BuildingFixture(dp, t, 1)
	branch := "factoryd/" + id + "-001"
	baseSHA := fmt.Sprintf("%040d", 1)

	r := requestdrivertest.LoadRequest(t, dataDir, id)
	r.TicketIndex = 1
	r.PlanEvidence = &request.PlanEvidence{Spend: &request.JobSpend{InputTokens: 5000, At: time.Now()}}
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	ticket := &r.Tickets[0]
	runRecord := &run.Run{
		ID: "run-under-test", State: run.StateQuarantined, Branch: branch, BaseSHA: baseSHA,
		GateResults: []run.GateResult{
			{Check: "canonical_verify", Passed: true},
			{Check: "spec_conformity", Passed: false},
		},
		SpecConformityVerdicts: []run.ReviewVerdict{{Criterion: "1. x", Verdict: "flagged", Detail: "still not fixed"}},
	}

	calls, _ := requestdrivertest.StubReviewCorrectiveRunner(t, dataDir, func(dataDir, roundRunID string) *run.Run {
		return &run.Run{ID: roundRunID, State: run.StateAccepted}
	})

	cfg := requestdriver.WorkerConfig{ReviewCorrectiveRounds: 1, Settings: sessionconfig.Settings{RequestTokenBudget: 1000}}
	handled, err := requestdriver.TryReviewCorrectiveRound(dp, context.Background(), dataDir, r, ticket, runRecord, cfg, time.Now())
	if !handled {
		t.Fatal("tryReviewCorrectiveRound: handled = false, want true (budget exhausted quarantines)")
	}
	if err != nil {
		t.Fatalf("tryReviewCorrectiveRound: %v", err)
	}
	if *calls != 0 {
		t.Fatalf("reviewCorrectiveRunner calls = %d, want 0", *calls)
	}
	if r.State != request.StateQuarantined {
		t.Fatalf("State = %q, want %q", r.State, request.StateQuarantined)
	}
	if r.QuarantineCheck != request.QuarantineCheckBudgetRequest {
		t.Errorf("QuarantineCheck = %q, want %q", r.QuarantineCheck, request.QuarantineCheckBudgetRequest)
	}
}
