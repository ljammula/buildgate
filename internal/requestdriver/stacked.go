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
// ticket's current PR-head run. With no pull request (none was opened) or a
// merged one there is nothing to check. Otherwise the pull request is read:
// closed without merging parks; open needs its branch on origin at the
// commit of Ticket.PRHeadRunID, which checkEarlierTicketBranch checks.
func checkEarlierTicket(dp Deps, ctx context.Context, dataDir string, r *request.Request, idx int, cfg WorkerConfig) (stackHold, error) {
	if idx <= 1 {
		return stackHold{}, nil
	}
	prev, err := TicketAt(r, idx-1)
	if err != nil {
		return stackHold{}, err
	}
	if prev.PRURL == "" || prev.PRState == "merged" {
		return stackHold{}, nil
	}
	state, err := dp.ReadReviewState(ctx, prev.PRURL, prAuthorPolicy(cfg))
	if err != nil {
		return stackHold{wait: fmt.Sprintf("ticket %d/%d waits: ticket %d's pull request %s could not be read (%v); the next pass checks again", idx, r.TicketCount, prev.Index, prev.PRURL, err)}, nil
	}
	switch {
	case state.MergedAt != nil || state.State == forge.PRStateMerged:
		recordTicketMerged(prev)
		return stackHold{}, r.Save(dataDir)
	case state.State == forge.PRStateClosed:
		prev.PRState = "closed"
		return stackHold{park: fmt.Sprintf("ticket %d/%d's pull request %s was closed without merging, and ticket %d builds on it: reopen it", prev.Index, r.TicketCount, prev.PRURL, idx)}, nil
	case state.State != forge.PRStateOpen:
		return stackHold{wait: fmt.Sprintf("ticket %d/%d waits: ticket %d's pull request %s reported state %q; the next pass reads it again", idx, r.TicketCount, prev.Index, prev.PRURL, state.State)}, nil
	}
	noteReopenedPR(prev, state)
	return checkEarlierTicketBranch(dp, ctx, dataDir, r, idx, prev)
}

// checkEarlierTicketBranch checks that prev's open pull-request branch on
// origin is at the commit of prev's current PR-head run. A branch origin no
// longer has, under a pull request that is not merged, parks; a branch at
// another commit (one the factory did not push) is a wait.
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
		return stackHold{wait: fmt.Sprintf("ticket %d/%d waits: ticket %d's branch %s could not be read on origin (%v); the next pass checks again", idx, r.TicketCount, prev.Index, head.Branch, err)}, nil
	case remote != head.Run.ResultSHA:
		return stackHold{wait: fmt.Sprintf("ticket %d/%d waits: ticket %d's branch %s is at %s on origin, not at %s, the commit of its run %s that ticket %d builds on; the next pass checks again", idx, r.TicketCount, prev.Index, head.Branch, shortSHA(remote), shortSHA(head.Run.ResultSHA), head.Run.ID, idx)}, nil
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
		return true, parkRequest(dataDir, r, hold.park, now)
	case hold.wait != "":
		return true, recordStackedWait(dataDir, r, hold.wait, now)
	}
	r.StackedWait = nil
	return false, nil
}

// recordStackedWait records why the current ticket's build waits, keeping
// the time the wait began.
func recordStackedWait(dataDir string, r *request.Request, reason string, now time.Time) error {
	since := now.UTC().Format(time.RFC3339Nano)
	if r.StackedWait != nil && r.StackedWait.Since != "" {
		since = r.StackedWait.Since
	}
	if r.StackedWait == nil || r.StackedWait.On != reason {
		log.Printf("request %s: %s", r.ID, reason)
	}
	r.StackedWait = &request.StackedWait{Since: since, On: reason}
	return r.Save(dataDir)
}

// parkRequest moves r to resume_review for an earlier ticket an operator must
// repair. No step was lost: the resume reruns the step, which checks again.
// No ticket's merge readiness holds while the request is parked.
func parkRequest(dataDir string, r *request.Request, reason string, now time.Time) error {
	for i := range r.Tickets {
		r.Tickets[i].MergeReadiness = nil
	}
	return EnterResumeReview(dataDir, r, request.LostStep{Reason: reason}, now)
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

// loadPRHead loads ticket's PRHeadRunID. The branch is the run's own;
// the ticket's, then "factoryd/<run id>", stand in for records that predate
// run.Run.Branch. The diff base is the run's DiffBase: an earlier round's
// -diff-base, carried forward, or the first build's own base, which is the
// earlier ticket's tip the ticket was built on.
func loadPRHead(dataDir string, ticket *request.Ticket) (prHead, error) {
	id := ticket.PRHeadRunID()
	if id == "" {
		return prHead{}, fmt.Errorf("ticket %d records no run", ticket.Index)
	}
	rec, err := run.Load(dataDir, id)
	if err != nil {
		return prHead{}, fmt.Errorf("load ticket %d's current PR-head run %s: %w", ticket.Index, id, err)
	}
	head := prHead{Run: rec, Branch: rec.Branch, DiffBase: rec.DiffBase()}
	if head.Branch == "" {
		head.Branch = ticket.Branch
	}
	if head.Branch == "" {
		head.Branch = "factoryd/" + id
	}
	return head, nil
}

// stackBaseOf is the earlier ticket's tip record's commits sit on: its
// StackBaseSHA, or, for a first build recorded before that field, its
// -prior-run's result. "" for a run that is not stacked, or a round recorded
// before the field, whose tip cannot be recovered without guessing.
func stackBaseOf(dataDir string, record *run.Run) string {
	if record == nil {
		return ""
	}
	if record.StackBaseSHA != "" || record.PriorRunID == "" {
		return record.StackBaseSHA
	}
	prior, err := run.Load(dataDir, record.PriorRunID)
	if err != nil {
		return ""
	}
	return prior.ResultSHA
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
	return stackBaseOf(dataDir, rec)
}
