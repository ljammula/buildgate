package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"buildgate/internal/modelrole"
)

// routesModeSessionConfigNoRelayImage is a routes:/models:/roles: block
// whose one route names a credential variable no test sets.
const routesModeSessionConfigNoRelayImage = `routes:
  litellm:
    credential_mode: static
    upstream: https://litellm.example.invalid
    credential_env: RELAY_IMAGE_TEST_KEY
models:
  luna:
    id: gpt-5.6-luna
    routes: [litellm]
roles:
  execution:
    model: luna
`

// routesModeSessionConfigNoRelayImageWithAllowedSonnet is
// routesModeSessionConfigNoRelayImage plus a second execution-role
// model, "sonnet", in roles.execution.allowed -- the fixture the
// -execution-model tests below choose within.
const routesModeSessionConfigNoRelayImageWithAllowedSonnet = `routes:
  litellm:
    credential_mode: static
    upstream: https://litellm.example.invalid
    credential_env: RELAY_IMAGE_TEST_KEY
models:
  luna:
    id: gpt-5.6-luna
    routes: [litellm]
  sonnet:
    id: sonnet-4
    routes: [litellm]
roles:
  execution:
    model: luna
    allowed: [luna, sonnet]
`

// TestRunMainWithReadyExecutionModelSelectsAllowedModel proves
// -execution-model sonnet (a member of roles.execution.allowed) reaches
// modelrole.SelectRoute for the chosen model: with RELAY_IMAGE_TEST_KEY
// deliberately unset, the run still fails, but with
// modelrole.ErrNoRouteAvailable (the route was tried and failed on its
// own missing credential), not a rejection of the choice itself.
func TestRunMainWithReadyExecutionModelSelectsAllowedModel(t *testing.T) {
	dp := newTestDeps(t)
	path := isolateSessionConfig(t)
	writeSessionConfig(t, path, routesModeSessionConfigNoRelayImageWithAllowedSonnet)

	err := runMainWithReady(dp, context.Background(), []string{
		"-ticket", "t", "-workspace", "/does/not/exist", "-spec", "/does/not/exist",
		"-execution-model", "sonnet",
	}, nil)
	if err == nil {
		t.Fatal("runMainWithReady: err = nil, want a refusal (RELAY_IMAGE_TEST_KEY is not set)")
	}
	if !errors.Is(err, modelrole.ErrNoRouteAvailable) {
		t.Fatalf("err = %v, want modelrole.ErrNoRouteAvailable (sonnet was accepted and its own route tried)", err)
	}
}

// TestRunMainWithReadyExecutionModelRefusedInLegacyMode proves
// -execution-model is refused outright with no routes:/models:/roles:
// session config at all -- there is no roles.execution.allowed to
// validate the choice against.
func TestRunMainWithReadyExecutionModelRefusedInLegacyMode(t *testing.T) {
	dp := newTestDeps(t)
	isolateSessionConfig(t)

	err := runMainWithReady(dp, context.Background(), []string{
		"-ticket", "t", "-workspace", "/does/not/exist", "-spec", "/does/not/exist",
		"-execution-model", "sonnet",
	}, nil)
	if err == nil {
		t.Fatal("runMainWithReady: err = nil, want a refusal (-execution-model has no roles.execution.allowed to validate against in legacy mode)")
	}
	if !strings.Contains(err.Error(), "-execution-model") {
		t.Errorf("err = %v, want it to name -execution-model", err)
	}
}

// TestRunMainWithReadyExplicitBuildScriptGetsARouteOnlyWithAnExecutionRole
// proves modelRouteNeeded for an explicit -build-app-script: with no
// roles.execution it is an offline build and no route is selected (the run
// fails later, on its nonexistent workspace); with roles.execution naming a
// model its route is selected, here failing on the unset credential.
func TestRunMainWithReadyExplicitBuildScriptGetsARouteOnlyWithAnExecutionRole(t *testing.T) {
	args := []string{
		"-ticket", "t", "-workspace", "/does/not/exist", "-spec", "/does/not/exist",
		"-build-app-script", "/does/not/exist/offline_build.py",
	}
	t.Run("no execution role", func(t *testing.T) {
		dp := newTestDeps(t)
		writeSessionConfig(t, isolateSessionConfig(t), "")
		err := runMainWithReady(dp, context.Background(), args, nil)
		if err == nil {
			t.Fatal("runMainWithReady: err = nil, want a failure (the workspace/spec paths do not exist)")
		}
		if errors.Is(err, modelrole.ErrNoRouteAvailable) || strings.Contains(err.Error(), "roles.execution") {
			t.Errorf("err = %v, must not resolve roles.execution for an offline -build-app-script", err)
		}
	})
	t.Run("execution role configured", func(t *testing.T) {
		dp := newTestDeps(t)
		writeSessionConfig(t, isolateSessionConfig(t), routesModeSessionConfigNoRelayImage)
		err := runMainWithReady(dp, context.Background(), args, nil)
		if !errors.Is(err, modelrole.ErrNoRouteAvailable) {
			t.Fatalf("err = %v, want modelrole.ErrNoRouteAvailable (the role's route was tried and its credential is unset)", err)
		}
	})
}

// TestRelayLaunchWithoutExecutionRoleFailsClearly proves a launch that
// needs a model route (the default, model-backed build_app.py) with no
// roles.execution at all fails with modelrole's own clear
// "roles.execution.model is not configured" diagnostic -- routes:/models:/
// roles: is the only session-config schema, so there is no other model
// source left to fall back to.
func TestRelayLaunchWithoutExecutionRoleFailsClearly(t *testing.T) {
	dp := newTestDeps(t)
	path := isolateSessionConfig(t)
	writeSessionConfig(t, path, "")
	err := runMainWithReady(dp, context.Background(), []string{
		"-ticket", "t", "-workspace", "/does/not/exist", "-spec", "/does/not/exist",
	}, nil)
	if err == nil || !strings.Contains(err.Error(), "roles.execution.model is not configured") {
		t.Fatalf("runMainWithReady with no roles.execution: err = %v, want the clear roles.execution.model diagnostic", err)
	}
}

// TestRunRejectsRemovedRelayFlag proves a legacy per-route CLI flag
// (-relay-upstream, one of the fourteen deleted alongside routes:/models:/
// roles: becoming the only session-config schema) is an unknown flag on
// `run`'s own flag.FlagSet -- Go's flag package refuses it itself, naming
// it, rather than `run` silently accepting and ignoring it.
func TestRunRejectsRemovedRelayFlag(t *testing.T) {
	dp := newTestDeps(t)
	err := runMainWithReady(dp, context.Background(), []string{
		"-ticket", "t", "-workspace", "/does/not/exist", "-spec", "/does/not/exist",
		"-relay-upstream", "https://example.invalid",
	}, nil)
	if err == nil || !strings.Contains(err.Error(), "flag provided but not defined: -relay-upstream") {
		t.Fatalf("runMainWithReady with the removed -relay-upstream flag: err = %v, want the flag package's own unknown-flag refusal", err)
	}
}
