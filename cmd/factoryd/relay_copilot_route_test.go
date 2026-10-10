package main

import (
	"testing"

	"buildgate/internal/meter"
	"buildgate/internal/modelrole"
	"buildgate/internal/requestdriver"
	"buildgate/internal/sessionconfig"
)

// TestResolveRequestJobRelaySpecResolvesGitHubCopilotRoute is the
// github-copilot analog of
// TestResolveRequestJobRelaySpecCarriesResponsesWorkerAPI
// (relay_codex_test.go): resolving a spec/plan/oracle drafting relay spec
// from a routes:/models:/roles: github-copilot route must produce a
// valid Copilot-routed spec, not an Anthropic-shaped default.
func TestResolveRequestJobRelaySpecResolvesGitHubCopilotRoute(t *testing.T) {
	t.Setenv("GITHUB_COPILOT_TOKEN", "gho_test_token")
	settings := sessionconfig.DefaultSettings()
	settings.Routes = map[string]sessionconfig.Route{
		"copilot": {CredentialMode: meter.CredentialModeGitHubCopilot},
	}
	settings.Models = map[string]sessionconfig.Model{
		"gpt-5.6-luna": {ID: "gpt-5.6-luna", Routes: []string{"copilot"}},
	}
	settings.Roles = &sessionconfig.Roles{Execution: &sessionconfig.RoleConfig{Model: "gpt-5.6-luna"}}
	cfg := requestdriver.WorkerConfig{Settings: settings}
	sel, err := modelrole.SelectRoute(settings, modelrole.RoleExecution, "", "", "", func(_ string, r sessionconfig.Route) error {
		_, err := resolveRouteCredentials(r)
		return err
	})
	if err != nil {
		t.Fatalf("SelectRoute: %v", err)
	}
	spec, err := resolveRequestJobRelaySpec(cfg, "spec drafting", "run-1", t.TempDir(), requestJobRoleOverride{Thinking: sel.Thinking, RouteSelection: &sel})
	if err != nil {
		t.Fatalf("resolveRequestJobRelaySpec = %v, want a valid github-copilot drafting relay", err)
	}
	if spec.Upstream != meter.CopilotAPIBase {
		t.Fatalf("drafting relay upstream = %q, want %q", spec.Upstream, meter.CopilotAPIBase)
	}
	if spec.AllowedPathPrefix != meter.CopilotChatCompletionsPath {
		t.Fatalf("drafting relay allowed path prefix = %q, want %q", spec.AllowedPathPrefix, meter.CopilotChatCompletionsPath)
	}
	if spec.WorkerBasePath != "" {
		t.Fatalf("drafting relay worker base path = %q, want \"\"", spec.WorkerBasePath)
	}
}
