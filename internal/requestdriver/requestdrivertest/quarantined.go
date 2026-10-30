package requestdrivertest

import (
	"context"
	"testing"

	"buildgate/internal/handoff"
	"buildgate/internal/requestdriver"
	"buildgate/internal/run"
)

// CorrectableQuarantine builds a quarantined run a corrective build may follow:
// the changed tests pass on the base commit (tests_red_on_base failed, a check
// whose bin is corrective), with the handoff written and hashed as the save in
// cmd/factoryd leaves it. extraFailedChecks are recorded as failed too.
func CorrectableQuarantine(t *testing.T, dataDir, id, branch, baseSHA, resultSHA string, extraFailedChecks ...string) *run.Run {
	t.Helper()
	rr := &run.Run{
		ID: id, Ticket: id, State: run.StateQuarantined, Branch: branch, BaseSHA: baseSHA, ResultSHA: resultSHA,
		ChangedFiles: []string{"internal/sum/sum.go"},
		GateResults:  []run.GateResult{{Check: "canonical_verify", Passed: true}, {Check: "diff_scope", Passed: true}},
		CodeReview:   &run.CodeReviewResult{Policy: "required", Available: true},
	}
	for _, check := range extraFailedChecks {
		rr.GateResults = append(rr.GateResults, run.GateResult{Check: check, Passed: false, ExitCode: 1})
	}
	rr.GateResults = append(rr.GateResults, run.GateResult{Check: "tests_red_on_base", Passed: false, ExitCode: 1})
	if err := handoff.Sync(rr, dataDir); err != nil {
		t.Fatalf("write the handoff: %v", err)
	}
	return rr
}

// ReviewQuarantinedBuildRunner returns a stub ticketRunner mimicking a
// ticket's first build that a corrective-bin check refused
// (CorrectableQuarantine); extraFailedGate, if any, is recorded as failed too.
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
		return CorrectableQuarantine(t, dataDir, ticket, branch, baseSHA, "2222222222222222222222222222222222222222", extra...).Save(dataDir)
	}
}
