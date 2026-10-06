package main

import (
	"fmt"
	"os"

	"buildgate/internal/modelrole"
	"buildgate/internal/sessionconfig"
)

// logRouteSkips prints one stderr line per route sel skipped on its way
// to its own chosen route -- operator visibility into a fallback
// (operator rule: silence is a bug). role names which role this
// selection was for (e.g. "execution", "review"), in the message only,
// never affecting which routes were tried. A nil sel, or one with no
// skips (the common case -- the model's first declared route usually
// just works), prints nothing.
func logRouteSkips(role string, sel *modelrole.Selection) {
	if sel == nil {
		return
	}
	for _, skip := range sel.Skipped {
		fmt.Fprintf(os.Stderr, "route %s skipped for roles.%s: %s; using %s\n", skip.Route, role, skip.Reason, sel.RouteName)
	}
}

// modelRouteNeeded reports whether a build's worker is given a model route.
// The default, model-backed build_app.py always needs one. An explicitly
// supplied -build-app-script may be an offline worker, so it gets one only
// when roles.execution names a model.
func modelRouteNeeded(settings sessionconfig.Settings, buildAppScriptExplicit bool) bool {
	return !buildAppScriptExplicit || modelrole.RoleConfigured(settings, modelrole.RoleExecution)
}

// applyFinalRelayCeilings refreshes execSel/reviewSel's own
// RoutePolicy.TokenCeiling/CostCeilingMicroUSD from settings.
// EffectiveRelayCeilings() -- the one shared helper both this function
// and a request-job-role relay launch (resolveRequestJobRole, which
// calls modelrole.SelectRoute -> relayPolicyForRoute again, re-deriving
// its ceilings fresh, at its own launch time) call, so the two paths
// can't drift. Must run
// AFTER applyProjectConfigDefaults, which can lower these ceilings from
// a target repo's own .factory.yml: modelrole.SelectRoute (run earlier
// in runMainWithReady, before applyProjectConfigDefaults) has no way to
// see that adjustment, so its own selections' policies still carry the
// pre-project-config ceiling until this call corrects them, in place,
// on the same *modelrole.Selection values run_ticket.go's own launch
// construction reads. Either argument may be nil (routes: mode is off,
// or roles.review is unset).
func applyFinalRelayCeilings(settings sessionconfig.Settings, execSel, reviewSel *modelrole.Selection) {
	tokenCeiling, costCeilingMicroUSD := settings.EffectiveRelayCeilings()
	if execSel != nil {
		execSel.Policy.TokenCeiling = tokenCeiling
		execSel.Policy.CostCeilingMicroUSD = costCeilingMicroUSD
	}
	if reviewSel != nil {
		reviewSel.Policy.TokenCeiling = tokenCeiling
		reviewSel.Policy.CostCeilingMicroUSD = costCeilingMicroUSD
	}
}
