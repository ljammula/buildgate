package main

import (
	"strings"
	"testing"

	"buildgate/internal/policy"
	"buildgate/internal/run"
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
