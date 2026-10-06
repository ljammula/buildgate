package main

import (
	"buildgate/internal/request"
	"buildgate/internal/run"
)

// findOwningRequest returns the id of the request (if any) whose Tickets
// slice names runID as one of its own ticket runs. run.Run.RequestID is
// the fast path -- an O(1) run.Load, set once when request_driver.go
// starts a ticket's run (see that field's own doc comment) -- so the full
// request.List directory scan (stat + JSON-decode every request) below
// only ever runs for a run from before that field existed. Empty, not an
// error, when runID belongs to no request (e.g. a plain `factoryd <run>`
// invocation, the run failed to load, or requests failed to list at all)
// -- best-effort, since this is only ever used to decide what to print as
// a followup command, never to gate anything.
func findOwningRequest(dataDir, runID string) string {
	if loaded, err := run.Load(dataDir, runID); err == nil && loaded.RequestID != "" {
		return loaded.RequestID
	}
	requests, err := request.List(dataDir)
	if err != nil {
		return ""
	}
	for _, r := range requests {
		for _, t := range r.Tickets {
			if t.RunID == runID {
				return r.ID
			}
		}
	}
	return ""
}
