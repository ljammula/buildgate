package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"buildgate/internal/meter"
	"buildgate/internal/modelrole"
	"buildgate/internal/sessionconfig"
)

// quickstartLegacyRouteKeys are the top-level relay_*/model_aliases keys
// no quickstart-written config may contain once Phase 2F converts a
// route to routes:/models:/roles: -- see legacyRoutingKeys in
// internal/sessionconfig/routing.go, the same set ValidateRouting
// refuses once routes: is present.
var quickstartLegacyRouteKeys = []string{
	"relay_upstream",
	"relay_credential_mode",
	"relay_worker_model_id",
	"relay_worker_model_extra_json",
	"model_aliases",
}

// quickstartBuildWriteReload drives quickstartBuildConfig non-
// interactively, writes the result with quickstartWriteConfig (the exact
// path `factoryd quickstart` itself uses), and loads it back through the
// real config loader -- proving the round trip a real quickstart run/
// later `factoryd worker` would see, not just the in-memory *Config
// quickstartBuildConfig returns.
func quickstartBuildWriteReload(dp *deps, t *testing.T, opts *quickstartOptions) (*sessionconfig.Config, sessionconfig.Settings, string) {
	t.Helper()
	var out bytes.Buffer
	cfg, _, err := quickstartBuildConfig(dp, opts, newQuickstartPrompter(strings.NewReader("")), &out, t.TempDir())
	if err != nil {
		t.Fatalf("quickstartBuildConfig: %v", err)
	}
	configPath := filepath.Join(t.TempDir(), "config.yml")
	if err := quickstartWriteConfig(configPath, cfg); err != nil {
		t.Fatalf("quickstartWriteConfig: %v", err)
	}
	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read written config: %v", err)
	}
	for _, key := range quickstartLegacyRouteKeys {
		if strings.Contains(string(raw), key+":") {
			t.Errorf("written config contains legacy key %q:\n%s", key, raw)
		}
	}
	reloaded, err := sessionconfig.Load(configPath)
	if err != nil {
		t.Fatalf("sessionconfig.Load: %v", err)
	}
	settings, err := reloaded.ApplySettings(sessionconfig.DefaultSettings())
	if err != nil {
		t.Fatalf("ApplySettings: %v", err)
	}
	if len(settings.Routes) == 0 {
		t.Fatal("settings.Routes is empty, want this route written as routes:/models:/roles:")
	}
	if err := sessionconfig.ValidateRouting(settings); err != nil {
		t.Fatalf("ValidateRouting: %v", err)
	}
	return reloaded, settings, configPath
}

// quickstartRouteProbeAlwaysOK is a modelrole.SelectRoute probe that
// never refuses a route -- these tests check which route/policy
// SelectRoute resolves to, not credential availability.
func quickstartRouteProbeAlwaysOK(string, sessionconfig.Route) error { return nil }

func TestQuickstartWritesRoutesConfigOpenAIWithCredential(t *testing.T) {
	dp := newTestDeps(t)
	opts := &quickstartOptions{
		NonInteractive:        true,
		Route:                 "openai",
		ModelHost:             "https://model-host.example:8443",
		ModelID:               "local-model",
		ContextWindow:         131072,
		ContextWindowExplicit: true,
		Credential:            "sk-test",
		CredentialProvided:    true,
	}
	_, settings, _ := quickstartBuildWriteReload(dp, t, opts)

	sel, err := modelrole.SelectRoute(settings, modelrole.RoleExecution, "", "", "", quickstartRouteProbeAlwaysOK)
	if err != nil {
		t.Fatalf("SelectRoute: %v", err)
	}
	if sel.Policy.Upstream != "https://model-host.example:8443" {
		t.Errorf("Upstream = %q", sel.Policy.Upstream)
	}
	if sel.Policy.AuthMode != meter.CredentialModeStatic {
		t.Errorf("AuthMode = %q, want %q", sel.Policy.AuthMode, meter.CredentialModeStatic)
	}
	if sel.Policy.WorkerModelID != "local-model" {
		t.Errorf("WorkerModelID = %q", sel.Policy.WorkerModelID)
	}
	if sel.Policy.WorkerModelAPI != meter.RequestFormatOpenAICompletions {
		t.Errorf("WorkerModelAPI = %q, want %q", sel.Policy.WorkerModelAPI, meter.RequestFormatOpenAICompletions)
	}
	if sel.Policy.AllowedPathPrefix != "/v1" {
		t.Errorf("AllowedPathPrefix = %q, want /v1", sel.Policy.AllowedPathPrefix)
	}
}

