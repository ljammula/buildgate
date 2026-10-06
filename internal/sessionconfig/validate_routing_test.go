package sessionconfig

import (
	"reflect"
	"strings"
	"testing"
)

// routesModeSettings returns a minimal, valid routes: mode Settings: one
// static route, one model reaching it, and an execution role -- the
// starting point every negative test below mutates one field of.
func routesModeSettings() Settings {
	return Settings{
		Routes: map[string]Route{
			"litellm": {CredentialMode: "static", Upstream: "https://litellm.example.invalid"},
		},
		Models: map[string]Model{
			"luna": {ID: "gpt-5.6-luna", Routes: []string{"litellm"}},
		},
		Roles: &Roles{
			Execution: &RoleConfig{Model: "luna"},
		},
	}
}

// TestValidateRoutingOKWhenRoutesAbsent proves an empty config (no
// routes:/models:/roles: at all, e.g. an offline build) passes
// unconditionally, regardless of what else is or isn't set.
func TestValidateRoutingOKWhenRoutesAbsent(t *testing.T) {
	if err := ValidateRouting(Settings{}); err != nil {
		t.Fatalf("ValidateRouting: %v, want nil when routes: is absent", err)
	}
}

// TestValidateRoutingAcceptsAMinimalValidRoutesConfig is the positive
// counterpart every negative test below mutates away from.
func TestValidateRoutingAcceptsAMinimalValidRoutesConfig(t *testing.T) {
	if err := ValidateRouting(routesModeSettings()); err != nil {
		t.Fatalf("ValidateRouting: %v, want nil for a minimal valid routes: config", err)
	}
}

// TestConfigWithoutRoutesIsValidForOfflineBuild proves a config with no
// routes:/models:/roles: at all is valid: an offline build with an
// explicit -build-app-script needs no relay at all.
func TestConfigWithoutRoutesIsValidForOfflineBuild(t *testing.T) {
	if err := ValidateRouting(Settings{}); err != nil {
		t.Fatalf("ValidateRouting: %v, want nil for a config with no routes:/models:/roles: at all", err)
	}
}

// TestLoadRefusesLegacyRoutingKeyWithMigrationHint proves Load refuses
// every legacy session-relay/model key outright, naming the offending
// key and pointing at its own migration target (routes:/models:/roles:
// for most; internal/prices/prices.yml for the two retired per-token
// session prices -- see legacyRoutingKeyHint), rather than the generic
// "field X not found" KnownFields would otherwise produce for a deleted
// Config field.
func TestLoadRefusesLegacyRoutingKeyWithMigrationHint(t *testing.T) {
	for _, key := range legacyRoutingKeys {
		t.Run(key, func(t *testing.T) {
			path := writeConfig(t, key+": x\n")
			_, err := Load(path)
			if err == nil {
				t.Fatalf("Load: err = nil, want a refusal naming %q", key)
			}
			if !strings.Contains(err.Error(), key) {
				t.Errorf("err = %v, want it to name %q", err, key)
			}
			if !strings.Contains(err.Error(), legacyRoutingKeyHintFor(key)) {
				t.Errorf("err = %v, want it to point at %q", err, legacyRoutingKeyHintFor(key))
			}
		})
	}
}

// TestValidateRoutingRequiresExecutionRole proves routes: mode refuses a
// config with no roles.execution at all -- there is no legacy default
// model/route left to fall back to once routes: is present.
func TestValidateRoutingRequiresExecutionRole(t *testing.T) {
	s := routesModeSettings()
	s.Roles = nil
	err := ValidateRouting(s)
	if err == nil {
		t.Fatal("ValidateRouting: err = nil, want a refusal when roles.execution is unset")
	}
	if !strings.Contains(err.Error(), "roles.execution") {
		t.Errorf("err = %v, want it to name roles.execution", err)
	}
}

