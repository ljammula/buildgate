package requestdriver_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"buildgate/internal/request"
	"buildgate/internal/requestdriver"
	"buildgate/internal/requestdriver/requestdrivertest"
	"buildgate/internal/run"
)

// TestAdvanceBuildingSetsQuarantineCheckDiffScope covers advanceBuilding's
// own StateQuarantined case (request_driver.go): a run that quarantines
// with diff_scope as its only failed gate (no spec_conformity/code_review
// alongside it, so reviewShapeOnly is false) must set
// request.QuarantineCheckDiffScope, so NextAction leads with
// `factoryd amend-scope` instead of a generic "fix the cause, then retry".
func TestAdvanceBuildingSetsQuarantineCheckDiffScope(t *testing.T) {
	dp := newFakeDeps(t)
	dataDir, id := requestdrivertest.BuildingFixture(dp, t, 1)
	buildRunner := func(ctx context.Context, args []string, onReady func(*run.Run)) error {
		ticket := requestdrivertest.ArgValue(args, "-ticket")
		if onReady != nil {
			onReady(&run.Run{ID: ticket})
		}
		rr := &run.Run{
			ID:        ticket,
			State:     run.StateQuarantined,
			HaltError: "gate failed: diff_scope",
			Branch:    "factoryd/" + id + "-001",
			BaseSHA:   fmt.Sprintf("%040d", 1),
			GateResults: []run.GateResult{
				{Check: "canonical_verify", Passed: true},
				{Check: "diff_scope", Passed: false},
			},
		}
		return rr.Save(dataDir)
	}

	if err := requestdrivertest.DriveRequests(dp, context.Background(), dataDir, requestdriver.WorkerConfig{}, requestdrivertest.FailingSpecDraftRunner(t), requestdrivertest.FailingPlanTicketsRunner(t), requestdrivertest.FailingOracleDraftRunner(t), buildRunner); err != nil {
		t.Fatalf("driveRequests: %v", err)
	}
	loaded, err := request.Load(dataDir, id)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.State != request.StateQuarantined {
		t.Fatalf("State = %q, want %q", loaded.State, request.StateQuarantined)
	}
	if loaded.QuarantineCheck != request.QuarantineCheckDiffScope {
		t.Errorf("QuarantineCheck = %q, want %q", loaded.QuarantineCheck, request.QuarantineCheckDiffScope)
	}
	if next := loaded.NextAction(); !strings.Contains(next, "factoryd amend-scope") {
		t.Errorf("NextAction() = %q, want it to name `factoryd amend-scope`", next)
	}
}

// Every build that follows an earlier run of a request carries the commit the
// request started from as -instruction-base, so a review does not take an
// earlier build's unmerged instruction text as genuine: ticket N>1 gets the
// value recorded on ticket 1's run, ticket 1 the value recorded on its own
// earlier run. A ticket's very first build carries none.
func TestStackedTicketsCarryTheInstructionBase(t *testing.T) {
	dp := newFakeDeps(t)
	dataDir, id := requestdrivertest.BuildingFixture(dp, t, 2)
	r, err := request.Load(dataDir, id)
	if err != nil {
		t.Fatal(err)
	}
	ticketOneBase, ticketOneDiffBase := fmt.Sprintf("%040d", 1), fmt.Sprintf("%040d", 3)
	cfg := requestdriver.WorkerConfig{ReviewCorrectiveRounds: 1}
	argsOf := func(ticket request.Ticket) (first, corrective []string) {
		t.Helper()
		first, err := requestdriver.BuildRequestBuildArgs(dataDir, r, ticket, cfg)
		if err != nil {
			t.Fatalf("BuildRequestBuildArgs ticket %d: %v", ticket.Index, err)
		}
		corrective, err = requestdriver.BuildCorrectiveArgs(dataDir, r, ticket, cfg, ticket.SpecPath, "round-1", "some-branch", fmt.Sprintf("%040d", 2))
		if err != nil {
			t.Fatalf("BuildCorrectiveArgs ticket %d: %v", ticket.Index, err)
		}
		return first, corrective
	}

	// Ticket 1's very first build has no earlier run: nothing to pass.
	if first, corrective := argsOf(r.Tickets[0]); requestdrivertest.HasFlag(first, "-instruction-base") || requestdrivertest.HasFlag(corrective, "-instruction-base") {
		t.Errorf("ticket 1 before any run carries -instruction-base: %v / %v", first, corrective)
	}

	ticketOne := &run.Run{ID: requestdrivertest.TicketRunID(id, 1), BaseSHA: ticketOneBase, InstructionBaseSHA: ticketOneBase}
	if err := ticketOne.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	r.Tickets[0].RunID = ticketOne.ID
	for name, ticket := range map[string]request.Ticket{"ticket 1 (a retry)": r.Tickets[0], "ticket 2": r.Tickets[1]} {
		first, corrective := argsOf(ticket)
		for what, args := range map[string][]string{"first build and retry": first, "corrective round": corrective} {
			if got := requestdrivertest.ArgValue(args, "-instruction-base"); got != ticketOneBase {
				t.Errorf("%s %s: -instruction-base = %q, want ticket 1's base %q", name, what, got, ticketOneBase)
			}
		}
	}

	// A record from before the field existed: its diff base outranks its base.
	ticketOne.InstructionBaseSHA, ticketOne.DiffBaseSHA = "", ticketOneDiffBase
	if err := ticketOne.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	if first, _ := argsOf(r.Tickets[1]); requestdrivertest.ArgValue(first, "-instruction-base") != ticketOneDiffBase {
		t.Errorf("-instruction-base = %q, want ticket 1's diff base %q", requestdrivertest.ArgValue(first, "-instruction-base"), ticketOneDiffBase)
	}
	ticketOne.InstructionBaseSHA = fmt.Sprintf("%040d", 4)
	if err := ticketOne.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	if first, _ := argsOf(r.Tickets[1]); requestdrivertest.ArgValue(first, "-instruction-base") != ticketOne.InstructionBaseSHA {
		t.Errorf("-instruction-base = %q, want ticket 1's recorded instruction base", requestdrivertest.ArgValue(first, "-instruction-base"))
	}
}