func TestQuickstartWritesRoutesConfigOpenAIWithoutCredentialPlaintext(t *testing.T) {
	dp := newTestDeps(t)
	opts := &quickstartOptions{
		NonInteractive:        true,
		Route:                 "openai",
		ModelHost:             "http://127.0.0.1:8080",
		ModelID:               "local-model",
		ContextWindow:         131072,
		ContextWindowExplicit: true,
	}
	_, settings, _ := quickstartBuildWriteReload(dp, t, opts)

	sel, err := modelrole.SelectRoute(settings, modelrole.RoleExecution, "", "", "", quickstartRouteProbeAlwaysOK)
	if err != nil {
		t.Fatalf("SelectRoute: %v", err)
	}
	if sel.Policy.Upstream != "http://127.0.0.1:8080" {
		t.Errorf("Upstream = %q", sel.Policy.Upstream)
	}
	if sel.Policy.AuthMode != meter.CredentialModeStatic {
		t.Errorf("AuthMode = %q, want %q", sel.Policy.AuthMode, meter.CredentialModeStatic)
	}
	if sel.Policy.WorkerModelID != "local-model" {
		t.Errorf("WorkerModelID = %q", sel.Policy.WorkerModelID)
	}
	if sel.Policy.WorkerModelAPI != meter.RequestFormatOpenAICompletions {
		t.Errorf("WorkerModelAPI = %q, want %q", sel.Policy.WorkerModelAPI, meter.RequestFormatOpenAICompletions)
	}
	if sel.Policy.AllowedPathPrefix != "/v1" {
		t.Errorf("AllowedPathPrefix = %q, want /v1", sel.Policy.AllowedPathPrefix)
	}
	if !sel.Policy.AllowPlaintextUpstream {
		t.Error("AllowPlaintextUpstream = false, want true (http:// upstream)")
	}
}