// TestValidateRoutingRejectsUnknownModel proves a role naming a models:
// entry that does not exist is refused, naming the role.
func TestValidateRoutingRejectsUnknownModel(t *testing.T) {
	s := routesModeSettings()
	s.Roles.Execution.Model = "does-not-exist"
	err := ValidateRouting(s)
	if err == nil {
		t.Fatal("ValidateRouting: err = nil, want a refusal for an unknown model")
	}
	if !strings.Contains(err.Error(), "roles.execution") {
		t.Errorf("err = %v, want it to name roles.execution", err)
	}
}

// TestValidateRoutingRejectsModelRouteNotInRoutes proves a models: entry
// naming a route that isn't in routes: is refused.
func TestValidateRoutingRejectsModelRouteNotInRoutes(t *testing.T) {
	s := routesModeSettings()
	s.Models["luna"] = Model{ID: "gpt-5.6-luna", Routes: []string{"does-not-exist"}}
	err := ValidateRouting(s)
	if err == nil {
		t.Fatal("ValidateRouting: err = nil, want a refusal for a route not in routes:")
	}
	if !strings.Contains(err.Error(), "does-not-exist") {
		t.Errorf("err = %v, want it to name the unknown route", err)
	}
}

// TestValidateRoutingRejectsMissingModelID proves a models: entry with no
// id is refused -- id is required in routes: mode (unlike the legacy
// synthesized "" session-default model, which may leave it empty for a
// base-path-only local route).
func TestValidateRoutingRejectsMissingModelID(t *testing.T) {
	s := routesModeSettings()
	s.Models["luna"] = Model{Routes: []string{"litellm"}}
	err := ValidateRouting(s)
	if err == nil || !strings.Contains(err.Error(), "models.luna: id is required") {
		t.Errorf("ValidateRouting = %v, want models.luna: id is required", err)
	}
}

// TestValidateRoutingRejectsRouteIDsKeyNotInRoutes proves route_ids can
// only override a route the model itself already declares.
func TestValidateRoutingRejectsRouteIDsKeyNotInRoutes(t *testing.T) {
	s := routesModeSettings()
	s.Models["luna"] = Model{
		ID:       "gpt-5.6-luna",
		Routes:   []string{"litellm"},
		RouteIDs: map[string]string{"codex": "gpt-5.6-luna-codex"},
	}
	err := ValidateRouting(s)
	if err == nil || !strings.Contains(err.Error(), "route_ids") {
		t.Errorf("ValidateRouting = %v, want a refusal naming route_ids", err)
	}
}

// TestValidateRoutingRejectsUnknownRouteCredentialMode proves a routes:
// entry's own credential_mode is checked against the closed set.
func TestValidateRoutingRejectsUnknownRouteCredentialMode(t *testing.T) {
	s := routesModeSettings()
	s.Routes["litellm"] = Route{CredentialMode: "made-up-mode"}
	err := ValidateRouting(s)
	if err == nil || !strings.Contains(err.Error(), "credential_mode") {
		t.Errorf("ValidateRouting = %v, want a refusal naming credential_mode", err)
	}
}

// TestValidateRoutingRejectsStaticRouteWithoutUpstream proves a static
// route needs an upstream unless it opts out via allow_no_credential.
func TestValidateRoutingRejectsStaticRouteWithoutUpstream(t *testing.T) {
	s := routesModeSettings()
	s.Routes["litellm"] = Route{CredentialMode: "static"}
	err := ValidateRouting(s)
	if err == nil || !strings.Contains(err.Error(), "upstream is required") {
		t.Errorf("ValidateRouting = %v, want a refusal naming upstream", err)
	}
	s.Routes["litellm"] = Route{CredentialMode: "static", AllowNoCredential: true}
	if err := ValidateRouting(s); err != nil {
		t.Errorf("ValidateRouting = %v, want nil once allow_no_credential waives the upstream requirement", err)
	}
}

// TestValidateRoutingRejectsCredentialEnvOnNonStaticRoute proves
// credential_env is refused outside a static route.
func TestValidateRoutingRejectsCredentialEnvOnNonStaticRoute(t *testing.T) {
	s := routesModeSettings()
	s.Routes["litellm"] = Route{CredentialMode: "chatgpt-codex", CredentialEnv: "FOO"}
	err := ValidateRouting(s)
	if err == nil || !strings.Contains(err.Error(), "credential_env") {
		t.Errorf("ValidateRouting = %v, want a refusal naming credential_env", err)
	}
}

