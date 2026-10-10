// Schema and validation for an operator-configured routes:/models:/roles:
// session-config block -- the only session-config schema (see Config's
// own doc comment on Routes/Models).
package sessionconfig

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"

	"buildgate/internal/harness"
	"buildgate/internal/meter"
	"buildgate/internal/prices"
	"buildgate/internal/run"

	"gopkg.in/yaml.v3"
)

// Route is one named upstream/credential pairing a `routes:` block
// configures -- see Config's own doc comment on Routes. Unlike
// Model/RoleConfig, a Route legitimately names an upstream, an
// allowed path prefix, and a credential source: it IS the
// operator-configured trust boundary those other types must never be
// able to reach around (SC-015) -- so it carries no
// TestModelNeverSetsUpstreamOrPath-style reflection guard.
type Route struct {
	// CredentialMode is one of meter.CredentialModeStatic ("static", the
	// default when empty), meter.CredentialModeGitHubCopilot
	// ("github-copilot"), or meter.CredentialModeChatGPTCodex
	// ("chatgpt-codex").
	CredentialMode string `yaml:"credential_mode,omitempty"`
	// Upstream is required for a static route unless AllowNoCredential; a
	// chatgpt-codex route defaults to meter.ChatGPTCodexAPIBase and is
	// then pinned there (meter.ValidateChatGPTCodexRoute), and a
	// github-copilot route defaults to meter.CopilotAPIBase and is pinned
	// to a *.githubcopilot.com host (meter.ValidateGitHubCopilotRoute) --
	// see defaultedRoute/validateRoute.
	Upstream               string  `yaml:"upstream,omitempty"`
	AllowedPathPrefix      string  `yaml:"allowed_path_prefix,omitempty"`
	WorkerBasePath         *string `yaml:"worker_base_path,omitempty"`
	CredentialHeader       string  `yaml:"credential_header,omitempty"` // static only
	CredentialEnv          string  `yaml:"credential_env,omitempty"`    // static only; default ANTHROPIC_API_KEY
	GitHubTokenFile        string  `yaml:"github_token_file,omitempty"` // github-copilot only
	GitHubTokenKey         string  `yaml:"github_token_key,omitempty"`  // github-copilot only
	CodexAuthFile          string  `yaml:"codex_auth_file,omitempty"`   // chatgpt-codex only
	AllowPlaintextUpstream bool    `yaml:"allow_plaintext_upstream,omitempty"`
	AllowNoCredential      bool    `yaml:"allow_no_credential,omitempty"`
	// Billing labels spend for display only (run.BillingSubscription or
	// run.BillingMetered) -- it never changes enforcement. Empty means
	// "derive from CredentialMode" (see defaultedRoute).
	Billing string `yaml:"billing,omitempty"`
}

// effectiveCredentialMode is r.CredentialMode with its own empty-means-
// static default applied -- the single place every credential-mode-
// dependent default/check below reads from, so "empty" and "static" are
// never handled as two different cases by accident.
func (r Route) effectiveCredentialMode() string {
	if r.CredentialMode == "" {
		return meter.CredentialModeStatic
	}
	return r.CredentialMode
}

// defaultedRoute fills every default Routing()/validateRoute needs to
// apply -- in exactly the same way whether r came from an
// operator-authored routes: block or was synthesized from a legacy
// session config, so the two modes can never silently diverge on what
// "the default" means: a chatgpt-codex route defaults to
// meter.ChatGPTCodexAPIBase/meter.ChatGPTCodexResponsesPath/an empty
// worker base path (mirroring cmd/factoryd's own
// applyChatGPTCodexRelayDefaults), a github-copilot route defaults its
// upstream to meter.CopilotAPIBase (mirroring
// applyGitHubCopilotRelayDefaults's own upstream default -- its path-
// prefix default depends on which model/API reaches it, which a Route
// alone doesn't know, so that half is left to a later change that
// resolves a specific model against it), CredentialEnv defaults to
// ANTHROPIC_API_KEY for a static route, and Billing defaults from the
// effective credential mode.
func defaultedRoute(r Route) Route {
	mode := r.effectiveCredentialMode()
	switch mode {
	case meter.CredentialModeChatGPTCodex:
		if r.Upstream == "" {
			r.Upstream = meter.ChatGPTCodexAPIBase
		}
		if r.AllowedPathPrefix == "" {
			r.AllowedPathPrefix = meter.ChatGPTCodexResponsesPath
		}
		if r.WorkerBasePath == nil {
			empty := ""
			r.WorkerBasePath = &empty
		}
	case meter.CredentialModeGitHubCopilot:
		if r.Upstream == "" {
			r.Upstream = meter.CopilotAPIBase
		}
	}
	if mode == meter.CredentialModeStatic && r.CredentialEnv == "" {
		r.CredentialEnv = "ANTHROPIC_API_KEY"
	}
	if r.Billing == "" {
		switch mode {
		case meter.CredentialModeChatGPTCodex, meter.CredentialModeGitHubCopilot:
			r.Billing = run.BillingSubscription
		default:
			r.Billing = run.BillingMetered
		}
	}
	return r
}

