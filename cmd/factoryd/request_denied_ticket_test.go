package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"buildgate/internal/release"
	"buildgate/internal/request"
	"buildgate/internal/requestdriver"
	"buildgate/internal/run"
)

// deniedThenAcceptingRunner records every ticket's run as accepted. Ticket
// 1's release decision is decision (nil records none) and it has no pull
// request; a later ticket is allowed and, when pull requests are on, has one.
func deniedThenAcceptingRunner(t *testing.T, dataDir, requestID string, decision *release.Decision, built *[]string) requestdriver.TicketRunner {
	return func(ctx context.Context, args []string, onReady func(*run.Run)) error {
		ticket := argValue(args, "-ticket")
		*built = append(*built, ticket)
		if onReady != nil {
			onReady(&run.Run{ID: ticket, State: run.StateReady})
		}
		rr := &run.Run{ID: ticket, Project: "app", State: run.StateAccepted, Branch: "factoryd/" + ticket, BaseSHA: strings.Repeat("1", 40), ResultSHA: strings.Repeat("2", 40)}
		recorded := &release.Decision{RunID: ticket, Project: "app", Allowed: true}
		if ticket == ticketRunID(requestID, 1) {
			recorded = decision
		} else if hasFlag(args, "-open-pull-request") {
			rr.PullRequestURL = "https://github.com/acme/app/pull/" + ticket
		}
		if err := rr.Save(dataDir); err != nil {
			t.Fatalf("save stub run %q: %v", ticket, err)
		}
		if recorded != nil {
			recorded.RunID = ticket
			if err := release.SaveDecision(dataDir, *recorded); err != nil {
				t.Fatalf("save decision of %q: %v", ticket, err)
			}
		}
		return nil
	}
}

// Ticket 2 is built on ticket 1's commit and its own changed files leave
// ticket 1's out, so its release and its pull request would carry a change
// the release policy refused. A ticket whose release decision is not allowed
// therefore halts the request before any later ticket is built, with pull
// requests on or off.
func TestTicketWithADeniedReleaseDecisionHaltsTheRequestBeforeTheNextTicket(t *testing.T) {
	denied := &release.Decision{Project: "app", Allowed: false, Reasons: []string{release.ReasonMemorySectionNotMemoryChange}}
	invalidated := &release.Decision{Project: "app", Allowed: false, Invalidated: true, Reasons: []string{"invalidated by a later run"}}
	cases := []struct {
		name     string
		openPR   bool
		decision *release.Decision
		reason   string
	}{
		{"pull requests on, denied", true, denied, release.ReasonMemorySectionNotMemoryChange},
		{"pull requests off, denied", false, denied, release.ReasonMemorySectionNotMemoryChange},
		{"pull requests on, invalidated", true, invalidated, "invalidated by a later run"},
		{"pull requests on, no decision recorded", true, nil, "no release decision is recorded"},
		{"pull requests off, no decision recorded", false, nil, "no release decision is recorded"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dp := newTestDeps(t)
			dataDir, id := buildingFixture(dp, t, 2)
			var built []string
			runner := deniedThenAcceptingRunner(t, dataDir, id, c.decision, &built)
			cfg := requestdriver.WorkerConfig{OpenPullRequest: c.openPR}
			for i := 0; i < 3; i++ {
				if err := driveRequests(dp, context.Background(), dataDir, cfg, failingSpecDraftRunner(t), failingPlanTicketsRunner(t), failingOracleDraftRunner(t), runner); err != nil {
					t.Fatalf("driveRequests (pass %d): %v", i+1, err)
				}
			}
			if len(built) != 1 || built[0] != ticketRunID(id, 1) {
				t.Fatalf("built %q, want only ticket 1: a later ticket would carry its refused change", built)
			}
			loaded, err := request.Load(dataDir, id)
			if err != nil {
				t.Fatal(err)
			}
			if loaded.State != request.StateHalted || !loaded.AwaitingPullRequest() || loaded.TicketIndex != 1 {
				t.Fatalf("state = %q halt kind = %q ticket index = %d, want halted as accepted with no pull request on ticket 1", loaded.State, loaded.HaltKind, loaded.TicketIndex)
			}
			if !strings.Contains(loaded.Error, c.reason) {
				t.Errorf("reason = %q, want it to name %q", loaded.Error, c.reason)
			}
			if tk := loaded.Tickets[1]; tk.RunID != "" || tk.PRURL != "" {
				t.Errorf("ticket 2 = %+v, want it never started", tk)
			}
		})
	}
}

// With an allowed decision and no pull request (pull requests off), the next
// ticket is built as before: nothing was refused.
func TestTicketWithAnAllowedReleaseDecisionAndNoPullRequestStillAdvances(t *testing.T) {
	dp := newTestDeps(t)
	dataDir, id := buildingFixture(dp, t, 2)
	var built []string
	runner := deniedThenAcceptingRunner(t, dataDir, id, &release.Decision{Project: "app", Allowed: true}, &built)
	for i := 0; i < 2; i++ {
		if err := driveRequests(dp, context.Background(), dataDir, requestdriver.WorkerConfig{}, failingSpecDraftRunner(t), failingPlanTicketsRunner(t), failingOracleDraftRunner(t), runner); err != nil {
			t.Fatalf("driveRequests (pass %d): %v", i+1, err)
		}
	}
	if len(built) != 2 {
		t.Fatalf("built %q, want both tickets", built)
	}
}

// The halt is recoverable: once the decision allows the run (the kill switch
// was released, say) a retry opens ticket 1's pull request, and the request
// goes on to build ticket 2 on it.
func TestRetryAfterADeniedTicketOpensItsPullRequestAndBuildsTheNextTicket(t *testing.T) {
	dp := newTestDeps(t)
	dataDir, id := buildingFixture(dp, t, 2)
	var built []string
	runner := deniedThenAcceptingRunner(t, dataDir, id, &release.Decision{Project: "app", Allowed: false, Reasons: []string{"project kill switch is engaged"}}, &built)
	cfg := requestdriver.WorkerConfig{OpenPullRequest: true}
	drive := func() {
		t.Helper()
		if err := driveRequests(dp, context.Background(), dataDir, cfg, failingSpecDraftRunner(t), failingPlanTicketsRunner(t), failingOracleDraftRunner(t), runner); err != nil {
			t.Fatalf("driveRequests: %v", err)
		}
	}
	drive()
	if loaded, err := request.Load(dataDir, id); err != nil || !loaded.AwaitingPullRequest() {
		t.Fatalf("request = %+v, %v, want halted awaiting ticket 1's pull request", loaded, err)
	}
	openPR := func(_, runID string) request.PROpenOutcome {
		return request.PROpenOutcome{PRURL: "https://github.com/acme/app/pull/" + runID}
	}
	if _, err := request.Retry(dataDir, id, "alice", "", time.Now(), openPR); err != nil {
		t.Fatalf("retry: %v", err)
	}
	drive() // pr_review: ticket 2 was never built, so building resumes
	drive() // ticket 2 builds
	loaded, err := request.Load(dataDir, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(built) != 2 || built[1] != ticketRunID(id, 2) || loaded.State != request.StatePRReview || loaded.TicketIndex != 2 {
		t.Fatalf("built %q, state %q, ticket index %d (%s), want ticket 2 built and the request in pr_review", built, loaded.State, loaded.TicketIndex, loaded.Error)
	}
	if tk := loaded.Tickets[1]; tk.PRURL == "" {
		t.Errorf("ticket 2 = %+v, want its pull request recorded", tk)
	}
}
