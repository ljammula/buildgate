package sessionconfig

import (
	"strings"
	"testing"
)

// routesModeSettingsWithPlanningAndAllowed extends routesModeSettings
// (routing_test.go's own minimal execution-only fixture) with a
// roles.planning role and a roles.execution.allowed list naming a second
// model, "sonnet" -- the fixture every ValidateRequestModels test below
// selects within.
func routesModeSettingsWithPlanningAndAllowed() Settings {
	s := routesModeSettings()
	s.Models["sonnet"] = Model{ID: "sonnet-4", Routes: []string{"litellm"}}
	s.Roles.Execution.Allowed = []string{"luna", "sonnet"}
	s.Roles.Planning = &RoleConfig{Model: "luna", Allowed: []string{"luna", "sonnet"}}
	return s
}

func TestValidateRequestModelsEmptyIsOK(t *testing.T) {
	if err := ValidateRequestModels(routesModeSettingsWithPlanningAndAllowed(), nil); err != nil {
		t.Fatalf("ValidateRequestModels(nil): %v, want nil", err)
	}
	if err := ValidateRequestModels(routesModeSettingsWithPlanningAndAllowed(), map[string]string{}); err != nil {
		t.Fatalf("ValidateRequestModels(empty): %v, want nil", err)
	}
}

func TestValidateRequestModelsAcceptsAllowedChoicePerRole(t *testing.T) {
	s := routesModeSettingsWithPlanningAndAllowed()
	if err := ValidateRequestModels(s, map[string]string{"execution": "sonnet"}); err != nil {
		t.Errorf("execution=sonnet: %v, want nil (sonnet is in roles.execution.allowed)", err)
	}
	if err := ValidateRequestModels(s, map[string]string{"planning": "sonnet"}); err != nil {
		t.Errorf("planning=sonnet: %v, want nil (sonnet is in roles.planning.allowed)", err)
	}
	if err := ValidateRequestModels(s, map[string]string{"planning": "luna", "execution": "sonnet"}); err != nil {
		t.Errorf("both roles: %v, want nil", err)
	}
}

func TestValidateRequestModelsRejectsModelOutsideAllowed(t *testing.T) {
	s := routesModeSettingsWithPlanningAndAllowed()
	err := ValidateRequestModels(s, map[string]string{"execution": "opus"})
	if err == nil {
		t.Fatal("ValidateRequestModels: err = nil, want a refusal for a model outside roles.execution.allowed")
	}
	if got := err.Error(); !strings.Contains(got, `"opus"`) || !strings.Contains(got, "roles.execution.allowed") {
		t.Errorf("err = %v, want it to name the rejected model and roles.execution.allowed", got)
	}
}

func TestValidateRequestModelsRejectsReviewRole(t *testing.T) {
	s := routesModeSettingsWithPlanningAndAllowed()
	s.Roles.Review = &RoleConfig{Model: "luna"}
	err := ValidateRequestModels(s, map[string]string{"review": "luna"})
	if err == nil {
		t.Fatal("ValidateRequestModels: err = nil, want a refusal -- review is never requester-selectable")
	}
	if got := err.Error(); !strings.Contains(got, `"review"`) {
		t.Errorf("err = %v, want it to name the rejected role", got)
	}
}

func TestValidateRequestModelsRejectsUnknownRole(t *testing.T) {
	s := routesModeSettingsWithPlanningAndAllowed()
	err := ValidateRequestModels(s, map[string]string{"build": "luna"})
	if err == nil {
		t.Fatal("ValidateRequestModels: err = nil, want a refusal for an unrecognized role key")
	}
}

func TestValidateRequestModelsRejectsUnconfiguredRole(t *testing.T) {
	s := routesModeSettingsWithPlanningAndAllowed()
	s.Roles.Planning = nil
	err := ValidateRequestModels(s, map[string]string{"planning": "luna"})
	if err == nil {
		t.Fatal("ValidateRequestModels: err = nil, want a refusal when roles.planning is not configured at all")
	}
}

func TestValidateRequestModelsRejectsInLegacyMode(t *testing.T) {
	err := ValidateRequestModels(Settings{}, map[string]string{"execution": "sonnet"})
	if err == nil {
		t.Fatal("ValidateRequestModels: err = nil, want a refusal in legacy (no routes:/models:/roles:) mode")
	}
}
