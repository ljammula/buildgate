package requestdriver

import (
	"context"
	"fmt"
	"log"
	"path/filepath"
	"time"

	"buildgate/internal/codereview"
	"buildgate/internal/handoff"
	"buildgate/internal/request"
	"buildgate/internal/run"
)

// The advice round (session key review_advice_round, off by default).
//
// A finding of a ticket run's code review refuses the run only through a
// reproduction the factory runs (SC-022); every other finding is advice,
// listed in the pull request body. With the key on, an accepted ticket run
// whose review recorded at least one high-severity finding gets one more
// build before the request moves on: on the run's own branch, given those
// findings as the record of the attempt it follows (-earlier-attempt), with
// the ticket's unchanged spec, every gate and a fresh review. It never
// refuses anything:
//
//	round accepted, decision allowed, push confirmed -> the round's run is the ticket's
//	anything else (not accepted, a push check refuses)  -> the accepted run stands
//	a push that failed once sent                        -> halt, as a PR-review round's
//
// The request stays in building throughout, so pr_review still means the
// factory's work on every ticket is done. At most one per ticket, counted
// against neither round budget, and only where the run's pull request is
// open (pull requests on): with them off nothing is pushed, so there is
// nothing to replace the accepted run with.

// adviceRoundName names the advice round in logs and push reasons.
const adviceRoundName = "advice round"

// adviceStart is the sentence the advice round's record opens with: where the
// build starts, and that the attempt it follows was accepted.
const adviceStart = startsOnItsBranch + " That attempt was accepted: what follows is not a failure but a reviewer's concerns about it."

// highFindings are the high-severity findings of runRecord's code review.
func highFindings(runRecord *run.Run) []run.CodeReviewFinding {
	if runRecord.CodeReview == nil {
		return nil
	}
	return codereview.Blocking(runRecord.CodeReview.Findings)
}

// adviceRoundApplies reports whether accepted, the ticket's accepted run, is
// followed by an advice round: the key is on, the run opened its pull
// request on its branch, its review recorded a high-severity finding, and
// the ticket has had no advice round.
func adviceRoundApplies(ticket *request.Ticket, accepted *run.Run, cfg WorkerConfig) bool {
	return cfg.Settings.ReviewAdviceRound && cfg.OpenPullRequest &&
		accepted.PullRequestURL != "" && accepted.Branch != "" &&
		len(highFindings(accepted)) > 0 && !ticket.HadAdviceRound()
}

// acceptAfterAdvice accepts accepted as the ticket's outcome and advances the
// request (acceptTicketRun), after the advice round when one applies. The
// pull request and branch recorded are accepted's: the round builds on that
// branch and opens nothing.
func acceptAfterAdvice(dp Deps, ctx context.Context, dataDir string, r *request.Request, ticket *request.Ticket, accepted *run.Run, cfg WorkerConfig, now time.Time) error {
	if adviceRoundApplies(ticket, accepted, cfg) {
		if stop, err := runAdviceRound(dp, ctx, dataDir, r, ticket, accepted, cfg, now); stop {
			return err
		}
		now = time.Now()
	}
	return acceptTicketRun(dataDir, r, ticket, accepted, cfg.OpenPullRequest, now)
}

