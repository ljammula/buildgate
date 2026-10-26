package requestdriver

import (
	"slices"
	"testing"
	"time"

	"buildgate/internal/request"
	"buildgate/internal/run"
)

// mergeTicketList takes a drop current records off r's list run by run,
// keeps the reproduction for a refusal no drop ended, and carries current's
// Unheld mark onto an entry it adds.
func TestMergeTicketListFiltersDroppedRunsAndKeepsUnheld(t *testing.T) {
	a := run.RecheckEntry{RunID: "x", HeldReproduction: run.HeldReproduction{Path: "p/a_test.go", SHA256: "aa"}}
	b := run.RecheckEntry{RunID: "y", HeldReproduction: run.HeldReproduction{Path: "p/b_test.go", SHA256: "bb"}}
	r := &request.Request{Tickets: []request.Ticket{{Index: 1, Reproductions: []request.TicketReproduction{{RecheckEntry: a, RefusedRuns: []string{"x", "z"}}}}}}
	current := &request.Request{Tickets: []request.Ticket{{Index: 1, Reproductions: []request.TicketReproduction{
		{RecheckEntry: a, RefusedRuns: []string{"z"}},
		{RecheckEntry: b, RefusedRuns: []string{"y"}, Unheld: true},
	}}}}
	current.DroppedReproductions = []request.DroppedReproduction{{Ticket: 1, RecheckEntry: a, RefusedRuns: []string{"x"}, By: "bob", At: time.Now().Format(time.RFC3339Nano)}}
	mergeTicketList(r, 1, current)
	list := r.Tickets[0].Reproductions
	if len(r.DroppedReproductions) != 1 || len(list) != 2 || !slices.Equal(list[0].RefusedRuns, []string{"z"}) || !list[1].Unheld || list[0].Unheld {
		t.Errorf("list %+v, drops %+v; want a for z alone and b marked unheld", list, r.DroppedReproductions)
	}
}

// overriddenRuns groups the overridden runs by each one's override author,
// so each author's override is its own drop.
func TestOverriddenRunsAreGroupedByAuthor(t *testing.T) {
	dataDir := t.TempDir()
	for id, by := range map[string]string{"x1": "bob", "x2": "carol", "x3": "bob"} {
		rec := &run.Run{ID: id, State: run.StateQuarantined}
		if err := rec.ApplyOverride(by, "why", run.StateAccepted, func() string { return time.Now().Format(time.RFC3339) }); err != nil {
			t.Fatal(err)
		}
		if err := rec.Save(dataDir); err != nil {
			t.Fatal(err)
		}
	}
	if err := (&run.Run{ID: "q", State: run.StateQuarantined}).Save(dataDir); err != nil {
		t.Fatal(err)
	}
	got := overriddenRuns(dataDir, []string{"x1", "q", "x2", "x3", "gone"})
	if len(got) != 2 || !slices.Equal(got["bob"], []string{"x1", "x3"}) || !slices.Equal(got["carol"], []string{"x2"}) {
		t.Errorf("overriddenRuns = %v, want bob: x1, x3 and carol: x2", got)
	}
}
