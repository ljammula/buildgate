package memory

import (
	"reflect"
	"strings"
	"testing"
)

func eq(t *testing.T, name string, got, want []string) {
	t.Helper()
	if len(got) == 0 && len(want) == 0 {
		return
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%s = %q, want %q", name, got, want)
	}
}

func TestFitWithinBudget(t *testing.T) {
	r := Fit([]string{"- h."}, nil, []string{"- a.", "- b."}, Budget{}, 5)
	eq(t, "lines", r.Lines, []string{"- h.", "- a.", "- b."})
	eq(t, "added", r.Added, []string{"- a.", "- b."})
	eq(t, "removed", r.Removed, nil)
	eq(t, "notfitting", r.NotFitting, nil)
}

func TestFitLinesBudget(t *testing.T) {
	r := Fit([]string{"- h1.", "- h2."}, nil, []string{"- a.", "- b."}, Budget{Lines: 3, Chars: 1000}, 5)
	eq(t, "lines", r.Lines, []string{"- h1.", "- h2.", "- a."})
	eq(t, "notfitting", r.NotFitting, []string{"- b."})
}

func TestFitCharsBudget(t *testing.T) {
	// "- h1." is 5 bytes + newline = 6; "- aaaa." is 7+1 = 8.
	r := Fit([]string{"- h1."}, nil, []string{"- aaaa.", "- bbbb."}, Budget{Lines: 10, Chars: 14}, 5)
	eq(t, "added", r.Added, []string{"- aaaa."})
	eq(t, "notfitting", r.NotFitting, []string{"- bbbb."})
	big := strings.Repeat("x", 100)
	r = Fit(nil, nil, []string{"- " + big}, Budget{Lines: 10, Chars: 50}, 5)
	eq(t, "notfitting", r.NotFitting, []string{"- " + big})
}

func TestFitHumanLinesAreNeverRemoved(t *testing.T) {
	r := Fit([]string{"- h1.", "- h2."}, map[string]KnownLine{}, []string{"- a."}, Budget{Lines: 2, Chars: 1000}, 5)
	eq(t, "lines", r.Lines, []string{"- h1.", "- h2."})
	eq(t, "removed", r.Removed, nil)
	eq(t, "notfitting", r.NotFitting, []string{"- a."})
}

func TestFitRemovesRetireProposedFirst(t *testing.T) {
	known := map[string]KnownLine{
		"- old.":   {LastConfirmedAt: "2026-01-01"},
		"- stale.": {RetireProposed: true, LastConfirmedAt: "2026-09-01"},
	}
	r := Fit([]string{"- human.", "- old.", "- stale."}, known, []string{"- new."}, Budget{Lines: 3, Chars: 1000}, 5)
	eq(t, "lines", r.Lines, []string{"- human.", "- old.", "- new."})
	eq(t, "removed", r.Removed, []string{"- stale."})
	eq(t, "added", r.Added, []string{"- new."})
}

func TestFitRetireProposedGoesEvenWithRoom(t *testing.T) {
	known := map[string]KnownLine{"- stale.": {RetireProposed: true}}
	r := Fit([]string{"- stale.", "- keep."}, known, nil, Budget{}, 5)
	eq(t, "lines", r.Lines, []string{"- keep."})
	eq(t, "removed", r.Removed, []string{"- stale."})
}

func TestFitEvictsOldestConfirmedNext(t *testing.T) {
	known := map[string]KnownLine{
		"- a.": {LastConfirmedAt: "2026-05-01"},
		"- b.": {LastConfirmedAt: "2026-03-01"},
		"- c.": {},
		"- d.": {LastConfirmedAt: "2026-03-01"},
	}
	r := Fit([]string{"- a.", "- b.", "- c.", "- d."}, known, []string{"- n."}, Budget{Lines: 4, Chars: 1000}, 5)
	eq(t, "removed", r.Removed, []string{"- c."}) // empty sorts oldest
	r = Fit([]string{"- a.", "- b.", "- d."}, known, []string{"- n."}, Budget{Lines: 3, Chars: 1000}, 5)
	eq(t, "removed tie by position", r.Removed, []string{"- b."})
	eq(t, "lines", r.Lines, []string{"- a.", "- d.", "- n."})
}

func TestFitMaxChangesCountsRemovals(t *testing.T) {
	known := map[string]KnownLine{}
	var cur []string
	for _, s := range []string{"- r1.", "- r2.", "- r3.", "- r4.", "- r5.", "- r6."} {
		known[s] = KnownLine{RetireProposed: true}
		cur = append(cur, s)
	}
	r := Fit(cur, known, []string{"- n."}, Budget{}, 5)
	eq(t, "removed", r.Removed, cur[:5])
	eq(t, "notfitting", r.NotFitting, []string{"- n."})
	eq(t, "lines", r.Lines, []string{"- r6."})

	// An eviction plus an addition needs two changes.
	k2 := map[string]KnownLine{"- a.": {}}
	r = Fit([]string{"- a."}, k2, []string{"- n."}, Budget{Lines: 1, Chars: 100}, 1)
	eq(t, "notfitting", r.NotFitting, []string{"- n."})
	eq(t, "kept", r.Lines, []string{"- a."})
	r = Fit([]string{"- a."}, k2, []string{"- n."}, Budget{Lines: 1, Chars: 100}, 2)
	eq(t, "lines", r.Lines, []string{"- n."})

	r = Fit(nil, nil, []string{"- 1.", "- 2.", "- 3.", "- 4.", "- 5.", "- 6.", "- 7."}, Budget{}, 5)
	if len(r.Added) != 5 || len(r.NotFitting) != 2 {
		t.Fatalf("added %d notfitting %d", len(r.Added), len(r.NotFitting))
	}
	r = Fit(nil, nil, []string{"- 1."}, Budget{}, 0)
	eq(t, "zero changes", r.NotFitting, []string{"- 1."})
}

func TestFitDuplicates(t *testing.T) {
	r := Fit([]string{"- a."}, nil, []string{"- a.", "- b.", "- b."}, Budget{}, 5)
	eq(t, "lines", r.Lines, []string{"- a.", "- b."})
	eq(t, "added", r.Added, []string{"- b."})
}

func TestFitRefusesLinesTheFenceCouldNotRead(t *testing.T) {
	r := Fit(nil, nil, []string{"no bullet", "- two\nlines", "- ok."}, Budget{}, 5)
	eq(t, "added", r.Added, []string{"- ok."})
	eq(t, "notfitting", r.NotFitting, []string{"no bullet", "- two\nlines"})
}

func TestFitIsDeterministic(t *testing.T) {
	known := map[string]KnownLine{"- a.": {}, "- b.": {}, "- c.": {LastConfirmedAt: "x"}, "- s.": {RetireProposed: true}}
	cur := []string{"- a.", "- b.", "- c.", "- s.", "- h."}
	add := []string{"- n1.", "- n2.", "- n3."}
	first := Fit(cur, known, add, Budget{Lines: 5, Chars: 1000}, 5)
	for i := 0; i < 50; i++ {
		if got := Fit(cur, known, add, Budget{Lines: 5, Chars: 1000}, 5); !reflect.DeepEqual(got, first) {
			t.Fatalf("run %d differs: %+v vs %+v", i, got, first)
		}
	}
	if !reflect.DeepEqual(cur, []string{"- a.", "- b.", "- c.", "- s.", "- h."}) {
		t.Fatal("input was modified")
	}
}
