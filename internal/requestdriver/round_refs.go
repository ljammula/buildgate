package requestdriver

import (
	"errors"
	"fmt"
	"log"
	"time"

	"buildgate/internal/request"
	"buildgate/internal/run"
	"buildgate/internal/workspace"
)

// roundTagOutcome is the ledger word for a run's terminal state, "" for a run
// that is not in one.
func roundTagOutcome(s run.State) string {
	switch s {
	case run.StateAccepted:
		return string(request.RoundAccepted)
	case run.StateQuarantined:
		return string(request.RoundQuarantined)
	case run.StateHalted:
		return string(request.RoundHalted)
	}
	return ""
}

// roundFailedChecks are the checks of runRecord's gates that did not pass.
func roundFailedChecks(runRecord *run.Run) []string {
	var names []string
	for _, gate := range runRecord.GateResults {
		if !gate.Passed {
			names = append(names, gate.Check)
		}
	}
	return names
}

// roundRepoDir is the target repository's directory for the refs and branches
// of a round: the repository the run was made in, else the request's workspace.
func roundRepoDir(r *request.Request, runRecord *run.Run) string {
	if runRecord != nil && runRecord.RepositoryRoot != "" {
		return runRecord.RepositoryRoot
	}
	return r.Workspace
}

// tagTicketRound records the round runRecord ended as the next entry of
// ticket's ledger and writes its ref, refs/buildgate/rounds/<request>/t<n>/r<k>,
// in the target repository: that keeps the round's commit reachable after the
// branch is deleted, for `factoryd retry -from <k>`. Only a round that ended in
// a commit of its own is recorded (request.RoundTagFor). The ref is local and
// is never pushed. It does not save r: the caller does. A failure to write the
// ref is logged and nothing is recorded; the ledger is an aid, never a
// condition of the build.
func tagTicketRound(dp Deps, r *request.Request, ticket *request.Ticket, runRecord *run.Run, kind string, now time.Time) {
	outcome := roundTagOutcome(runRecord.State)
	repo := roundRepoDir(r, runRecord)
	if dp == nil || outcome == "" || repo == "" {
		return
	}
	tag, ok := r.RoundTagFor(ticket.Index, request.RoundTag{
		RunID:        runRecord.ID,
		Kind:         kind,
		SHA:          runRecord.ResultSHA,
		BaseSHA:      roundStart(runRecord),
		DiffBaseSHA:  runRecord.DiffBaseSHA,
		Outcome:      outcome,
		FailedChecks: roundFailedChecks(runRecord),
		At:           now.UTC().Format(time.RFC3339Nano),
	})
	if !ok {
		return
	}
	tag, err := createRoundRef(dp, repo, r.ID, ticket.Index, tag)
	if err != nil {
		log.Printf("request %s: ticket %d: could not keep round %d's commit under %s: %v", r.ID, ticket.Index, tag.N, tag.Ref, err)
		return
	}
	tag.Repo = repo
	r.AppendRoundTag(ticket.Index, tag)
}

// roundStart is the commit a round's own work starts from, for its ledger
// entry: the run's BaseSHA, or for a replay run, whose BaseSHA is the replayed
// head it judges, the commit the replay is onto, so the replayed commits are
// the round's own and it gets a ref of its own.
func roundStart(runRecord *run.Run) string {
	if runRecord.Replay != nil {
		return runRecord.Replay.OntoSHA
	}
	return runRecord.BaseSHA
}

// maxRoundRefAttempts bounds the round numbers createRoundRef tries.
const maxRoundRefAttempts = 1000

// createRoundRef creates tag's ref, never moving one that exists: a ticket's
// ledger starts again after a send-back re-plans it, while the refs of its
// earlier plan still keep their commits, so a taken name moves the round to
// the next free number.
func createRoundRef(dp Deps, repo, id string, ticketIndex int, tag request.RoundTag) (request.RoundTag, error) {
	for attempt := 0; attempt < maxRoundRefAttempts; attempt++ {
		candidate := tag
		candidate.N = tag.N + attempt
		candidate.Ref = request.RoundRef(id, ticketIndex, candidate.N)
		err := dp.CreateRef(repo, candidate.Ref, candidate.SHA)
		if err == nil {
			return candidate, nil
		}
		if !errors.Is(err, workspace.ErrRefExists) {
			return candidate, err
		}
	}
	return tag, fmt.Errorf("every round ref name from %s is taken", tag.Ref)
}

// tagRoundByID is tagTicketRound for the run named by runID, read from disk.
func tagRoundByID(dp Deps, dataDir string, r *request.Request, ticket *request.Ticket, runID, kind string, now time.Time) {
	runRecord, err := run.Load(dataDir, runID)
	if err != nil {
		log.Printf("request %s: ticket %d: round of run %s is not in the ledger: %v", r.ID, ticket.Index, runID, err)
		return
	}
	tagTicketRound(dp, r, ticket, runRecord, kind, now)
}

// maxRoundBranchAttempts bounds the numbered branch names withRoundBranch tries.
const maxRoundBranchAttempts = 20

// roundBranchName is the branch a retry from round n of ticket builds on:
// buildgate/<request>/t<ticket>-from-r<n>, then -2, -3 when an earlier retry
// from the same round left its branch somewhere else.
func roundBranchName(id string, ticketIndex, n, attempt int) string {
	name := fmt.Sprintf("buildgate/%s/t%d-from-r%d", id, ticketIndex, n)
	if attempt > 1 {
		name += fmt.Sprintf("-%d", attempt)
	}
	return name
}

