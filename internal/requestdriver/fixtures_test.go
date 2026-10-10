package requestdriver_test

import (
	"context"
	"os"
	"testing"

	"buildgate/internal/request"
	"buildgate/internal/requestdriver"
	"buildgate/internal/run"
)

// stubSpecDraftRunner returns a fixed (specMD, evidence, err) for every
// call, recording how many times (and with which request) it was
// invoked -- the same "inject a stub instead of a real subprocess" shape
// worker_config_test.go's own stub ticketRunner uses for the worker.
func stubSpecDraftRunner(specMD string, evidence *request.SpecEvidence, err error) (requestdriver.SpecDraftRunner, *int) {
	calls := 0
	runner := func(ctx context.Context, dataDir string, r *request.Request, cfg requestdriver.WorkerConfig) (string, *request.SpecEvidence, error) {
		calls++
		return specMD, evidence, err
	}
	return runner, &calls
}

// failingSpecDraftRunner fails the test outright if ever called -- used
// where driveRequests must not touch the spec-drafting job at all (e.g. a
// request already past spec_drafting, or the pure submitted->
// spec_drafting move which needs no job).
func failingSpecDraftRunner(t *testing.T) requestdriver.SpecDraftRunner {
	return func(ctx context.Context, dataDir string, r *request.Request, cfg requestdriver.WorkerConfig) (string, *request.SpecEvidence, error) {
		t.Fatal("specDraftRunner must not be called")
		return "", nil, nil
	}
}

// failingPlanTicketsRunner is failingSpecDraftRunner's own sibling for
// the plan-drafting job -- used everywhere driveRequests must not touch
// planning at all.
func failingPlanTicketsRunner(t *testing.T) requestdriver.PlanTicketsRunner {
	return func(ctx context.Context, dataDir string, r *request.Request, cfg requestdriver.WorkerConfig, verifyCommand string) ([]requestdriver.DraftedTicket, *request.PlanEvidence, error) {
		t.Fatal("planTicketsRunner must not be called")
		return nil, nil, nil
	}
}

// failingOracleDraftRunner is failingSpecDraftRunner's sibling for the oracle
// drafting job -- used everywhere driveRequests must not touch
// oracle_drafting (every pre-existing test: no request there sets
// -draft-oracles).
func failingOracleDraftRunner(t *testing.T) requestdriver.OracleDraftRunner {
	return func(ctx context.Context, in requestdriver.OracleDraftInput) (request.OracleDraft, error) {
		t.Fatal("oracleDraftRunner must not be called")
		return request.OracleDraft{}, nil
	}
}

// failingBuildRunner is failingSpecDraftRunner's own sibling for a
// ticket build (ticketRunner) -- used everywhere driveRequests must not
// touch building at all.
func failingBuildRunner(t *testing.T) requestdriver.TicketRunner {
	return func(ctx context.Context, args []string, onReady func(*run.Run)) error {
		t.Fatal("ticketRunner (ticket build) must not be called")
		return nil
	}
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func containsFlag(args []string, name string) bool {
	for _, a := range args {
		if a == name {
			return true
		}
	}
	return false
}

func containsArg(args []string, name, value string) bool {
	for i, a := range args {
		if a == name && i+1 < len(args) && args[i+1] == value {
			return true
		}
	}
	return false
}
