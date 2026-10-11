package modelrole

import (
	"strings"
	"testing"

	"buildgate/internal/sessionconfig"
)

// escalationSettings is routesModeSettings (luna at medium) with a second
// allowed model, "sol", on its own route as roles.execution.escalation at
// high.
func escalationSettings() sessionconfig.Settings {
	s := routesModeSettings()
	s.Routes["strong"] = sessionconfig.Route{CredentialMode: "static", Upstream: "https://strong.example.invalid", CredentialEnv: "STRONG_KEY"}
	s.Models["sol"] = sessionconfig.Model{ID: "gpt-5.6-sol", Routes: []string{"strong"}}
	s.Roles.Execution.Allowed = []string{"luna", "sol"}
	s.Roles.Execution.Escalation = &sessionconfig.Escalation{Model: "sol", Thinking: "high"}
	return s
}

// TestCheckRouteBindingBindsTheEscalationsLevelToItsModel: a Worker accepts
// the escalation's model at the escalation's level or the role's, the role's
// model at the role's level only, and no level its config does not name.
func TestCheckRouteBindingBindsTheEscalationsLevelToItsModel(t *testing.T) {
	s := escalationSettings()
	onEscalation, err := SelectRoute(s, RoleExecution, "sol", "", "", alwaysOK)
	if err != nil {
		t.Fatalf("SelectRoute(sol): %v", err)
	}
	onRole, err := SelectRoute(s, RoleExecution, "", "", "", alwaysOK)
	if err != nil {
		t.Fatalf("SelectRoute: %v", err)
	}
	for _, level := range []string{"high", "medium"} {
		if err := CheckRouteBinding(s, RoleExecution, onEscalation.Policy, level); err != nil {
			t.Errorf("escalation's model at %s: %v, want accepted", level, err)
		}
	}
	if err := CheckRouteBinding(s, RoleExecution, onRole.Policy, "medium"); err != nil {
		t.Errorf("role's model at the role's level: %v, want accepted", err)
	}
	for name, tc := range map[string]struct {
		selection Selection
		level     string
	}{
		"the escalation's level on the role's model": {onRole, "high"},
		"a level the config does not name":           {onEscalation, "max"},
		"no level on the escalation's model":         {onEscalation, ""},
	} {
		if err := CheckRouteBinding(s, RoleExecution, tc.selection.Policy, tc.level); err == nil || !strings.Contains(err.Error(), "thinking") {
			t.Errorf("%s: err = %v, want the level refused", name, err)
		}
	}

	// Without an escalation only the role's level binds, on either model.
	s.Roles.Execution.Escalation = nil
	if err := CheckRouteBinding(s, RoleExecution, onEscalation.Policy, "medium"); err != nil {
		t.Errorf("no escalation, an allowed model at the role's level: %v, want accepted", err)
	}
	if err := CheckRouteBinding(s, RoleExecution, onEscalation.Policy, "high"); err == nil {
		t.Error("a Worker with no escalation accepted a level that is not the role's")
	}
}

// TestCheckRouteBindingEscalationSharingTheRolesBackend: the escalation is the
// role's own model under a second models: name, at another level. The policy
// is the same for both names, and each level binds.
func TestCheckRouteBindingEscalationSharingTheRolesBackend(t *testing.T) {
	s := routesModeSettings()
	s.Models["luna-again"] = s.Models["luna"]
	s.Roles.Execution.Allowed = []string{"luna", "luna-again"}
	s.Roles.Execution.Escalation = &sessionconfig.Escalation{Model: "luna-again", Thinking: "high"}
	sel, err := SelectRoute(s, RoleExecution, "luna-again", "", "", alwaysOK)
	if err != nil {
		t.Fatalf("SelectRoute: %v", err)
	}
	for _, level := range []string{"medium", "high"} {
		if err := CheckRouteBinding(s, RoleExecution, sel.Policy, level); err != nil {
			t.Errorf("level %s: %v, want accepted", level, err)
		}
	}
	if err := CheckRouteBinding(s, RoleExecution, sel.Policy, "low"); err == nil {
		t.Error("level low accepted; the config names it for neither model")
	}
}
