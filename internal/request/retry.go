package request

import (
	"fmt"
	"os"
	"time"
)

// PROpenOutcome is what a PROpener (below) reports for one attempt to
// open a pull request against an already-accepted run's own branch --
// see Retry's own "accepted, no PR" case.
type PROpenOutcome struct {
	// PRURL is the opened PR's URL, non-empty exactly on success.
	PRURL string
	// WithheldReason is non-empty when the release decision denies this
	// run outright -- no push/open was even attempted, and retrying
	// again with an unchanged policy will deny again (mirrors
	// cmd/factoryd's own releasePullRequestWithheldReason distinction,
	// used so a caller can tell "fix the policy first" apart from "just
	// try again").
	WithheldReason string
	// Err is the opener's own push/PR-open failure -- empty on success
	// or a WithheldReason denial.
	Err error
}

// PROpener attempts to open a pull request for the already-accepted run
// runID, whose branch exists but has no PR yet. stackBase is the branch the
// PR stacks on now (Request.StackBase), for a run that recorded none: one
// built while the ticket under it had no pull request yet. Injected (may be nil --
// see Retry) so this package never needs run.Load/release.LoadDecision/
// forge.GHPullRequestOpener itself: cmd/factoryd's retryPullRequestOpener
// is the one real implementation, reusing openEvidencePullRequest exactly
// the way the original accept-time open path does (same
// recordReleaseDecision re-check, same forge.GHPullRequestOpener); a test
// supplies a fake instead.
type PROpener func(dataDir, runID, stackBase string) PROpenOutcome

// retryAcceptedNoPR is Retry's own HaltAcceptedNoPR case: for a ticket
// halted with HaltAcceptedNoPR (its run was accepted but no pull request
// exists), a retry used to rebuild the whole ticket -- a fresh, paid
// agent run -- just to re-attempt a `gh pr create` call. This instead
// calls openPR to re-attempt ONLY the PR open against that existing
// accepted run and branch; nil (every real caller supplies
// retryPullRequestOpener) falls back to the old rebuild behavior rather
// than panicking. Factored out of Retry for readability: r.AwaitingPRTicket() picks the SAME ticket
// advancePRReview's own halt loop (internal/requestdriver/pr_review_driver.go)
// would halt on -- the lowest-index ticket with no PR yet, whether or
// not it has ever been built.
//
//   - No such ticket at all (!ok): defensive only -- refuses outright
//     rather than guessing which ticket to rebuild.
//   - The ticket has never been built (RunID == "", possible under
//     advance_on: pr_approved, where a later ticket in r.Tickets can be
//     plan-approved but not yet started while an earlier one sits in
//     pr_review): there is no run for openPR to re-open a PR against --
//     only a rebuild can ever produce one.
//   - Otherwise, openPR is called against that run. A WithheldReason
//     outcome (a release-DENIED run can never be recovered by replaying
//     its own frozen, unchanging decision) also needs a rebuild to let
//     a policy fix take effect.
//
// A round-2 review's own decision on that WithheldReason case: a
// rebuild is only ever safe when the awaiting ticket IS r.TicketIndex's
// own ticket -- rebuild (Retry's
// own local closure, r.Retry) rebuilds whatever r.TicketIndex already
// names, unchanged, so rebuilding for a DIFFERENT ticket would silently
// start a fresh, paid build on the wrong one. When they differ, this
// refuses instead, naming both indices and the actual cause, rather than
// guessing.
func retryAcceptedNoPR(dataDir, id, by, reason string, r *Request, now time.Time, openPR PROpener, rebuild func() error) error {
	awaiting, ok := r.AwaitingPRTicket()
	if !ok {
		return fmt.Errorf("request %s: halted as accepted-awaiting-PR, but no ticket actually matches (an empty PRURL) -- this request's state is inconsistent; a human needs to look at it directly", id)
	}
	if awaiting.RunID == "" {
		if awaiting.Index != r.TicketIndex {
			return fmt.Errorf("request %s: ticket %d has no accepted run yet, and retry can only rebuild the CURRENT ticket (%d) -- cancel and resubmit the request, or wait for ticket %d to build first and retry again", id, awaiting.Index, r.TicketIndex, r.TicketIndex)
		}
		return rebuild()
	}
	outcome := openPR(dataDir, awaiting.RunID, r.StackBase(awaiting.Index))
	switch {
	case outcome.WithheldReason != "":
		if awaiting.Index != r.TicketIndex {
			return fmt.Errorf("request %s: ticket %d's accepted run %s cannot open a pull request (%s), and retry can only rebuild the CURRENT ticket (%d) -- cancel and resubmit the request, or fix the cause so the decision allows it and retry again", id, awaiting.Index, awaiting.RunID, outcome.WithheldReason, r.TicketIndex)
		}
		return rebuild()
	case outcome.Err != nil:
		return fmt.Errorf("request %s: retry could not open a pull request for run %s: %w", id, awaiting.RunID, outcome.Err)
	default:
		awaiting.PRURL = outcome.PRURL
		awaiting.PRState = "open"
		return r.ResumeReview(by, reason, now)
	}
}

