package requestdriver

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"slices"
	"time"

	"buildgate/internal/handoff"
	"buildgate/internal/prorder"
	"buildgate/internal/release"
	"buildgate/internal/request"
	"buildgate/internal/run"
)

// This file holds what a repository's pr_order (.factory.yml) asks of a
// request: a ticket whose accepted run changed files of more than one group
// is carved into one ticket per group, each built as a replay run of its
// carved commit and opening its own stacked pull request; a part the gates
// refuse, or a change that cannot be carved, goes back to planning once.

// orderGroupOf is the 1-based pr_order group a carved ticket is limited to,
// 0 for a ticket that is not carved.
func orderGroupOf(ticket request.Ticket) int {
	if ticket.Order == nil {
		return 0
	}
	return ticket.Order.Group
}

// handleMixedRun carves the request's ticket when its accepted run's release
// decision was denied for pr_order's groups and for nothing else, and no pull
// request was opened. handled is false for any other run. A request already
// sent to planning for size or order, or built from a handed-over plan, is
// halted instead of carved. A change that cannot be carved goes to planning
// (orderToPlanning).
func handleMixedRun(dp Deps, ctx context.Context, dataDir string, r *request.Request, ticket *request.Ticket, record *run.Run, now time.Time) (handled bool, err error) {
	reasons := release.PROrderReasons(*record)
	if len(reasons) == 0 || record.PullRequestURL != "" || ticket.Order != nil {
		return false, nil
	}
	decision, err := release.LoadDecision(dataDir, record.Project, record.ID)
	if err != nil || decision == nil || decision.Allowed || decision.Invalidated || !slices.Equal(decision.Reasons, reasons) {
		return false, nil
	}
	if r.SizeReplan != nil || r.PlanImported {
		return true, HaltRequest(dataDir, r, fmt.Sprintf("run %s: %s; this plan cannot be sent to planning for order again, and is not carved", record.ID, reasons[0]), now)
	}
	if why := carveTicket(dp, ctx, dataDir, r, ticket, record); why != "" {
		return true, orderToPlanning(dataDir, r, record, fmt.Sprintf("run %s: %s; it could not be carved: %s", record.ID, reasons[0], why), now)
	}
	log.Printf("request %s: building -> building (ticket %d/%d carved into %d tickets by pr_order)", r.ID, record.TicketIndex, r.TicketCount, len(r.Tickets))
	return true, r.Save(dataDir)
}

// carveTicket makes one commit per group the run's change touches (on the
// diff base, each on the one before), checks that together they make the
// run's result tree, and replaces ticket with one ticket per commit. It
// returns why it could not, "" when the request now holds the carved tickets.
// The commits are objects and local branches only: no worktree, no push.
func carveTicket(dp Deps, ctx context.Context, dataDir string, r *request.Request, ticket *request.Ticket, record *run.Run) string {
	groups := record.PROrder.Groups
	base, result := record.DiffBase(), record.ResultSHA
	if base == "" || result == "" {
		return "the run records no diff base or result commit"
	}
	changed, err := dp.ChangedPaths(r.Workspace, base, result)
	if err != nil {
		return fmt.Sprintf("list the changed paths: %v", err)
	}
	parts := prorder.Order(groups).Carve(changed)
	if len(parts) < 2 {
		return fmt.Sprintf("the change splits into %d part(s)", len(parts))
	}
	files := make([][]string, len(parts))
	for i, part := range parts {
		files[i] = part.Files
	}
	if err := dp.CarveChain(ctx, r.Workspace, base, result, files); err != nil {
		return fmt.Sprintf("the parts do not make the run's result: %v", err)
	}
	order := prorder.Order(groups)
	numbers, commits, onto := make([]int, len(parts)), make([]string, len(parts)), make([]string, len(parts))
	parent := base
	for i, part := range parts {
		onto[i] = parent
		message := fmt.Sprintf("%s of request %s", order.Label(part.Group), r.ID)
		if commits[i], err = dp.CarveCommit(ctx, r.Workspace, parent, result, part.Files, message); err != nil {
			return fmt.Sprintf("commit %s: %v", order.Label(part.Group), err)
		}
		numbers[i], parent = part.Group+1, commits[i]
	}
	first := ticket.Index
	// Every git effect comes before the request's files move, so a failure or
	// a restart in between leaves the request exactly as it was.
	branches := make([]string, len(parts))
	for i := range parts {
		branches[i] = fmt.Sprintf("buildgate/%s/t%d-carve", r.ID, first+i)
		if err := dp.MoveBranch(r.Workspace, branches[i], commits[i]); err != nil {
			return fmt.Sprintf("branch %s: %v", branches[i], err)
		}
	}
	if err := r.ReplaceWithCarved(dataDir, first, record.ID, groups, numbers); err != nil {
		return err.Error()
	}
	for i := range parts {
		carved := r.Tickets[first-1+i].Order
		carved.Commit, carved.Onto, carved.Branch = commits[i], onto[i], branches[i]
	}
	if err := r.Save(dataDir); err != nil {
		return fmt.Sprintf("save the carved tickets: %v", err)
	}
	return ""
}

