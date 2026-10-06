// Route selection for a routes:/models:/roles: session config -- see
// SelectRoute's own doc comment.
package modelrole

import (
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"

	"buildgate/internal/harness"
	"buildgate/internal/meter"
	"buildgate/internal/prices"
	"buildgate/internal/sandbox"
	"buildgate/internal/sessionconfig"
)

// ErrNoRouteAvailable is SelectRoute's own fail-closed sentinel: every
// route this role's model declares failed its host-local pre-launch
// checks (policy invalid, upstream scheme rejected, credential
// unavailable, or a plaintext-upstream-with-credential conflict). Wrapped
// with the model/role name and the full skip list so the caller's error
// message names every route that was tried and why, without ever
// resolving to a different model -- see SelectRoute's own doc comment for
// why that never happens.
var ErrNoRouteAvailable = errors.New("modelrole: no configured route is usable for this role's model")

// RouteSkip records one candidate route SelectRoute passed over, and a
// FIXED, factory-authored reason -- never the resolver's own (probe's)
// error text, which could otherwise echo a file path, an env var name, or
// a fragment of a real secret into a run record or a returned error. See
// TestSelectRouteSkipReasonNeverEchoesResolverError.
//
// The one exception is skipReasonPolicyInvalid's own RoutePolicy.Validate
// error, appended verbatim below -- Validate's error strings are all
// fixed, factory-authored text describing the policy shape itself (e.g.
// "relay worker model id must not contain a slash: ..."), never a value
// pulled from a credential resolver, so appending one carries none of the
// resolver-echo risk this doc comment otherwise warns about. Found live
// (M3 walk, 2026-09-28): a bare "route's relay policy failed validation"
// left the halted run naming no actual cause, even though
// modelrole.ValidateAllowedPolicies (the config-load-time half of this
// same fix) would have caught this exact policy before launch ever ran.
type RouteSkip struct {
	Route  string
	Reason string
}

// Selection is SelectRoute's own successful result: the role's resolved
// model and its first usable route, the RoutePolicy built for that route
// (credential-free -- see sandbox.RoutePolicy's own doc comment), and
// every route this selection passed over on the way there. Selection
// itself never carries a credential value: resolving one is the caller's
// own job (see cmd/factoryd/route_credentials.go), done only for the one
// route actually returned here.
type Selection struct {
	ModelName string
	Model     sessionconfig.Model
	RouteName string
	Route     sessionconfig.Route
	Thinking  string
	// Harness is the coding-agent CLI the role's jobs run under: the request's
	// per-role choice, else roles.<r>.harness, else pi.
	Harness string
	Policy  sandbox.RoutePolicy
	Skipped []RouteSkip
}

// Fixed skip reasons -- see RouteSkip's own doc comment for why these
// must never be (or contain) the resolver's own error text.
// skipReasonPolicyInvalid alone is exempt from the "or contain" half: see
// RouteSkip's own doc comment for why appending RoutePolicy.Validate's
// own error text to it is safe.
const (
	skipReasonRouteNotConfigured    = "route is not configured"
	skipReasonPolicyInvalid         = "route's relay policy failed validation"
	skipReasonUpstreamSchemeUnsafe  = "route's upstream scheme was rejected"
	skipReasonCredentialUnavailable = "route's credential did not resolve"
	skipReasonCredentialUnsafe      = "route's credential is unsafe for its upstream"
)