// Retry moves a quarantined or halted request back into its build/review/
// drafting pipeline -- the one function `factoryd retry <request-id>`
// (cmd/factoryd/retry.go's own retryRequest, which now delegates here) and
// POST /requests/{id}/retry both call, so neither path can diverge on what
// counts as a legal retry. by names the retrying operator (the CLI's own
// currentOSUser(), or the API's requestAPIPrincipal/operator-supplied
// "by" -- mirrors Approve/Reject's own identity handling) and is recorded
// on the appended History entry.
//
// Only a quarantined or halted request is retryable, and only in the
// shapes the pre-refactor cmd/factoryd retryRequest already recognized:
//   - a halt/quarantine before any ticket existed (TicketCount == 0),
//     or a halt in planning (a re-plan after a plan_review rejection),
//     resumes the drafting stage the artifacts on disk show it reached --
//     planning if an approved spec.md exists, otherwise spec
//     drafting;
//   - a ticket that already has an open PR (TicketIndex names one with a
//     recorded PRURL) resumes pr_review, so a fresh run never tries to
//     open a second PR on the same branch;
//   - any other in-flight ticket (TicketIndex in [0, TicketCount]) is
//     retried at that same index, previous tickets' own recorded run ids
//     and PR URLs left untouched.
//
// Anything else -- not quarantined/halted, or a TicketIndex outside
// [0, TicketCount] -- is refused with an error wrapping
// ErrIllegalTransition, the same sentinel Approve/Reject's own wrong-state
// refusals wrap, so a caller (the API) can map it to 409 without string-
// matching.
//
// reason is the operator's own stated reason for retrying (may be empty --
// `factoryd retry` has no flag for one yet); folded into the appended
// History entry's own text by retryReason, not dropped the way it used to
// be (an adversarial review, 2026-09-24, found: the console already
// requires an operator to type one before it will call POST
// /requests/{id}/retry, but internal/api's own handler decoded it and then
// never passed it anywhere -- this function took no reason parameter at
// all).
func Retry(dataDir, id, by, reason string, now time.Time, openPR PROpener) (*Request, error) {
	return retry(dataDir, id, by, reason, retryChoice{}, now, openPR)
}

// retryChoice is where the operator wants a rebuild to start: scratch drops the
// failed attempt's work (`-from scratch`), round names a ledger round
// (`-from <N>`); neither is the default.
type retryChoice struct {
	scratch bool
	round   int
}

// RetryFromRound is Retry for an operator who wants the ticket rebuilt on a new
// branch at the commit of round n of its ledger (`factoryd retry -from <N>`).
// It refuses a ticket whose branch has been pushed, and a retry that rebuilds
// no ticket.
func RetryFromRound(dataDir, id, by, reason string, n int, now time.Time, openPR PROpener) (*Request, error) {
	if n < 1 {
		return nil, fmt.Errorf("request %s: round %d is not a round number %w", id, n, ErrIllegalTransition)
	}
	return retry(dataDir, id, by, reason, retryChoice{round: n}, now, openPR)
}

// recoveryWithoutBuild checks the operator's choice against the request, then
// returns the recovery of a halt that rebuilds nothing (reopensAReview), nil
// for a retry that rebuilds.
func (r *Request) recoveryWithoutBuild(choice retryChoice) (func(by, reason string, now time.Time) error, error) {
	if choice.round > 0 {
		if err := r.retryFromRoundCheck(choice.round); err != nil {
			return nil, err
		}
	}
	return r.reopensAReview(), nil
}

// reopensAReview is the recovery of a halt that returns the request to a stage
// without rebuilding anything: the transition to run, or nil when the halt is
// retried the ordinary way.
func (r *Request) reopensAReview() func(by, reason string, now time.Time) error {
	if r.State != StateHalted {
		return nil
	}
	switch r.HaltKind {
	case HaltGateExpired:
		return r.ReopenReview
	}
	return nil
}

// RetryFromScratch is Retry for an operator who wants the ticket rebuilt
// from the base commit whatever the quarantined attempt left
// (`factoryd retry -from scratch`): a rebuild it leads to never starts on
// that attempt's branch (Request.RetryFromScratch). It changes nothing for a
// retry that rebuilds no ticket.
func RetryFromScratch(dataDir, id, by, reason string, now time.Time, openPR PROpener) (*Request, error) {
	return retry(dataDir, id, by, reason, retryChoice{scratch: true}, now, openPR)
}

