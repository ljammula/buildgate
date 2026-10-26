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
// attempt's coverage on the record (found via review).
func TestLoadersClearStaleStateWhenEvidenceIsGone(t *testing.T) {
	t.Parallel()
	r := &run.Run{ID: "run-1", OracleCoveredCriteria: []string{"1. x"}}
	loadOracleCoverage(r, "run-1") // no ReferenceOracleDir
	if r.OracleCoveredCriteria != nil {
		t.Fatalf("stale state survived: covered=%v", r.OracleCoveredCriteria)
	}
}
