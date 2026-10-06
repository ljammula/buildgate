package main

import (
	"os"
	"strings"
	"testing"

	"buildgate/internal/modelrole"
	"buildgate/internal/sessionconfig"
)

// TestRouteBinderResolvesOnlyTheBoundRoute proves
// checkRouteFunc/resolveRouteCredentialsFunc -- the Temporal Worker's
// own workflow.Activities.CheckRoute/ResolveRouteCredentials closures --
// check a submitted RoutePolicy against this Worker's own routes: config
// (modelrole.CheckRouteBinding) and then resolve ONLY that one route's
// own credential (its own env var), never the other configured route's,
// even though both are present in this Worker's own routes: config.
func TestRouteBinderResolvesOnlyTheBoundRoute(t *testing.T) {
	t.Setenv("RB_PRIMARY_KEY", "sk-primary")
	t.Setenv("RB_FALLBACK_KEY", "sk-fallback")

	s := sessionconfig.DefaultSettings()
	s.Routes = map[string]sessionconfig.Route{
		"primary":  {CredentialMode: "static", Upstream: "https://primary.example.invalid", CredentialEnv: "RB_PRIMARY_KEY"},
		"fallback": {CredentialMode: "static", Upstream: "https://fallback.example.invalid", CredentialEnv: "RB_FALLBACK_KEY"},
	}
	s.Models = map[string]sessionconfig.Model{
		"m": {ID: "test-model", Routes: []string{"primary", "fallback"}},
	}
	s.Roles = &sessionconfig.Roles{Execution: &sessionconfig.RoleConfig{Model: "m"}}

	sel, err := modelrole.SelectRoute(s, modelrole.RoleExecution, "", "", "primary", func(string, sessionconfig.Route) error { return nil })
	if err != nil {
		t.Fatalf("SelectRoute: %v", err)
	}

	checkRoute := checkRouteFunc(s)
	resolveCreds := resolveRouteCredentialsFunc(s)
	if err := checkRoute(string(modelrole.RoleExecution), sel.Policy, sel.Thinking); err != nil {
		t.Fatalf("checkRoute: %v", err)
	}
	creds, err := resolveCreds(sel.Policy.Route)
	if err != nil {
		t.Fatalf("resolveCreds: %v", err)
	}
	if !creds.APIKey.Configured() {
		t.Fatal("APIKey not configured, want the bound route's own credential")
	}
	// resolveRouteCredentials never exposes a resolved value directly for
	// comparison (see sandbox.RouteSecret's own doc comment) -- this
	// test relies on TestResolveRouteCredentialsStaticFromEnv's own proof
	// that a static route's credential comes from its own CredentialEnv,
	// and instead proves here that binding the OTHER route's policy
	// yields a DIFFERENT (still configured) credential, so the two never
	// collapse to the same value by accident.
	fallbackSel, err := modelrole.SelectRoute(s, modelrole.RoleExecution, "", "", "fallback", func(string, sessionconfig.Route) error { return nil })
	if err != nil {
		t.Fatalf("SelectRoute (fallback): %v", err)
	}
	if err := checkRoute(string(modelrole.RoleExecution), fallbackSel.Policy, fallbackSel.Thinking); err != nil {
		t.Fatalf("checkRoute (fallback): %v", err)
	}
	fallbackCreds, err := resolveCreds(fallbackSel.Policy.Route)
	if err != nil {
		t.Fatalf("resolveCreds (fallback): %v", err)
	}
	if creds.APIKey == fallbackCreds.APIKey {
		t.Error("primary and fallback routes resolved to the same credential, want each route's own")
	}
}

func TestResolveRouteCredentialsStaticFromEnv(t *testing.T) {
	t.Setenv("RC_TEST_KEY", "sk-test-value")
	r := sessionconfig.Route{CredentialMode: "static", Upstream: "https://example.invalid", CredentialEnv: "RC_TEST_KEY"}
	creds, err := resolveRouteCredentials(r)
	if err != nil {
		t.Fatalf("resolveRouteCredentials: %v", err)
	}
	if !creds.apiKey.Configured() {
		t.Fatal("apiKey not configured, want the RC_TEST_KEY value")
	}
	if creds.githubToken.Configured() || creds.chatGPTToken.Configured() {
		t.Error("only apiKey should be configured for a static route")
	}
}

