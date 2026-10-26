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

	"buildgate/internal/evidence"
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

// noteReproductionRefusals records on ticket every reproduction that refused
// runRecord, a run of that ticket (SC-022): each blocking finding's held
// copy, under runRecord's own review-repro directory, and each recheck it
// ran that did not pass. It is where the request driver observes a refusal
// (tagTicketRound, and each attempt of a PR-review round), and the only
// place a reproduction is added to the ticket's list.
func noteReproductionRefusals(dataDir string, ticket *request.Ticket, runRecord *run.Run) {
	if !refusedByAReproduction(runRecord) {
		return
	}
	for _, rc := range runRecord.ReviewReproductionRechecks {
		if rc.Verdict != reproductionPasses && rc.Recheck != nil {
			ticket.NoteReproductionRefusal(*rc.Recheck, runRecord.ID)
		}
	}
	if runRecord.CodeReview == nil {
		return
	}
	dir, err := filepath.Abs(run.ReviewReproductionDir(run.Dir(dataDir, runRecord.ID)))
	if err != nil {
		log.Printf("ticket %d: run %s: the reproductions that refused it are not recorded: %v", ticket.Index, runRecord.ID, err)
		return
	}
	for i, f := range runRecord.CodeReview.Findings {
		if f.Disposition == run.DispositionBlocks && f.Reproduction != nil {
			ticket.NoteReproductionRefusal(heldBlockingCopy(runRecord.ID, dir, i, f), runRecord.ID)
		}
	}
}

// heldBlockingCopy is the recheck entry of finding i's reproduction, held by
// run runID under dir (its review-repro directory). Whether the copy is
// there with its hash is checked when a build is given it (heldCopyOnDisk).
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
// First it drops what an operator's override ended (dropOverridden). A
// reproduction whose held copy is gone or no longer matches its hash is
// marked Unheld and refuses the build (an unheldError, errNoRecheck): no
// build continues without it.
func ticketRecheck(dataDir string, r *request.Request, ticket *request.Ticket, buildID string) (string, error) {
	dropOverridden(dataDir, ticket, time.Now())
	if len(ticket.Reproductions) == 0 {
		return "", nil
	}
	m := run.RecheckManifest{RequestID: r.ID}
	var unheld []string
	for i := range ticket.Reproductions {
		rp := &ticket.Reproductions[i]
		if !heldCopyOnDisk(dataDir, rp.RecheckEntry) {
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

// dropOverridden takes off ticket's list every reproduction that refused a
// run an operator's override then accepted (run.Run.Overrides): the
// override is the human's decision on it. It is the one place an override
// is observed for the list; each drop is recorded with the override's
// author.
func dropOverridden(dataDir string, ticket *request.Ticket, now time.Time) {
	for _, rp := range slices.Clone(ticket.Reproductions) {
		by, runID := overriddenBy(dataDir, rp.RefusedRuns)
		if by == "" {
			continue
		}
		key := request.ReproductionKey(rp.RecheckEntry)
		ticket.DropReproductions(func(x request.TicketReproduction) bool { return request.ReproductionKey(x.RecheckEntry) == key }, by,
			fmt.Sprintf("run %s, which it refused, was accepted by an operator's override", runID), now)
	}
}

// overriddenBy is the author of the override that accepted one of runIDs,
// and that run, or "" when none was.
func overriddenBy(dataDir string, runIDs []string) (by, runID string) {
	for _, id := range runIDs {
		rec, err := run.Load(dataDir, id)
		if err != nil || rec.State != run.StateAccepted {
			continue
		}
		for _, o := range rec.Overrides {
			if o.NewState == run.StateAccepted {
				return o.By, id
			}
		}
	}
	return "", ""
}

// heldCopyOnDisk reports whether e's host path is a file under its run's
// review-repro directory in dataDir with the hash e records. factoryd checks
// every entry again, links included, before the build starts.
func heldCopyOnDisk(dataDir string, e run.RecheckEntry) bool {
	if !run.ValidID(e.RunID) || e.SHA256 == "" {
		return false
	}
	dir, err := filepath.Abs(run.ReviewReproductionDir(run.Dir(dataDir, e.RunID)))
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(dir, e.HostPath)
	if err != nil || !filepath.IsLocal(rel) {
		return false
	}
	sum, err := evidence.SHA256File(e.HostPath)
	return err == nil && sum == e.SHA256
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
// A held copy that is gone halts with HaltReproductionUnheld, whose reason
// names the operator's way out: `factoryd retry` drops those reproductions,
// recorded on the ticket, and builds again.
func haltWithoutRecheck(dataDir string, r *request.Request, err error, now time.Time) error {
	var unheld *unheldError
	if !errors.As(err, &unheld) {
		return HaltRequest(dataDir, r, err.Error(), now)
	}
	reason := fmt.Sprintf("%v; `factoryd retry %s` drops them (recorded on the ticket) and builds again, or `factoryd cancel %s`", err, r.ID, r.ID)
	oldState := r.State
	if err := r.HaltReproductionsUnheld(reason, now); err != nil {
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