// relayPolicyForRoute builds the candidate sandbox.RoutePolicy a
// particular (model, route) pair would launch with: every session-scoped
// field (budgets, window/ceiling, base pricing) comes from s
// itself, unchanged by route choice; every route-scoped field (upstream,
// path prefix, auth mode, worker model id/API/base path/extra JSON,
// Route/Billing) is resolved from model and route -- route is already
// the defaulted copy Settings.Routing() returns (upstream/path-prefix/
// base-path defaults already applied for chatgpt-codex/github-copilot),
// except a github-copilot route's own AllowedPathPrefix, whose default
// depends on the model's effective API and so is filled in here (mirrors
// cmd/factoryd's own applyGitHubCopilotRelayDefaults -- see routing.go's
// defaultedRoute doc comment for why that half is left to this later,
// model-aware step instead of Routing() itself).
func relayPolicyForRoute(s sessionconfig.Settings, model sessionconfig.Model, routeName string, route sessionconfig.Route) (sandbox.RoutePolicy, error) {
	api := model.EffectiveAPI(route)

	allowedPathPrefix := effectiveAllowedPathPrefix(route, api)

	workerBasePath := ""
	if route.WorkerBasePath != nil {
		workerBasePath = *route.WorkerBasePath
	}

	// UsageFormat is derived purely from the model's effective API --
	// routes:/models:/roles: is the only session-config schema, and
	// there is no per-session or per-request override of it.
	var usageFormat string
	if api == meter.RequestFormatOpenAIResponses {
		usageFormat = meter.UsageFormatOpenAIResponses
	} else {
		usageFormat = meter.UsageFormatOpenAI
	}

	// A model with no price-table entry prices at $0 (prices.ErrNoPrice);
	// sessionconfig already refused a malformed table at load.
	price, _ := prices.Lookup(model.ID)

	tokenCeiling, costCeiling := s.EffectiveRelayCeilings()

	workerModelExtraJSON, err := model.WorkerModelJSON(routeName)
	if err != nil {
		return sandbox.RoutePolicy{}, err
	}

	return sandbox.RoutePolicy{
		Upstream:                     route.Upstream,
		AllowedPathPrefix:            allowedPathPrefix,
		MaxRequestBytes:              s.MeterMaxRequestBytes,
		RequestsPerMinute:            s.MeterRequestsPerMinute,
		TokenBudget:                  s.MeterTokenBudget,
		TokenBudgetWindow:            s.MeterTokenBudgetWindow,
		CostBudgetMicroUSD:           s.MeterCostBudgetMicroUSD,
		CostBudgetWindow:             s.MeterCostBudgetWindow,
		InputMicroUSDPerMTok:         int64(price.Input),
		CachedInputMicroUSDPerMTok:   int64(price.EffectiveCachedInput()),
		CacheWriteMicroUSDPerMTok:    int64(price.EffectiveCacheWrite()),
		OutputMicroUSDPerMTok:        int64(price.Output),
		TokenCeiling:                 tokenCeiling,
		CostCeilingMicroUSD:          costCeiling,
		UsageFormat:                  usageFormat,
		UpstreamAuthHeader:           route.CredentialHeader,
		AuthMode:                     route.EffectiveCredentialMode(),
		WorkerModelID:                model.EffectiveID(routeName),
		WorkerModelAPI:               api,
		WorkerBasePath:               workerBasePath,
		WorkerModelExtraJSON:         workerModelExtraJSON,
		AllowUnauthenticatedUpstream: route.AllowNoCredential,
		AllowPlaintextUpstream:       route.AllowPlaintextUpstream,
		Route:                        routeName,
		Billing:                      route.Billing,
	}, nil
}

// effectiveAllowedPathPrefix is route's own AllowedPathPrefix with its
// github-copilot default applied -- the one place both relayPolicyForRoute
// (building a candidate policy) and CheckRouteBinding (re-deriving the
// Worker's own expected value to compare a submitted policy against)
// compute this, so they can't drift on what a route's own effective
// prefix is.
func effectiveAllowedPathPrefix(route sessionconfig.Route, api string) string {
	if route.AllowedPathPrefix == "" && route.EffectiveCredentialMode() == meter.CredentialModeGitHubCopilot {
		return meter.CopilotAllowedPathPrefixFor(api)
	}
	return route.AllowedPathPrefix
}

// roleConfig returns s.Roles.<r>, or nil when roles: (or that particular
// role) is unset. Panics on an unrecognized Role, exactly like ForStage.
func roleConfig(s sessionconfig.Settings, r Role) *sessionconfig.RoleConfig {
	if !isValidRole(r) {
		panic(fmt.Sprintf("modelrole: unknown role %q", r))
	}
	if s.Roles == nil {
		return nil
	}
	switch r {
	case RolePlanning:
		return s.Roles.Planning
	case RoleExecution:
		return s.Roles.Execution
	case RoleReview:
		return s.Roles.Review
	default:
		return nil
	}
}

