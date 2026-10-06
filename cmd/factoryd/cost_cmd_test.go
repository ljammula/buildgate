package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"buildgate/internal/request"
	"buildgate/internal/run"
)

// buildCostFixtureDataDir writes a two-request fixture under a fresh
// t.TempDir(): req-1 has a spec-drafting Spend and one accepted, one
// quarantined ticket (plus one PR-review corrective round on the accepted
// ticket), and one spec_review rejection; req-2 (submitted a day later)
// has a plan-drafting Spend and one accepted ticket -- enough to exercise
// role/model grouping, AcceptedTickets, CostPerAcceptedTicketMicroUSD, the
// quarantined-ticket count, the rejected-spec count, and the -since
// filter, all in one fixture.
func buildCostFixtureDataDir(t *testing.T) string {
	t.Helper()
	dataDir := t.TempDir()

	req1 := request.New("req-1", "/repos/app", "app", request.Source{Kind: request.SourceText}, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	req1.SpecEvidence = &request.SpecEvidence{
		Spend: &request.JobSpend{Role: "planning", Model: "gpt-5.6-luna", InputTokens: 100, OutputTokens: 50, CostMicroUSD: 1_000_000},
	}
	req1.Rejections = []request.Rejection{
		{By: "operator", At: time.Now().UTC().Format(time.RFC3339), Reason: "redo scope", FromState: request.StateSpecReview},
	}
	req1.Tickets = []request.Ticket{
		{Index: 1, RunID: "run-1-accepted", Rounds: []request.Round{{Index: 1, RunID: "run-1-round1"}}},
		{Index: 2, RunID: "run-1-quarantined"},
	}
	if err := request.SaveText(dataDir, req1.ID, "text"); err != nil {
		t.Fatalf("SaveText req-1: %v", err)
	}
	if err := req1.Save(dataDir); err != nil {
		t.Fatalf("Save req-1: %v", err)
	}

	if err := (&run.Run{
		ID: "run-1-accepted", RequestID: "req-1", State: run.StateAccepted,
		Attempts: []run.Attempt{{Kind: "build", Role: "execution", RelayWorkerModelID: "gpt-5.6-luna", RelayConsumedInputTokens: 10, RelayConsumedOutputTokens: 5, RelayConsumedCostMicroUSD: 200_000}},
	}).Save(dataDir); err != nil {
		t.Fatalf("save run-1-accepted: %v", err)
	}
	if err := (&run.Run{
		ID: "run-1-round1", RequestID: "req-1", State: run.StateAccepted,
		Attempts: []run.Attempt{{Kind: "build", Role: "execution", RelayWorkerModelID: "gpt-5.6-luna", RelayConsumedCostMicroUSD: 100_000}},
	}).Save(dataDir); err != nil {
		t.Fatalf("save run-1-round1: %v", err)
	}
	if err := (&run.Run{
		ID: "run-1-quarantined", RequestID: "req-1", State: run.StateQuarantined,
		Attempts: []run.Attempt{{Kind: "build", Role: "execution", RelayWorkerModelID: "gpt-5.6-luna", RelayConsumedCostMicroUSD: 300_000}},
	}).Save(dataDir); err != nil {
		t.Fatalf("save run-1-quarantined: %v", err)
	}

	req2 := request.New("req-2", "/repos/app", "app", request.Source{Kind: request.SourceText}, time.Date(2026, 1, 3, 0, 0, 0, 0, time.UTC))
	req2.PlanEvidence = &request.PlanEvidence{
		Spend: &request.JobSpend{Role: "planning", Model: "model-b", InputTokens: 40, OutputTokens: 20, CostMicroUSD: 400_000},
	}
	req2.Tickets = []request.Ticket{{Index: 1, RunID: "run-2-accepted"}}
	if err := request.SaveText(dataDir, req2.ID, "text"); err != nil {
		t.Fatalf("SaveText req-2: %v", err)
	}
	if err := req2.Save(dataDir); err != nil {
		t.Fatalf("Save req-2: %v", err)
	}
	if err := (&run.Run{
		ID: "run-2-accepted", RequestID: "req-2", State: run.StateAccepted,
		Attempts: []run.Attempt{{Kind: "build", Role: "execution", RelayWorkerModelID: "model-b", RelayConsumedCostMicroUSD: 100_000}},
	}).Save(dataDir); err != nil {
		t.Fatalf("save run-2-accepted: %v", err)
	}

	return dataDir
}

// TestBuildCostReportAggregatesAcrossRequests covers the aggregate case
// (-request unset): both requests' own drafting Spend and every ticket
// run/round contribute, grouped by (role, model), with AcceptedTickets and
// CostPerAcceptedTicketMicroUSD computed across the whole fixture, and the
// human-cost proxy counts (quarantined tickets, rejected specs) correct.
func TestBuildCostReportAggregatesAcrossRequests(t *testing.T) {
	dataDir := buildCostFixtureDataDir(t)

	rep, err := buildCostReport(dataDir, "", "")
	if err != nil {
		t.Fatalf("buildCostReport: %v", err)
	}

	if rep.RequestCount != 2 {
		t.Errorf("RequestCount = %d, want 2", rep.RequestCount)
	}
	// req-1: spec 1.0 + runs (0.2 + 0.1 + 0.3) = 1.6; req-2: plan 0.4 + run 0.1 = 0.5; total 2.1
	if got, want := rep.Total, 2.1; got < want-1e-9 || got > want+1e-9 {
		t.Errorf("Total = %v, want %v", got, want)
	}
	if rep.AcceptedTickets != 2 {
		t.Errorf("AcceptedTickets = %d, want 2 (run-1-accepted, run-2-accepted)", rep.AcceptedTickets)
	}
	wantMicro := int64(2.1 * 1e6 / 2)
	if rep.CostPerAcceptedTicketMicroUSD != wantMicro {
		t.Errorf("CostPerAcceptedTicketMicroUSD = %d, want %d", rep.CostPerAcceptedTicketMicroUSD, wantMicro)
	}
	if rep.QuarantinedTickets != 1 {
		t.Errorf("QuarantinedTickets = %d, want 1", rep.QuarantinedTickets)
	}
	if rep.RejectedSpecs != 1 {
		t.Errorf("RejectedSpecs = %d, want 1", rep.RejectedSpecs)
	}
	if rep.RejectedPlans != 0 {
		t.Errorf("RejectedPlans = %d, want 0", rep.RejectedPlans)
	}

	// gpt-5.6-luna's own planning-role entry (spec drafting) and
	// execution-role entry (the three req-1 build/round runs) must stay
	// distinct -- exactly costModelKey's own (role, model) grouping.
	found := map[string]bool{}
	for i := range rep.ByModel {
		mu := rep.ByModel[i]
		found[mu.Role+"|"+mu.Model] = true
		switch {
		case mu.Role == "planning" && mu.Model == "gpt-5.6-luna":
			if mu.Tokens != 150 || mu.CostMicroUSD != 1_000_000 {
				t.Errorf("planning/gpt-5.6-luna = %+v, want tokens=150 cost=1000000", mu)
			}
		case mu.Role == "execution" && mu.Model == "gpt-5.6-luna":
			if mu.Tokens != 15 || mu.CostMicroUSD != 600_000 {
				t.Errorf("execution/gpt-5.6-luna = %+v, want tokens=15 cost=600000 (0.2+0.1+0.3)", mu)
			}
		case mu.Role == "planning" && mu.Model == "model-b":
			if mu.Tokens != 60 || mu.CostMicroUSD != 400_000 {
				t.Errorf("planning/model-b = %+v, want tokens=60 cost=400000", mu)
			}
		case mu.Role == "execution" && mu.Model == "model-b":
			if mu.Tokens != 0 || mu.CostMicroUSD != 100_000 {
				t.Errorf("execution/model-b = %+v, want tokens=0 cost=100000", mu)
			}
		}
	}
	for _, want := range []string{"planning|gpt-5.6-luna", "execution|gpt-5.6-luna", "planning|model-b", "execution|model-b"} {
		if !found[want] {
			t.Errorf("ByModel missing entry %q; got %+v", want, rep.ByModel)
		}
	}

	// Sorted by role then model: "execution" < "planning" alphabetically.
	for i := 1; i < len(rep.ByModel); i++ {
		prev, cur := rep.ByModel[i-1], rep.ByModel[i]
		if prev.Role > cur.Role || (prev.Role == cur.Role && prev.Model > cur.Model) {
			t.Errorf("ByModel not sorted at index %d: %+v then %+v", i, prev, cur)
		}
	}
}

// TestBuildCostReportSinceFilter covers -since: req-1 (submitted 2026-01-01)
// must be excluded by a 2026-01-02 cutoff, leaving only req-2.
func TestBuildCostReportSinceFilter(t *testing.T) {
	dataDir := buildCostFixtureDataDir(t)

	rep, err := buildCostReport(dataDir, "", "2026-01-02")
	if err != nil {
		t.Fatalf("buildCostReport: %v", err)
	}
	if rep.RequestCount != 1 {
		t.Fatalf("RequestCount = %d, want 1 (only req-2 submitted on/after 2026-01-02)", rep.RequestCount)
	}
	if rep.AcceptedTickets != 1 {
		t.Errorf("AcceptedTickets = %d, want 1", rep.AcceptedTickets)
	}
}

// TestBuildCostReportSingleRequest covers -request: only req-1's own
// figures, none of req-2's.
func TestBuildCostReportSingleRequest(t *testing.T) {
	dataDir := buildCostFixtureDataDir(t)

	rep, err := buildCostReport(dataDir, "req-1", "")
	if err != nil {
		t.Fatalf("buildCostReport: %v", err)
	}
	if rep.RequestCount != 1 {
		t.Fatalf("RequestCount = %d, want 1", rep.RequestCount)
	}
	if rep.AcceptedTickets != 1 {
		t.Errorf("AcceptedTickets = %d, want 1 (req-1's own run-1-accepted only)", rep.AcceptedTickets)
	}
	if rep.QuarantinedTickets != 1 {
		t.Errorf("QuarantinedTickets = %d, want 1", rep.QuarantinedTickets)
	}
}

// TestPrintCostReportText covers the plain-text table rendering: role and
// model columns, a dollar-formatted cost, and the summary lines including
// the human-cost proxy counts.
func TestPrintCostReportText(t *testing.T) {
	dataDir := buildCostFixtureDataDir(t)
	rep, err := buildCostReport(dataDir, "", "")
	if err != nil {
		t.Fatalf("buildCostReport: %v", err)
	}
	var buf bytes.Buffer
	printCostReport(&buf, rep)
	out := buf.String()
	for _, want := range []string{
		"ROLE", "MODEL", "TOKENS", "COST",
		"execution", "planning", "gpt-5.6-luna", "model-b",
		"accepted tickets:    2",
		"quarantined tickets: 1",
		"rejected specs:      1",
		"rejected plans:      0",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("printCostReport output missing %q; got:\n%s", want, out)
		}
	}
}