// validateRoute checks one routes: entry's own internal consistency,
// independent of any model/role that might reference it: a known
// credential_mode; a known billing value; each mode-specific field
// (credential_env/credential_header are static-only; github_token_file/
// _key are github-copilot-only; codex_auth_file is chatgpt-codex-only)
// is refused on a route in the wrong mode, rather than silently ignored
// -- an operator who sets e.g. codex_auth_file on a static route almost
// certainly made a mistake, and a silently-ignored field is a worse
// failure mode than a loud one. A static route needs an upstream unless
// it explicitly opts out (AllowNoCredential); a chatgpt-codex or
// github-copilot route is validated, on its own defaulted upstream/path
// (defaultedRoute), against the same host pins cmd/factoryd's own relay
// launch enforces (meter.ValidateChatGPTCodexRoute/
// ValidateGitHubCopilotRoute) -- so an operator override that the pin
// would reject at launch (a leftover upstream from another route, a
// typo'd path) is refused here too, at validation time, not only when a
// job actually launches a relay against it.
func validateRoute(name string, r Route) error {
	switch r.CredentialMode {
	case "", meter.CredentialModeStatic, meter.CredentialModeGitHubCopilot, meter.CredentialModeChatGPTCodex:
	default:
		return fmt.Errorf("routes.%s: credential_mode %q is not one of static, github-copilot, chatgpt-codex", name, r.CredentialMode)
	}
	switch r.Billing {
	case "", run.BillingSubscription, run.BillingMetered:
	default:
		return fmt.Errorf("routes.%s: billing %q is not one of %q, %q", name, r.Billing, run.BillingSubscription, run.BillingMetered)
	}

	mode := r.effectiveCredentialMode()
	defaulted := defaultedRoute(r)
	switch mode {
	case meter.CredentialModeChatGPTCodex:
		workerBasePath := ""
		if defaulted.WorkerBasePath != nil {
			workerBasePath = *defaulted.WorkerBasePath
		}
		if err := meter.ValidateChatGPTCodexRoute(defaulted.Upstream, defaulted.AllowedPathPrefix, workerBasePath); err != nil {
			return fmt.Errorf("routes.%s: %w", name, err)
		}
	case meter.CredentialModeGitHubCopilot:
		if err := meter.ValidateGitHubCopilotRoute(defaulted.Upstream); err != nil {
			return fmt.Errorf("routes.%s: %w", name, err)
		}
	default: // static
		if r.Upstream == "" && !r.AllowNoCredential {
			return fmt.Errorf("routes.%s: upstream is required for a static route (or set allow_no_credential: true)", name)
		}
	}

	if r.CredentialEnv != "" && mode != meter.CredentialModeStatic {
		return fmt.Errorf("routes.%s: credential_env only applies to a static route", name)
	}
	if r.CredentialHeader != "" && mode != meter.CredentialModeStatic {
		return fmt.Errorf("routes.%s: credential_header only applies to a static route", name)
	}
	if (r.GitHubTokenFile != "" || r.GitHubTokenKey != "") && mode != meter.CredentialModeGitHubCopilot {
		return fmt.Errorf("routes.%s: github_token_file/github_token_key only apply to a github-copilot route", name)
	}
	if r.CodexAuthFile != "" && mode != meter.CredentialModeChatGPTCodex {
		return fmt.Errorf("routes.%s: codex_auth_file only applies to a chatgpt-codex route", name)
	}
	return nil
}

