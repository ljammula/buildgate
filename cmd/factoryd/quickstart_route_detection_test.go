package main

import (
	"bytes"
	"context"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"buildgate/internal/meter"
)

// ---- quickstartDetectCodexLogin / quickstartDetectCopilotLogin ---------

func TestQuickstartDetectCodexLoginTrueForValidStructure(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CODEX_HOME", dir)
	writeCodexAuthFile(t, dir, map[string]any{
		"auth_mode": "chatgpt",
		"tokens": map[string]any{
			"access_token": fakeCodexJWT(t, time.Now().Add(24*time.Hour)),
			"account_id":   "acct-123",
		},
	})
	if !quickstartDetectCodexLogin() {
		t.Fatal("quickstartDetectCodexLogin = false, want true for a structurally valid auth.json")
	}
}

// TestQuickstartDetectCodexLoginIgnoresExpiry proves detection checks
// existence/structure only, never the access token's own expiry -- that
// fail-closed check belongs to resolveChatGPTCodexCredential /
// doctorCheckChatGPTCodexCredential at relay-launch time, not to a route
// picker deciding what to offer.
func TestQuickstartDetectCodexLoginIgnoresExpiry(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CODEX_HOME", dir)
	writeCodexAuthFile(t, dir, map[string]any{
		"auth_mode": "chatgpt",
		"tokens": map[string]any{
			"access_token": fakeCodexJWT(t, time.Now().Add(-1*time.Hour)), // already expired
			"account_id":   "acct-123",
		},
	})
	if !quickstartDetectCodexLogin() {
		t.Fatal("quickstartDetectCodexLogin = false, want true even for a near/past-expiry token (detection is structure-only)")
	}
}

func TestQuickstartDetectCodexLoginFalseForMissingOrMalformed(t *testing.T) {
	t.Run("missing file", func(t *testing.T) {
		t.Setenv("CODEX_HOME", t.TempDir())
		if quickstartDetectCodexLogin() {
			t.Fatal("quickstartDetectCodexLogin = true, want false with no auth.json present")
		}
	})
	t.Run("wrong auth_mode", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("CODEX_HOME", dir)
		writeCodexAuthFile(t, dir, map[string]any{
			"auth_mode": "apikey",
			"tokens":    map[string]any{"access_token": "x", "account_id": "y"},
		})
		if quickstartDetectCodexLogin() {
			t.Fatal("quickstartDetectCodexLogin = true, want false for auth_mode != chatgpt")
		}
	})
	t.Run("missing tokens", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("CODEX_HOME", dir)
		writeCodexAuthFile(t, dir, map[string]any{"auth_mode": "chatgpt"})
		if quickstartDetectCodexLogin() {
			t.Fatal("quickstartDetectCodexLogin = true, want false with no tokens")
		}
	})
}

func TestQuickstartDetectCopilotLoginTrueForValidPiAuthFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	path := home + "/.pi/agent/auth.json"
	writeCopilotAuthFixture(t, path, "gho_test_token")

	detected, gotPath := quickstartDetectCopilotLogin()
	if !detected {
		t.Fatal("quickstartDetectCopilotLogin = false, want true for a valid pi auth.json")
	}
	if gotPath != path {
		t.Errorf("path = %q, want %q", gotPath, path)
	}
}

func TestQuickstartDetectCopilotLoginFalseWhenAbsent(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if detected, path := quickstartDetectCopilotLogin(); detected {
		t.Fatalf("quickstartDetectCopilotLogin = true (path %q), want false with no auth file present", path)
	}
}

// ---- quickstartDetectRouteOptions ---------------------------------------