// runAdviceRound runs the advice round that follows accepted and settles it
// (concludeAdviceRound). A round that cannot start (the launch budget is
// reached or cannot be read, its arguments cannot be made) is skipped with
// a log line: the accepted run stands. stop is true when the caller must
// return err at once: the worker is stopping or the request left building
// mid-round (the round stays in flight on the ticket, Ticket.Advice, for the
// next pass to settle), or the request was halted.
func runAdviceRound(dp Deps, ctx context.Context, dataDir string, r *request.Request, ticket *request.Ticket, accepted *run.Run, cfg WorkerConfig, now time.Time) (stop bool, err error) {
	if check, reason, err := CheckLaunchBudget(dataDir, r, cfg.Settings, now); err != nil {
		log.Printf("request %s: ticket %d/%d: no advice round: read the launch budget: %v; run %s stands", r.ID, ticket.Index, r.TicketCount, err, accepted.ID)
		return false, nil
	} else if check != "" {
		log.Printf("request %s: ticket %d/%d: no advice round: %s; run %s stands", r.ID, ticket.Index, r.TicketCount, reason, accepted.ID)
		return false, nil
	}
	roundRunID := fmt.Sprintf("%s-%03d-advice1", r.ID, ticket.Index)
	args, err := adviceRoundArgs(dataDir, r, ticket, accepted, cfg, roundRunID)
	if err != nil {
		log.Printf("request %s: ticket %d/%d: no advice round: %v; run %s stands", r.ID, ticket.Index, r.TicketCount, err, accepted.ID)
		return false, nil
	}
	// The round pushes to the run's open pull request: the ticket records
	// it before the push, as acceptTicketRun would.
	ticket.Branch, ticket.PRURL = accepted.Branch, accepted.PullRequestURL
	ticket.Advice = &request.TicketAdvice{Of: accepted.ID, Since: now.UTC().Format(time.RFC3339Nano)}
	if err := r.Save(dataDir); err != nil {
		return true, err
	}
	log.Printf("request %s: ticket %d/%d: run %s accepted with %d high-severity review finding(s); advice round", r.ID, ticket.Index, r.TicketCount, accepted.ID, len(highFindings(accepted)))
	launched, stop, err := launchCorrectiveRound(dp, ctx, dataDir, r, ticket, args, roundRunID, adviceRoundName, "", run.StackBase(dataDir, accepted), executionEscalation(cfg) != nil, false)
	if stop {
		return true, err
	}
	return concludeAdviceRound(dp, ctx, dataDir, r, ticket, accepted, launched, time.Now())
}

// adviceRoundArgs is the advice round's build argv: the ticket's unchanged
// build spec, accepted's branch and diff base, no pull request of its own,
// and the record of accepted with its high-severity findings
// (handoff.Advice) as -earlier-attempt.
func adviceRoundArgs(dataDir string, r *request.Request, ticket *request.Ticket, accepted *run.Run, cfg WorkerConfig, roundRunID string) ([]string, error) {
	roundDir := filepath.Join(request.Dir(dataDir, r.ID), "rounds", fmt.Sprintf("%03d-advice", ticket.Index))
	recordPath, err := writeRecordFile(roundDir, handoff.Advice(accepted), adviceStart)
	if err != nil {
		return nil, err
	}
	buildSpecPath, err := writeTicketBuildInput(dataDir, r, *ticket)
	if err != nil {
		return nil, fmt.Errorf("build spec: %w", err)
	}
	argv, err := correctiveBuildArgs(dataDir, r, *ticket, cfg, buildSpecPath, roundRunID, accepted.Branch, accepted.DiffBase(), true)
	if err != nil {
		return nil, err
	}
	return append(argv, "-earlier-attempt", recordPath), nil
}

