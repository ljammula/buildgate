package requestdriver

import (
	"context"
	"fmt"
	"log"
	"path/filepath"
	"strings"
	"time"

	"buildgate/internal/handoff"
	"buildgate/internal/request"
	"buildgate/internal/run"
)

// The ticket's challenge round (SC-022): after an accepted run whose
// challenge was red, one build on the run's branch with the ticket's unchanged
// spec on the execution role's model, given the failing held files as its
// -earlier-attempt record, with the files mounted read-only in its sandbox. It
// is judged by every gate and a fresh review, and the held files run again on
// its result; only an accepted round with a green challenge replaces the run it
// followed. It shares the advice round's push, comment and restore rules, and
// counts against neither review_corrective_rounds nor max_review_rounds.

const challengeRoundName = "challenge round"

const challengeStart = startsOnItsBranch + " That attempt was accepted: what follows is not a failure of a check but tests written against it that fail."

var challengeFlavor = roundFlavor{
	name: challengeRoundName, kind: request.ChallengeRoundKind, tag: request.RoundTagChallenge,
	comment: challengeComment,
	replaces: func(round *run.Run) bool {
		return round.Challenge != nil && round.Challenge.Verdict == run.ChallengeGreen
	},
	declined: challengeDeclinedComment,
}

// challengeRoundApplies reports whether current, the ticket's accepted run, is
// followed by the challenge round: its own challenge was red, no challenge
// round was started yet, the ticket's pull request is open on current's
// branch, and held files exist to run again.
func challengeRoundApplies(ticket *request.Ticket, current *run.Run, cfg WorkerConfig) bool {
	return current.Challenge != nil && !current.Challenge.Rerun && current.Challenge.Verdict == run.ChallengeRed &&
		!ticket.ChallengeRounded && ticket.Advice == nil && ticket.Challenged &&
		cfg.OpenPullRequest && (ticket.PRURL != "" || current.PullRequestURL != "") && current.Branch != ""
}

// runChallengeRound runs the challenge round that follows current and settles
// it (concludeAdviceRound with the challenge flavor); pushed is the round's run
// when its result replaced current. A round that cannot start (a launch budget
// is reached, its arguments cannot be made) is skipped with a log line:
// current stands. stop is true when the caller must return err at once.
func runChallengeRound(dp Deps, ctx context.Context, dataDir string, r *request.Request, ticket *request.Ticket, current *run.Run, cfg WorkerConfig, now time.Time) (pushed *run.Run, stop bool, err error) {
	if why := adviceBudgetRefusal(dataDir, r, current, cfg, now); why != "" {
		log.Printf("request %s: ticket %d/%d: no challenge round: %s; run %s stands", r.ID, ticket.Index, r.TicketCount, why, current.ID)
		return nil, false, nil
	}
	roundRunID := fmt.Sprintf("%s-%03d-challenge1", r.ID, ticket.Index)
	args, err := challengeRoundArgs(dataDir, r, ticket, current, cfg, roundRunID)
	if err != nil {
		log.Printf("request %s: ticket %d/%d: no challenge round: %v; run %s stands", r.ID, ticket.Index, r.TicketCount, err, current.ID)
		return nil, false, nil
	}
	ticket.ChallengeRounded = true
	ticket.Advice = &request.TicketAdvice{Of: current.ID, Since: now.UTC().Format(time.RFC3339Nano), Challenge: true}
	if err := r.Save(dataDir); err != nil {
		return nil, true, err
	}
	log.Printf("request %s: ticket %d/%d: run %s accepted with a red challenge; challenge round", r.ID, ticket.Index, r.TicketCount, current.ID)
	launched, stop, err := launchCorrectiveRound(dp, ctx, dataDir, r, ticket, args, roundRunID, challengeRoundName, "", run.StackBase(dataDir, current), false, true)
	if stop {
		return nil, true, err
	}
	return concludeAdviceRound(dp, ctx, dataDir, r, ticket, current, launched, challengeFlavor, time.Now())
}