// RoleConfigured reports whether role r has its own roles.<r>.model set
// in s -- the signal a caller needs to decide whether to call SelectRoute
// for r directly or fall back to a different role's model (see
// cmd/factoryd's resolveRequestJobRole, which falls a drafting job's
// unset planning/review role back to roles.execution's own model).
// Panics on an unrecognized Role, exactly like SelectRoute.
func RoleConfigured(s sessionconfig.Settings, r Role) bool {
	rc := roleConfig(s, r)
	return rc != nil && rc.Model != ""
}

// ConfiguredModel describes role r's configured model for display, read
// straight from roles:/models: without route selection: modelName,
// modelID (the model's id on its first declared route), thinking. ok is
// false when the role is unset or names an unknown model. Unlike
// SelectRoute it never validates a relay policy, so a display caller
// (`factoryd status`) still shows the role when its route is unusable.
func ConfiguredModel(s sessionconfig.Settings, r Role) (modelName, modelID, thinking string, ok bool) {
	rc := roleConfig(s, r)
	if rc == nil || rc.Model == "" {
		return "", "", "", false
	}
	_, models, err := s.Routing()
	if err != nil {
		return "", "", "", false
	}
	model, found := models[rc.Model]
	if !found {
		return "", "", "", false
	}
	if len(model.Routes) > 0 {
		modelID = model.EffectiveID(model.Routes[0])
	}
	return rc.Model, modelID, rc.Thinking, true
}

// SelectRoute resolves role r's model against s's routes:/models:
// config, then picks the first of that model's own declared routes (in
// the model's own declared order, or the single forceRoute when
// non-empty) whose host-local pre-launch checks all pass: its candidate
// RoutePolicy validates (Validate, ValidateUpstreamScheme), probe (the
// caller's own credential resolver, discarding the resolved value -- see
// cmd/factoryd/route_credentials.go) succeeds, and
// sandbox.CredentialSafeForUpstream accepts it. probe may be nil (no
// credential check at all, e.g. a route-listing/dry-run caller).
//
// choice is the caller's own per-request model pick within roles.<r>:
// empty keeps today's default (roles.<r>.model); non-empty must be one
// of roles.<r>.model or roles.<r>.allowed (candidateModelNames) or this
// returns an error naming the role, the rejected choice, and the full
// allowed list -- SelectRoute never falls back to a different model
// once choice is accepted, exactly like its Model-only behaviour before
// choice existed (see TestSelectRouteNeverCrossesModels). The chosen
// model's own declared routes are then tried exactly as rc.Model's
// would be.
//
// harnessChoice is the caller's own per-request harness pick, the same way:
// empty keeps roles.<r>.harness (default pi); non-empty must be one of
// roles.<r>.allowed_harnesses. The chosen harness must speak the api of the
// resolved model's route (sessionconfig.CheckHarnessAPI).
//
// A route that fails is recorded in the returned Selection.Skipped (on
// success) or the returned error (on total failure) with a FIXED,
// factory-authored reason (RouteSkip's own doc comment) -- never probe's
// own error text, which could echo a file path or a secret fragment.
func SelectRoute(s sessionconfig.Settings, r Role, choice, harnessChoice string, forceRoute string, probe func(name string, route sessionconfig.Route) error) (Selection, error) {
	rc := roleConfig(s, r)
	if rc == nil || rc.Model == "" {
		return Selection{}, fmt.Errorf("modelrole: roles.%s.model is not configured; configure routes:/models:/roles: (see USAGE_REFERENCE.md \"Model routes\")", r)
	}

	selectedModel := rc.Model
	if choice != "" {
		allowed := candidateModelNames(rc)
		if !slices.Contains(allowed, choice) {
			return Selection{}, fmt.Errorf("modelrole: model %q is not in roles.%s.allowed %v", choice, r, allowed)
		}
		selectedModel = choice
	}

	selectedHarness, err := RoleHarness(s, r, harnessChoice)
	if err != nil {
		return Selection{}, err
	}
	harnessDescriptor, err := harnessLookup(selectedHarness)
	if err != nil {
		return Selection{}, fmt.Errorf("modelrole: roles.%s: %w", r, err)
	}

	routes, models, err := s.Routing()
	if err != nil {
		return Selection{}, fmt.Errorf("modelrole: %w", err)
	}
	model, ok := models[selectedModel]
	if !ok {
		return Selection{}, fmt.Errorf("modelrole: roles.%s: model %q is not a models: entry", r, selectedModel)
	}

	candidateRoutes := model.Routes
	if forceRoute != "" {
		found := false
		for _, name := range model.Routes {
			if name == forceRoute {
				found = true
				break
			}
		}
		if !found {
			return Selection{}, fmt.Errorf("modelrole: roles.%s: forced route %q is not one of model %q's own routes %v", r, forceRoute, selectedModel, model.Routes)
		}
		candidateRoutes = []string{forceRoute}
	}

	var skipped []RouteSkip
	for _, routeName := range candidateRoutes {
		route, ok := routes[routeName]
		if !ok {
			skipped = append(skipped, RouteSkip{Route: routeName, Reason: skipReasonRouteNotConfigured})
			continue
		}
		policy, err := relayPolicyForRoute(s, model, routeName, route)
		if err != nil {
			skipped = append(skipped, RouteSkip{Route: routeName, Reason: skipReasonPolicyInvalid})
			continue
		}
		// Not a skippable route defect: the harness/model pairing itself is
		// wrong, and another route of the same model would not fix it.
		if err := sessionconfig.CheckHarnessAPI(string(r), harnessDescriptor, selectedModel, policy.WorkerModelAPI); err != nil {
			return Selection{}, fmt.Errorf("modelrole: %w", err)
		}
		if err := policy.Validate(); err != nil {
			skipped = append(skipped, RouteSkip{Route: routeName, Reason: skipReasonPolicyInvalid + ": " + err.Error()})
			continue
		}
		if err := policy.ValidateUpstreamScheme(); err != nil {
			skipped = append(skipped, RouteSkip{Route: routeName, Reason: skipReasonUpstreamSchemeUnsafe})
			continue
		}
		if probe != nil {
			if err := probe(routeName, route); err != nil {
				skipped = append(skipped, RouteSkip{Route: routeName, Reason: skipReasonCredentialUnavailable})
				continue
			}
		}
		credentialPresent := !policy.AllowUnauthenticatedUpstream
		if err := sandbox.CredentialSafeForUpstream(policy.Upstream, policy.AllowPlaintextUpstream, credentialPresent); err != nil {
			skipped = append(skipped, RouteSkip{Route: routeName, Reason: skipReasonCredentialUnsafe})
			continue
		}
		return Selection{
			ModelName: selectedModel,
			Model:     model,
			RouteName: routeName,
			Route:     route,
			Thinking:  rc.Thinking,
			Harness:   selectedHarness,
			Policy:    policy,
			Skipped:   skipped,
		}, nil
	}

	return Selection{}, fmt.Errorf("%w: roles.%s.model %q, routes tried %v", ErrNoRouteAvailable, r, selectedModel, skipped)
}