// Model is one named model a `models:` block configures -- see Config's
// own doc comment on Models. It may never itself name an upstream,
// allowed path prefix, script, or interpreter (SC-015): only
// Routes says which already-operator-configured Route(s) this model may
// reach, in order, and only ever picks among them -- never another model.
// See TestModelNeverSetsUpstreamOrPath.
type Model struct {
	// ID is the upstream model id (e.g. "gpt-5.6-luna"). Required by
	// ValidateRouting; may be left empty only on a Model a caller builds
	// directly (never through Load+ValidateRouting) for a base-path-only
	// local route.
	ID string `yaml:"id,omitempty"`
	// API is the request shape ("openai-responses"/"openai-completions");
	// empty defers to whatever the resolving route's usage format implies.
	API string `yaml:"api,omitempty"`
	// Routes is the ordered fallback list of Route names this model may
	// resolve through -- never another model. Required, non-empty, in
	// routes: mode.
	Routes []string `yaml:"routes,omitempty"`
	// RouteIDs overrides ID per route, for a model whose id differs by
	// backend (e.g. a Copilot-hosted id that differs from its codex id).
	// Every key must itself be one of Routes.
	RouteIDs         map[string]string `yaml:"route_ids,omitempty"`
	ContextWindow    int               `yaml:"context_window,omitempty"`
	Reasoning        bool              `yaml:"reasoning,omitempty"`
	ThinkingLevelMap map[string]string `yaml:"thinking_level_map,omitempty"`
	ExtraJSON        map[string]any    `yaml:"extra_json,omitempty"`
}

// effectiveID returns m's own model id for route, applying RouteIDs'
// per-route override when one is declared for it.
func (m Model) effectiveID(route string) string {
	if id, ok := m.RouteIDs[route]; ok && id != "" {
		return id
	}
	return m.ID
}

// EffectiveID is effectiveID, exported for internal/modelrole's
// SelectRoute (Phase 2C-1): the resolved-per-route model id a caller
// outside this package needs to fill RoutePolicy.WorkerModelID.
func (m Model) EffectiveID(route string) string { return m.effectiveID(route) }

// EffectiveCredentialMode is Route.effectiveCredentialMode, exported for
// internal/modelrole's SelectRoute: the same empty-means-static default
// every credential-mode-dependent check in this package already applies.
func (r Route) EffectiveCredentialMode() string { return r.effectiveCredentialMode() }

// effectiveAPI returns m's own request shape for route: m.API verbatim
// when set, else the one default every route kind actually forces today
// (a chatgpt-codex route only ever speaks the Responses API --
// see meter.ChatGPTCodexResponsesPath's own doc comment -- and every
// other route defaults to the same Completions shape
// DefaultSettings.RelayWorkerAPI already does).
func (m Model) effectiveAPI(route Route) string {
	if m.API != "" {
		return m.API
	}
	if route.CredentialMode == meter.CredentialModeChatGPTCodex {
		return meter.RequestFormatOpenAIResponses
	}
	return meter.RequestFormatOpenAICompletions
}

// EffectiveAPI is effectiveAPI, exported for internal/modelrole's
// SelectRoute: the resolved-per-route request shape it needs to fill
// RoutePolicy.WorkerModelAPI (and to derive UsageFormat from -- there is
// no session or per-request override of UsageFormat in routes: mode).
func (m Model) EffectiveAPI(route Route) string { return m.effectiveAPI(route) }

// backendIdentity is the resolved-backend tuple the review/execution
// independence rule compares by: which real upstream a model, reached
// through one particular route, actually talks to, and which id/API it
// sends there -- not which route NAME reaches it. Two models reachable
// through differently-named routes that both resolve to this same tuple
// are the identical backend in every way that matters (the same
// credential is forced onto the same upstream, asking for the same
// model in the same shape), so an operator can't dodge the independence
// rule by simply giving two routes to the same backend different names.
type backendIdentity struct {
	credentialMode string
	upstream       string
	id             string
	api            string
}

// modelBackendIdentities returns m's own backendIdentity for every route
// in m.Routes that actually exists in routes -- a route name Routing()/
// checkModel has already refused if missing, so this is defensive only.
func modelBackendIdentities(m Model, routes map[string]Route) []backendIdentity {
	identities := make([]backendIdentity, 0, len(m.Routes))
	for _, routeName := range m.Routes {
		route, ok := routes[routeName]
		if !ok {
			continue
		}
		defaulted := defaultedRoute(route)
		identities = append(identities, backendIdentity{
			credentialMode: defaulted.effectiveCredentialMode(),
			upstream:       defaulted.Upstream,
			id:             m.effectiveID(routeName),
			api:            m.effectiveAPI(defaulted),
		})
	}
	return identities
}

// worker-model-JSON's own always-reserved keys: id/api/baseUrl are
// RoutePolicy fields this composition's caller sets around whatever
// WorkerModelJSON returns, so extra_json has no legitimate reason to
// carry any of them, regardless of whether the corresponding first-class
// field is even set (unlike contextWindow/reasoning/thinkingLevelMap
// below, there is no "first-class field left unset" case where letting
// one of these three through would be correct).
var alwaysReservedWorkerModelJSONKeys = []string{"id", "api", "baseUrl"}

