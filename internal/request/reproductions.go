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
// missing copy and a send-back take it off too (each recorded in
// Request.DroppedReproductions, covering the refusals it ended). Nothing else edits the list. It is
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
// ticket, the refusals it ended (RefusedRuns, the runs the reproduction had
// refused when it was dropped), who dropped it, when and why. A drop covers
// only those refusals: the same reproduction refusing another run of the
// ticket (a later run, a new review after an override or a send-back) is
// on the list again. Keyed by ticket and run, a drop never matches a ticket
// renumbered after a send-back, whose runs are new.
type DroppedReproduction struct {
	Ticket int `json:"ticket"`
	run.RecheckEntry
	RefusedRuns []string `json:"refused_runs,omitempty"`
	By          string   `json:"by"`
	At          string   `json:"at"`
	Why         string   `json:"why"`
}

// ReproductionKey tells two reproductions apart: path and hash.
func ReproductionKey(e run.RecheckEntry) string { return e.SHA256 + " " + e.Path }

// SentBackSince reports whether the request was sent back (SendBack, whose
// Rejection names the stage it feeds) after t: the tickets of then were
// discarded and planned again, so a run that started at t judged a plan the
// request no longer has.
func (r *Request) SentBackSince(t time.Time) bool {
	for _, rj := range r.Rejections {
		at, err := time.Parse(time.RFC3339Nano, rj.At)
		if rj.ForStage != "" && err == nil && at.After(t) {
			return true
		}
	}
	return false
}

// RefusalDropped reports whether e's refusal of run runID, a run of ticket
// index, was ended by a drop: one of that ticket, with e's path and hash,
// made for that run.
func (r *Request) RefusalDropped(index int, e run.RecheckEntry, runID string) bool {
	return slices.ContainsFunc(r.DroppedReproductions, func(d DroppedReproduction) bool {
		return d.Ticket == index && ReproductionKey(d.RecheckEntry) == ReproductionKey(e) && slices.Contains(d.RefusedRuns, runID)
	})
}

// ReproductionDropped reports whether every refusal rp records on ticket
// index was ended by a drop (RefusalDropped).
func (r *Request) ReproductionDropped(index int, rp TicketReproduction) bool {
	for _, id := range rp.RefusedRuns {
		if !r.RefusalDropped(index, rp.RecheckEntry, id) {
			return false
		}
	}
	return len(rp.RefusedRuns) > 0
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

// DropRefusals ends refusals on ticket index's list: for each
// reproduction, the runs ended(rp) names (of those it refused) are taken
// off its RefusedRuns and recorded as one drop in DroppedReproductions with
// by, why and now; a reproduction left refusing no run is taken off the
// list.
func (r *Request) DropRefusals(index int, ended func(TicketReproduction) []string, by, why string, now time.Time) {
	if index < 1 || index > len(r.Tickets) {
		return
	}
	t := &r.Tickets[index-1]
	kept := t.Reproductions[:0]
	for _, rp := range t.Reproductions {
		runs := slices.DeleteFunc(slices.Clone(ended(rp)), func(id string) bool { return !slices.Contains(rp.RefusedRuns, id) })
		if len(runs) > 0 {
			r.DroppedReproductions = append(r.DroppedReproductions, DroppedReproduction{Ticket: index, RecheckEntry: rp.RecheckEntry, RefusedRuns: runs, By: by, At: now.UTC().Format(time.RFC3339Nano), Why: why})
			rp.RefusedRuns = slices.DeleteFunc(slices.Clone(rp.RefusedRuns), func(id string) bool { return slices.Contains(runs, id) })
		}
		if len(rp.RefusedRuns) > 0 {
			kept = append(kept, rp)
		}
	}
	t.Reproductions = kept
}

// everyRefusalIf ends every refusal of a reproduction drop reports true
// for: the whole reproduction is dropped.
func everyRefusalIf(drop func(TicketReproduction) bool) func(TicketReproduction) []string {
	return func(rp TicketReproduction) []string {
		if drop(rp) {
			return rp.RefusedRuns
		}
		return nil
	}
}

// dropAllReproductions records every ticket's list as dropped, by by, for
// why: a send-back that discards the tickets ends their recheck chains.
func (r *Request) dropAllReproductions(by, why string, now time.Time) {
	for i := range r.Tickets {
		r.DropRefusals(i+1, everyRefusalIf(func(TicketReproduction) bool { return true }), by, why, now)
	}
}

// HaltRecheckUnheld marks a halt raised because a build of a ticket would
// continue without a reproduction it must pass again: the held copy is gone
// or no longer matches its hash (TicketReproduction.Unheld). The recovery,
// `factoryd retry`, drops those reproductions whose copy is still missing
// (recorded on the request, in DroppedReproductions) before it builds. It is a request's
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
		r.DropRefusals(i+1, everyRefusalIf(func(rp TicketReproduction) bool { return rp.Unheld && missing(rp.RecheckEntry) }), by,
			fmt.Sprintf("its held copy was gone or changed; dropped by %s's retry", by), now)
		for j := range r.Tickets[i].Reproductions {
			r.Tickets[i].Reproductions[j].Unheld = false
		}
	}
}
