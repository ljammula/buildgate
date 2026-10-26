package requestdriver

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"buildgate/internal/policy"
	"buildgate/internal/request"
	"buildgate/internal/run"
)

// recheckFileName is a build's -recheck-reproductions manifest, written in
// the build's own directory under the request's rounds/.
const recheckFileName = "recheck.json"

// reproductionPasses is the verdict a recheck records for a reproduction
// that passed on every test ID on the result (reproduction.VerdictPasses).
const reproductionPasses = "passes"

// noteReproductionRefusals records every reproduction that refused
// runRecord, a run of ticket index (SC-022), on the ticket's list: on the
// request on disk at once, whatever its state (recordReproductionRefusals),
// and in r. It is called where the request driver observes a run's end
// (noteRunRefusals: a ticket build, a corrective round, each attempt of a
// PR-review round, and the build-time safety net of ticketRecheck), and is
// the only way a reproduction is added to the list.
func noteReproductionRefusals(dataDir string, r *request.Request, index int, runRecord *run.Run) {
	entries := refusingReproductions(dataDir, runRecord)
	if len(entries) == 0 {
		return
	}
	if err := recordReproductionRefusals(dataDir, r, index, runRecord.ID, entries); err != nil {
		log.Printf("request %s: ticket %d: record the reproductions that refused run %s: %v", r.ID, index, runRecord.ID, err)
	}
}

// noteRunRefusals is noteReproductionRefusals for the run runID of ticket
// index, loaded from its record; nothing when runID is "" or its record
// cannot be read (renoteTicketRuns notes it again before the next build).
// Every build of a ticket calls it as soon as its runner returns, before
// anything else is decided about the request.
func noteRunRefusals(dataDir string, r *request.Request, index int, runID string) {
	if runID == "" {
		return
	}
	if rec, err := run.Load(dataDir, runID); err == nil {
		noteReproductionRefusals(dataDir, r, index, rec)
	}
}

// refusingReproductions are the reproductions that refused runRecord: each
// blocking finding's held copy, under runRecord's own review-repro
// directory, and each recheck it ran that did not pass. None when no
// reproduction refused it.
func refusingReproductions(dataDir string, runRecord *run.Run) []run.RecheckEntry {
	if !refusedByAReproduction(runRecord) {
		return nil
	}
	var out []run.RecheckEntry
	for _, rc := range runRecord.ReviewReproductionRechecks {
		if rc.Verdict != reproductionPasses && rc.Recheck != nil {
			out = append(out, *rc.Recheck)
		}
	}
	if runRecord.CodeReview == nil {
		return out
	}
	dir, err := filepath.Abs(run.ReviewReproductionDir(run.Dir(dataDir, runRecord.ID)))
	if err != nil {
		return out
	}
	for i, f := range runRecord.CodeReview.Findings {
		if f.Disposition == run.DispositionBlocks && f.Reproduction != nil {
			out = append(out, heldBlockingCopy(runRecord.ID, dir, i, f))
		}
	}
	return out
}

// recordReproductionRefusals adds entries, which refused run runID of ticket
// index, to that ticket's list on the request on disk, under the request's
// lock and from its current record, whatever its state (a request that left
// building while the run ran keeps them too), except a cancelled one; a
// refusal a drop already ended (Request.RefusalDropped: the same
// reproduction, ticket and run) is not added again; the same reproduction
// refusing another run is. r is brought up to the same list, so a later save of
// r does not undo the write.
func recordReproductionRefusals(dataDir string, r *request.Request, index int, runID string, entries []run.RecheckEntry) error {
	unlock, err := request.Lock(dataDir, r.ID)
	if err != nil {
		return err
	}
	defer unlock()
	current, err := request.Load(dataDir, r.ID)
	if err != nil {
		return err
	}
	if current.State != request.StateCancelled && index >= 1 && index <= len(current.Tickets) {
		if noteOn(current, index, runID, entries) {
			if err := current.Save(dataDir); err != nil {
				return err
			}
		}
		mergeTicketList(r, index, current)
	}
	noteOn(r, index, runID, entries)
	return nil
}