// WorkerModelJSON composes m's worker model JSON: extra_json plus
// contextWindow/reasoning/thinkingLevelMap written in from the
// first-class fields above -- ending the replace-not-merge trap a
// session-level worker_model_extra_json/relay_worker_model_extra_json
// default had, since a model now owns its whole extra object rather than
// layering a session-level default underneath it. route, when non-empty,
// must be one of m.Routes -- SC-015's "never another model, only its own
// declared routes" invariant enforced structurally here rather than
// trusted to every caller; a model declaring no Routes at all can never
// pass a non-empty route (there is nothing for the empty Routes list to
// have granted).
//
// A first-class field only refuses (rather than silently overwrites) the
// same-named key when extra_json ALSO sets it: id/api/baseUrl are
// refused unconditionally; contextWindow/reasoning/thinkingLevelMap are
// refused only when the corresponding first-class field is actually set
// (non-zero/true/non-empty) -- when it is not, the extra_json value
// (whatever shape it has) simply passes through untouched, which is what
// a legacy alias's pre-existing extra_json needs (see
// extractFirstClassFields's own doc comment for the synthesis side of
// this: a legacy alias's own extra_json can use "reasoning" for an
// unrelated, non-boolean, vendor-specific parameter, which extraction
// deliberately leaves alone).
func (m Model) WorkerModelJSON(route string) (string, error) {
	if route != "" && (len(m.Routes) == 0 || !slices.Contains(m.Routes, route)) {
		return "", fmt.Errorf("model: route %q is not one of this model's routes %v", route, m.Routes)
	}
	for _, key := range alwaysReservedWorkerModelJSONKeys {
		if _, conflict := m.ExtraJSON[key]; conflict {
			return "", fmt.Errorf("model: extra_json.%s conflicts with a first-class model field; remove it from extra_json", key)
		}
	}
	out := make(map[string]any, len(m.ExtraJSON)+3)
	for k, v := range m.ExtraJSON {
		out[k] = v
	}
	if m.ContextWindow != 0 {
		if _, conflict := m.ExtraJSON["contextWindow"]; conflict {
			return "", fmt.Errorf("model: extra_json.contextWindow conflicts with a first-class model field; remove it from extra_json")
		}
		out["contextWindow"] = m.ContextWindow
	}
	if m.Reasoning {
		if _, conflict := m.ExtraJSON["reasoning"]; conflict {
			return "", fmt.Errorf("model: extra_json.reasoning conflicts with a first-class model field; remove it from extra_json")
		}
		out["reasoning"] = true
	}
	if len(m.ThinkingLevelMap) > 0 {
		if _, conflict := m.ExtraJSON["thinkingLevelMap"]; conflict {
			return "", fmt.Errorf("model: extra_json.thinkingLevelMap conflicts with a first-class model field; remove it from extra_json")
		}
		out["thinkingLevelMap"] = m.ThinkingLevelMap
	}
	encoded, err := json.Marshal(out)
	if err != nil {
		return "", fmt.Errorf("model: encode worker model json: %w", err)
	}
	return string(encoded), nil
}

// Routing returns a defaulted copy of s.Routes/s.Models: the routes/
// models a job resolves against. defaultedRoute applies uniformly so
// credential_env/billing defaults are never computed twice.
func (s Settings) Routing() (routes map[string]Route, models map[string]Model, err error) {
	routes = make(map[string]Route, len(s.Routes))
	for name, r := range s.Routes {
		routes[name] = defaultedRoute(r)
	}
	return routes, s.Models, nil
}

// legacyRoutingKeys names every top-level YAML key a past schema version
// once accepted, now moved under routes:/models:/roles: -- Load refuses
// any config that still writes one of these, naming it, rather than the
// generic "field X not found" KnownFields would otherwise produce for a
// deleted Config field. TestNoConfigYAMLTagOverlapsLegacyRoutingKeys
// guards against a future field reusing one of these names by mistake.
var legacyRoutingKeys = []string{
	"relay_upstream",
	"relay_allowed_path_prefix",
	"relay_credential_header",
	"relay_credential_mode",
	"relay_github_token_file",
	"relay_github_token_key",
	"relay_codex_auth_file",
	"relay_allow_plaintext_upstream",
	"relay_allow_no_credential",
	"relay_worker_model_id",
	"relay_worker_api",
	"relay_worker_base_path",
	"relay_worker_model_extra_json",
	"relay_usage_format",
	"model_aliases",
	"relay_cost_per_input_token_micro_usd",
	"relay_cost_per_output_token_micro_usd",
	"engine",
	"relay_image",
	"relay_upstream_timeout",
	"sandbox_pids",
	"relay_max_request_bytes",
	"relay_requests_per_minute",
	"relay_token_budget",
	"relay_token_budget_window",
	"relay_cost_budget_micro_usd",
	"relay_cost_budget_window",
	"relay_token_ceiling",
	"relay_cost_ceiling_micro_usd",
}

