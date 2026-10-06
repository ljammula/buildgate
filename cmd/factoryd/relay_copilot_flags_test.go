package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"buildgate/internal/meter"
	"buildgate/internal/modelrole"
)

// TestRunMainWithReadyRejectsGitHubCopilotModeWithNoTokenSource proves a
// github-copilot route with no GitHub OAuth token source (neither
// github_token_file nor GITHUB_COPILOT_TOKEN) fails before ever touching
// Docker: modelrole.SelectRoute's own credential probe
// (resolveRouteCredentials) skips the route, so this comes back as
// modelrole.ErrNoRouteAvailable -- SelectRoute never echoes the
// resolver's own error text (see skipReasonCredentialUnavailable's own
// doc comment), so the missing-token detail itself is asserted directly
// against resolveGitHubCopilotToken in relay_copilot_flags_test.go's own
// TestResolveGitHubCopilotTokenMissingKeyListsPresentKeys and friends.
func TestRunMainWithReadyRejectsGitHubCopilotModeWithNoTokenSource(t *testing.T) {
	dp := newTestDeps(t)
	t.Setenv("GITHUB_COPILOT_TOKEN", "")
	t.Setenv("HOME", t.TempDir())
	path := isolateSessionConfig(t)
	writeSessionConfig(t, path, "sandbox_image: sandbox@sha256:"+strings.Repeat("a", 64)+"\n"+
		"routes:\n  copilot:\n    credential_mode: github-copilot\n"+
		"models:\n  m:\n    id: some-model\n    routes: [copilot]\n"+
		"roles:\n  execution:\n    model: m\n")
	err := runMainWithReady(dp, context.Background(), []string{
		"-ticket", "t", "-workspace", "/does/not/exist", "-spec", "/does/not/exist",
	}, nil)
	if !errors.Is(err, modelrole.ErrNoRouteAvailable) {
		t.Fatalf("runMainWithReady with github-copilot mode and no token source: error = %v, want modelrole.ErrNoRouteAvailable", err)
	}
}

// TestRunMainWithReadyRefusesInvalidRolesBlock proves the direct-run path
// validates roles: too, silently (error only, no warning print -- see
// validateRoles' own doc comment): an unknown model_aliases entry named
// by a role must refuse the whole invocation. `run` has no -config flag
// of its own, so this isolates HOME/XDG_CONFIG_HOME (isolateSessionConfig)
// and writes the invalid roles: block at the default session-config
// search path resolveSettings() would otherwise resolve for a bare
// invocation.
func TestRunMainWithReadyRefusesInvalidRolesBlock(t *testing.T) {
	dp := newTestDeps(t)
	path := isolateSessionConfig(t)
	writeSessionConfig(t, path, "roles:\n  execution:\n    model: does-not-exist\n")
	err := runMainWithReady(dp, context.Background(), []string{
		"-ticket", "t", "-workspace", "/does/not/exist", "-spec", "/does/not/exist",
	}, nil)
	if err == nil || !strings.Contains(err.Error(), "roles.execution") {
		t.Fatalf("runMainWithReady with an invalid roles: block: error = %v, want it to name roles.execution", err)
	}
}

// TestRunMainWithReadyRejectsReferenceOracleInLoopRetryWithoutCommand is
// the regression test for a second real finding (found via review, same
// class as the Temporal check above): -reference-oracle-in-loop-retry set
// without one of its three required siblings (-reference-oracle-dir/
// -mount-path/-command) previously no-opped silently -- the build-phase
// gating condition just stayed false with no error, identical in shape to
// the Temporal-path silent-no-op this same review round already fixed.
func TestRunMainWithReadyRejectsReferenceOracleInLoopRetryWithoutCommand(t *testing.T) {
	dp := newTestDeps(t)
	t.Setenv("HOME", t.TempDir())
	path := isolateSessionConfig(t)
	writeSessionConfig(t, path, "sandbox_image: example.test/worker@sha256:"+strings.Repeat("a", 64)+"\n"+
		"routes:\n  local:\n    allow_no_credential: true\n    upstream: https://model-a.example.invalid\n"+
		"models:\n  m:\n    id: some-model\n    routes: [local]\n"+
		"roles:\n  execution:\n    model: m\n")
	err := runMainWithReady(dp, context.Background(), []string{
		"-ticket", "t", "-workspace", "/does/not/exist", "-spec", "/does/not/exist",
		// -registry-proxy defaults on for this default, model-backed
		// build_app.py, and -registry-proxy-image has no built-in default
		// (see that flag's own help) -- disabled explicitly so this test
		// reaches its own -reference-oracle-in-loop-retry check instead of
		// failing earlier on an unrelated, unconfigured registry proxy.
		"-registry-proxy=false",
		"-reference-oracle-in-loop-retry",
		"-reference-oracle-dir", "/does/not/exist",
		"-reference-oracle-mount-path", "verify",
		// -reference-oracle-command deliberately omitted.
	}, nil)
	if err == nil || !strings.Contains(err.Error(), "-reference-oracle-in-loop-retry requires") {
		t.Fatalf("runMainWithReady with -reference-oracle-in-loop-retry and no -reference-oracle-command: error = %v, want it to name the missing precondition", err)
	}
}

