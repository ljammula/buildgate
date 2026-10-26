package requestdriver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"buildgate/internal/forge"
	"buildgate/internal/notify"
	"buildgate/internal/request"
	"buildgate/internal/run"
	"buildgate/internal/workspace"
)

// This file holds the restack of a stacked ticket (safety-contract.md's
// Forge row): when the branch its pull request stacks on has moved, or the
// earlier ticket merged, the host replays the ticket's commits onto the new
// tip on a local branch, a replay run judges them with every gate and review
// and no build, and the result replaces the pull request's head through a
// push with a lease. Nothing in it rebuilds the ticket from its spec.

// restackNeed is what checkRestackNeed found for one ticket: whether it needs
// a restack, and what one would replay. upstream..head.Run.ResultSHA are its
// commits; onto, the tip of ontoBranch on origin, is where they go.
type restackNeed struct {
	needed     bool
	onto       string
	ontoBranch string
	upstream   string
	head       prHead
}

// checkRestackNeed reports whether ticket needs a restack. A ticket's pull
// request stacks on the earlier ticket's branch while that ticket is not
// merged: it needs a restack exactly when the tip of that branch on origin
// is not an ancestor of the ticket's PR head. Once the earlier ticket merged,
// its pull request stacks on the branch it merged into, and it needs one
// exactly when the commit its commits sit on (run.StackBase of its PR-head
// run) is not an ancestor of that branch's tip: a squash or a rebase merge
// leaves it out, a merge commit does not, and the branch moving on later is
// no reason to restack again.
//
// The first ticket, a ticket with no pull request or with a merged or closed
// one, an earlier ticket with no pull request, and a ticket that was not
// built stacked (its PR-head run records no stack base and its pull request
// does not target the earlier ticket's branch) are never restacked. A fact
// that cannot be read is an error, and so is a pull request on the earlier
// ticket's branch whose PR-head run records no stack base (errNoStackBase):
// nothing is decided on either.
func checkRestackNeed(dp Deps, ctx context.Context, dataDir string, r *request.Request, ticket *request.Ticket) (restackNeed, error) {
	if ticket.Index <= 1 || ticket.PRURL == "" || ticket.PRState == "merged" || ticket.PRState == "closed" {
		return restackNeed{}, nil
	}
	earlier, err := TicketAt(r, ticket.Index-1)
	if err != nil || earlier.PRURL == "" {
		return restackNeed{}, err
	}
	head, err := loadPRHead(dataDir, ticket)
	if err != nil {
		return restackNeed{}, err
	}
	if head.Run.ResultSHA == "" {
		return restackNeed{}, fmt.Errorf("ticket %d's PR-head run %s records no result commit", ticket.Index, head.Run.ID)
	}
	need := restackNeed{head: head, upstream: run.StackBase(dataDir, head.Run)}
	if need.upstream == "" {
		if ticket.PRBase != "" && ticket.PRBase == ticketPRBranch(dataDir, earlier) {
			return restackNeed{}, fmt.Errorf("%w: run %s", errNoStackBase, head.Run.ID)
		}
		// Not stacked: built on the branch the earlier ticket merged into.
		return restackNeed{}, nil
	}
	merged := earlier.PRState == "merged"
	need.ontoBranch = ticketPRBranch(dataDir, earlier)
	if merged {
		if need.ontoBranch = earlier.PRBase; need.ontoBranch == "" {
			return restackNeed{}, fmt.Errorf("ticket %d merged into a branch the factory did not record", earlier.Index)
		}
	}
	if need.onto, err = dp.RemoteBranchHeadSHA(ctx, r.Workspace, need.ontoBranch); err != nil {
		return restackNeed{}, err
	}
	if err := dp.FetchBranchCommit(ctx, r.Workspace, need.ontoBranch, need.onto); err != nil {
		return restackNeed{}, err
	}
	ancestor, descendant := need.onto, head.Run.ResultSHA
	if merged {
		ancestor, descendant = need.upstream, need.onto
	}
	under, err := dp.RoundResultDescendsFromHead(r.Workspace, ancestor, descendant)
	if err != nil {
		return restackNeed{}, err
	}
	need.needed = !under
	return need, nil
}

// errNoStackBase is checkRestackNeed's error for a stacked ticket whose
// PR-head run records no stack base (a run recorded before stack bases
// were): which of its commits are its own cannot be told.
var errNoStackBase = errors.New("its PR-head run records no stack base, so which commits under its head are its own cannot be told")

// earlierRestackPending reports whether a ticket before ticket needs a
// restack or has one in flight: the cascade restacks ticket N+1 only after
// ticket N's own restack is pushed and confirmed, so that it replays onto
// ticket N's final head.
func earlierRestackPending(dp Deps, ctx context.Context, dataDir string, r *request.Request, ticket *request.Ticket) (bool, error) {
	for i := 2; i < ticket.Index; i++ {
		earlier, err := TicketAt(r, i)
		if err != nil {
			return false, err
		}
		if earlier.Restack != nil {
			return true, nil
		}
		need, err := checkRestackNeed(dp, ctx, dataDir, r, earlier)
		if err != nil || need.needed {
			return true, err
		}
	}
	return false, nil
}