// legacyRoutingKeyHint is the migration text Load's legacyRoutingKeys
// check reports for each retired key: "<key>: <hint>". Every key
// not listed here defaults to "moved to routes:/models:/roles: (see
// USAGE_REFERENCE.md \"Model routes\")". The two retired per-token session
// prices moved somewhere different -- the compiled price table
// (internal/prices/prices.yml) -- since 2026-09-28: a real provider price is
// no longer a session-config knob at all, operator-set or defaulted.
var legacyRoutingKeyHint = map[string]string{
	"engine":                                "moved to roles.<role>.harness (pi or pifork; see USAGE_REFERENCE.md \"Model routes\")",
	"relay_cost_per_input_token_micro_usd":  "moved to internal/prices/prices.yml (see USAGE_REFERENCE.md \"Model prices\")",
	"relay_cost_per_output_token_micro_usd": "moved to internal/prices/prices.yml (see USAGE_REFERENCE.md \"Model prices\")",
	"relay_image":                           "deleted with the inference relay (OpenShell is the sandbox runtime); remove this line",
	"relay_upstream_timeout":                "deleted with the inference relay (OpenShell is the sandbox runtime); remove this line",
	"sandbox_pids":                          "deleted: a worker's process limit is the OpenShell gateway's own; remove this line",
	"relay_max_request_bytes":               "renamed to meter_max_request_bytes (the meter enforces it)",
	"relay_requests_per_minute":             "renamed to meter_requests_per_minute (the meter enforces it)",
	"relay_token_budget":                    "renamed to meter_token_budget (the meter enforces it)",
	"relay_token_budget_window":             "renamed to meter_token_budget_window (the meter enforces it)",
	"relay_cost_budget_micro_usd":           "renamed to meter_cost_budget_micro_usd (the meter enforces it)",
	"relay_cost_budget_window":              "renamed to meter_cost_budget_window (the meter enforces it)",
	"relay_token_ceiling":                   "renamed to meter_token_ceiling (the meter enforces it)",
	"relay_cost_ceiling_micro_usd":          "renamed to meter_cost_ceiling_micro_usd (the meter enforces it)",
}

func legacyRoutingKeyHintFor(key string) string {
	if hint, ok := legacyRoutingKeyHint[key]; ok {
		return hint
	}
	return "moved to routes:/models:/roles: (see USAGE_REFERENCE.md \"Model routes\")"
}

// retiredModelPriceKeys are the per-model cost_per_* fields deleted from
// Model (2026-09-28) in favor of a real provider price looked up from
// internal/prices by the model's own id. retiredModelPriceKey scans a
// config's models: section for either, so Load can refuse it with a
// migration hint naming exactly where it was, rather than KnownFields'
// generic "field not found" for a deleted Model field.
var retiredModelPriceKeys = []string{
	"cost_per_input_token_micro_usd",
	"cost_per_output_token_micro_usd",
}

// retiredModelPriceKey scans data's top-level models: mapping for the first
// model entry that still sets one of retiredModelPriceKeys, returning that
// model's own name and the offending key.
func retiredModelPriceKey(data []byte) (modelName, key string, found bool) {
	var node yaml.Node
	if err := yaml.Unmarshal(data, &node); err != nil || len(node.Content) == 0 {
		return "", "", false
	}
	doc := node.Content[0]
	if doc.Kind != yaml.MappingNode {
		return "", "", false
	}
	for i := 0; i+1 < len(doc.Content); i += 2 {
		if doc.Content[i].Value != "models" {
			continue
		}
		modelsNode := doc.Content[i+1]
		if modelsNode.Kind != yaml.MappingNode {
			return "", "", false
		}
		for j := 0; j+1 < len(modelsNode.Content); j += 2 {
			name := modelsNode.Content[j].Value
			entry := modelsNode.Content[j+1]
			if entry.Kind != yaml.MappingNode {
				continue
			}
			for k := 0; k+1 < len(entry.Content); k += 2 {
				for _, retired := range retiredModelPriceKeys {
					if entry.Content[k].Value == retired {
						return name, retired, true
					}
				}
			}
		}
		return "", "", false
	}
	return "", "", false
}