// candidateModelNames is the closed set of models: entries role's own
// roles.<role>.model/roles.<role>.allowed may resolve to, in that
// order, each name appearing at most once -- rc.Model always first
// (today's only per-request choice), then rc.Allowed in its own
// declared order for a future per-request choice within that set. nil
// rc (role unconfigured) yields no candidates at all.
func candidateModelNames(rc *sessionconfig.RoleConfig) []string {
	if rc == nil {
		return nil
	}
	names := make([]string, 0, 1+len(rc.Allowed))
	seen := make(map[string]bool, 1+len(rc.Allowed))
	add := func(name string) {
		if name == "" || seen[name] {
			return
		}
		seen[name] = true
		names = append(names, name)
	}
	add(rc.Model)
	for _, name := range rc.Allowed {
		add(name)
	}
	return names
}

// relayPolicyFieldCount is TestCheckRouteBindingCoversEveryRelayPolicyField's
// own reflection-count guard: sandbox.RoutePolicy's field count as of this
// writing. If that struct ever gains or loses a field, this constant goes
// stale and that test fails, forcing whoever changed RoutePolicy to also
// decide, explicitly, whether compareBoundRelayPolicy's field-by-field walk
// (which reflects over EVERY field with no allowlist to fall out of sync)
// already covers the new field correctly, or whether it needs its own entry
// in ceilingExemptRelayPolicyFields -- never a silent gap in the Worker's
// own binding check.
const relayPolicyFieldCount = 25