// TestValidateRoutingRejectsGitHubTokenFieldsOnNonCopilotRoute proves
// github_token_file/github_token_key are refused outside a github-copilot
// route.
func TestValidateRoutingRejectsGitHubTokenFieldsOnNonCopilotRoute(t *testing.T) {
	s := routesModeSettings()
	s.Routes["litellm"] = Route{CredentialMode: "static", Upstream: "https://x.invalid", GitHubTokenFile: "~/.pi/agent/auth.json"}
	err := ValidateRouting(s)
	if err == nil || !strings.Contains(err.Error(), "github_token_file") {
		t.Errorf("ValidateRouting = %v, want a refusal naming github_token_file", err)
	}
}

// TestValidateRoutingRejectsCodexAuthFileOnNonCodexRoute proves
// codex_auth_file is refused outside a chatgpt-codex route.
func TestValidateRoutingRejectsCodexAuthFileOnNonCodexRoute(t *testing.T) {
	s := routesModeSettings()
	s.Routes["litellm"] = Route{CredentialMode: "static", Upstream: "https://x.invalid", CodexAuthFile: "~/.codex/auth.json"}
	err := ValidateRouting(s)
	if err == nil || !strings.Contains(err.Error(), "codex_auth_file") {
		t.Errorf("ValidateRouting = %v, want a refusal naming codex_auth_file", err)
	}
}

// TestValidateRolesRejectsReviewInExecutionAllowed proves the
// independence rule: a review role resolving to the same model
// (effective id+API on a shared route) as a model in
// roles.execution.allowed is refused unless waived.
func TestValidateRolesRejectsReviewInExecutionAllowed(t *testing.T) {
	s := routesModeSettings()
	s.Models["opus"] = Model{ID: "gpt-5.6-luna", Routes: []string{"litellm"}} // same ID as luna
	s.Roles.Execution.Allowed = []string{"luna"}
	s.Roles.Review = &RoleConfig{Model: "opus"}
	err := ValidateRouting(s)
	if err == nil {
		t.Fatal("ValidateRouting: err = nil, want a refusal when review shares execution's resolved model")
	}
	if !strings.Contains(err.Error(), "roles.review") {
		t.Errorf("err = %v, want it to name roles.review", err)
	}
}

// TestValidateRolesAllowsSharedModelWhenWaived proves
// roles.review.allow_shared_model: true waives the independence rule
// above -- the Phase-0 Luna config's own documented waiver.
func TestValidateRolesAllowsSharedModelWhenWaived(t *testing.T) {
	s := routesModeSettings()
	s.Models["opus"] = Model{ID: "gpt-5.6-luna", Routes: []string{"litellm"}}
	s.Roles.Execution.Allowed = []string{"luna"}
	s.Roles.Review = &RoleConfig{Model: "opus", AllowSharedModel: true}
	if err := ValidateRouting(s); err != nil {
		t.Fatalf("ValidateRouting: %v, want nil once allow_shared_model waives the independence rule", err)
	}
}

// TestValidateRolesIndependenceOnlyOnSharedRoute proves the independence
// rule is scoped to a route the two models actually share: the same
// resolved id/API on two routes that never overlap is not a conflict --
// the two models can never actually collide in practice, since a job
// only ever resolves one route per model.
func TestValidateRolesIndependenceOnlyOnSharedRoute(t *testing.T) {
	s := routesModeSettings()
	s.Routes["codex"] = Route{CredentialMode: "chatgpt-codex"}
	s.Models["luna"] = Model{ID: "gpt-5.6-luna", Routes: []string{"litellm"}}
	s.Models["opus"] = Model{ID: "gpt-5.6-luna", Routes: []string{"codex"}} // same id, disjoint route
	s.Roles.Execution.Allowed = []string{"luna"}
	s.Roles.Review = &RoleConfig{Model: "opus"}
	if err := ValidateRouting(s); err != nil {
		t.Errorf("ValidateRouting = %v, want nil: the two models never share a route", err)
	}
}