// ValidateRouting is the only validator for the routes:/models:/roles:
// schema. A config with no routes:, models:, or roles: at all is valid
// (an offline build with an explicit -build-app-script needs no relay);
// any launch that actually needs a relay with no roles.execution
// configured must fail this check first (see the roles.execution check
// below) rather than only failing later, opaquely, when it tries to
// build a relay policy with nothing to resolve.
//
// Otherwise this requires: every routes: entry to be internally
// consistent, including a chatgpt-codex/github-copilot route's own
// upstream host pin (validateRoute); every models: entry, referenced by
// a role or not, to have a non-empty id, a non-empty valid Routes list,
// route_ids keys that are themselves all declared routes, and no
// extra_json/first-class field conflict (checkModel); roles.execution to
// be configured (no roles: at all, or roles: with execution unset, has
// no way to pick a model for the one role every build needs); a role's
// thinking checked against every model in its own allowed list, not just
// its default model; and, unless roles.review.allow_shared_model is set,
// no model in roles.review's own allowed list to resolve to the same
// backend (backendIdentity: credential_mode + upstream, both after
// defaults, plus effective id and effective API) as any model in
// roles.execution.allowed -- compared by backend, not by route name, so
// two differently-named routes to the same real upstream are still
// caught, and with no special case for two resolved backends that
// happen to both carry an empty id/upstream (every models: entry
// requires a non-empty id and every route has a defaulted, non-empty
// upstream by the time this comparison runs, so that case can't occur in
// practice, but the comparison itself carries no exemption that could
// silently reopen it).
func resolveAllowed(rc *RoleConfig) []string {
	if len(rc.Allowed) > 0 {
		return rc.Allowed
	}
	return []string{rc.Model}
}

func ValidateRouting(s Settings) error {
	if s.Routes == nil && s.Models == nil && s.Roles == nil {
		return nil
	}

	for name, r := range s.Routes {
		if err := validateRoute(name, r); err != nil {
			return err
		}
	}

	// Every declared model is validated, referenced by a role or not: an
	// operator who defines a broken models: entry they haven't wired to
	// any role yet must still learn about it now, not the day they
	// finally reference it.
	for name := range s.Models {
		if _, err := checkRoutingModel(s, "models", name); err != nil {
			return err
		}
	}

	if s.Roles == nil || s.Roles.Execution == nil {
		return errors.New("roles.execution is required to launch a relay; configure routes:/models:/roles: (see USAGE_REFERENCE.md \"Model routes\")")
	}

	if err := checkRoutingRole(s, "planning", s.Roles.Planning); err != nil {
		return err
	}
	if err := checkRoutingRole(s, "execution", s.Roles.Execution); err != nil {
		return err
	}
	if err := checkRoutingRole(s, "review", s.Roles.Review); err != nil {
		return err
	}

	return checkReviewBackendDiffers(s)
}

// checkRoutingModel returns the models: entry name, or why it is unusable.
// context names who referenced it, for the error of a name that is no entry.
func checkRoutingModel(s Settings, context, name string) (Model, error) {
	model, ok := s.Models[name]
	if !ok {
		return Model{}, fmt.Errorf("%s: model %q is not a models: entry", context, name)
	}
	if model.ID == "" {
		return Model{}, fmt.Errorf("models.%s: id is required", name)
	}
	if len(model.Routes) == 0 {
		return Model{}, fmt.Errorf("models.%s: routes is required (at least one)", name)
	}
	for _, r := range model.Routes {
		if _, ok := s.Routes[r]; !ok {
			return Model{}, fmt.Errorf("models.%s: route %q is not a routes: entry", name, r)
		}
	}
	for r := range model.RouteIDs {
		if !slices.Contains(model.Routes, r) {
			return Model{}, fmt.Errorf("models.%s: route_ids names %q, which is not in this model's own routes %v", name, r, model.Routes)
		}
	}
	if _, err := model.WorkerModelJSON(""); err != nil {
		return Model{}, fmt.Errorf("models.%s: %w", name, err)
	}
	// A model with no price-table entry is allowed (it costs $0; doctor
	// warns); only a malformed compiled table is an error here.
	if _, err := prices.Lookup(model.ID); err != nil && !errors.Is(err, prices.ErrNoPrice) {
		return Model{}, err
	}
	return model, nil
}

