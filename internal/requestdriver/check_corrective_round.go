package requestdriver

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"buildgate/internal/evidence"
	"buildgate/internal/handoff"
	"buildgate/internal/request"
	"buildgate/internal/run"
)

// TryCheckCorrectiveRound is the counterpart of TryReviewCorrectiveRound for
// a ticket run quarantined by checks other than the two reviews alone: when
// every check it failed is one a build can fix when told about it (the
// run's handoff says so, internal/handoff), it runs one more build of the
// same ticket on the quarantined run's branch and gives that build the
// factory's record of the failed attempt.
//
// What makes a round eligible is correctableByABuild. What the round is
// given: the ticket's own build spec, unchanged, as -spec, and the handoff
// rendered as text as -earlier-attempt, which only the build's first prompt
// receives. The record is never part of the spec, so the round's own
// reviews judge the result against the same spec and criteria as the first
// build's, and see nothing the earlier build or its checks wrote.
//
// Budget, launch, recording and outcomes are runCorrectiveRounds', shared
// with the review round: -review-corrective-rounds is one budget per
// ticket build for both kinds, the launch budget is checked before each
// round, an accepted round is the ticket's accepted run, and a round that
// quarantines again runs again only while the budget lasts and only when
// this same kind of round still applies to it. A round of one kind is
// never followed by a round of the other: a check round whose result only
// the reviews fail quarantines the request.
func TryCheckCorrectiveRound(dp Deps, ctx context.Context, dataDir string, r *request.Request, ticket *request.Ticket, runRecord *run.Run, cfg WorkerConfig, now time.Time) (handled bool, err error) {
	plan := correctivePlan{
		kind: request.CorrectiveRoundKind, suffix: "corrective", label: "corrective round",
		eligible: func(quarantined *run.Run) bool {
			_, ok := correctableByABuild(dataDir, quarantined)
			return ok
		},
		args: func(current *run.Run, roundIndex int, roundRunID, diffBase string) ([]string, string, error) {
			doc, ok := correctableByABuild(dataDir, current)
			if !ok {
				return nil, "", fmt.Errorf("run %s has no handoff a build may be given", current.ID)
			}
			recordPath, err := writeEarlierAttemptRecord(dataDir, r.ID, ticket, doc, roundIndex)
			if err != nil {
				return nil, "", err
			}
			buildSpecPath, err := writeTicketBuildSpecFile(dataDir, r, *ticket)
			if err != nil {
				return nil, "", fmt.Errorf("build spec: %w", err)
			}
			argv, err := BuildReviewCorrectiveArgs(dataDir, r, *ticket, cfg, buildSpecPath, roundRunID, current.Branch, diffBase)
			if err != nil {
				return nil, "", err
			}
			return append(argv, "-earlier-attempt", recordPath), "failed " + failedCheckNames(doc), nil
		},
	}
	return runCorrectiveRounds(dp, ctx, dataDir, r, ticket, runRecord, cfg, now, plan)
}

// correctableByABuild loads a quarantined run's handoff and reports whether
// a build may be given it: the run recorded one, the file is the one it
// recorded and describes the state the run is in (handoff.Load), and what
// its failed checks allow is "corrective".
//
// A failed reference oracle (handoff.BinCorrectiveIfOracleInLoop) counts as
// corrective here: a request's ticket only has an oracle when it was
// approved at plan review, and every such ticket's build is already run
// with the oracle mounted and -reference-oracle-in-loop-retry
// (BuildTicketRunArgs), so the build this round starts sees nothing the
// first one did not. Everything else is not eligible: a check a build is
// never told about (tests_added, a review with no verdict, an unknown
// check), a halt, a run with no handoff, and a run only the reviews
// failed (TryReviewCorrectiveRound's case).
func correctableByABuild(dataDir string, quarantined *run.Run) (handoff.Document, bool) {
	// A quarantine by the two reviews alone is the review round's: when
	// that round found nothing to address, neither is there here.
	if reviewShapeOnly(quarantined) {
		return handoff.Document{}, false
	}
	return handoffForABuild(dataDir, quarantined)
}

// handoffForABuild loads a quarantined run's handoff when a build may be
// given it: the run recorded one, the file is the one it recorded and
// describes the state the run is in (handoff.Load), and what its failed
// checks allow is "corrective" (see correctableByABuild for the reference
// oracle).
func handoffForABuild(dataDir string, quarantined *run.Run) (handoff.Document, bool) {
	if quarantined.State != run.StateQuarantined || quarantined.HandoffSHA256 == "" {
		return handoff.Document{}, false
	}
	doc, err := handoff.Load(run.Dir(dataDir, quarantined.ID), quarantined.HandoffSHA256, quarantined.State)
	if err != nil {
		return handoff.Document{}, false
	}
	if doc.Next != handoff.BinCorrective && doc.Next != handoff.BinCorrectiveIfOracleInLoop {
		return handoff.Document{}, false
	}
	return doc, true
}

