package api

import (
	"testing"

	"buildgate/internal/request"
	"buildgate/internal/run"
)

// TestComputeCostSummaryCountsSupersededRetryAndCorrectiveRunsViaRequestID
// is a regression test for the live undercount found in the Flutter + Go app repo M-E1
// run (2026-09-28, request
// feature-habit-insights-endpoint-and-mcp-20260928-082441): a ticket
// retried after a quarantined first build left that first build's run
// unreachable from Ticket.RunID (overwritten by the retry) or
// Ticket.Rounds (which only ever records corrective rounds, never a full
// retry), so ComputeCostSummary silently dropped its cost. Every run a
// ticket build/retry, a spec_conformity corrective round, or a
// PR-review corrective round starts sets RequestID at that same start
// point (request_driver.go, pr_review_driver.go) -- this fixture mirrors
// that by seeding all three runs with RequestID set and only the
// retried build reachable via Ticket.RunID, matching what a real retry
// leaves on disk.
func TestComputeCostSummaryCountsSupersededRetryAndCorrectiveRunsViaRequestID(t *testing.T) {
	dataDir := t.TempDir()
	seedApprovableRequest(t, dataDir, "req-1", request.StateBuilding, true)
	r, err := request.Load(dataDir, "req-1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	r.SpecEvidence = &request.SpecEvidence{Usage: map[string]any{"total_cost_usd": 0.0}}
	r.PlanEvidence = &request.PlanEvidence{Usage: map[string]any{"total_cost_usd": 0.0}}
	r.Tickets = []request.Ticket{
		{
			Index: 1,
			// Only the retry's run id survives here -- the same way a
			// real retryRequest rebuilds a quarantined ticket without
			// clearing its previous RunID first, then immediately
			// overwrites it once the new build starts (request_driver.go's
			// own onReady callback).
			RunID: "run-retry",
			Rounds: []request.Round{
				{Index: 1, Kind: request.ConformityRoundKind, RunID: "run-corrective"},
			},
		},
	}
	if err := r.Save(dataDir); err != nil {
		t.Fatalf("Save: %v", err)
	}
	seedRun(t, dataDir, run.Run{
		ID: "run-first", RequestID: "req-1", State: run.StateQuarantined,
		Attempts: []run.Attempt{{Kind: "build", RelayWorkerModelID: "gpt-5.6-luna", RelayConsumedCostMicroUSD: 1_040_000}},
	})
	seedRun(t, dataDir, run.Run{
		ID: "run-retry", RequestID: "req-1", State: run.StateQuarantined,
		Attempts: []run.Attempt{{Kind: "build", RelayWorkerModelID: "gpt-5.6-luna", RelayConsumedCostMicroUSD: 6_420_000}},
	})
	seedRun(t, dataDir, run.Run{
		ID: "run-corrective", RequestID: "req-1", State: run.StateAccepted,
		Attempts: []run.Attempt{{Kind: "spec_conformity", Role: "review", RelayWorkerModelID: "gpt-5.6-luna", RelayConsumedCostMicroUSD: 220_000}},
	})

	s := NewServer(dataDir)
	req, err := request.Load(dataDir, "req-1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	cs := s.ComputeCostSummary(req)
	// 1.04 (superseded first build, unreachable from Ticket.RunID/Rounds)
	// + 6.42 (the retry, Ticket.RunID's own run) + 0.22 (corrective round)
	if got, want := cs.Runs, 1.04+6.42+0.22; got < want-1e-9 || got > want+1e-9 {
		t.Errorf("Runs = %v, want %v (superseded first build + retry + corrective round, all three)", got, want)
	}
	if !cs.Complete {
		t.Errorf("Complete = false, want true")
	}
}

// TestComputeCostSummaryExcludesOtherRequestsRuns covers the flip side of
// the RequestID scan: a run tagged with a DIFFERENT request's id must
// never bleed into this request's own Runs total, even though both runs
// live in the same dataDir/runs scan.
func TestComputeCostSummaryExcludesOtherRequestsRuns(t *testing.T) {
	dataDir := t.TempDir()
	seedApprovableRequest(t, dataDir, "req-1", request.StateBuilding, true)
	r, err := request.Load(dataDir, "req-1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	r.Tickets = []request.Ticket{{Index: 1, RunID: "run-1"}}
	if err := r.Save(dataDir); err != nil {
		t.Fatalf("Save: %v", err)
	}
	seedRun(t, dataDir, run.Run{
		ID: "run-1", RequestID: "req-1", State: run.StateAccepted,
		Attempts: []run.Attempt{{Kind: "build", RelayWorkerModelID: "gpt-5.6-luna", RelayConsumedCostMicroUSD: 1_000_000}},
	})
	// A run belonging to an entirely different request -- must not
	// contribute to req-1's own rollup.
	seedRun(t, dataDir, run.Run{
		ID: "run-other-request", RequestID: "req-2", State: run.StateAccepted,
		Attempts: []run.Attempt{{Kind: "build", RelayWorkerModelID: "gpt-5.6-luna", RelayConsumedCostMicroUSD: 9_000_000}},
	})

	s := NewServer(dataDir)
	req, err := request.Load(dataDir, "req-1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	cs := s.ComputeCostSummary(req)
	if cs.Runs != 1.0 {
		t.Errorf("Runs = %v, want 1.0 (req-2's own run must not be counted)", cs.Runs)
	}
}

// TestComputeCostSummaryNoDoubleCountWhenRunFoundBothWays covers the
// dedup contract: a run reachable both as the ticket's own Ticket.RunID
// AND (redundantly) as one of its Ticket.Rounds[].RunID entries must be
// counted exactly once, not twice -- the scan finds it once (one file),
// and the ticket loop's own byID lookup must recognize it as already
// seen rather than adding it a second time.
func TestComputeCostSummaryNoDoubleCountWhenRunFoundBothWays(t *testing.T) {
	dataDir := t.TempDir()
	seedApprovableRequest(t, dataDir, "req-1", request.StateBuilding, true)
	r, err := request.Load(dataDir, "req-1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	r.Tickets = []request.Ticket{{
		Index: 1,
		RunID: "run-1",
		// Same run id redundantly present in Rounds too -- not a shape
		// the real driver code produces, but the rollup must not double
		// count it regardless.
		Rounds: []request.Round{{Index: 1, RunID: "run-1"}},
	}}
	if err := r.Save(dataDir); err != nil {
		t.Fatalf("Save: %v", err)
	}
	seedRun(t, dataDir, run.Run{
		ID: "run-1", RequestID: "req-1", State: run.StateAccepted,
		Attempts: []run.Attempt{{Kind: "build", RelayWorkerModelID: "gpt-5.6-luna", RelayConsumedCostMicroUSD: 1_000_000}},
	})

	s := NewServer(dataDir)
	req, err := request.Load(dataDir, "req-1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	cs := s.ComputeCostSummary(req)
	if cs.Runs != 1.0 {
		t.Errorf("Runs = %v, want 1.0 (run-1 counted once, not twice)", cs.Runs)
	}
}

// A spec and plan the operator handed over, never revised by a model, cost
// nothing: the rollup is an exact zero, not "at least zero". Once a
// rejection has had the model revise the spec, its missing evidence is a
// gap again.
func TestComputeCostSummaryHandedOverStagesAreAnExactZero(t *testing.T) {
	dataDir := t.TempDir()
	seedApprovableRequest(t, dataDir, "req-1", request.StatePlanReview, true)
	r, err := request.Load(dataDir, "req-1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	r.SpecEvidence, r.PlanEvidence = nil, nil
	s := NewServer(dataDir)

	if cs := s.ComputeCostSummary(r); cs.Complete || cs.TokensComplete {
		t.Fatalf("a drafted request with no evidence: Complete = %v, TokensComplete = %v; want both false", cs.Complete, cs.TokensComplete)
	}
	r.SpecImported, r.PlanImported = true, true
	if cs := s.ComputeCostSummary(r); !cs.Complete || !cs.TokensComplete || cs.Spec != 0 || cs.Plan != 0 {
		t.Fatalf("handed over: %+v; want complete and zero", cs)
	}
	r.Rejections = append(r.Rejections, request.Rejection{By: "op", At: "2026-10-04T00:00:00Z", Reason: "revise", FromState: request.StateSpecReview})
	if cs := s.ComputeCostSummary(r); cs.Complete {
		t.Fatalf("handed over, then revised by the model with no evidence: Complete = true; want false")
	}
}
