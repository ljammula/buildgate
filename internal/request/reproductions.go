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
// the ticket it refused. The request driver appends one when it observes a
// run of the ticket refused by a reproduction, and takes it off when an
// operator's override accepted a run it refused (DroppedReproductions);
// nothing else edits the list. It is deduplicated by path and hash, and it
// grows only by refusals, each bounded by review_corrective_rounds and
// max_review_rounds.
type TicketReproduction struct {
	run.RecheckEntry
	RefusedRuns []string `json:"refused_runs,omitempty"`
	// Unheld is set when a build found the held copy gone or no longer
	// matching its hash; the request then halts (HaltReproductionUnheld),
	// and `factoryd retry` drops it.
	Unheld bool `json:"unheld,omitempty"`
}

// DroppedReproduction is a reproduction taken off a ticket's list: who
// dropped it, when and why.
type DroppedReproduction struct {
	run.RecheckEntry
	By  string `json:"by"`
	At  string `json:"at"`
	Why string `json:"why"`
}

// ReproductionKey tells two reproductions apart: path and hash.
func ReproductionKey(e run.RecheckEntry) string { return e.SHA256 + " " + e.Path }

// NoteReproductionRefusal records that e refused the ticket's run runID:
// added to Reproductions when the ticket has no reproduction with its path
// and hash, else runID is added to that one's RefusedRuns.
func (t *Ticket) NoteReproductionRefusal(e run.RecheckEntry, runID string) {
	for i := range t.Reproductions {
		if ReproductionKey(t.Reproductions[i].RecheckEntry) == ReproductionKey(e) {
			if !slices.Contains(t.Reproductions[i].RefusedRuns, runID) {
				t.Reproductions[i].RefusedRuns = append(t.Reproductions[i].RefusedRuns, runID)
			}
			return
		}
	}
	t.Reproductions = append(t.Reproductions, TicketReproduction{RecheckEntry: e, RefusedRuns: []string{runID}})
}

// DropReproductions takes every reproduction drop reports true for off the
// list, recording each in DroppedReproductions with by, why and now.
func (t *Ticket) DropReproductions(drop func(TicketReproduction) bool, by, why string, now time.Time) {
	kept := t.Reproductions[:0]
	for _, rp := range t.Reproductions {
		if !drop(rp) {
			kept = append(kept, rp)
			continue
		}
		t.DroppedReproductions = append(t.DroppedReproductions, DroppedReproduction{RecheckEntry: rp.RecheckEntry, By: by, At: now.UTC().Format(time.RFC3339), Why: why})
	}
	t.Reproductions = kept
}

// HaltReproductionUnheld marks a halt raised because a build of a ticket
// would continue without a reproduction it must pass again: the held copy
// is gone or no longer matches its hash (TicketReproduction.Unheld). The
// recovery, `factoryd retry`, drops those reproductions (recorded on the
// ticket) before it builds.
const HaltReproductionUnheld HaltKind = "reproduction_unheld"

// HaltReproductionsUnheld is Halt for that condition, recording the typed kind.
func (r *Request) HaltReproductionsUnheld(reason string, now time.Time) error {
	if err := r.Halt(reason, now); err != nil {
		return err
	}
	r.HaltKind = HaltReproductionUnheld
	return nil
}

// dropUnheldReproductions is a retry's recovery of a HaltReproductionUnheld
// halt: every ticket's unheld reproductions are dropped, by the retrying
// operator.
func (r *Request) dropUnheldReproductions(by string, now time.Time) {
	if r.State != StateHalted || r.HaltKind != HaltReproductionUnheld {
		return
	}
	for i := range r.Tickets {
		r.Tickets[i].DropReproductions(func(rp TicketReproduction) bool { return rp.Unheld }, by,
			fmt.Sprintf("its held copy was gone or changed; dropped by %s's retry", by), now)
	}
}