// TestValidateRolesIndependenceBypassViaRouteIDs is one of the three
// bypasses the independence rule must close: two models whose top-level
// ID fields differ, but whose route_ids override makes them resolve to
// the identical id on their one shared route, must still be caught.
func TestValidateRolesIndependenceBypassViaRouteIDs(t *testing.T) {
	s := routesModeSettings()
	s.Models["luna"] = Model{ID: "id-a", Routes: []string{"litellm"}, RouteIDs: map[string]string{"litellm": "shared-id"}}
	s.Models["opus"] = Model{ID: "id-b", Routes: []string{"litellm"}, RouteIDs: map[string]string{"litellm": "shared-id"}}
	s.Roles.Execution.Allowed = []string{"luna"}
	s.Roles.Review = &RoleConfig{Model: "opus"}
	if err := ValidateRouting(s); err == nil {
		t.Fatal("ValidateRouting: err = nil, want a refusal for a route_ids-only overlap")
	}
}

// TestValidateRolesIndependenceNeverSkipsOnEmptyResolvedIDs is the second
// bypass the independence rule must close: id is required on every
// models: entry (checkModel), so an empty resolved id can't occur through
// ID alone in practice -- but the comparison itself (reviewModel.
// effectiveID(route) == execModel.effectiveID(route)) has no special
// case that would treat two empty strings as "different" either, should
// a future change ever relax the id-required rule for a base-path-only
// routes: mode model the way legacy model_aliases already allows. This
// locks in that absence of a special case: a model missing its required
// id is refused before the independence check ever runs, not silently
// waved through as "obviously different from anything else".
func TestValidateRolesIndependenceNeverSkipsOnEmptyResolvedIDs(t *testing.T) {
	s := routesModeSettings()
	s.Models["opus"] = Model{Routes: []string{"litellm"}} // no id at all
	s.Roles.Execution.Allowed = []string{"luna"}
	s.Roles.Review = &RoleConfig{Model: "opus"}
	err := ValidateRouting(s)
	if err == nil || !strings.Contains(err.Error(), "models.opus: id is required") {
		t.Errorf("ValidateRouting = %v, want the id-required refusal for models.opus", err)
	}
}

// TestValidateRoutingAcceptsChatGPTCodexRouteWithDefaults proves a
// chatgpt-codex route left entirely at its own defaults (no upstream/
// allowed_path_prefix/worker_base_path set) passes the same host pin a
// real relay launch enforces (meter.ValidateChatGPTCodexRoute).
func TestValidateRoutingAcceptsChatGPTCodexRouteWithDefaults(t *testing.T) {
	s := routesModeSettings()
	s.Routes["litellm"] = Route{CredentialMode: "chatgpt-codex"}
	if err := ValidateRouting(s); err != nil {
		t.Fatalf("ValidateRouting: %v, want nil for a chatgpt-codex route left at its own defaults", err)
	}
}

// TestValidateRoutingRejectsChatGPTCodexRouteWithWrongUpstream proves an
// explicit upstream override the host pin would reject at relay-launch
// time is caught here too, at validation time.
func TestValidateRoutingRejectsChatGPTCodexRouteWithWrongUpstream(t *testing.T) {
	s := routesModeSettings()
	s.Routes["litellm"] = Route{CredentialMode: "chatgpt-codex", Upstream: "https://not-chatgpt.example.invalid"}
	err := ValidateRouting(s)
	if err == nil || !strings.Contains(err.Error(), "chatgpt-codex") {
		t.Errorf("ValidateRouting = %v, want a refusal naming the chatgpt-codex host pin", err)
	}
}

// TestValidateRoutingRejectsChatGPTCodexRouteWithWorkerBasePath proves a
// non-empty worker_base_path override -- which the pin also rejects --
// is caught the same way.
func TestValidateRoutingRejectsChatGPTCodexRouteWithWorkerBasePath(t *testing.T) {
	s := routesModeSettings()
	basePath := "/v1"
	s.Routes["litellm"] = Route{CredentialMode: "chatgpt-codex", WorkerBasePath: &basePath}
	if err := ValidateRouting(s); err == nil {
		t.Fatal("ValidateRouting: err = nil, want a refusal for a non-empty worker_base_path on a chatgpt-codex route")
	}
}