// An earlier run that cannot be loaded or records no base halts the build
// with a reason, instead of reviewing against the diff base.
func TestInstructionBaseUnknownRefusesTheBuild(t *testing.T) {
	dp := newFakeDeps(t)
	dataDir, id := requestdrivertest.BuildingFixture(dp, t, 2)
	r, err := request.Load(dataDir, id)
	if err != nil {
		t.Fatal(err)
	}
	cfg := requestdriver.WorkerConfig{ReviewCorrectiveRounds: 1}
	want := func(ticket request.Ticket, runID string) {
		t.Helper()
		_, err := requestdriver.BuildRequestBuildArgs(dataDir, r, ticket, cfg)
		prefix := fmt.Sprintf("ticket %d: cannot determine the commit the request started from (run %s ", ticket.Index, runID)
		if err == nil || !strings.Contains(err.Error(), prefix) || !strings.Contains(err.Error(), "a review would trust an earlier build's instruction files") {
			t.Errorf("ticket %d: err = %v, want a refusal beginning %q", ticket.Index, err, prefix)
		}
	}
	// Ticket 2 with no ticket 1 run, and with one that cannot be loaded.
	want(r.Tickets[1], "")
	r.Tickets[0].RunID = "no-such-run"
	want(r.Tickets[1], "no-such-run")
	// Ticket 1's own earlier run, unloadable.
	want(r.Tickets[0], "no-such-run")
	// A run that records no base at all.
	empty := &run.Run{ID: requestdrivertest.TicketRunID(id, 1)}
	if err := empty.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	r.Tickets[0].RunID = empty.ID
	want(r.Tickets[1], empty.ID)
	// Ticket 1's own earlier run with a result but no base: something was
	// built, and what it was built on is unknown. (With no result either
	// it built nothing: TestARetryAfterARunThatRecordedNoBaseIsAFirstBuild.)
	empty.ResultSHA = fmt.Sprintf("%040d", 7)
	if err := empty.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	want(r.Tickets[0], empty.ID)
}

// TestARetryAfterARunThatRecordedNoBaseIsAFirstBuild: a ticket's run that
// halted before it recorded the commit it started from built nothing, so
// the build that follows it is given no -instruction-base (its own base is
// the request's) instead of the request being left with no retry that
// works. A later ticket still refuses: its base is an earlier build's output.
func TestARetryAfterARunThatRecordedNoBaseIsAFirstBuild(t *testing.T) {
	dp := newFakeDeps(t)
	dataDir, id := requestdrivertest.BuildingFixture(dp, t, 2)
	r, err := request.Load(dataDir, id)
	if err != nil {
		t.Fatal(err)
	}
	halted := &run.Run{ID: requestdrivertest.TicketRunID(id, 1), State: run.StateHalted}
	if err := halted.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	r.Tickets[0].RunID = halted.ID
	args, err := requestdriver.BuildRequestBuildArgs(dataDir, r, r.Tickets[0], requestdriver.WorkerConfig{})
	if err != nil || requestdrivertest.HasFlag(args, "-instruction-base") {
		t.Fatalf("ticket 1 after a run with no base: err %v, args carry -instruction-base: %v; want a plain first build", err, requestdrivertest.HasFlag(args, "-instruction-base"))
	}
	if _, err := requestdriver.BuildRequestBuildArgs(dataDir, r, r.Tickets[1], requestdriver.WorkerConfig{}); err == nil || !strings.Contains(err.Error(), "cannot determine the commit the request started from") {
		t.Fatalf("ticket 2 with a base-less ticket 1 run: err = %v, want the refusal", err)
	}
	// A run that recorded a result but no base is not "nothing built".
	halted.ResultSHA = fmt.Sprintf("%040d", 7)
	if err := halted.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	if _, err := requestdriver.BuildRequestBuildArgs(dataDir, r, r.Tickets[0], requestdriver.WorkerConfig{}); err == nil {
		t.Fatal("ticket 1 after a run with a result and no base built without a refusal")
	}
}
