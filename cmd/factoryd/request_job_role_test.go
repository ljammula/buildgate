package main

import (
	"os"
	"strings"
	"testing"

	"buildgate/internal/modelrole"
	"buildgate/internal/requestdriver"
	"buildgate/internal/sessionconfig"
)

// routesModeExecutionOnlySettings is a minimal, launchable routes: mode
// Settings with only roles.execution configured -- ValidateRouting only
// requires that much, so a drafting job's own roles.planning/roles.review
// can legitimately be unset (see resolveRequestJobRole's own routes:
// mode fallback).
func routesModeExecutionOnlySettings() sessionconfig.Settings {
	s := sessionconfig.DefaultSettings()
	s.Routes = map[string]sessionconfig.Route{
		"litellm": {CredentialMode: "static", Upstream: "https://litellm.example.invalid", CredentialEnv: "REQUEST_JOB_ROLE_TEST_KEY"},
	}
	s.Models = map[string]sessionconfig.Model{
		"luna": {ID: "gpt-5.6-luna", Routes: []string{"litellm"}},
	}
	s.Roles = &sessionconfig.Roles{Execution: &sessionconfig.RoleConfig{Model: "luna", Thinking: "medium"}}
	return s
}

// TestResolveRequestJobRoleRoutesModeSpecDraftingFallsBackToExecution
// proves a routes: mode config with only roles.execution configured
// still resolves modelrole.StageSpecDrafting (roles.planning) to
// roles.execution's own model/route -- routes: mode has no implicit
// session-default model, so an unset roles.planning must fall back to
// the build's own role rather than refusing the job outright.
func TestResolveRequestJobRoleRoutesModeSpecDraftingFallsBackToExecution(t *testing.T) {
	t.Setenv("REQUEST_JOB_ROLE_TEST_KEY", "sk-test")
	cfg := requestdriver.WorkerConfig{Settings: routesModeExecutionOnlySettings()}
	got, err := resolveRequestJobRole(cfg, modelrole.StageSpecDrafting, "spec drafting", "", "")
	if err != nil {
		t.Fatalf("resolveRequestJobRole: %v", err)
	}
	if got.RouteSelection == nil {
		t.Fatalf("resolveRequestJobRole = %+v, want a RouteSelection", got)
	}
	if got.RouteSelection.ModelName != "luna" || got.RouteSelection.RouteName != "litellm" {
		t.Errorf("RouteSelection = %+v, want roles.execution's own model/route (luna/litellm)", got.RouteSelection)
	}
}

// TestResolveRequestJobRoleRoutesModeOracleDraftingFallsBackToExecution
// is the same proof for modelrole.StageOracleDrafting (roles.review).
func TestResolveRequestJobRoleRoutesModeOracleDraftingFallsBackToExecution(t *testing.T) {
	t.Setenv("REQUEST_JOB_ROLE_TEST_KEY", "sk-test")
	cfg := requestdriver.WorkerConfig{Settings: routesModeExecutionOnlySettings()}
	got, err := resolveRequestJobRole(cfg, modelrole.StageOracleDrafting, "oracle drafting", "", "")
	if err != nil {
		t.Fatalf("resolveRequestJobRole: %v", err)
	}
	if got.RouteSelection == nil {
		t.Fatalf("resolveRequestJobRole = %+v, want a RouteSelection", got)
	}
	if got.RouteSelection.ModelName != "luna" || got.RouteSelection.RouteName != "litellm" {
		t.Errorf("RouteSelection = %+v, want roles.execution's own model/route (luna/litellm)", got.RouteSelection)
	}
}

// TestResolveRequestJobRoleRoutesModeUsesItsOwnRoleWhenConfigured proves
// the fallback only fires for an UNSET role: when roles.planning is
// itself configured, it is used directly, not roles.execution's.
func TestResolveRequestJobRoleRoutesModeUsesItsOwnRoleWhenConfigured(t *testing.T) {
	t.Setenv("REQUEST_JOB_ROLE_TEST_KEY", "sk-test")
	t.Setenv("REQUEST_JOB_ROLE_TEST_KEY2", "sk-test2")
	settings := routesModeExecutionOnlySettings()
	settings.Routes["litellm2"] = sessionconfig.Route{CredentialMode: "static", Upstream: "https://litellm2.example.invalid", CredentialEnv: "REQUEST_JOB_ROLE_TEST_KEY2"}
	settings.Models["sonnet"] = sessionconfig.Model{ID: "sonnet-4", Routes: []string{"litellm2"}}
	settings.Roles.Planning = &sessionconfig.RoleConfig{Model: "sonnet"}

	cfg := requestdriver.WorkerConfig{Settings: settings}
	got, err := resolveRequestJobRole(cfg, modelrole.StageSpecDrafting, "spec drafting", "", "")
	if err != nil {
		t.Fatalf("resolveRequestJobRole: %v", err)
	}
	if got.RouteSelection == nil || got.RouteSelection.ModelName != "sonnet" {
		t.Fatalf("RouteSelection = %+v, want roles.planning's own model (sonnet), not the execution fallback", got.RouteSelection)
	}
}

