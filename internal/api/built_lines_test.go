package api

import (
	"testing"

	"buildgate/internal/request"
	"buildgate/internal/run"
)

// TestTicketBuiltChangedLinesFollowsThePRHeadRun pins which run the "built"
// figure reads: the accepted run on the pull request, and only when its stat
// is measured from the ticket's own base.
func TestTicketBuiltChangedLinesFollowsThePRHeadRun(t *testing.T) {
	dataDir := t.TempDir()
	save := func(id string, state run.State, diffBase string, ins, del int) {
		t.Helper()
		r := &run.Run{ID: id, State: state, BaseSHA: "base", DiffBaseSHA: diffBase, DiffStat: &run.DiffStat{Insertions: ins, Deletions: del}}
		if err := r.Save(dataDir); err != nil {
			t.Fatal(err)
		}
	}
	save("first", run.StateAccepted, "", 40, 10)
	save("quarantined", run.StateQuarantined, "", 7, 1)
	save("round", run.StateAccepted, "base0", 3, 2)
	s := NewServer(dataDir)

	cases := []struct {
		name   string
		ticket request.Ticket
		want   int
	}{
		{"accepted first build", request.Ticket{RunID: "first"}, 50},
		{"not accepted", request.Ticket{RunID: "quarantined"}, 0},
		{"no run", request.Ticket{}, 0},
		{"missing run record", request.Ticket{RunID: "gone"}, 0},
		{
			"pushed corrective round is the head but measured from its own start",
			request.Ticket{RunID: "first", Rounds: []request.Round{{Index: 1, RunID: "round", Outcome: request.RoundAccepted, Pushed: true}}},
			0,
		},
		{
			"unpushed round leaves the first build as the head",
			request.Ticket{RunID: "first", Rounds: []request.Round{{Index: 1, RunID: "round", Outcome: request.RoundAccepted}}},
			50,
		},
	}
	for _, tc := range cases {
		if got := s.ticketBuiltChangedLines(tc.ticket); got != tc.want {
			t.Errorf("%s: = %d, want %d", tc.name, got, tc.want)
		}
	}
}