// ceilingExemptRelayPolicyFields are the only sandbox.RoutePolicy fields a
// submitted, routes:-named policy may LOWER from this Worker's own expected
// value without being refused: a target repo's own project config may
// tighten a run's budget ceiling after modelrole.SelectRoute already built
// its policy (cmd/factoryd's applyFinalRelayCeilings, the one place that
// adjusts a *modelrole.Selection's Policy post-selection, touches only
// TokenCeiling/CostCeilingMicroUSD -- grepped to confirm nothing else does;
// if a future change makes it touch another field, that field must be
// added here too, explicitly, not silently exempted by widening this map's
// keys). Every other field must match the Worker's own expected value
// EXACTLY -- see compareBoundRelayPolicy.
var ceilingExemptRelayPolicyFields = map[string]bool{
	"TokenCeiling":        true,
	"CostCeilingMicroUSD": true,
}

// compareBoundRelayPolicy is CheckRouteBinding's own field-by-field walk of
// p against expected (the Worker's own relayPolicyForRoute result for the
// route/model p claims): reflects over every sandbox.RoutePolicy field by
// name rather than a hand-maintained list of comparisons, so a field this
// package's own author forgets to add a check for can never silently pass
// unexamined -- see relayPolicyFieldCount's own doc comment for the guard
// that catches a field being added without updating either this function
// or its exemption map. A ceiling-exempt field must be strictly positive
// and no greater than expected's own value (0 < p <= expected -- a
// project config may only tighten, never raise or zero out, a ceiling);
// every other field must be identical, and a mismatch names the field,
// never the values (a submitted policy's own field value could
// otherwise leak a route detail this error message has no business
// repeating).
func compareBoundRelayPolicy(routeName string, p, expected sandbox.RoutePolicy) error {
	pv := reflect.ValueOf(p)
	ev := reflect.ValueOf(expected)
	t := pv.Type()
	for i := 0; i < t.NumField(); i++ {
		name := t.Field(i).Name
		if ceilingExemptRelayPolicyFields[name] {
			v := pv.Field(i).Int()
			if v <= 0 {
				return fmt.Errorf("modelrole: route %q: policy's %s must be positive", routeName, name)
			}
			if v > ev.Field(i).Int() {
				return fmt.Errorf("modelrole: route %q: policy's %s exceeds this Worker's own configured ceiling", routeName, name)
			}
			continue
		}
		if !reflect.DeepEqual(pv.Field(i).Interface(), ev.Field(i).Interface()) {
			return fmt.Errorf("modelrole: route %q: policy's %s does not match this Worker's own route/model", routeName, name)
		}
	}
	return nil
}

