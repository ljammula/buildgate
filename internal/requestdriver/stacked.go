package requestdriver

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"buildgate/internal/forge"
	"buildgate/internal/request"
	"buildgate/internal/run"
)

// This file holds what a stacked ticket (ticket N+1, whose commits sit on
// ticket N's) needs from the ticket under it: the run it chains from, the
// check that ticket N's pull-request branch is still at that run's commit,
// and the stack base each of its runs records.

// ErrRemoteBranchNotFound is RemoteBranchHeadSHA's error for a branch origin
// does not have. A stacked request parks for it while the branch's pull
// request is not merged; any other read failure is a wait, checked again on
// the next pass.
var ErrRemoteBranchNotFound = errors.New("remote branch not found")

// stackHold is what the earlier ticket says about building the current one
// now: wait (the next pass checks again) or park (an operator must repair the
// earlier ticket first). Both empty: build.
type stackHold struct {
	wait string
	park string
}

// checkEarlierTicket decides whether ticket idx may be built on the earlier
// ticket's current PR-head run. Every earlier ticket is read, since ticket
// idx's commits sit on all of theirs: one closed without merging, or open
// with its branch gone from origin, parks (checkEarlierPR). The ticket just
// before idx must also have its open branch at the commit of its
// Ticket.PRHeadRunID, else idx waits (checkEarlierTicketBranch). A park
// outranks a wait; the first wait found is the one recorded.
func checkEarlierTicket(dp Deps, ctx context.Context, dataDir string, r *request.Request, idx int, cfg WorkerConfig) (stackHold, error) {
	var wait string
	for i := 1; i < idx; i++ {
		earlier, err := TicketAt(r, i)
		if err != nil {
			return stackHold{}, err
		}
		hold, err := checkEarlierPR(dp, ctx, dataDir, r, idx, earlier, cfg)
		if err != nil || hold.park != "" {
			return hold, err
		}
		if wait == "" {
			wait = hold.wait
		}
	}
	return stackHold{wait: wait}, nil
}

// checkEarlierPR checks one earlier ticket of ticket idx. With no pull
// request (none was opened) or a merged one there is nothing to check.
// Otherwise the pull request is read: closed without merging parks; open
// needs its branch on origin (checkEarlierTicketBranch).
func checkEarlierPR(dp Deps, ctx context.Context, dataDir string, r *request.Request, idx int, earlier *request.Ticket, cfg WorkerConfig) (stackHold, error) {
	if earlier.PRURL == "" || earlier.PRState == "merged" {
		return stackHold{}, nil
	}
	state, err := dp.ReadReviewState(ctx, earlier.PRURL, prAuthorPolicy(cfg))
	if err != nil {
		return stackHold{wait: fmt.Sprintf("ticket %d/%d waits: ticket %d's pull request %s could not be read (%v). If gh is not logged in or cannot reach GitHub, fix that (`gh auth status`); the worker reads it again every PR poll", idx, r.TicketCount, earlier.Index, earlier.PRURL, err)}, nil
	}
	switch {
	case state.MergedAt != nil || state.State == forge.PRStateMerged:
		recordTicketMerged(earlier)
		return stackHold{}, r.Save(dataDir)
	case state.State == forge.PRStateClosed:
		earlier.PRState = "closed"
		return stackHold{park: fmt.Sprintf("ticket %d/%d's pull request %s was closed without merging, and ticket %d builds on it: reopen it", earlier.Index, r.TicketCount, earlier.PRURL, idx)}, nil
	case state.State != forge.PRStateOpen:
		return stackHold{wait: fmt.Sprintf("ticket %d/%d waits: ticket %d's pull request %s reported state %q; the worker reads it again every PR poll", idx, r.TicketCount, earlier.Index, earlier.PRURL, state.State)}, nil
	}
	noteReopenedPR(earlier, state)
	return checkEarlierTicketBranch(dp, ctx, dataDir, r, idx, earlier)
}