// concludeAdviceRound records the advice round that followed accepted
// (ended is how its build ended) and settles which run is the ticket's: the
// round's, once it was accepted and its result is confirmed on the pull
// request's branch (pushRoundResult, afterRoundPushed); accepted otherwise.
// A push check that refuses leaves the branch untouched and accepted
// standing. A push or read-back that failed once the push was sent halts the
// request, as for a PR-review round: the commit may be on the remote, and a
// human reconciles the branch. Nothing is saved here: the caller's next save
// records the round with the ticket's next step, and until then the round
// stays in flight on disk, so a pass that is lost in between settles it
// again (a repeated push of the same commit is a no-op).
func concludeAdviceRound(dp Deps, ctx context.Context, dataDir string, r *request.Request, ticket *request.Ticket, accepted *run.Run, ended correctiveLaunch, now time.Time) (stop bool, err error) {
	ticket.Advice = nil
	ticket.RunID = accepted.ID
	ticket.Rounds = append(ticket.Rounds, request.Round{
		Index: 1, Kind: request.AdviceRoundKind, RunID: ended.loadID, Outcome: ended.outcome,
		StartFailure: ended.startFailure, Unresolved: ended.unresolved, Error: ended.errText,
		At: now.UTC().Format(time.RFC3339Nano),
	})
	slice := len(ticket.Rounds) - 1
	tagRoundByID(dp, dataDir, r, ticket, ended.loadID, request.RoundTagAdvice, now)
	if ended.outcome != request.RoundAccepted {
		log.Printf("request %s: ticket %d/%d: advice round %s %s; run %s stands", r.ID, ticket.Index, r.TicketCount, ended.loadID, ended.outcome, accepted.ID)
		return false, nil
	}
	roundRun, err := run.Load(dataDir, ended.loadID)
	if err != nil {
		ticket.Rounds[slice].Error = fmt.Sprintf("load the round's run: %v", err)
		log.Printf("request %s: ticket %d/%d: advice round %s: %s; run %s stands", r.ID, ticket.Index, r.TicketCount, ended.loadID, ticket.Rounds[slice].Error, accepted.ID)
		return false, nil
	}
	refused, failed := pushRoundResult(dp, ctx, dataDir, r, ticket, roundRun, accepted.Branch, adviceRoundName)
	switch {
	case failed != "":
		return true, HaltRequest(dataDir, r, failed, now)
	case refused != "":
		ticket.Rounds[slice].Error = refused
		log.Printf("request %s: ticket %d/%d: advice round %s accepted but not pushed: %s; run %s stands", r.ID, ticket.Index, r.TicketCount, roundRun.ID, refused, accepted.ID)
		return false, nil
	}
	afterRoundPushed(dp, ctx, r, ticket, slice, roundRun)
	ticket.RunID = roundRun.ID
	log.Printf("request %s: ticket %d/%d: advice round %s accepted and pushed to %s as %s", r.ID, ticket.Index, r.TicketCount, roundRun.ID, accepted.Branch, roundRun.ResultSHA)
	return false, nil
}

// settleInterruptedAdviceRound settles the advice round a pass left in flight
// (Ticket.Advice: the worker stopped or was lost mid-round), never starting
// it again: its run, once ended, is concluded as any round's
// (concludeAdviceRound), one that never started is a start failure, and the
// request then advances on the accepted run it followed, or on the round's.
// While the run has not ended the pass ends with an error, and the step is
// tried again. A decision a lost round's resume_review asked for is used up:
// the round is settled whatever it chose, and its kept worktree is reaped.
func settleInterruptedAdviceRound(dp Deps, ctx context.Context, dataDir string, r *request.Request, ticket *request.Ticket, cfg WorkerConfig, now time.Time) error {
	accepted, err := run.Load(dataDir, ticket.Advice.Of)
	if err != nil {
		return fmt.Errorf("request %s: ticket %d: load run %s the advice round followed: %w", r.ID, ticket.Index, ticket.Advice.Of, err)
	}
	ended := correctiveLaunch{loadID: ticket.RunID}
	if ticket.RunID == accepted.ID {
		ended.roundOutcome = roundOutcome{outcome: request.RoundHalted, startFailure: true, errText: "the worker stopped before the advice round's run started"}
	} else if rec, loadErr := run.Load(dataDir, ticket.RunID); loadErr != nil {
		ended.roundOutcome = roundOutcome{outcome: request.RoundHalted, startFailure: true, errText: loadErr.Error()}
	} else if !finishedRun(rec) {
		return fmt.Errorf("request %s: ticket %d: advice round run %s has not ended", r.ID, ticket.Index, rec.ID)
	} else if ended.roundOutcome, err = correctiveRoundOutcome(dataDir, rec.ID, nil); err != nil {
		return err
	}
	log.Printf("request %s: ticket %d/%d: settling the advice round left in flight (run %s, %s)", r.ID, ticket.Index, r.TicketCount, ended.loadID, ended.outcome)
	r.ConsumeResumeDecision()
	if err := ClearKeptRunsOfRequest(dataDir, r.ID); err != nil {
		log.Printf("request %s: ticket %d: discard the advice round's kept worktree: %v", r.ID, ticket.Index, err)
	}
	if stop, err := concludeAdviceRound(dp, ctx, dataDir, r, ticket, accepted, ended, now); stop {
		return err
	}
	return acceptTicketRun(dataDir, r, ticket, accepted, cfg.OpenPullRequest, now)
}

// finishedRun reports whether rec is in a terminal state.
func finishedRun(rec *run.Run) bool {
	return rec.State == run.StateAccepted || rec.State == run.StateQuarantined || rec.State == run.StateHalted
}