// restackIfNeeded is the PR poll's restack step for ticket, run before its
// retarget, review rounds and ready flip. prHeadSHA is the head of the
// ticket's pull request (its branch on origin) as the poll read it: when it
// is the result of an accepted replay run of the ticket whose push is not
// confirmed, that push is recorded first (confirmLandedRestack), whether or
// not a restack is needed now. ran is true when it acted on the ticket
// (reconciled an interrupted restack, waited on its replay run that has not
// ended, recorded a landed push, or restacked it), and the poll of this
// ticket is over. held
// is true when the ticket needs a restack that cannot run yet (an earlier
// ticket's comes first), or whether it needs one cannot be told: its pull request
// is then neither retargeted nor marked ready this poll. A need that cannot
// be told is recorded on the ticket (Ticket.RestackHold), shown in the
// request's next action and watch, and reminded at the reminder interval,
// until a poll can tell.
func restackIfNeeded(dp Deps, ctx context.Context, dataDir string, r *request.Request, ticket *request.Ticket, prHeadSHA string, cfg WorkerConfig, now time.Time) (ran, held bool, err error) {
	if ticket.Restack != nil {
		ran, held, err := reconcileRestack(dp, ctx, dataDir, r, ticket, cfg.HitlReminderInterval, now)
		if !held && err == nil && ticket.RestackHold != nil {
			ticket.RestackHold = nil
			err = r.Save(dataDir)
		}
		return ran, held, err
	}
	if confirmed, err := confirmLandedRestack(dp, ctx, dataDir, r, ticket, prHeadSHA, now); confirmed || err != nil {
		return true, false, err
	}
	need, err := checkRestackNeed(dp, ctx, dataDir, r, ticket)
	if err != nil {
		holdRestack(dataDir, r, ticket, restackHoldSentence(r, ticket, err), cfg.HitlReminderInterval, now)
		return false, true, nil
	}
	if ticket.RestackHold != nil {
		log.Printf("request %s: ticket %d: whether it needs a restack can be told again", r.ID, ticket.Index)
		ticket.RestackHold = nil
	}
	if !need.needed {
		return false, false, nil
	}
	if pending, err := earlierRestackPending(dp, ctx, dataDir, r, ticket); err != nil || pending {
		log.Printf("request %s: ticket %d needs a restack onto %s, after an earlier ticket's restack (%v)", r.ID, ticket.Index, shortSHA(need.onto), err)
		return false, true, nil
	}
	return true, false, restackTicket(dp, ctx, dataDir, r, ticket, need, cfg, now)
}

// restackHoldSentence is the operator sentence of a ticket whose restack
// need cannot be told because of err: what could not be read, what is
// held, and what resolves it.
func restackHoldSentence(r *request.Request, ticket *request.Ticket, err error) string {
	held := fmt.Sprintf("ticket %d/%d (%s) is held: whether it needs a restack cannot be told (%v), so its pull request is neither retargeted nor marked ready", ticket.Index, r.TicketCount, ticket.PRURL, err)
	if errors.Is(err, errNoStackBase) {
		return held + fmt.Sprintf(". The factory cannot restack it: once the pull request under it merges, retarget it and mark it ready by hand, or `factoryd cancel %s`", r.ID)
	}
	return held + fmt.Sprintf(". If git cannot reach origin, fix its access (`git ls-remote origin` in %s); the worker reads it again every PR poll (or `factoryd cancel %s`)", r.Workspace, r.ID)
}

// holdRestack records on ticket that whether it needs a restack cannot be
// told (Ticket.RestackHold), keeping the time the hold began, and reminds
// the operator once it has lasted interval since it began or since the last
// reminder. An interval of 0 or less reminds never.
func holdRestack(dataDir string, r *request.Request, ticket *request.Ticket, on string, interval time.Duration, now time.Time) {
	ts := now.UTC().Format(time.RFC3339Nano)
	if ticket.RestackHold == nil {
		ticket.RestackHold = &request.TicketRestackHold{Since: ts}
	}
	if ticket.RestackHold.On != on {
		log.Printf("request %s: %s", r.ID, on)
	}
	ticket.RestackHold.On = on
	from, err := time.Parse(time.RFC3339Nano, ticket.RestackHold.Since)
	if err != nil || interval <= 0 {
		return
	}
	if last, err := time.Parse(time.RFC3339Nano, ticket.RestackHold.RemindedAt); err == nil && last.After(from) {
		from = last
	}
	if now.Sub(from) < interval {
		return
	}
	n := prepareRequestNotification(dataDir, r, requestNotice{
		Ask:    "A stacked pull request is held",
		Detail: on,
		Next:   fmt.Sprintf("fix what it names; the worker checks again every PR poll (or `factoryd cancel %s`)", r.ID),
	}, now)
	notify.DispatchExternal(dataDir, n)
	ticket.RestackHold.RemindedAt = ts
}

// humanCommitPark is the park reason for ticket's branch on origin at remote,
// not at the commit of its PR-head run: someone else's commit is on it.
func humanCommitPark(r *request.Request, ticket *request.Ticket, need restackNeed, remote string) string {
	return fmt.Sprintf("ticket %d/%d's branch %s is at %s on origin, not at %s, the commit of its run %s, so it cannot be restacked onto %s: a commit the factory did not make is on it. Move the branch back to %s, or take the commit out of it", ticket.Index, r.TicketCount, need.head.Branch, shortSHA(remote), shortSHA(need.head.Run.ResultSHA), need.head.Run.ID, shortSHA(need.onto), shortSHA(need.head.Run.ResultSHA))
}

// restackTicket restacks ticket: it checks the host's git (hostGitRefusal),
// the branch on origin and the commits to replay, replays them (replayTicketCommits), judges the result
// with a replay run (judgeRestack) and pushes it (pushRestack). A human
// commit on the branch, a merge commit, or a commit none of the ticket's own
// runs judged parks the request before anything is replayed.
func restackTicket(dp Deps, ctx context.Context, dataDir string, r *request.Request, ticket *request.Ticket, need restackNeed, cfg WorkerConfig, now time.Time) error {
	if why := hostGitRefusal(dp, ctx); why != nil {
		return parkRequest(dp, dataDir, r, fmt.Sprintf("ticket %d/%d cannot be restacked onto %s: %v. Upgrade git to %s or later", ticket.Index, r.TicketCount, shortSHA(need.onto), why, workspace.GitFloor), now)
	}
	head := need.head.Run.ResultSHA
	remote, err := dp.RemoteBranchHeadSHA(ctx, r.Workspace, need.head.Branch)
	if err != nil {
		log.Printf("request %s: ticket %d: read branch %s before a restack: %v; the next poll tries again", r.ID, ticket.Index, need.head.Branch, err)
		return r.Save(dataDir)
	}
	if remote != head {
		if confirmed, err := confirmLandedRestack(dp, ctx, dataDir, r, ticket, remote, now); confirmed || err != nil {
			return err
		}
		return parkRequest(dp, dataDir, r, humanCommitPark(r, ticket, need, remote), now)
	}
	commits, merges, err := dp.RevListRange(r.Workspace, need.upstream, head)
	switch {
	case err != nil:
		return HaltRequest(dataDir, r, fmt.Sprintf("ticket %d/%d: list the commits to restack: %v", ticket.Index, r.TicketCount, err), now)
	case len(merges) > 0:
		return parkRequest(dp, dataDir, r, fmt.Sprintf("ticket %d/%d's commits after %s include merge commit %s, which the factory does not replay, so it cannot be restacked onto %s: take the merge out of branch %s", ticket.Index, r.TicketCount, shortSHA(need.upstream), shortSHA(merges[0]), shortSHA(need.onto), need.head.Branch), now)
	case len(commits) == 0:
		return HaltRequest(dataDir, r, fmt.Sprintf("ticket %d/%d has no commits after %s to restack", ticket.Index, r.TicketCount, shortSHA(need.upstream)), now)
	}
	sources, uncovered, err := restackSources(dp, dataDir, r, ticket, need, commits)
	switch {
	case err != nil:
		return HaltRequest(dataDir, r, fmt.Sprintf("ticket %d/%d: read the runs of its commits before a restack: %v", ticket.Index, r.TicketCount, err), now)
	case uncovered != "":
		return parkRequest(dp, dataDir, r, fmt.Sprintf("ticket %d/%d's commit %s on branch %s is not the work of any of its own runs, so it cannot be restacked onto %s: take it out of the branch", ticket.Index, r.TicketCount, shortSHA(uncovered), need.head.Branch, shortSHA(need.onto)), now)
	}
	replay, file, err := replayTicketCommits(dp, ctx, dataDir, r, ticket, need, commits, sources, now)
	if err != nil || replay == nil {
		return err
	}
	return judgeRestack(dp, ctx, dataDir, r, ticket, need, replay, file, cfg, now)
}

