package run

// Transition is one edge of a run's state graph: a run saved in From may
// next be saved in To.
type Transition struct {
	From State
	To   State
}

// Transitions is the complete state graph of a run as run.json records it,
// the one safety-contract.md's "Run state graph" lists edge for edge
// (internal/claims checks the two are the same set):
//
//	ready -> slice_running -> accepted | quarantined
//	  |            |
//	  +------------+--> halted, quarantined
//	halted (unconfirmed) -> accepted | quarantined   (the owner's own result)
//	quarantined -> accepted | halted                 (an authenticated override only)
//
// A run is created ready, or already halted (a confirmed halt) when a check
// before its build refuses it. Before the build is dispatched it is saved
// slice_running, and the next save is the workflow's verdict: accepted or
// quarantined from the policy evaluation, or halted. verifying is a stage
// of the workflow's progress feed (RunWorkflowResult, the owner's query),
// never saved, so it is not a state here. A halt (lost step, refused launch,
// deadline) may end any unfinished state; a give-up path that could not
// confirm the owner stopped saves it halted and unconfirmed, and the orphan
// reclaim later applies the owner's real result to it (accepted,
// quarantined, or a confirmed halt) or quarantines it once its owner and
// container are gone. accepted is terminal: the release decision recorded
// for an accepted run (release.Decision, its own record) is not a state, and
// no edge leads to a merged or deployed one. Only ApplyOverride leaves
// quarantined.
func Transitions() []Transition {
	return []Transition{
		{StateReady, StateSliceRunning},
		{StateReady, StateHalted},
		{StateReady, StateQuarantined},
		{StateSliceRunning, StateAccepted},
		{StateSliceRunning, StateHalted},
		{StateSliceRunning, StateQuarantined},
		{StateHalted, StateAccepted},
		{StateHalted, StateQuarantined},
		{StateQuarantined, StateAccepted},
		{StateQuarantined, StateHalted},
	}
}

// States is every state run.json records, in the order Transitions first
// names them. StateVerifying is not one: it is the workflow's progress stage.
func States() []State {
	return []State{StateReady, StateSliceRunning, StateAccepted, StateHalted, StateQuarantined}
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
