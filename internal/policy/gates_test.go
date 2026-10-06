package policy

import (
	"reflect"
	"testing"
)

// TestAllGateChecksExactValue pins AllGateChecks' exact value and order --
// the same set EvaluateRun has always produced, now assembled from
// CommandGates' own registry order instead of being hand-listed twice.
// A change here that isn't also reflected in EvaluateRun's own gate order
// (or vice versa) is exactly the drift this slice exists to prevent.
func TestAllGateChecksExactValue(t *testing.T) {
	want := []string{
		"canonical_verify",
		"diff_scope",
		"required_files_changed",
		"required_content_present",
		"tests_added",
		"full_suite_verify",
		"lint",
		"security_audit",
		"unit_tests",
		"integration_tests",
		"reference_oracle",
	}
	if !reflect.DeepEqual(AllGateChecks, want) {
		t.Fatalf("AllGateChecks = %v, want %v", AllGateChecks, want)
	}
}

// TestCommandGatesOrderAndFields pins CommandGates' registry order and
// each entry's ID/Flag/ProjectKey/RerunAfterOracleCommit -- the table
// every command-gate call site now iterates instead of naming gates
// individually.
func TestCommandGatesOrderAndFields(t *testing.T) {
	type want struct {
		id, flag, projectKey string
		rerun                bool
	}
	wantGates := []want{
		{"lint", "lint-command", "lint_command", true},
		{"security_audit", "security-command", "security_command", true},
		{"unit_tests", "unit-test-command", "unit_test_command", true},
		{"integration_tests", "integration-test-command", "integration_test_command", true},
		{"reference_oracle", "reference-oracle-command", "reference_oracle_command", false},
	}
	if len(CommandGates) != len(wantGates) {
		t.Fatalf("len(CommandGates) = %d, want %d", len(CommandGates), len(wantGates))
	}
	for i, w := range wantGates {
		g := CommandGates[i]
		if g.ID != w.id || g.Flag != w.flag || g.ProjectKey != w.projectKey || g.RerunAfterOracleCommit != w.rerun {
			t.Errorf("CommandGates[%d] = %+v, want ID=%q Flag=%q ProjectKey=%q RerunAfterOracleCommit=%v", i, g, w.id, w.flag, w.projectKey, w.rerun)
		}
		if g.Help == "" {
			t.Errorf("CommandGates[%d] (%s) has empty Help", i, g.ID)
		}
	}
	if g := ReferenceOracleGateID; g != "reference_oracle" {
		t.Errorf("ReferenceOracleGateID = %q, want \"reference_oracle\"", g)
	}
}

// TestCommandGateIDsMatchesRegistryOrder pins CommandGateIDs()'s output
// against CommandGates' own order, since several callers (post-oracle-
// commit gate derivation, the CLI flag loop) depend on both staying in
// lockstep.
func TestCommandGateIDsMatchesRegistryOrder(t *testing.T) {
	ids := CommandGateIDs()
	if len(ids) != len(CommandGates) {
		t.Fatalf("len(CommandGateIDs()) = %d, want %d", len(ids), len(CommandGates))
	}
	for i, g := range CommandGates {
		if ids[i] != g.ID {
			t.Errorf("CommandGateIDs()[%d] = %q, want %q", i, ids[i], g.ID)
		}
	}
}

// TestCommandGatesRerunAfterOracleCommitSet pins the set of gates that
// re-run after an oracle commit: every command gate except
// reference_oracle -- internal/workflow's postOracleCommitGates and
// cmd/factoryd's own post-oracle-commit re-run loop both derive their
// gate list from this field.
func TestCommandGatesRerunAfterOracleCommitSet(t *testing.T) {
	var got []string
	for _, g := range CommandGates {
		if g.RerunAfterOracleCommit {
			got = append(got, g.ID)
		}
	}
	want := []string{"lint", "security_audit", "unit_tests", "integration_tests"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("gates with RerunAfterOracleCommit = %v, want %v", got, want)
	}
}