// hostGitRefusal says why the host's git cannot run a restack's git calls
// (workspace.CheckGitFloor): older than workspace.GitFloor, or its version
// cannot be read. nil when it can.
func hostGitRefusal(dp Deps, ctx context.Context) error {
	out, err := dp.HostGitVersion(ctx)
	if err != nil {
		return fmt.Errorf("the host's git version cannot be read (%w)", err)
	}
	return workspace.CheckGitFloor(out)
}

// restackSources are the ticket's own runs whose judged range (from the
// run's diff base, or a replay run's new base, to its result) holds any of
// commits: the run that opened the pull request and every round of it whose
// push was confirmed, the PR-head run always among them. uncovered names a
// commit none of them judged. A source recorded before runs named their
// ticket is given the ticket's index (the request's own record says the run
// is the ticket's), so the replay run's check of it can be made.
func restackSources(dp Deps, dataDir string, r *request.Request, ticket *request.Ticket, need restackNeed, commits []string) (sources []string, uncovered string, err error) {
	candidates := []string{ticket.RunID}
	for _, rnd := range ticket.Rounds {
		if rnd.Outcome == request.RoundAccepted && rnd.Pushed && !slices.Contains(candidates, rnd.RunID) {
			candidates = append(candidates, rnd.RunID)
		}
	}
	wanted := map[string]bool{}
	for _, c := range commits {
		wanted[c] = true
	}
	judged := map[string]bool{}
	for _, id := range candidates {
		rec, err := run.Load(dataDir, id)
		if err != nil {
			return nil, "", err
		}
		base := prHeadDiffBase(rec)
		if base == "" || rec.ResultSHA == "" {
			continue
		}
		owned, _, err := dp.RevListRange(r.Workspace, base, rec.ResultSHA)
		if err != nil {
			return nil, "", err
		}
		holds := id == need.head.Run.ID
		for _, c := range owned {
			if wanted[c] {
				judged[c], holds = true, true
			}
		}
		if !holds {
			continue
		}
		if err := nameRunsTicket(dataDir, r, ticket, rec); err != nil {
			return nil, "", err
		}
		sources = append(sources, id)
	}
	for _, c := range commits {
		if !judged[c] {
			return nil, c, nil
		}
	}
	return sources, "", nil
}

// nameRunsTicket records the ticket on a run of it recorded before runs
// named their ticket (run.Run.TicketIndex 0, the request's own id). The
// record is reloaded and written under the run's lock (run.WithLock), and
// only its TicketIndex is set, so a write the run made meanwhile is kept.
func nameRunsTicket(dataDir string, r *request.Request, ticket *request.Ticket, rec *run.Run) error {
	if rec.TicketIndex != 0 || rec.RequestID != r.ID {
		return nil
	}
	err := run.WithLock(dataDir, rec.ID, func() error {
		cur, err := run.Load(dataDir, rec.ID)
		if err != nil {
			return err
		}
		if cur.TicketIndex != 0 || cur.RequestID != r.ID {
			rec.TicketIndex = cur.TicketIndex
			return nil
		}
		cur.TicketIndex = ticket.Index
		if err := cur.Persist(dataDir); err != nil {
			return err
		}
		rec.TicketIndex = ticket.Index
		log.Printf("request %s: ticket %d: run %s now names its ticket", r.ID, ticket.Index, rec.ID)
		return nil
	})
	return err
}

// maxRestackBranchAttempts bounds the restack branch numbers tried.
const maxRestackBranchAttempts = 20

// restackBranchName is the local-only branch of restack n of ticketIndex:
// buildgate/<request>/t<ticket>-restack<n>. It is pruned with the request's
// other unpushed branches.
func restackBranchName(id string, ticketIndex, n int) string {
	return fmt.Sprintf("buildgate/%s/t%d-restack%d", id, ticketIndex, n)
}

// restackScratchPath is the scratch worktree a ticket's commits are
// replayed in, beside the request's record; one at a time per ticket.
func restackScratchPath(dataDir, id string, ticketIndex int) string {
	return filepath.Join(request.Dir(dataDir, id), "restack", fmt.Sprintf("t%d-worktree", ticketIndex))
}

