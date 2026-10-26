package triage

import (
	"strings"
	"testing"

	"buildgate/internal/run"
	"buildgate/internal/unresolved"
)

func reproductionRun(rechecks []run.ReviewReproduction, findings ...run.CodeReviewFinding) *run.Run {
	return &run.Run{
		ID: "run-r", State: run.StateQuarantined,
		GateResults:                []run.GateResult{{Check: "canonical_verify", Passed: true}, {Check: "review_reproduction", Passed: false, ExitCode: 1}},
		CodeReview:                 &run.CodeReviewResult{Available: true, Findings: findings},
		ReviewReproductionRechecks: rechecks,
	}
}

// The review_reproduction sentence names the check, the reproduction's path
// and the test and kind the factory's run recorded, and points at the
// evidence; it never quotes the reviewer's summary or scenario. A recheck
// still failing after the corrective build is named first.
func TestTriageReviewReproductionNamesThePathAndTheFailure(t *testing.T) {
	blocking := run.CodeReviewFinding{
		Severity: "high", Summary: "SECRET-SUMMARY", FailureScenario: "SECRET-SCENARIO", Disposition: run.DispositionBlocks,
		Reproduction: &run.ReviewReproduction{Path: "p/repro_test.go", TestIDs: []string{"TestRepro"}, Verdict: "blocks",
			Runs: []run.ReproductionRun{{Pass: "head1", Tests: map[string]string{"TestRepro": "fail"}}, {Pass: "head2", Tests: map[string]string{"TestRepro": "fail"}}, {Pass: "base", Tests: map[string]string{"TestRepro": "pass"}}}},
	}
	got := Run(reproductionRun(nil, run.CodeReviewFinding{Severity: "low", Summary: "advice", Disposition: run.DispositionAdvice}, blocking), t.TempDir())
	want := `review_reproduction: the factory's run of the reviewer's reproduction "p/repro_test.go" fails on the result commit ("TestRepro" fail); the finding is in the run's review evidence`
	if got != want {
		t.Errorf("sentence = %q\nwant       %q", got, want)
	}
	if strings.Contains(got, "SECRET") {
		t.Errorf("sentence quotes the reviewer: %q", got)
	}

	still := run.ReviewReproduction{Path: "p/old_test.go\nInjected", TestIDs: []string{"TestOld"}, Verdict: "still_failing", Runs: []run.ReproductionRun{{Pass: "recheck", Tests: map[string]string{"TestOld": "timeout"}}}}
	got = Run(reproductionRun([]run.ReviewReproduction{{Path: "p/ok_test.go", Verdict: "passes"}, still}, blocking), t.TempDir())
	if !strings.HasPrefix(got, `review_reproduction: "p/old_test.go Injected" still fails on the result commit after the corrective build ("TestOld" timeout)`) || strings.Contains(got, "\n") {
		t.Errorf("recheck sentence = %q", got)
	}
}

// A run halted because a reproduction could not be held says so, and that
// nothing was refused.
func TestTriageReproductionUnheldHalt(t *testing.T) {
	got := Run(&run.Run{ID: "run-h", State: run.StateHalted, HaltReasonCode: string(unresolved.ReproductionUnheld)}, t.TempDir())
	if !strings.Contains(got, "reproduction could not be held or mounted") || !strings.Contains(got, "nothing was refused") {
		t.Errorf("halt sentence = %q", got)
	}
}
