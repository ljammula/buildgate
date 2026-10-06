package main

import (
	"testing"

	"buildgate/internal/modelrole"
	"buildgate/internal/sandbox"
	"buildgate/internal/sessionconfig"
)

// TestApplyFinalRelayCeilingsUsesPostProjectConfigValues proves
// applyFinalRelayCeilings refreshes both the execution and review
// selections' own RoutePolicy.TokenCeiling/CostCeilingMicroUSD from the
// settings a caller passes it -- the fix for a real ordering bug found
// via review round 2: modelrole.SelectRoute runs (and freezes its own
// selections' ceilings) BEFORE run_ticket.go's own
// applyProjectConfigDefaults can lower them from a target repo's
// .factory.yml (a token_ceiling below the session's own 5x-budget
// default), so without this second pass, a build/review would launch
// with the higher, pre-project-config ceiling regardless of what the
// repo's own .factory.yml capped it to.
func TestApplyFinalRelayCeilingsUsesPostProjectConfigValues(t *testing.T) {
	settings := sessionconfig.DefaultSettings()
	settings.MeterTokenBudget = 1000
	settings.MeterCostBudgetMicroUSD = 1000
	// Simulates applyProjectConfigDefaults already having lowered these
	// from a .factory.yml token_ceiling/cost_ceiling_micro_usd below the
	// session's own 5x-budget default (5000 each).
	settings.MeterTokenCeiling = 1500
	settings.MeterCostCeilingMicroUSD = 1500

	// execSelection/reviewSelection as modelrole.SelectRoute would have
	// left them: still carrying the STALE, pre-project-config ceiling
	// (5000, the 5x-budget default, computed before the .factory.yml
	// override applied).
	execSel := &modelrole.Selection{Policy: sandbox.RoutePolicy{TokenCeiling: 5000, CostCeilingMicroUSD: 5000}}
	reviewSel := &modelrole.Selection{Policy: sandbox.RoutePolicy{TokenCeiling: 5000, CostCeilingMicroUSD: 5000}}

	applyFinalRelayCeilings(settings, execSel, reviewSel)

	if execSel.Policy.TokenCeiling != 1500 {
		t.Errorf("execSel.Policy.TokenCeiling = %d, want the project-config ceiling 1500, not the stale pre-project-config 5000", execSel.Policy.TokenCeiling)
	}
	if execSel.Policy.CostCeilingMicroUSD != 1500 {
		t.Errorf("execSel.Policy.CostCeilingMicroUSD = %d, want 1500", execSel.Policy.CostCeilingMicroUSD)
	}
	if reviewSel.Policy.TokenCeiling != 1500 {
		t.Errorf("reviewSel.Policy.TokenCeiling = %d, want the project-config ceiling 1500 (build and review must not drift)", reviewSel.Policy.TokenCeiling)
	}
	if reviewSel.Policy.CostCeilingMicroUSD != 1500 {
		t.Errorf("reviewSel.Policy.CostCeilingMicroUSD = %d, want 1500", reviewSel.Policy.CostCeilingMicroUSD)
	}
}

// TestApplyFinalRelayCeilingsNilSafe proves a nil execSel/reviewSel (no
// relay selection, or roles.review unset) never panics.
func TestApplyFinalRelayCeilingsNilSafe(t *testing.T) {
	applyFinalRelayCeilings(sessionconfig.DefaultSettings(), nil, nil)
}
