package requestdrivertest

import (
	"context"
	"testing"

	"buildgate/internal/request"
	"buildgate/internal/requestdriver"
	"buildgate/internal/run"
)

// FailingSpecDraftRunner fails the test outright if ever called -- used
// where driveRequests must not touch the spec-drafting job at all (e.g. a
// request already past spec_drafting, or the pure submitted->
// spec_drafting move which needs no job).
func FailingSpecDraftRunner(t *testing.T) requestdriver.SpecDraftRunner {
	return func(ctx context.Context, dataDir string, r *request.Request, cfg requestdriver.WorkerConfig) (string, *request.SpecEvidence, error) {
		t.Fatal("specDraftRunner must not be called")
		return "", nil, nil
	}
}

// FailingPlanTicketsRunner is FailingSpecDraftRunner's own sibling for
// the plan-drafting job -- used everywhere driveRequests must not touch
// planning at all.
func FailingPlanTicketsRunner(t *testing.T) requestdriver.PlanTicketsRunner {
	return func(ctx context.Context, dataDir string, r *request.Request, cfg requestdriver.WorkerConfig, verifyCommand string) ([]requestdriver.DraftedTicket, *request.PlanEvidence, error) {
		t.Fatal("planTicketsRunner must not be called")
		return nil, nil, nil
	}
}

// FailingOracleDraftRunner is FailingSpecDraftRunner's sibling for the oracle
// drafting job -- used everywhere driveRequests must not touch
// oracle_drafting (every pre-existing test: no request there sets
// -draft-oracles).
func FailingOracleDraftRunner(t *testing.T) requestdriver.OracleDraftRunner {
	return func(ctx context.Context, in requestdriver.OracleDraftInput) (request.OracleDraft, error) {
		t.Fatal("oracleDraftRunner must not be called")
		return request.OracleDraft{}, nil
	}
}

// FailingBuildRunner is FailingSpecDraftRunner's own sibling for a
// ticket build (ticketRunner) -- used everywhere driveRequests must not
// touch building at all.
func FailingBuildRunner(t *testing.T) requestdriver.TicketRunner {
	return func(ctx context.Context, args []string, onReady func(*run.Run)) error {
		t.Fatal("ticketRunner (ticket build) must not be called")
		return nil
	}
}

// ArgValue returns the value following flag in args, or "" if flag is
// absent or has no following value.
func ArgValue(args []string, flag string) string {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

// HasFlag reports whether the bare flag (a boolean flag with no value,
// e.g. -open-pull-request) is present in args.
func HasFlag(args []string, flag string) bool {
	for _, a := range args {
		if a == flag {
			return true
		}
	}
	return false
}
