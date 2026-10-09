package triage

import (
	"path/filepath"
	"testing"

	"buildgate/internal/run"
)

// colouredPytestSetupErrors is the tail of a real verify log (humanize,
// 2026-10-08): pytest under the repository's own `--color=yes`, every
// failing line starting with a colour code.
const colouredPytestSetupErrors = "\x1b[32m.\x1b[0m\x1b[32m.\x1b[0m\n" +
	"==================================== ERRORS ====================================\n" +
	"\x1b[31m\x1b[1m_______________________ ERROR at setup of test_apnumber ________________________\x1b[0m\n" +
	"file /workspace/tests/test_benchmarks.py, line 24\n" +
	"\x1b[31mE       fixture 'benchmark' not found\x1b[0m\n" +
	"\x1b[36m\x1b[1m=========================== short test summary info ============================\x1b[0m\n" +
	"\x1b[31mERROR\x1b[0m tests/test_benchmarks.py::\x1b[1mtest_apnumber\x1b[0m\n" +
	"\x1b[31m\x1b[32m736 passed\x1b[0m, \x1b[31m\x1b[1m15 errors\x1b[0m\x1b[31m in 1.03s\x1b[0m\n"

func freshContainerRun(t *testing.T, dataDir string, verifyLog string, rounds []run.AgentEvidenceRound) *run.Run {
	t.Helper()
	const id = "run1"
	writeRunFile(t, dataDir, id, "verify.log", verifyLog)
	return &run.Run{
		ID:            id,
		State:         run.StateQuarantined,
		ChangedFiles:  []string{"src/humanize/time.py"},
		GateResults:   []run.GateResult{{Check: "canonical_verify", Passed: false}},
		AgentEvidence: &run.AgentEvidence{Rounds: rounds},
		Attempts: []run.Attempt{
			{Kind: "build", ExitCode: 0},
			{Kind: "verify", ExitCode: 1, LogPath: filepath.Join(run.Dir(dataDir, id), "verify.log")},
		},
	}
}

func boolPtr(b bool) *bool { return &b }

// TestTriageNamesAColouredPytestError: before the colour codes were stripped
// ahead of matching, this log gave "canonical_verify failed: exit 1".
func TestTriageNamesAColouredPytestError(t *testing.T) {
	dataDir := t.TempDir()
	r := freshContainerRun(t, dataDir, colouredPytestSetupErrors, []run.AgentEvidenceRound{{Index: 1, VerifyPassed: boolPtr(false)}})
	want := "canonical_verify failed: ERROR at setup of test_apnumber (pytest)"
	if got := Run(r, dataDir); got != want {
		t.Errorf("Run() = %q, want %q", got, want)
	}
}

// TestTriageNamesAPytestSummaryErrorLine covers the summary's own
// "ERROR <node id>" line, the only one a collection error leaves.
func TestTriageNamesAPytestSummaryErrorLine(t *testing.T) {
	dataDir := t.TempDir()
	r := freshContainerRun(t, dataDir, "collected 0 items / 1 error\n\nERROR tests/test_x.py - ModuleNotFoundError\n", nil)
	want := "canonical_verify failed: tests/test_x.py (pytest)"
	if got := Run(r, dataDir); got != want {
		t.Errorf("Run() = %q, want %q", got, want)
	}
}

// TestTriageSaysTheVerifyPassedInsideTheBuild: the build's last round ran the
// same command and it passed, so the fresh container is what differs. The
// sentence says so, and stays within the length every triage line has.
func TestTriageSaysTheVerifyPassedInsideTheBuild(t *testing.T) {
	dataDir := t.TempDir()
	r := freshContainerRun(t, dataDir, colouredPytestSetupErrors, []run.AgentEvidenceRound{
		{Index: 1, VerifyPassed: boolPtr(false)},
		{Index: 2, VerifyPassed: boolPtr(true)},
		{Index: 3, VerifyPassed: boolPtr(true)},
	})
	want := "canonical_verify failed: ERROR at setup of test_apnumber (pytest); it passed inside the build (round 3)"
	got := Run(r, dataDir)
	if got != want {
		t.Errorf("Run() = %q, want %q", got, want)
	}
	if len(got) > maxTriageSentenceLen {
		t.Errorf("Run() is %d bytes, over the %d limit", len(got), maxTriageSentenceLen)
	}
}

// TestTriageKeepsQuietWhenTheBuildItselfFailed: a build whose own process
// failed has no passing round to contrast the verify with.
func TestTriageKeepsQuietWhenTheBuildItselfFailed(t *testing.T) {
	dataDir := t.TempDir()
	r := freshContainerRun(t, dataDir, colouredPytestSetupErrors, []run.AgentEvidenceRound{{Index: 1, VerifyPassed: boolPtr(true)}})
	r.Attempts[0].ExitCode = 2
	if got := passedInsideTheBuildSuffix(r); got != "" {
		t.Errorf("passedInsideTheBuildSuffix = %q, want none for a failed build", got)
	}
}

// TestTriageKeepsALongMarkerBesideTheBuildSuffix: the suffix is short enough
// that a long pytest node id keeps its runner label.
func TestTriageKeepsALongMarkerBesideTheBuildSuffix(t *testing.T) {
	dataDir := t.TempDir()
	const node = "tests/integration/payments/test_refund_idempotency.py::TestRefunds::test_second_refund_is_a_no_op[card-eur]"
	r := freshContainerRun(t, dataDir, "FAILED "+node+" - AssertionError\n", []run.AgentEvidenceRound{{Index: 2, VerifyPassed: boolPtr(true)}})
	want := "canonical_verify failed: " + node + " (pytest); it passed inside the build (round 2)"
	if got := Run(r, dataDir); got != want {
		t.Errorf("Run() = %q, want %q", got, want)
	}
}

// TestTriageDoesNotCallAnotherToolsErrorLinePytest: "ERROR in ./src/x.ts"
// (webpack) or an application's own "ERROR ..." log line is not a pytest
// node id.
func TestTriageDoesNotCallAnotherToolsErrorLinePytest(t *testing.T) {
	dataDir := t.TempDir()
	r := freshContainerRun(t, dataDir, "ERROR in ./src/x.ts\nERROR connection refused\n--- FAIL: TestDial (0.00s)\n", nil)
	want := "canonical_verify failed: TestDial (go test)"
	if got := Run(r, dataDir); got != want {
		t.Errorf("Run() = %q, want %q", got, want)
	}
}
