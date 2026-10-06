package modelrole

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"buildgate/internal/sandbox"
	"buildgate/internal/sessionconfig"
)

// routesModeSettings returns a minimal, valid, LAUNCHABLE routes: mode
// Settings (unlike sessionconfig's own routesModeSettings helper, which
// only needs to pass ValidateRouting's schema checks, not RoutePolicy's
// own budget/image validation): DefaultSettings' own budgets/ceilings,
// plus one execution role over two static routes, "primary" then
// "fallback", both reachable via a plain env-var credential.
func routesModeSettings() sessionconfig.Settings {
	s := sessionconfig.DefaultSettings()
	s.Routes = map[string]sessionconfig.Route{
		"primary": {
			CredentialMode: "static",
			Upstream:       "https://primary.example.invalid",
			CredentialEnv:  "PRIMARY_KEY",
		},
		"fallback": {
			CredentialMode: "static",
			Upstream:       "https://fallback.example.invalid",
			CredentialEnv:  "FALLBACK_KEY",
		},
	}
	s.Models = map[string]sessionconfig.Model{
		"luna": {ID: "gpt-5.6-luna", Routes: []string{"primary", "fallback"}},
	}
	s.Roles = &sessionconfig.Roles{
		Execution: &sessionconfig.RoleConfig{Model: "luna", Thinking: "medium"},
	}
	return s
}

func alwaysOK(string, sessionconfig.Route) error { return nil }

func TestSelectRouteFallsBackWithinModelOnly(t *testing.T) {
	s := routesModeSettings()
	probe := func(name string, _ sessionconfig.Route) error {
		if name == "primary" {
			return errors.New("credential file not found")
		}
		return nil
	}
	sel, err := SelectRoute(s, RoleExecution, "", "", "", probe)
	if err != nil {
		t.Fatalf("SelectRoute: %v", err)
	}
	if sel.RouteName != "fallback" {
		t.Errorf("RouteName = %q, want %q", sel.RouteName, "fallback")
	}
	if len(sel.Skipped) != 1 || sel.Skipped[0].Route != "primary" {
		t.Fatalf("Skipped = %+v, want one skip for primary", sel.Skipped)
	}
	if sel.Skipped[0].Reason != skipReasonCredentialUnavailable {
		t.Errorf("Skipped[0].Reason = %q, want the fixed factory reason", sel.Skipped[0].Reason)
	}
}

func TestSelectRouteNeverCrossesModels(t *testing.T) {
	s := routesModeSettings()
	// A second model, "sonnet", whose own route would succeed -- but
	// roles.execution names "luna", so it must never be considered.
	s.Models["sonnet"] = sessionconfig.Model{ID: "claude-sonnet-5", Routes: []string{"fallback"}}
	probe := func(name string, _ sessionconfig.Route) error {
		return errors.New("no credential for " + name)
	}
	_, err := SelectRoute(s, RoleExecution, "", "", "", probe)
	if err == nil {
		t.Fatal("SelectRoute: err = nil, want every route of luna to fail")
	}
	if !errors.Is(err, ErrNoRouteAvailable) {
		t.Errorf("err = %v, want it to wrap ErrNoRouteAvailable", err)
	}
	if strings.Contains(err.Error(), "sonnet") {
		t.Errorf("err = %v, must never name a model other than roles.execution's own", err)
	}
}

// routesModeSettingsWithAllowedSonnet is routesModeSettings plus a second
// execution-role model, "sonnet", reachable only over its own "fallback2"
// route, and roles.execution.allowed naming both -- the fixture SelectRoute's
// own choice tests below select within.
func routesModeSettingsWithAllowedSonnet() sessionconfig.Settings {
	s := routesModeSettings()
	s.Routes["fallback2"] = sessionconfig.Route{
		CredentialMode: "static",
		Upstream:       "https://fallback2.example.invalid",
		CredentialEnv:  "FALLBACK2_KEY",
	}
	s.Models["sonnet"] = sessionconfig.Model{ID: "claude-sonnet-5", Routes: []string{"fallback2"}}
	s.Roles.Execution.Allowed = []string{"luna", "sonnet"}
	return s
}

func TestSelectRouteChoiceEmptyKeepsDefaultModel(t *testing.T) {
	s := routesModeSettingsWithAllowedSonnet()
	sel, err := SelectRoute(s, RoleExecution, "", "", "", alwaysOK)
	if err != nil {
		t.Fatalf("SelectRoute: %v", err)
	}
	if sel.ModelName != "luna" {
		t.Errorf("ModelName = %q, want the role's default model %q", sel.ModelName, "luna")
	}
}

