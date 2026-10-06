package modelrole

import (
	"strings"
	"testing"

	"buildgate/internal/sessionconfig"
)

// TestValidateAllowedPoliciesAcceptsAValidMultiModelAllowedList is the
// positive case: a routes: mode config with several allowed models, all
// of them launchable, passes cleanly.
func TestValidateAllowedPoliciesAcceptsAValidMultiModelAllowedList(t *testing.T) {
	s := routesModeSettingsWithAllowedSonnet()
	if err := ValidateAllowedPolicies(s); err != nil {
		t.Fatalf("ValidateAllowedPolicies: %v, want nil for a config whose every allowed model is launchable", err)
	}
}

// TestValidateAllowedPoliciesOKWhenRoutesAbsent mirrors
// sessionconfig.ValidateRouting's own "no routes:/models:/roles: at all"
// exemption (an offline build with an explicit -build-app-script needs
// no relay).
func TestValidateAllowedPoliciesOKWhenRoutesAbsent(t *testing.T) {
	if err := ValidateAllowedPolicies(sessionconfig.Settings{}); err != nil {
		t.Fatalf("ValidateAllowedPolicies: %v, want nil when routes:/models:/roles: are all absent", err)
	}
}

// TestValidateAllowedPoliciesRejectsSlashModelIDInAllowed is the live M3
// walk case (2026-09-28): roles.execution.allowed named a model
// ("qwen-path") whose id was a local filesystem path containing a slash.
// sandbox.RoutePolicy.Validate already rejected that (pi's own CLI hangs
// resolving a slash-containing --model value together with an explicit
// --provider), but nothing checked an `allowed` entry against Validate
// before launch, so config load, `doctor`, and `submit` all accepted it
// and a human drafted and approved a spec/plan against it before the
// real build ever tried to launch and failed. This proves
// ValidateAllowedPolicies now refuses it at config-load time, naming the
// role, the model, the route, and the actual Validate failure.
func TestValidateAllowedPoliciesRejectsSlashModelIDInAllowed(t *testing.T) {
	s := routesModeSettingsWithAllowedSonnet()
	s.Models["sonnet"] = sessionconfig.Model{ID: "/Users/operator/code/ai-stack/models/Qwen3.8-27B-MTPLX-Optimized-Quality", Routes: []string{"fallback2"}}
	err := ValidateAllowedPolicies(s)
	if err == nil {
		t.Fatal("ValidateAllowedPolicies: err = nil, want a refusal for a slash-containing allowed model id")
	}
	for _, want := range []string{"roles.execution.allowed", `"sonnet"`, `"fallback2"`, "must not contain a slash"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v, want it to contain %q", err, want)
		}
	}
}

// TestValidateAllowedPoliciesRejectsRoleDefaultModel proves the check
// also covers a role's own default (roles.<role>.model), not just the
// rest of its allowed list.
func TestValidateAllowedPoliciesRejectsRoleDefaultModel(t *testing.T) {
	s := routesModeSettings()
	s.Models["luna"] = sessionconfig.Model{ID: "bad/id", Routes: []string{"primary", "fallback"}}
	err := ValidateAllowedPolicies(s)
	if err == nil {
		t.Fatal("ValidateAllowedPolicies: err = nil, want a refusal for roles.execution's own default model")
	}
	if !strings.Contains(err.Error(), "must not contain a slash") {
		t.Errorf("err = %v, want it to name the Validate failure", err)
	}
}