// CheckRouteBinding is the Temporal Worker's own trust check for a
// dispatched run's RoutePolicy/ReviewRelayPolicy: p names a route
// (p.Route) and a worker model id (p.WorkerModelID) the submitter
// selected via SelectRoute, carried credential-free in Workflow input --
// a Workflow input can be submitted by anything that can reach this
// Temporal namespace, not only by the cmd/factoryd process that resolved
// it, so this Worker must never simply trust ANY of p's fields, only
// what its OWN routes:/models: config, for role's own configured model(s)
// (roles.<role>.model plus roles.<role>.allowed -- candidateModelNames),
// would itself build.
//
// The mode itself is never decided by p: the caller (relaySpecFor)
// decides whether a policy is even eligible to reach this function at
// all based on ITS OWN configuration (Activities.CheckRoute nil or not),
// never on p.Route being empty or not -- see relaySpecFor's own doc
// comment. This function itself still refuses a routeless p (nothing to
// bind) and a Worker with no routes: config at all, as a defensive
// second check, not the primary mode switch.
//
// Binding: find the first of role's own candidate models that declares
// p.Route among its own Routes and whose EffectiveID(p.Route) equals
// p.WorkerModelID (no match -- wrong model, wrong route, or a role that
// isn't configured at all -- refuses naming the route and role, never
// the model id, which came from the submitter). Build this Worker's own
// expected RoutePolicy for that exact (model, route) pair
// (relayPolicyForRoute, the same builder SelectRoute itself uses) and
// compare it against p field-by-field (compareBoundRelayPolicy): every
// field must match exactly except TokenCeiling/CostCeilingMicroUSD,
// which may only be tightened (p <= expected), never raised -- a target
// repo's own project config can lower a run's ceiling after the
// submitter's own SelectRoute already built its policy, and that must
// still bind.
//
// Passing role, not just p.Route, on the credential side too: a review
// policy naming a route that IS one of role's models' routes but paired
// with a worker_model_id that belongs only to a DIFFERENT role's model
// still refuses here, since EffectiveID(p.Route) is checked against only
// role's own candidate models -- a review policy can never bind to an
// execution-only model, or vice versa.
//
// thinking is the submitter's own resolved Pi reasoning-effort level for
// this same role (RunWorkflowInput.Thinking for role Execution,
// RunWorkflowInput.ReviewThinking for a routed role Review) and must
// equal roleConfig(s, role).Thinking exactly -- this Worker's own
// roles.<role>.thinking, never the submitter's own claimed value, is
// what actually reaches build_app.py/conformity_review.py/code_review.py's
// --thinking, so the two must never be allowed to silently diverge.
func CheckRouteBinding(s sessionconfig.Settings, role Role, p sandbox.RoutePolicy, thinking string) error {
	if p.Route == "" {
		return fmt.Errorf("modelrole: CheckRouteBinding requires a policy naming a route")
	}
	rc := roleConfig(s, role)
	candidates := candidateModelNames(rc)
	if len(candidates) == 0 {
		return fmt.Errorf("modelrole: roles.%s is not configured on this Worker", role)
	}
	if thinking != rc.Thinking {
		return fmt.Errorf("modelrole: roles.%s: policy's thinking does not match this Worker's own roles.%s.thinking", role, role)
	}
	routes, models, err := s.Routing()
	if err != nil {
		return fmt.Errorf("modelrole: %w", err)
	}
	route, ok := routes[p.Route]
	if !ok {
		return fmt.Errorf("modelrole: roles.%s: route %q is not configured on this Worker", role, p.Route)
	}
	var matched sessionconfig.Model
	found := false
	for _, name := range candidates {
		model, ok := models[name]
		if !ok {
			continue
		}
		hasRoute := false
		for _, r := range model.Routes {
			if r == p.Route {
				hasRoute = true
				break
			}
		}
		if !hasRoute {
			continue
		}
		if model.EffectiveID(p.Route) != p.WorkerModelID {
			continue
		}
		matched = model
		found = true
		break
	}
	if !found {
		return fmt.Errorf("modelrole: roles.%s: policy's WorkerModelID does not match any of role's configured models on route %q", role, p.Route)
	}
	expected, err := relayPolicyForRoute(s, matched, p.Route, route)
	if err != nil {
		return fmt.Errorf("modelrole: roles.%s: %w", role, err)
	}
	return compareBoundRelayPolicy(p.Route, p, expected)
}

// harnessLookup resolves a harness name; a variable only so a test can inject
// a registry entry the compiled registry does not have yet.
var harnessLookup = harness.Lookup

// RoleHarness resolves role r's harness without selecting a route: the
// caller's per-request choice when non-empty (it must be one of
// roles.<r>.allowed_harnesses), else roles.<r>.harness, else pi. An
// unconfigured role resolves to pi and accepts no choice. It is the single
// place both SelectRoute and the pre-route requirement checks (a harness that
// needs its own sandbox image) resolve a role's harness, so they cannot
// disagree.
func RoleHarness(s sessionconfig.Settings, r Role, choice string) (string, error) {
	rc := roleConfig(s, r)
	if rc == nil {
		if choice != "" {
			return "", fmt.Errorf("modelrole: roles.%s is not configured; cannot choose harness %q for it", r, choice)
		}
		return harness.Pi, nil
	}
	if choice == "" {
		return rc.HarnessName(), nil
	}
	allowed := rc.AllowedHarnessNames()
	chosen := strings.ToLower(strings.TrimSpace(choice))
	if !slices.Contains(allowed, chosen) {
		return "", fmt.Errorf("modelrole: harness %q is not in roles.%s.allowed_harnesses %v", choice, r, allowed)
	}
	return chosen, nil
}
