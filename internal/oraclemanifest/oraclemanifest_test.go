package oraclemanifest

import (
	"testing"

	"buildgate/internal/run"
)

func boolPtr(b bool) *bool { return &b }

func TestCoveredCriteriaKeepsOnlyOracleBackedCriteria(t *testing.T) {
	got, err := CoveredCriteria([]byte(`[
		{"criterion":"1. Returns 200.","oracle_file":"oracle_001.go","rationale":"x"},
		{"criterion":"2. Code is clean.","oracle_file":null,"rationale":"judgment"},
		{"criterion":"3. Empty name.","oracle_file":"","rationale":"x"},
		{"criterion":"","oracle_file":"oracle_004.go","rationale":"x"}]`))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "1. Returns 200." {
		t.Fatalf("covered = %v, want only the one real oracle-backed criterion", got)
	}
}

func TestCoveredCriteriaToleratesNewFields(t *testing.T) {
	got, err := CoveredCriteria([]byte(`[
		{"criterion":"1. Returns 200.","oracle_file":"oracle_001.go","rationale":"x",
		 "criterion_index":1,"target_path":"internal/a/zz_oracle_test.go","supersedes":[]},
		{"criterion":"2. Code is clean.","oracle_file":null,"rationale":"judgment",
		 "criterion_index":2,"target_path":null,"supersedes":[]},
		{"criterion":"3. Dropped.","oracle_file":null,"rationale":"dropped: over per-ticket cap",
		 "criterion_index":3,"target_path":null,"supersedes":["old_test.go"]}]`))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "1. Returns 200." {
		t.Fatalf("covered = %v, want only criterion 1", got)
	}
}

func TestCoveredCriteriaOldManifestWithoutNewFields(t *testing.T) {
	got, err := CoveredCriteria([]byte(`[{"criterion":"1. A.","oracle_file":"o.go","rationale":"r"}]`))
	if err != nil || len(got) != 1 {
		t.Fatalf("got %v, %v; want the one covered criterion", got, err)
	}
}

func TestCoveredCriteriaAllNullOracleFilesCoverNothing(t *testing.T) {
	got, err := CoveredCriteria([]byte(`[{"criterion":"1. A.","oracle_file":null,"criterion_index":1,"target_path":null,"supersedes":[]}]`))
	if err != nil || len(got) != 0 {
		t.Fatalf("got %v, %v; want no covered criteria", got, err)
	}
}

func TestCoveredCriteriaRejectsAnObject(t *testing.T) {
	if _, err := CoveredCriteria([]byte(`{"criterion":"x"}`)); err == nil {
		t.Fatal("CoveredCriteria(object): want an error, got nil")
	}
}

func TestGateOutcomeAnyFailureWins(t *testing.T) {
	gr := func(check string, passed bool) run.GateResult { return run.GateResult{Check: check, Passed: passed} }
	if got := GateOutcome([]run.GateResult{gr("verify", true)}); got != nil {
		t.Errorf("no reference_oracle result: got %v, want nil", *got)
	}
	if got := GateOutcome([]run.GateResult{gr("reference_oracle", true), gr("reference_oracle", true)}); got == nil || !*got {
		t.Errorf("all passed: got %v, want true", got)
	}
	if got := GateOutcome([]run.GateResult{gr("reference_oracle", false), gr("reference_oracle", true)}); got == nil || *got {
		t.Errorf("earlier failure then pass: got %v, want false", got)
	}
}
