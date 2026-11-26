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
	writeTestSubmitRepo(t, workspace, `verify_command: "make ci-verify"
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
