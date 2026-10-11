package run

import "buildgate/internal/unresolved"

// UnresolvedCauseOf is the cause a halted run recorded, "" for a run that is
// not halted or whose halt is a result (a refused precondition, a ceiling, a
// failed baseline). A run kept for a resume with no recorded cause was lost
// with its worker.
func UnresolvedCauseOf(r *Run) unresolved.Cause {
	if r == nil || r.State != StateHalted {
		return ""
	}
	if c := unresolved.Cause(r.HaltReasonCode); c.Known() {
		return c
	}
	if r.KeptForResume && r.HaltReasonCode == "" {
		return unresolved.WorkerLost
	}
	return ""
}
