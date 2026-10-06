package main

import (
	"bytes"
	"strings"
	"testing"

	"buildgate/internal/sessionconfig"
)

// TestDoctorCheckRolesResolveOKWhenAbsent is the migration guarantee at
// the doctor layer: a session config with no roles:/models:/routes: key
// at all must pass this check cleanly (an offline build needs no relay).
func TestDoctorCheckRolesResolveOKWhenAbsent(t *testing.T) {
	c := doctorCheckRolesResolve(doctorInputs{})
	if c.Err != nil {
		t.Errorf("Err = %v, want nil when routes:/models:/roles: are absent", c.Err)
	}
}

// TestDoctorCheckRolesResolveFailsOnUnknownAlias proves an invalid
// roles: block is a hard FAIL (not Advisory), naming the offending role.
func TestDoctorCheckRolesResolveFailsOnUnknownAlias(t *testing.T) {
	roles := &sessionconfig.Roles{Execution: &sessionconfig.RoleConfig{Model: "does-not-exist"}}
	c := doctorCheckRolesResolve(doctorInputs{settings: sessionconfig.Settings{Roles: roles}})
	if c.Err == nil {
		t.Fatal("Err = nil, want a diagnosis for an unknown models: entry")
	}
	if c.Advisory {
		t.Error("Advisory = true, want false: an invalid role must fail worker's shared preflight")
	}
	if !strings.Contains(c.Err.Error(), "roles.execution") {
		t.Errorf("Err = %v, want it to name roles.execution", c.Err)
	}
}

// TestDoctorCheckRolesResolveFailsOnSharedModelWithoutWaiver proves the
// review/execution independence rule is a hard FAIL, never Advisory --
// ValidateRouting has no unwaived-warning case: a reviewer that shares
// the builder's own model must be refused, not merely noted.
func TestDoctorCheckRolesResolveFailsOnSharedModelWithoutWaiver(t *testing.T) {
	settings := routesModeDoctorSettings()
	c := doctorCheckRolesResolve(doctorInputs{settings: settings})
	if c.Err == nil {
		t.Fatal("Err = nil, want a refusal for roles.review sharing roles.execution's own model")
	}
	if c.Advisory {
		t.Error("Advisory = true, want false: a shared review/execution model is refused, not merely warned about")
	}
}

// TestDoctorCheckRolesResolvePassesOnSharedModelWithWaiver is the waived
// counterpart: allow_shared_model: true lets the same config through
// cleanly.
func TestDoctorCheckRolesResolvePassesOnSharedModelWithWaiver(t *testing.T) {
	settings := routesModeDoctorSettings()
	settings.Roles.Review.AllowSharedModel = true
	c := doctorCheckRolesResolve(doctorInputs{settings: settings})
	if c.Err != nil {
		t.Errorf("Err = %v, want nil once allow_shared_model waives the independence rule", c.Err)
	}
}

// routesModeDoctorSettings returns a minimal, LAUNCHABLE routes:/models:/
// roles: config where roles.review resolves to the exact same model as
// roles.execution (both "luna") -- the starting point for the
// independence-rule tests above. Built on sessionconfig.DefaultSettings
// (budgets/ceilings), not a bare Settings{}
// literal, so it satisfies modelrole.ValidateAllowedPolicies too (see
// doctorCheckRolesResolve's own doc comment), not just ValidateRouting's
// schema checks.
func routesModeDoctorSettings() sessionconfig.Settings {
	s := sessionconfig.DefaultSettings()
	s.Routes = map[string]sessionconfig.Route{"codex": {CredentialMode: "chatgpt-codex"}}
	s.Models = map[string]sessionconfig.Model{
		"luna": {ID: "gpt-5.6-luna", Routes: []string{"codex"}},
	}
	s.Roles = &sessionconfig.Roles{
		Execution: &sessionconfig.RoleConfig{Model: "luna"},
		Review:    &sessionconfig.RoleConfig{Model: "luna"},
	}
	return s
}

// TestDoctorCheckRolesResolveValidatesRoutesModePerRoute proves the
// "roles resolve" check validates through sessionconfig.ValidateRouting,
// so a role naming a models: entry whose own routes: list names a route
// that doesn't exist in routes: is caught here, at `factoryd doctor`
// time, exactly as it would be at any real process start.
func TestDoctorCheckRolesResolveValidatesRoutesModePerRoute(t *testing.T) {
	settings := sessionconfig.Settings{
		Routes: map[string]sessionconfig.Route{"codex": {CredentialMode: "chatgpt-codex"}},
		Models: map[string]sessionconfig.Model{
			"luna": {ID: "gpt-5.6-luna", Routes: []string{"does-not-exist"}},
		},
		Roles: &sessionconfig.Roles{Execution: &sessionconfig.RoleConfig{Model: "luna"}},
	}
	c := doctorCheckRolesResolve(doctorInputs{settings: settings})
	if c.Err == nil {
		t.Fatal("Err = nil, want a diagnosis for models.luna naming a route routes: does not declare")
	}
	if c.Advisory {
		t.Error("Advisory = true, want false: an invalid routes: mode role must fail worker's shared preflight")
	}
	if !strings.Contains(c.Err.Error(), "does-not-exist") {
		t.Errorf("Err = %v, want it to name the unknown route", c.Err)
	}
}