// noteOn notes entries, refusing run runID, on ticket index of req, except
// those dropped from it; it reports whether the list changed.
func noteOn(req *request.Request, index int, runID string, entries []run.RecheckEntry) bool {
	if index < 1 || index > len(req.Tickets) {
		return false
	}
	changed := false
	for _, e := range entries {
		if !req.RefusalDropped(index, e, runID) && req.Tickets[index-1].NoteReproductionRefusal(e, runID) {
			changed = true
		}
	}
	return changed
}

// mergeTicketList brings r's ticket index up to current's: the drops
// current records (taken off r's list too), then every reproduction and
// refused run current's list holds.
func mergeTicketList(r *request.Request, index int, current *request.Request) {
	if index < 1 || index > len(r.Tickets) {
		return
	}
	for _, d := range current.DroppedReproductions {
		if !slices.ContainsFunc(r.DroppedReproductions, func(x request.DroppedReproduction) bool {
			return x.At == d.At && x.Ticket == d.Ticket && request.ReproductionKey(x.RecheckEntry) == request.ReproductionKey(d.RecheckEntry)
		}) {
			r.DroppedReproductions = append(r.DroppedReproductions, d)
		}
	}
	t := &r.Tickets[index-1]
	t.Reproductions = slices.DeleteFunc(t.Reproductions, func(rp request.TicketReproduction) bool { return r.ReproductionDropped(index, rp) })
	for _, rp := range current.Tickets[index-1].Reproductions {
		for _, id := range rp.RefusedRuns {
			noteOn(r, index, id, []run.RecheckEntry{rp.RecheckEntry})
		}
	}
}

// heldBlockingCopy is the recheck entry of finding i's reproduction, held by
// run runID under dir (its review-repro directory). Whether the copy is
// there with its hash is checked when a build is given it (run.HeldCopyOnDisk).
func heldBlockingCopy(runID, dir string, i int, f run.CodeReviewFinding) run.RecheckEntry {
	rep := f.Reproduction
	held := run.HeldReproduction{Finding: i, Path: rep.Path, SHA256: rep.SHA256, HostPath: filepath.Join(dir, strconv.Itoa(i), filepath.FromSlash(rep.Path)), TestIDs: rep.TestIDs, Runner: rep.Runner}
	if f.SpecLine != nil {
		held.SpecLine = f.SpecLine.Number
	}
	return run.RecheckEntry{RunID: runID, HeldReproduction: held}
}

// ticketRecheck is the -recheck-reproductions manifest of a build of ticket
// (buildID), written under the request's rounds/<buildID>/, from the
// ticket's list (request.Ticket.Reproductions), "" when the list is empty.
// It is the one place every build of a ticket that runs reproductions gets
// its recheck (ticketQueueEntry): the first build, a corrective round, a
// retry, a retry -from <N>, a resume and every PR-review round attempt.
// First it notes again what refused any run recorded on the ticket
// (renoteTicketRuns), then drops what an operator's override ended
// (dropOverridden). A
// reproduction whose held copy is gone or no longer matches its hash is
// marked Unheld and refuses the build (an unheldError, errNoRecheck): no
// build continues without it.
func ticketRecheck(dataDir string, r *request.Request, ticket *request.Ticket, buildID string) (string, error) {
	renoteTicketRuns(dataDir, r, ticket)
	dropOverridden(dataDir, r, ticket.Index, time.Now())
	if len(ticket.Reproductions) == 0 {
		return "", nil
	}
	m := run.RecheckManifest{RequestID: r.ID}
	var unheld []string
	for i := range ticket.Reproductions {
		rp := &ticket.Reproductions[i]
		if !run.HeldCopyOnDisk(dataDir, rp.RecheckEntry) {
			rp.Unheld = true
			unheld = append(unheld, rp.Path)
			continue
		}
		m.Reproductions = append(m.Reproductions, rp.RecheckEntry)
	}
	if len(unheld) > 0 {
		return "", &unheldError{ticket: ticket.Index, paths: unheld}
	}
	return writeRecheckManifest(filepath.Join(request.Dir(dataDir, r.ID), "rounds", buildID), m)
}