// replayTicketCommits replays commits onto need.onto on the next free
// restack branch, records the replay in flight on the ticket
// (Ticket.Restack), and writes the replay record the run reads, with each
// pair's patch ids (runner.GitPatchID). A conflict, or a pair whose patch
// ids differ (an original that changes something replayed as a commit that
// changes nothing among them), parks the request in resume_review with the
// reason (restackConflictPark) and returns no replay, before any replay run;
// a replay that fails otherwise halts it.
func replayTicketCommits(dp Deps, ctx context.Context, dataDir string, r *request.Request, ticket *request.Ticket, need restackNeed, commits, sources []string, now time.Time) (*run.Replay, string, error) {
	n := 1
	for _, rnd := range ticket.Rounds {
		if rnd.Kind == request.RestackRoundKind {
			n++
		}
	}
	scratch := restackScratchPath(dataDir, r.ID, ticket.Index)
	if err := os.MkdirAll(filepath.Dir(scratch), 0o700); err != nil {
		return nil, "", err
	}
	var replayed []string
	var err error
	for attempt := 0; attempt < maxRestackBranchAttempts; attempt, n = attempt+1, n+1 {
		ticket.Restack = &request.TicketRestack{N: n, Branch: restackBranchName(r.ID, ticket.Index, n), OntoSHA: need.onto, Since: now.UTC().Format(time.RFC3339Nano)}
		if err = r.Save(dataDir); err != nil {
			return nil, "", err
		}
		replayed, err = dp.ReplayCommits(ctx, r.Workspace, scratch, ticket.Restack.Branch, need.onto, commits)
		if !errors.Is(err, workspace.ErrBranchExists) {
			break
		}
	}
	if err != nil {
		branch := ticket.Restack.Branch
		ticket.Restack = nil
		if errors.Is(err, workspace.ErrReplayConflict) {
			log.Printf("request %s: ticket %d: its commits do not apply onto %s: %v", r.ID, ticket.Index, shortSHA(need.onto), err)
			return nil, "", parkRequest(dp, dataDir, r, restackConflictPark(r, ticket, need, "do not apply cleanly onto it (git's cherry-pick stopped on a conflict, and was aborted)"), now)
		}
		return nil, "", HaltRequest(dataDir, r, fmt.Sprintf("ticket %d/%d: replay its commits onto %s on %s: %v", ticket.Index, r.TicketCount, shortSHA(need.onto), branch, err), now)
	}
	replay := &run.Replay{RequestID: r.ID, TicketIndex: ticket.Index, OfRunID: need.head.Run.ID, SourceRunIDs: sources, UpstreamSHA: need.upstream, SourceHeadSHA: need.head.Run.ResultSHA, OntoSHA: need.onto, HeadSHA: replayed[len(replayed)-1]}
	for i := range commits {
		pair := run.ReplayedCommit{Original: commits[i], Replayed: replayed[i]}
		if pair.OriginalPatchID, err = dp.PatchID(r.Workspace, pair.Original); err == nil {
			pair.ReplayedPatchID, err = dp.PatchID(r.Workspace, pair.Replayed)
		}
		if err != nil {
			ticket.Restack = nil
			return nil, "", HaltRequest(dataDir, r, fmt.Sprintf("ticket %d/%d: patch id of a restacked commit: %v", ticket.Index, r.TicketCount, err), now)
		}
		replay.Commits = append(replay.Commits, pair)
		if what := replayedPairDiffers(pair); what != "" {
			ticket.Restack = nil
			log.Printf("request %s: ticket %d: commit %s replayed onto %s as %s: %s", r.ID, ticket.Index, shortSHA(pair.Original), shortSHA(need.onto), shortSHA(pair.Replayed), what)
			return nil, "", parkRequest(dp, dataDir, r, restackConflictPark(r, ticket, need, fmt.Sprintf("do not replay faithfully onto it: commit %s %s", shortSHA(pair.Original), what)), now)
		}
	}
	file := filepath.Join(filepath.Dir(scratch), fmt.Sprintf("t%d-restack%d.json", ticket.Index, ticket.Restack.N))
	if err := writeReplayRecord(file, replay); err != nil {
		ticket.Restack = nil
		return nil, "", HaltRequest(dataDir, r, fmt.Sprintf("ticket %d/%d: write the restack's replay record: %v", ticket.Index, r.TicketCount, err), now)
	}
	return replay, file, nil
}

// replayedPairDiffers says how a replayed commit is not the same change as
// its original, "" when it is: equal patch ids (both empty only for an
// original that changed nothing).
func replayedPairDiffers(pair run.ReplayedCommit) string {
	switch {
	case pair.OriginalPatchID != "" && pair.ReplayedPatchID == "":
		return "changes something, and its replay changes nothing: the new base already holds the change"
	case pair.OriginalPatchID != pair.ReplayedPatchID:
		return "and its replay are not the same change (their patch ids differ)"
	}
	return ""
}

// restackConflictPark is the park reason of a restack whose commits, how,
// cannot be replayed onto need.onto. Nothing was replayed or judged, and
// nothing pushed. A resume tries the replay again, which succeeds only once
// the branch under the ticket has moved so that the commits replay
// faithfully; the factory never rebuilds the ticket to resolve it, and a
// fix pushed by hand to the ticket's branch is refused as a human commit.
func restackConflictPark(r *request.Request, ticket *request.Ticket, need restackNeed, how string) string {
	return fmt.Sprintf("ticket %d/%d's commits on branch %s cannot be restacked onto %s, the new tip of %s: they %s. Nothing was judged or pushed, and the factory does not rebuild the ticket to resolve it; a fix pushed by hand to %s is refused as a commit the factory did not make. `factoryd resume %s` tries the replay again, which succeeds once %s has moved so the commits replay cleanly; or `factoryd cancel %s`", ticket.Index, r.TicketCount, need.head.Branch, shortSHA(need.onto), need.ontoBranch, how, need.head.Branch, r.ID, need.ontoBranch, r.ID)
}

