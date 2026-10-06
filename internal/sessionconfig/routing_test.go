package sessionconfig

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"

	"buildgate/internal/run"
)

// TestRoutingAppliesRouteDefaults confirms Settings.Routing() applies
// defaultedRoute uniformly across every routes: entry: a static route
// defaults CredentialEnv to ANTHROPIC_API_KEY and Billing to metered; a
// chatgpt-codex route defaults Billing to subscription and never
// defaults CredentialEnv (credential_env only applies to a static
// route). Routing only defaults Routes, never Models.
func TestRoutingAppliesRouteDefaults(t *testing.T) {
	s := DefaultSettings()
	s.Routes = map[string]Route{
		"litellm": {CredentialMode: "static", Upstream: "https://litellm.example.invalid"},
		"codex":   {CredentialMode: "chatgpt-codex"},
	}
	s.Models = map[string]Model{"luna": {ID: "gpt-5.6-luna", Routes: []string{"litellm", "codex"}}}
	routes, models, err := s.Routing()
	if err != nil {
		t.Fatalf("Routing: %v", err)
	}
	if routes["litellm"].CredentialEnv != "ANTHROPIC_API_KEY" {
		t.Errorf(`routes["litellm"].CredentialEnv = %q, want ANTHROPIC_API_KEY (static default)`, routes["litellm"].CredentialEnv)
	}
	if routes["litellm"].Billing != run.BillingMetered {
		t.Errorf(`routes["litellm"].Billing = %q, want %q`, routes["litellm"].Billing, run.BillingMetered)
	}
	if routes["codex"].Billing != run.BillingSubscription {
		t.Errorf(`routes["codex"].Billing = %q, want %q`, routes["codex"].Billing, run.BillingSubscription)
	}
	if routes["codex"].CredentialEnv != "" {
		t.Errorf(`routes["codex"].CredentialEnv = %q, want empty: credential_env only applies to a static route`, routes["codex"].CredentialEnv)
	}
	if len(models) != 1 || models["luna"].ID != "gpt-5.6-luna" {
		t.Errorf("models = %#v, want the configured luna model unchanged (Routing only defaults Routes, never Models)", models)
	}
}

// TestModelNeverSetsUpstreamOrPath is the reflection check applied to
// Model: a routes: mode model only ever
// names an id/api plus which already-operator-configured Route(s) it may
// resolve through (never another model) -- it must never itself be able
// to name an upstream, allowed-path prefix, script, or interpreter. A
// field added to Model later that matches one of these names would
// regress SC-015 without this test catching it structurally.
func TestModelNeverSetsUpstreamOrPath(t *testing.T) {
	forbidden := []string{"upstream", "allowedpath", "pathprefix", "script", "interpreter"}
	typ := reflect.TypeOf(Model{})
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i).Name
		name := strings.ToLower(field)
		for _, bad := range forbidden {
			if strings.Contains(name, bad) {
				t.Fatalf("Model.%s contains forbidden term %q -- a model must never be able to set an upstream, allowed-path prefix, script, or interpreter", field, bad)
			}
		}
	}
}

