package api

import (
	"reflect"
	"testing"

	"buildgate/internal/request"
	"buildgate/internal/run"
)

// TestComputeCostSummaryTokensRelayPreferred covers the console's "model id
// and tokens spent" headline: a ticket run with relay-consumed tokens must
// report them, attributed to the relay's own pinned model id, in
// preference to any AgentEvidence-derived figure.
func TestComputeCostSummaryTokensRelayPreferred(t *testing.T) {
	dataDir := t.TempDir()
	seedApprovableRequest(t, dataDir, "req-1", request.StateBuilding, true)
	r, err := request.Load(dataDir, "req-1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	r.SpecEvidence = &request.SpecEvidence{Usage: map[string]any{"totalTokens": 0.0}}
	r.PlanEvidence = &request.PlanEvidence{Usage: map[string]any{"totalTokens": 0.0}}
	r.Tickets = []request.Ticket{{Index: 1, RunID: "run-1"}}
	if err := r.Save(dataDir); err != nil {
		t.Fatalf("Save: %v", err)
	}
	seedRun(t, dataDir, run.Run{
		ID:        "run-1",
		RequestID: "req-1",
		State:     run.StateAccepted,
		Attempts: []run.Attempt{
			{Kind: "build", RelayWorkerModelID: "gpt-5.6-luna", RelayConsumedInputTokens: 1000, RelayConsumedOutputTokens: 500},
		},
		// AgentEvidence present too, to prove relay tokens win.
		AgentEvidence: &run.AgentEvidence{
			Rounds: []run.AgentEvidenceRound{{Index: 1, Usage: map[string]any{"totalTokens": 999999.0}}},
		},
	})

	s := NewServer(dataDir)
	req, err := request.Load(dataDir, "req-1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	cs := s.ComputeCostSummary(req)
	if !cs.TokensComplete {
		t.Fatalf("TokensComplete = false, want true")
	}
	if cs.Tokens != 1500 {
		t.Fatalf("Tokens = %d, want 1500", cs.Tokens)
	}
	want := []ModelUsage{{Model: "gpt-5.6-luna", Tokens: 1500}}
	if !reflect.DeepEqual(cs.ByModel, want) {
		t.Fatalf("ByModel = %+v, want %+v", cs.ByModel, want)
	}
}

// TestComputeCostSummaryTokensAgentEvidenceFallback covers a run with no
// relay-consumed tokens (an older evidence shape) falling back to the
// AgentEvidence rounds' own usage.totalTokens, attributed to the run's own
// RelayWorkerModelID when any attempt recorded one.
func TestComputeCostSummaryTokensAgentEvidenceFallback(t *testing.T) {
	dataDir := t.TempDir()
	seedApprovableRequest(t, dataDir, "req-1", request.StateBuilding, true)
	r, err := request.Load(dataDir, "req-1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	r.SpecEvidence = &request.SpecEvidence{Usage: map[string]any{"totalTokens": 0.0}}
	r.PlanEvidence = &request.PlanEvidence{Usage: map[string]any{"totalTokens": 0.0}}
	r.Tickets = []request.Ticket{{Index: 1, RunID: "run-1"}}
	if err := r.Save(dataDir); err != nil {
		t.Fatalf("Save: %v", err)
	}
	seedRun(t, dataDir, run.Run{
		ID:        "run-1",
		RequestID: "req-1",
		State:     run.StateAccepted,
		Attempts: []run.Attempt{
			{Kind: "build", RelayWorkerModelID: "gpt-5.6-luna"},
		},
		AgentEvidence: &run.AgentEvidence{
			Rounds: []run.AgentEvidenceRound{
				{Index: 1, Usage: map[string]any{"totalTokens": 300.0}},
				{Index: 2, Usage: map[string]any{"totalTokens": 200.0}},
			},
		},
	})

	s := NewServer(dataDir)
	req, err := request.Load(dataDir, "req-1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	cs := s.ComputeCostSummary(req)
	if !cs.TokensComplete {
		t.Fatalf("TokensComplete = false, want true")
	}
	if cs.Tokens != 500 {
		t.Fatalf("Tokens = %d, want 500", cs.Tokens)
	}
	want := []ModelUsage{{Model: "gpt-5.6-luna", Tokens: 500}}
	if !reflect.DeepEqual(cs.ByModel, want) {
		t.Fatalf("ByModel = %+v, want %+v", cs.ByModel, want)
	}
}

// TestComputeCostSummaryTokensCorrectiveRoundsCounted mirrors
// TestListRequestsCostSummaryIncludesCorrectiveRoundRuns for tokens: a
// corrective PR-review round's own run must contribute its tokens too.
func TestComputeCostSummaryTokensCorrectiveRoundsCounted(t *testing.T) {
	dataDir := t.TempDir()
	seedApprovableRequest(t, dataDir, "req-1", request.StateBuilding, true)
	r, err := request.Load(dataDir, "req-1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	r.Tickets = []request.Ticket{{
		Index: 1, RunID: "run-1",
		Rounds: []request.Round{{RunID: "run-2"}},
	}}
	if err := r.Save(dataDir); err != nil {
		t.Fatalf("Save: %v", err)
	}
	seedRun(t, dataDir, run.Run{
		ID: "run-1", RequestID: "req-1", State: run.StateAccepted,
		Attempts: []run.Attempt{{Kind: "build", RelayWorkerModelID: "model-a", RelayConsumedInputTokens: 100, RelayConsumedOutputTokens: 50}},
	})
	seedRun(t, dataDir, run.Run{
		ID: "run-2", RequestID: "req-1", State: run.StateAccepted,
		Attempts: []run.Attempt{{Kind: "build", RelayWorkerModelID: "model-b", RelayConsumedInputTokens: 10, RelayConsumedOutputTokens: 5}},
	})

	s := NewServer(dataDir)
	req, err := request.Load(dataDir, "req-1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	cs := s.ComputeCostSummary(req)
	if cs.Tokens != 165 {
		t.Fatalf("Tokens = %d, want 165", cs.Tokens)
	}
	want := []ModelUsage{{Model: "model-a", Tokens: 150}, {Model: "model-b", Tokens: 15}}
	if !reflect.DeepEqual(cs.ByModel, want) {
		t.Fatalf("ByModel = %+v, want %+v", cs.ByModel, want)
	}
}

// TestComputeCostSummaryTokensSkipsStartFailureRounds mirrors
// TestListRequestsCostSummaryExcludesStartFailureRounds for tokens: a round
// whose run never actually started is a genuine zero, not missing
// evidence, so it must not flip TokensComplete false.
func TestComputeCostSummaryTokensSkipsStartFailureRounds(t *testing.T) {
	dataDir := t.TempDir()
	seedApprovableRequest(t, dataDir, "req-1", request.StateBuilding, true)
	r, err := request.Load(dataDir, "req-1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	r.SpecEvidence = &request.SpecEvidence{Usage: map[string]any{"totalTokens": 0.0}}
	r.PlanEvidence = &request.PlanEvidence{Usage: map[string]any{"totalTokens": 0.0}}
	r.Tickets = []request.Ticket{{
		Index: 1, RunID: "run-1",
		Rounds: []request.Round{{StartFailure: true}},
	}}
	if err := r.Save(dataDir); err != nil {
		t.Fatalf("Save: %v", err)
	}
	seedRun(t, dataDir, run.Run{
		ID: "run-1", RequestID: "req-1", State: run.StateAccepted,
		Attempts: []run.Attempt{{Kind: "build", RelayWorkerModelID: "model-a", RelayConsumedInputTokens: 100, RelayConsumedOutputTokens: 50}},
	})

	s := NewServer(dataDir)
	req, err := request.Load(dataDir, "req-1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	cs := s.ComputeCostSummary(req)
	if !cs.TokensComplete {
		t.Fatalf("TokensComplete = false, want true (StartFailure round is a genuine zero)")
	}
	if cs.Tokens != 150 {
		t.Fatalf("Tokens = %d, want 150", cs.Tokens)
	}
}

// TestComputeCostSummaryTokensIncompleteWhenRunMissing mirrors
// TestListRequestsCostSummaryIncompleteWhenRunMissing for tokens.
func TestComputeCostSummaryTokensIncompleteWhenRunMissing(t *testing.T) {
	dataDir := t.TempDir()
	seedApprovableRequest(t, dataDir, "req-1", request.StateBuilding, true)
	r, err := request.Load(dataDir, "req-1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	r.Tickets = []request.Ticket{{Index: 1, RunID: "missing-run"}}
	if err := r.Save(dataDir); err != nil {
		t.Fatalf("Save: %v", err)
	}

	s := NewServer(dataDir)
	req, err := request.Load(dataDir, "req-1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	cs := s.ComputeCostSummary(req)
	if cs.TokensComplete {
		t.Fatalf("TokensComplete = true, want false (run record missing)")
	}
	if cs.Tokens != 0 {
		t.Fatalf("Tokens = %d, want 0", cs.Tokens)
	}
}

// TestComputeCostSummaryByModelDistinctOrdered covers multiple distinct
// models contributing tokens (spec drafting plus a build run) landing in
// ByModel in first-seen order, one entry per model.
func TestComputeCostSummaryByModelDistinctOrdered(t *testing.T) {
	dataDir := t.TempDir()
	seedApprovableRequest(t, dataDir, "req-1", request.StateBuilding, true)
	r, err := request.Load(dataDir, "req-1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	r.SpecEvidence = &request.SpecEvidence{Model: "model-a", Usage: map[string]any{"totalTokens": 40.0}}
	r.Tickets = []request.Ticket{{Index: 1, RunID: "run-1"}}
	if err := r.Save(dataDir); err != nil {
		t.Fatalf("Save: %v", err)
	}
	seedRun(t, dataDir, run.Run{
		ID: "run-1", RequestID: "req-1", State: run.StateAccepted,
		Attempts: []run.Attempt{{Kind: "build", RelayWorkerModelID: "model-b", RelayConsumedInputTokens: 10, RelayConsumedOutputTokens: 5}},
	})

	s := NewServer(dataDir)
	req, err := request.Load(dataDir, "req-1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	cs := s.ComputeCostSummary(req)
	want := []ModelUsage{{Model: "model-a", Tokens: 40}, {Model: "model-b", Tokens: 15}}
	if !reflect.DeepEqual(cs.ByModel, want) {
		t.Fatalf("ByModel = %+v, want %+v", cs.ByModel, want)
	}
}

// TestAddRunModelUsagePartialRelaySpendIsLowerBound covers a run whose relay
// crashed and whose spend was recovered from its usage ledger
// (run.Attempt.RelaySpendPartial): the tokens still count, but the rollup
// must report them as a lower bound, never a confirmed total.
func TestAddRunModelUsagePartialRelaySpendIsLowerBound(t *testing.T) {
	r := &run.Run{Attempts: []run.Attempt{
		{Kind: "build", RelayWorkerModelID: "gpt-5.6-luna", RelayConsumedInputTokens: 800, RelayConsumedOutputTokens: 200, RelaySpendPartial: true},
	}}
	acc := newModelUsageAccumulator()
	if complete := addRunModelUsage(r, acc); complete {
		t.Fatalf("addRunModelUsage complete = true, want false for partial relay spend")
	}
	want := []ModelUsage{{Model: "gpt-5.6-luna", Tokens: 1000}}
	if got := acc.finish(); !reflect.DeepEqual(got, want) {
		t.Fatalf("ByModel = %+v, want %+v", got, want)
	}
}

// TestRunViewForReportsTokensLowerBound covers the run detail route: the
// view must carry addRunModelUsage's completeness flag, so the console can
// render a crash-recovered run's tokens as "≥" there too, not only inside a
// request's cost_summary.
func TestRunViewForReportsTokensLowerBound(t *testing.T) {
	s := NewServer(t.TempDir())
	partial := s.runViewFor(&run.Run{ID: "r", State: run.StateAccepted, Attempts: []run.Attempt{
		{Kind: "build", RelayWorkerModelID: "gpt-5.6-luna", RelayConsumedInputTokens: 10, RelaySpendPartial: true},
	}})
	if partial.TokensComplete {
		t.Fatalf("TokensComplete = true, want false for partial relay spend")
	}
	confirmed := s.runViewFor(&run.Run{ID: "r", State: run.StateAccepted, Attempts: []run.Attempt{
		{Kind: "build", RelayWorkerModelID: "gpt-5.6-luna", RelayConsumedInputTokens: 10},
	}})
	if !confirmed.TokensComplete {
		t.Fatalf("TokensComplete = false, want true for confirmed relay spend")
	}
}

// TestAddRunModelUsageGroupsByRoleAndModel covers M3-C1's own grouping
// requirement: a run whose attempts span both an "execution" (build) and a
// "review" (spec_conformity) role, each with its own relay-consumed cost,
// must land in two distinct ByModel entries -- one per role -- rather than
// blending their spends into a single per-model total.
func TestAddRunModelUsageGroupsByRoleAndModel(t *testing.T) {
	r := &run.Run{Attempts: []run.Attempt{
		{Kind: "build", Role: "execution", RelayWorkerModelID: "gpt-5.6-luna", RelayConsumedInputTokens: 100, RelayConsumedOutputTokens: 50, RelayConsumedCostMicroUSD: 900},
		{Kind: "spec_conformity", Role: "review", RelayWorkerModelID: "gpt-5.6-luna", RelayConsumedInputTokens: 20, RelayConsumedOutputTokens: 10, RelayConsumedCostMicroUSD: 300},
	}}
	acc := newModelUsageAccumulator()
	if complete := addRunModelUsage(r, acc); !complete {
		t.Fatalf("addRunModelUsage complete = false, want true")
	}
	want := []ModelUsage{
		{Role: "execution", Model: "gpt-5.6-luna", Tokens: 150, CostMicroUSD: 900},
		{Role: "review", Model: "gpt-5.6-luna", Tokens: 30, CostMicroUSD: 300},
	}
	if got := acc.finish(); !reflect.DeepEqual(got, want) {
		t.Fatalf("ByModel = %+v, want %+v (sorted execution before review)", got, want)
	}
}

// TestComputeCostSummaryDraftingSpendGroupsByRoleAndNeverDoubleCounts
// covers two of M3-C1's decided-design requirements at once: (1) a
// SpecEvidence.Spend contributes its own role/model/tokens/cost to
// ByModel, exactly like a ticket build's own relay attempt does, and (2)
// when Spend exists, the older self-reported Usage["total_cost_usd"] for
// the SAME job must never also be counted -- cs.Spec is the Spend's own
// dollar figure alone, and ByModel gets exactly one entry for this job,
// not two.
func TestComputeCostSummaryDraftingSpendGroupsByRoleAndNeverDoubleCounts(t *testing.T) {
	dataDir := t.TempDir()
	seedApprovableRequest(t, dataDir, "req-1", request.StateBuilding, true)
	r, err := request.Load(dataDir, "req-1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	r.SpecEvidence = &request.SpecEvidence{
		// A self-reported Usage total_cost_usd/totalTokens alongside a
		// real Spend -- if ComputeCostSummary ever consulted both, this
		// job's cost/tokens would land in ByModel/cs.Spec twice.
		Usage: map[string]any{"totalTokens": 999999.0, "total_cost_usd": 999.0},
		Spend: &request.JobSpend{Role: "planning", Model: "gpt-5.6-luna", InputTokens: 100, OutputTokens: 50, CostMicroUSD: 1_500_000},
	}
	r.Tickets = nil
	if err := r.Save(dataDir); err != nil {
		t.Fatalf("Save: %v", err)
	}

	s := NewServer(dataDir)
	req, err := request.Load(dataDir, "req-1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	cs := s.ComputeCostSummary(req)
	if cs.Spec != 1.5 {
		t.Fatalf("Spec = %v, want 1.5 (Spend's own dollar figure, not Usage's 999.0)", cs.Spec)
	}
	if cs.Tokens != 150 {
		t.Fatalf("Tokens = %d, want 150 (Spend's own tokens, not Usage's 999999)", cs.Tokens)
	}
	want := []ModelUsage{{Role: "planning", Model: "gpt-5.6-luna", Tokens: 150, CostMicroUSD: 1_500_000}}
	if !reflect.DeepEqual(cs.ByModel, want) {
		t.Fatalf("ByModel = %+v, want exactly one entry %+v", cs.ByModel, want)
	}
}

// TestComputeCostSummaryByModelSortedByRoleThenModel covers finish's own
// deterministic ordering: entries observed in the reverse of role/model
// alphabetical order must still come out sorted role-then-model.
func TestComputeCostSummaryByModelSortedByRoleThenModel(t *testing.T) {
	acc := newModelUsageAccumulator()
	acc.add("review", "model-z", 1, 0)
	acc.add("planning", "model-b", 1, 0)
	acc.add("planning", "model-a", 1, 0)
	acc.add("", "model-unknown-role", 1, 0)
	got := acc.finish()
	want := []ModelUsage{
		{Model: "model-unknown-role", Tokens: 1},
		{Role: "planning", Model: "model-a", Tokens: 1},
		{Role: "planning", Model: "model-b", Tokens: 1},
		{Role: "review", Model: "model-z", Tokens: 1},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("finish() = %+v, want %+v (role \"\" sorts first, then role, then model)", got, want)
	}
}

// TestComputeCostSummaryAcceptedTicketsAndCostPerTicket covers
// AcceptedTickets/CostPerAcceptedTicketMicroUSD: two tickets, one accepted
// and one still quarantined, with a drafting spend and both tickets'
// build cost folded into the per-accepted-ticket figure.
func TestComputeCostSummaryAcceptedTicketsAndCostPerTicket(t *testing.T) {
	dataDir := t.TempDir()
	seedApprovableRequest(t, dataDir, "req-1", request.StateBuilding, true)
	r, err := request.Load(dataDir, "req-1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	r.SpecEvidence = &request.SpecEvidence{Spend: &request.JobSpend{Role: "planning", Model: "gpt-5.6-luna", CostMicroUSD: 500_000}}
	r.Tickets = []request.Ticket{
		{Index: 1, RunID: "run-accepted"},
		{Index: 2, RunID: "run-quarantined"},
	}
	if err := r.Save(dataDir); err != nil {
		t.Fatalf("Save: %v", err)
	}
	seedRun(t, dataDir, run.Run{
		ID: "run-accepted", RequestID: "req-1", State: run.StateAccepted,
		Attempts: []run.Attempt{{Kind: "build", Role: "execution", RelayWorkerModelID: "gpt-5.6-luna", RelayConsumedCostMicroUSD: 1_000_000}},
	})
	seedRun(t, dataDir, run.Run{
		ID: "run-quarantined", RequestID: "req-1", State: run.StateQuarantined,
		Attempts: []run.Attempt{{Kind: "build", Role: "execution", RelayWorkerModelID: "gpt-5.6-luna", RelayConsumedCostMicroUSD: 2_000_000}},
	})

	s := NewServer(dataDir)
	req, err := request.Load(dataDir, "req-1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	cs := s.ComputeCostSummary(req)
	if cs.AcceptedTickets != 1 {
		t.Fatalf("AcceptedTickets = %d, want 1 (only run-accepted reached run.StateAccepted)", cs.AcceptedTickets)
	}
	// Total = 0.5 (spec) + 1.0 + 2.0 (both tickets' runs, accepted or not) = 3.5
	wantMicro := int64(3_500_000)
	if cs.CostPerAcceptedTicketMicroUSD != wantMicro {
		t.Fatalf("CostPerAcceptedTicketMicroUSD = %d, want %d (Total incl. the quarantined ticket's own spend, / 1 accepted ticket)", cs.CostPerAcceptedTicketMicroUSD, wantMicro)
	}
}

// TestComputeCostSummaryCostPerAcceptedTicketZeroWhenNoneAccepted covers
// the "0 when none accepted" contract: an undefined per-ticket figure must
// never render as a divide-by-zero or a misleadingly real number.
func TestComputeCostSummaryCostPerAcceptedTicketZeroWhenNoneAccepted(t *testing.T) {
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
		ID: "run-1", RequestID: "req-1", State: run.StateQuarantined,
		Attempts: []run.Attempt{{Kind: "build", RelayWorkerModelID: "gpt-5.6-luna", RelayConsumedCostMicroUSD: 1_000_000}},
	})

	s := NewServer(dataDir)
	req, err := request.Load(dataDir, "req-1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	cs := s.ComputeCostSummary(req)
	if cs.AcceptedTickets != 0 {
		t.Fatalf("AcceptedTickets = %d, want 0", cs.AcceptedTickets)
	}
	if cs.CostPerAcceptedTicketMicroUSD != 0 {
		t.Fatalf("CostPerAcceptedTicketMicroUSD = %d, want 0", cs.CostPerAcceptedTicketMicroUSD)
	}
}