// checkEarlierTicketBranch checks prev's open pull-request branch on origin.
// A branch origin no longer has, under a pull request that is not merged,
// parks. For the ticket just before idx, the branch must be at the commit of
// prev's current PR-head run: at another commit (one the factory did not
// push) idx waits, and the sentence says what the operator does about it.
func checkEarlierTicketBranch(dp Deps, ctx context.Context, dataDir string, r *request.Request, idx int, prev *request.Ticket) (stackHold, error) {
	head, err := loadPRHead(dataDir, prev)
	if err != nil {
		return stackHold{}, err
	}
	if head.Run.ResultSHA == "" {
		return stackHold{}, fmt.Errorf("ticket %d's current PR-head run %s records no result commit", prev.Index, head.Run.ID)
	}
	remote, err := dp.RemoteBranchHeadSHA(ctx, r.Workspace, head.Branch)
	switch {
	case errors.Is(err, ErrRemoteBranchNotFound):
		return stackHold{park: fmt.Sprintf("ticket %d/%d's branch %s is gone from origin while its pull request %s is not merged, and ticket %d builds on it: restore the branch", prev.Index, r.TicketCount, head.Branch, prev.PRURL, idx)}, nil
	case err != nil:
		return stackHold{wait: fmt.Sprintf("ticket %d/%d waits: ticket %d's branch %s could not be read on origin (%v). If git cannot reach origin, fix its access (`git ls-remote origin` in %s); the worker reads it again every PR poll", idx, r.TicketCount, prev.Index, head.Branch, err, r.Workspace)}, nil
	case prev.Index == idx-1 && remote != head.Run.ResultSHA:
		return stackHold{wait: fmt.Sprintf("ticket %d/%d waits: ticket %d's branch %s is at %s on origin, not at %s, the commit of its run %s that ticket %d builds on. A commit the factory did not make is on it: move the branch back to %s (`git push --force-with-lease=refs/heads/%s:%s origin %s:refs/heads/%s`), or merge ticket %d's pull request, after which ticket %d builds on the merge, or `factoryd cancel %s`; the worker checks again every PR poll", idx, r.TicketCount, prev.Index, head.Branch, shortSHA(remote), shortSHA(head.Run.ResultSHA), head.Run.ID, idx, shortSHA(head.Run.ResultSHA), head.Branch, remote, head.Run.ResultSHA, head.Branch, prev.Index, idx, r.ID)}, nil
	}
	return stackHold{}, nil
}

// holdForEarlierTicket applies checkEarlierTicket to a build about to start:
// held is true when the build must not start (the request was parked, or
// records the wait). A check that could not be made halts the request.
func holdForEarlierTicket(dp Deps, ctx context.Context, dataDir string, r *request.Request, idx int, cfg WorkerConfig, now time.Time) (held bool, err error) {
	hold, err := checkEarlierTicket(dp, ctx, dataDir, r, idx, cfg)
	switch {
	case err != nil:
		return true, HaltRequest(dataDir, r, fmt.Sprintf("ticket %d/%d: %v", idx, r.TicketCount, err), now)
	case hold.park != "":
		return true, parkRequest(dp, dataDir, r, hold.park, now)
	case hold.wait != "":
		return true, recordStackedWait(dataDir, r, hold.wait, cfg.HitlReminderInterval, now)
	}
	if r.StackedWait != nil {
		// The wait is over: its reminders end with it.
		r.StackedWait = nil
		r.ClearReminderState()
	}
	return false, nil
}

// recordStackedWait records why the current ticket's build waits, keeping
// the time the wait began, and reminds the operator once the wait has lasted
// the reminder interval since it began or since the last reminder
// (stackedWaitReminderDue), as at a review gate.
func recordStackedWait(dataDir string, r *request.Request, reason string, interval time.Duration, now time.Time) error {
	since := now.UTC().Format(time.RFC3339Nano)
	if r.StackedWait != nil && r.StackedWait.Since != "" {
		since = r.StackedWait.Since
	} else {
		// A new wait: reminders count from its start, not from a reminder
		// sent at an earlier gate.
		r.ClearReminderState()
	}
	if r.StackedWait == nil || r.StackedWait.On != reason {
		log.Printf("request %s: %s", r.ID, reason)
	}
	r.StackedWait = &request.StackedWait{Since: since, On: reason}
	if stackedWaitReminderDue(r, interval, now) {
		RemindRequest(dataDir, r, now)
	}
	return r.Save(dataDir)
}

// stackedWaitReminderDue reports whether a stacked wait is due a reminder at
// now: interval has passed since the wait began, and since its last
// reminder. An interval of 0 or less reminds never.
func stackedWaitReminderDue(r *request.Request, interval time.Duration, now time.Time) bool {
	if interval <= 0 || r.StackedWait == nil {
		return false
	}
	from, err := time.Parse(time.RFC3339Nano, r.StackedWait.Since)
	if err != nil {
		return false
	}
	if last, err := time.Parse(time.RFC3339Nano, r.LastNotifiedAt); err == nil && last.After(from) {
		from = last
	}
	return now.Sub(from) >= interval
}

// parkRequest moves r to resume_review for an earlier ticket an operator must
// repair. No step was lost: the resume reruns the step, which checks again.
// A park that interrupts the operator's resume of a lost build (a pending
// ResumeRound decision) keeps that build's run, cause and retry time, so the
// resume after the repair still continues the kept build instead of
// rebuilding the ticket. No ticket's merge readiness holds while the request
// is parked.
func parkRequest(dp Deps, dataDir string, r *request.Request, reason string, now time.Time) error {
	for i := range r.Tickets {
		r.Tickets[i].MergeReadiness = nil
	}
	step := request.LostStep{Reason: reason}
	if r.PendingResumeVerb() == request.ResumeRound && r.Resume.LostRunID != "" {
		step.RunID, step.Cause, step.RetryAt = r.Resume.LostRunID, r.Resume.Cause, r.Resume.RetryAt
	}
	return EnterResumeReview(dp, dataDir, r, step, now)
}