func TestQuickstartWritesRoutesConfigCopilotWithTokenFile(t *testing.T) {
	dp := newTestDeps(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	authPath := home + "/.pi/agent/auth.json"
	writeCopilotAuthFixture(t, authPath, "gho_discovered_token")
	stubListGitHubCopilotModels(t, func(ctx context.Context, caBundlePath, githubToken, upstream string) ([]meter.CopilotModel, error) {
		return []meter.CopilotModel{{ID: "gpt-5.6-luna", ContextWindow: 200000}}, nil
	})

	opts := &quickstartOptions{
		NonInteractive:        true,
		Route:                 "copilot",
		ModelID:               "gpt-5.6-luna",
		ContextWindow:         131072,
		ContextWindowExplicit: true,
	}
	_, settings, _ := quickstartBuildWriteReload(dp, t, opts)

	sel, err := modelrole.SelectRoute(settings, modelrole.RoleExecution, "", "", "", quickstartRouteProbeAlwaysOK)
	if err != nil {
		t.Fatalf("SelectRoute: %v", err)
	}
	if sel.Policy.Upstream != meter.CopilotAPIBase {
		t.Errorf("Upstream = %q, want %q", sel.Policy.Upstream, meter.CopilotAPIBase)
	}
	if sel.Policy.AuthMode != meter.CredentialModeGitHubCopilot {
		t.Errorf("AuthMode = %q, want %q", sel.Policy.AuthMode, meter.CredentialModeGitHubCopilot)
	}
	if sel.Policy.WorkerModelID != "gpt-5.6-luna" {
		t.Errorf("WorkerModelID = %q", sel.Policy.WorkerModelID)
	}
	if sel.Policy.WorkerModelAPI != meter.RequestFormatOpenAICompletions {
		t.Errorf("WorkerModelAPI = %q, want %q", sel.Policy.WorkerModelAPI, meter.RequestFormatOpenAICompletions)
	}
	if sel.Policy.AllowedPathPrefix != meter.CopilotChatCompletionsPath {
		t.Errorf("AllowedPathPrefix = %q, want %q", sel.Policy.AllowedPathPrefix, meter.CopilotChatCompletionsPath)
	}
}

func TestQuickstartWritesRoutesConfigChatGPTCodex(t *testing.T) {
	dp := newTestDeps(t)
	t.Setenv("CODEX_HOME", t.TempDir())
	opts := &quickstartOptions{NonInteractive: true, Route: "chatgpt-codex"}
	_, settings, _ := quickstartBuildWriteReload(dp, t, opts)

	sel, err := modelrole.SelectRoute(settings, modelrole.RoleExecution, "", "", "", quickstartRouteProbeAlwaysOK)
	if err != nil {
		t.Fatalf("SelectRoute: %v", err)
	}
	if sel.Policy.Upstream != meter.ChatGPTCodexAPIBase {
		t.Errorf("Upstream = %q, want %q", sel.Policy.Upstream, meter.ChatGPTCodexAPIBase)
	}
	if sel.Policy.AuthMode != meter.CredentialModeChatGPTCodex {
		t.Errorf("AuthMode = %q, want %q", sel.Policy.AuthMode, meter.CredentialModeChatGPTCodex)
	}
	if sel.Policy.WorkerModelID != chatGPTCodexDefaultModelID {
		t.Errorf("WorkerModelID = %q, want %q", sel.Policy.WorkerModelID, chatGPTCodexDefaultModelID)
	}
	if sel.Policy.WorkerModelAPI != meter.RequestFormatOpenAIResponses {
		t.Errorf("WorkerModelAPI = %q, want %q", sel.Policy.WorkerModelAPI, meter.RequestFormatOpenAIResponses)
	}
	if sel.Policy.AllowedPathPrefix != meter.ChatGPTCodexResponsesPath {
		t.Errorf("AllowedPathPrefix = %q, want %q", sel.Policy.AllowedPathPrefix, meter.ChatGPTCodexResponsesPath)
	}
}

// TestQuickstartWritesRoutesConfigAnthropic proves -route anthropic writes
// a native Messages API route the sandbox runtime and the meter accept: the
// worker is given the bare API root (its client appends /v1/messages), the
// policy admits /v1/messages, and the meter parses Anthropic's shapes.
func TestQuickstartWritesRoutesConfigAnthropic(t *testing.T) {
	dp := newTestDeps(t)
	opts := &quickstartOptions{
		NonInteractive:     true,
		Route:              "anthropic",
		Credential:         "sk-test",
		CredentialProvided: true,
	}
	_, settings, _ := quickstartBuildWriteReload(dp, t, opts)

	sel, err := modelrole.SelectRoute(settings, modelrole.RoleExecution, "", "", "", quickstartRouteProbeAlwaysOK)
	if err != nil {
		t.Fatalf("SelectRoute: %v", err)
	}
	if sel.Policy.Upstream != "https://api.anthropic.com" {
		t.Errorf("Upstream = %q, want https://api.anthropic.com", sel.Policy.Upstream)
	}
	if sel.Policy.AuthMode != meter.CredentialModeStatic {
		t.Errorf("AuthMode = %q, want %q", sel.Policy.AuthMode, meter.CredentialModeStatic)
	}
	if sel.Policy.WorkerModelID != anthropicDefaultModelID {
		t.Errorf("WorkerModelID = %q, want %q", sel.Policy.WorkerModelID, anthropicDefaultModelID)
	}
	if sel.Policy.WorkerModelAPI != meter.WorkerAPIAnthropicMessages {
		t.Errorf("WorkerModelAPI = %q, want %q", sel.Policy.WorkerModelAPI, meter.WorkerAPIAnthropicMessages)
	}
	if sel.Policy.UsageFormat != meter.UsageFormatAnthropic {
		t.Errorf("UsageFormat = %q, want %q", sel.Policy.UsageFormat, meter.UsageFormatAnthropic)
	}
	if sel.Policy.AllowedPathPrefix != "/v1/messages" || sel.Policy.WorkerBasePath != "" {
		t.Errorf("AllowedPathPrefix, WorkerBasePath = %q, %q, want /v1/messages and empty", sel.Policy.AllowedPathPrefix, sel.Policy.WorkerBasePath)
	}
	var extra map[string]any
	if err := json.Unmarshal([]byte(sel.Policy.WorkerModelExtraJSON), &extra); err != nil {
		t.Fatalf("worker model JSON %q: %v", sel.Policy.WorkerModelExtraJSON, err)
	}
	if extra["reasoning"] != true || extra["maxTokens"] != float64(anthropicDefaultMaxOutputTokens) {
		t.Errorf("worker model JSON = %s, want reasoning and a %d-token output cap", sel.Policy.WorkerModelExtraJSON, anthropicDefaultMaxOutputTokens)
	}
	if compat, _ := extra["compat"].(map[string]any); compat["forceAdaptiveThinking"] != true {
		t.Errorf("worker model JSON = %s, want compat.forceAdaptiveThinking", sel.Policy.WorkerModelExtraJSON)
	}
	if levels, _ := extra["thinkingLevelMap"].(map[string]any); levels == nil || levels["off"] != nil {
		t.Errorf("worker model JSON = %s, want thinkingLevelMap.off null", sel.Policy.WorkerModelExtraJSON)
	} else if _, ok := levels["off"]; !ok {
		t.Errorf("worker model JSON = %s, want thinkingLevelMap.off present and null", sel.Policy.WorkerModelExtraJSON)
	}
	access, err := sel.Policy.RouteAccess(t.TempDir(), []string{"/usr/bin/node"})
	if err != nil {
		t.Fatalf("RouteAccess: %v", err)
	}
	for _, want := range []string{"FACTORY_MODEL_BASE_URL=https://api.anthropic.com", "FACTORY_MODEL_API=anthropic-messages"} {
		if !slices.Contains(access.Environment, want) {
			t.Errorf("worker environment %v lacks %q", access.Environment, want)
		}
	}
	// Exactly /v1/messages: a prefix would admit /v1/messages/batches,
	// whose requests the meter cannot price.
	if access.Endpoint.Path != "/v1/messages" || access.Endpoint.PathIsPrefix {
		t.Errorf("endpoint path = %q (prefix %v), want exactly /v1/messages", access.Endpoint.Path, access.Endpoint.PathIsPrefix)
	}
	config, err := sel.Policy.MeterConfig(t.TempDir(), "run-1", "sandbox-1")
	if err != nil {
		t.Fatalf("MeterConfig: %v", err)
	}
	if config["request_format"] != meter.RequestFormatAnthropic || config["usage_format"] != meter.UsageFormatAnthropic {
		t.Errorf("meter formats = %v, %v, want anthropic, anthropic", config["request_format"], config["usage_format"])
	}
}