func TestSelectRouteChoiceWithinAllowedSelectsIt(t *testing.T) {
	s := routesModeSettingsWithAllowedSonnet()
	sel, err := SelectRoute(s, RoleExecution, "sonnet", "", "", alwaysOK)
	if err != nil {
		t.Fatalf("SelectRoute: %v", err)
	}
	if sel.ModelName != "sonnet" {
		t.Errorf("ModelName = %q, want the requested choice %q", sel.ModelName, "sonnet")
	}
	if sel.RouteName != "fallback2" {
		t.Errorf("RouteName = %q, want sonnet's own route %q", sel.RouteName, "fallback2")
	}
}

func TestSelectRouteChoiceNotInAllowedErrors(t *testing.T) {
	s := routesModeSettingsWithAllowedSonnet()
	_, err := SelectRoute(s, RoleExecution, "opus", "", "", alwaysOK)
	if err == nil {
		t.Fatal("SelectRoute: err = nil, want a refusal for a choice outside roles.execution.allowed")
	}
	if !strings.Contains(err.Error(), `"opus"`) || !strings.Contains(err.Error(), "roles.execution.allowed") {
		t.Errorf("err = %v, want it to name the rejected choice and roles.execution.allowed", err)
	}
}

func TestSelectRouteChoiceNeverFallsBackToAnotherModel(t *testing.T) {
	// sonnet's own only route (fallback2) fails its probe; SelectRoute
	// must refuse rather than silently trying luna's own routes instead.
	s := routesModeSettingsWithAllowedSonnet()
	probe := func(string, sessionconfig.Route) error { return errors.New("no credential") }
	_, err := SelectRoute(s, RoleExecution, "sonnet", "", "", probe)
	if err == nil {
		t.Fatal("SelectRoute: err = nil, want every route of the chosen model to fail")
	}
	if strings.Contains(err.Error(), "luna") {
		t.Errorf("err = %v, must never name a model other than the one actually chosen", err)
	}
}

func TestCheckRouteBindingAcceptsAllowedNonDefaultModel(t *testing.T) {
	s := routesModeSettingsWithAllowedSonnet()
	sel, err := SelectRoute(s, RoleExecution, "sonnet", "", "", alwaysOK)
	if err != nil {
		t.Fatalf("SelectRoute: %v", err)
	}
	if err := CheckRouteBinding(s, RoleExecution, sel.Policy, sel.Thinking); err != nil {
		t.Errorf("CheckRouteBinding: %v, want the Worker to accept an allowed non-default model", err)
	}
}

func TestSelectRouteForceRoute(t *testing.T) {
	s := routesModeSettings()
	_, err := SelectRoute(s, RoleExecution, "", "", "not-a-real-route", alwaysOK)
	if err == nil {
		t.Fatal("SelectRoute: err = nil, want a refusal for a forced route outside the model's own routes")
	}
	if !strings.Contains(err.Error(), "not-a-real-route") {
		t.Errorf("err = %v, want it to name the forced route", err)
	}

	sel, err := SelectRoute(s, RoleExecution, "", "", "fallback", alwaysOK)
	if err != nil {
		t.Fatalf("SelectRoute with a valid forced route: %v", err)
	}
	if sel.RouteName != "fallback" {
		t.Errorf("RouteName = %q, want the forced route %q", sel.RouteName, "fallback")
	}
}

func TestSelectRouteSkipReasonNeverEchoesResolverError(t *testing.T) {
	const secret = "sk-super-secret-token-xyz"
	s := routesModeSettings()
	probe := func(name string, _ sessionconfig.Route) error {
		return errors.New("resolve " + name + ": token " + secret + " is expired")
	}
	_, err := SelectRoute(s, RoleExecution, "", "", "", probe)
	if err == nil {
		t.Fatal("SelectRoute: err = nil, want a refusal (probe always fails)")
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("err = %v, must never echo the resolver's own error text", err)
	}
}

