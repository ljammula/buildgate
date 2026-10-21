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
		BaseSHA:      runRecord.BaseSHA,
		DiffBaseSHA:  runRecord.DiffBaseSHA,
		Outcome:      outcome,
		FailedChecks: roundFailedChecks(runRecord),
		At:           now.UTC().Format(time.RFC3339Nano),
	})
	if !ok {
		return
	}
	if err := dp.UpdateRef(repo, tag.Ref, tag.SHA); err != nil {
		log.Printf("request %s: ticket %d: could not keep round %d's commit under %s: %v", r.ID, ticket.Index, tag.N, tag.Ref, err)
		return
	}
	r.AppendRoundTag(ticket.Index, tag)
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
	if r.RetryFromRound > 0 && resumeRunID == "" {
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
	tag, ok := ticket.RoundTagN(r.RetryFromRound)
	if !ok || ticket.Pushed() {
		return args, "", false, fmt.Errorf("ticket %d/%d: retry from round %d: that round is not in the ledger, or the ticket's branch was pushed", ticket.Index, r.TicketCount, r.RetryFromRound)
	}
	previous, loadErr := run.Load(dataDir, tag.RunID)
	if loadErr != nil || previous.RequestID != r.ID {
		previous = nil
	}
	branch, err := createRoundBranch(dp, roundRepoDir(r, previous), r.ID, ticket.Index, tag)
	if err != nil {
		return args, "", false, fmt.Errorf("ticket %d/%d: retry from round %d: %w", ticket.Index, r.TicketCount, tag.N, err)
	}
	if previous != nil {
		if doc, ok := recordableHandoff(dataDir, previous, args); ok {
			if path, ok := writeEarlierRecord(dataDir, r, ticket, previous, doc, "retry", startsOnItsBranch); ok {
				args = append(args, "-earlier-attempt", path)
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
