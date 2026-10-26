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
	"buildgate/internal/request"
	"buildgate/internal/run"
	"buildgate/internal/unresolved"
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
// one, an earlier ticket with no pull request, and a PR-head run recorded
// before stack bases were are never restacked. A fact that cannot be read is
// an error: nothing is decided on it.
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
// retarget, review rounds and ready flip. ran is true when it acted on the
// ticket (reconciled an interrupted restack, or restacked it), and the poll
// of this ticket is over. held is true when the ticket needs a restack that
// cannot run yet (an earlier ticket's comes first), or whether it needs one
// cannot be told: its pull request is then neither retargeted nor marked
// ready this poll.
func restackIfNeeded(dp Deps, ctx context.Context, dataDir string, r *request.Request, ticket *request.Ticket, cfg WorkerConfig, now time.Time) (ran, held bool, err error) {
	if ticket.Restack != nil {
		return true, false, reconcileRestack(dp, ctx, dataDir, r, ticket, now)
	}
	need, err := checkRestackNeed(dp, ctx, dataDir, r, ticket)
	if err != nil {
		log.Printf("request %s: ticket %d: whether it needs a restack could not be told (%v); its pull request is neither retargeted nor marked ready this poll", r.ID, ticket.Index, err)
		return false, true, nil
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

// humanCommitPark is the park reason for ticket's branch on origin at remote,
// not at the commit of its PR-head run: someone else's commit is on it.
func humanCommitPark(r *request.Request, ticket *request.Ticket, need restackNeed, remote string) string {
	return fmt.Sprintf("ticket %d/%d's branch %s is at %s on origin, not at %s, the commit of its run %s, so it cannot be restacked onto %s: a commit the factory did not make is on it. Move the branch back to %s, or take the commit out of it", ticket.Index, r.TicketCount, need.head.Branch, shortSHA(remote), shortSHA(need.head.Run.ResultSHA), need.head.Run.ID, shortSHA(need.onto), shortSHA(need.head.Run.ResultSHA))
}

// restackTicket restacks ticket: it checks the branch on origin and the
// commits to replay, replays them (replayTicketCommits), judges the result
// with a replay run (judgeRestack) and pushes it (pushRestack). A human
// commit on the branch, a merge commit, or a commit none of the ticket's own
// runs judged parks the request before anything is replayed.
func restackTicket(dp Deps, ctx context.Context, dataDir string, r *request.Request, ticket *request.Ticket, need restackNeed, cfg WorkerConfig, now time.Time) error {
	head := need.head.Run.ResultSHA
	remote, err := dp.RemoteBranchHeadSHA(ctx, r.Workspace, need.head.Branch)
	if err != nil {
		log.Printf("request %s: ticket %d: read branch %s before a restack: %v; the next poll tries again", r.ID, ticket.Index, need.head.Branch, err)
		return r.Save(dataDir)
	}
	if remote != head {
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
// named their ticket (run.Run.TicketIndex 0, the request's own id).
func nameRunsTicket(dataDir string, r *request.Request, ticket *request.Ticket, rec *run.Run) error {
	if rec.TicketIndex != 0 || rec.RequestID != r.ID {
		return nil
	}
	rec.TicketIndex = ticket.Index
	log.Printf("request %s: ticket %d: run %s now names its ticket", r.ID, ticket.Index, rec.ID)
	return rec.Persist(dataDir)
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
// pair's patch ids. A conflict parks the request in resume_review with cause
// restack_conflict and returns no replay; a replay that fails otherwise
// halts it.
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
			return nil, "", EnterResumeReview(dp, dataDir, r, request.LostStep{Cause: unresolved.RestackConflict}, now)
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
	}
	file := filepath.Join(filepath.Dir(scratch), fmt.Sprintf("t%d-restack%d.json", ticket.Index, ticket.Restack.N))
	if err := writeReplayRecord(file, replay); err != nil {
		ticket.Restack = nil
		return nil, "", HaltRequest(dataDir, r, fmt.Sprintf("ticket %d/%d: write the restack's replay record: %v", ticket.Index, r.TicketCount, err), now)
	}
	return replay, file, nil
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
		log.Printf("request %s: ticket %d: restack run %s did not start (%s); the next poll restacks again", r.ID, ticket.Index, runID, ended.errText)
		return r.Save(dataDir)
	case ended.outcome != request.RoundAccepted:
		return HaltRequest(dataDir, r, fmt.Sprintf("ticket %d/%d's restack onto %s was not accepted: run %s %s (%s); nothing was pushed, and the factory does not rebuild the ticket to fix it. Read the run's report (`factoryd status %s`), then `factoryd retry %s` to restack again once the cause is fixed, or `factoryd cancel %s`", ticket.Index, r.TicketCount, shortSHA(need.onto), runID, ended.outcome, ended.errText, runID, r.ID, r.ID), now)
	}
	return pushRestack(dp, ctx, dataDir, r, ticket, len(ticket.Rounds)-1, runID, need, now)
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
	entry, err := ticketQueueEntry(dataDir, r, *ticket, cfg, ticketID, specPath)
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
	ended, err := correctiveRoundOutcome(dataDir, startedID, runErr)
	if err != nil {
		return startedID, roundOutcome{}, err
	}
	if rec, loadErr := run.Load(dataDir, startedID); loadErr == nil && rec.HaltReasonCode == run.HaltReasonReplayRefused {
		ended.startFailure = false
	}
	return startedID, ended, nil
}

// pushRestack pushes ticket's accepted replay run runID to its pull-request
// branch. Before the push: origin's branch must still be at the commit of
// the ticket's PR-head run (else it is a human commit, and the request
// parks), a ready pull request is turned back to draft, and the run's
// release decision must allow it. The push is leased on the commit read; a
// lease that fails pushes nothing and parks as a human commit. The run
// becomes the PR-head run (Round.Pushed) only when origin reads back its
// result, and only then is the local branch moved to it.
func pushRestack(dp Deps, ctx context.Context, dataDir string, r *request.Request, ticket *request.Ticket, slice int, runID string, need restackNeed, now time.Time) error {
	rec, err := run.Load(dataDir, runID)
	if err != nil {
		return HaltRequest(dataDir, r, fmt.Sprintf("ticket %d/%d: load restack run %s: %v", ticket.Index, r.TicketCount, runID, err), now)
	}
	branch, old := need.head.Branch, need.head.Run.ResultSHA
	remote, err := dp.RemoteBranchHeadSHA(ctx, r.Workspace, branch)
	switch {
	case err != nil:
		return HaltRequest(dataDir, r, fmt.Sprintf("ticket %d/%d: read branch %s before pushing restack run %s: %v; nothing was pushed", ticket.Index, r.TicketCount, branch, runID, err), now)
	case remote != old:
		return parkRequest(dp, dataDir, r, humanCommitPark(r, ticket, need, remote), now)
	}
	if err := draftBeforeRestackPush(dp, ctx, ticket); err != nil {
		return HaltRequest(dataDir, r, fmt.Sprintf("ticket %d/%d: turn pull request %s back to draft before pushing restack run %s: %v; nothing was pushed", ticket.Index, r.TicketCount, ticket.PRURL, runID, err), now)
	}
	if allowed, err := decisionStillAllows(dataDir, runID); err != nil || !allowed {
		return HaltRequest(dataDir, r, fmt.Sprintf("ticket %d/%d: restack run %s's release decision does not allow it (%v); withholding the push to %s", ticket.Index, r.TicketCount, runID, err, branch), now)
	}
	pushCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	pushErr := dp.PushWithLease(pushCtx, r.Workspace, rec.ResultSHA, branch, old)
	cancel()
	if pushErr != nil {
		if after, readErr := dp.RemoteBranchHeadSHA(ctx, r.Workspace, branch); readErr == nil && after != old {
			return parkRequest(dp, dataDir, r, humanCommitPark(r, ticket, need, after), now)
		}
		return HaltRequest(dataDir, r, fmt.Sprintf("ticket %d/%d: push restack run %s to %s: %v", ticket.Index, r.TicketCount, runID, branch, pushErr), now)
	}
	back, err := dp.RemoteBranchHeadSHA(ctx, r.Workspace, branch)
	if err != nil || back != rec.ResultSHA {
		return HaltRequest(dataDir, r, fmt.Sprintf("ticket %d/%d: pushed restack run %s (%s) to %s, but origin reads %s (%v); the pull request's head run stays %s", ticket.Index, r.TicketCount, runID, shortSHA(rec.ResultSHA), branch, shortSHA(back), err, need.head.Run.ID), now)
	}
	ticket.Rounds[slice].Pushed = true
	if err := dp.MoveBranch(r.Workspace, branch, rec.ResultSHA); err != nil {
		log.Printf("request %s: ticket %d: move local branch %s to the restacked head: %v", r.ID, ticket.Index, branch, err)
	}
	ticket.MergeReadiness = nil
	// The next pass polls the pull request at once: a retarget or a ready
	// flip held for this restack is due.
	ticket.LastPolledAt = ""
	log.Printf("request %s: ticket %d: restacked onto %s and pushed %s to %s (run %s)", r.ID, ticket.Index, shortSHA(need.onto), shortSHA(rec.ResultSHA), branch, runID)
	return r.Save(dataDir)
}

// draftBeforeRestackPush turns ticket's pull request back to draft when it
// is ready for review, so no pull request is left ready across a head the
// factory changed. The ticket's state says whether the factory made it
// ready; a person may have, so a draft the factory did not record is not
// assumed.
func draftBeforeRestackPush(dp Deps, ctx context.Context, ticket *request.Ticket) error {
	readCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	state, err := dp.ReadReviewState(readCtx, ticket.PRURL, forge.AuthorPolicy{})
	if err != nil {
		return err
	}
	if state.IsDraft {
		return nil
	}
	if err := dp.UndoMarkPullRequestReady(readCtx, ticket.PRURL); err != nil {
		return err
	}
	ticket.PRState = "draft"
	return nil
}

// reconcileRestack settles a restack that a pass left in flight
// (Ticket.Restack set: the worker stopped mid-restack). When its replay run
// was accepted and origin's branch is at the run's result, the push landed:
// the round is recorded as pushed, as pushRestack would have. Otherwise the
// restack is dropped, and the next poll checks again whether one is needed.
func reconcileRestack(dp Deps, ctx context.Context, dataDir string, r *request.Request, ticket *request.Ticket, now time.Time) error {
	rs := ticket.Restack
	ticket.Restack = nil
	ticket.LastPolledAt = ""
	if rs.RunID != "" {
		rec, err := run.Load(dataDir, rs.RunID)
		branch := ticketPRBranch(dataDir, ticket)
		if err == nil && rec.State == run.StateAccepted && rec.Replay != nil && rec.ResultSHA != "" {
			if remote, err := dp.RemoteBranchHeadSHA(ctx, r.Workspace, branch); err == nil && remote == rec.ResultSHA {
				ticket.Rounds = append(ticket.Rounds, request.Round{Index: len(ticket.Rounds) + 1, Kind: request.RestackRoundKind, RunID: rec.ID, Outcome: request.RoundAccepted, Pushed: true, At: now.UTC().Format(time.RFC3339Nano)})
				tagRoundByID(dp, dataDir, r, ticket, rec.ID, request.RoundTagRestack, now)
				if err := dp.MoveBranch(r.Workspace, branch, rec.ResultSHA); err != nil {
					log.Printf("request %s: ticket %d: move local branch %s to the restacked head: %v", r.ID, ticket.Index, branch, err)
				}
				log.Printf("request %s: ticket %d: the interrupted restack %d's push had landed (run %s); recorded it", r.ID, ticket.Index, rs.N, rec.ID)
				return r.Save(dataDir)
			}
		}
	}
	log.Printf("request %s: ticket %d: dropped the interrupted restack %d (%s); the next poll checks again", r.ID, ticket.Index, rs.N, strings.TrimSpace(rs.Branch))
	return r.Save(dataDir)
}