func TestRunMainWithReadyUsesPiforkSessionDefaults(t *testing.T) {
	dp := newTestDeps(t)
	t.Setenv("GITHUB_COPILOT_TOKEN", "")
	configPath := isolateSessionConfig(t)
	writeSessionConfig(t, configPath, `
sandbox_image: worker@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
routes:
  copilot:
    credential_mode: github-copilot
models:
  m:
    id: entitled-copilot-model
    routes: [copilot]
roles:
  execution:
    model: m
    harness: pifork
`)
	err := runMainWithReady(dp, context.Background(), []string{
		"-ticket", "t", "-workspace", "/does/not/exist", "-spec", "/does/not/exist",
	}, nil)
	if err == nil {
		t.Fatal("runMainWithReady with pifork session defaults: err = nil, want a host-only Copilot auth failure")
	}
	if strings.Contains(err.Error(), "entitled-copilot-model") {
		t.Fatalf("runMainWithReady error leaked the configured model id: error = %v", err)
	}
}

func TestRunMainWithReadyPreservesPiforkSessionRelayPath(t *testing.T) {
	dp := newTestDeps(t)
	t.Setenv("GITHUB_COPILOT_TOKEN", "test-token")
	configPath := isolateSessionConfig(t)
	writeSessionConfig(t, configPath, `
sandbox_image: worker@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
routes:
  copilot:
    credential_mode: github-copilot
    allowed_path_prefix: not-a-valid-prefix
    worker_base_path: /custom
models:
  m:
    id: entitled-copilot-model
    routes: [copilot]
roles:
  execution:
    model: m
    harness: pifork
`)
	err := runMainWithReady(dp, context.Background(), []string{
		"-ticket", "t", "-workspace", "/does/not/exist", "-spec", "/does/not/exist",
	}, nil)
	// modelrole.ValidateAllowedPolicies (validateRoles' own config-load
	// check, added for the live M3 walk finding -- see its own doc
	// comment) now catches the invalid allowed_path_prefix before
	// runMainWithReady ever reaches modelrole.SelectRoute/
	// GITHUB_COPILOT_TOKEN's own credential probe, so this no longer
	// comes back wrapped in modelrole.ErrNoRouteAvailable -- it proves
	// the configured path was carried through to a real validation
	// attempt (not silently replaced by the preset) by checking that the
	// refusal names the configured route, not by asserting on
	// ErrNoRouteAvailable.
	if err == nil || !strings.Contains(err.Error(), "copilot") {
		t.Fatalf("runMainWithReady with a pifork session relay path: error = %v, want a refusal naming the configured route", err)
	}
}

