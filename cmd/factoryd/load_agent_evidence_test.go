package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"buildgate/internal/run"
)

// captureStdout redirects os.Stdout for the duration of fn and returns
// what it wrote. loadAgentEvidence reports its best-effort warnings via
// plain fmt.Printf (see its own doc comment on why they're warnings, not
// errors), so this is the only way a test can assert on the warning text
// itself, as opposed to r.AgentEvidence's own resulting state.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	orig := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = orig }()

	fn()

	if err := w.Close(); err != nil {
		t.Fatalf("close pipe writer: %v", err)
	}
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read captured stdout: %v", err)
	}
	return string(out)
}

// TestLoadAgentEvidenceRetainsBuildReport is the regression test for the
// 2026-09-05 Opus review finding S6: BUILD_REPORT.md — the agent's own
// prose account of what it did — was printed as an informational
// workspace path and never read again, so a shared checkout's next slice
// (or an isolated-worktree rollback) could overwrite or delete it before
// a human ever asked to see it. loadAgentEvidence must copy it into this
// run's own durable directory.
func TestLoadAgentEvidenceRetainsBuildReport(t *testing.T) {
	t.Parallel()
	workspace := t.TempDir()
	dataDir := t.TempDir()
	const report = "# Build report\n\nDid the thing.\n"
	if err := os.WriteFile(filepath.Join(workspace, "BUILD_REPORT.md"), []byte(report), 0o644); err != nil {
		t.Fatalf("write fixture BUILD_REPORT.md: %v", err)
	}

	r := &run.Run{ID: "run-1"}
	loadAgentEvidence(r, workspace, dataDir, "run-1")

	got, err := os.ReadFile(filepath.Join(run.Dir(dataDir, "run-1"), "BUILD_REPORT.md"))
	if err != nil {
		t.Fatalf("read retained BUILD_REPORT.md: %v", err)
	}
	if string(got) != report {
		t.Errorf("retained BUILD_REPORT.md = %q, want %q", got, report)
	}
}

