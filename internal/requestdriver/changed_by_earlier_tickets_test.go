package requestdriver_test

import (
	"fmt"
	"strings"
	"testing"

	"buildgate/internal/request"
	"buildgate/internal/requestdriver"
	"buildgate/internal/requestdriver/requestdrivertest"
	"buildgate/internal/run"
)

// A later ticket's build carries, as -changed-by-earlier-tickets, the files
// among its Required-Changed-Files that an earlier ticket's run changed, on
// its first build and on a corrective round alike; ticket 1 never does, and
// a file no earlier ticket changed is not named.
func TestLaterTicketsCarryTheRequiredFilesEarlierTicketsChanged(t *testing.T) {
	dp := newFakeDeps(t)
	dataDir, id := requestdrivertest.BuildingFixture(dp, t, 3)
	r, err := request.Load(dataDir, id)
	if err != nil {
		t.Fatal(err)
	}
	cfg := requestdriver.WorkerConfig{ReviewCorrectiveRounds: 1}
	const required = "internal/payments/refunds.go"
	base := fmt.Sprintf("%040d", 1)
	argsOf := func(ticket request.Ticket) map[string][]string {
		t.Helper()
		first, err := requestdriver.BuildRequestBuildArgs(dataDir, r, ticket, cfg)
		if err != nil {
			t.Fatalf("BuildRequestBuildArgs ticket %d: %v", ticket.Index, err)
		}
		corrective, err := requestdriver.BuildCorrectiveArgs(dataDir, r, ticket, cfg, ticket.SpecPath, "round-1", "some-branch", fmt.Sprintf("%040d", 2))
		if err != nil {
			t.Fatalf("BuildCorrectiveArgs ticket %d: %v", ticket.Index, err)
		}
		return map[string][]string{"first build": first, "corrective round": corrective}
	}
	saveRun := func(index int, changed ...string) {
		t.Helper()
		rr := &run.Run{ID: requestdrivertest.TicketRunID(id, index), BaseSHA: base, InstructionBaseSHA: base, ChangedFiles: changed}
		if err := rr.Save(dataDir); err != nil {
			t.Fatal(err)
		}
		r.Tickets[index-1].RunID = rr.ID
	}
	wantFlag := func(ticket request.Ticket, want string) {
		t.Helper()
		for what, args := range argsOf(ticket) {
			if got := requestdrivertest.ArgValue(args, "-changed-by-earlier-tickets"); got != want {
				t.Errorf("ticket %d %s: -changed-by-earlier-tickets = %q, want %q", ticket.Index, what, got, want)
			}
		}
	}

	// Ticket 1 changed only its test file: ticket 2 is owed its required file.
	saveRun(1, "internal/payments/refunds_test.go")
	wantFlag(r.Tickets[1], "")

	// Ticket 1 changed ticket 2's required file: ticket 2 carries it, and
	// ticket 1's own retry and corrective round never do.
	saveRun(1, required, "internal/payments/refunds_test.go")
	wantFlag(r.Tickets[0], "")
	wantFlag(r.Tickets[1], required)

	// Any earlier ticket counts, not only the one just before.
	saveRun(1, "internal/payments/refunds_test.go")
	saveRun(2, required)
	wantFlag(r.Tickets[2], required)

	// An earlier ticket's run that cannot be loaded refuses the build.
	r.Tickets[1].RunID = "no-such-run"
	if _, err := requestdriver.BuildRequestBuildArgs(dataDir, r, r.Tickets[2], cfg); err == nil || !strings.Contains(err.Error(), "load ticket 2's run no-such-run") {
		t.Errorf("ticket 3 with ticket 2's run unloadable: err = %v, want a refusal naming it", err)
	}
}
