package main

import "testing"

func TestSummarizeAndPercentDone(t *testing.T) {
	todo := func(done bool) Todo { return Todo{ID: "x", Title: "t", Done: done} }
	cases := []struct {
		name    string
		todos   []Todo
		want    Summary
		wantPct int
	}{
		{
			name:    "empty",
			todos:   []Todo{},
			want:    Summary{Total: 0, Done: 0, Open: 0},
			wantPct: 0,
		},
		{
			name:    "all done",
			todos:   []Todo{todo(true), todo(true), todo(true)},
			want:    Summary{Total: 3, Done: 3, Open: 0},
			wantPct: 100,
		},
		{
			name:    "none done",
			todos:   []Todo{todo(false), todo(false)},
			want:    Summary{Total: 2, Done: 0, Open: 2},
			wantPct: 0,
		},
		{
			name:    "1 of 3",
			todos:   []Todo{todo(true), todo(false), todo(false)},
			want:    Summary{Total: 3, Done: 1, Open: 2},
			wantPct: 33,
		},
		{
			name:    "2 of 3",
			todos:   []Todo{todo(true), todo(true), todo(false)},
			want:    Summary{Total: 3, Done: 2, Open: 1},
			wantPct: 67,
		},
		{
			name:    "1 of 8",
			todos:   []Todo{todo(true), todo(false), todo(false), todo(false), todo(false), todo(false), todo(false), todo(false)},
			want:    Summary{Total: 8, Done: 1, Open: 7},
			wantPct: 13,
		},
		{
			name:    "3 of 8",
			todos:   []Todo{todo(true), todo(true), todo(true), todo(false), todo(false), todo(false), todo(false), todo(false)},
			want:    Summary{Total: 8, Done: 3, Open: 5},
			wantPct: 38,
		},
		{
			name:    "1 of 2",
			todos:   []Todo{todo(true), todo(false)},
			want:    Summary{Total: 2, Done: 1, Open: 1},
			wantPct: 50,
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := Summarize(tc.todos)
			if got != tc.want {
				t.Fatalf("Summarize = %+v, want %+v", got, tc.want)
			}
			if p := got.PercentDone(); p != tc.wantPct {
				t.Fatalf("PercentDone = %d, want %d", p, tc.wantPct)
			}
		})
	}
}