// TestPrintCostReportTextNoAcceptedTickets covers the "0 accepted" text
// rendering: cost/accepted-ticket must say "n/a", never a divide-by-zero
// or a misleadingly real dollar figure.
func TestPrintCostReportTextNoAcceptedTickets(t *testing.T) {
	rep := &costReport{Complete: true, TokensComplete: true, RequestCount: 1}
	var buf bytes.Buffer
	printCostReport(&buf, rep)
	if !strings.Contains(buf.String(), "n/a (no accepted tickets)") {
		t.Errorf("printCostReport with 0 accepted tickets = %q, want it to say n/a", buf.String())
	}
}

// TestCostReportJSONRoundTrips covers -json: the report must encode with
// the documented snake_case field names and decode back losslessly.
func TestCostReportJSONRoundTrips(t *testing.T) {
	dataDir := buildCostFixtureDataDir(t)
	rep, err := buildCostReport(dataDir, "", "")
	if err != nil {
		t.Fatalf("buildCostReport: %v", err)
	}
	b, err := json.Marshal(rep)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	for _, want := range []string{`"accepted_tickets"`, `"cost_per_accepted_ticket_micro_usd"`, `"quarantined_tickets"`, `"rejected_specs"`, `"rejected_plans"`, `"by_model"`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("JSON missing key %q; got %s", want, b)
		}
	}
	var decoded costReport
	if err := json.Unmarshal(b, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if decoded.AcceptedTickets != rep.AcceptedTickets || decoded.CostPerAcceptedTicketMicroUSD != rep.CostPerAcceptedTicketMicroUSD {
		t.Errorf("round-tripped report = %+v, want it to match %+v", decoded, rep)
	}
}
