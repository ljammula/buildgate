package request

import (
	"slices"
	"strings"
	"testing"
	"time"

	"buildgate/internal/run"
)

// A ticket's list holds a reproduction once by path and hash, with every
// run it refused; dropping one records who dropped it and why.
func TestTicketReproductionsAreDeduplicatedAndDropsRecorded(t *testing.T) {
	a := run.RecheckEntry{RunID: "r1", HeldReproduction: run.HeldReproduction{Path: "p/a_test.go", SHA256: "aa"}}
	b := run.RecheckEntry{RunID: "r2", HeldReproduction: run.HeldReproduction{Path: "p/b_test.go", SHA256: "bb"}}
	var tk Ticket
	tk.NoteReproductionRefusal(a, "r1")
	tk.NoteReproductionRefusal(b, "r2")
	again := a
	again.RunID = "r3"
	tk.NoteReproductionRefusal(again, "r3")
	tk.NoteReproductionRefusal(again, "r3")
	if len(tk.Reproductions) != 2 || tk.Reproductions[0].RunID != "r1" || !slices.Equal(tk.Reproductions[0].RefusedRuns, []string{"r1", "r3"}) {
		t.Fatalf("list = %+v, want a once (held by r1, refused r1 and r3) and b", tk.Reproductions)
	}
	r := &Request{Tickets: []Ticket{tk}}
	r.DropReproductions(1, func(rp TicketReproduction) bool { return rp.Path == b.Path }, "bob", "overridden", time.Now())
	if len(r.Tickets[0].Reproductions) != 1 || len(r.DroppedReproductions) != 1 || r.DroppedReproductions[0].By != "bob" || r.DroppedReproductions[0].Path != b.Path || r.DroppedReproductions[0].Ticket != 1 || !r.ReproductionDropped(1, b) || r.ReproductionDropped(1, a) {
		t.Errorf("list %+v, dropped %+v; want b dropped by bob", r.Tickets[0].Reproductions, r.DroppedReproductions)
	}
	r.dropAllReproductions("carol", "sent back", time.Now())
	if len(r.Tickets[0].Reproductions) != 0 || len(r.DroppedReproductions) != 2 || r.DroppedReproductions[1].By != "carol" {
		t.Errorf("after a send-back: list %+v, dropped %+v; want every one dropped by carol", r.Tickets[0].Reproductions, r.DroppedReproductions)
	}
}

// A send-back to plan discards the tickets and ends their recheck chains:
// each reproduction on a list is recorded as dropped by the operator who
// sent the request back.
func TestASendBackRecordsTheTicketsReproductionsAsDropped(t *testing.T) {
	dataDir := t.TempDir()
	r := newQuarantinedRequestWithPlan(t, dataDir, "req-1", StateQuarantined, true, true)
	r.Tickets[0].NoteReproductionRefusal(run.RecheckEntry{RunID: "r1", HeldReproduction: run.HeldReproduction{Path: "p/a_test.go", SHA256: "aa"}}, "r1")
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	got, err := SendBack(dataDir, "req-1", "alice", "a different plan", SendBackToPlan, fixedNow)
	if err != nil {
		t.Fatalf("SendBack: %v", err)
	}
	if got.Tickets != nil || len(got.DroppedReproductions) != 1 || got.DroppedReproductions[0].By != "alice" || got.DroppedReproductions[0].Ticket != 1 || !strings.Contains(got.DroppedReproductions[0].Why, "sent back") {
		t.Errorf("tickets %+v, dropped %+v; want the reproduction dropped by alice for the send-back", got.Tickets, got.DroppedReproductions)
	}
}