// TestSelectRouteSkipReasonNamesValidateFailure is the live M3 walk case
// (2026-09-28): a model whose id contains a slash produces a policy that
// RoutePolicy.Validate rejects (pi's own CLI hangs on a slash-containing
// --model value). Before this fix the halted run's skip reason said only
// "route's relay policy failed validation" -- naming no actual cause;
// this proves the skip reason now names it.
func TestSelectRouteSkipReasonNamesValidateFailure(t *testing.T) {
	s := routesModeSettings()
	s.Models["luna"] = sessionconfig.Model{ID: "some/slash-id", Routes: []string{"primary", "fallback"}}
	_, err := SelectRoute(s, RoleExecution, "", "", "", alwaysOK)
	if err == nil {
		t.Fatal("SelectRoute: err = nil, want a refusal for a slash-containing model id")
	}
	if !strings.Contains(err.Error(), "must not contain a slash") {
		t.Errorf("err = %v, want the skip reason to include RoutePolicy.Validate's own error text", err)
	}
}

func TestSelectRouteReturnsPolicyRouteAndBilling(t *testing.T) {
	s := routesModeSettings()
	sel, err := SelectRoute(s, RoleExecution, "", "", "", alwaysOK)
	if err != nil {
		t.Fatalf("SelectRoute: %v", err)
	}
	if sel.Policy.Route != "primary" {
		t.Errorf("Policy.Route = %q, want %q", sel.Policy.Route, "primary")
	}
	if sel.Policy.Billing != "metered" {
		t.Errorf("Policy.Billing = %q, want %q (static route)", sel.Policy.Billing, "metered")
	}
	if sel.Policy.Upstream != "https://primary.example.invalid" {
		t.Errorf("Policy.Upstream = %q, want the primary route's own upstream", sel.Policy.Upstream)
	}
	if sel.Policy.WorkerModelID != "gpt-5.6-luna" {
		t.Errorf("Policy.WorkerModelID = %q, want the model's own id", sel.Policy.WorkerModelID)
	}
	if sel.Thinking != "medium" {
		t.Errorf("Thinking = %q, want roles.execution.thinking", sel.Thinking)
	}
}

// TestCheckRouteBindingAcceptsSelectedPolicy is the success case: a
// policy built by SelectRoute for one of the Worker's own routes binds
// cleanly.
func TestCheckRouteBindingAcceptsSelectedPolicy(t *testing.T) {
	s := routesModeSettings()
	sel, err := SelectRoute(s, RoleExecution, "", "", "", alwaysOK)
	if err != nil {
		t.Fatalf("SelectRoute: %v", err)
	}
	if err := CheckRouteBinding(s, RoleExecution, sel.Policy, sel.Thinking); err != nil {
		t.Fatalf("CheckRouteBinding: %v", err)
	}
}

func TestCheckRouteBindingRefusesUnknownRoute(t *testing.T) {
	s := routesModeSettings()
	sel, err := SelectRoute(s, RoleExecution, "", "", "", alwaysOK)
	if err != nil {
		t.Fatalf("SelectRoute: %v", err)
	}
	policy := sel.Policy
	policy.Route = "not-a-configured-route"
	if err := CheckRouteBinding(s, RoleExecution, policy, sel.Thinking); err == nil {
		t.Fatal("CheckRouteBinding: err = nil, want a refusal for an unknown route name")
	} else if !strings.Contains(err.Error(), "not-a-configured-route") {
		t.Errorf("err = %v, want it to name the unknown route", err)
	}
}

func TestCheckRouteBindingRefusesLegacyPolicy(t *testing.T) {
	s := routesModeSettings()
	if err := CheckRouteBinding(s, RoleExecution, sandbox.RoutePolicy{}, ""); err == nil {
		t.Fatal("CheckRouteBinding: err = nil, want a refusal for a routeless (legacy) policy")
	}
}

// TestCheckRouteBindingRefusesModelOutsideRole proves a policy whose
// worker_model_id belongs only to a model roles.execution never
// declares (not its own Model, not in its own Allowed) refuses, even
// when the route name itself is one roles.execution's actual model also
// happens to use.
func TestCheckRouteBindingRefusesModelOutsideRole(t *testing.T) {
	s := routesModeSettings()
	// "other" is a second model on the SAME "primary" route, with a
	// different worker model id, that no role configures at all.
	s.Models["other"] = sessionconfig.Model{ID: "other-model", Routes: []string{"primary"}}
	sel, err := SelectRoute(s, RoleExecution, "", "", "", alwaysOK)
	if err != nil {
		t.Fatalf("SelectRoute: %v", err)
	}
	policy := sel.Policy
	policy.WorkerModelID = "other-model"
	if err := CheckRouteBinding(s, RoleExecution, policy, sel.Thinking); err == nil {
		t.Fatal("CheckRouteBinding: err = nil, want a refusal for a worker_model_id belonging to a model roles.execution never configures")
	}
}