// TestResolveRequestJobRolePassesPlanningModelChoice proves the
// modelChoice parameter (a request's own r.Models["planning"]) reaches
// modelrole.SelectRoute only when this stage's role actually resolves to
// planning itself: accepted when it names a member of
// roles.planning.allowed, and never even consulted for
// modelrole.StageOracleDrafting (the review role, never
// requester-selectable).
func TestResolveRequestJobRolePassesPlanningModelChoice(t *testing.T) {
	t.Setenv("REQUEST_JOB_ROLE_TEST_KEY", "sk-test")
	t.Setenv("REQUEST_JOB_ROLE_TEST_KEY2", "sk-test2")
	settings := routesModeExecutionOnlySettings()
	settings.Routes["litellm2"] = sessionconfig.Route{CredentialMode: "static", Upstream: "https://litellm2.example.invalid", CredentialEnv: "REQUEST_JOB_ROLE_TEST_KEY2"}
	settings.Models["sonnet"] = sessionconfig.Model{ID: "sonnet-4", Routes: []string{"litellm2"}}
	settings.Roles.Planning = &sessionconfig.RoleConfig{Model: "luna", Allowed: []string{"luna", "sonnet"}}

	cfg := requestdriver.WorkerConfig{Settings: settings}
	got, err := resolveRequestJobRole(cfg, modelrole.StageSpecDrafting, "spec drafting", "sonnet", "")
	if err != nil {
		t.Fatalf("resolveRequestJobRole: %v", err)
	}
	if got.RouteSelection == nil || got.RouteSelection.ModelName != "sonnet" {
		t.Fatalf("RouteSelection = %+v, want the planning choice sonnet", got.RouteSelection)
	}

	// modelChoice is never applied to oracle drafting (the review role):
	// "sonnet" is not even a roles.review model, so passing it through
	// would refuse the job outright if it were consulted at all.
	got, err = resolveRequestJobRole(cfg, modelrole.StageOracleDrafting, "oracle drafting", "sonnet", "")
	if err != nil {
		t.Fatalf("resolveRequestJobRole(oracle drafting): %v", err)
	}
	if got.RouteSelection == nil || got.RouteSelection.ModelName != "luna" {
		t.Fatalf("RouteSelection = %+v, want roles.execution's own fallback model (luna), unaffected by the planning choice", got.RouteSelection)
	}
}

// TestResolveRequestJobRoleRecordsSkipOnRouteSelection proves a
// fallback selection (the model's first declared route unusable, the
// second chosen instead) records that skip on the returned
// requestJobRoleOverride.RouteSelection -- run_ticket.go's own
// routeSkipsForRun then carries it onto the recorded run.Attempt
// (operator rule: silence is a bug).
func TestResolveRequestJobRoleRecordsSkipOnRouteSelection(t *testing.T) {
	os.Unsetenv("REQUEST_JOB_ROLE_SKIP_FIRST")
	t.Setenv("REQUEST_JOB_ROLE_SKIP_SECOND", "sk-test")
	settings := sessionconfig.DefaultSettings()
	settings.Routes = map[string]sessionconfig.Route{
		"first":  {CredentialMode: "static", Upstream: "https://first.example.invalid", CredentialEnv: "REQUEST_JOB_ROLE_SKIP_FIRST"},
		"second": {CredentialMode: "static", Upstream: "https://second.example.invalid", CredentialEnv: "REQUEST_JOB_ROLE_SKIP_SECOND"},
	}
	settings.Models = map[string]sessionconfig.Model{
		"luna": {ID: "gpt-5.6-luna", Routes: []string{"first", "second"}},
	}
	settings.Roles = &sessionconfig.Roles{Execution: &sessionconfig.RoleConfig{Model: "luna"}}

	cfg := requestdriver.WorkerConfig{Settings: settings}
	got, err := resolveRequestJobRole(cfg, modelrole.StageSpecDrafting, "spec drafting", "", "")
	if err != nil {
		t.Fatalf("resolveRequestJobRole: %v", err)
	}
	if got.RouteSelection == nil || got.RouteSelection.RouteName != "second" {
		t.Fatalf("RouteSelection = %+v, want route %q chosen", got.RouteSelection, "second")
	}
	if len(got.RouteSelection.Skipped) != 1 || got.RouteSelection.Skipped[0].Route != "first" {
		t.Fatalf("RouteSelection.Skipped = %+v, want one skip naming %q", got.RouteSelection.Skipped, "first")
	}
}