// TestRunMainWithReadySessionConfigRegistryProxyImage is the regression
// test for a real finding (adversarial review of the ghcr-removal
// change): -registry-proxy defaults on for the default, model-backed
// build_app.py, but sessionconfig.Settings had no RegistryProxyImage
// field and runMainWithReady never read registry_proxy_image from
// session config at all -- so every bare run (including the common case
// right after `make install`, which writes exactly this config) failed
// "-registry-proxy configuration: registry proxy image must be pinned by
// a sha256 digest" before ever reaching Docker. With sandbox_image
// and registry_proxy_image both configured via session
// config (mirroring what `make install`/`factoryd configure-images`
// writes) and a nonexistent -sandbox-docker binary, this must fail on
// the Docker step, never on registry-proxy configuration.
func TestRunMainWithReadySessionConfigRegistryProxyImage(t *testing.T) {
	dp := newTestDeps(t)
	t.Setenv("ANTHROPIC_API_KEY", "test-key")
	configPath := isolateSessionConfig(t)
	writeSessionConfig(t, configPath, "\n"+
		"sandbox_docker: /does/not/exist/docker\n"+
		"sandbox_image: worker@sha256:"+strings.Repeat("a", 64)+"\n"+
		"registry_proxy_image: proxy@sha256:"+strings.Repeat("c", 64)+"\n")
	err := runMainWithReady(dp, context.Background(), []string{
		"-ticket", "t", "-workspace", "/does/not/exist", "-spec", "/does/not/exist",
	}, nil)
	if err == nil {
		t.Fatal("runMainWithReady unexpectedly succeeded with a nonexistent -sandbox-docker binary")
	}
	if strings.Contains(err.Error(), "-registry-proxy configuration") || strings.Contains(err.Error(), "must be pinned by a sha256 digest") {
		t.Fatalf("runMainWithReady rejected the session-config registry_proxy_image instead of applying it: error = %v", err)
	}
}

// TestRunMainWithReadyRejectsGitHubCopilotModeWithoutWorkerModelID proves
// a models: entry is required to name an id (Copilot's API is OpenAI-
// compatible; the worker must reach the relay as such a provider) --
// checked structurally by ValidateRouting before ever resolving a route,
// so this fails before the missing-token-source error, naming the more
// fundamental misconfiguration first.
func TestRunMainWithReadyRejectsGitHubCopilotModeWithoutWorkerModelID(t *testing.T) {
	dp := newTestDeps(t)
	// Not t.Parallel(): isolateSessionConfig and the GITHUB_COPILOT_TOKEN
	// override below call t.Setenv, which Go forbids once a test is
	// parallel.
	path := isolateSessionConfig(t)
	t.Setenv("GITHUB_COPILOT_TOKEN", "")
	writeSessionConfig(t, path, "sandbox_image: sandbox@sha256:deadbeef\n"+
		"routes:\n  copilot:\n    credential_mode: github-copilot\n"+
		"models:\n  m:\n    routes: [copilot]\n"+
		"roles:\n  execution:\n    model: m\n")
	err := runMainWithReady(dp, context.Background(), []string{
		"-ticket", "t", "-workspace", "/does/not/exist", "-spec", "/does/not/exist",
	}, nil)
	if err == nil || !strings.Contains(err.Error(), "models.m: id is required") {
		t.Fatalf("runMainWithReady with github-copilot mode and no worker model id: error = %v, want it to name the missing models.m.id", err)
	}
}

func TestRunMainWithReadyRejectsResponsesWithoutWorkerModelID(t *testing.T) {
	dp := newTestDeps(t)
	path := isolateSessionConfig(t)
	writeSessionConfig(t, path, "sandbox_image: sandbox@sha256:deadbeef\n"+
		"routes:\n  local:\n    allow_no_credential: true\n    upstream: https://model-a.example.invalid\n"+
		"models:\n  m:\n    api: "+meter.RequestFormatOpenAIResponses+"\n    routes: [local]\n"+
		"roles:\n  execution:\n    model: m\n")
	err := runMainWithReady(dp, context.Background(), []string{
		"-ticket", "t", "-workspace", "/does/not/exist", "-spec", "/does/not/exist",
	}, nil)
	if err == nil || !strings.Contains(err.Error(), "models.m: id is required") {
		t.Fatalf("runMainWithReady with Responses and no worker model: error = %v, want it to name the missing models.m.id", err)
	}
}

// TestRunMainWithReadyRejectsExplicitCredentialHeaderWithGitHubCopilotMode
// proves a routes: entry's credential_header is rejected on a non-static
// (github-copilot) route rather than silently ignored (Authorization is
// always forced in this mode) -- ValidateRouting's own structural
// per-route check, verified before ever resolving a route.
func TestRunMainWithReadyRejectsExplicitCredentialHeaderWithGitHubCopilotMode(t *testing.T) {
	dp := newTestDeps(t)
	path := isolateSessionConfig(t)
	writeSessionConfig(t, path, "sandbox_image: sandbox@sha256:deadbeef\n"+
		"routes:\n  copilot:\n    credential_mode: github-copilot\n    credential_header: Authorization\n"+
		"models:\n  m:\n    id: some-model\n    routes: [copilot]\n"+
		"roles:\n  execution:\n    model: m\n")
	err := runMainWithReady(dp, context.Background(), []string{
		"-ticket", "t", "-workspace", "/does/not/exist", "-spec", "/does/not/exist",
	}, nil)
	if err == nil || !strings.Contains(err.Error(), "credential_header only applies to a static route") {
		t.Fatalf("runMainWithReady with an explicit credential_header on a github-copilot route: error = %v, want the explicit-header rejection", err)
	}
}