func retry(dataDir, id, by, reason string, choice retryChoice, now time.Time, openPR PROpener) (*Request, error) {
	unlock, err := Lock(dataDir, id)
	if err != nil {
		return nil, err
	}
	defer unlock()
	r, err := Load(dataDir, id)
	if err != nil {
		return nil, err
	}
	if r.State == StateResumeReview {
		return nil, fmt.Errorf("request %s: cannot retry a request waiting in %q: run `factoryd resume %s` to continue the lost step, `factoryd resume -from scratch %s` to start it over, or `factoryd cancel %s` %w", id, r.State, id, id, id, ErrIllegalTransition)
	}
	if r.State != StateQuarantined && r.State != StateHalted {
		return nil, fmt.Errorf("request %s: cannot retry from state %q (must be %q or %q) %w", id, r.State, StateQuarantined, StateHalted, ErrIllegalTransition)
	}
	if reopen, err := r.recoveryWithoutBuild(choice); err != nil {
		return nil, err
	} else if reopen != nil {
		if err := reopen(by, reason, now); err != nil {
			return nil, err
		}
		if err := r.Save(dataDir); err != nil {
			return nil, err
		}
		return r, nil
	}
	if r.TicketCount == 0 || r.HaltedFrom() == StatePlanning {
		// Halted before any ticket existed (a spec-drafting or planning
		// failure), or halted in a re-plan after a plan_review rejection,
		// which keeps the rejected plan's TicketCount but none of its
		// tickets.
		return resumeDraftingAfterHalt(dataDir, r, by, reason, now)
	}
	// TicketIndex 0 is valid here: a request halted at the very start of
	// building (e.g. an approved-hash mismatch, which is refused
	// before ticket 1 is ever marked started) has not advanced the index
	// yet, and advanceBuilding defaults an index below 1 to ticket 1.
	if r.TicketIndex < 0 || r.TicketIndex > r.TicketCount {
		return nil, fmt.Errorf("request %s: ticket index %d is out of range [0,%d] %w", id, r.TicketIndex, r.TicketCount, ErrIllegalTransition)
	}
	// A ticket that already has a PR was halted out of pr_review (review
	// rounds exhausted, a rejected push): resume
	// watching that PR rather than rebuilding the ticket, which would try
	// to open a second PR on the same branch.
	var ticket *Ticket
	if r.TicketIndex >= 1 && r.TicketIndex-1 < len(r.Tickets) {
		ticket = &r.Tickets[r.TicketIndex-1]
	}
	rebuild := func() error { return r.retryRebuild(by, reason, choice, now) }
	switch {
	case r.HaltKind == HaltAcceptedNoPR && openPR != nil:
		// Found via review: this case must be checked BEFORE "ticket has
		// a PR" below, not after -- a
		// 3-ticket request where ticket 1 lacks a PR but r.TicketIndex's
		// own ticket (say ticket 3) already has one used to fall into
		// the ResumeReview case below instead, which just resumes
		// polling and immediately halts again on ticket 1 at the very
		// next poll (ticket 3's own already-open PR was never the
		// problem this halt was about).
		if err := retryAcceptedNoPR(dataDir, id, by, reason, r, now, openPR, rebuild); err != nil {
			return nil, err
		}
	case ticket != nil && ticket.PRURL != "":
		if err := r.ResumeReview(by, reason, now); err != nil {
			return nil, err
		}
	default:
		// No PR yet and no accepted run to re-open one for (or no
		// PROpener was given): rebuilt from scratch.
		if err := rebuild(); err != nil {
			return nil, err
		}
	}
	if err := r.Save(dataDir); err != nil {
		return nil, err
	}
	return r, nil
}

// retryRebuild is Request.Retry plus the operator's choice of where the
// rebuild starts, recorded after the transition (which drops an earlier
// retry's choice).
func (r *Request) retryRebuild(by, reason string, choice retryChoice, now time.Time) error {
	if err := r.Retry(by, reason, now); err != nil {
		return err
	}
	r.RetryFromScratch = choice.scratch
	r.RetryFromRound = choice.round
	if choice.scratch || choice.round > 0 {
		r.RetryTicket = max(r.TicketIndex, 1)
	}
	return nil
}

// StackBase is the branch ticket index's pull request stacks on: the
// previous ticket's branch while that ticket's pull request is open and
// unmerged, else "" (the default branch). A request's first ticket has no
// predecessor to stack on.
func (r *Request) StackBase(index int) string {
	if index <= 1 || index > len(r.Tickets) {
		return ""
	}
	prev := r.Tickets[index-2]
	if prev.PRURL != "" && prev.PRState != "merged" && prev.Branch != "" {
		return prev.Branch
	}
	return ""
}

// resumeDraftingAfterHalt resumes the drafting stage the artifacts on disk
// show r reached: planning once an approved spec.md exists, otherwise spec
// drafting.
func resumeDraftingAfterHalt(dataDir string, r *Request, by, reason string, now time.Time) (*Request, error) {
	target := StateSpecDrafting
	if _, err := os.Stat(SpecPath(dataDir, r.ID)); err == nil && r.ApprovedSHA256[specFileName] != "" {
		target = StatePlanning
	}
	if err := r.ResumeDrafting(target, by, reason, now); err != nil {
		return nil, err
	}
	if err := r.Save(dataDir); err != nil {
		return nil, err
	}
	return r, nil
}