// recordTicketMerged records that ticket's pull request merged.
func recordTicketMerged(ticket *request.Ticket) {
	ticket.PRState = "merged"
	ticket.MergeReadiness = nil
}

// noteReopenedPR clears a closed ticket's recorded state once its pull
// request reads open again (an operator reopened it), so it is handled as
// the draft or ready pull request it is.
func noteReopenedPR(ticket *request.Ticket, state forge.ReviewState) {
	if ticket.PRState != "closed" || state.State != forge.PRStateOpen {
		return
	}
	ticket.PRState = "draft"
	if !state.IsDraft {
		ticket.PRState = "ready"
	}
}

// prAuthorPolicy is the review-thread author policy the worker reads pull
// requests with.
func prAuthorPolicy(cfg WorkerConfig) forge.AuthorPolicy {
	return forge.AuthorPolicy{Trusted: cfg.PrTrustedAuthors, Ignore: cfg.PrIgnoreAuthors}
}

// prHead is a ticket's current PR-head run and what a round started on it
// builds from: the branch it pushed and the diff base the ticket's gates
// judge the whole pull request against.
type prHead struct {
	Run      *run.Run
	Branch   string
	DiffBase string
}

// loadPRHead loads ticket's PRHeadRunID. The branch is the ticket's pull-
// request branch (ticketPRBranch), never the run's own: a replay run judged
// its commits on a local restack branch, and the pull request's branch was
// moved to its result only after the push was confirmed. The diff base is
// prHeadDiffBase: an earlier round's -diff-base, carried forward, the first
// build's own base (the earlier ticket's tip the ticket was built on), or a
// replay run's new base.
func loadPRHead(dataDir string, ticket *request.Ticket) (prHead, error) {
	id := ticket.PRHeadRunID()
	if id == "" {
		return prHead{}, fmt.Errorf("ticket %d records no run", ticket.Index)
	}
	rec, err := run.Load(dataDir, id)
	if err != nil {
		return prHead{}, fmt.Errorf("load ticket %d's current PR-head run %s: %w", ticket.Index, id, err)
	}
	return prHead{Run: rec, Branch: ticketPRBranch(dataDir, ticket), DiffBase: prHeadDiffBase(rec)}, nil
}

// ticketPRBranch is the branch of ticket's pull request: the branch its
// accepted build pushed and the pull request was opened from
// (request.Ticket.Branch), which every push, remote read and round of the
// ticket uses. For a record from before that field, the branch of the
// ticket's own build (ticket.RunID's run.Run.Branch), then
// "factoryd/<run id>".
func ticketPRBranch(dataDir string, ticket *request.Ticket) string {
	if ticket.Branch != "" {
		return ticket.Branch
	}
	if rec, err := run.Load(dataDir, ticket.RunID); err == nil && rec.Branch != "" {
		return rec.Branch
	}
	return "factoryd/" + ticket.RunID
}

// prHeadDiffBase is the diff base a round started on run record keeps: the
// commit a replay run's commits were replayed onto, else the run's DiffBase
// (an earlier round's -diff-base, or a first build's own base).
func prHeadDiffBase(record *run.Run) string {
	if record.Replay != nil {
		return record.Replay.OntoSHA
	}
	return record.DiffBase()
}

// continuedStackBase is the stack base a ticket build copies from the run it
// continues: the run whose kept worktree it adopts, or, on an existing
// branch, the earlier attempt it continues (the ticket's last run when no
// record of one is passed). A build chained with -prior-run records that
// run's result itself, so this is "" for it.
func continuedStackBase(dataDir string, ticket *request.Ticket, recordOf, resumeRunID string, onBranch bool) string {
	from := resumeRunID
	if from == "" && onBranch {
		from = recordOf
		if from == "" {
			from = ticket.RunID
		}
	}
	if from == "" {
		return ""
	}
	rec, err := run.Load(dataDir, from)
	if err != nil {
		return ""
	}
	return run.StackBase(dataDir, rec)
}

// staleStackBase says why work of ticket idx recorded in record (a kept build
// to resume, or an attempt to continue on its branch) no longer sits on the
// earlier ticket's current PR head: the earlier ticket's head moved (a review
// round or a restack was pushed) after it was built. "" when it still does,
// for the first ticket, and when either commit is not recorded (a record
// from before stack bases), which is checked as it was before.
func staleStackBase(dataDir string, r *request.Request, idx int, record *run.Run) string {
	if idx <= 1 {
		return ""
	}
	prev, err := TicketAt(r, idx-1)
	if err != nil {
		return ""
	}
	head, err := run.Load(dataDir, prev.PRHeadRunID())
	if err != nil {
		return ""
	}
	base := run.StackBase(dataDir, record)
	if base == "" || head.ResultSHA == "" || base == head.ResultSHA {
		return ""
	}
	return fmt.Sprintf("run %s sits on %s, but ticket %d's pull-request head is now %s (run %s): it is built on ticket %d's old head", record.ID, shortSHA(base), prev.Index, shortSHA(head.ResultSHA), head.ID, prev.Index)
}
