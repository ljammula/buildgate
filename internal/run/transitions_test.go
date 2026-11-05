package run

import "testing"

func TestTransitionsNameOnlyKnownStatesAndReachEveryOne(t *testing.T) {
	known := map[State]bool{}
	for _, s := range States() {
		known[s] = true
	}
	reached := map[State]bool{StateReady: true}
	for _, tr := range Transitions() {
		if !known[tr.From] || !known[tr.To] {
			t.Errorf("transition %s -> %s names a state outside States()", tr.From, tr.To)
		}
		if tr.From == tr.To {
			t.Errorf("transition %s -> %s is a self-edge", tr.From, tr.To)
		}
		reached[tr.To] = true
	}
	for s := range known {
		if !reached[s] {
			t.Errorf("state %s is in States() and no transition reaches it", s)
		}
	}
}

func TestAcceptedIsTerminal(t *testing.T) {
	for _, tr := range Transitions() {
		if tr.From == StateAccepted {
			t.Errorf("accepted has an outgoing edge to %s", tr.To)
		}
	}
}

func TestApplyOverrideFollowsTheGraph(t *testing.T) {
	now := func() string { return "2026-11-05T00:00:00Z" }
	for _, to := range States() {
		r := &Run{ID: "r", State: StateQuarantined}
		err := r.ApplyOverride("op", "why", to, now)
		if want := CanTransition(StateQuarantined, to); (err == nil) != want {
			t.Errorf("override to %s: err = %v, want allowed = %v", to, err, want)
		}
	}
}