// checkRoutingRole validates one roles: entry; a role that is not configured
// passes.
func checkRoutingRole(s Settings, roleName string, rc *RoleConfig) error {
	if rc == nil {
		return nil
	}
	if rc.Model == "" {
		return fmt.Errorf("roles.%s: model is required", roleName)
	}
	allowedNames := resolveAllowed(rc)
	if !slices.Contains(allowedNames, rc.Model) {
		return fmt.Errorf("roles.%s: model %q must be included in allowed", roleName, rc.Model)
	}
	for _, name := range allowedNames {
		model, err := checkRoutingModel(s, fmt.Sprintf("roles.%s.allowed", roleName), name)
		if err != nil {
			return err
		}
		if rc.Thinking == "" || rc.Thinking == "off" {
			continue
		}
		if !validThinkingLevels[rc.Thinking] {
			return fmt.Errorf("roles.%s: thinking %q is not a supported level (want one of off, minimal, low, medium, high, xhigh, max)", roleName, rc.Thinking)
		}
		if !model.Reasoning {
			return fmt.Errorf("roles.%s: thinking %q needs models.%s.reasoning: true (Pi sends no reasoning effort at all without it)", roleName, rc.Thinking, name)
		}
		if rc.Thinking == "xhigh" || rc.Thinking == "max" {
			if model.ThinkingLevelMap[rc.Thinking] == "" {
				return fmt.Errorf("roles.%s: thinking %q needs models.%s.thinking_level_map.%s (Pi silently clamps an undeclared level to high)", roleName, rc.Thinking, name, rc.Thinking)
			}
		}
	}
	return checkRoleHarness(roleName, rc, s.Routes, s.Models)
}

// checkReviewBackendDiffers refuses a review model that resolves to the same
// backend as an execution model, unless roles.review.allow_shared_model
// waives it.
func checkReviewBackendDiffers(s Settings) error {
	if s.Roles.Review == nil || s.Roles.Review.AllowSharedModel {
		return nil
	}
	for _, reviewName := range resolveAllowed(s.Roles.Review) {
		reviewModel, err := checkRoutingModel(s, "roles.review.allowed", reviewName)
		if err != nil {
			return err
		}
		reviewIdentities := modelBackendIdentities(reviewModel, s.Routes)
		for _, execName := range resolveAllowed(s.Roles.Execution) {
			execModel, err := checkRoutingModel(s, "roles.execution.allowed", execName)
			if err != nil {
				return err
			}
			for _, ri := range reviewIdentities {
				for _, ei := range modelBackendIdentities(execModel, s.Routes) {
					if ri == ei {
						return fmt.Errorf("roles.review: model %q resolves to the same backend as roles.execution.allowed %q (credential_mode=%s upstream=%s id=%s api=%s); set roles.review.allow_shared_model: true to waive this", reviewName, execName, ri.credentialMode, ri.upstream, ri.id, ri.api)
					}
				}
			}
		}
	}
	return nil
}

// ValidateRequestModels checks a request's own per-request model picks
// (request.Request.Models / requestsubmit.Params.Models -- role name ->
// models: entry) before they're ever recorded: routes:/models:/roles:
// session config is required whenever models is non-empty (a legacy
// -relay-* session has no roles.<role>.allowed to validate against);
// each key must be "planning" or "execution" -- review is never
// requester-selectable, by design, not as a gap to close later; and each
// value must be a member of that role's own resolved allowed set (its
// roles.<role>.allowed, or just [roles.<role>.model] when allowed is
// empty -- the same resolution ValidateRouting itself applies). The
// factory still owns every other per-role setting (routes, thinking);
// this only ever narrows a request to a model the operator's own config
// already permits for that role.
func ValidateRequestModels(s Settings, models map[string]string) error {
	if len(models) == 0 {
		return nil
	}
	if s.Routes == nil && s.Models == nil && s.Roles == nil {
		return errors.New("models: requires routes:/models:/roles: session config; this session has no roles.<role>.allowed to validate a per-request model choice against")
	}

	roleNames := make([]string, 0, len(models))
	for role := range models {
		roleNames = append(roleNames, role)
	}
	sort.Strings(roleNames)

	for _, role := range roleNames {
		model := models[role]
		var rc *RoleConfig
		switch role {
		case "planning":
			if s.Roles != nil {
				rc = s.Roles.Planning
			}
		case "execution":
			if s.Roles != nil {
				rc = s.Roles.Execution
			}
		default:
			return fmt.Errorf("models: role %q is not selectable; only \"planning\" and \"execution\" can be chosen per-request (review is never requester-selectable)", role)
		}
		if rc == nil || rc.Model == "" {
			return fmt.Errorf("models: roles.%s is not configured; cannot choose a model for it", role)
		}
		allowed := resolveAllowed(rc)
		if !slices.Contains(allowed, model) {
			return fmt.Errorf("models: model %q is not in roles.%s.allowed %v", model, role, allowed)
		}
	}
	return nil
}