// challengeRoundArgs is the challenge round's build argv: the ticket's
// unchanged build spec, current's branch and diff base, no pull request of its
// own, the execution role's model, the record of the failing files
// (handoff.Challenge) as -earlier-attempt, and the held files as
// -challenge-held with -challenge-round.
func challengeRoundArgs(dataDir string, r *request.Request, ticket *request.Ticket, current *run.Run, cfg WorkerConfig, roundRunID string) ([]string, error) {
	roundDir := filepath.Join(request.Dir(dataDir, r.ID), "rounds", fmt.Sprintf("%03d-challenge1", ticket.Index))
	recordPath, err := writeRecordFile(roundDir, handoff.Challenge(current), challengeStart)
	if err != nil {
		return nil, err
	}
	buildSpecPath, err := writeTicketBuildInput(dataDir, r, *ticket)
	if err != nil {
		return nil, fmt.Errorf("build spec: %w", err)
	}
	argv, err := correctiveBuildArgs(dataDir, r, *ticket, cfg, buildSpecPath, roundRunID, current.Branch, current.DiffBase(), true, true)
	if err != nil {
		return nil, err
	}
	if !hasFlagArg(argv, "-challenge-round") {
		return nil, fmt.Errorf("the ticket has no held challenge file to give the round")
	}
	return withRoundSession(append(argv, "-earlier-attempt", recordPath), cfg, current), nil
}

func hasFlagArg(argv []string, flag string) bool {
	for _, a := range argv {
		if a == flag {
			return true
		}
	}
	return false
}

// challengeComment is the one PR comment after a challenge round's result was
// pushed: that it was, and which held files it satisfied. No file content.
func challengeComment(current, roundRun *run.Run) string {
	var b strings.Builder
	fmt.Fprintf(&b, "A challenge round fixed the change: it now passes the held challenge tests that failed on the earlier head (%s), and the tests are committed on this branch.\n", shortSHA(current.ResultSHA))
	if roundRun.Challenge != nil {
		for _, f := range roundRun.Challenge.Files {
			if f.Verdict == run.ChallengeGreen {
				fmt.Fprintf(&b, "\n- `%s` (%s)", handoff.CleanSentence(f.Path), handoff.CleanWord(f.Criterion))
			}
		}
	}
	return b.String()
}

// challengeDeclinedComment is the advice comment for a challenge round that
// left the run it followed as the ticket's: each red file's path, the
// criterion it cites and the build's one-line reason, cleaned; never a
// file's content.
func challengeDeclinedComment(dataDir string, current, roundRun *run.Run) string {
	reason := "the round gave no reason"
	if roundRun != nil {
		if line := handoff.NoteLine(run.Dir(dataDir, roundRun.ID)); line != "" {
			reason = line
		}
	}
	var b strings.Builder
	b.WriteString("A challenge test the factory ran against this change still fails; a challenge round did not make it pass, so the tests are not committed. For each one a person decides whether the code or the test is wrong:\n")
	for _, f := range current.Challenge.Files {
		if f.Verdict == run.ChallengeRed {
			fmt.Fprintf(&b, "\n- `%s` (%s): %s", handoff.CleanSentence(f.Path), handoff.CleanWord(f.Criterion), reason)
		}
	}
	return b.String()
}

// commentDeclined posts fl's declined comment on the ticket's pull request; a
// failed comment is logged.
func commentDeclined(dp Deps, ctx context.Context, dataDir string, r *request.Request, ticket *request.Ticket, fl roundFlavor, current, roundRun *run.Run) {
	if fl.declined == nil || current.Challenge == nil {
		return
	}
	commentCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := dp.CommentOnPullRequest(commentCtx, ticket.PRURL, fl.declined(dataDir, current, roundRun)); err != nil {
		log.Printf("request %s: ticket %d: comment on %s about the %s: %v", r.ID, ticket.Index, ticket.PRURL, fl.name, err)
	}
}
