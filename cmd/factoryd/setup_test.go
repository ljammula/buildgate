package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"buildgate/internal/harness"
	"buildgate/internal/sessionconfig"
)

// setupFixture is a machine as `make install` leaves it: an images-only
// session config, a ChatGPT login in codex's auth.json, nothing else under
// HOME for a route to be detected from.
func setupFixture(t *testing.T) (dp *deps, configPath string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("ANTHROPIC_API_KEY", "")
	codexHome := filepath.Join(home, ".codex")
	if err := os.MkdirAll(codexHome, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_HOME", codexHome)
	writeCodexAuthFile(t, codexHome, map[string]any{
		"auth_mode": "chatgpt",
		"tokens": map[string]any{
			"access_token": fakeCodexJWT(t, time.Now().Add(72*time.Hour)),
			"account_id":   "acct-123",
		},
	})
	configPath = filepath.Join(home, ".config", "factoryd", "config.yml")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
		t.Fatal(err)
	}
	imagesOnly := "sandbox_image: localhost:5050/buildgate-worker@sha256:" + strings.Repeat("a", 64) + "\n" +
		"meter_image: localhost:5050/factoryd-meter@sha256:" + strings.Repeat("b", 64) + "\n"
	if err := os.WriteFile(configPath, []byte(imagesOnly), 0o600); err != nil {
		t.Fatal(err)
	}
	dp = newTestDeps(t)
	fakeForgeOf(dp).userLoginFn = func() (string, error) { return "operator", nil }
	return dp, configPath
}

func setupExecutionRole(t *testing.T, configPath string) *sessionconfig.RoleConfig {
	t.Helper()
	cfg, err := sessionconfig.Load(configPath)
	if err != nil {
		t.Fatalf("load written config: %v", err)
	}
	if cfg.Roles == nil || cfg.Roles.Execution == nil {
		t.Fatalf("written config has no execution role")
	}
	if cfg.SandboxImage == nil || cfg.MeterImage == nil {
		t.Errorf("setup dropped the images make install recorded")
	}
	return cfg.Roles.Execution
}

// TestSetupAsksTheModelAndTheCodingAgentAndNothingElse: the two questions,
// in order, on a ChatGPT login: which model route, its model id (Enter keeps
// the default), which coding agent.
func TestSetupAsksTheModelAndTheCodingAgentAndNothingElse(t *testing.T) {
	dp, configPath := setupFixture(t)
	var out bytes.Buffer
	if err := runSetup(dp, &quickstartOptions{ConfigPath: configPath, Harness: harness.Pi}, strings.NewReader("1\n\n2\n"), &out); err != nil {
		t.Fatalf("setup: %v\n%s", err, out.String())
	}
	role := setupExecutionRole(t, configPath)
	if role.Model != chatGPTCodexDefaultModelID || role.Harness != harness.Codex {
		t.Errorf("execution role = model %q harness %q, want %q and codex", role.Model, role.Harness, chatGPTCodexDefaultModelID)
	}
	for _, want := range []string{"How should the daemon reach a model?", "Which coding agent should do the work?", "Set up: model " + chatGPTCodexDefaultModelID + ", coding agent codex"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
	if n := strings.Count(out.String(), "?"); n != 2 {
		t.Errorf("setup asked %d questions, want 2:\n%s", n, out.String())
	}
}

func TestSetupDefaultCodingAgentIsPi(t *testing.T) {
	dp, configPath := setupFixture(t)
	var out bytes.Buffer
	if err := runSetup(dp, &quickstartOptions{ConfigPath: configPath, Harness: harness.Pi}, strings.NewReader("1\n\n1\n"), &out); err != nil {
		t.Fatalf("setup: %v\n%s", err, out.String())
	}
	if role := setupExecutionRole(t, configPath); role.Harness != "" {
		t.Errorf("execution harness = %q, want the pi default left unwritten", role.Harness)
	}
}

// TestSetupAsksNothingAFlagAnswers: -route and -harness given, only the
// model id is left, and with no terminal not even that.
func TestSetupAsksNothingAFlagAnswers(t *testing.T) {
	t.Run("flags", func(t *testing.T) {
		dp, configPath := setupFixture(t)
		var out bytes.Buffer
		opts := &quickstartOptions{ConfigPath: configPath, Route: "chatgpt-codex", Harness: harness.Codex, HarnessExplicit: true}
		if err := runSetup(dp, opts, strings.NewReader("\n"), &out); err != nil {
			t.Fatalf("setup: %v\n%s", err, out.String())
		}
		if strings.Contains(out.String(), "?") {
			t.Errorf("setup asked a question its flags answered:\n%s", out.String())
		}
		if role := setupExecutionRole(t, configPath); role.Harness != harness.Codex {
			t.Errorf("execution harness = %q, want codex", role.Harness)
		}
	})
	t.Run("no terminal", func(t *testing.T) {
		dp, configPath := setupFixture(t)
		var out bytes.Buffer
		if err := runSetup(dp, &quickstartOptions{ConfigPath: configPath, NonInteractive: true, Harness: harness.Pi}, strings.NewReader(""), &out); err != nil {
			t.Fatalf("setup: %v\n%s", err, out.String())
		}
		if strings.Contains(out.String(), "?") {
			t.Errorf("setup asked a question with no terminal:\n%s", out.String())
		}
		if role := setupExecutionRole(t, configPath); role.Model != chatGPTCodexDefaultModelID {
			t.Errorf("execution model = %q, want the detected login's default", role.Model)
		}
	})
}

func TestSetupLeavesAConfiguredMachineAlone(t *testing.T) {
	dp, configPath := setupFixture(t)
	first := &quickstartOptions{ConfigPath: configPath, NonInteractive: true, Harness: harness.Pi}
	if err := runSetup(dp, first, strings.NewReader(""), &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runSetup(dp, &quickstartOptions{ConfigPath: configPath, Harness: harness.Pi}, strings.NewReader(""), &out); err != nil {
		t.Fatalf("second setup: %v", err)
	}
	after, _ := os.ReadFile(configPath)
	if !bytes.Equal(before, after) {
		t.Errorf("setup rewrote a configured machine's config")
	}
	if !strings.Contains(out.String(), "Already set up") || !strings.Contains(out.String(), "-reconfigure") {
		t.Errorf("output = %q, want it to say it is set up and how to choose again", out.String())
	}
}
