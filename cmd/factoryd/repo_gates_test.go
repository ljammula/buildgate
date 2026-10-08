package main

import (
	"strings"
	"testing"

	"buildgate/internal/policy"
	"buildgate/internal/run"
	"buildgate/internal/workflow"
)

// A repository's own gates have no flag: they come from its committed
// .factory.yml alone and reach the run's gate commands as "repo-<id>".
func TestRepoDefinedGatesReachTheRunsGateCommands(t *testing.T) {
	dir := t.TempDir()
	writeTestFactoryYML(t, dir, `lint_command: "golangci-lint run ./..."
gates:
  - id: no_todo
    command: "! grep -rn TODO src"
  - id: licenses
    command: "scripts/check-licenses.sh"
`)
	fs, verifyCommand, fastCheckCommand, gateCommands, preflightProfile, releaseProtectedPaths, testPatterns, meterTokenCeiling, meterCostCeilingMicroUSD, meterTokenBudget, meterCostBudget := newProjectConfigTestFlags()
	if err := fs.Parse(nil); err != nil {
		t.Fatal(err)
	}
	if err := applyProjectConfigDefaults(explicitFlagNames(fs), dir, verifyCommand, fastCheckCommand, gateCommands, preflightProfile, releaseProtectedPaths, testPatterns, meterTokenCeiling, meterCostCeilingMicroUSD, *meterTokenBudget, *meterCostBudget); err != nil {
		t.Fatalf("applyProjectConfigDefaults: %v", err)
	}
	got := resolvedGateCommands(gateCommands)
	want := map[string]string{
		"lint":          "golangci-lint run ./...",
		"repo-no_todo":  "! grep -rn TODO src",
		"repo-licenses": "scripts/check-licenses.sh",
	}
	for check, command := range want {
		if got[check] != command {
			t.Errorf("gate %s = %q, want %q", check, got[check], command)
		}
	}
	if checks := policy.RepoGateChecks(got); len(checks) != 2 || checks[0] != "repo-licenses" || checks[1] != "repo-no_todo" {
		t.Errorf("repo gates = %v, want [repo-licenses repo-no_todo]", checks)
	}
	for _, g := range policy.CommandGates {
		if _, ok := got[g.ID]; !ok {
			t.Errorf("registry gate %s is missing from the resolved commands", g.ID)
		}
	}
}

// The evidence report lists each repo-defined gate that ran, and only those.
func TestEvidenceListsRepoDefinedGates(t *testing.T) {
	var b strings.Builder
	writeRepoGateLines(&b, []run.GateResult{
		{Check: "canonical_verify", Passed: true},
		{Check: "lint", Passed: true},
		{Check: "repo-licenses", Passed: true},
		{Check: "repo-no_todo", Passed: false},
	})
	if got, want := b.String(), "- `repo-licenses`: pass\n- `repo-no_todo`: FAIL\n"; got != want {
		t.Errorf("repo gate lines = %q, want %q", got, want)
	}
}

// An accepted result with no result for a repo-defined gate was judged by
// workflow code that predates them: it is quarantined, naming the gate.
func TestAcceptedResultMissingARepoGateIsQuarantined(t *testing.T) {
	commands := map[string]string{"lint": "golangci-lint run", "repo-licenses": "scripts/check-licenses.sh", "repo-no_todo": "! grep -rn TODO src"}
	complete := workflow.RunWorkflowResult{State: run.StateAccepted, GateResults: []run.GateResult{
		{Check: "lint", Passed: true}, {Check: "repo-licenses", Passed: true}, {Check: "repo-no_todo", Passed: true},
	}}
	if got := requireRepoGateResults(commands, complete); got.State != run.StateAccepted || len(got.GateResults) != 3 {
		t.Errorf("a result with every repo gate changed: state %s, %d gate results", got.State, len(got.GateResults))
	}
	missing := workflow.RunWorkflowResult{State: run.StateAccepted, GateResults: []run.GateResult{
		{Check: "lint", Passed: true}, {Check: "repo-licenses", Passed: true},
	}}
	got := requireRepoGateResults(commands, missing)
	if got.State != run.StateQuarantined {
		t.Fatalf("state = %s, want quarantined", got.State)
	}
	last := got.GateResults[len(got.GateResults)-1]
	if len(got.GateResults) != 3 || last.Check != "repo-no_todo" || last.Passed {
		t.Errorf("gate results = %+v, want a failed repo-no_todo added", got.GateResults)
	}
	quarantined := workflow.RunWorkflowResult{State: run.StateQuarantined}
	if got := requireRepoGateResults(commands, quarantined); len(got.GateResults) != 0 {
		t.Errorf("a result that is not accepted gained gate results: %+v", got.GateResults)
	}
}
