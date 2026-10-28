package requestdriver

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"buildgate/internal/api"
	"buildgate/internal/request"
	"buildgate/internal/run"
	"buildgate/internal/sessionconfig"
)

// CheckLaunchBudget is the one gate every job-launch point in the request
// driver (AdvanceSpecDrafting, AdvancePlanning,
// AdvanceBuilding, runCorrectiveRounds in request_driver.go, plus
// RunCorrectiveRound in pr_review_driver.go) calls immediately before
// starting a container -- see this file's own package doc comment on M3-C2
// for why a host-side check is needed at all: the relay's own
// MeterTokenCeiling/MeterCostCeilingMicroUSD (sessionconfig.Settings.
// EffectiveRelayCeilings) bound a single job, but each corrective round
// gets a fresh full ceiling, so nothing before this bounded a request's
// (or an operator's monthly) total spend.
//
// check is "" when the launch may proceed. Otherwise it is
// request.QuarantineCheckBudgetRequest or
// request.QuarantineCheckBudgetMonthly, and reason names the exact key,
// the spend to date, and the configured limit -- the caller passes both
// straight into quarantineRequestWithCheck without launching anything.
//
// The request budget is checked before the monthly one: a request that
// has already spent past its own per-request cap is the more specific,
// more actionable diagnosis even when the monthly cap would also have
// caught it.
//
// All four budget keys are 0 (absent) by default, meaning unlimited --
// when every one of them is 0, this returns immediately with no disk
// scan at all, so an operator who never opts into budgets pays nothing
// for this check on every launch.
func CheckLaunchBudget(dataDir string, r *request.Request, settings sessionconfig.Settings, now time.Time) (check string, reason string, err error) {
	if settings.RequestTokenBudget <= 0 && settings.RequestCostBudgetMicroUSD <= 0 &&
		settings.MonthlyTokenBudget <= 0 && settings.MonthlyCostBudgetMicroUSD <= 0 {
		return "", "", nil
	}

	if settings.RequestTokenBudget > 0 || settings.RequestCostBudgetMicroUSD > 0 {
		// api.Server.ComputeCostSummary is the one existing rollup of a
		// request's total spend (drafting + every ticket build + every
		// corrective round, conformity or PR-review) -- reused here
		// rather than re-walking the same evidence a second way, so this
		// check can never disagree with `factoryd cost`/the console's own
		// figure for the same request.
		cs := api.NewServer(dataDir).ComputeCostSummary(r)
		tokens := cs.Tokens
		costMicroUSD := costSummaryMicroUSD(cs)
		if settings.RequestTokenBudget > 0 && tokens >= int64(settings.RequestTokenBudget) {
			return request.QuarantineCheckBudgetRequest,
				budgetExhaustedReason("request", tokens, costMicroUSD, "request_token_budget", int64(settings.RequestTokenBudget)),
				nil
		}
		if settings.RequestCostBudgetMicroUSD > 0 && costMicroUSD >= settings.RequestCostBudgetMicroUSD {
			return request.QuarantineCheckBudgetRequest,
				budgetExhaustedReason("request", tokens, costMicroUSD, "request_cost_budget_micro_usd", settings.RequestCostBudgetMicroUSD),
				nil
		}
	}

	if settings.MonthlyTokenBudget > 0 || settings.MonthlyCostBudgetMicroUSD > 0 {
		tokens, costMicroUSD, merr := MonthToDateSpend(dataDir, now)
		if merr != nil {
			return "", "", merr
		}
		if settings.MonthlyTokenBudget > 0 && tokens >= int64(settings.MonthlyTokenBudget) {
			return request.QuarantineCheckBudgetMonthly,
				budgetExhaustedReason("monthly", tokens, costMicroUSD, "monthly_token_budget", int64(settings.MonthlyTokenBudget)),
				nil
		}
		if settings.MonthlyCostBudgetMicroUSD > 0 && costMicroUSD >= settings.MonthlyCostBudgetMicroUSD {
			return request.QuarantineCheckBudgetMonthly,
				budgetExhaustedReason("monthly", tokens, costMicroUSD, "monthly_cost_budget_micro_usd", settings.MonthlyCostBudgetMicroUSD),
				nil
		}
	}

	return "", "", nil
}

// costSummaryMicroUSD sums cs.ByModel's own exact per-(role,model)
// CostMicroUSD figures -- the same integer components
// api.ComputeCostSummary's modelUsageAccumulator already added up -- rather
// than round-tripping through cs.Total's float64 dollars (cs.Total *
// 1e6), so a launch-budget comparison never disagrees with itself by a
// rounding cent CostSummary's own display path never notices.
func costSummaryMicroUSD(cs api.CostSummary) int64 {
	var total int64
	for _, m := range cs.ByModel {
		total += m.CostMicroUSD
	}
	return total
}

// budgetExhaustedReason renders CheckLaunchBudget's own quarantine reason:
// scope is "request" or "monthly", key is the exact session-config key
// that tripped, limit is that key's own configured value.
func budgetExhaustedReason(scope string, tokens, costMicroUSD int64, key string, limit int64) string {
	return fmt.Sprintf("%s spend $%.2f (%d tokens) reached %s %d", scope, float64(costMicroUSD)/1e6, tokens, key, limit)
}

