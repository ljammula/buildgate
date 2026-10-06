package modelrole

import (
	"strings"
	"testing"

	"buildgate/internal/harness"
)

func TestSelectRouteHarnessDefaultsToPi(t *testing.T) {
	sel, err := SelectRoute(routesModeSettings(), RoleExecution, "", "", "", alwaysOK)
	if err != nil {
		t.Fatalf("SelectRoute: %v", err)
	}
	if sel.Harness != "pi" {
		t.Errorf("Harness = %q, want pi for a role that sets none", sel.Harness)
	}
}

func TestSelectRouteHarnessFromRoleAndRequestChoice(t *testing.T) {
	s := routesModeSettings()
	s.Roles.Execution.Harness = "pifork"
	s.Roles.Execution.AllowedHarnesses = []string{"pifork", "pi"}
	sel, err := SelectRoute(s, RoleExecution, "", "", "", alwaysOK)
	if err != nil || sel.Harness != "pifork" {
		t.Fatalf("role default: sel.Harness = %q, err = %v, want pifork", sel.Harness, err)
	}
	sel, err = SelectRoute(s, RoleExecution, "", "PI", "", alwaysOK)
	if err != nil || sel.Harness != "pi" {
		t.Fatalf("request choice: sel.Harness = %q, err = %v, want pi", sel.Harness, err)
	}
}

func TestSelectRouteHarnessChoiceOutsideAllowedErrors(t *testing.T) {
	s := routesModeSettings()
	_, err := SelectRoute(s, RoleExecution, "", "pifork", "", alwaysOK)
	if err == nil || !strings.Contains(err.Error(), "roles.execution.allowed_harnesses") {
		t.Fatalf("SelectRoute = %v, want a refusal naming roles.execution.allowed_harnesses", err)
	}
}

// TestSelectRouteRefusesHarnessThatCannotSpeakTheModelAPI injects a registry
// entry whose wire APIs exclude the model's: every compiled harness speaks all
// three apis today.
func TestSelectRouteRefusesHarnessThatCannotSpeakTheModelAPI(t *testing.T) {
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
	_, err := SelectRoute(s, RoleExecution, "", "", "", alwaysOK)
	if err == nil {
		t.Fatal("SelectRoute accepted a harness that cannot speak the model's api")
	}
	for _, want := range []string{"roles.execution", `"narrow"`, `"luna"`, "openai-completions"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v, missing %q", err, want)
		}
	}
}