// renoteTicketRuns notes again (noteReproductionRefusals) what refused each
// run recorded on ticket: its run and every round's run and earlier
// attempts. It is the safety net under the notes made where a run's outcome
// is observed: a run refused but never noted (a worker stopped between the
// run's end and the note, a request moved out of its state while the run
// ran) is on the list before the next build. A refusal a drop ended (the
// same reproduction and run) is not added again.
func renoteTicketRuns(dataDir string, r *request.Request, ticket *request.Ticket) {
	ids := []string{ticket.RunID}
	for _, rnd := range ticket.Rounds {
		ids = append(append(ids, rnd.PriorRunIDs...), rnd.RunID)
	}
	seen := map[string]bool{}
	for _, id := range ids {
		if !seen[id] {
			seen[id] = true
			noteRunRefusals(dataDir, r, ticket.Index, id)
		}
	}
}

// dropOverridden ends, on ticket index's list, every refusal of a run an
// operator's override then accepted (run.Run.Overrides): the override is
// the human's decision on it. The drop covers those runs alone: the same
// reproduction refusing another run stays, or comes back. It is the one
// place an override is observed for the list; each drop is recorded with
// the override's author, on disk at once (recordReproductionDrop).
func dropOverridden(dataDir string, r *request.Request, index int, now time.Time) {
	for _, rp := range slices.Clone(r.Tickets[index-1].Reproductions) {
		by, runs := overriddenRuns(dataDir, rp.RefusedRuns)
		if len(runs) == 0 {
			continue
		}
		key := request.ReproductionKey(rp.RecheckEntry)
		ended := func(x request.TicketReproduction) []string {
			if request.ReproductionKey(x.RecheckEntry) != key {
				return nil
			}
			return runs
		}
		why := fmt.Sprintf("run %s, which it refused, was accepted by an operator's override", strings.Join(runs, ", "))
		if err := recordReproductionDrop(dataDir, r, index, ended, by, why, now); err != nil {
			log.Printf("request %s: ticket %d: record the drop of %s: %v", r.ID, index, rp.Path, err)
		}
	}
}

// recordReproductionDrop ends refusals on ticket index's list
// (request.Request.DropRefusals): on the request on disk at once, under its
// lock and from its current record, unless it was cancelled, and in r.
func recordReproductionDrop(dataDir string, r *request.Request, index int, ended func(request.TicketReproduction) []string, by, why string, now time.Time) error {
	r.DropRefusals(index, ended, by, why, now)
	unlock, err := request.Lock(dataDir, r.ID)
	if err != nil {
		return err
	}
	defer unlock()
	current, err := request.Load(dataDir, r.ID)
	if err != nil || current.State == request.StateCancelled || index > len(current.Tickets) {
		return err
	}
	current.DropRefusals(index, ended, by, why, now)
	if err := current.Save(dataDir); err != nil {
		return err
	}
	mergeTicketList(r, index, current)
	return nil
}

// overriddenRuns are those of runIDs an operator's override accepted, and
// the author of the first such override; none when no override did.
func overriddenRuns(dataDir string, runIDs []string) (by string, runs []string) {
	for _, id := range runIDs {
		rec, err := run.Load(dataDir, id)
		if err != nil || rec.State != run.StateAccepted {
			continue
		}
		for _, o := range rec.Overrides {
			if o.NewState == run.StateAccepted {
				if by == "" {
					by = o.By
				}
				runs = append(runs, id)
				break
			}
		}
	}
	return by, runs
}