// TestWorkerHelpMentionsRoutesConfigMigrationHint is
// TestWorkerHelpMentionsGitHubCopilotFlags' own successor: the
// per-route -relay-credential-mode/-relay-github-token-file flags it
// used to prove are named in `worker -h` are gone (routes:/models:/
// roles: is the only session-config schema now). -relay-usage-format
// itself is also gone: the resolved route/model decides usage format
// with no CLI or session override anywhere in routes: mode.
func TestWorkerHelpMentionsRoutesConfigMigrationHint(t *testing.T) {
	dp := newTestDeps(t)
	out := captureStderr(t, func() {
		_, _, _ = loadTestWorkerConfig(dp, []string{"-h"})
	})
	for _, removed := range []string{"-relay-credential-mode", "-relay-github-token-file", "-relay-usage-format"} {
		if strings.Contains(out, removed) {
			t.Errorf("worker -h output still names removed per-route flag %q; output:\n%s", removed, out)
		}
	}
}

// TestDoctorCheckCopilotTokenExchangeFailsWithNoTokenSource is the doctor-
// level counterpart to TestRunMainWithReadyRejectsGitHubCopilotModeWithNoTokenSource:
// with neither -relay-github-token-file nor GITHUB_COPILOT_TOKEN set, the
// check must fail naming the missing token, without ever attempting a
// network call (resolveGitHubCopilotToken returns "" before
// meter.ExchangeGitHubCopilotToken is ever reached).
func TestDoctorCheckCopilotTokenExchangeFailsWithNoTokenSource(t *testing.T) {
	t.Setenv("GITHUB_COPILOT_TOKEN", "")
	t.Setenv("HOME", t.TempDir())
	check := doctorCheckCopilotTokenExchange(context.Background(), "", "", "", doctorRoutesModeRouteKeys("copilot", "copilot-model"))
	if check.Err == nil || !strings.Contains(check.Err.Error(), "no GitHub OAuth token configured") {
		t.Fatalf("doctorCheckCopilotTokenExchange with no token source: Err = %v, want it to name the missing token", check.Err)
	}
}

// TestResolveGitHubCopilotTokenReadsPiAuthFile proves the auth.json parsing
// this relay's -relay-github-token-file reads pi's (or a pi fork's) own credential
// store: the "refresh" field of its "github-copilot" entry, never "access"
// (pi's own short-lived, already-cached Copilot token -- see
// piAuthFileCredential's own doc comment for why this relay ignores it and
// performs its own exchange instead).
func TestResolveGitHubCopilotTokenReadsPiAuthFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	auth := map[string]any{
		"github-copilot": map[string]any{
			"type":    "oauth",
			"refresh": "gho_the_real_github_oauth_token",
			"access":  "cached-copilot-token-must-not-be-used",
		},
	}
	data, err := json.Marshal(auth)
	if err != nil {
		t.Fatalf("marshal fixture auth.json: %v", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write fixture auth.json: %v", err)
	}
	token, err := resolveGitHubCopilotToken(path, "")
	if err != nil {
		t.Fatalf("resolveGitHubCopilotToken: %v", err)
	}
	if token != "gho_the_real_github_oauth_token" {
		t.Fatalf("resolveGitHubCopilotToken = %q, want the \"refresh\" field's value", token)
	}
}

// TestResolveGitHubCopilotTokenReadsCustomKey proves -relay-github-token-key
// looks up a different auth.json entry than the "github-copilot" default,
// for a pi fork that registers its Copilot provider under
// another id.
func TestResolveGitHubCopilotTokenReadsCustomKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	auth := map[string]any{
		"fork-copilot": map[string]any{
			"type":    "oauth",
			"refresh": "gho_fork_token",
		},
	}
	data, err := json.Marshal(auth)
	if err != nil {
		t.Fatalf("marshal fixture auth.json: %v", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write fixture auth.json: %v", err)
	}
	token, err := resolveGitHubCopilotToken(path, "fork-copilot")
	if err != nil {
		t.Fatalf("resolveGitHubCopilotToken: %v", err)
	}
	if token != "gho_fork_token" {
		t.Fatalf("resolveGitHubCopilotToken = %q, want the custom key's \"refresh\" value", token)
	}
}

