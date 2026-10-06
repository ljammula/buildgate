package conformity

import (
	"testing"

	"buildgate/internal/run"
)

func boolPtr(b bool) *bool { return &b }

func TestParseVerdictsReadsConformityEvidence(t *testing.T) {
	got, err := ParseVerdicts([]byte(`{"schema_version":1,"succeeded":true,"review_verdicts":[
		{"criterion":"1. It works.","verdict":"clean"},
		{"criterion":"2. It is fast.","verdict":"flagged","detail":"slow"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Verdict != "clean" || got[1].Detail != "slow" {
		t.Fatalf("verdicts = %+v", got)
	}
}

func TestParseVerdictsRejectsGarbage(t *testing.T) {
	if _, err := ParseVerdicts([]byte("not json")); err == nil {
		t.Fatal("ParseVerdicts(garbage): want an error, got nil")
	}
}

func TestParseOracleManifestKeepsOnlyOracleBackedCriteria(t *testing.T) {
	got, err := ParseOracleManifest([]byte(`[
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

func TestParseOracleManifestToleratesNewFields(t *testing.T) {
	got, err := ParseOracleManifest([]byte(`[
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

func TestParseOracleManifestOldManifestWithoutNewFields(t *testing.T) {
	got, err := ParseOracleManifest([]byte(`[{"criterion":"1. A.","oracle_file":"o.go","rationale":"r"}]`))
	if err != nil || len(got) != 1 {
		t.Fatalf("got %v, %v; want the one covered criterion", got, err)
	}
}

func TestParseOracleManifestAllNullOracleFilesCoverNothing(t *testing.T) {
	got, err := ParseOracleManifest([]byte(`[{"criterion":"1. A.","oracle_file":null,"criterion_index":1,"target_path":null,"supersedes":[]}]`))
	if err != nil || len(got) != 0 {
		t.Fatalf("got %v, %v; want no covered criteria", got, err)
	}
}

func TestParseOracleManifestRejectsAnObject(t *testing.T) {
	if _, err := ParseOracleManifest([]byte(`{"criterion":"x"}`)); err == nil {
		t.Fatal("ParseOracleManifest(object): want an error, got nil")
	}
}

func TestCrossCheckClassifiesEveryCase(t *testing.T) {
	verdicts := []run.ReviewVerdict{
		{Criterion: "1. Returns 200.", Verdict: "clean"},
		{Criterion: "2. Handles empty input.", Verdict: "flagged"},
		{Criterion: "3. Code is clean.", Verdict: "clean"},
		{Criterion: "4. Rejects bad tokens.", Verdict: "unavailable"},
	}
	covered := []string{"1. Returns 200.", "2. Handles empty input.", "4. Rejects bad tokens."}

	t.Run("oracle passed", func(t *testing.T) {
		got := CrossCheck(verdicts, covered, boolPtr(true))
		want := []string{AgreementAgree, AgreementDisagree, AgreementProseOnly, AgreementReviewerUnavailable}
		for i, w := range want {
			if got[i].Agreement != w {
				t.Errorf("criterion %d agreement = %q, want %q", i+1, got[i].Agreement, w)
			}
		}
	})
	t.Run("oracle failed", func(t *testing.T) {
		got := CrossCheck(verdicts, covered, boolPtr(false))
		// One command covers the whole ticket, so a failure is not
		// attributable to any one criterion: no per-criterion claim.
		want := []string{AgreementOracleFailed, AgreementOracleFailed, AgreementProseOnly, AgreementOracleFailed}
		for i, w := range want {
			if got[i].Agreement != w {
				t.Errorf("criterion %d agreement = %q, want %q", i+1, got[i].Agreement, w)
			}
		}
	})
	t.Run("oracle did not run", func(t *testing.T) {
		got := CrossCheck(verdicts, covered, nil)
		want := []string{AgreementOracleNotRun, AgreementOracleNotRun, AgreementProseOnly, AgreementOracleNotRun}
		for i, w := range want {
			if got[i].Agreement != w {
				t.Errorf("criterion %d agreement = %q, want %q", i+1, got[i].Agreement, w)
			}
		}
	})
}

// A manifest entry echoed without its leading number must still match the
// verdict's numbered criterion, or every covered criterion would silently
// read as prose-only.
func TestCrossCheckMatchesAcrossNumberPrefixVariance(t *testing.T) {
	got := CrossCheck(
		[]run.ReviewVerdict{{Criterion: "1) Returns 200.", Verdict: "clean"}},
		[]string{"Returns 200."},
		boolPtr(true),
	)
	if len(got) != 1 || !got[0].OracleCovered || got[0].Agreement != AgreementAgree {
		t.Fatalf("CrossCheck = %+v, want the criterion matched as oracle-covered and in agreement", got)
	}
}

func TestCrossCheckEmptyWithoutVerdicts(t *testing.T) {
	if got := CrossCheck(nil, []string{"1. x"}, boolPtr(true)); len(got) != 0 {
		t.Fatalf("CrossCheck(nil verdicts) = %+v, want empty", got)
	}
}

func TestOracleOutcomeAnyFailureWins(t *testing.T) {
	gr := func(check string, passed bool) run.GateResult { return run.GateResult{Check: check, Passed: passed} }
	if got := OracleOutcome([]run.GateResult{gr("verify", true)}); got != nil {
		t.Errorf("no reference_oracle result: got %v, want nil", *got)
	}
	if got := OracleOutcome([]run.GateResult{gr("reference_oracle", true), gr("reference_oracle", true)}); got == nil || !*got {
		t.Errorf("all passed: got %v, want true", got)
	}
	if got := OracleOutcome([]run.GateResult{gr("reference_oracle", false), gr("reference_oracle", true)}); got == nil || *got {
		t.Errorf("earlier failure then pass: got %v, want false", got)
	}
}
