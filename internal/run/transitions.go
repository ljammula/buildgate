package run

// Transition is one edge of a run's state graph: a run in From may be saved
// in To.
type Transition struct {
	From State
	To   State
}

// Transitions is the complete state graph of a run, the one
// safety-contract.md's "Run state graph" lists edge for edge
// (internal/claims checks the two are the same set):
//
//	ready -> slice_running -> verifying -> accepted
//	  |            |              |
//	  +------------+--------------+--> halted, quarantined
//	halted (unconfirmed) -> quarantined
//	quarantined -> accepted | halted   (an authenticated override only)
//
// A run is created ready. A halt (lost step, refused launch, deadline) may
// end any unfinished state; a failed or ambiguous policy check quarantines
// from slice_running (a setup or repository gate) or verifying, never via
// halted; the orphan reclaim quarantines a run whose owner and container are
// gone from any unfinished state, an unconfirmed halt included. accepted is
// terminal: the release decision recorded for an accepted run
// (release.Decision, its own record) is not a state, and no edge leads to a
// merged or deployed one. Only ApplyOverride leaves quarantined.
func Transitions() []Transition {
	return []Transition{
		{StateReady, StateSliceRunning},
		{StateReady, StateHalted},
		{StateReady, StateQuarantined},
		{StateSliceRunning, StateVerifying},
		{StateSliceRunning, StateHalted},
		{StateSliceRunning, StateQuarantined},
		{StateVerifying, StateAccepted},
		{StateVerifying, StateHalted},
		{StateVerifying, StateQuarantined},
		{StateHalted, StateQuarantined},
		{StateQuarantined, StateAccepted},
		{StateQuarantined, StateHalted},
	}
}

// States is every state a run can be in, in the order Transitions first
// names them.
func States() []State {
	return []State{StateReady, StateSliceRunning, StateVerifying, StateAccepted, StateHalted, StateQuarantined}
}

// CanTransition reports whether Transitions holds the edge from -> to.
func CanTransition(from, to State) bool {
	for _, t := range Transitions() {
		if t.From == from && t.To == to {
			return true
		}
	}
	return false
}