// aRecheckStillFails reports whether a recheck r ran is still failing. The
// recheck runs once per automatic correction: a corrective build that left a
// reproduction failing is not followed by another automatic one (a
// corrective round, a PR-review round's fix attempt). An operator's retry,
// resume or a later PR-review round may follow it, and rechecks again.
func aRecheckStillFails(r *run.Run) bool {
	for _, rc := range r.ReviewReproductionRechecks {
		if rc.Verdict != reproductionPasses {
			return true
		}
	}
	return false
}

// automaticCorrectionMayFollow reports whether an automatic corrective
// build may follow quarantined on its commits: a build may be given its
// record (handoffForABuild), and no recheck it ran is still failing
// (aRecheckStillFails).
func automaticCorrectionMayFollow(dataDir string, quarantined *run.Run) bool {
	_, ok := handoffForABuild(dataDir, quarantined)
	return ok && !aRecheckStillFails(quarantined)
}

// refusedByAReproduction reports whether a reviewer's reproduction refused
// runRecord (its review_reproduction check failed).
func refusedByAReproduction(runRecord *run.Run) bool {
	return failedGate(runRecord, policy.ReviewReproductionCheck)
}

// onlyAReproductionRefused reports whether review_reproduction is the only
// check runRecord failed.
func onlyAReproductionRefused(runRecord *run.Run) bool {
	for _, g := range runRecord.GateResults {
		if !g.Passed && g.Check != policy.ReviewReproductionCheck {
			return false
		}
	}
	return refusedByAReproduction(runRecord)
}

// writeRecheckManifest writes m as dir's recheckFileName and returns its
// path. The file is the factory's, outside any workspace; factoryd checks
// every entry again before the build starts.
func writeRecheckManifest(dir string, m run.RecheckManifest) (string, error) {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return "", fmt.Errorf("encode the recheck manifest: %w", err)
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", fmt.Errorf("create %s: %w", dir, err)
	}
	path := filepath.Join(dir, recheckFileName)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return "", fmt.Errorf("write %s: %w", path, err)
	}
	return path, nil
}

// errNoRecheck is in the chain of every refusal to build a ticket without
// the reproductions its list says it must pass again.
var errNoRecheck = errors.New("not built without a recheck of the reviewer's reproductions")

// unheldError is ticketRecheck's refusal when a held copy is gone or no
// longer matches its hash.
type unheldError struct {
	ticket int
	paths  []string
}

func (e *unheldError) Error() string {
	return fmt.Sprintf("ticket %d: %v: the held copy of %s is gone or no longer matches its hash", e.ticket, errNoRecheck, strings.Join(e.paths, ", "))
}

func (e *unheldError) Unwrap() error { return errNoRecheck }

// haltWithoutRecheck halts r for err, a refusal to build without a recheck.
// A held copy that is gone halts with HaltRecheckUnheld, whose reason
// names the operator's way out: `factoryd retry` drops those reproductions,
// recorded on the ticket, and builds again.
func haltWithoutRecheck(dataDir string, r *request.Request, err error, now time.Time) error {
	var unheld *unheldError
	if !errors.As(err, &unheld) {
		return HaltRequest(dataDir, r, err.Error(), now)
	}
	reason := fmt.Sprintf("%v; `factoryd retry %s` drops them (recorded on the ticket) and builds again, or `factoryd cancel %s`", err, r.ID, r.ID)
	oldState := r.State
	if err := r.HaltRechecksUnheld(reason, now); err != nil {
		return err
	}
	log.Printf("request %s: %s -> halted (%s)", r.ID, oldState, reason)
	return notifyTerminalRequest(dataDir, r, reason, now)
}

// hasFlag reports whether flag is one of argv's arguments.
func hasFlag(argv []string, flag string) bool {
	for _, a := range argv {
		if a == flag {
			return true
		}
	}
	return false
}
