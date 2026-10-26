package requestdrivertest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"buildgate/internal/handoff"
	"buildgate/internal/requestdriver"
	"buildgate/internal/run"
)

// The blocking finding ReproductionRefusedOn records: what a later build
// may be told of it (its severity, summary, cited line, path, test and
// kind), and what it never is (the failure scenario, the reproduction's
// content).
const (
	ReproductionPath     = "internal/sum/sum_repro_test.go"
	ReproductionTestID   = "TestSumKeepsTheLastElement"
	ReproductionSummary  = "Sum drops the last element"
	ReproductionScenario = "SCENARIO-NEVER-TOLD: Sum([1,2,3]) returns 3"
	ReproductionContent  = "package sum\n\n// CONTENT-NEVER-TOLD\nfunc TestSumKeepsTheLastElement(t *testing.T) {}\n"
	ReproductionRunner   = "go-test-json"
	ReproductionSpecLine = 7
)

// ReproductionRefusedOn builds a quarantined run that a reviewer's
// reproduction refused (review_reproduction, SC-022), as cmd/factoryd's save
// leaves it: one blocking finding whose reproduction the factory held at the
// run's review-repro/0/<path> (read-only, its SHA-256 on the record) and saw
// fail twice on the result and pass on the base, one finding that is advice,
// and the handoff written and hashed. extraFailedChecks are recorded as
// failed too.
func ReproductionRefusedOn(t *testing.T, dataDir, id, branch, baseSHA, resultSHA string, extraFailedChecks ...string) *run.Run {
	t.Helper()
	host := filepath.Join(run.ReviewReproductionDir(run.Dir(dataDir, id)), "0", filepath.FromSlash(ReproductionPath))
	if err := os.MkdirAll(filepath.Dir(host), 0o750); err != nil {
		t.Fatal(err)
	}
	// A later build stubbed under the same run id holds the same copy again.
	if err := os.Remove(host); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if err := os.WriteFile(host, []byte(ReproductionContent), 0o444); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(ReproductionContent))
	failed := map[string]string{ReproductionTestID: "fail"}
	rr := &run.Run{
		ID: id, Ticket: id, State: run.StateQuarantined, Branch: branch, BaseSHA: baseSHA, ResultSHA: resultSHA,
		ChangedFiles: []string{"internal/sum/sum.go"},
		GateResults:  []run.GateResult{{Check: "canonical_verify", Passed: true}, {Check: "diff_scope", Passed: true}},
		CodeReview: &run.CodeReviewResult{Policy: "required", Available: true, Findings: []run.CodeReviewFinding{
			{
				Severity: "high", File: "internal/sum/sum.go", Line: 3, Summary: ReproductionSummary, FailureScenario: ReproductionScenario,
				SpecLine: &run.SpecCitation{Number: ReproductionSpecLine, Text: "Sum returns the sum of every element."},
				Reproduction: &run.ReviewReproduction{
					Path: ReproductionPath, SHA256: hex.EncodeToString(sum[:]), Runner: ReproductionRunner, TestIDs: []string{ReproductionTestID},
					Runs: []run.ReproductionRun{
						{Pass: "head1", Commit: resultSHA, Outcome: "failed", Tests: failed},
						{Pass: "head2", Commit: resultSHA, Outcome: "failed", Tests: failed},
						{Pass: "base", Commit: baseSHA, Outcome: "passed", Tests: map[string]string{ReproductionTestID: "pass"}},
					},
					Verdict: "blocks", Reason: "fails on the result, passes on the base",
				},
				Disposition: run.DispositionBlocks,
			},
			{Severity: "high", Summary: "ADVICE-NEVER-TOLD: a high finding with no reproduction", FailureScenario: "nothing ran it", Disposition: run.DispositionAdvice},
		}},
	}
	// The sentence cmd/factoryd's save records (triage.Run) for it.
	rr.Triage = fmt.Sprintf("review_reproduction: the factory's run of the reviewer's reproduction %q fails on the result commit (%q fail); the finding is in the run's review evidence", ReproductionPath, ReproductionTestID)
	for _, check := range extraFailedChecks {
		rr.GateResults = append(rr.GateResults, run.GateResult{Check: check, Passed: false, ExitCode: 1})
	}
	rr.GateResults = append(rr.GateResults, run.GateResult{Check: "review_reproduction", Passed: false, ExitCode: 1})
	if err := handoff.Sync(rr, dataDir); err != nil {
		t.Fatalf("write the handoff: %v", err)
	}
	return rr
}