func TestResolveRouteCredentialsStaticMissingEnvRefuses(t *testing.T) {
	os.Unsetenv("RC_TEST_KEY_MISSING")
	r := sessionconfig.Route{CredentialMode: "static", Upstream: "https://example.invalid", CredentialEnv: "RC_TEST_KEY_MISSING"}
	if _, err := resolveRouteCredentials(r); err == nil {
		t.Fatal("resolveRouteCredentials: err = nil, want a refusal for a missing env credential")
	}
}

func TestResolveRouteCredentialsStaticAllowsNoCredential(t *testing.T) {
	os.Unsetenv("RC_TEST_KEY_MISSING2")
	r := sessionconfig.Route{CredentialMode: "static", Upstream: "https://example.invalid", CredentialEnv: "RC_TEST_KEY_MISSING2", AllowNoCredential: true}
	creds, err := resolveRouteCredentials(r)
	if err != nil {
		t.Fatalf("resolveRouteCredentials: %v", err)
	}
	if creds.apiKey.Configured() {
		t.Error("apiKey should be unconfigured when the env var is empty and allow_no_credential is set")
	}
}

func TestResolveRouteCredentialsGitHubCopilotMissingTokenRefuses(t *testing.T) {
	t.Setenv("GITHUB_COPILOT_TOKEN", "")
	dir := t.TempDir()
	r := sessionconfig.Route{CredentialMode: "github-copilot", GitHubTokenFile: dir + "/does-not-exist.json"}
	if _, err := resolveRouteCredentials(r); err == nil {
		t.Fatal("resolveRouteCredentials: err = nil, want a refusal when no GitHub token resolves")
	}
}

func TestResolveRouteCredentialsChatGPTCodexMissingAuthFileRefuses(t *testing.T) {
	dir := t.TempDir()
	r := sessionconfig.Route{CredentialMode: "chatgpt-codex", CodexAuthFile: dir + "/does-not-exist.json"}
	if _, err := resolveRouteCredentials(r); err == nil {
		t.Fatal("resolveRouteCredentials: err = nil, want a refusal when the codex auth file is missing")
	}
}

// TestRouteCredentialsResolvedOnlyForChosenRoute proves resolveRouteCredentials
// -- used as modelrole.SelectRoute's own probe -- is only ever invoked for
// this job's own candidate routes, stopping at the first one that
// resolves, in declared order: a static route with its env var unset is
// skipped, and a later route that DOES resolve is chosen without the
// first route's own failure reason leaking into the result.
func TestRouteCredentialsResolvedOnlyForChosenRoute(t *testing.T) {
	os.Unsetenv("RC_ORDER_FIRST")
	t.Setenv("RC_ORDER_SECOND", "sk-second")

	s := sessionconfig.DefaultSettings()
	s.Routes = map[string]sessionconfig.Route{
		"first":  {CredentialMode: "static", Upstream: "https://first.example.invalid", CredentialEnv: "RC_ORDER_FIRST"},
		"second": {CredentialMode: "static", Upstream: "https://second.example.invalid", CredentialEnv: "RC_ORDER_SECOND"},
	}
	s.Models = map[string]sessionconfig.Model{
		"m": {ID: "test-model", Routes: []string{"first", "second"}},
	}
	s.Roles = &sessionconfig.Roles{Execution: &sessionconfig.RoleConfig{Model: "m"}}

	var tried []string
	probe := func(name string, r sessionconfig.Route) error {
		tried = append(tried, name)
		_, err := resolveRouteCredentials(r)
		return err
	}
	sel, err := modelrole.SelectRoute(s, modelrole.RoleExecution, "", "", "", probe)
	if err != nil {
		t.Fatalf("SelectRoute: %v", err)
	}
	if sel.RouteName != "second" {
		t.Fatalf("selected route = %q, want %q", sel.RouteName, "second")
	}
	if len(tried) != 2 || tried[0] != "first" || tried[1] != "second" {
		t.Fatalf("tried = %v, want [first second] in order", tried)
	}
}

func TestResolveRouteCredentialsErrorNeverEchoesSecretValue(t *testing.T) {
	t.Setenv("RC_TEST_SECRET_ENV", "")
	r := sessionconfig.Route{CredentialMode: "static", Upstream: "https://example.invalid", CredentialEnv: "RC_TEST_SECRET_ENV"}
	_, err := resolveRouteCredentials(r)
	if err == nil {
		t.Fatal("resolveRouteCredentials: err = nil, want a refusal")
	}
	if strings.Contains(err.Error(), "sk-") {
		t.Errorf("err = %v, must never contain a credential value", err)
	}
}
