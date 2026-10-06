package sessionconfig

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"buildgate/internal/harness"
)

func TestRoleHarnessDefaultsToPi(t *testing.T) {
	rc := &RoleConfig{Model: "luna"}
	if got := rc.HarnessName(); got != "pi" {
		t.Fatalf("HarnessName() = %q, want pi", got)
	}
	if got := rc.AllowedHarnessNames(); len(got) != 1 || got[0] != "pi" {
		t.Fatalf("AllowedHarnessNames() = %v, want [pi]", got)
	}
}

func TestValidateRoutingRejectsUnknownHarness(t *testing.T) {
	s := routesModeSettings()
	s.Roles.Execution.Harness = "nope"
	err := ValidateRouting(s)
	if err == nil || !strings.Contains(err.Error(), "roles.execution") || !strings.Contains(err.Error(), "codex, copilot, pi, pifork") {
		t.Fatalf("ValidateRouting = %v, want a refusal naming the role and the valid harnesses", err)
	}
	s = routesModeSettings()
	s.Roles.Execution.AllowedHarnesses = []string{"pi", "nope"}
	if err := ValidateRouting(s); err == nil {
		t.Fatal("ValidateRouting accepted an unknown allowed harness")
	}
}

func TestValidateRoutingHarnessMustBeInAllowedHarnesses(t *testing.T) {
	s := routesModeSettings()
	s.Roles.Execution.Harness = "pifork"
	s.Roles.Execution.AllowedHarnesses = []string{"pi"}
	err := ValidateRouting(s)
	if err == nil || !strings.Contains(err.Error(), "allowed_harnesses") {
		t.Fatalf("ValidateRouting = %v, want a refusal naming allowed_harnesses", err)
	}
	s.Roles.Execution.AllowedHarnesses = []string{"pi", "pifork"}
	if err := ValidateRouting(s); err != nil {
		t.Fatalf("ValidateRouting = %v, want nil", err)
	}
}

// TestValidateRoutingRefusesHarnessThatCannotSpeakTheModelAPI injects a
// registry entry whose wire APIs exclude the fixture model's api: the
// compiled harnesses all speak every api today.
func TestValidateRoutingRefusesHarnessThatCannotSpeakTheModelAPI(t *testing.T) {
	prev := harnessLookup
	t.Cleanup(func() { harnessLookup = prev })
	harnessLookup = func(name string) (harness.Descriptor, error) {
		if name == "narrow" {
			return harness.Descriptor{Name: "narrow", WireAPIs: []string{"anthropic-messages"}}, nil
		}
		return prev(name)
	}
	s := routesModeSettings()
	s.Roles.Execution.Harness = "narrow"
	err := ValidateRouting(s)
	if err == nil {
		t.Fatal("ValidateRouting accepted a harness that cannot speak the model's api")
	}
	for _, want := range []string{"roles.execution", `"narrow"`, `"luna"`, "openai-completions"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v, missing %q", err, want)
		}
	}
}

// TestValidateRoutingCodexNeedsAResponsesRoute pins the harness x route check
// for the real registry entries: Codex CLI speaks only the Responses API, so a
// role whose model resolves to completions or anthropic-messages is refused
// with a message naming the role, harness, model and api; Copilot speaks all
// three.
func TestValidateRoutingCodexNeedsAResponsesRoute(t *testing.T) {
	for _, api := range []string{"", "openai-completions", "anthropic-messages"} {
		s := routesModeSettings()
		s.Models["luna"] = Model{ID: "gpt-5.6-luna", Routes: []string{"litellm"}, API: api}
		s.Roles.Execution.Harness = "codex"
		err := ValidateRouting(s)
		if err == nil {
			t.Fatalf("api %q: ValidateRouting accepted codex on a non-Responses route", api)
		}
		for _, want := range []string{"roles.execution", `"codex"`, `"luna"`, "openai-responses"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("api %q: err = %v, missing %q", api, err, want)
			}
		}
	}
	s := routesModeSettings()
	s.Models["luna"] = Model{ID: "gpt-5.6-luna", Routes: []string{"litellm"}, API: "openai-responses"}
	s.Roles.Execution.Harness = "codex"
	if err := ValidateRouting(s); err != nil {
		t.Fatalf("ValidateRouting(codex on a Responses route) = %v, want nil", err)
	}
	for _, api := range []string{"", "openai-responses", "anthropic-messages"} {
		s := routesModeSettings()
		s.Models["luna"] = Model{ID: "gpt-5.6-luna", Routes: []string{"litellm"}, API: api}
		s.Roles.Execution.Harness = "copilot"
		if err := ValidateRouting(s); err != nil {
			t.Errorf("api %q: ValidateRouting(copilot) = %v, want nil", api, err)
		}
	}
}

func TestValidateRequestHarnesses(t *testing.T) {
	s := routesModeSettingsWithPlanningAndAllowed()
	s.Roles.Execution.AllowedHarnesses = []string{"pi", "pifork"}
	s.Roles.Review = &RoleConfig{Model: "luna"}

	if err := ValidateRequestHarnesses(s, nil); err != nil {
		t.Fatalf("empty: %v", err)
	}
	if err := ValidateRequestHarnesses(s, map[string]string{"execution": "pifork"}); err != nil {
		t.Fatalf("execution=pifork: %v, want nil", err)
	}
	if err := ValidateRequestHarnesses(s, map[string]string{"planning": "pi"}); err != nil {
		t.Fatalf("planning=pi: %v, want nil (the default is always allowed)", err)
	}
	for name, tc := range map[string]struct {
		in   map[string]string
		want string
	}{
		"outside allowed": {map[string]string{"planning": "pifork"}, "roles.planning.allowed_harnesses"},
		"review":          {map[string]string{"review": "pi"}, "never requester-selectable"},
		"unknown":         {map[string]string{"execution": "nope"}, "pi, pifork"},
		"unknown role":    {map[string]string{"other": "pi"}, "not selectable"},
	} {
		err := ValidateRequestHarnesses(s, tc.in)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want it to contain %q", name, err, tc.want)
		}
	}
	if err := ValidateRequestHarnesses(Settings{}, map[string]string{"execution": "pi"}); err == nil {
		t.Error("a session with no roles accepted a per-request harness")
	}
}

func TestLoadRefusesTheRetiredEngineKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("engine: pifork\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "engine: moved to roles.<role>.harness") {
		t.Fatalf("Load = %v, want the engine key refused with the roles.<role>.harness fix", err)
	}
}

func TestLoadReadsRoleHarness(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	body := "roles:\n  execution:\n    model: luna\n    harness: pifork\n    allowed_harnesses: [pifork, pi]\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	rc := cfg.Roles.Execution
	if rc.Harness != "pifork" || len(rc.AllowedHarnesses) != 2 {
		t.Fatalf("role = %#v", rc)
	}
}
