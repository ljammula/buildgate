package request

import (
	"slices"
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
	tk.DropReproductions(func(rp TicketReproduction) bool { return rp.Path == b.Path }, "bob", "overridden", time.Now())
	if len(tk.Reproductions) != 1 || len(tk.DroppedReproductions) != 1 || tk.DroppedReproductions[0].By != "bob" || tk.DroppedReproductions[0].Path != b.Path {
		t.Errorf("list %+v, dropped %+v; want b dropped by bob", tk.Reproductions, tk.DroppedReproductions)
	}
}