// TestQuickstartDetectRouteOptionsSortsDetectedFirst proves a detected
// login (here, only ANTHROPIC_API_KEY) sorts ahead of the undetected
// options, while preserving each group's own relative order
// (chatgpt-codex, copilot, anthropic, openai).
func TestQuickstartDetectRouteOptionsSortsDetectedFirst(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CODEX_HOME", t.TempDir())
	t.Setenv("ANTHROPIC_API_KEY", "sk-test")

	options := quickstartDetectRouteOptions()
	if len(options) != 4 {
		t.Fatalf("len(options) = %d, want 4", len(options))
	}
	if options[0].Route != "anthropic" || !options[0].Detected {
		t.Errorf("options[0] = %+v, want the detected anthropic option first", options[0])
	}
	for _, o := range options[1:] {
		if o.Detected {
			t.Errorf("option %+v marked Detected, want only anthropic detected here", o)
		}
	}
	// Relative order among the undetected remainder is unchanged from the
	// base list (chatgpt-codex, copilot, openai).
	wantOrder := []string{"chatgpt-codex", "copilot", "openai"}
	for i, want := range wantOrder {
		if options[i+1].Route != want {
			t.Errorf("options[%d].Route = %q, want %q", i+1, options[i+1].Route, want)
		}
	}
}

func TestQuickstartDetectRouteOptionsNoneDetected(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CODEX_HOME", t.TempDir())
	t.Setenv("ANTHROPIC_API_KEY", "")

	options := quickstartDetectRouteOptions()
	for _, o := range options {
		if o.Detected {
			t.Errorf("option %+v marked Detected, want none detected in a clean environment", o)
		}
	}
	if options[len(options)-1].Route != "openai" {
		t.Errorf("last option = %+v, want openai (never detected, always last among ties)", options[len(options)-1])
	}
}

// ---- non-interactive -route auto-selection ------------------------------

func TestQuickstartBuildConfigNonInteractiveAutoSelectsSingleDetectedLogin(t *testing.T) {
	dp := newTestDeps(t)
	t.Setenv("HOME", t.TempDir())
	codexDir := t.TempDir()
	t.Setenv("CODEX_HOME", codexDir)
	writeCodexAuthFile(t, codexDir, map[string]any{
		"auth_mode": "chatgpt",
		"tokens": map[string]any{
			"access_token": fakeCodexJWT(t, time.Now().Add(24*time.Hour)),
			"account_id":   "acct-123",
		},
	})
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("GITHUB_COPILOT_TOKEN", "")

	opts := &quickstartOptions{NonInteractive: true}
	var out bytes.Buffer
	cfg, credentialEnv, err := quickstartBuildConfig(dp, opts, newQuickstartPrompter(strings.NewReader("")), &out, t.TempDir())
	if err != nil {
		t.Fatalf("quickstartBuildConfig: %v", err)
	}
	if !strings.Contains(out.String(), "Detected ChatGPT") {
		t.Errorf("output = %q, want it to announce the detected route", out.String())
	}
	if len(credentialEnv) != 0 {
		t.Errorf("credentialEnv = %v, want none -- chatgpt-codex reads its credential fresh from auth.json", credentialEnv)
	}
	if cfg == nil || cfg.Routes["codex"].CredentialMode != meter.CredentialModeChatGPTCodex {
		t.Fatalf("cfg.Routes[codex].CredentialMode = %v, want chatgpt-codex (the single file-based login detected)", cfg)
	}
}