// TestCheckRouteBindingCoversEveryRelayPolicyField is the reflection-
// count guard relayPolicyFieldCount's own doc comment describes: if
// sandbox.RoutePolicy gains or loses a field, this fails, forcing an
// explicit decision on whether compareBoundRelayPolicy's field-by-field
// walk (which needs no updating itself, since it reflects over every
// field) also needs a new ceilingExemptRelayPolicyFields entry.
func TestCheckRouteBindingCoversEveryRelayPolicyField(t *testing.T) {
	got := reflect.TypeOf(sandbox.RoutePolicy{}).NumField()
	if got != relayPolicyFieldCount {
		t.Fatalf("sandbox.RoutePolicy has %d fields, want %d (relayPolicyFieldCount in route.go) -- a field was added or removed; update relayPolicyFieldCount, and decide whether the new/removed field needs its own ceilingExemptRelayPolicyFields entry", got, relayPolicyFieldCount)
	}
}

// TestCheckRouteBindingAllowsTightenedCeilingRefusesRaisedOne proves the
// one exception to exact-match binding: TokenCeiling/CostCeilingMicroUSD
// may be LOWERED from this Worker's own expected value (a target repo's
// project config tightening a run's budget after SelectRoute already
// built its policy) but never RAISED.
func TestCheckRouteBindingAllowsTightenedCeilingRefusesRaisedOne(t *testing.T) {
	s := routesModeSettings()
	sel, err := SelectRoute(s, RoleExecution, "", "", "", alwaysOK)
	if err != nil {
		t.Fatalf("SelectRoute: %v", err)
	}

	tightened := sel.Policy
	tightened.TokenCeiling -= 1
	tightened.CostCeilingMicroUSD -= 1
	if err := CheckRouteBinding(s, RoleExecution, tightened, sel.Thinking); err != nil {
		t.Errorf("CheckRouteBinding with a tightened ceiling: %v, want nil", err)
	}

	raisedToken := sel.Policy
	raisedToken.TokenCeiling += 1
	if err := CheckRouteBinding(s, RoleExecution, raisedToken, sel.Thinking); err == nil {
		t.Error("CheckRouteBinding: err = nil, want a refusal for a raised TokenCeiling")
	} else if !strings.Contains(err.Error(), "TokenCeiling") {
		t.Errorf("err = %v, want it to name TokenCeiling", err)
	}

	raisedCost := sel.Policy
	raisedCost.CostCeilingMicroUSD += 1
	if err := CheckRouteBinding(s, RoleExecution, raisedCost, sel.Thinking); err == nil {
		t.Error("CheckRouteBinding: err = nil, want a refusal for a raised CostCeilingMicroUSD")
	} else if !strings.Contains(err.Error(), "CostCeilingMicroUSD") {
		t.Errorf("err = %v, want it to name CostCeilingMicroUSD", err)
	}

	zeroToken := sel.Policy
	zeroToken.TokenCeiling = 0
	if err := CheckRouteBinding(s, RoleExecution, zeroToken, sel.Thinking); err == nil {
		t.Error("CheckRouteBinding: err = nil, want a refusal for a zero TokenCeiling")
	} else if !strings.Contains(err.Error(), "TokenCeiling") {
		t.Errorf("err = %v, want it to name TokenCeiling", err)
	}

	negativeToken := sel.Policy
	negativeToken.TokenCeiling = -1
	if err := CheckRouteBinding(s, RoleExecution, negativeToken, sel.Thinking); err == nil {
		t.Error("CheckRouteBinding: err = nil, want a refusal for a negative TokenCeiling")
	} else if !strings.Contains(err.Error(), "TokenCeiling") {
		t.Errorf("err = %v, want it to name TokenCeiling", err)
	}

	zeroCost := sel.Policy
	zeroCost.CostCeilingMicroUSD = 0
	if err := CheckRouteBinding(s, RoleExecution, zeroCost, sel.Thinking); err == nil {
		t.Error("CheckRouteBinding: err = nil, want a refusal for a zero CostCeilingMicroUSD")
	} else if !strings.Contains(err.Error(), "CostCeilingMicroUSD") {
		t.Errorf("err = %v, want it to name CostCeilingMicroUSD", err)
	}

	negativeCost := sel.Policy
	negativeCost.CostCeilingMicroUSD = -1
	if err := CheckRouteBinding(s, RoleExecution, negativeCost, sel.Thinking); err == nil {
		t.Error("CheckRouteBinding: err = nil, want a refusal for a negative CostCeilingMicroUSD")
	} else if !strings.Contains(err.Error(), "CostCeilingMicroUSD") {
		t.Errorf("err = %v, want it to name CostCeilingMicroUSD", err)
	}
}