// TestDoctorCheckRolesResolveAdvisesValidRoutesModeConfig is the positive
// schema-validity counterpart: a well-formed routes:/models:/roles:
// config is a plain OK here.
func TestDoctorCheckRolesResolveAdvisesValidRoutesModeConfig(t *testing.T) {
	settings := sessionconfig.DefaultSettings()
	settings.Routes = map[string]sessionconfig.Route{"codex": {CredentialMode: "chatgpt-codex"}}
	settings.Models = map[string]sessionconfig.Model{
		"luna": {ID: "gpt-5.6-luna", Routes: []string{"codex"}},
	}
	settings.Roles = &sessionconfig.Roles{Execution: &sessionconfig.RoleConfig{Model: "luna"}}
	c := doctorCheckRolesResolve(doctorInputs{settings: settings})
	if c.Advisory {
		t.Error("Advisory = true, want false: a schema-valid routes: config is usable now")
	}
	if c.Err != nil {
		t.Errorf("Err = %v, want nil for a schema-valid routes: config", c.Err)
	}
}

// TestDoctorCheckRolesResolveFailsOnUnlaunchableAllowedModel is the live
// M3 walk case (2026-09-28): roles.execution.allowed named a model
// ("qwen-path") whose id was a local filesystem path containing a slash.
// `factoryd doctor` reported this "roles resolve" check OK (16/16 checks
// passed) because it only ran ValidateRouting's schema checks, never
// actually built a RoutePolicy for the allowed model and validated it --
// this proves doctor now catches it here, before a human ever drafts and
// approves a spec/plan against a model that can't launch.
func TestDoctorCheckRolesResolveFailsOnUnlaunchableAllowedModel(t *testing.T) {
	settings := routesModeDoctorSettings()
	settings.Roles.Review = nil
	settings.Models["luna"] = sessionconfig.Model{ID: "/Users/operator/code/ai-stack/models/Qwen3.8-27B-MTPLX-Optimized-Quality", Routes: []string{"codex"}}
	c := doctorCheckRolesResolve(doctorInputs{settings: settings})
	if c.Err == nil {
		t.Fatal("Err = nil, want a refusal for an allowed model whose id can never produce a launchable relay policy")
	}
	if c.Advisory {
		t.Error("Advisory = true, want false: an unlaunchable allowed model must fail worker's shared preflight")
	}
	if !strings.Contains(c.Err.Error(), "must not contain a slash") {
		t.Errorf("Err = %v, want it to name RoutePolicy.Validate's own slash refusal", c.Err)
	}
}

// TestDoctorChecksForIncludesRolesResolveCheck proves the check is wired
// into the shared doctorChecksFor list, not just a standalone function
// nothing calls.
func TestDoctorChecksForIncludesRolesResolveCheck(t *testing.T) {
	in := doctorInputs{sandboxImage: "example/image@sha256:" + strings.Repeat("a", 64)}
	checks := doctorChecksFor(t.Context(), in)
	found := false
	for _, c := range checks {
		if c.Name == "roles resolve" {
			found = true
			if c.Err != nil {
				t.Errorf("Err = %v, want nil for the zero-value doctorInputs (no roles configured)", c.Err)
			}
		}
	}
	if !found {
		t.Fatal("doctorChecksFor did not include the roles resolve check")
	}
}

// TestDoctorRunChecksPrintsFullTableWithRolesFailRow is the end-to-end
// regression test for the doctor-abort finding: an invalid roles: block
// must surface as a FAIL row for "roles resolve" while runDoctorChecks
// still prints every other check's own ok/FAIL line, exactly as it would
// for any other independent check failure -- doctor must never abort
// silently before printing the table at all.
func TestDoctorRunChecksPrintsFullTableWithRolesFailRow(t *testing.T) {
	in := doctorInputs{
		sandboxImage: "example/image@sha256:" + strings.Repeat("a", 64),
		settings: sessionconfig.Settings{
			Routes: map[string]sessionconfig.Route{"codex": {CredentialMode: "chatgpt-codex"}},
			Models: map[string]sessionconfig.Model{"luna": {ID: "gpt-5.6-luna", Routes: []string{"codex"}}},
			Roles:  &sessionconfig.Roles{Execution: &sessionconfig.RoleConfig{Model: "does-not-exist"}},
		},
	}
	checks := doctorChecksFor(t.Context(), in)
	var out bytes.Buffer
	failed := runDoctorChecks(checks, &out)
	if failed == 0 {
		t.Fatal("runDoctorChecks: failed = 0, want at least the roles resolve failure counted")
	}
	printed := out.String()
	if !strings.Contains(printed, "FAIL  roles resolve:") {
		t.Errorf("output = %q, want a FAIL line for roles resolve", printed)
	}
	// A check unrelated to roles: (release policy always runs) must still
	// have printed its own line -- proof the table was not cut short.
	if !strings.Contains(printed, "release policy allows a PR") {
		t.Errorf("output = %q, want the full check table printed alongside the roles FAIL row, not just it", printed)
	}
}