// withEarlierAttemptOf gives a ticket's build the factory's record of the
// attempt it follows, and returns the id of the run that record is of ("" when
// the build is given none), for the started run to carry
// (run.Run.EarlierAttemptOf).
//
// A rebuild after `factoryd retry`: the ticket's last run is the quarantined
// one, and when its handoff is one a build may be given (handoffForABuild)
// the fresh build is told what that attempt failed on instead of starting
// from the spec alone. The rebuild itself is unchanged: a new run from the
// base, the ticket's own spec, every gate again.
//
// A build that follows one which never finished: the ticket's last run
// halted (its worker was lost, its sandbox could not start) and its own
// build had been given a record, as a corrective round or a retry's rebuild
// is. The build that follows it, resumed in its kept worktree (`factoryd
// resume`, resumeRunID) or started again from the base (`resume -from
// scratch`, `retry`), is given the record of that same attempt. The record
// is loaded again from that attempt's handoff, under every check of the
// first time; it is never copied from what the halted run was handed, and
// the halted run itself has nothing a build is told.
//
// Anything else adds nothing: a ticket's first build, a run that was
// accepted, a halted run that was given no record, a run of another
// request, a ticket whose spec has changed since (sameSpec), a failure a
// build is never told about, a handoff the run does not vouch for. An error
// writing the record is logged and the build goes ahead without it; the
// record is an aid, never a condition.
func withEarlierAttemptOf(dataDir string, r *request.Request, ticket *request.Ticket, args []string, resumeRunID string) (argv []string, recordOf string) {
	lastID := ticket.RunID
	if resumeRunID != "" {
		lastID = resumeRunID
	}
	if lastID == "" {
		return args, ""
	}
	previous, err := run.Load(dataDir, lastID)
	// The record of another request's run is never this build's.
	if err != nil || previous.RequestID != r.ID {
		return args, ""
	}
	dirName, start := "retry", startsFromBase
	if previous.State == run.StateHalted && previous.EarlierAttemptOf != "" {
		unfinished := previous
		if previous, err = run.Load(dataDir, unfinished.EarlierAttemptOf); err != nil || previous.RequestID != r.ID {
			return args, ""
		}
		if resumeRunID != "" {
			// Whether the attempt's commit is under the interrupted
			// build's work is a fact about branches, not commit ids: an
			// attempt that committed nothing has its base as its result.
			dirName, start = "resume", resumesLostBuild+" "+interruptedStartedFromBase
			if previous.Branch != "" && unfinished.Branch == previous.Branch {
				start = resumesLostBuild + " " + interruptedStartedOnItsBranch
			}
		}
	}
	// The ticket's spec has changed since that attempt (an amended scope, an
	// edited ticket): what it failed on was judged against another task, and
	// telling the build "the task has not changed" would be false.
	if !sameSpec(previous, argValueOf(args, "-spec")) {
		return args, ""
	}
	doc, ok := handoffForABuild(dataDir, previous)
	if !ok {
		return args, ""
	}
	dir := filepath.Join(request.Dir(dataDir, r.ID), "rounds", fmt.Sprintf("%03d-%s", ticket.Index, dirName))
	path, err := writeRecordFile(dir, doc, start)
	if err != nil {
		log.Printf("request %s: ticket %d: the build goes ahead without the record of run %s: %v", r.ID, ticket.Index, previous.ID, err)
		return args, ""
	}
	log.Printf("request %s: ticket %d/%d: the build is given the record of run %s (failed %s)", r.ID, ticket.Index, r.TicketCount, previous.ID, failedCheckNames(doc))
	return append(args, "-earlier-attempt", path), previous.ID
}

// The sentence a record opens with, saying where the build that reads it
// starts: the two cases differ in whether the earlier attempt's changes are
// in the workspace.
const (
	startsOnItsBranch = "This build continues on that attempt's branch: what it committed is in the workspace."
	startsFromBase    = "This build starts again from the base commit: none of that attempt's changes are in the workspace, and the commit and files named below are not there."
	// A resumed build's record says two things: which attempt the record
	// is of (not the interrupted build, which the handoff note beside it
	// describes), and whether that attempt's commit is under the
	// interrupted build's work.
	resumesLostBuild              = "This build resumes a later build of this ticket that was interrupted. The record below is of the attempt before that one, which finished and failed its checks."
	interruptedStartedOnItsBranch = "The interrupted build ran on that attempt's branch: what that attempt committed is in the workspace, under whatever the interrupted build changed."
	interruptedStartedFromBase    = "The interrupted build had started again from the base commit: that attempt's commit is not in the workspace, and a file named below is there only if the interrupted build wrote it again."
)

// sameSpec reports whether specPath holds the spec previous was built from.
func sameSpec(previous *run.Run, specPath string) bool {
	if previous.SpecSHA256 == "" || specPath == "" {
		return false
	}
	sum, err := evidence.SHA256File(specPath)
	return err == nil && sum == previous.SpecSHA256
}

// argValueOf returns the value following flag in argv, "" when absent.
func argValueOf(argv []string, flag string) string {
	for i := 0; i+1 < len(argv); i++ {
		if argv[i] == flag {
			return argv[i+1]
		}
	}
	return ""
}

// failedCheckNames lists the checks a handoff's attempt was judged to have
// failed, for a log line.
func failedCheckNames(doc handoff.Document) string {
	var names []string
	for _, c := range doc.Checks {
		if !c.NotJudged {
			names = append(names, c.Check)
		}
	}
	return strings.Join(names, ", ")
}

// writeEarlierAttemptRecord writes the handoff as the text a corrective
// build is given, beside the request's other round files, and returns its
// path. The file is the factory's: it is written outside any workspace, and
// the build receives a read-only copy.
func writeEarlierAttemptRecord(dataDir, requestID string, ticket *request.Ticket, doc handoff.Document, roundIndex int) (string, error) {
	return writeRecordFile(filepath.Join(request.Dir(dataDir, requestID), "rounds", fmt.Sprintf("%03d-corrective%d", ticket.Index, roundIndex)), doc, startsOnItsBranch)
}

// writeRecordFile writes start, then doc, as earlier-attempt.md in dir.
func writeRecordFile(dir string, doc handoff.Document, start string) (string, error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", fmt.Errorf("create %s: %w", dir, err)
	}
	path := filepath.Join(dir, "earlier-attempt.md")
	if err := os.WriteFile(path, []byte(start+"\n\n"+doc.Markdown()), 0o600); err != nil {
		return "", fmt.Errorf("write %s: %w", path, err)
	}
	return path, nil
}