// TestResolveGitHubCopilotTokenMissingKeyListsPresentKeys proves a wrong
// -relay-github-token-key fails naming the keys the auth.json actually
// has (never their values), so a naming mismatch is obvious rather than
// indistinguishable from a genuinely empty file.
func TestResolveGitHubCopilotTokenMissingKeyListsPresentKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	auth := map[string]any{
		"anthropic": map[string]any{"type": "oauth", "refresh": "should-never-appear-in-error"},
		"openai":    map[string]any{"type": "oauth", "refresh": "should-never-appear-in-error-either"},
	}
	data, err := json.Marshal(auth)
	if err != nil {
		t.Fatalf("marshal fixture auth.json: %v", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write fixture auth.json: %v", err)
	}
	_, err = resolveGitHubCopilotToken(path, "github-copilot")
	if err == nil {
		t.Fatal("resolveGitHubCopilotToken with a missing key = nil error, want one naming the present keys")
	}
	for _, want := range []string{"anthropic", "openai"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %v, want it to list present key %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "should-never-appear-in-error") {
		t.Fatalf("error leaked a credential value: %v", err)
	}
}

// TestResolveGitHubCopilotTokenFallsBackToEnv proves the documented
// fallback order: -relay-github-token-file left empty means
// GITHUB_COPILOT_TOKEN is consulted instead.
func TestResolveGitHubCopilotTokenFallsBackToEnv(t *testing.T) {
	t.Setenv("GITHUB_COPILOT_TOKEN", "gho_from_env")
	token, err := resolveGitHubCopilotToken("", "")
	if err != nil {
		t.Fatalf("resolveGitHubCopilotToken: %v", err)
	}
	if token != "gho_from_env" {
		t.Fatalf("resolveGitHubCopilotToken = %q, want the GITHUB_COPILOT_TOKEN value", token)
	}
}

func TestResolveGitHubCopilotTokenDiscoveryReadsPiAuth(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("GITHUB_COPILOT_TOKEN", "")
	piPath := filepath.Join(home, ".pi", "agent", "auth.json")
	writeCopilotAuthFixture(t, piPath, "gho_pi_fallback")

	token, source, err := resolveGitHubCopilotTokenSource("", "")
	if err != nil {
		t.Fatalf("resolveGitHubCopilotTokenSource: %v", err)
	}
	if token != "gho_pi_fallback" || source != piPath {
		t.Fatalf("resolved token/source = %q/%q, want Pi auth %q/%q", token, source, "gho_pi_fallback", piPath)
	}
}

func TestResolveGitHubCopilotTokenExplicitAndEnvSourcesPrecedeDiscovery(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	discoveredPath := filepath.Join(home, ".pi", "agent", "auth.json")
	writeCopilotAuthFixture(t, discoveredPath, "gho_discovered")
	t.Setenv("GITHUB_COPILOT_TOKEN", "gho_environment")

	explicitPath := filepath.Join(t.TempDir(), "auth.json")
	writeCopilotAuthFixture(t, explicitPath, "gho_explicit")
	token, source, err := resolveGitHubCopilotTokenSource(explicitPath, "")
	if err != nil {
		t.Fatalf("explicit resolveGitHubCopilotTokenSource: %v", err)
	}
	if token != "gho_explicit" || source != explicitPath {
		t.Fatalf("explicit token/source = %q/%q, want %q/%q", token, source, "gho_explicit", explicitPath)
	}

	token, source, err = resolveGitHubCopilotTokenSource("", "")
	if err != nil {
		t.Fatalf("environment resolveGitHubCopilotTokenSource: %v", err)
	}
	if token != "gho_environment" || source != "env:"+gitHubCopilotTokenEnv {
		t.Fatalf("environment token/source = %q/%q, want %q/%q", token, source, "gho_environment", "env:"+gitHubCopilotTokenEnv)
	}
}

