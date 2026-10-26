package run

import "testing"

// TestStackBase: a replay run's stack base is the commit it replayed onto,
// whatever else it records; a run records its own; a first build recorded
// before the field reads its prior run's result; anything else is "".
func TestStackBase(t *testing.T) {
	dataDir := t.TempDir()
	prior := &Run{ID: "prior", State: StateAccepted, ResultSHA: sha('p')}
	if err := prior.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	replay := validReplay()
	cases := map[string]struct {
		record *Run
		want   string
	}{
		"a replay run":            {&Run{StackBaseSHA: sha('9'), Replay: &replay}, replay.OntoSHA},
		"a recorded stack base":   {&Run{StackBaseSHA: sha('9'), PriorRunID: "prior"}, sha('9')},
		"a first build before it": {&Run{PriorRunID: "prior"}, sha('p')},
		"a prior run gone":        {&Run{PriorRunID: "gone"}, ""},
		"not stacked":             {&Run{}, ""},
		"no record":               {nil, ""},
	}
	for name, c := range cases {
		if got := StackBase(dataDir, c.record); got != c.want {
			t.Errorf("%s: StackBase = %q, want %q", name, got, c.want)
		}
	}
}
