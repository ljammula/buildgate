package requestdriver_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"buildgate/internal/request"
	"buildgate/internal/requestdriver"
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
	if err := requestdriver.AdvanceSpecDrafting(context.Background(), dataDir, r, cfg, failingSpecDraftRunner(t), time.Now()); err != nil {
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
