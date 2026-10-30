package prorder

import (
	"reflect"
	"strings"
	"testing"
)

var migrations = Order{{"db/migrations/"}, {"*.sql", "api/"}}

func TestGroupsSplitsByFirstMatchingGroupInGroupOrder(t *testing.T) {
	tests := []struct {
		name    string
		order   Order
		changed []string
		want    [][]string
	}{
		{"none changed", migrations, nil, [][]string{}},
		{"one group", migrations, []string{"db/migrations/001.sql"}, [][]string{{"db/migrations/001.sql"}}},
		{"first match wins", migrations, []string{"db/migrations/001.sql", "x.sql"}, [][]string{{"db/migrations/001.sql"}, {"x.sql"}}},
		{"unmatched is one more group", migrations, []string{"main.go", "db/migrations/a.sql", "api/h.go"}, [][]string{{"db/migrations/a.sql"}, {"api/h.go"}, {"main.go"}}},
		{"group order not path order", migrations, []string{"api/z.go", "db/migrations/a.sql"}, [][]string{{"db/migrations/a.sql"}, {"api/z.go"}}},
		{"files sorted, repeats dropped", migrations, []string{"api/b.go", "api/a.go", "api/b.go"}, [][]string{{"api/a.go", "api/b.go"}}},
		{"no groups listed", nil, []string{"b", "a"}, [][]string{{"a", "b"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.order.Groups(tt.changed); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("Groups = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestReasonsNameTheFileAndTheGroup(t *testing.T) {
	if got := migrations.MixedReason([]string{"api/a.go", "api/b.go"}); got != "" {
		t.Errorf("one group is mixed: %q", got)
	}
	got := migrations.MixedReason([]string{"main.go", "db/migrations/001.sql"})
	for _, want := range []string{"db/migrations/001.sql in pr_order group 1 (db/migrations/)", "main.go in the files no pr_order group lists"} {
		if !strings.Contains(got, want) {
			t.Errorf("MixedReason = %q, want it to contain %q", got, want)
		}
	}
	if got := migrations.OutsideReason(0, []string{"db/migrations/a.sql"}); got != "" {
		t.Errorf("OutsideReason inside the group = %q", got)
	}
	got = migrations.OutsideReason(0, []string{"db/migrations/a.sql", "api/h.go"})
	if !strings.Contains(got, "api/h.go belongs to pr_order group 2") || !strings.Contains(got, "limited to pr_order group 1") {
		t.Errorf("OutsideReason = %q", got)
	}
}

func TestCarveHoldsEveryPathAndSendsByproductsWithTheLastPart(t *testing.T) {
	changed := []string{"main.go", "db/migrations/001.sql", "api/h.go", ".pi-combined-review-session/s.json"}
	parts := migrations.Carve(changed)
	want := []Part{
		{Group: 0, Files: []string{"db/migrations/001.sql"}},
		{Group: 1, Files: []string{"api/h.go"}},
		{Group: 2, Files: []string{".pi-combined-review-session/s.json", "main.go"}},
	}
	if !reflect.DeepEqual(parts, want) {
		t.Errorf("Carve = %v, want %v", parts, want)
	}
	if got := migrations.Carve([]string{".pi-combined-review-session/s.json"}); len(got) != 0 {
		t.Errorf("a change of byproducts only has parts: %v", got)
	}
}