// TestQuickstartBuildConfigNonInteractiveAnthropicEnvVarAloneNeverAutoSelects
// is the regression test for a round-2 review finding: ANTHROPIC_API_KEY is
// routine for anyone who also uses Claude Code and has nothing to do with
// which model route THIS invocation should use, so it must never be
// silently auto-selected under -non-interactive, even when it is the
// only "detected" option -- the same -route-is-required error as before
// this feature existed at all.
func TestQuickstartBuildConfigNonInteractiveAnthropicEnvVarAloneNeverAutoSelects(t *testing.T) {
	dp := newTestDeps(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CODEX_HOME", t.TempDir())
	t.Setenv("ANTHROPIC_API_KEY", "sk-test")
	t.Setenv("GITHUB_COPILOT_TOKEN", "")

	opts := &quickstartOptions{NonInteractive: true, Credential: "sk-test", CredentialProvided: true}
	_, _, err := quickstartBuildConfig(dp, opts, newQuickstartPrompter(strings.NewReader("")), &bytes.Buffer{}, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "-route is required") {
		t.Fatalf("err = %v, want the same -route-is-required error as with no login detected at all (ANTHROPIC_API_KEY alone must never auto-select)", err)
	}
}

func TestQuickstartBuildConfigNonInteractiveNoLoginDetectedRequiresRoute(t *testing.T) {
	dp := newTestDeps(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CODEX_HOME", t.TempDir())
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("GITHUB_COPILOT_TOKEN", "")

	opts := &quickstartOptions{NonInteractive: true}
	_, _, err := quickstartBuildConfig(dp, opts, newQuickstartPrompter(strings.NewReader("")), &bytes.Buffer{}, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "-route is required") {
		t.Fatalf("err = %v, want a -route-is-required error naming no login detected", err)
	}
}

// TestQuickstartBuildConfigNonInteractiveMultipleDetectedRequiresRoute
// uses two FILE-BASED logins (Codex and Copilot) -- both AutoSelectable
// -- since that same round-2 fix means ANTHROPIC_API_KEY alone is never
// auto-selectable and so can no longer contribute to a "multiple
// detected" ambiguity on its own.
func TestQuickstartBuildConfigNonInteractiveMultipleDetectedRequiresRoute(t *testing.T) {
	dp := newTestDeps(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	codexDir := t.TempDir()
	t.Setenv("CODEX_HOME", codexDir)
	writeCodexAuthFile(t, codexDir, map[string]any{
		"auth_mode": "chatgpt",
		"tokens": map[string]any{
			"access_token": fakeCodexJWT(t, time.Now().Add(24*time.Hour)),
			"account_id":   "acct-123",
		},
	})
	writeCopilotAuthFixture(t, home+"/.pi/agent/auth.json", "gho_test_token")
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("GITHUB_COPILOT_TOKEN", "")

	opts := &quickstartOptions{NonInteractive: true}
	_, _, err := quickstartBuildConfig(dp, opts, newQuickstartPrompter(strings.NewReader("")), &bytes.Buffer{}, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "multiple logins detected") {
		t.Fatalf("err = %v, want a multiple-logins-detected error", err)
	}
	if !strings.Contains(err.Error(), "chatgpt-codex") || !strings.Contains(err.Error(), "copilot") {
		t.Errorf("err = %v, want it to name both detected routes", err)
	}
}

// ---- chatgpt-codex route --------------------------------------------------

func TestQuickstartBuildConfigChatGPTCodexDefaultsModelAndContextWindow(t *testing.T) {
	dp := newTestDeps(t)
	// CODEX_HOME sandboxed to an empty temp dir so the detected-auth-file
	// persistence this case now does (resolveCodexAuthFilePath + os.Stat)
	// never touches this machine's own real ~/.codex/auth.json.
	t.Setenv("CODEX_HOME", t.TempDir())
	opts := &quickstartOptions{NonInteractive: true, Route: "chatgpt-codex"}
	var out bytes.Buffer
	cfg, credentialEnv, err := quickstartBuildConfig(dp, opts, newQuickstartPrompter(strings.NewReader("")), &out, t.TempDir())
	if err != nil {
		t.Fatalf("quickstartBuildConfig: %v", err)
	}
	if len(credentialEnv) != 0 {
		t.Errorf("credentialEnv = %v, want none -- the ChatGPT token is read fresh from auth.json at relay-launch time", credentialEnv)
	}
	if cfg.Routes["codex"].CredentialMode != meter.CredentialModeChatGPTCodex {
		t.Fatalf("Routes[codex].CredentialMode = %v, want %q", cfg.Routes["codex"].CredentialMode, meter.CredentialModeChatGPTCodex)
	}
	model, ok := cfg.Models[chatGPTCodexDefaultModelID]
	if !ok || model.ID != chatGPTCodexDefaultModelID {
		t.Errorf("Models[%s].ID = %v, want the default %q", chatGPTCodexDefaultModelID, model.ID, chatGPTCodexDefaultModelID)
	}
	if model.ContextWindow != chatGPTCodexDefaultContextWindow {
		t.Errorf("contextWindow = %v, want the default %d", model.ContextWindow, chatGPTCodexDefaultContextWindow)
	}
}

// TestQuickstartBuildConfigChatGPTCodexNonDefaultModelRequiresContextWindow
// proves there is no listing to fall back on for a non-default model id --
// codex exec has no GET /models endpoint at all (unlike -route
// openai/copilot), so a caller naming a different model must also supply
// -context-window explicitly.
func TestQuickstartBuildConfigChatGPTCodexNonDefaultModelRequiresContextWindow(t *testing.T) {
	dp := newTestDeps(t)
	t.Setenv("CODEX_HOME", t.TempDir())
	opts := &quickstartOptions{NonInteractive: true, Route: "chatgpt-codex", ModelID: "some-other-model"}
	_, _, err := quickstartBuildConfig(dp, opts, newQuickstartPrompter(strings.NewReader("")), &bytes.Buffer{}, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "-context-window is required") {
		t.Fatalf("err = %v, want a -context-window-is-required error for a non-default model id", err)
	}
}

func TestQuickstartBuildConfigChatGPTCodexRejectsSlashInModelID(t *testing.T) {
	dp := newTestDeps(t)
	t.Setenv("CODEX_HOME", t.TempDir())
	opts := &quickstartOptions{NonInteractive: true, Route: "chatgpt-codex", ModelID: "vendor/model", ContextWindow: 1000, ContextWindowExplicit: true}
	_, _, err := quickstartBuildConfig(dp, opts, newQuickstartPrompter(strings.NewReader("")), &bytes.Buffer{}, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "must not contain a slash") {
		t.Fatalf("err = %v, want a slash-rejection error", err)
	}
}

// ---- copilot: auto-discovered login, model picker, weak-model warning ---

// TestQuickstartBuildConfigCopilotUsesDiscoveredLoginNeverPromptsForToken
// is a regression test: a pi login already on the machine must be
// used automatically -- never a pasted token -- and persisted as
// relay_github_token_file, never as a token value anywhere in the written
// config or credentialEnv.
func TestQuickstartBuildConfigCopilotUsesDiscoveredLoginNeverPromptsForToken(t *testing.T) {
	dp := newTestDeps(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	authPath := home + "/.pi/agent/auth.json"
	writeCopilotAuthFixture(t, authPath, "gho_discovered_token")
	stubListGitHubCopilotModels(t, func(ctx context.Context, caBundlePath, githubToken, upstream string) ([]meter.CopilotModel, error) {
		if githubToken != "gho_discovered_token" {
			t.Errorf("githubToken = %q, want the token resolved from the discovered auth file", githubToken)
		}
		return []meter.CopilotModel{{ID: "gpt-5.6-luna", ContextWindow: 272000}}, nil
	})

	opts := &quickstartOptions{Route: "copilot"}
	var out bytes.Buffer
	// "1\n" picks the first (only) offered model from quickstartPickCopilotModelID's menu.
	cfg, credentialEnv, err := quickstartBuildConfig(dp, opts, newQuickstartPrompter(strings.NewReader("1\n")), &out, t.TempDir())
	if err != nil {
		t.Fatalf("quickstartBuildConfig: %v", err)
	}
	if len(credentialEnv) != 0 {
		t.Errorf("credentialEnv = %v, want none -- the token is never put in the daemon's environment for a discovered login", credentialEnv)
	}
	if cfg.Routes["copilot"].GitHubTokenFile != authPath {
		t.Errorf("Routes[copilot].GitHubTokenFile = %v, want %q", cfg.Routes["copilot"].GitHubTokenFile, authPath)
	}
	model, ok := cfg.Models["gpt-5.6-luna"]
	if !ok || model.ID != "gpt-5.6-luna" {
		t.Errorf("Models[gpt-5.6-luna].ID = %v, want gpt-5.6-luna", model.ID)
	}
	if model.ContextWindow != 272000 {
		t.Errorf("contextWindow = %v, want 272000 from the listing", model.ContextWindow)
	}
	if strings.Contains(out.String(), "gho_discovered_token") {
		t.Error("output leaked the discovered token value")
	}
}

// TestQuickstartBuildConfigCopilotExplicitCredentialWinsOverDiscoveredLogin
// is the regression test for a round-2 review's credential-precedence
// finding: before this fix, a discovered pi login won
// unconditionally, so an operator who
// explicitly passed -credential (naming a DIFFERENT GitHub account) had
// it silently ignored -- the persisted relay_github_token_file then kept
// steering every later run at pi's account instead of the one the
// operator actually named. A real pi login is present on disk
// here specifically to prove it loses.
func TestQuickstartBuildConfigCopilotExplicitCredentialWinsOverDiscoveredLogin(t *testing.T) {
	dp := newTestDeps(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeCopilotAuthFixture(t, home+"/.pi/agent/auth.json", "gho_discovered_token")

	var gotToken string
	stubListGitHubCopilotModels(t, func(ctx context.Context, caBundlePath, githubToken, upstream string) ([]meter.CopilotModel, error) {
		gotToken = githubToken
		return []meter.CopilotModel{{ID: "gpt-5.6-luna", ContextWindow: 272000}}, nil
	})

	opts := &quickstartOptions{NonInteractive: true, Route: "copilot", ModelID: "gpt-5.6-luna", Credential: "gho_explicit_token", CredentialProvided: true}
	var out bytes.Buffer
	cfg, credentialEnv, err := quickstartBuildConfig(dp, opts, newQuickstartPrompter(strings.NewReader("")), &out, t.TempDir())
	if err != nil {
		t.Fatalf("quickstartBuildConfig: %v", err)
	}
	if gotToken != "gho_explicit_token" {
		t.Errorf("token used for the model listing = %q, want the explicit -credential value, not the discovered login", gotToken)
	}
	if len(credentialEnv) != 1 || credentialEnv[0] != "GITHUB_COPILOT_TOKEN=gho_explicit_token" {
		t.Errorf("credentialEnv = %v, want GITHUB_COPILOT_TOKEN=gho_explicit_token", credentialEnv)
	}
	if cfg.Routes["copilot"].GitHubTokenFile != "" {
		t.Errorf("Routes[copilot].GitHubTokenFile = %v, want unset -- an explicit -credential must never persist the unrelated discovered login's path", cfg.Routes["copilot"].GitHubTokenFile)
	}
	if !strings.Contains(out.String(), "Using the GitHub Copilot token from -credential") {
		t.Errorf("output = %q, want it to name -credential as the source used", out.String())
	}
	if strings.Contains(out.String(), "gho_explicit_token") || strings.Contains(out.String(), "gho_discovered_token") {
		t.Error("output leaked a token value")
	}
}

// TestQuickstartBuildConfigCopilotEnvTokenWinsOverDiscoveredLogin is the
// same round-2 review finding's second precedence case: GITHUB_COPILOT_TOKEN
// must also win over a discovered login, exactly like -credential does.
func TestQuickstartBuildConfigCopilotEnvTokenWinsOverDiscoveredLogin(t *testing.T) {
	dp := newTestDeps(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeCopilotAuthFixture(t, home+"/.pi/agent/auth.json", "gho_discovered_token")
	t.Setenv("GITHUB_COPILOT_TOKEN", "gho_env_token")

	var gotToken string
	stubListGitHubCopilotModels(t, func(ctx context.Context, caBundlePath, githubToken, upstream string) ([]meter.CopilotModel, error) {
		gotToken = githubToken
		return []meter.CopilotModel{{ID: "gpt-5.6-luna", ContextWindow: 272000}}, nil
	})

	opts := &quickstartOptions{NonInteractive: true, Route: "copilot", ModelID: "gpt-5.6-luna"}
	var out bytes.Buffer
	cfg, credentialEnv, err := quickstartBuildConfig(dp, opts, newQuickstartPrompter(strings.NewReader("")), &out, t.TempDir())
	if err != nil {
		t.Fatalf("quickstartBuildConfig: %v", err)
	}
	if gotToken != "gho_env_token" {
		t.Errorf("token used for the model listing = %q, want the GITHUB_COPILOT_TOKEN value, not the discovered login", gotToken)
	}
	if len(credentialEnv) != 1 || credentialEnv[0] != "GITHUB_COPILOT_TOKEN=gho_env_token" {
		t.Errorf("credentialEnv = %v, want GITHUB_COPILOT_TOKEN=gho_env_token", credentialEnv)
	}
	if cfg.Routes["copilot"].GitHubTokenFile != "" {
		t.Errorf("Routes[copilot].GitHubTokenFile = %v, want unset -- GITHUB_COPILOT_TOKEN must never persist the unrelated discovered login's path", cfg.Routes["copilot"].GitHubTokenFile)
	}
}

// TestQuickstartBuildConfigCopilotNonInteractiveMissingModelIDNeverCallsListing
// is the regression test for another round-2 review finding: under
// -non-interactive with -model-id missing, the call must fail
// immediately -- before ever resolving a token or calling the (up to
// 15s) model listing, which would
// only be thrown away once the missing -model-id error fires anyway.
func TestQuickstartBuildConfigCopilotNonInteractiveMissingModelIDNeverCallsListing(t *testing.T) {
	dp := newTestDeps(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeCopilotAuthFixture(t, home+"/.pi/agent/auth.json", "gho_discovered_token")

	called := false
	stubListGitHubCopilotModels(t, func(ctx context.Context, caBundlePath, githubToken, upstream string) ([]meter.CopilotModel, error) {
		called = true
		return nil, nil
	})

	opts := &quickstartOptions{NonInteractive: true, Route: "copilot"}
	_, _, err := quickstartBuildConfig(dp, opts, newQuickstartPrompter(strings.NewReader("")), &bytes.Buffer{}, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "-model-id is required") {
		t.Fatalf("err = %v, want a -model-id-is-required error", err)
	}
	if called {
		t.Error("the model listing was called despite -model-id being missing under -non-interactive")
	}
}

// TestQuickstartBuildConfigChatGPTCodexPersistsDetectedAuthFilePath is
// the regression test for another round-2 review finding:
// quickstartDetectCodexLogin follows $CODEX_HOME, but before this fix
// the chatgpt-codex branch never
// persisted relay_codex_auth_file, so a daemon started later without
// CODEX_HOME set (e.g. from a launchd/systemd unit with a different
// environment) would silently fall back to ~/.codex/auth.json and could
// read a different login than the one quickstart just detected.
func TestQuickstartBuildConfigChatGPTCodexPersistsDetectedAuthFilePath(t *testing.T) {
	dp := newTestDeps(t)
	codexDir := t.TempDir()
	t.Setenv("CODEX_HOME", codexDir)
	authPath := writeCodexAuthFile(t, codexDir, map[string]any{
		"auth_mode": "chatgpt",
		"tokens": map[string]any{
			"access_token": fakeCodexJWT(t, time.Now().Add(24*time.Hour)),
			"account_id":   "acct-123",
		},
	})

	opts := &quickstartOptions{NonInteractive: true, Route: "chatgpt-codex"}
	cfg, _, err := quickstartBuildConfig(dp, opts, newQuickstartPrompter(strings.NewReader("")), &bytes.Buffer{}, t.TempDir())
	if err != nil {
		t.Fatalf("quickstartBuildConfig: %v", err)
	}
	if cfg.Routes["codex"].CodexAuthFile != authPath {
		t.Errorf("Routes[codex].CodexAuthFile = %v, want %q (the exact detected path, independent of a later process's own CODEX_HOME)", cfg.Routes["codex"].CodexAuthFile, authPath)
	}
}

// TestQuickstartBuildConfigChatGPTCodexAbsolutesRelativeCodexHome is the
// regression test for another round-3 review finding: a relative
// $CODEX_HOME (filepath.Join keeps it relative) must not save a relative
// relay_codex_auth_file -- a later process reading that config from a
// different working directory would resolve it against the wrong
// directory entirely.
func TestQuickstartBuildConfigChatGPTCodexAbsolutesRelativeCodexHome(t *testing.T) {
	dp := newTestDeps(t)
	cwd := t.TempDir()
	t.Chdir(cwd)
	t.Setenv("CODEX_HOME", "relative-codex-home")
	if err := os.MkdirAll(filepath.Join(cwd, "relative-codex-home"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeCodexAuthFile(t, filepath.Join(cwd, "relative-codex-home"), map[string]any{
		"auth_mode": "chatgpt",
		"tokens": map[string]any{
			"access_token": fakeCodexJWT(t, time.Now().Add(24*time.Hour)),
			"account_id":   "acct-123",
		},
	})

	opts := &quickstartOptions{NonInteractive: true, Route: "chatgpt-codex"}
	cfg, _, err := quickstartBuildConfig(dp, opts, newQuickstartPrompter(strings.NewReader("")), &bytes.Buffer{}, t.TempDir())
	if err != nil {
		t.Fatalf("quickstartBuildConfig: %v", err)
	}
	codexAuthFile := cfg.Routes["codex"].CodexAuthFile
	if codexAuthFile == "" {
		t.Fatal("Routes[codex].CodexAuthFile = \"\", want the detected path persisted")
	}
	if !filepath.IsAbs(codexAuthFile) {
		t.Errorf("Routes[codex].CodexAuthFile = %q, want an absolute path even though CODEX_HOME was relative", codexAuthFile)
	}
	wantPath := filepath.Join(cwd, "relative-codex-home", "auth.json")
	if codexAuthFile != wantPath {
		t.Errorf("Routes[codex].CodexAuthFile = %q, want %q", codexAuthFile, wantPath)
	}
}

// ---- -egress-ca-bundle: absolute'd and validated before persisting -----

// TestQuickstartBuildConfigAbsolutesEgressCABundle is the regression
// test for a round-3 review finding: a relative -egress-ca-bundle must be
// saved absolute -- a later process reading the config from a different
// working directory would otherwise resolve it against the wrong
// directory (or fail to find it at all).
func TestQuickstartBuildConfigAbsolutesEgressCABundle(t *testing.T) {
	dp := newTestDeps(t)
	cwd := t.TempDir()
	t.Chdir(cwd)
	caBundle := writeTestCABundleFixture(t, cwd)

	opts := &quickstartOptions{
		NonInteractive: true, Route: "openai", ModelHost: "http://127.0.0.1:1",
		ModelID: "local-model", ContextWindow: 131072, ContextWindowExplicit: true,
		EgressCABundle: "relative-ca.pem",
	}
	cfg, _, err := quickstartBuildConfig(dp, opts, newQuickstartPrompter(strings.NewReader("")), &bytes.Buffer{}, t.TempDir())
	if err != nil {
		t.Fatalf("quickstartBuildConfig: %v", err)
	}
	if cfg.EgressCABundle == nil || !filepath.IsAbs(*cfg.EgressCABundle) {
		t.Fatalf("EgressCABundle = %v, want an absolute path", cfg.EgressCABundle)
	}
	if *cfg.EgressCABundle != caBundle {
		t.Errorf("EgressCABundle = %q, want %q", *cfg.EgressCABundle, caBundle)
	}
}

// TestQuickstartBuildConfigRejectsUnloadableEgressCABundle proves a
// -egress-ca-bundle that fails to parse (not a real PEM certificate) is
// refused before ever being persisted into the config, rather than
// writing a value that only fails once a real run tries to load it.
func TestQuickstartBuildConfigRejectsUnloadableEgressCABundle(t *testing.T) {
	dp := newTestDeps(t)
	badPath := filepath.Join(t.TempDir(), "not-a-cert.pem")
	if err := os.WriteFile(badPath, []byte("this is not a PEM certificate"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	opts := &quickstartOptions{
		NonInteractive: true, Route: "openai", ModelHost: "http://127.0.0.1:1",
		ModelID: "local-model", ContextWindow: 131072, ContextWindowExplicit: true,
		EgressCABundle: badPath,
	}
	_, _, err := quickstartBuildConfig(dp, opts, newQuickstartPrompter(strings.NewReader("")), &bytes.Buffer{}, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "-egress-ca-bundle") {
		t.Fatalf("err = %v, want an -egress-ca-bundle error naming the unloadable file", err)
	}
}

// writeTestCABundleFixture writes a real, PEM-encoded self-signed
// certificate (via crypto/tls's own generated test cert, the simplest
// deterministic way to get bytes meter.OutboundTransport's own PEM
// parser accepts) into dir/relative-ca.pem and returns its absolute path.
func writeTestCABundleFixture(t *testing.T, dir string) string {
	t.Helper()
	server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer server.Close()
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	path := filepath.Join(dir, "relative-ca.pem")
	if err := os.WriteFile(path, pemBytes, 0o600); err != nil {
		t.Fatalf("write test CA bundle: %v", err)
	}
	return path
}

// TestQuickstartWarnIfWeakModelPrintsAdvisory proves the #14 advisory
// prints for a known-weak model id and stays silent for an unlisted one.
func TestQuickstartWarnIfWeakModelPrintsAdvisory(t *testing.T) {
	var out bytes.Buffer
	quickstartWarnIfWeakModel(&out, "gpt-4.1")
	if !strings.Contains(out.String(), "too weak for the build loop") {
		t.Errorf("output = %q, want the weak-model warning for gpt-4.1", out.String())
	}

	out.Reset()
	quickstartWarnIfWeakModel(&out, "gpt-5.6-luna")
	if out.String() != "" {
		t.Errorf("output = %q, want no warning for a model not on the advisory list", out.String())
	}
}

// TestDoctorSubscriptionLoginsLine is route-visibility's own regression
// test (2026-09-25): `factoryd doctor`'s info line must reflect exactly
// what quickstartDetectCodexLogin/quickstartDetectCopilotLogin detect, for
// both, one, and neither present -- reusing the same HOME/CODEX_HOME
// faking TestQuickstartDetectCodexLoginTrueForValidStructure and
// TestQuickstartDetectCopilotLoginTrueForValidPiAuthFile already use.
func TestDoctorSubscriptionLoginsLine(t *testing.T) {
	t.Run("both detected", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv("CODEX_HOME", t.TempDir())
		writeCodexAuthFile(t, os.Getenv("CODEX_HOME"), map[string]any{
			"auth_mode": "chatgpt",
			"tokens": map[string]any{
				"access_token": fakeCodexJWT(t, time.Now().Add(24*time.Hour)),
				"account_id":   "acct-123",
			},
		})
		writeCopilotAuthFixture(t, home+"/.pi/agent/auth.json", "gho_test_token")

		if got, want := doctorSubscriptionLoginsLine(), "subscription logins detected: chatgpt-codex, copilot"; got != want {
			t.Errorf("doctorSubscriptionLoginsLine() = %q, want %q", got, want)
		}
	})

	t.Run("one detected", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		t.Setenv("CODEX_HOME", t.TempDir())
		writeCodexAuthFile(t, os.Getenv("CODEX_HOME"), map[string]any{
			"auth_mode": "chatgpt",
			"tokens": map[string]any{
				"access_token": fakeCodexJWT(t, time.Now().Add(24*time.Hour)),
				"account_id":   "acct-123",
			},
		})

		if got, want := doctorSubscriptionLoginsLine(), "subscription logins detected: chatgpt-codex"; got != want {
			t.Errorf("doctorSubscriptionLoginsLine() = %q, want %q", got, want)
		}
	})

	t.Run("none detected", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		t.Setenv("CODEX_HOME", t.TempDir())

		if got, want := doctorSubscriptionLoginsLine(), "subscription logins detected: none"; got != want {
			t.Errorf("doctorSubscriptionLoginsLine() = %q, want %q", got, want)
		}
	})
}