// carvedRunRefused sends a carved ticket's request back to planning when the
// ticket's replay run was refused by a gate: quarantined, or refused before
// any gate (replay_refused). handled is false for any other run.
func carvedRunRefused(dataDir string, r *request.Request, ticket *request.Ticket, record *run.Run, reason string, now time.Time) (handled bool, err error) {
	if ticket.Order == nil || record.Replay == nil || record.Replay.Carve == nil {
		return false, nil
	}
	if record.State != run.StateQuarantined && record.HaltReasonCode != run.HaltReasonReplayRefused {
		return false, nil
	}
	if reviewNoVerdictCause(record) != "" {
		// Nothing was judged: a person resumes it (SC-017), the carve stands.
		return false, nil
	}
	source, loadErr := run.Load(dataDir, ticket.Order.SourceRunID)
	why := fmt.Sprintf("the carved commit of pr_order group %d was refused: %s", ticket.Order.Group, reason)
	if loadErr != nil {
		return true, HaltRequest(dataDir, r, why+fmt.Sprintf("; the run it was carved from cannot be read: %v", loadErr), now)
	}
	return true, orderToPlanning(dataDir, r, source, why, now)
}

// orderToPlanning is the one move to planning for order: the request goes
// back with source's measurement and the groups, once. A request that was
// already sent to planning, or whose source run records no measurement or
// groups, halts with why.
func orderToPlanning(dataDir string, r *request.Request, source *run.Run, why string, now time.Time) error {
	if source.PROrder == nil || source.ChangeSize == nil || r.SizeReplan != nil {
		return HaltRequest(dataDir, r, why+"; the request cannot be sent to planning for order", now)
	}
	if err := r.ReturnToPlanningForOrder(source.ID, source.ChangeSize, source.PROrder.Groups, why, now); err != nil {
		return HaltRequest(dataDir, r, why+"; "+err.Error(), now)
	}
	log.Printf("request %s: building -> planning (%s)", r.ID, why)
	return r.Save(dataDir)
}

// carvedBuildArgs is the argv of a carved ticket's build: its ordinary build
// of the ticket's input with no ticket scope (the parts together are what
// the scope judged), -on-branch the carved branch, -diff-base the commit it
// is carved onto and -replay the record of the carve. The run opens the
// ticket's own pull request, stacked on the ticket before it.
func carvedBuildArgs(dataDir string, r *request.Request, ticket request.Ticket, cfg WorkerConfig) ([]string, error) {
	o := ticket.Order
	if o == nil || o.Commit == "" || o.Onto == "" || o.Branch == "" {
		return nil, fmt.Errorf("ticket %d/%d records no carved commit to build", ticket.Index, r.TicketCount)
	}
	source, err := run.Load(dataDir, o.SourceRunID)
	if err != nil {
		return nil, fmt.Errorf("load run %s that ticket %d was carved from: %w", o.SourceRunID, ticket.Index, err)
	}
	replay := &run.Replay{
		RequestID: r.ID, TicketIndex: ticket.Index, OfRunID: source.ID, SourceRunIDs: []string{source.ID},
		UpstreamSHA: source.DiffBase(), SourceHeadSHA: source.ResultSHA, OntoSHA: o.Onto, HeadSHA: o.Commit,
		Commits: []run.ReplayedCommit{{Original: source.ResultSHA, Replayed: o.Commit}},
		Carve:   &run.Carve{Groups: o.Groups, Group: o.Group},
	}
	if ticket.Index == o.First {
		replay.Carve.Notes = mixedChallengeNotes(source)
	}
	file := filepath.Join(request.Dir(dataDir, r.ID), "restack", fmt.Sprintf("t%d-carve.json", ticket.Index))
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		return nil, err
	}
	if err := writeReplayRecord(file, replay); err != nil {
		return nil, fmt.Errorf("write the carve record of ticket %d: %w", ticket.Index, err)
	}
	specPath, err := writeTicketBuildInput(dataDir, r, ticket)
	if err != nil {
		return nil, fmt.Errorf("build spec for request %s ticket %d: %w", r.ID, ticket.Index, err)
	}
	entry, err := ticketQueueEntry(dataDir, r, ticket, cfg, fmt.Sprintf("%s-%03d", r.ID, ticket.Index), specPath)
	if err != nil {
		return nil, err
	}
	entry.NoTicketScope = true
	return append(BuildTicketRunArgs(dataDir, entry, cfg), "-on-branch", o.Branch, "-diff-base", o.Onto, "-replay", file), nil
}

// mixedChallengeNotes are the lines naming the red and broken challenge files
// of a mixed run, which no challenge round followed: its carved tickets run no
// challenger, so the first one's pull request lists them (SC-022).
func mixedChallengeNotes(source *run.Run) []string {
	if source.Challenge == nil || source.Challenge.Rerun {
		return nil
	}
	var notes []string
	for _, f := range source.Challenge.Files {
		var what string
		switch f.Verdict {
		case run.ChallengeRed:
			what = "fails on the mixed change"
		case "broken":
			what = "is broken (a compile or collection error, or no report)"
		default:
			continue
		}
		notes = append(notes, fmt.Sprintf("challenge test `%s` (%s) %s; no challenge round ran for it, because the change was split by pr_order", handoff.CleanSentence(f.Path), handoff.CleanWord(f.Criterion), what))
	}
	return notes
}