// TestValidateRoutingAcceptsGitHubCopilotRouteWithDefaults proves a
// github-copilot route left at its own default upstream passes the same
// host-suffix pin a real relay launch enforces
// (meter.ValidateGitHubCopilotRoute).
func TestValidateRoutingAcceptsGitHubCopilotRouteWithDefaults(t *testing.T) {
	s := routesModeSettings()
	s.Routes["litellm"] = Route{CredentialMode: "github-copilot"}
	if err := ValidateRouting(s); err != nil {
		t.Fatalf("ValidateRouting: %v, want nil for a github-copilot route left at its own default upstream", err)
	}
}

// TestValidateRoutingRejectsGitHubCopilotRouteWithWrongUpstream proves an
// explicit upstream override that isn't a *.githubcopilot.com host is
// refused.
func TestValidateRoutingRejectsGitHubCopilotRouteWithWrongUpstream(t *testing.T) {
	s := routesModeSettings()
	s.Routes["litellm"] = Route{CredentialMode: "github-copilot", Upstream: "https://evilgithubcopilot.com"}
	err := ValidateRouting(s)
	if err == nil || !strings.Contains(err.Error(), "githubcopilot.com") {
		t.Errorf("ValidateRouting = %v, want a refusal naming the githubcopilot.com host pin", err)
	}
}

// TestValidateRoutingValidatesEveryDeclaredModel proves a models: entry
// is validated whether or not any role references it -- an operator who
// defines a broken entry they haven't wired up yet must still learn
// about it now.
func TestValidateRoutingValidatesEveryDeclaredModel(t *testing.T) {
	s := routesModeSettings()
	s.Models["unreferenced"] = Model{Routes: []string{"does-not-exist"}} // no id, bad route
	err := ValidateRouting(s)
	if err == nil || !strings.Contains(err.Error(), "models.unreferenced") {
		t.Errorf("ValidateRouting = %v, want a refusal naming models.unreferenced even though no role references it", err)
	}
}

// TestValidateRolesIndependenceOverWholeReviewAllowedSet proves the
// independence rule checks every model in roles.review's own allowed
// list, not just its default model -- a review role whose default model
// is safe but whose allowed list also names one that collides with
// execution must still fail, since a caller could resolve review to
// either.
func TestValidateRolesIndependenceOverWholeReviewAllowedSet(t *testing.T) {
	s := routesModeSettings()
	s.Models["safe-reviewer"] = Model{ID: "gpt-5.6-terra", Routes: []string{"litellm"}}
	s.Models["colliding-reviewer"] = Model{ID: "gpt-5.6-luna", Routes: []string{"litellm"}} // same id as luna
	s.Roles.Execution.Allowed = []string{"luna"}
	s.Roles.Review = &RoleConfig{Model: "safe-reviewer", Allowed: []string{"safe-reviewer", "colliding-reviewer"}}
	err := ValidateRouting(s)
	if err == nil {
		t.Fatal("ValidateRouting: err = nil, want a refusal: roles.review.allowed also names a model colliding with execution")
	}
	if !strings.Contains(err.Error(), "colliding-reviewer") {
		t.Errorf("err = %v, want it to name colliding-reviewer", err)
	}
}

// TestValidateRolesIndependenceComparesByBackendNotRouteName proves the
// independence rule compares the resolved backend (credential_mode +
// upstream after defaults + effective id + effective API), not the route
// NAME: two differently-named static routes that both resolve to the
// identical upstream still count as the same backend, so a model reached
// through one and a model reached through the other, sharing an id/API,
// still collide -- an operator can't dodge the rule by simply giving the
// same backend two different route names.
func TestValidateRolesIndependenceComparesByBackendNotRouteName(t *testing.T) {
	s := routesModeSettings()
	s.Routes["litellm-b"] = Route{CredentialMode: "static", Upstream: "https://litellm.example.invalid"} // same upstream as "litellm", different name
	s.Models["luna"] = Model{ID: "gpt-5.6-luna", Routes: []string{"litellm"}}
	s.Models["luna-via-b"] = Model{ID: "gpt-5.6-luna", Routes: []string{"litellm-b"}}
	s.Roles.Execution.Allowed = []string{"luna"}
	s.Roles.Review = &RoleConfig{Model: "luna-via-b"}
	err := ValidateRouting(s)
	if err == nil {
		t.Fatal("ValidateRouting: err = nil, want a refusal: \"litellm\" and \"litellm-b\" are the same backend under a different name")
	}
	if !strings.Contains(err.Error(), "same backend") {
		t.Errorf("err = %v, want it to name the same-backend collision", err)
	}
}

