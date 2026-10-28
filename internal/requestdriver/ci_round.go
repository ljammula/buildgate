package requestdriver

import (
	"context"
	"fmt"
	"log"
	"slices"
	"strings"
	"time"

	"buildgate/internal/forge"
	"buildgate/internal/handoff"
	"buildgate/internal/notify"
	"buildgate/internal/request"
	"buildgate/internal/run"
)

// CI rounds (safety-contract.md, "A pull request's CI checks are read on
// every PR-review poll").
//
// A required check that completed with FAILURE on a PR head the factory
// pushed starts one PR-review round, the same round a review thread starts
// (runPRReviewRound), given the check as its earlier-attempt record. A
// ticket runs at most one such round per check name; the same check failing
// again parks the request in resume_review. Sorting the checks reads only
// their structured fields (forge.ClassifyChecks).

// ciStart is the sentence a CI round's record opens with: where the build
// starts, and that the attempt it follows was accepted.
const ciStart = startsOnItsBranch + " That attempt was accepted and pushed: what follows is a failed required check of its pull request, not a failure of the factory's own checks."

// ciAfterRefusalStart opens the record of a CI round that follows a refused
// round: the refused attempt's checks, and the pull request's failed check.
const ciAfterRefusalStart = startsOnItsBranch + " That attempt was refused by the checks below, and the pull request it follows also has a failed required check."

// githubActionsApp is the slug GitHub records for a check run posted by
// GitHub Actions, whose check run id is its job's id.
const githubActionsApp = "github-actions"

// roundTrigger is what starts a PR-review round: new review threads, or one
// failed required CI check. Exactly one is set.
type roundTrigger struct {
	threads []forge.Thread
	ci      *forge.Check
}

// checkName is the CI check that started the round, "" for threads.
func (t roundTrigger) checkName() string {
	if t.ci == nil {
		return ""
	}
	return t.ci.Name
}

// record is the earlier-attempt record of a CI round, nil for a round of
// threads. head is the run behind the pull request's head. The job's log is
// read through Deps.ReadJobLog for a GitHub Actions check run only; a log
// that cannot be read leaves the record with the check's name, conclusion
// and details URL alone.
func (t roundTrigger) record(dp Deps, ctx context.Context, head *run.Run, ticket *request.Ticket) *handoff.Document {
	if t.ci == nil || head == nil {
		return nil
	}
	logText, noLog := "", "it is not a GitHub Actions job"
	if t.ci.RunID != 0 && strings.EqualFold(t.ci.Poster, githubActionsApp) {
		readCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
		defer cancel()
		var err error
		if logText, err = dp.ReadJobLog(readCtx, ticket.PRURL, t.ci.RunID); err != nil {
			log.Printf("ticket %d: read the log of check %q: %v", ticket.Index, t.ci.Name, err)
			logText, noLog = "", "reading it failed"
		}
	}
	doc := handoff.CI(head, t.ci.Name, t.ci.Conclusion, t.ci.DetailsURL, logText, noLog)
	return &doc
}

// ranCIRound reports whether ticket already ran a CI round for the check
// name (one that counts: not a start failure, not one with no verdict).
func ranCIRound(ticket *request.Ticket, name string) bool {
	for _, rnd := range ticket.Rounds {
		if rnd.CICheck == name && rnd.Counted() {
			return true
		}
	}
	return false
}

// handleCIChecks reacts to the checks of the pull request's head after the
// poll's review-thread handling found nothing to act on. handled is true
// when it started a round or parked the request, and the caller returns err.
func handleCIChecks(dp Deps, ctx context.Context, dataDir string, r *request.Request, ticket *request.Ticket, state forge.ReviewState, cfg WorkerConfig, now time.Time) (handled bool, err error) {
	// A memory request's pull request releases only the text the operator
	// approved: a build could change nothing there.
	if r.Source.Kind == request.SourceMemory || len(state.Checks) == 0 || !prHeadIsRun(dataDir, ticket.PRHeadRunID(), state.HeadSHA) {
		return false, nil
	}
	verdict := forge.ClassifyChecks(state.Checks, state.RequiredUnknown)
	if err := triageCIChecks(dataDir, r, ticket, verdict.Triage, state.HeadSHA, now); err != nil {
		return true, err
	}
	var again *forge.Check
	for i := range verdict.Rounds {
		c := verdict.Rounds[i]
		if !ranCIRound(ticket, c.Name) {
			return true, runPRReviewRound(dp, ctx, dataDir, r, ticket, roundTrigger{ci: &c}, cfg, now)
		}
		if again == nil {
			again = &verdict.Rounds[i]
		}
	}
	if again == nil {
		return false, nil
	}
	reason := fmt.Sprintf("ticket %d/%d: required check %q still fails on pull request %s after its CI round", ticket.Index, r.TicketCount, handoff.CleanSentence(again.Name), ticket.PRURL)
	return true, parkRequest(dp, dataDir, r, reason, now)
}

// triageCIChecks sends the operator one triage sentence for each required
// check in checks whose failure is not the code's, once per check and head.
func triageCIChecks(dataDir string, r *request.Request, ticket *request.Ticket, checks []forge.Check, headSHA string, now time.Time) error {
	sent := false
	for _, c := range checks {
		key := c.Name + "@" + headSHA
		if slices.Contains(ticket.CITriaged, key) {
			continue
		}
		ticket.CITriaged = append(ticket.CITriaged, key)
		sent = true
		why := fmt.Sprintf("concluded %s, which a code change does not fix", handoff.CleanWord(c.Conclusion))
		if c.IsCLAOrDCO() {
			why = "is a CLA or DCO check, which runs none of the change's code"
		}
		n := prepareRequestNotification(dataDir, r, requestNotice{
			Ask:    "Required check needs a person: no corrective round",
			Detail: fmt.Sprintf("Ticket %d/%d: required check %q on %s %s; no corrective round was started", ticket.Index, r.TicketCount, handoff.CleanSentence(c.Name), ticket.PRURL, why),
			Next:   fmt.Sprintf("review %s", ticket.PRURL),
		}, now)
		notify.DispatchExternal(dataDir, n)
	}
	if !sent {
		return nil
	}
	return r.Save(dataDir)
}

// ciReadiness is checkMergeReadiness's half about the checks: the required
// checks still running, the required set not having been read, and, as
// notes, the checks that failed without being required.
func ciReadiness(state forge.ReviewState) (blockers, notes []string) {
	if len(state.Checks) == 0 {
		return nil, nil
	}
	verdict := forge.ClassifyChecks(state.Checks, state.RequiredUnknown)
	if names := checkNames(verdict.Pending); names != "" {
		blockers = append(blockers, "waiting for checks to complete: "+names)
	}
	if state.RequiredUnknown && len(verdict.Rounds) > 0 {
		blockers = append(blockers, "which checks are required could not be read, so every failed check counts as required")
	}
	if names := checkNames(verdict.Notes); names != "" {
		notes = append(notes, "checks that failed and are not required: "+names)
	}
	return blockers, notes
}

func checkNames(checks []forge.Check) string {
	names := make([]string, 0, len(checks))
	for _, c := range checks {
		names = append(names, fmt.Sprintf("%q", handoff.CleanSentence(c.Name)))
	}
	return strings.Join(names, ", ")
}