// TestLoadAgentEvidenceRetainsRoundLogs: the file a round's failure_log
// names lives in the worktree's session folder, which does not outlive the
// run; a copy is kept in the run's own directory.
func TestLoadAgentEvidenceRetainsRoundLogs(t *testing.T) {
	t.Parallel()
	workspace := t.TempDir()
	dataDir := t.TempDir()
	roundLog := filepath.Join(workspace, ".pi-build-session", "feedback", "round-1", "verify.log")
	if err := os.MkdirAll(filepath.Dir(roundLog), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(roundLog, []byte("--- FAIL: TestSum\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	loadAgentEvidence(&run.Run{ID: "run-1"}, workspace, dataDir, "run-1")

	got, err := os.ReadFile(filepath.Join(run.Dir(dataDir, "run-1"), "round-logs", "round-1", "verify.log"))
	if err != nil || string(got) != "--- FAIL: TestSum\n" {
		t.Errorf("retained round log = %q, %v, want the round's verify output", got, err)
	}
}

// TestLoadAgentEvidenceToleratesMissingBuildReport covers a build_app.py
// version that predates BUILD_REPORT.md, or a run that never reached the
// point of writing one: absence must not be an error, and must not
// prevent r.AgentEvidence from still being attached.
func TestLoadAgentEvidenceToleratesMissingBuildReport(t *testing.T) {
	t.Parallel()
	workspace := t.TempDir()
	dataDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "BUILD_EVIDENCE.json"), []byte(`{"succeeded":true}`), 0o644); err != nil {
		t.Fatalf("write fixture BUILD_EVIDENCE.json: %v", err)
	}

	r := &run.Run{ID: "run-1"}
	loadAgentEvidence(r, workspace, dataDir, "run-1")

	if r.AgentEvidence == nil || !r.AgentEvidence.Succeeded {
		t.Errorf("AgentEvidence = %+v, want it still attached despite no BUILD_REPORT.md", r.AgentEvidence)
	}
	if _, err := os.Stat(filepath.Join(run.Dir(dataDir, "run-1"), "BUILD_REPORT.md")); !os.IsNotExist(err) {
		t.Errorf("Stat retained BUILD_REPORT.md error = %v, want IsNotExist", err)
	}
}

// TestLoadAgentEvidenceAttachesEvidenceRegardlessOfSchemaVersion is the
// regression test for run.AgentEvidenceSchemaVersion: a schema mismatch
// (or its absence, from a build_app.py old enough to predate the field
// entirely) must only warn, never withhold r.AgentEvidence -- this
// evidence is best-effort by design (see AgentEvidence's own doc comment)
// and is never itself a gate input.
func TestLoadAgentEvidenceAttachesEvidenceRegardlessOfSchemaVersion(t *testing.T) {
	t.Parallel()
	for name, evidenceJSON := range map[string]string{
		"no schema_version field (legacy build_app.py)": `{"succeeded":true}`,
		"current schema_version":                        `{"schema_version":2,"succeeded":true}`,
		"future schema_version this factoryd predates":  `{"schema_version":3,"succeeded":true}`,
	} {
		t.Run(name, func(t *testing.T) {
			workspace := t.TempDir()
			dataDir := t.TempDir()
			if err := os.WriteFile(filepath.Join(workspace, "BUILD_EVIDENCE.json"), []byte(evidenceJSON), 0o644); err != nil {
				t.Fatalf("write fixture BUILD_EVIDENCE.json: %v", err)
			}

			r := &run.Run{ID: "run-1"}
			loadAgentEvidence(r, workspace, dataDir, "run-1")

			if r.AgentEvidence == nil || !r.AgentEvidence.Succeeded {
				t.Errorf("AgentEvidence = %+v, want it attached regardless of schema_version", r.AgentEvidence)
			}
		})
	}
}

// TestLoadAgentEvidenceRoundTripsReviewVerdicts is the per-criterion
// conformity review's schema round-trip: build_app.py's BUILD_EVIDENCE.json
// review_verdicts field
// must decode into run.AgentEvidence.ReviewVerdicts field-for-field, the
// same "no translation layer" contract write_evidence_json's own doc
// comment already promises for every other field.
func TestLoadAgentEvidenceRoundTripsReviewVerdicts(t *testing.T) {
	t.Parallel()
	workspace := t.TempDir()
	dataDir := t.TempDir()
	const evidenceJSON = `{"schema_version":2,"succeeded":true,"review_verdicts":[` +
		`{"criterion":"1. Foo","verdict":"clean","detail":""},` +
		`{"criterion":"2. Bar","verdict":"unavailable","detail":"no-review-verdict"}]}`
	if err := os.WriteFile(filepath.Join(workspace, "BUILD_EVIDENCE.json"), []byte(evidenceJSON), 0o644); err != nil {
		t.Fatalf("write fixture BUILD_EVIDENCE.json: %v", err)
	}

	r := &run.Run{ID: "run-1"}
	loadAgentEvidence(r, workspace, dataDir, "run-1")

	if r.AgentEvidence == nil {
		t.Fatal("AgentEvidence = nil, want it attached")
	}
	want := []run.ReviewVerdict{
		{Criterion: "1. Foo", Verdict: "clean"},
		{Criterion: "2. Bar", Verdict: "unavailable", Detail: "no-review-verdict"},
	}
	if len(r.AgentEvidence.ReviewVerdicts) != len(want) {
		t.Fatalf("ReviewVerdicts = %+v, want %+v", r.AgentEvidence.ReviewVerdicts, want)
	}
	for i := range want {
		if r.AgentEvidence.ReviewVerdicts[i] != want[i] {
			t.Errorf("ReviewVerdicts[%d] = %+v, want %+v", i, r.AgentEvidence.ReviewVerdicts[i], want[i])
		}
	}
}

// TestLoadAgentEvidenceIncompatibleFutureSchemaStillNamesTheVersionMismatch
// is the regression test for a real GitHub Codex App review finding on
// this same schema_version mechanism: a future schema_version whose
// change is a field's JSON *type*, not just an added/removed field (here,
// "rounds" as an object instead of an array), fails json.Unmarshal outright
// against run.AgentEvidence's current shape -- unlike the sibling test
// above, whose three cases never change any field's type, only the
// schema_version number itself. r.AgentEvidence is still correctly left
// nil (there is no shape to attach), but the warning printed must still
// name the schema_version mismatch as the likely cause, not fall back to
// the same opaque "could not parse" message a genuinely corrupt file
// would produce -- exactly the diagnosis schema_version exists to give.
// This doesn't (and can't, without a hand-written partial JSON decoder
// far past what this fix warrants) recover partial evidence from an
// incompatible shape; it only makes the resulting warning traceable to
// version skew instead of unexplained.
func TestLoadAgentEvidenceIncompatibleFutureSchemaStillNamesTheVersionMismatch(t *testing.T) {
	workspace := t.TempDir()
	dataDir := t.TempDir()
	// schema_version 3 with "rounds" as an object, not the []AgentEvidenceRound
	// array run.AgentEvidence declares -- a real type conflict, not just an
	// unknown field json.Unmarshal would otherwise silently tolerate.
	const evidenceJSON = `{"schema_version":3,"succeeded":true,"rounds":{"unexpected":"shape"}}`
	if err := os.WriteFile(filepath.Join(workspace, "BUILD_EVIDENCE.json"), []byte(evidenceJSON), 0o644); err != nil {
		t.Fatalf("write fixture BUILD_EVIDENCE.json: %v", err)
	}

	stdout := captureStdout(t, func() {
		r := &run.Run{ID: "run-1"}
		loadAgentEvidence(r, workspace, dataDir, "run-1")
		if r.AgentEvidence != nil {
			t.Errorf("AgentEvidence = %+v, want nil -- an incompatible shape cannot be decoded into it", r.AgentEvidence)
		}
	})

	if !strings.Contains(stdout, "schema_version 3") || !strings.Contains(stdout, "this factoryd understands 2") {
		t.Errorf("warning output = %q, want it to name the schema_version mismatch rather than only report a generic parse failure", stdout)
	}
}

// A round's additive `autofix` record (build_app.py) does not stop the
// evidence from loading: the field is carried by the file, not by the struct.
func TestLoadAgentEvidenceToleratesTheAutofixRoundField(t *testing.T) {
	// Not parallel: captureStdout swaps os.Stdout for the whole process.
	workspace := t.TempDir()
	body := `{"schema_version": 2, "rounds": [{"index": 1, "agent": "pi", "autofix": {"commands": [{"command": "gofmt -w .", "exit_code": 1, "timed_out": false, "duration_s": 0.2}], "reverted_count": 0, "reverted": []}}]}`
	if err := os.WriteFile(filepath.Join(workspace, "BUILD_EVIDENCE.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	r := &run.Run{ID: "run-1"}
	out := captureStdout(t, func() { loadAgentEvidence(r, workspace, t.TempDir(), "run-1") })
	if strings.Contains(out, "warning") {
		t.Errorf("loading warned: %q", out)
	}
	if r.AgentEvidence == nil || len(r.AgentEvidence.Rounds) != 1 || r.AgentEvidence.Rounds[0].Index != 1 {
		t.Errorf("AgentEvidence = %+v, want one round loaded", r.AgentEvidence)
	}
}