// TestNoConfigYAMLTagOverlapsLegacyRoutingKeys is legacyRoutingKeys' own
// drift guard now that routes:/models:/roles: is the only schema: no
// yaml tag Config actually declares may collide with one of the retired
// key names legacyRoutingKeys lists (Load's own friendly-error check),
// since every one of those keys was deleted outright, not renamed or
// reused -- a future field reusing one of these names by mistake would
// otherwise silently make Load's own migration-hint error unreachable
// for it.
func TestNoConfigYAMLTagOverlapsLegacyRoutingKeys(t *testing.T) {
	legacy := make(map[string]bool, len(legacyRoutingKeys))
	for _, key := range legacyRoutingKeys {
		legacy[key] = true
	}
	typ := reflect.TypeOf(Config{})
	for i := 0; i < typ.NumField(); i++ {
		tag := typ.Field(i).Tag.Get("yaml")
		name, _, _ := strings.Cut(tag, ",")
		if name == "" {
			continue
		}
		if legacy[name] {
			t.Errorf("Config field %s reuses retired legacy key name %q", typ.Field(i).Name, name)
		}
	}
}

// TestValidateRoutingChecksThinkingAgainstEveryAllowedModel proves the
// silent-clamp guard now reads every model named in a role's allowed
// list, not just its default model -- a role whose default model
// declares reasoning but whose allowed list also names one that does not
// must still fail, since a caller could resolve to either.
func TestValidateRoutingChecksThinkingAgainstEveryAllowedModel(t *testing.T) {
	s := routesModeSettings()
	s.Models["luna"] = Model{ID: "gpt-5.6-luna", Routes: []string{"litellm"}, Reasoning: true}
	s.Models["cheap"] = Model{ID: "gpt-5.6-terra", Routes: []string{"litellm"}} // no reasoning
	s.Roles.Execution.Allowed = []string{"luna", "cheap"}
	s.Roles.Execution.Thinking = "medium"
	err := ValidateRouting(s)
	if err == nil {
		t.Fatal("ValidateRouting: err = nil, want a refusal since \"cheap\" (in allowed) does not declare reasoning")
	}
	if !strings.Contains(err.Error(), "models.cheap") {
		t.Errorf("err = %v, want it to name models.cheap", err)
	}
}

// TestValidateRoutingRejectsUnsupportedThinkingLevel proves a
// roles.<kind>.thinking value outside the closed validThinkingLevels set
// is refused by name, rather than silently passed through to Pi (which
// would then either ignore it or misinterpret it).
func TestValidateRoutingRejectsUnsupportedThinkingLevel(t *testing.T) {
	s := routesModeSettings()
	s.Models["luna"] = Model{ID: "gpt-5.6-luna", Routes: []string{"litellm"}, Reasoning: true}
	s.Roles.Execution.Thinking = "ultra"
	err := ValidateRouting(s)
	if err == nil || !strings.Contains(err.Error(), "not a supported level") {
		t.Fatalf("ValidateRouting = %v, want a refusal naming \"not a supported level\" for roles.execution.thinking: ultra", err)
	}
}

