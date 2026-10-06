package api

import (
	"testing"

	"buildgate/internal/request"
	"buildgate/internal/run"
)

// TestRequestDetailNamesACorrectiveRoundUnderWay: a ticket records a
// PR-review round when the round ends, so while one runs the request named
// nothing of it. The detail view finds the round's run by its request id
// and round ticket id.
func TestRequestDetailNamesACorrectiveRoundUnderWay(t *testing.T) {
	dataDir := t.TempDir()
	seedApprovableRequest(t, dataDir, "req-1", request.StatePRReview, true)
	r, err := request.Load(dataDir, "req-1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	r.Tickets = []request.Ticket{
		{Index: 1, RunID: "req-1-001-build", PRURL: "https://example.test/pull/1", PRState: "ready",
			Rounds: []request.Round{{Index: 1, RunID: "req-1-001-review1-a", Outcome: request.RoundQuarantined}}},
		{Index: 2, RunID: "req-1-002-build", PRURL: "https://example.test/pull/2", PRState: "ready"},
	}
	if err := r.Save(dataDir); err != nil {
		t.Fatalf("Save: %v", err)
	}
	seedRun(t, dataDir, run.Run{ID: "req-1-001-build", RequestID: "req-1", State: run.StateAccepted})
	// Round 1 ended and is recorded on the ticket; round 2 is running.
	seedRun(t, dataDir, run.Run{ID: "req-1-001-review1-a", RequestID: "req-1", State: run.StateQuarantined})
	seedRun(t, dataDir, run.Run{ID: "req-1-001-review2-b", RequestID: "req-1", State: run.StateSliceRunning})
	// Ticket 2: a finished round's run that the ticket has not recorded yet
	// is not "under way", and another request's run is not this one's.
	seedRun(t, dataDir, run.Run{ID: "req-1-002-review1-c", RequestID: "req-1", State: run.StateAccepted})
	seedRun(t, dataDir, run.Run{ID: "req-1-002-review1-d", RequestID: "req-2", State: run.StateSliceRunning})

	loaded, err := request.Load(dataDir, "req-1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	view := NewServer(dataDir).buildRequestDetailView(dataDir, "req-1", loaded)
	if got := view.Tickets[0].ActiveRoundRunID; got != "req-1-001-review2-b" {
		t.Errorf("ticket 1 ActiveRoundRunID = %q, want the running round's run", got)
	}
	if got := view.Tickets[1].ActiveRoundRunID; got != "" {
		t.Errorf("ticket 2 ActiveRoundRunID = %q, want none", got)
	}

	// Outside pr_review nothing is looked for.
	loaded.State = request.StateBuilding
	view = NewServer(dataDir).buildRequestDetailView(dataDir, "req-1", loaded)
	if got := view.Tickets[0].ActiveRoundRunID; got != "" {
		t.Errorf("building: ActiveRoundRunID = %q, want none", got)
	}
}