// TestWorkerModelJSONComposesFirstClassFields proves WorkerModelJSON
// writes contextWindow/reasoning/thinkingLevelMap in from the model's own
// first-class fields, ending the replace-not-merge trap: a caller no
// longer repeats contextWindow in every model's own extra_json.
func TestWorkerModelJSONComposesFirstClassFields(t *testing.T) {
	m := Model{
		ContextWindow:    272000,
		Reasoning:        true,
		ThinkingLevelMap: map[string]string{"xhigh": "xhigh", "max": "max"},
		ExtraJSON:        map[string]any{"samplingParams": map[string]any{}},
	}
	encoded, err := m.WorkerModelJSON("")
	if err != nil {
		t.Fatalf("WorkerModelJSON: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(encoded), &got); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if got["contextWindow"] != float64(272000) {
		t.Errorf("contextWindow = %v, want 272000", got["contextWindow"])
	}
	if got["reasoning"] != true {
		t.Errorf("reasoning = %v, want true", got["reasoning"])
	}
	if _, ok := got["thinkingLevelMap"]; !ok {
		t.Errorf("thinkingLevelMap missing from %v", got)
	}
	if _, ok := got["samplingParams"]; !ok {
		t.Errorf("samplingParams (extra_json) missing from %v -- the model's own extra_json must still come through", got)
	}
}

// TestWorkerModelJSONRejectsFirstClassKeyInExtraJSON is the first-class-
// key-vs-extra_json conflict test: a model that puts a reserved key (here
// contextWindow) inside extra_json itself, rather than as the first-class
// field, is a config error naming the offending key -- not silently
// overwritten or silently kept, either of which would resurrect the
// replace-not-merge ambiguity this composition exists to end.
func TestWorkerModelJSONRejectsFirstClassKeyInExtraJSON(t *testing.T) {
	m := Model{
		ContextWindow: 131072,
		ExtraJSON:     map[string]any{"contextWindow": 999},
	}
	_, err := m.WorkerModelJSON("")
	if err == nil {
		t.Fatal("WorkerModelJSON: err = nil, want a conflict error for extra_json.contextWindow")
	}
	if !strings.Contains(err.Error(), "extra_json.contextWindow") {
		t.Errorf("err = %v, want it to name extra_json.contextWindow", err)
	}
}

// TestWorkerModelJSONRejectsReservedRelayPolicyKeyInExtraJSON covers the
// other half of workerModelJSONReservedKeys: id/api/baseUrl, the fields
// RoutePolicy itself composes around whatever WorkerModelJSON returns,
// must never be smuggled into extra_json either.
func TestWorkerModelJSONRejectsReservedRelayPolicyKeyInExtraJSON(t *testing.T) {
	m := Model{ExtraJSON: map[string]any{"baseUrl": "https://example.invalid"}}
	_, err := m.WorkerModelJSON("")
	if err == nil {
		t.Fatal("WorkerModelJSON: err = nil, want a conflict error for extra_json.baseUrl")
	}
	if !strings.Contains(err.Error(), "extra_json.baseUrl") {
		t.Errorf("err = %v, want it to name extra_json.baseUrl", err)
	}
}

// TestWorkerModelJSONRejectsRouteNotInModelRoutes is SC-015's "never
// another model, only its own declared routes" invariant enforced
// structurally: a caller passing a route this model does not itself list
// gets an error, not a silently-composed JSON for a route the operator
// never granted this model.
func TestWorkerModelJSONRejectsRouteNotInModelRoutes(t *testing.T) {
	m := Model{Routes: []string{"codex"}}
	if _, err := m.WorkerModelJSON("litellm"); err == nil {
		t.Fatal("WorkerModelJSON: err = nil, want a refusal for a route not in Routes")
	}
	if _, err := m.WorkerModelJSON("codex"); err != nil {
		t.Errorf("WorkerModelJSON(%q): %v, want no error for a route the model does list", "codex", err)
	}
}

// TestYAMLRoundTripRejectsUnknownRoutesKey proves Load's own
// dec.KnownFields(true) still rejects a typo'd routes:/models:/roles:
// key -- the new schema doesn't loosen that guarantee.
func TestYAMLRoundTripRejectsUnknownRoutesKey(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/config.yml"
	bad := "routes:\n  codex:\n    credentail_mode: chatgpt-codex\n"
	if err := os.WriteFile(path, []byte(bad), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("Load: err = nil, want an unknown-field error for the typo'd credentail_mode")
	}
}

// TestYAMLRoundTripParsesRoutesAndModels is the schema's own round-trip
// smoke test: a config using every documented routes:/models:/roles: key
// loads cleanly and every field lands where expected.
func TestYAMLRoundTripParsesRoutesAndModels(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/config.yml"
	yaml := `
routes:
  codex:
    credential_mode: chatgpt-codex
    codex_auth_file: ~/.codex/auth.json
  copilot:
    credential_mode: github-copilot
    github_token_file: ~/.pi/agent/auth.json
    upstream: https://api.business.githubcopilot.com
models:
  luna:
    id: gpt-5.6-luna
    api: openai-responses
    routes: [codex, copilot]
    route_ids: { copilot: gpt-5.6-luna }
    context_window: 272000
    reasoning: true
    thinking_level_map: { xhigh: xhigh, max: max }
    extra_json: { samplingParams: {} }
roles:
  execution: { model: luna, thinking: medium, allowed: [luna] }
  review: { model: luna, thinking: max, allow_shared_model: true }
`
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Routes) != 2 {
		t.Fatalf("Routes = %#v, want 2 entries", cfg.Routes)
	}
	luna, ok := cfg.Models["luna"]
	if !ok {
		t.Fatalf("Models = %#v, want a luna entry", cfg.Models)
	}
	if luna.ID != "gpt-5.6-luna" || luna.API != "openai-responses" {
		t.Errorf("luna = %#v, want id/api parsed", luna)
	}
	if len(luna.Routes) != 2 || luna.Routes[0] != "codex" || luna.Routes[1] != "copilot" {
		t.Errorf("luna.Routes = %#v, want [codex copilot]", luna.Routes)
	}
	if luna.RouteIDs["copilot"] != "gpt-5.6-luna" {
		t.Errorf("luna.RouteIDs = %#v, want copilot: gpt-5.6-luna", luna.RouteIDs)
	}
	if cfg.Roles == nil || cfg.Roles.Execution == nil || len(cfg.Roles.Execution.Allowed) != 1 {
		t.Fatalf("Roles.Execution = %#v, want Allowed: [luna]", cfg.Roles.Execution)
	}
	if cfg.Roles.Review == nil || !cfg.Roles.Review.AllowSharedModel {
		t.Fatalf("Roles.Review = %#v, want AllowSharedModel: true", cfg.Roles.Review)
	}
}