// TestResolveRequestJobRoleAppliesThinkingFromTheRole covers the planning
// role configured with its own thinking level: the resolved
// RouteSelection carries the model/route the role names, and Thinking
// mirrors sel.Thinking (roles.planning.thinking).
func TestResolveRequestJobRoleAppliesThinkingFromTheRole(t *testing.T) {
	t.Setenv("REQUEST_JOB_ROLE_TEST_KEY", "sk-test")
	settings := sessionconfig.DefaultSettings()
	settings.Routes = map[string]sessionconfig.Route{
		"litellm": {CredentialMode: "static", Upstream: "https://litellm.example.invalid", CredentialEnv: "REQUEST_JOB_ROLE_TEST_KEY"},
	}
	settings.Models = map[string]sessionconfig.Model{
		"model-a": {ID: "model-a", Routes: []string{"litellm"}, API: "openai-completions"},
	}
	settings.Roles = &sessionconfig.Roles{
		Planning: &sessionconfig.RoleConfig{Model: "model-a", Thinking: "max"},
	}
	cfg := requestdriver.WorkerConfig{Settings: settings}
	got, err := resolveRequestJobRole(cfg, modelrole.StageSpecDrafting, "spec drafting", "", "")
	if err != nil {
		t.Fatalf("resolveRequestJobRole: %v", err)
	}
	if got.RouteSelection == nil || got.RouteSelection.ModelName != "model-a" {
		t.Fatalf("resolveRequestJobRole = %+v, want roles.planning's own model", got)
	}
	if got.RouteSelection.Policy.WorkerModelAPI != "openai-completions" {
		t.Errorf("Policy.WorkerModelAPI = %q, want the model's own openai-completions", got.RouteSelection.Policy.WorkerModelAPI)
	}
	if got.Thinking != "max" {
		t.Errorf("Thinking = %q, want max", got.Thinking)
	}
}

// TestResolveRequestJobRoleAllowNoCredentialReachesTheSelection covers a
// route with allow_no_credential: true reaching the resolved
// RouteSelection's Policy even when no credential is configured at all.
func TestResolveRequestJobRoleAllowNoCredentialReachesTheSelection(t *testing.T) {
	settings := sessionconfig.DefaultSettings()
	settings.Routes = map[string]sessionconfig.Route{
		"local": {AllowNoCredential: true, Upstream: "https://model-a.example.invalid"},
	}
	settings.Models = map[string]sessionconfig.Model{
		"model-a": {ID: "model-a", Routes: []string{"local"}},
	}
	settings.Roles = &sessionconfig.Roles{
		Review: &sessionconfig.RoleConfig{Model: "model-a"},
	}
	cfg := requestdriver.WorkerConfig{Settings: settings}
	got, err := resolveRequestJobRole(cfg, modelrole.StageOracleDrafting, "oracle drafting", "", "")
	if err != nil {
		t.Fatalf("resolveRequestJobRole: %v", err)
	}
	if got.RouteSelection == nil || !got.RouteSelection.Policy.AllowUnauthenticatedUpstream {
		t.Error("Policy.AllowUnauthenticatedUpstream = false, want true from the route's allow_no_credential")
	}
}

// TestResolveRequestJobRoleUnresolvableModelErrorsWithRoleAndModel covers a
// role that IS configured but names an unknown models: entry: the job
// must fail before launch, with a reason naming both the role and the
// unresolvable model.
func TestResolveRequestJobRoleUnresolvableModelErrorsWithRoleAndModel(t *testing.T) {
	settings := sessionconfig.DefaultSettings()
	settings.Roles = &sessionconfig.Roles{
		Review: &sessionconfig.RoleConfig{Model: "no-such-model"},
	}
	cfg := requestdriver.WorkerConfig{Settings: settings}
	_, err := resolveRequestJobRole(cfg, modelrole.StageOracleDrafting, "oracle drafting", "", "")
	if err == nil {
		t.Fatal("resolveRequestJobRole(unresolvable model) = nil error, want one")
	}
	msg := err.Error()
	if !strings.Contains(msg, "review") || !strings.Contains(msg, "no-such-model") || !strings.Contains(msg, "oracle drafting") {
		t.Errorf("resolveRequestJobRole error = %q, want it to name the role, model, and job", msg)
	}
}