// writeReplayRecord writes replay to path, refusing one that is not well
// formed (run.Replay.Validate), as the run reading it does.
func writeReplayRecord(path string, replay *run.Replay) error {
	if err := replay.Validate(); err != nil {
		return err
	}
	data, err := json.MarshalIndent(replay, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

// judgeRestack starts the replay run of ticket's restack (-on-branch the
// restack branch, -diff-base the new base, -replay the record, no pull
// request) and records it as the ticket's next round, of kind restack, with
// its round ref. An accepted run is pushed (pushRestack). One that gave no
// verdict on the work waits in resume_review; one that never started is
// tried again on the next poll; any other is quarantined or halted like any
// run, pushes nothing and halts the request.
func judgeRestack(dp Deps, ctx context.Context, dataDir string, r *request.Request, ticket *request.Ticket, need restackNeed, replay *run.Replay, file string, cfg WorkerConfig, now time.Time) error {
	rs := ticket.Restack
	runID, ended, err := runReplay(dp, ctx, dataDir, r, ticket, cfg, rs, file)
	if err != nil {
		ticket.Restack = nil
		return HaltRequest(dataDir, r, fmt.Sprintf("ticket %d/%d: restack %d: %v", ticket.Index, r.TicketCount, rs.N, err), now)
	}
	if ok, err := stillInState(dataDir, r.ID, request.StatePRReview); err != nil || !ok {
		log.Printf("request %s: ticket %d: restack %d finished but the request left pr_review while it ran -- discarding the result", r.ID, ticket.Index, rs.N)
		return err
	}
	return settleRestackRun(dp, ctx, dataDir, r, ticket, need, runID, ended, now)
}

// settleRestackRun records ticket's ended replay run runID as its next
// round, of kind restack, with its round ref, and acts on how it ended: an
// accepted run is pushed (pushRestack); one that gave no verdict on the work
// waits in resume_review; one that never started is tried again on the next
// poll, until maxRestackStartFailures in a row (restackStartFailures) halt
// the request; any other pushes nothing and halts the request.
func settleRestackRun(dp Deps, ctx context.Context, dataDir string, r *request.Request, ticket *request.Ticket, need restackNeed, runID string, ended roundOutcome, now time.Time) error {
	ticket.Restack = nil
	ticket.Rounds = append(ticket.Rounds, request.Round{
		Index:        len(ticket.Rounds) + 1,
		Kind:         request.RestackRoundKind,
		RunID:        runID,
		Outcome:      ended.outcome,
		StartFailure: ended.startFailure,
		Unresolved:   ended.unresolved,
		At:           now.UTC().Format(time.RFC3339Nano),
		Error:        ended.errText,
	})
	tagRoundByID(dp, dataDir, r, ticket, runID, request.RoundTagRestack, now)
	switch {
	case ended.unresolved != "":
		log.Printf("request %s: ticket %d: restack run %s gave no verdict (%s): %s", r.ID, ticket.Index, runID, ended.unresolved, ended.errText)
		return EnterResumeReview(dp, dataDir, r, request.LostStep{Cause: ended.unresolved, RetryAt: ended.retryAt}, now)
	case ended.startFailure:
		if n := restackStartFailures(r, ticket); n >= maxRestackStartFailures {
			return HaltRequest(dataDir, r, fmt.Sprintf("ticket %d/%d: the replay run of its restack onto %s did not start %d times in a row, the last (run %s): %s. Nothing was pushed. `factoryd retry %s` restacks again once the cause is fixed, or `factoryd cancel %s`", ticket.Index, r.TicketCount, shortSHA(need.onto), n, runID, ended.errText, r.ID, r.ID), now)
		}
		log.Printf("request %s: ticket %d: restack run %s did not start (%s); the next poll restacks again", r.ID, ticket.Index, runID, ended.errText)
		return r.Save(dataDir)
	case ended.outcome != request.RoundAccepted:
		return HaltRequest(dataDir, r, fmt.Sprintf("ticket %d/%d's restack onto %s was not accepted: run %s %s (%s); nothing was pushed, and the factory does not rebuild the ticket to fix it. Read the run's report (`factoryd status %s`), then `factoryd retry %s` to restack again once the cause is fixed, or `factoryd cancel %s`", ticket.Index, r.TicketCount, shortSHA(need.onto), runID, ended.outcome, ended.errText, runID, r.ID, r.ID), now)
	}
	return pushRestack(dp, ctx, dataDir, r, ticket, runID, need, now)
}

// maxRestackStartFailures is how many replay runs of a ticket's restack in a
// row may fail to start before the request halts.
const maxRestackStartFailures = 3

// restackStartFailures counts the ticket's last rounds that are restack
// rounds whose replay run never started, recorded since the request last
// entered its state: a `factoryd retry` of the halted request moves it back
// to pr_review, so the count starts again from zero.
func restackStartFailures(r *request.Request, ticket *request.Ticket) int {
	since, sinceOK := r.InStateSince()
	n := 0
	for i := len(ticket.Rounds) - 1; i >= 0; i-- {
		rnd := ticket.Rounds[i]
		if rnd.Kind != request.RestackRoundKind || !rnd.StartFailure {
			break
		}
		if at, err := time.Parse(time.RFC3339Nano, rnd.At); sinceOK && err == nil && at.Before(since) {
			break
		}
		n++
	}
	return n
}

// runReplay runs the replay run of restack rs of ticket: the ticket's own
// build input as its spec, the request's settings for every gate and
// review, no pull request, and -on-branch, -diff-base and -replay. It
// returns the run's id and how it ended (correctiveRoundOutcome), with a
// run refused before any gate (replay_refused) read as having run.
func runReplay(dp Deps, ctx context.Context, dataDir string, r *request.Request, ticket *request.Ticket, cfg WorkerConfig, rs *request.TicketRestack, file string) (string, roundOutcome, error) {
	specPath, err := writeTicketBuildInput(dataDir, r, *ticket)
	if err != nil {
		return "", roundOutcome{}, err
	}
	ticketID := fmt.Sprintf("%s-%03d-restack%d", r.ID, ticket.Index, rs.N)
	// A replay run's review is advice only: it holds and runs no
	// reproduction (no -review-reproduction, -review-spec or
	// -recheck-reproductions), since a replay cannot take a corrective
	// build. The ticket's list stays, for the round after it.
	entry, err := ticketQueueEntry(dataDir, r, *ticket, cfg, ticketID, specPath, false)
	if err != nil {
		return "", roundOutcome{}, err
	}
	entry.IssueRef, entry.PRBase = "", ""
	replayCfg := cfg
	replayCfg.OpenPullRequest = false
	args := append(BuildTicketRunArgs(dataDir, entry, replayCfg), "-on-branch", rs.Branch, "-diff-base", rs.OntoSHA, "-replay", file)
	var startedID string
	runErr := dp.RunTicket(ctx, args, func(started *run.Run) {
		startedID = started.ID
		rs.RunID = started.ID
		if err := r.Save(dataDir); err != nil {
			log.Printf("request %s: ticket %d: record restack run %s: %v", r.ID, ticket.Index, started.ID, err)
		}
	})
	if startedID == "" {
		startedID = ticketID
	}
	ended, err := replayRunOutcome(dataDir, startedID, runErr)
	return startedID, ended, err
}

// replayRunOutcome is how replay run runID ended (correctiveRoundOutcome),
// with a run refused before any gate (replay_refused) read as having run.
func replayRunOutcome(dataDir, runID string, runErr error) (roundOutcome, error) {
	ended, err := correctiveRoundOutcome(dataDir, runID, runErr)
	if err != nil {
		return roundOutcome{}, err
	}
	if rec, loadErr := run.Load(dataDir, runID); loadErr == nil && rec.HaltReasonCode == run.HaltReasonReplayRefused {
		ended.startFailure = false
	}
	return ended, nil
}

// pushRestack pushes ticket's accepted replay run runID to its pull-request
// branch. Before the push: the run's result must be the replayed head or
// descend from it (replayResultRefusal), origin's branch must still be at the commit of
// the ticket's PR-head run (else it is a human commit, and the request
// parks), a ready pull request is turned back to draft
// (draftBeforeFactoryPush), and the run's release decision must allow it.
// The push is leased on the commit read. Whatever the push returned, origin
// is read back: at the run's result, the run becomes the PR-head run
// (Round.Pushed) and the local branch moves to it, even when the push
// reported an error after landing; at the old head, nothing was pushed and
// the request halts; at any other commit, confirmLandedRestack decides, and
// a commit no accepted replay run of the ticket made parks the request as a
// human commit (a failed lease among them). A read-back that fails halts
// the request with the round unpushed; the pass after `factoryd retry`
// finds origin at the run's result and records it then.
func pushRestack(dp Deps, ctx context.Context, dataDir string, r *request.Request, ticket *request.Ticket, runID string, need restackNeed, now time.Time) error {
	rec, err := run.Load(dataDir, runID)
	if err != nil {
		return HaltRequest(dataDir, r, fmt.Sprintf("ticket %d/%d: load restack run %s: %v", ticket.Index, r.TicketCount, runID, err), now)
	}
	if why := replayResultRefusal(dp, r, rec); why != "" {
		return HaltRequest(dataDir, r, fmt.Sprintf("ticket %d/%d: restack run %s: %s; nothing was pushed", ticket.Index, r.TicketCount, runID, why), now)
	}
	branch, old := need.head.Branch, need.head.Run.ResultSHA
	remote, err := dp.RemoteBranchHeadSHA(ctx, r.Workspace, branch)
	switch {
	case err != nil:
		return HaltRequest(dataDir, r, fmt.Sprintf("ticket %d/%d: read branch %s before pushing restack run %s: %v; nothing was pushed", ticket.Index, r.TicketCount, branch, runID, err), now)
	case remote != old:
		if confirmed, err := confirmLandedRestack(dp, ctx, dataDir, r, ticket, remote, now); confirmed || err != nil {
			return err
		}
		return parkRequest(dp, dataDir, r, humanCommitPark(r, ticket, need, remote), now)
	}
	if err := draftBeforeFactoryPush(dp, ctx, ticket); err != nil {
		return HaltRequest(dataDir, r, fmt.Sprintf("ticket %d/%d: turn pull request %s back to draft before pushing restack run %s: %v; nothing was pushed", ticket.Index, r.TicketCount, ticket.PRURL, runID, err), now)
	}
	if allowed, err := decisionStillAllows(dataDir, runID); err != nil || !allowed {
		return HaltRequest(dataDir, r, fmt.Sprintf("ticket %d/%d: restack run %s's release decision does not allow it (%v); withholding the push to %s", ticket.Index, r.TicketCount, runID, err, branch), now)
	}
	pushCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	pushErr := dp.PushWithLease(pushCtx, r.Workspace, rec.ResultSHA, branch, old)
	cancel()
	readCtx, cancelRead := context.WithTimeout(ctx, 2*time.Minute)
	back, readErr := dp.RemoteBranchHeadSHA(readCtx, r.Workspace, branch)
	cancelRead()
	switch {
	case readErr != nil:
		pushed := "pushed"
		if pushErr != nil {
			pushed = fmt.Sprintf("pushed (the push reported: %v)", pushErr)
		}
		return HaltRequest(dataDir, r, fmt.Sprintf("ticket %d/%d: restack run %s (%s) was %s to %s, but origin could not be read back: %v. The pull request's head run stays %s; `factoryd retry %s` polls again, and records the restack as pushed if origin is at %s", ticket.Index, r.TicketCount, runID, shortSHA(rec.ResultSHA), pushed, branch, readErr, need.head.Run.ID, r.ID, shortSHA(rec.ResultSHA)), now)
	case back == old && pushErr != nil:
		return HaltRequest(dataDir, r, fmt.Sprintf("ticket %d/%d: push restack run %s to %s: %v; origin is still at %s, so nothing was pushed", ticket.Index, r.TicketCount, runID, branch, pushErr, shortSHA(old)), now)
	case back == old:
		return HaltRequest(dataDir, r, fmt.Sprintf("ticket %d/%d: pushed restack run %s (%s) to %s, but origin reads %s; the pull request's head run stays %s", ticket.Index, r.TicketCount, runID, shortSHA(rec.ResultSHA), branch, shortSHA(back), need.head.Run.ID), now)
	}
	if pushErr != nil {
		log.Printf("request %s: ticket %d: the push of restack run %s reported %v; origin reads %s", r.ID, ticket.Index, runID, pushErr, shortSHA(back))
	}
	if confirmed, err := confirmLandedRestack(dp, ctx, dataDir, r, ticket, back, now); confirmed || err != nil {
		return err
	}
	return parkRequest(dp, dataDir, r, humanCommitPark(r, ticket, need, back), now)
}

// replayResultRefusal says why replay run rec's result may not be pushed as
// its restack: it records no replay, or its ResultSHA is neither the
// replayed head it judged (run.Replay.HeadSHA) nor a descendant of it (the
// evidence step's commit of what verification left behind). "" when it may.
func replayResultRefusal(dp Deps, r *request.Request, rec *run.Run) string {
	if rec.Replay == nil {
		return "its record holds no replay"
	}
	if rec.ResultSHA == rec.Replay.HeadSHA {
		return ""
	}
	under, err := dp.RoundResultDescendsFromHead(r.Workspace, rec.Replay.HeadSHA, rec.ResultSHA)
	switch {
	case err != nil:
		return fmt.Sprintf("whether its result %s descends from the replayed head %s cannot be told: %v", shortSHA(rec.ResultSHA), shortSHA(rec.Replay.HeadSHA), err)
	case !under:
		return fmt.Sprintf("its result %s does not descend from the replayed head %s it judged", shortSHA(rec.ResultSHA), shortSHA(rec.Replay.HeadSHA))
	}
	return ""
}

// confirmLandedRestack is the one answer to origin's branch of ticket at
// remote, a commit other than the result of the ticket's PR-head run: when
// remote is the ResultSHA of an accepted replay run of this request ticket
// whose push is not yet confirmed (the restack in flight, Ticket.Restack,
// or a restack round recorded after the PR-head run's round), that restack's
// push landed: its round is recorded as pushed (appended, for the one in
// flight), the local branch is moved to remote, the pull request is turned
// back to draft if it is ready (redraftAfterFactoryPush), and it is polled
// again on the next pass. confirmed is false, and nothing is changed, for
// any other commit: the caller treats it as a human commit.
func confirmLandedRestack(dp Deps, ctx context.Context, dataDir string, r *request.Request, ticket *request.Ticket, remote string, now time.Time) (confirmed bool, err error) {
	runID, slice := landedRestackRun(dataDir, r, ticket, remote)
	if runID == "" {
		return false, nil
	}
	if slice < 0 {
		ticket.Rounds = append(ticket.Rounds, request.Round{Index: len(ticket.Rounds) + 1, Kind: request.RestackRoundKind, RunID: runID, Outcome: request.RoundAccepted, Pushed: true, At: now.UTC().Format(time.RFC3339Nano)})
		tagRoundByID(dp, dataDir, r, ticket, runID, request.RoundTagRestack, now)
	} else {
		ticket.Rounds[slice].Pushed = true
	}
	ticket.Restack = nil
	branch := ticketPRBranch(dataDir, ticket)
	if err := dp.MoveBranch(r.Workspace, branch, remote); err != nil {
		log.Printf("request %s: ticket %d: move local branch %s to the restacked head: %v", r.ID, ticket.Index, branch, err)
	}
	ticket.MergeReadiness = nil
	// The next pass polls the pull request at once: a retarget or a ready
	// flip held for this restack is due.
	ticket.LastPolledAt = ""
	log.Printf("request %s: ticket %d: origin's %s is at %s, the result of restack run %s: recorded its push", r.ID, ticket.Index, branch, shortSHA(remote), runID)
	redraftAfterFactoryPush(dp, ctx, r, ticket)
	return true, r.Save(dataDir)
}

// landedRestackRun finds the accepted replay run of ticket whose result is
// remote and whose push is not confirmed: among the ticket's restack rounds
// after the last pushed round (slice its index), then the restack in flight
// (slice -1). "" when there is none.
func landedRestackRun(dataDir string, r *request.Request, ticket *request.Ticket, remote string) (runID string, slice int) {
	landed := func(id string) bool {
		if id == "" || remote == "" {
			return false
		}
		rec, err := run.Load(dataDir, id)
		return err == nil && rec.State == run.StateAccepted && rec.Replay != nil && rec.ResultSHA == remote && rec.RequestID == r.ID && rec.TicketIndex == ticket.Index
	}
	last := -1
	for i, rnd := range ticket.Rounds {
		if rnd.Outcome == request.RoundAccepted && rnd.Pushed {
			last = i
		}
	}
	for i := len(ticket.Rounds) - 1; i > last; i-- {
		rnd := ticket.Rounds[i]
		if rnd.Kind == request.RestackRoundKind && rnd.Outcome == request.RoundAccepted && !rnd.Pushed && landed(rnd.RunID) {
			return rnd.RunID, i
		}
	}
	if ticket.Restack != nil && landed(ticket.Restack.RunID) {
		return ticket.Restack.RunID, -1
	}
	return "", -1
}

// draftBeforeFactoryPush turns ticket's pull request back to draft when it
// is ready for review, before every push the factory makes to its open
// branch (a restack's, and a PR-review round's), so no ready state crosses
// a head the factory changed; the ready flip (advancePRReadyOrApproved)
// runs again on the new head. GitHub's own state is read, not the ticket's:
// a person may have marked it ready. A pull request GitHub reports draft is
// recorded as draft (markTicketDraft) whoever turned it, so a flip whose save
// a crash lost leaves no ready state on the ticket.
func draftBeforeFactoryPush(dp Deps, ctx context.Context, ticket *request.Ticket) error {
	readCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	state, err := dp.ReadReviewState(readCtx, ticket.PRURL, forge.AuthorPolicy{})
	if err != nil {
		return err
	}
	if !state.IsDraft {
		if err := dp.UndoMarkPullRequestReady(readCtx, ticket.PRURL); err != nil {
			return err
		}
	}
	markTicketDraft(ticket)
	return nil
}

// redraftAfterFactoryPush is draftBeforeFactoryPush's second half, run once
// a push the factory made to ticket's open branch is confirmed on origin (a
// restack's, confirmLandedRestack, and a PR-review round's): a person may
// have marked the pull request ready between the flip to draft and the
// push, so GitHub's state is read once more and a ready pull request is
// turned back to draft. A read or a flip that fails is logged: the poll's
// ready flip judges the new head either way, and the merge-readiness check
// reads GitHub's own draft state.
func redraftAfterFactoryPush(dp Deps, ctx context.Context, r *request.Request, ticket *request.Ticket) {
	readCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	state, err := dp.ReadReviewState(readCtx, ticket.PRURL, forge.AuthorPolicy{})
	if err != nil {
		log.Printf("request %s: ticket %d: read pull request %s after the factory's push: %v", r.ID, ticket.Index, ticket.PRURL, err)
		return
	}
	if state.IsDraft {
		markTicketDraft(ticket)
		return
	}
	if err := dp.UndoMarkPullRequestReady(readCtx, ticket.PRURL); err != nil {
		log.Printf("request %s: ticket %d: pull request %s was marked ready before the factory's push landed, and turning it back to draft failed: %v", r.ID, ticket.Index, ticket.PRURL, err)
		return
	}
	markTicketDraft(ticket)
	log.Printf("request %s: ticket %d: pull request %s was marked ready before the factory's push landed; turned it back to draft for the new head", r.ID, ticket.Index, ticket.PRURL)
}

// markTicketDraft records ticket's pull request as draft; a stacked one
// ("stacked", itself a draft) keeps its state.
func markTicketDraft(ticket *request.Ticket) {
	if ticket.PRState != "stacked" {
		ticket.PRState = "draft"
	}
}

// reconcileRestack settles a restack that a pass left in flight
// (Ticket.Restack set: the worker stopped mid-restack). Its replay run is
// adopted, never run twice: while it has not ended, the ticket's poll ends
// there, so no other replay or review round of the ticket starts, and its
// pull request is neither retargeted nor marked ready. Once it has ended:
// origin's branch at its result means its push landed, and is recorded
// (confirmLandedRestack); origin's branch still at the PR-head run's result
// it was replayed from means it was never pushed, and it is settled as if
// this pass had run it (settleRestackRun: an accepted one is pushed, any
// other halts or waits as it would have). Otherwise, with no run started,
// or with a run whose record does not exist, the restack is dropped, and
// the next poll checks again whether one is needed. A run record or an
// origin branch that cannot be read decides nothing: the restack is kept,
// and the ticket is held with its poll ended this pass (as for a need that
// cannot be told), so a push that landed is never taken for a human commit.
func reconcileRestack(dp Deps, ctx context.Context, dataDir string, r *request.Request, ticket *request.Ticket, interval time.Duration, now time.Time) (ran, held bool, err error) {
	rs := ticket.Restack
	if rs.RunID != "" {
		rec, loadErr := run.Load(dataDir, rs.RunID)
		switch {
		case loadErr == nil && !runEnded(rec):
			log.Printf("request %s: ticket %d: restack %d's replay run %s is still %s; no other replay or round of the ticket starts until it ends", r.ID, ticket.Index, rs.N, rec.ID, rec.State)
			return true, false, r.Save(dataDir)
		case loadErr != nil && !errors.Is(loadErr, os.ErrNotExist):
			return holdInterruptedRestack(dataDir, r, ticket, interval, now, fmt.Errorf("its replay run %s cannot be read: %w", rs.RunID, loadErr))
		case loadErr == nil:
			branch := ticketPRBranch(dataDir, ticket)
			remote, err := dp.RemoteBranchHeadSHA(ctx, r.Workspace, branch)
			if err != nil {
				return holdInterruptedRestack(dataDir, r, ticket, interval, now, fmt.Errorf("its branch %s cannot be read on origin: %w", branch, err))
			}
			if confirmed, err := confirmLandedRestack(dp, ctx, dataDir, r, ticket, remote, now); confirmed || err != nil {
				return true, false, err
			}
			if adopted, err := settleAdoptedReplay(dp, ctx, dataDir, r, ticket, rs, rec, remote, now); adopted || err != nil {
				return true, false, err
			}
		}
	}
	ticket.Restack = nil
	ticket.LastPolledAt = ""
	log.Printf("request %s: ticket %d: dropped the interrupted restack %d (%s); the next poll checks again", r.ID, ticket.Index, rs.N, strings.TrimSpace(rs.Branch))
	return true, false, r.Save(dataDir)
}

// holdInterruptedRestack keeps ticket's interrupted restack for the next
// pass because why: what it needs to settle it could not be read. The
// ticket's poll ends this pass, and the hold is recorded and reminded as a
// need that cannot be told is (holdRestack).
func holdInterruptedRestack(dataDir string, r *request.Request, ticket *request.Ticket, interval time.Duration, now time.Time, why error) (ran, held bool, err error) {
	on := fmt.Sprintf("ticket %d/%d (%s) is held: its interrupted restack %d cannot be settled (%v), so its pull request is neither retargeted nor marked ready, and no round starts. If git cannot reach origin, fix its access (`git ls-remote origin` in %s); the worker reads it again every PR poll (or `factoryd cancel %s`)", ticket.Index, r.TicketCount, ticket.PRURL, ticket.Restack.N, why, r.Workspace, r.ID)
	holdRestack(dataDir, r, ticket, on, interval, now)
	return true, true, r.Save(dataDir)
}

// settleAdoptedReplay settles rec, the ended replay run of the interrupted
// restack rs, when it is still the restack the ticket needs: it replayed the
// ticket's current PR-head run, and origin's branch (remote) is still at
// that run's result, so nothing was pushed. adopted is false otherwise.
func settleAdoptedReplay(dp Deps, ctx context.Context, dataDir string, r *request.Request, ticket *request.Ticket, rs *request.TicketRestack, rec *run.Run, remote string, now time.Time) (adopted bool, err error) {
	head, err := loadPRHead(dataDir, ticket)
	if err != nil || rec.Replay == nil || rec.RequestID != r.ID || rec.TicketIndex != ticket.Index || rec.Replay.OfRunID != head.Run.ID || remote != head.Run.ResultSHA {
		return false, nil
	}
	ended, err := replayRunOutcome(dataDir, rec.ID, nil)
	if err != nil {
		return false, nil
	}
	log.Printf("request %s: ticket %d: the interrupted restack %d's replay run %s ended %s; settling it", r.ID, ticket.Index, rs.N, rec.ID, ended.outcome)
	need := restackNeed{needed: true, onto: rs.OntoSHA, upstream: rec.Replay.UpstreamSHA, head: head}
	return true, settleRestackRun(dp, ctx, dataDir, r, ticket, need, rec.ID, ended, now)
}

// runEnded reports whether rec has reached a state its run never leaves:
// accepted, quarantined or halted.
func runEnded(rec *run.Run) bool {
	return rec.State == run.StateAccepted || rec.State == run.StateQuarantined || rec.State == run.StateHalted
}
