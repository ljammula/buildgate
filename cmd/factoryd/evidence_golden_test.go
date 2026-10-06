package main

import (
	"os"
	"path/filepath"
	"testing"

	"buildgate/internal/codereview"
	"buildgate/internal/conformity"
	"buildgate/internal/run"
)

// goldenDir is internal/evidence/testdata/golden, the one committed
// fixture set every harness evidence file's schema lives and dies by:
// agent/pi/tests/generate_evidence_goldens.py writes it from the REAL
// Python writers (build_app.write_evidence_json,
// conformity_review.write_conformity_evidence_json,
// code_review.write_code_review_evidence_json, draft_spec.write_evidence,
// plan_tickets.write_evidence, draft_acceptance_oracles.write_evidence),
// and agent/pi/tests/test_evidence_goldens.py fails if a writer's current
// output has drifted from what's committed here. This file is the other
// half of that contract: every golden (and the two hand-recorded real-run
// files) parsed by the REAL Go reader, asserting load-bearing fields, not
// just "no error" -- plus one version-mismatch case per reader, proving
// each one actually rejects (or, for BUILD_EVIDENCE.json's deliberately
// best-effort loadAgentEvidence, tolerates) a schema_version it doesn't
// recognise the way its own doc comment says it must.
func goldenDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.Abs(filepath.Join("..", "..", "internal", "evidence", "testdata", "golden"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("golden dir %s: %v", dir, err)
	}
	return dir
}

func readGolden(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(goldenDir(t), name))
	if err != nil {
		t.Fatalf("read golden %s: %v", name, err)
	}
	return b
}

// TestBuildEvidenceGoldenParsesWithLoadAgentEvidence covers
// BUILD_EVIDENCE.json, both the generated golden and a real recorded run
// (build_evidence.recorded.json, copied from a real factoryd-example-app run
// and scrubbed of host-identifying content).
func TestBuildEvidenceGoldenParsesWithLoadAgentEvidence(t *testing.T) {
	for _, name := range []string{"build_evidence.json", "build_evidence.recorded.json"} {
		t.Run(name, func(t *testing.T) {
			workspace := t.TempDir()
			dataDir := t.TempDir()
			if err := os.WriteFile(filepath.Join(workspace, "BUILD_EVIDENCE.json"), readGolden(t, name), 0o600); err != nil {
				t.Fatal(err)
			}
			r := &run.Run{ID: "run-1"}
			loadAgentEvidence(r, workspace, dataDir, "run-1")
			if r.AgentEvidence == nil {
				t.Fatal("AgentEvidence = nil, want it populated from the golden")
			}
			if r.AgentEvidence.SchemaVersion != run.AgentEvidenceSchemaVersion {
				t.Errorf("SchemaVersion = %d, want %d", r.AgentEvidence.SchemaVersion, run.AgentEvidenceSchemaVersion)
			}
			if !r.AgentEvidence.Succeeded {
				t.Error("Succeeded = false, want true")
			}
			if len(r.AgentEvidence.Rounds) == 0 {
				t.Fatal("Rounds is empty, want at least one recorded round")
			}
			if r.AgentEvidence.Rounds[0].VerifyPassed == nil || !*r.AgentEvidence.Rounds[0].VerifyPassed {
				t.Error("Rounds[0].VerifyPassed = nil/false, want true")
			}
		})
	}
}

// TestBuildEvidenceUnknownSchemaVersionIsToleratedNotRejected pins
// loadAgentEvidence's deliberate exception to "reject an unknown version":
// BUILD_EVIDENCE.json is best-effort by design (run.AgentEvidence's own
// doc comment) -- a future schema_version still parses successfully here
// (json.Unmarshal doesn't care about the value) and is still attached to
// the run, only with a printed warning, never an error.
func TestBuildEvidenceUnknownSchemaVersionIsToleratedNotRejected(t *testing.T) {
	workspace := t.TempDir()
	dataDir := t.TempDir()
	bad := []byte(`{"schema_version":99,"succeeded":true,"stopped_reason":"x","rounds":[]}`)
	if err := os.WriteFile(filepath.Join(workspace, "BUILD_EVIDENCE.json"), bad, 0o600); err != nil {
		t.Fatal(err)
	}
	r := &run.Run{ID: "run-1"}
	out := captureStdout(t, func() { loadAgentEvidence(r, workspace, dataDir, "run-1") })
	if r.AgentEvidence == nil {
		t.Fatal("AgentEvidence = nil, want it still populated despite the unknown schema_version")
	}
	if r.AgentEvidence.SchemaVersion != 99 {
		t.Errorf("SchemaVersion = %d, want 99", r.AgentEvidence.SchemaVersion)
	}
	if out == "" {
		t.Error("want a printed warning naming the schema_version mismatch")
	}
}

func TestConformityEvidenceGoldenParsesWithParseVerdicts(t *testing.T) {
	for _, tc := range []struct {
		name     string
		wantLen  int
		wantFlag bool
	}{
		{"conformity_evidence.json", 2, true},
		{"conformity_evidence.recorded.json", 6, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			verdicts, err := conformity.ParseVerdicts(readGolden(t, tc.name))
			if err != nil {
				t.Fatalf("ParseVerdicts: %v", err)
			}
			if len(verdicts) != tc.wantLen {
				t.Fatalf("len(verdicts) = %d, want %d", len(verdicts), tc.wantLen)
			}
			gotFlag := false
			for _, v := range verdicts {
				if v.Verdict == "flagged" {
					gotFlag = true
				}
			}
			if gotFlag != tc.wantFlag {
				t.Errorf("has a flagged verdict = %v, want %v", gotFlag, tc.wantFlag)
			}
		})
	}
}