func TestResolveGitHubCopilotTokenDiscoveryRedactsMalformedCredential(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("GITHUB_COPILOT_TOKEN", "")
	path := filepath.Join(home, ".pi", "agent", "auth.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir auth fixture: %v", err)
	}
	secret := "gho_secret_must_not_be_returned"
	if err := os.WriteFile(path, []byte(`{"github-copilot":{"type":"oauth","refresh":"`+secret), 0o600); err != nil {
		t.Fatalf("write malformed auth fixture: %v", err)
	}
	_, _, err := resolveGitHubCopilotTokenSource("", "")
	if err == nil {
		t.Fatal("malformed discovered auth = nil error, want parse failure")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("malformed auth error leaked credential: %v", err)
	}
	for _, want := range []string{filepath.Join(home, ".pi", "agent", "auth.json")} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("malformed auth error = %v, want it to identify checked path %q", err, want)
		}
	}
}

func writeCopilotAuthFixture(t *testing.T, path, token string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir auth fixture: %v", err)
	}
	data, err := json.Marshal(map[string]any{
		"github-copilot": map[string]any{"type": "oauth", "refresh": token},
	})
	if err != nil {
		t.Fatalf("marshal auth fixture: %v", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write auth fixture: %v", err)
	}
}

// TestRunMainWithReadyAppliesRelayAllowedPathPrefixFromSessionConfigWhenUnset
// is the regression test for an adversarial-review finding: USAGE.md
// documents -relay-allowed-path-prefix (like -relay-worker-base-path and
// -relay-credential-header) as also settable via session config alone,
// with no CLI flag needed, but run_ticket.go never actually read
// settings.RelayAllowedPathPrefix before this fix -- an operator setting
// only this key in session config got the hardcoded "/v1/messages"
// default regardless. Proven with an intentionally invalid session-config
// value (no leading "/"): this only fails if the config-derived value
// genuinely reached RoutePolicy instead of being silently dropped in
// favor of the valid hardcoded default. It now fails at validateRoles'
// own config-load check (modelrole.ValidateAllowedPolicies, added for
// the live M3 walk finding), before runMainWithReady ever reaches
// modelrole.SelectRoute/Docker, rather than wrapped in
// modelrole.ErrNoRouteAvailable at launch time.
func TestRunMainWithReadyAppliesRelayAllowedPathPrefixFromSessionConfigWhenUnset(t *testing.T) {
	dp := newTestDeps(t)
	path := isolateSessionConfig(t)
	writeSessionConfig(t, path, "sandbox_image: sandbox@sha256:"+strings.Repeat("a", 64)+"\n"+
		"routes:\n  local:\n    allow_no_credential: true\n    upstream: https://model-a.example.invalid\n    allowed_path_prefix: not-a-valid-prefix\n"+
		"models:\n  m:\n    id: some-model\n    routes: [local]\n"+
		"roles:\n  execution:\n    model: m\n")
	err := runMainWithReady(dp, context.Background(), []string{
		"-ticket", "t", "-workspace", "/does/not/exist", "-spec", "/does/not/exist",
	}, nil)
	if err == nil || !strings.Contains(err.Error(), "allowed path prefix") {
		t.Fatalf("runMainWithReady with an invalid session-config routes.local.allowed_path_prefix: error = %v, want a refusal naming the invalid prefix (the invalid prefix makes the route unusable)", err)
	}
}

