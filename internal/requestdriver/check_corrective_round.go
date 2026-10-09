package requestdriver

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

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
	if quarantined.State != run.StateQuarantined || quarantined.HandoffSHA256 == "" {
		return handoff.Document{}, false
	}
	// A quarantine by the two reviews alone is the review round's: when
	// that round found nothing to address, neither is there here.
	if reviewShapeOnly(quarantined) {
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
	dir := filepath.Join(request.Dir(dataDir, requestID), "rounds", fmt.Sprintf("%03d-corrective%d", ticket.Index, roundIndex))
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", fmt.Errorf("create %s: %w", dir, err)
	}
	path := filepath.Join(dir, "earlier-attempt.md")
	if err := os.WriteFile(path, []byte(doc.Markdown()), 0o600); err != nil {
		return "", fmt.Errorf("write %s: %w", path, err)
	}
	return path, nil
}