// harnessLookup resolves a harness name; a variable only so a test can
// inject a registry entry the compiled registry does not have yet.
var harnessLookup = harness.Lookup

// HarnessName is the role's default harness, canonical: lower-cased,
// trimmed, "" meaning pi.
func (rc *RoleConfig) HarnessName() string {
	return canonicalHarnessName(rc.Harness)
}

// AllowedHarnessNames is the closed set of harnesses a request may pick for
// this role: AllowedHarnesses, or just the default when that is empty.
func (rc *RoleConfig) AllowedHarnessNames() []string {
	if len(rc.AllowedHarnesses) == 0 {
		return []string{rc.HarnessName()}
	}
	names := make([]string, len(rc.AllowedHarnesses))
	for i, n := range rc.AllowedHarnesses {
		names[i] = canonicalHarnessName(n)
	}
	return names
}

func canonicalHarnessName(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		return harness.Pi
	}
	return name
}

// CheckHarnessAPI refuses a role whose harness cannot speak the api its
// resolved model uses. The message names role, harness, model and api.
func CheckHarnessAPI(role string, d harness.Descriptor, modelName, api string) error {
	if d.SupportsAPI(api) {
		return nil
	}
	return fmt.Errorf("roles.%s: harness %q cannot speak model %q's api %q (it supports %s); pick another harness or set models.%s.api", role, d.Name, modelName, api, strings.Join(d.WireAPIs, ", "), modelName)
}

// checkRoleHarness validates a role's harness choices: every named harness is
// a registry name, the default is in the allowed set, and every allowed
// harness can speak the api of every model/route the role may resolve to.
func checkRoleHarness(role string, rc *RoleConfig, routes map[string]Route, models map[string]Model) error {
	descriptors := map[string]harness.Descriptor{}
	names := append([]string{rc.HarnessName()}, rc.AllowedHarnessNames()...)
	for _, name := range names {
		d, err := harnessLookup(name)
		if err != nil {
			return fmt.Errorf("roles.%s: %w", role, err)
		}
		descriptors[name] = d
	}
	if !slices.Contains(rc.AllowedHarnessNames(), rc.HarnessName()) {
		return fmt.Errorf("roles.%s: harness %q must be included in allowed_harnesses", role, rc.HarnessName())
	}
	for _, hn := range rc.AllowedHarnessNames() {
		for _, mn := range resolveAllowed(rc) {
			model := models[mn]
			for _, routeName := range model.Routes {
				route, ok := routes[routeName]
				if !ok {
					continue
				}
				if err := CheckHarnessAPI(role, descriptors[hn], mn, model.effectiveAPI(defaultedRoute(route))); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// ValidateRequestHarnesses checks a request's per-request harness picks
// (request.Request.Harnesses -- role name -> harness name) the way
// ValidateRequestModels checks model picks: only "planning" and "execution"
// are selectable (review never is), the role must be configured, and the
// value must be in that role's allowed harness set.
func ValidateRequestHarnesses(s Settings, harnesses map[string]string) error {
	if len(harnesses) == 0 {
		return nil
	}
	if s.Roles == nil {
		return errors.New("harnesses: requires roles: session config; this session has no roles.<role>.allowed_harnesses to validate a per-request harness choice against")
	}
	roleNames := make([]string, 0, len(harnesses))
	for role := range harnesses {
		roleNames = append(roleNames, role)
	}
	sort.Strings(roleNames)
	for _, role := range roleNames {
		var rc *RoleConfig
		switch role {
		case "planning":
			rc = s.Roles.Planning
		case "execution":
			rc = s.Roles.Execution
		default:
			return fmt.Errorf("harnesses: role %q is not selectable; only \"planning\" and \"execution\" can be chosen per-request (review is never requester-selectable)", role)
		}
		if rc == nil {
			return fmt.Errorf("harnesses: roles.%s is not configured; cannot choose a harness for it", role)
		}
		if _, err := harnessLookup(harnesses[role]); err != nil {
			return fmt.Errorf("harnesses: %w", err)
		}
		chosen := canonicalHarnessName(harnesses[role])
		if allowed := rc.AllowedHarnessNames(); !slices.Contains(allowed, chosen) {
			return fmt.Errorf("harnesses: harness %q is not in roles.%s.allowed_harnesses %v", chosen, role, allowed)
		}
	}
	return nil
}
