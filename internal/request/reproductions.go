package request

import (
	"fmt"
	"slices"
	"time"

	"buildgate/internal/run"
)

// TicketReproduction is one reviewer reproduction a ticket's builds must
// pass again (SC-022): the held copy (the run whose review held it, the
// finding, path, SHA-256, host path, runner and test IDs) and every run of
// the ticket it refused. The request driver adds one when it observes a run
// of the ticket refused by a reproduction, written to the request on disk at
// once (whatever the request's state), and takes it off when an operator's
// override accepted a run it refused; a retry of a request halted for a
// missing copy and a send-back to planning take it off too (both recorded in
// Request.DroppedReproductions). Nothing else edits the list. It is
// deduplicated by path and hash, and it grows only by refusals, each bounded
// by review_corrective_rounds and max_review_rounds.
type TicketReproduction struct {
	run.RecheckEntry
	RefusedRuns []string `json:"refused_runs,omitempty"`
	// Unheld is set when a build found the held copy gone or no longer
	// matching its hash; the request then halts (HaltRecheckUnheld), and
	// `factoryd retry` drops it when the copy is still missing.
	Unheld bool `json:"unheld,omitempty"`
}

// DroppedReproduction is a reproduction taken off a ticket's list: which
// ticket, who dropped it, when and why. A dropped reproduction is never
// added to that ticket's list again.
type DroppedReproduction struct {
	Ticket int `json:"ticket"`
	run.RecheckEntry
	By  string `json:"by"`
	At  string `json:"at"`
	Why string `json:"why"`
}

// ReproductionKey tells two reproductions apart: path and hash.
func ReproductionKey(e run.RecheckEntry) string { return e.SHA256 + " " + e.Path }

// ReproductionDropped reports whether e was dropped from ticket index's list.
func (r *Request) ReproductionDropped(index int, e run.RecheckEntry) bool {
	return slices.ContainsFunc(r.DroppedReproductions, func(d DroppedReproduction) bool {
		return d.Ticket == index && ReproductionKey(d.RecheckEntry) == ReproductionKey(e)
	})
}

// NoteReproductionRefusal records that e refused the ticket's run runID:
// added to Reproductions when the ticket has no reproduction with its path
// and hash, else runID is added to that one's RefusedRuns. It reports
// whether the ticket changed.
func (t *Ticket) NoteReproductionRefusal(e run.RecheckEntry, runID string) bool {
	for i := range t.Reproductions {
		if ReproductionKey(t.Reproductions[i].RecheckEntry) == ReproductionKey(e) {
			if slices.Contains(t.Reproductions[i].RefusedRuns, runID) {
				return false
			}
			t.Reproductions[i].RefusedRuns = append(t.Reproductions[i].RefusedRuns, runID)
			return true
		}
	}
	t.Reproductions = append(t.Reproductions, TicketReproduction{RecheckEntry: e, RefusedRuns: []string{runID}})
	return true
}

// DropReproductions takes every reproduction of ticket index that drop
// reports true for off its list, recording each in DroppedReproductions
// with by, why and now.
func (r *Request) DropReproductions(index int, drop func(TicketReproduction) bool, by, why string, now time.Time) {
	if index < 1 || index > len(r.Tickets) {
		return
	}
	t := &r.Tickets[index-1]
	kept := t.Reproductions[:0]
	for _, rp := range t.Reproductions {
		if !drop(rp) {
			kept = append(kept, rp)
			continue
		}
		r.DroppedReproductions = append(r.DroppedReproductions, DroppedReproduction{Ticket: index, RecheckEntry: rp.RecheckEntry, By: by, At: now.UTC().Format(time.RFC3339), Why: why})
	}
	t.Reproductions = kept
}

// dropAllReproductions records every ticket's list as dropped, by by, for
// why: a send-back that discards the tickets ends their recheck chains.
func (r *Request) dropAllReproductions(by, why string, now time.Time) {
	for i := range r.Tickets {
		r.DropReproductions(i+1, func(TicketReproduction) bool { return true }, by, why, now)
	}
}

// HaltRecheckUnheld marks a halt raised because a build of a ticket would
// continue without a reproduction it must pass again: the held copy is gone
// or no longer matches its hash (TicketReproduction.Unheld). The recovery,
// `factoryd retry`, drops those reproductions whose copy is still missing
// (recorded in DroppedReproductions) before it builds. It is a request's
// halt kind, not the unresolved cause reproduction_unheld, which halts a run
// whose copy could not be held or mounted during the run itself.
const HaltRecheckUnheld HaltKind = "recheck_unheld"

// HaltRechecksUnheld is Halt for that condition, recording the typed kind.
func (r *Request) HaltRechecksUnheld(reason string, now time.Time) error {
	if err := r.Halt(reason, now); err != nil {
		return err
	}
	r.HaltKind = HaltRecheckUnheld
	return nil
}

// dropUnheldReproductions is a retry's recovery of a HaltRecheckUnheld halt:
// every ticket's unheld reproduction whose held copy is still missing
// (missing) is dropped, by the retrying operator; one whose copy is back is
// kept, unmarked.
func (r *Request) dropUnheldReproductions(by string, missing func(run.RecheckEntry) bool, now time.Time) {
	if r.State != StateHalted || r.HaltKind != HaltRecheckUnheld {
		return
	}
	for i := range r.Tickets {
		r.DropReproductions(i+1, func(rp TicketReproduction) bool { return rp.Unheld && missing(rp.RecheckEntry) }, by,
			fmt.Sprintf("its held copy was gone or changed; dropped by %s's retry", by), now)
		for j := range r.Tickets[i].Reproductions {
			r.Tickets[i].Reproductions[j].Unheld = false
		}
	}
}
