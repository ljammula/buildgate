package main

import (
	"os"
	"path/filepath"
	"testing"

	"buildgate/internal/run"
)

func writeRunFile(t *testing.T, dataDir, id, name, content string) {
	t.Helper()
	dir := run.Dir(dataDir, id)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestLoadSpecConformityVerdictsReadsTheRetainedEvidence(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	writeRunFile(t, dataDir, "run-1", "CONFORMITY_EVIDENCE.json",
		`{"schema_version":1,"succeeded":true,"review_verdicts":[{"criterion":"1. It works.","verdict":"clean"}]}`)
	r := &run.Run{ID: "run-1", SpecConformityConfigured: true}

	loadSpecConformityVerdicts(r, dataDir, "run-1")

	if len(r.SpecConformityVerdicts) != 1 || r.SpecConformityVerdicts[0].Verdict != "clean" {
		t.Fatalf("SpecConformityVerdicts = %+v, want the one clean verdict", r.SpecConformityVerdicts)
	}
}

// A ticket that declared no criteria must not pick up a stale or foreign
// CONFORMITY_EVIDENCE.json that happens to sit in the run directory.
func TestLoadSpecConformityVerdictsIgnoresARunThatNeverDeclaredCriteria(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	writeRunFile(t, dataDir, "run-1", "CONFORMITY_EVIDENCE.json",
		`{"review_verdicts":[{"criterion":"1. x","verdict":"clean"}]}`)
	r := &run.Run{ID: "run-1"}

	loadSpecConformityVerdicts(r, dataDir, "run-1")

	if len(r.SpecConformityVerdicts) != 0 {
		t.Fatalf("SpecConformityVerdicts = %+v, want none when SpecConformityConfigured is false", r.SpecConformityVerdicts)
	}
}

func TestLoadSpecConformityVerdictsToleratesMissingAndMalformedEvidence(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	r := &run.Run{ID: "run-1", SpecConformityConfigured: true}
	loadSpecConformityVerdicts(r, dataDir, "run-1") // no file at all: not an event
	if len(r.SpecConformityVerdicts) != 0 {
		t.Fatalf("verdicts = %+v, want none", r.SpecConformityVerdicts)
	}

	writeRunFile(t, dataDir, "run-1", "CONFORMITY_EVIDENCE.json", "not json")
	loadSpecConformityVerdicts(r, dataDir, "run-1") // malformed: a warning, never a panic or a state change
	if len(r.SpecConformityVerdicts) != 0 {
		t.Fatalf("verdicts = %+v, want none for malformed evidence", r.SpecConformityVerdicts)
	}
}

func TestLoadOracleCoverageReadsTheManifest(t *testing.T) {
	t.Parallel()
	oracleDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(oracleDir, "MANIFEST.json"),
		[]byte(`[{"criterion":"1. Returns 200.","oracle_file":"o.go","rationale":"x"},{"criterion":"2. Clean.","oracle_file":null,"rationale":"y"}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	r := &run.Run{ID: "run-1", ReferenceOracleDir: oracleDir}

	loadOracleCoverage(r, "run-1")

	if len(r.OracleCoveredCriteria) != 1 || r.OracleCoveredCriteria[0] != "1. Returns 200." {
		t.Fatalf("OracleCoveredCriteria = %v, want only the oracle-backed criterion", r.OracleCoveredCriteria)
	}
}

func TestLoadOracleCoverageWithoutAManifestOrDirIsANoOp(t *testing.T) {
	t.Parallel()
	r := &run.Run{ID: "run-1"}
	loadOracleCoverage(r, "run-1")
	r.ReferenceOracleDir = t.TempDir() // a directory with no MANIFEST.json (hand-written oracle, no Phase 1 draft)
	loadOracleCoverage(r, "run-1")
	if len(r.OracleCoveredCriteria) != 0 {
		t.Fatalf("OracleCoveredCriteria = %v, want none", r.OracleCoveredCriteria)
	}
}

// A symlinked MANIFEST.json must be refused, not followed: the oracle
// directory is operator-populated, and following a link would let it read
// an arbitrary host file into the run record.
func TestLoadOracleCoverageRefusesASymlinkedManifest(t *testing.T) {
	t.Parallel()
	oracleDir := t.TempDir()
	target := filepath.Join(t.TempDir(), "elsewhere.json")
	if err := os.WriteFile(target, []byte(`[{"criterion":"1. x","oracle_file":"o.go"}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(oracleDir, "MANIFEST.json")); err != nil {
		t.Fatal(err)
	}
	r := &run.Run{ID: "run-1", ReferenceOracleDir: oracleDir}

	loadOracleCoverage(r, "run-1")

	if len(r.OracleCoveredCriteria) != 0 {
		t.Fatalf("OracleCoveredCriteria = %v, want none: a symlinked manifest must not be followed", r.OracleCoveredCriteria)
	}
}

// A re-finalization whose evidence is now missing must not keep the prior
// attempt's verdicts/coverage on the record (found via review).
func TestLoadersClearStaleStateWhenEvidenceIsGone(t *testing.T) {
	t.Parallel()
	r := &run.Run{
		ID:                       "run-1",
		SpecConformityConfigured: true,
		SpecConformityVerdicts:   []run.ReviewVerdict{{Criterion: "1. x", Verdict: "clean"}},
		OracleCoveredCriteria:    []string{"1. x"},
	}
	loadSpecConformityVerdicts(r, t.TempDir(), "run-1")
	loadOracleCoverage(r, "run-1") // no ReferenceOracleDir
	if r.SpecConformityVerdicts != nil || r.OracleCoveredCriteria != nil {
		t.Fatalf("stale state survived: verdicts=%+v covered=%v", r.SpecConformityVerdicts, r.OracleCoveredCriteria)
	}
}