// MonthToDateSpend sums every relay-measured spend recorded anywhere under
// dataDir whose own timestamp falls within now's calendar month (UTC):
// drafting JobSpend (spec/plan) across every request in
// <data-dir>/requests, keyed by JobSpend.At, plus every run attempt's own
// RelayConsumed* figures across every run in <data-dir>/runs, keyed by
// Attempt.StartedAt -- including a bare run with no RequestID at all,
// since the monthly cap is meant to bound total spend regardless of
// whether a request pipeline was involved.
//
// Deliberately O(records) with no cache (see this file's own package doc
// comment on why): a data dir large enough for this scan to matter is
// also large enough that a stale cache would be the more dangerous
// failure mode for a spend-limiting check.
//
// A run directory that cannot be loaded (no run.json yet -- see
// run.FindChainSuccessor's identical reasoning -- or any other read
// error) is skipped rather than failing the whole scan: an unrelated
// corrupt or in-progress record must never block every future launch's
// budget check.
func MonthToDateSpend(dataDir string, now time.Time) (tokens int64, costMicroUSD int64, err error) {
	monthStart := time.Date(now.UTC().Year(), now.UTC().Month(), 1, 0, 0, 0, 0, time.UTC)
	monthEnd := monthStart.AddDate(0, 1, 0)
	inMonth := func(t time.Time) bool {
		return !t.Before(monthStart) && t.Before(monthEnd)
	}

	requests, err := request.List(dataDir)
	if err != nil {
		return 0, 0, fmt.Errorf("list requests for monthly budget: %w", err)
	}
	addJobSpend := func(sp *request.JobSpend) {
		if sp == nil || sp.At.IsZero() || !inMonth(sp.At.UTC()) {
			return
		}
		tokens += sp.InputTokens + sp.OutputTokens
		costMicroUSD += sp.CostMicroUSD
	}
	for _, req := range requests {
		if req.SpecEvidence != nil {
			addJobSpend(req.SpecEvidence.Spend)
		}
		if req.PlanEvidence != nil {
			addJobSpend(req.PlanEvidence.Spend)
		}
	}

	entries, err := os.ReadDir(filepath.Join(dataDir, "runs"))
	if err != nil {
		if os.IsNotExist(err) {
			return tokens, costMicroUSD, nil
		}
		return 0, 0, fmt.Errorf("list runs for monthly budget: %w", err)
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		loadedRun, loadErr := run.Load(dataDir, entry.Name())
		if loadErr != nil {
			continue
		}
		for _, attempt := range loadedRun.Attempts {
			startedAt, perr := time.Parse(time.RFC3339, attempt.StartedAt)
			if perr != nil || !inMonth(startedAt.UTC()) {
				continue
			}
			tokens += attempt.RelayConsumedInputTokens + attempt.RelayConsumedOutputTokens
			costMicroUSD += attempt.RelayConsumedCostMicroUSD
		}
	}
	return tokens, costMicroUSD, nil
}

// runSpend is what run's attempts recorded spending: tokens in and out, and
// cost.
func runSpend(rec *run.Run) (tokens, costMicroUSD int64) {
	for _, a := range rec.Attempts {
		tokens += a.RelayConsumedInputTokens + a.RelayConsumedOutputTokens
		costMicroUSD += a.RelayConsumedCostMicroUSD
	}
	return tokens, costMicroUSD
}

// budgetLeftFor is "" when every budget CheckLaunchBudget reads (the
// request's and the month's, tokens and cost) has at least the spend of
// repeats left, the build a launch is about to repeat; otherwise the reason
// it has not. A budget that is not set always has enough. It is the advice
// round's check: a build that is not a failure's recovery runs only when it
// cannot be the launch that crosses a budget.
func budgetLeftFor(dataDir string, r *request.Request, settings sessionconfig.Settings, repeats *run.Run, now time.Time) (string, error) {
	needTokens, needCost := runSpend(repeats)
	if settings.RequestTokenBudget > 0 || settings.RequestCostBudgetMicroUSD > 0 {
		cs := api.NewServer(dataDir).ComputeCostSummary(r)
		if reason := leftShort("request", cs.Tokens, costSummaryMicroUSD(cs), int64(settings.RequestTokenBudget), settings.RequestCostBudgetMicroUSD, needTokens, needCost); reason != "" {
			return reason, nil
		}
	}
	if settings.MonthlyTokenBudget > 0 || settings.MonthlyCostBudgetMicroUSD > 0 {
		tokens, cost, err := MonthToDateSpend(dataDir, now)
		if err != nil {
			return "", err
		}
		return leftShort("monthly", tokens, cost, int64(settings.MonthlyTokenBudget), settings.MonthlyCostBudgetMicroUSD, needTokens, needCost), nil
	}
	return "", nil
}

// leftShort is the reason scope's budget (tokenLimit, costLimit; 0 is unset)
// has less than needTokens and needCost left after spending tokens and cost,
// "" when it has enough.
func leftShort(scope string, tokens, cost, tokenLimit, costLimit, needTokens, needCost int64) string {
	if tokenLimit > 0 && tokenLimit-tokens < needTokens {
		return fmt.Sprintf("%s token budget has %d tokens left, under the %d the accepted run spent", scope, max(tokenLimit-tokens, 0), needTokens)
	}
	if costLimit > 0 && costLimit-cost < needCost {
		return fmt.Sprintf("%s cost budget has $%.2f left, under the $%.2f the accepted run spent", scope, float64(max(costLimit-cost, 0))/1e6, float64(needCost)/1e6)
	}
	return ""
}