func TestConformityEvidenceRejectsUnknownSchemaVersion(t *testing.T) {
	_, err := conformity.ParseVerdicts([]byte(`{"schema_version":2,"review_verdicts":[]}`))
	if err == nil {
		t.Fatal("ParseVerdicts(schema_version 2): want an error, got nil")
	}
}

func TestCodeReviewEvidenceGoldenParsesWithParseResult(t *testing.T) {
	result, err := codereview.ParseResult(readGolden(t, "code_review_evidence.json"))
	if err != nil {
		t.Fatalf("ParseResult: %v", err)
	}
	if result.Policy != "required" {
		t.Errorf("Policy = %q, want %q", result.Policy, "required")
	}
	if !result.Available {
		t.Error("Available = false, want true")
	}
	if len(result.Findings) != 1 || result.Findings[0].Severity != "high" {
		t.Fatalf("Findings = %+v, want one high-severity finding", result.Findings)
	}
}

func TestCodeReviewEvidenceRejectsUnknownSchemaVersion(t *testing.T) {
	if _, err := codereview.ParseResult([]byte(`{"schema_version":2,"review_policy":"required","available":true,"findings":[]}`)); err == nil {
		t.Fatal("ParseResult(schema_version 2): want an error, got nil")
	}
}

func TestSpecDraftEvidenceGoldenParsesWithReadSpecDraftOutputs(t *testing.T) {
	scratchDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(scratchDir, "evidence.json"), readGolden(t, "spec_draft_evidence.json"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, evidence, err := readSpecDraftOutputs(scratchDir)
	if err != nil {
		t.Fatalf("readSpecDraftOutputs: %v", err)
	}
	if evidence == nil {
		t.Fatal("evidence = nil, want it populated from the golden")
	}
	if evidence.AgentExitCode != 0 || !evidence.AgentsMDUsed || evidence.DurationS != 45.2 {
		t.Errorf("evidence = %+v, unexpected", evidence)
	}
}

func TestSpecDraftEvidenceRejectsUnknownSchemaVersion(t *testing.T) {
	scratchDir := t.TempDir()
	bad := []byte(`{"schema_version":2,"agent_exit_code":0,"duration_s":1,"agents_md_used":true}`)
	if err := os.WriteFile(filepath.Join(scratchDir, "evidence.json"), bad, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := readSpecDraftOutputs(scratchDir); err == nil {
		t.Fatal("readSpecDraftOutputs(schema_version 2): want an error, got nil")
	}
}

func TestPlanEvidenceGoldenParsesWithReadPlanTicketsOutputs(t *testing.T) {
	scratchDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(scratchDir, "evidence.json"), readGolden(t, "plan_evidence.json"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, evidence, err := readPlanTicketsOutputs(scratchDir)
	if err != nil {
		t.Fatalf("readPlanTicketsOutputs: %v", err)
	}
	if evidence == nil {
		t.Fatal("evidence = nil, want it populated from the golden")
	}
	if evidence.AgentExitCode != 0 || !evidence.AgentsMDUsed || evidence.DurationS != 63.4 {
		t.Errorf("evidence = %+v, unexpected", evidence)
	}
}

func TestPlanEvidenceRejectsUnknownSchemaVersion(t *testing.T) {
	scratchDir := t.TempDir()
	bad := []byte(`{"schema_version":2,"agent_exit_code":0,"duration_s":1,"agents_md_used":true}`)
	if err := os.WriteFile(filepath.Join(scratchDir, "evidence.json"), bad, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := readPlanTicketsOutputs(scratchDir); err == nil {
		t.Fatal("readPlanTicketsOutputs(schema_version 2): want an error, got nil")
	}
}

func TestOracleDraftEvidenceGoldenParsesWithReadDraftEvidence(t *testing.T) {
	scratchDir := t.TempDir()
	evidencePath := filepath.Join(scratchDir, "evidence.json")
	if err := os.WriteFile(evidencePath, readGolden(t, "oracle_draft_evidence.json"), 0o600); err != nil {
		t.Fatal(err)
	}
	ev, err := readDraftEvidence(evidencePath)
	if err != nil {
		t.Fatalf("readDraftEvidence: %v", err)
	}
	if ev.Status != "drafted" || ev.DroppedCount != 0 {
		t.Errorf("evidence = %+v, unexpected", ev)
	}
	if len(ev.Failures) != 1 || ev.Failures[0].CriterionIndex != 3 {
		t.Errorf("Failures = %+v, want one entry for criterion 3", ev.Failures)
	}
}

func TestOracleDraftEvidenceRejectsUnknownSchemaVersion(t *testing.T) {
	scratchDir := t.TempDir()
	evidencePath := filepath.Join(scratchDir, "evidence.json")
	bad := []byte(`{"schema_version":2,"status":"drafted","dropped_count":0,"failures":[]}`)
	if err := os.WriteFile(evidencePath, bad, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readDraftEvidence(evidencePath); err == nil {
		t.Fatal("readDraftEvidence(schema_version 2): want an error, got nil")
	}
}