// TestCheckRouteBindingRefusesThinkingMismatch proves a policy is bound
// against this Worker's own roles.<role>.thinking, not the submitter's
// own claimed value -- the two must never be allowed to silently
// diverge, since roles.<role>.thinking is what actually reaches
// build_app.py/conformity_review.py's --thinking.
func TestCheckRouteBindingRefusesThinkingMismatch(t *testing.T) {
	s := routesModeSettings()
	sel, err := SelectRoute(s, RoleExecution, "", "", "", alwaysOK)
	if err != nil {
		t.Fatalf("SelectRoute: %v", err)
	}
	if sel.Thinking == "" {
		t.Fatal("fixture roles.execution.thinking is empty, want a non-empty level to mismatch against")
	}
	if err := CheckRouteBinding(s, RoleExecution, sel.Policy, sel.Thinking+"-not-the-real-level"); err == nil {
		t.Fatal("CheckRouteBinding: err = nil, want a refusal for a mismatched thinking level")
	} else if !strings.Contains(err.Error(), "thinking") {
		t.Errorf("err = %v, want it to name thinking", err)
	}
}

// TestCheckRouteBindingRefusesEachTrustFieldMismatch tables one mutation
// per sandbox.RoutePolicy field on a policy that would otherwise bind
// cleanly (excluding the two ceiling fields, covered by
// TestCheckRouteBindingAllowsTightenedCeilingRefusesRaisedOne instead),
// and checks the refusal names that field.
func TestCheckRouteBindingRefusesEachTrustFieldMismatch(t *testing.T) {
	s := routesModeSettings()
	base, err := SelectRoute(s, RoleExecution, "", "", "", alwaysOK)
	if err != nil {
		t.Fatalf("SelectRoute: %v", err)
	}

	cases := []struct {
		name      string
		mutate    func(p *sandbox.RoutePolicy)
		wantField string
	}{
		{"Upstream", func(p *sandbox.RoutePolicy) { p.Upstream = "https://not-the-real-route.example.invalid" }, "Upstream"},
		{"AuthMode", func(p *sandbox.RoutePolicy) { p.AuthMode = "chatgpt-codex" }, "AuthMode"},
		{"UpstreamAuthHeader", func(p *sandbox.RoutePolicy) { p.UpstreamAuthHeader = "authorization" }, "UpstreamAuthHeader"},
		{"AllowedPathPrefix", func(p *sandbox.RoutePolicy) { p.AllowedPathPrefix = "/not/the/real/prefix" }, "AllowedPathPrefix"},
		{"AllowPlaintextUpstream", func(p *sandbox.RoutePolicy) { p.AllowPlaintextUpstream = !p.AllowPlaintextUpstream }, "AllowPlaintextUpstream"},
		{"AllowUnauthenticatedUpstream", func(p *sandbox.RoutePolicy) { p.AllowUnauthenticatedUpstream = !p.AllowUnauthenticatedUpstream }, "AllowUnauthenticatedUpstream"},
		{"WorkerModelID", func(p *sandbox.RoutePolicy) { p.WorkerModelID = "not-the-real-model-id" }, "WorkerModelID"},
		{"InputMicroUSDPerMTok", func(p *sandbox.RoutePolicy) { p.InputMicroUSDPerMTok++ }, "InputMicroUSDPerMTok"},
		{"OutputMicroUSDPerMTok", func(p *sandbox.RoutePolicy) { p.OutputMicroUSDPerMTok++ }, "OutputMicroUSDPerMTok"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			policy := base.Policy
			tc.mutate(&policy)
			err := CheckRouteBinding(s, RoleExecution, policy, base.Thinking)
			if err == nil {
				t.Fatalf("CheckRouteBinding: err = nil, want a refusal for a mismatched %s", tc.wantField)
			}
			if !strings.Contains(err.Error(), tc.wantField) {
				t.Errorf("err = %v, want it to name the mismatched field %q", err, tc.wantField)
			}
		})
	}
}