// TestValidateRoutingRejectsUndeclaredMaxThinkingLevel proves
// roles.execution.thinking: max is refused when the model's own
// thinking_level_map has no "max" entry at all -- Pi silently clamps an
// undeclared level to high, so this must fail loudly instead of quietly
// running at the wrong effort.
func TestValidateRoutingRejectsUndeclaredMaxThinkingLevel(t *testing.T) {
	s := routesModeSettings()
	s.Models["luna"] = Model{ID: "gpt-5.6-luna", Routes: []string{"litellm"}, Reasoning: true}
	s.Roles.Execution.Thinking = "max"
	err := ValidateRouting(s)
	if err == nil || !strings.Contains(err.Error(), "thinking_level_map.max") {
		t.Fatalf("ValidateRouting = %v, want a refusal naming models.luna.thinking_level_map.max", err)
	}
}

// TestValidateRoutingRejectsNullMappedThinkingLevel proves the same
// refusal fires when thinking_level_map.max is explicitly present but
// maps to an empty/null value (e.g. `max:` with nothing after the
// colon), not just when the key is absent entirely -- a declared-but-
// empty mapping is exactly as unusable to Pi as no mapping at all.
func TestValidateRoutingRejectsNullMappedThinkingLevel(t *testing.T) {
	s := routesModeSettings()
	s.Models["luna"] = Model{ID: "gpt-5.6-luna", Routes: []string{"litellm"}, Reasoning: true, ThinkingLevelMap: map[string]string{"max": ""}}
	s.Roles.Execution.Thinking = "max"
	err := ValidateRouting(s)
	if err == nil || !strings.Contains(err.Error(), "thinking_level_map.max") {
		t.Fatalf("ValidateRouting = %v, want a refusal naming models.luna.thinking_level_map.max even though the key is present (mapped to \"\")", err)
	}
}

// TestValidateRoutingAllowsDeclaredMaxThinkingLevel is the positive
// counterpart: once thinking_level_map.max names a real value, roles.
// execution.thinking: max is accepted.
func TestValidateRoutingAllowsDeclaredMaxThinkingLevel(t *testing.T) {
	s := routesModeSettings()
	s.Models["luna"] = Model{ID: "gpt-5.6-luna", Routes: []string{"litellm"}, Reasoning: true, ThinkingLevelMap: map[string]string{"max": "high"}}
	s.Roles.Execution.Thinking = "max"
	if err := ValidateRouting(s); err != nil {
		t.Fatalf("ValidateRouting = %v, want nil once models.luna.thinking_level_map.max is declared", err)
	}
}

// TestLoadRefusesRelayImageWithRemoveHint pins the wording for a config
// written before the inference relay was deleted: the error says to remove
// the line rather than claiming the key moved somewhere.
func TestLoadRefusesRelayImageWithRemoveHint(t *testing.T) {
	_, err := Load(writeConfig(t, "relay_image: localhost:5050/factoryd-relay@sha256:abc\n"))
	if err == nil {
		t.Fatal("Load: err = nil, want a refusal naming relay_image")
	}
	for _, want := range []string{"relay_image", "remove this line"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v, want it to contain %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "moved to") {
		t.Errorf("err = %v, must not say the key moved", err)
	}
}

// TestLoadRefusesSandboxPIDsWithRemoveHint: the gateway launcher never
// applied sandbox_pids, so the key is gone and a config still carrying it
// is told to drop the line.
func TestLoadRefusesSandboxPIDsWithRemoveHint(t *testing.T) {
	_, err := Load(writeConfig(t, "sandbox_pids: 512\n"))
	if err == nil {
		t.Fatal("Load: err = nil, want a refusal naming sandbox_pids")
	}
	for _, want := range []string{"sandbox_pids", "remove this line"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v, want it to contain %q", err, want)
		}
	}
}

// TestLoadRefusesARelayBudgetKeyNamingItsNewName: the budget keys are the
// meter's, so a config still spelling one relay_* is told the new name.
func TestLoadRefusesARelayBudgetKeyNamingItsNewName(t *testing.T) {
	_, err := Load(writeConfig(t, "relay_token_ceiling: 100000\n"))
	if err == nil {
		t.Fatal("Load: err = nil, want a refusal naming relay_token_ceiling")
	}
	for _, want := range []string{"relay_token_ceiling", "renamed to meter_token_ceiling"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v, want it to contain %q", err, want)
		}
	}
}