// createRoundBranch creates the branch for a retry from tag at tag.SHA.
func createRoundBranch(dp Deps, repo string, id string, ticketIndex int, tag request.RoundTag) (string, error) {
	for attempt := 1; attempt <= maxRoundBranchAttempts; attempt++ {
		name := roundBranchName(id, ticketIndex, tag.N, attempt)
		err := dp.CreateBranchAt(repo, name, tag.SHA)
		if err == nil {
			return name, nil
		}
		if !errors.Is(err, workspace.ErrBranchExists) {
			return "", err
		}
	}
	return "", fmt.Errorf("every branch name for a retry from round %d is taken", tag.N)
}

// startingPoint adds to a ticket build's argv where it starts and what it is
// told: after `factoryd retry -from <N>` (and not a resume of a lost build,
// which has its own decision) the round-N branch; otherwise the earlier
// attempt's record, as withEarlierAttemptOf says. Then the earlier tickets'
// work (withEarlierTicketsWork).
func startingPoint(dp Deps, dataDir string, r *request.Request, ticket *request.Ticket, args []string, resumeRunID string) (argv []string, recordOf string, onBranch bool, err error) {
	if _, round := r.RetryChoiceFor(ticket.Index); round > 0 && resumeRunID == "" {
		argv, recordOf, onBranch, err = withRoundBranch(dp, dataDir, r, ticket, args)
	} else {
		argv, recordOf, onBranch = withEarlierAttemptOf(dataDir, r, ticket, args, resumeRunID)
	}
	if err == nil {
		argv, err = withEarlierTicketsWork(r, ticket.Index, argv, resumeRunID, onBranch)
	}
	return argv, recordOf, onBranch, err
}

// withRoundBranch makes a build start on a new branch at round r.RetryFromRound's
// commit: -on-branch is that branch and -diff-base the round's own diff base, so
// the gates judge the ticket's cumulative diff as they did then. The old branch
// is untouched and stays reachable through the round refs. When the round was a
// failure a build is told about, the build is given its record, saying the
// attempt's commit is in the workspace.
func withRoundBranch(dp Deps, dataDir string, r *request.Request, ticket *request.Ticket, args []string) (argv []string, recordOf string, onBranch bool, err error) {
	_, round := r.RetryChoiceFor(ticket.Index)
	tag, ok := ticket.RoundTagN(round)
	if !ok || ticket.Pushed() {
		return args, "", false, fmt.Errorf("ticket %d/%d: retry from round %d: that round is not in the ledger, or the ticket's branch was pushed", ticket.Index, r.TicketCount, round)
	}
	previous, loadErr := run.Load(dataDir, tag.RunID)
	if loadErr != nil || previous.RequestID != r.ID {
		previous = nil
	}
	if tag.Repo == "" {
		return args, "", false, fmt.Errorf("ticket %d/%d: retry from round %d: the round records no repository", ticket.Index, r.TicketCount, tag.N)
	}
	branch, err := createRoundBranch(dp, tag.Repo, r.ID, ticket.Index, tag)
	if err != nil {
		return args, "", false, fmt.Errorf("ticket %d/%d: retry from round %d: %w", ticket.Index, r.TicketCount, tag.N, err)
	}
	if previous != nil {
		if doc, ok := recordableHandoff(dataDir, previous, args); ok {
			if path, ok := writeEarlierRecord(dataDir, r, ticket, previous, doc, "retry", startsOnItsBranch); ok {
				args = recheckBeside(dataDir, r, ticket, previous, path, append(args, "-earlier-attempt", path))
				recordOf = previous.ID
			}
		}
	}
	diffBase := tag.DiffBaseSHA
	if diffBase == "" {
		diffBase = tag.BaseSHA
	}
	log.Printf("request %s: ticket %d/%d: rebuilding on %s, at round %d's commit %s", r.ID, ticket.Index, r.TicketCount, branch, tag.N, tag.SHA)
	return append(args, "-on-branch", branch, "-diff-base", diffBase), recordOf, true, nil
}

// PruneRoundRefs deletes request r's round refs and its local branches, except
// the retry-from branches of its pushed tickets, from every repository its
// ledger names.
// A request that has reached done, closed or cancelled needs neither: they
// exist so an operator can retry from a round. A branch of a pushed ticket stays
// (it is the pull request's). Failures are logged, never returned: the request
// is already over.
func PruneRoundRefs(dp Deps, r *request.Request) {
	if dp == nil {
		return
	}
	repos := map[string][]string{}
	for i := range r.Tickets {
		ticket := &r.Tickets[i]
		for _, tag := range ticket.RoundTags {
			if tag.Repo == "" {
				continue
			}
			if _, ok := repos[tag.Repo]; !ok {
				repos[tag.Repo] = nil
			}
			if ticket.Pushed() {
				// A pushed ticket's pull request may be on a retry-from
				// branch; any other local branch of it (a replay branch,
				// buildgate/<request>/t<n>-restack<k>) is never pushed by
				// that name and goes.
				repos[tag.Repo] = append(repos[tag.Repo], fmt.Sprintf("buildgate/%s/t%d-from-r", r.ID, ticket.Index))
			}
		}
	}
	for repo, keep := range repos {
		if err := dp.PruneRoundRefs(repo, r.ID, keep); err != nil {
			log.Printf("request %s: round refs in %s not pruned: %v", r.ID, repo, err)
		}
	}
}
