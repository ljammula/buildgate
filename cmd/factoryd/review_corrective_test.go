package main

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"buildgate/internal/request"
	"buildgate/internal/requestdriver"
	"buildgate/internal/requestdriver/requestdrivertest"
	"buildgate/internal/run"
)

// TestAdvanceBuildingReviewCorrectiveRoundQuarantinedAgainQuarantines
// covers the corrective round's own "quarantined/halted again -> the request quarantines as
// today" done-when item, and that Retry/SendBack keep working: ticket.
// Branch/PRURL stay empty (AnyTicketAccepted must stay false), and exactly
// one corrective round runs (the budget is consumed, not retried again in
// the same pass).
func TestAdvanceBuildingReviewCorrectiveRoundQuarantinedAgainQuarantines(t *testing.T) {
	dp := newTestDeps(t)
	dataDir, id := requestdrivertest.BuildingFixture(dp, t, 1)
	branch := "factoryd/" + id + "-001"
	baseSHA := fmt.Sprintf("%040d", 1)
	buildRunner := requestdrivertest.ReviewQuarantinedBuildRunner(t, dataDir, branch, baseSHA, "")

	calls, _ := requestdrivertest.StubReviewCorrectiveRunner(t, dataDir, func(dataDir, roundRunID string) *run.Run {
		return &run.Run{ID: roundRunID, State: run.StateQuarantined, HaltError: "gate failed: spec_conformity", Branch: branch, BaseSHA: baseSHA,
			GateResults: []run.GateResult{
				{Check: "canonical_verify", Passed: true},
				{Check: "spec_conformity", Passed: false},
			},
			SpecConformityVerdicts: []run.ReviewVerdict{{Criterion: "2. rejects a negative amount", Verdict: "flagged", Detail: "still not fixed"}},
		}
	})

	cfg := requestdriver.WorkerConfig{ReviewCorrectiveRounds: 1}
	if err := driveRequests(dp, context.Background(), dataDir, cfg, requestdrivertest.FailingSpecDraftRunner(t), requestdrivertest.FailingPlanTicketsRunner(t), requestdrivertest.FailingOracleDraftRunner(t), buildRunner); err != nil {
		t.Fatalf("driveRequests: %v", err)
	}
	if *calls != 1 {
		t.Fatalf("reviewCorrectiveRunner calls = %d, want exactly 1 (budget is 1 and must not be retried again in the same pass)", *calls)
	}

	loaded, err := request.Load(dataDir, id)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.State != request.StateQuarantined {
		t.Fatalf("State = %q, want %q", loaded.State, request.StateQuarantined)
	}
	// The exhausted-rounds message quotes the last round's own reason, so
	// the operator sees why without opening each run.
	if !strings.Contains(loaded.Error, "rounds exhausted") || !strings.Contains(loaded.Error, "gate failed: spec_conformity") {
		t.Errorf("Error = %q, want the exhausted message quoting the last round's reason", loaded.Error)
	}
	tk := loaded.Tickets[0]
	if tk.Branch != "" || tk.PRURL != "" {
		t.Errorf("Tickets[0] = %+v, want Branch/PRURL left empty (AnyTicketAccepted must stay false)", tk)
	}
	if loaded.AnyTicketAccepted() {
		t.Error("AnyTicketAccepted() = true, want false after a quarantined corrective round")
	}
	if len(tk.Rounds) != 1 || tk.Rounds[0].Kind != request.ConformityRoundKind || tk.Rounds[0].Outcome != request.RoundQuarantined {
		t.Fatalf("Tickets[0].Rounds = %+v, want one quarantined conformity round", tk.Rounds)
	}
	if tk.RunID != tk.Rounds[0].RunID {
		t.Errorf("Tickets[0].RunID = %q, want it repointed at the corrective round's own run %q", tk.RunID, tk.Rounds[0].RunID)
	}

	// Retry must still work against this quarantined request (the corrective round's own
	// "does not break Retry" guard).
	handled, err := retryRequest(dp, dataDir, loaded, "", time.Now())
	if !handled || err != nil {
		t.Fatalf("retryRequest: handled=%v err=%v, want handled=true err=nil", handled, err)
	}
}

// ---- code_review coverage (M2-D) ----------------------------------------