// StillFailingAfterRecheck builds the run of a corrective build whose recheck
// of origin's reproduction (ReproductionRefusedOn's held copy) still failed
// on its result: it is refused by review_reproduction with no finding of its
// own, and its handoff is written.
func StillFailingAfterRecheck(t *testing.T, dataDir, id, origin, branch, baseSHA, resultSHA string) *run.Run {
	t.Helper()
	return AfterRecheck(t, dataDir, id, origin, branch, baseSHA, resultSHA, "still_failing", "review_reproduction")
}

// AfterRecheck builds the quarantined run of a build that rechecked
// origin's reproduction (ReproductionRefusedOn's held copy, recorded as the
// manifest entry it ran) on its result with verdict, and failed
// failedChecks; it has no finding of its own, and its handoff is written.
func AfterRecheck(t *testing.T, dataDir, id, origin, branch, baseSHA, resultSHA, verdict string, failedChecks ...string) *run.Run {
	t.Helper()
	entry := HeldEntry(t, dataDir, origin)
	kind, outcome := "fail", "failed"
	if verdict == "passes" {
		kind, outcome = "pass", "passed"
	}
	rr := &run.Run{
		ID: id, Ticket: id, State: run.StateQuarantined, Branch: branch, BaseSHA: baseSHA, DiffBaseSHA: baseSHA, ResultSHA: resultSHA,
		GateResults: []run.GateResult{{Check: "canonical_verify", Passed: true}},
		CodeReview:  &run.CodeReviewResult{Policy: "required", Available: true},
		ReviewReproductionRechecks: []run.ReviewReproduction{{
			Path: ReproductionPath, SHA256: entry.SHA256, Runner: ReproductionRunner, TestIDs: []string{ReproductionTestID},
			Runs:    []run.ReproductionRun{{Pass: run.ReproductionPassRecheck, Commit: resultSHA, Outcome: outcome, Tests: map[string]string{ReproductionTestID: kind}}},
			Verdict: verdict, Recheck: &entry,
		}},
	}
	for _, check := range failedChecks {
		rr.GateResults = append(rr.GateResults, run.GateResult{Check: check, Passed: false, ExitCode: 1})
	}
	// The sentence cmd/factoryd's save records (triage.Run) for it.
	rr.Triage = fmt.Sprintf("review_reproduction: %q still fails on the result commit after the corrective build (%q fail); the finding is in the run's review evidence", ReproductionPath, ReproductionTestID)
	if verdict == "passes" {
		rr.Triage = fmt.Sprintf("%s failed", failedChecks[0])
	}
	if err := handoff.Sync(rr, dataDir); err != nil {
		t.Fatalf("write the handoff: %v", err)
	}
	return rr
}

// HeldEntry is the recheck manifest entry of ReproductionRefusedOn's held
// copy under run origin's review-repro directory.
func HeldEntry(t *testing.T, dataDir, origin string) run.RecheckEntry {
	t.Helper()
	host, err := filepath.Abs(filepath.Join(run.ReviewReproductionDir(run.Dir(dataDir, origin)), "0", filepath.FromSlash(ReproductionPath)))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(ReproductionContent))
	return run.RecheckEntry{RunID: origin, HeldReproduction: run.HeldReproduction{
		Path: ReproductionPath, SHA256: hex.EncodeToString(sum[:]), HostPath: host, SpecLine: ReproductionSpecLine, TestIDs: []string{ReproductionTestID}, Runner: ReproductionRunner,
	}}
}

// ReviewQuarantinedBuildRunner returns a stub ticketRunner mimicking a
// ticket's first build that a reviewer's reproduction refused
// (ReproductionRefusedOn); extraFailedGate, if any, is recorded as failed
// too.
func ReviewQuarantinedBuildRunner(t *testing.T, dataDir, branch, baseSHA string, extraFailedGate string) requestdriver.TicketRunner {
	t.Helper()
	return func(ctx context.Context, args []string, onReady func(*run.Run)) error {
		ticket := ArgValue(args, "-ticket")
		if onReady != nil {
			onReady(&run.Run{ID: ticket})
		}
		var extra []string
		if extraFailedGate != "" {
			extra = append(extra, extraFailedGate)
		}
		return ReproductionRefusedOn(t, dataDir, ticket, branch, baseSHA, "2222222222222222222222222222222222222222", extra...).Save(dataDir)
	}
}
