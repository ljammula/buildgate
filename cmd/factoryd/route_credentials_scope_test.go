package main

import (
	"io"
	"strings"
	"testing"

	"buildgate/internal/sessionconfig"
)

// TestNoCredentialRouteNeverReadsAnInheritedKey proves a static route with
// allow_no_credential and no credential_env resolves no credential even when
// ANTHROPIC_API_KEY is set for another route: a route reads only the variable
// it names, so a key for one provider never reaches another's upstream.
func TestNoCredentialRouteNeverReadsAnInheritedKey(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-for-another-route")
	for _, upstream := range []string{"http://127.0.0.1:8080", "https://openai-compatible.example.invalid"} {
		r := sessionconfig.Route{CredentialMode: "static", Upstream: upstream, AllowPlaintextUpstream: true, AllowNoCredential: true}
		creds, err := resolveRouteCredentials(r)
		if err != nil {
			t.Fatalf("resolveRouteCredentials(%s): %v, want no credential and no error", upstream, err)
		}
		if creds.apiKey.Configured() {
			t.Fatalf("resolveRouteCredentials(%s) picked up ANTHROPIC_API_KEY for a route that names no credential_env", upstream)
		}
	}
}

// TestStaticRouteWithoutCredentialEnvRefuses proves a route that needs a
// credential but names no variable is refused at resolution too (validation
// refuses it first), rather than reading a default name.
func TestStaticRouteWithoutCredentialEnvRefuses(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-for-another-route")
	r := sessionconfig.Route{CredentialMode: "static", Upstream: "https://openai-compatible.example.invalid"}
	creds, err := resolveRouteCredentials(r)
	if err == nil {
		t.Fatal("resolveRouteCredentials: err = nil, want a refusal for a static route with no credential_env")
	}
	if creds.apiKey.Configured() {
		t.Fatal("resolveRouteCredentials returned a key for a route that names no credential_env")
	}
}

// TestQuickstartOpenAIRouteNeverTakesTheAnthropicKey proves quickstart's
// OpenAI-compatible route never takes an inherited ANTHROPIC_API_KEY: under
// -non-interactive with no -credential it is written with no credential, and
// with -credential it reads its own OPENAI_API_KEY, so a restart from a shell
// that holds an Anthropic key never sends that key to this upstream.
func TestQuickstartOpenAIRouteNeverTakesTheAnthropicKey(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-for-another-route")
	base := quickstartOptions{
		NonInteractive:        true,
		Route:                 "openai",
		ModelHost:             "https://model-host.example:8443",
		ModelID:               "local-model",
		ContextWindow:         131072,
		ContextWindowExplicit: true,
	}

	noCred := base
	cfg, credentialEnv, err := quickstartBuildConfig(newTestDeps(t), &noCred, newQuickstartPrompter(strings.NewReader("")), io.Discard, t.TempDir())
	if err != nil {
		t.Fatalf("quickstartBuildConfig without -credential: %v", err)
	}
	if r := cfg.Routes["local"]; !r.AllowNoCredential || r.CredentialEnv != "" || len(credentialEnv) != 0 {
		t.Fatalf("without -credential: route %+v, credentialEnv %v; want allow_no_credential, no credential_env, no env", r, credentialEnv)
	}

	withCred := base
	withCred.Credential, withCred.CredentialProvided = "sk-openai", true
	cfg, credentialEnv, err = quickstartBuildConfig(newTestDeps(t), &withCred, newQuickstartPrompter(strings.NewReader("")), io.Discard, t.TempDir())
	if err != nil {
		t.Fatalf("quickstartBuildConfig with -credential: %v", err)
	}
	if r := cfg.Routes["local"]; r.CredentialEnv != "OPENAI_API_KEY" || r.AllowNoCredential {
		t.Fatalf("with -credential: route %+v, want credential_env OPENAI_API_KEY", r)
	}
	if len(credentialEnv) != 1 || credentialEnv[0] != "OPENAI_API_KEY=sk-openai" {
		t.Fatalf("with -credential: credentialEnv = %v, want [OPENAI_API_KEY=sk-openai]", credentialEnv)
	}
}
