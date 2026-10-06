package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"buildgate/internal/request"
	"buildgate/internal/sessionconfig"
)

// validRoutesModeSessionConfig is a minimal routes:/models:/roles: block
// that passes sessionconfig.ValidateRouting cleanly -- the fixture every
// test below uses. Since Phase 2C-1 (route selection) a schema-clean
// routes: config like this one is genuinely usable by the direct build/
// conformity/drafting paths (modelrole.SelectRoute), and since Phase 2D
// the Temporal path resolves a route too (modelrole.CheckRouteBinding,
// Activities.CheckRoute/ResolveRouteCredentials) -- neither refuses it. codex's own
// credential (~/.codex/auth.json) is not expected to exist in this test
// environment, so a real execution attempt against this fixture still
// fails -- for a route-credential reason, never for the old blanket
// "not used by jobs yet" one.
const validRoutesModeSessionConfig = `routes:
  codex:
    credential_mode: chatgpt-codex
models:
  luna:
    id: gpt-5.6-luna
    routes: [codex]
roles:
  execution:
    model: luna
`

// TestApplySessionConfigAcceptsValidRoutesModeConfig proves worker's
// own applySessionConfig (its startup session-config resolution) no
// longer refuses a routes:/models: config that passes ValidateRouting
// cleanly.
func TestApplySessionConfigAcceptsValidRoutesModeConfig(t *testing.T) {
	path := isolateSessionConfig(t)
	writeSessionConfig(t, path, validRoutesModeSessionConfig)
	fs, _ := newWorkerFlags()
	settings, _, err := applySessionConfig(fs, "")
	if err != nil {
		t.Fatalf("applySessionConfig: %v, want a schema-valid routes: config accepted", err)
	}
	if len(settings.Routes) == 0 {
		t.Error("settings.Routes is empty, want the routes: config applied")
	}
}

// TestSubmitMainAcceptsValidRoutesModeConfig is submit's own start-site
// counterpart: a routes:-configured session accepts a submission that
// never names -model (per-request model choice against routes:/models:
// is a later change; -model itself was removed entirely).
func TestSubmitMainAcceptsValidRoutesModeConfig(t *testing.T) {
	dp := newTestDeps(t)
	workspace := t.TempDir()
	writeTestFactoryYML(t, workspace, `verify_command: "make ci-verify"
preflight_profile: brownfield
`)
	configPath := filepath.Join(t.TempDir(), "config.yml")
	if err := os.WriteFile(configPath, []byte(validRoutesModeSessionConfig), 0o644); err != nil {
		t.Fatal(err)
	}
	dataDir := t.TempDir()
	err := submitMain(dp, []string{"-config", configPath, "-data-dir", dataDir, workspace, "Add a new func"})
	if err != nil {
		t.Fatalf("submitMain with a valid routes: config: err = %v, want nil", err)
	}
	requests, err := request.List(dataDir)
	if err != nil || len(requests) != 1 {
		t.Fatalf("List = %v, %v; want exactly one request written", requests, err)
	}
}

// TestRunMainWithReadyRoutesModeAttemptsARealRun is direct `factoryd
// run`'s own start-site counterpart: since Phase 2C-1, a valid routes:
// config no longer refuses at the roles: stage -- runMainWithReady
// resolves an execution route (modelrole.SelectRoute) and proceeds,
// failing for an unrelated, later reason (this fixture's ticket/
// workspace/spec paths don't exist).
func TestRunMainWithReadyRoutesModeAttemptsARealRun(t *testing.T) {
	dp := newTestDeps(t)
	path := isolateSessionConfig(t)
	writeSessionConfig(t, path, validRoutesModeSessionConfig)
	err := runMainWithReady(dp, context.Background(), []string{
		"-ticket", "t", "-workspace", "/does/not/exist", "-spec", "/does/not/exist",
	}, nil)
	if err == nil {
		t.Fatal("runMainWithReady: err = nil, want a failure (the workspace/spec paths do not exist)")
	}
	if strings.Contains(err.Error(), "not used by jobs yet") {
		t.Errorf("err = %v, must not refuse routes: mode itself any more", err)
	}
}

// TestRunMainWithReadyRoutesModeNoLongerRefusesTemporal proves 2D removed
// the up-front -temporal-address refusal for a routes:-configured run:
// -temporal-address 127.0.0.1:1 is unreachable, so runMainWithReady fails
// for a reason of its own (an earlier check, or the dial) -- never the
// removed refusal.
func TestRunMainWithReadyRoutesModeNoLongerRefusesTemporal(t *testing.T) {
	dp := newTestDeps(t)
	path := isolateSessionConfig(t)
	writeSessionConfig(t, path, validRoutesModeSessionConfig)
	err := runMainWithReady(dp, context.Background(), []string{
		"-ticket", "t", "-workspace", "/does/not/exist", "-spec", "/does/not/exist",
		"-temporal-address", "127.0.0.1:1",
	}, nil)
	if err == nil {
		t.Fatal("runMainWithReady: err = nil, want a failure (the workspace/spec paths do not exist)")
	}
	if strings.Contains(err.Error(), "cannot dispatch to the Temporal Worker") {
		t.Errorf("err = %v, must not refuse routes: mode + Temporal any more (2D)", err)
	}
}

