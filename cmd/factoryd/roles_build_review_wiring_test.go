package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"buildgate/internal/run"
	"buildgate/internal/sessionconfig"
)

// TestIntegrationExecutionRoleThinkingReachesBuildAppArgv proves
// roles.execution's Pi thinking level actually reaches build_app.py's own
// argv (the build argv's --thinking), end to end through a real `factoryd`
// subprocess -- not just buildActivityArgs' unit test (internal/workflow),
// which can't catch a wiring gap between run_ticket.go's role resolution
// and the call site itself. isolateSessionConfig replaces the whole
// package-wide default session config for this one subprocess (see
// TestMain's own comment on why that's the documented way to win), so it
// must re-declare sandbox_docker itself -- the package-wide fake_docker.sh
// wiring -- alongside the routes:/models:/roles: block under test. The
// fixture's explicit -build-app-script still gets the role's route because
// roles.execution names a model (modelRouteNeeded).
func TestIntegrationExecutionRoleThinkingReachesBuildAppArgv(t *testing.T) {
	ws := newFixtureRepo(t)
	fakeDockerPath, err := filepath.Abs("testdata/fake_docker.sh")
	if err != nil {
		t.Fatalf("resolve fake docker path: %v", err)
	}
	path := isolateSessionConfig(t)
	t.Setenv("INTEGRATION_EXECUTION_THINKING_TEST_KEY", "sk-test")
	writeSessionConfig(t, path, "sandbox_docker: "+fakeDockerPath+"\n"+
		"routes:\n  litellm:\n    credential_mode: static\n    upstream: https://litellm.example.invalid\n    credential_env: INTEGRATION_EXECUTION_THINKING_TEST_KEY\n"+
		"models:\n  luna:\n    id: gpt-9000\n    api: openai-completions\n    routes: [litellm]\n    reasoning: true\n"+
		"roles:\n  execution:\n    model: luna\n    thinking: medium\n")

	// mode "commit" plus State assertions are deliberately not used here:
	// testdata/fake_build_app.sh doesn't recognize --thinking (a fixture
	// shared by many other tests in this package, not touched by this PR)
	// and exits nonzero on any unrecognized arg, so this run always
	// quarantines -- irrelevant to what this test actually checks, the
	// recorded build attempt's own argv.
	r := runFactorydWithSpecAndFlags(t, ws, "commit", "true", "# Ticket: fixture\n", "5m0s", nil, nil)
	var buildAttempt *run.Attempt
	for i := range r.Attempts {
		if r.Attempts[i].Kind == "build" {
			buildAttempt = &r.Attempts[i]
			break
		}
	}
	if buildAttempt == nil {
		t.Fatal("no build attempt recorded")
	}
	if !hasArgPair(buildAttempt.Command, "--thinking", "medium") {
		t.Errorf("build attempt argv = %v, want --thinking medium (roles.execution's own resolved level)", buildAttempt.Command)
	}
}

// TestRunMainWithReadyUsesConfigFlag proves -config is actually loaded
// instead of the default session-config search path: isolateSessionConfig
// points the default path at an empty HOME with no roles: block at all
// (so it would resolve settings with no error), while the -config path
// names a distinct roles.execution.model that is not a models: entry.
// Only if run_ticket.go loaded the -config file itself (rather than
// resolveSettings' own default-path fallback) does the run fail on this
// specific, literal model name.
func TestRunMainWithReadyUsesConfigFlag(t *testing.T) {
	dp := newTestDeps(t)
	isolateSessionConfig(t)
	configPath := filepath.Join(t.TempDir(), "custom-config.yml")
	writeSessionConfig(t, configPath, "roles:\n  execution:\n    model: distinctive-alias-name-xyz\n")

	err := runMainWithReady(dp, context.Background(), []string{
		"-ticket", "t", "-workspace", "/does/not/exist", "-spec", "/does/not/exist",
		"-config", configPath,
	}, nil)
	if err == nil {
		t.Fatal("runMainWithReady with -config naming an unresolvable roles.execution model = nil, want an error")
	}
	if !strings.Contains(err.Error(), `model "distinctive-alias-name-xyz" is not a models: entry`) {
		t.Fatalf("runMainWithReady error = %v, want it to name the -config file's own roles.execution.model", err)
	}
	if tier2SettingsOverride != nil {
		t.Fatal("tier2SettingsOverride left set after runMainWithReady returned; -config's override must be cleared on return")
	}
}

// TestRunMainWithReadyConfigNonexistentPathErrors proves a -config path
// that does not exist is a hard error (sessionconfig.Load's own ENOENT),
// not a silent fall-back to the default session-config search the way
// configPath == "" behaves.
func TestRunMainWithReadyConfigNonexistentPathErrors(t *testing.T) {
	dp := newTestDeps(t)
	isolateSessionConfig(t)

	err := runMainWithReady(dp, context.Background(), []string{
		"-ticket", "t", "-workspace", "/does/not/exist", "-spec", "/does/not/exist",
		"-config", filepath.Join(t.TempDir(), "does-not-exist.yml"),
	}, nil)
	if err == nil {
		t.Fatal("runMainWithReady with a nonexistent -config path = nil, want an error")
	}
	if tier2SettingsOverride != nil {
		t.Fatal("tier2SettingsOverride left set after a failed -config load")
	}
}

// TestRunMainWithReadyConfigRefusedWithCallerOverride proves -config is
// refused, rather than silently winning or silently losing, when a
// caller (worker's in-process drain loop, serve's API-started runs)
// already installed its own tier2SettingsOverride before invoking
// runMainWithReady -- our own callers never pass -config themselves, so
// this only ever catches a direct misuse.
func TestRunMainWithReadyConfigRefusedWithCallerOverride(t *testing.T) {
	dp := newTestDeps(t)
	isolateSessionConfig(t)
	configPath := filepath.Join(t.TempDir(), "custom-config.yml")
	writeSessionConfig(t, configPath, "")

	callerSettings := sessionconfig.DefaultSettings()
	tier2SettingsOverride = &callerSettings
	t.Cleanup(func() { tier2SettingsOverride = nil })

	err := runMainWithReady(dp, context.Background(), []string{
		"-ticket", "t", "-workspace", "/does/not/exist", "-spec", "/does/not/exist",
		"-config", configPath,
	}, nil)
	if err == nil || !strings.Contains(err.Error(), "-config cannot be combined with a caller-supplied session config") {
		t.Fatalf("runMainWithReady with -config and a caller-supplied override: error = %v, want the refusal", err)
	}
}

// hasArgPair reports whether args contains flag immediately followed by
// value.
func hasArgPair(args []string, flag, value string) bool {
	for i, a := range args {
		if a == flag && i+1 < len(args) && args[i+1] == value {
			return true
		}
	}
	return false
}