// TestRunMainWithReadyAppliesRouteFieldsFromSessionConfig is the
// regression test for a live finding: relay_upstream/
// relay_allow_no_credential/relay_allow_plaintext_upstream, once declared
// as top-level sessionconfig.Config keys, never actually carried through
// to the resolved Settings this function reads. routes:/models:/roles:
// is the only session-config schema now, so each of these is a
// routes.<name> field instead, and this proves each one genuinely reaches
// sandbox.RoutePolicy through modelrole.SelectRoute -- an intentionally
// invalid or telling value fires a specific, distinguishing error/
// acceptance before Docker is ever touched, rather than falling back to
// a hardcoded default silently. relay_usage_format is gone entirely
// (see TestWorkerHelpMentionsRoutesConfigMigrationHint and
// legacyRoutingKeys): the resolved route/model decides usage format with
// no CLI or session override anywhere in routes: mode.
func TestRunMainWithReadyAppliesRouteFieldsFromSessionConfig(t *testing.T) {
	dp := newTestDeps(t)
	baseArgs := []string{
		"-ticket", "t", "-workspace", "/does/not/exist", "-spec", "/does/not/exist",
	}
	// allow_no_credential is proven by its absence instead: with
	// ANTHROPIC_API_KEY unset, a relay-backed run is refused outright
	// unless this opt-out reached runMainWithReady via the route.
	t.Run("routes.allow_no_credential", func(t *testing.T) {
		t.Setenv("ANTHROPIC_API_KEY", "")
		path := isolateSessionConfig(t)
		writeSessionConfig(t, path, "sandbox_image: sandbox@sha256:"+strings.Repeat("a", 64)+"\n"+
			"routes:\n  local:\n    allow_no_credential: true\n    upstream: https://model-a.example.invalid\n"+
			"models:\n  m:\n    id: some-model\n    routes: [local]\n"+
			"roles:\n  execution:\n    model: m\n")
		err := runMainWithReady(dp, context.Background(), baseArgs, nil)
		if err != nil && strings.Contains(err.Error(), "requires") && strings.Contains(err.Error(), "ANTHROPIC_API_KEY") {
			t.Fatalf("runMainWithReady with routes.local.allow_no_credential: error = %v, want the credential requirement waived", err)
		}
	})
	// allow_plaintext_upstream is the inverse: an http:// upstream is
	// rejected by ValidateUpstreamScheme unless the opt-in reached the
	// policy, so the run getting *past* that rejection is the proof.
	t.Run("routes.allow_plaintext_upstream", func(t *testing.T) {
		t.Setenv("ANTHROPIC_API_KEY", "")
		path := isolateSessionConfig(t)
		writeSessionConfig(t, path, "sandbox_image: sandbox@sha256:"+strings.Repeat("a", 64)+"\n"+
			"routes:\n  local:\n    allow_no_credential: true\n    allow_plaintext_upstream: true\n    upstream: http://127.0.0.1:8080\n"+
			"models:\n  m:\n    id: some-model\n    routes: [local]\n"+
			"roles:\n  execution:\n    model: m\n")
		err := runMainWithReady(dp, context.Background(), baseArgs, nil)
		if err != nil && strings.Contains(err.Error(), "must be an https:// URL") {
			t.Fatalf("runMainWithReady with routes.local.allow_plaintext_upstream: error = %v, want the loopback plaintext upstream accepted", err)
		}
	})
}

// TestRunMainWithReadyAppliesRelayUpstreamFromSessionConfigWhenUnset is the
// regression test for a live finding: routes.<name>.upstream must
// actually reach sandbox.RoutePolicy, not fall back to any hardcoded
// default -- a plaintext http:// upstream with no allow_plaintext_upstream
// opt-in fails ValidateUpstreamScheme's "must be an https:// URL"
// rejection only if the configured value genuinely reached RoutePolicy.
// It now fails at validateRoles' own config-load check
// (modelrole.ValidateAllowedPolicies, added for the live M3 walk
// finding), before runMainWithReady ever reaches modelrole.SelectRoute,
// rather than wrapped in modelrole.ErrNoRouteAvailable at launch time.
func TestRunMainWithReadyAppliesRelayUpstreamFromSessionConfigWhenUnset(t *testing.T) {
	dp := newTestDeps(t)
	path := isolateSessionConfig(t)
	writeSessionConfig(t, path, "sandbox_image: sandbox@sha256:"+strings.Repeat("a", 64)+"\n"+
		"routes:\n  local:\n    allow_no_credential: true\n    upstream: http://127.0.0.1:8080\n"+
		"models:\n  m:\n    id: some-model\n    routes: [local]\n"+
		"roles:\n  execution:\n    model: m\n")
	err := runMainWithReady(dp, context.Background(), []string{
		"-ticket", "t", "-workspace", "/does/not/exist", "-spec", "/does/not/exist",
	}, nil)
	if err == nil || !strings.Contains(err.Error(), "must be an https:// URL") {
		t.Fatalf("runMainWithReady with routes.local.upstream (plaintext, no allow-plaintext opt-in): error = %v, want a refusal naming the plaintext upstream, proving the configured upstream (not a hardcoded default) reached RoutePolicy", err)
	}
}

// captureStderr redirects os.Stderr for the duration of fn and returns
// what it wrote -- flag.FlagSet's default Usage/PrintDefaults output goes
// there, not os.Stdout (see captureStdout in load_agent_evidence_test.go
// for this file's own os.Stdout counterpart).
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	orig := os.Stderr
	os.Stderr = w
	defer func() { os.Stderr = orig }()

	fn()

	if err := w.Close(); err != nil {
		t.Fatalf("close pipe writer: %v", err)
	}
	out := make([]byte, 1<<20)
	n, _ := r.Read(out)
	return string(out[:n])
}