// TestRunMainWithReadyRoutesModeRefusesLegacyRouteFlags proves a removed
// legacy -relay-* flag is an unknown flag on `run`'s own flag.FlagSet
// (Go's flag package refuses it itself, naming it), one representative
// flag.
func TestRunMainWithReadyRoutesModeRefusesLegacyRouteFlags(t *testing.T) {
	dp := newTestDeps(t)
	path := isolateSessionConfig(t)
	writeSessionConfig(t, path, validRoutesModeSessionConfig)
	err := runMainWithReady(dp, context.Background(), []string{
		"-ticket", "t", "-workspace", "/does/not/exist", "-spec", "/does/not/exist",
		"-relay-upstream", "https://example.invalid",
	}, nil)
	if err == nil || !strings.Contains(err.Error(), "relay-upstream") {
		t.Fatalf("runMainWithReady with -relay-upstream and a routes: config: err = %v, want a refusal naming relay-upstream", err)
	}
}

// TestDaemonMainNoLongerRefusesValidRoutesModeConfig is `factoryd
// daemon`'s own start-site counterpart: since Phase 2D the daemon (the
// Temporal Worker process itself) binds a submitted route to its own
// session config (modelrole.CheckRouteBinding, Activities.CheckRoute/ResolveRouteCredentials)
// instead of refusing routes: mode outright -- a schema-valid routes:
// config now gets all the way past config loading/validateRoles to
// daemonMain's own next requirement, the required -temporal-address/
// -repository check, exactly like a legacy config always has.
func TestDaemonMainNoLongerRefusesValidRoutesModeConfig(t *testing.T) {
	dp := newTestDeps(t)
	path := isolateSessionConfig(t)
	writeSessionConfig(t, path, validRoutesModeSessionConfig)
	err := daemonMain(dp, []string{"-data-dir", t.TempDir()})
	if err == nil {
		t.Fatal("daemonMain with a valid routes: config and no -temporal-address/-repository: want error, got nil")
	}
	if strings.Contains(err.Error(), "cannot dispatch to the Temporal Worker") {
		t.Errorf("err = %v, must not refuse routes: mode any more (2D)", err)
	}
	if !strings.Contains(err.Error(), "-temporal-address and -repository are required") {
		t.Fatalf("error = %q, want the required-flag error (proving routes: mode got all the way past config loading)", err.Error())
	}
}

// TestDaemonMainRoutesModeRefusesLegacyCredentialFlags proves
// daemonMain refuses the same legacy -relay-* flags runMainWithReady
// refuses once routes:/models: are configured -- specifically
// -relay-github-token-file/-relay-github-token-key/-relay-codex-auth-file,
// since a routes:-mode daemon never resolves either legacy credential
// at startup at all (its own routes:-mode branch resolves every
// route's own credential fresh, per submitted run, instead).
func TestDaemonMainRoutesModeRefusesLegacyCredentialFlags(t *testing.T) {
	dp := newTestDeps(t)
	path := isolateSessionConfig(t)
	writeSessionConfig(t, path, validRoutesModeSessionConfig)
	err := daemonMain(dp, []string{"-data-dir", t.TempDir(), "-relay-github-token-file", "/does/not/matter"})
	if err == nil {
		t.Fatal("daemonMain with -relay-github-token-file and a routes: config: want error, got nil")
	}
	if !strings.Contains(err.Error(), "relay-github-token-file") {
		t.Fatalf("error = %q, want it to name relay-github-token-file", err.Error())
	}
}

// TestDaemonMainRoutesModeSkipsLegacyGitHubCopilotTokenResolution proves
// a routes:-mode daemon with a github-copilot route configured, and NO
// GITHUB_COPILOT_TOKEN/discovered auth file/-relay-github-token-file at
// all, still gets past its own startup credential resolution (the
// legacy resolveGitHubCopilotToken call this daemon's own routes:-mode
// branch now skips entirely) to its next requirement, the required
// -temporal-address/-repository check -- exactly like
// TestDaemonMainNoLongerRefusesValidRoutesModeConfig already proves for
// the non-copilot fixture.
func TestDaemonMainRoutesModeSkipsLegacyGitHubCopilotTokenResolution(t *testing.T) {
	dp := newTestDeps(t)
	t.Setenv("GITHUB_COPILOT_TOKEN", "")
	path := isolateSessionConfig(t)
	writeSessionConfig(t, path, `routes:
  copilot:
    credential_mode: github-copilot
models:
  luna:
    id: gpt-5.6-luna
    routes: [copilot]
roles:
  execution:
    model: luna
`)
	err := daemonMain(dp, []string{"-data-dir", t.TempDir()})
	if err == nil {
		t.Fatal("daemonMain with a copilot routes: config and no -temporal-address/-repository: want error, got nil")
	}
	if strings.Contains(err.Error(), "GitHub OAuth token") {
		t.Errorf("err = %v, must never resolve the legacy GitHub Copilot token at startup in routes: mode", err)
	}
	if !strings.Contains(err.Error(), "-temporal-address and -repository are required") {
		t.Fatalf("error = %q, want the required-flag error (proving routes: mode got past credential resolution)", err.Error())
	}
}

// TestValidateAndWarnRolesAcceptsValidRoutesModeConfig is serve's own
// start-site check (validateRoles, called once by serveMain -- folded
// from a former validateAndWarnRoles wrapper that had no behavior of its
// own once ValidateRouting's unwaived-warning case never materialized)
// exercised directly rather than through serveMain itself -- serveMain
// blocks serving requests until shut down, which is exercised by this
// package's own dedicated serve lifecycle tests, not this file.
func TestValidateAndWarnRolesAcceptsValidRoutesModeConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	if err := os.WriteFile(path, []byte(validRoutesModeSessionConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := sessionconfig.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	settings, err := cfg.ApplySettings(sessionconfig.DefaultSettings())
	if err != nil {
		t.Fatalf("ApplySettings: %v", err)
	}
	if err := validateRoles(settings); err != nil {
		t.Fatalf("validateRoles: %v, want nil for a schema-valid routes: config", err)
	}
}
