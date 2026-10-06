package main

import (
	"strings"
	"testing"

	"buildgate/internal/modelrole"
	"buildgate/internal/requestdriver"
	"buildgate/internal/sessionconfig"
)

// jobHarnessSettings gives every role its own harness -- planning pifork,
// execution pi, review pifork -- so a job that used the wrong role's (or a
// run-wide) harness is visible in its argv and worker environment.
func jobHarnessSettings() sessionconfig.Settings {
	s := routesModeExecutionOnlySettings()
	s.Routes["litellm2"] = sessionconfig.Route{CredentialMode: "static", Upstream: "https://litellm2.example.invalid", CredentialEnv: "REQUEST_JOB_ROLE_TEST_KEY2"}
	s.Models["sonnet"] = sessionconfig.Model{ID: "sonnet-4", Routes: []string{"litellm2"}}
	s.Roles.Execution.Harness = "pi"
	s.Roles.Planning = &sessionconfig.RoleConfig{Model: "luna", Harness: "pifork", AllowedHarnesses: []string{"pifork", "pi"}}
	s.Roles.Review = &sessionconfig.RoleConfig{Model: "sonnet", Harness: "pifork"}
	return s
}

// TestDraftingJobsUseTheirOwnRolesHarness: spec and plan drafting resolve the
// planning role's harness, oracle drafting the review role's, and each job's
// argv names it and its worker environment follows it.
func TestDraftingJobsUseTheirOwnRolesHarness(t *testing.T) {
	t.Setenv("REQUEST_JOB_ROLE_TEST_KEY", "sk-test")
	t.Setenv("REQUEST_JOB_ROLE_TEST_KEY2", "sk-test2")
	cfg := requestdriver.WorkerConfig{Settings: jobHarnessSettings()}

	for _, tc := range []struct {
		name  string
		stage modelrole.Stage
		want  string
	}{
		{"spec drafting", modelrole.StageSpecDrafting, "pifork"},
		{"plan drafting", modelrole.StagePlanning, "pifork"},
		{"oracle drafting", modelrole.StageOracleDrafting, "pifork"},
	} {
		got, err := resolveRequestJobRole(cfg, tc.stage, tc.name, "", "")
		if err != nil {
			t.Fatalf("%s: resolveRequestJobRole: %v", tc.name, err)
		}
		if got.Harness != tc.want {
			t.Errorf("%s: Harness = %q, want %q", tc.name, got.Harness, tc.want)
		}
		if len(got.WorkerEnv) == 0 || got.WorkerEnv[0] != "PI_CODING_AGENT_DIR=/home/worker/.pi/agent" {
			t.Errorf("%s: WorkerEnv = %v, want pifork's environment", tc.name, got.WorkerEnv)
		}
	}

	// The planning choice picks a harness within allowed_harnesses for spec
	// and plan drafting only; the review role never takes one.
	got, err := resolveRequestJobRole(cfg, modelrole.StageSpecDrafting, "spec drafting", "", "pi")
	if err != nil || got.Harness != "pi" || len(got.WorkerEnv) != 0 {
		t.Errorf("planning choice pi: got %+v, err %v, want harness pi with no extra environment", got, err)
	}
	got, err = resolveRequestJobRole(cfg, modelrole.StageOracleDrafting, "oracle drafting", "", "pi")
	if err != nil || got.Harness != "pifork" {
		t.Errorf("oracle drafting with a planning harness choice: got %+v, err %v, want the review role's pifork, unaffected", got, err)
	}
	if _, err := resolveRequestJobRole(cfg, modelrole.StageSpecDrafting, "spec drafting", "", "codex"); err == nil || !strings.Contains(err.Error(), "allowed_harnesses") {
		t.Errorf("planning choice outside allowed_harnesses: err = %v, want a refusal naming allowed_harnesses", err)
	}

	// An unset role falls back to the execution role, harness included.
	cfg.Settings.Roles.Planning = nil
	got, err = resolveRequestJobRole(cfg, modelrole.StageSpecDrafting, "spec drafting", "", "")
	if err != nil || got.Harness != "pi" {
		t.Errorf("unset planning role: got %+v, err %v, want the execution role's harness pi", got, err)
	}
}

// TestDraftingJobArgvsAlwaysNameTheHarness: every drafting job's argv carries
// --harness with the value its role resolved, never omitted.
func TestDraftingJobArgvsAlwaysNameTheHarness(t *testing.T) {
	for _, name := range []string{"pi", "pifork"} {
		argvs := map[string][]string{
			"draft_spec":  draftSpecArgs("/h/draft_spec.py", "/w", "/r.md", "/o.md", "/e.json", draftInputFiles{}, 10, "", name),
			"plan":        planTicketsArgs("/h/plan_tickets.py", "/w", "/s.md", "/r.md", "make verify", "/o", "/e.json", draftInputFiles{}, 10, "", name),
			"oracle":      oracleDraftArgs("/h/draft_acceptance_oracles.py", "/w", "/c.md", "", "/o", "/e.json", 9, 3, "", "", name),
			"oracle-pair": oracleDraftArgs("/h/draft_acceptance_oracles.py", "/w", "/c.md", "/fb.md", "/o", "/e.json", 9, 3, draftEcosystem("go"), "max", name),
		}
		for job, args := range argvs {
			if !hasArgPair(args, "--harness", name) {
				t.Errorf("%s argv = %v, want --harness %s", job, args, name)
			}
		}
	}
}
