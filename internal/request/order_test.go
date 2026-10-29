package request

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReplaceWithCarvedRenumbersLaterTicketsAndKeepsEveryPin(t *testing.T) {
	dataDir, id := t.TempDir(), "req-1"
	r := newAmendScopeFixture(t, dataDir, id, "first ticket\n")
	r.State = StateBuilding
	dir := filepath.Join(Dir(dataDir, id), "tickets")
	second := filepath.Join(dir, "002.spec.md")
	if err := os.WriteFile(second, []byte("second ticket\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	hash, err := HashFile(dataDir, id, "tickets/002.spec.md")
	if err != nil {
		t.Fatal(err)
	}
	r.Tickets = append(r.Tickets, Ticket{Index: 2, SpecPath: second})
	r.TicketCount = 2
	r.ApprovedSHA256["tickets/002.spec.md"] = hash

	groups := [][]string{{"db/"}}
	if err := r.ReplaceWithCarved(dataDir, 1, "run-1", groups, []int{1, 2}); err != nil {
		t.Fatal(err)
	}
	if r.TicketCount != 3 || len(r.Tickets) != 3 {
		t.Fatalf("%d tickets, want 3", len(r.Tickets))
	}
	for i, ticket := range r.Tickets {
		if ticket.Index != i+1 || filepath.Base(ticket.SpecPath) != "00"+string(rune('1'+i))+".spec.md" {
			t.Fatalf("ticket %d is %d at %s", i+1, ticket.Index, ticket.SpecPath)
		}
	}
	if r.Tickets[0].Order == nil || r.Tickets[1].Order == nil || r.Tickets[2].Order != nil || r.Tickets[1].Order.Group != 2 || r.Tickets[1].Order.First != 1 {
		t.Fatalf("orders = %+v %+v %+v, want the first two carved from ticket 1 only", r.Tickets[0].Order, r.Tickets[1].Order, r.Tickets[2].Order)
	}
	if got, _ := os.ReadFile(r.Tickets[1].SpecPath); string(got) != "first ticket\n" {
		t.Fatalf("carved ticket holds %q, want the replaced ticket's file", got)
	}
	if got, _ := os.ReadFile(r.Tickets[2].SpecPath); string(got) != "second ticket\n" {
		t.Fatalf("the later ticket holds %q after the move", got)
	}
	if err := VerifyApprovedHashes(dataDir, r); err != nil {
		t.Fatalf("pins after the carve: %v", err)
	}
	if err := r.ReplaceWithCarved(dataDir, 1, "run-1", groups, []int{1}); err == nil {
		t.Fatal("a carve into one ticket was accepted")
	}
}

// A carve a crash cut short has already moved later tickets' files while the
// record still holds the old numbers. Carving again refuses, and no ticket's
// file is overwritten or lost.
func TestReplaceWithCarvedAfterACrashRefusesAndDestroysNothing(t *testing.T) {
	dataDir, id := t.TempDir(), "req-1"
	r := newAmendScopeFixture(t, dataDir, id, "first ticket\n")
	r.State = StateBuilding
	dir := filepath.Join(Dir(dataDir, id), "tickets")
	write := func(name, body string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	second, third := write("002.spec.md", "second\n"), write("003.spec.md", "third\n")
	r.Tickets = append(r.Tickets, Ticket{Index: 2, SpecPath: second}, Ticket{Index: 3, SpecPath: third})
	r.TicketCount = 3
	// The crash left ticket 3's file at 004 and ticket 2's at 003.
	if err := os.Rename(third, filepath.Join(dir, "004.spec.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(second, third); err != nil {
		t.Fatal(err)
	}
	err := r.ReplaceWithCarved(dataDir, 1, "run-1", [][]string{{"db/"}}, []int{1, 2})
	if err == nil || !strings.Contains(err.Error(), "cut short") {
		t.Fatalf("error = %v, want a refusal naming the interrupted carve", err)
	}
	for name, want := range map[string]string{"003.spec.md": "second\n", "004.spec.md": "third\n"} {
		if got, _ := os.ReadFile(filepath.Join(dir, name)); string(got) != want {
			t.Errorf("%s holds %q, want %q", name, got, want)
		}
	}
	if r.TicketCount != 3 || len(r.Tickets) != 3 {
		t.Errorf("the record changed: %d tickets", len(r.Tickets))
	}
}
