package requestdriver

import (
	"context"
	"fmt"
	"log"
	"path/filepath"
	"strings"
	"time"

	"buildgate/internal/codereview"
	"buildgate/internal/handoff"
	"buildgate/internal/request"
	"buildgate/internal/run"
	"buildgate/internal/workflow"
)

// Advice rounds (session key review_advice_rounds, 0 to 3, 3 by default).
//
// A finding of a ticket run's code review refuses the run only through a
// reproduction the factory runs (SC-022); every other finding is advice,
// listed in the pull request body. With the key above 0, an accepted ticket
// run whose review recorded at least one high-severity finding gets another
// build before the request moves on: on the run's own branch, given those
// findings as the record of the attempt it follows (-earlier-attempt), with
// the ticket's unchanged spec, every gate and a fresh review, on the
// execution role's model. It never refuses anything:
//
//	round accepted, decision allowed, push confirmed -> the round's run is the ticket's
//	anything else (not accepted, a push check refuses)  -> the run it followed stands,
//	                                                       the local branch is put back on it
//	a push that failed once sent                        -> halt, as a PR-review round's
//
// A pushed round whose own review recorded a high-severity finding is
// followed by the next, given that round's findings, up to the key; any
// other round ends them. The request stays in building throughout, so
// pr_review still means the factory's work on every ticket is done. Advice
// rounds count against neither round budget, and run only where the run's
// pull request is open (pull requests on): with them off nothing is pushed,
// so there is nothing to replace the accepted run with.

// adviceRoundName names an advice round in logs and push reasons.
const adviceRoundName = "advice round"

// roundFlavor is what differs between the rounds that follow an accepted run
// on its open pull request's branch and share the push, comment and restore
// rules (SC-022): the advice round and the challenge round.
type roundFlavor struct {
	name string
	kind string
	tag  string
	// comment is the one PR comment after a confirmed push.
	comment func(current, round *run.Run) string
	// replaces, when set, decides whether an accepted round's result replaces
	// current: a challenge round's only when its challenge is green.
	replaces func(round *run.Run) bool
	// declined, when set, is the PR comment for a round whose result was not
	// pushed (current stands): "" posts nothing.
	declined func(dataDir string, current, round *run.Run) string
	// pushFailureEndsRound, when set, makes a push or read-back that failed
	// once sent end the round as no verdict, with a note, instead of halting
	// the request: a challenge never halts (SC-022).
	pushFailureEndsRound bool
}

var adviceFlavor = roundFlavor{name: adviceRoundName, kind: request.AdviceRoundKind, tag: request.RoundTagAdvice, comment: adviceComment}

// flavorOf is the flavor of the round left in flight on a ticket.
func flavorOf(adv *request.TicketAdvice) roundFlavor {
	if adv != nil && adv.Challenge {
		return challengeFlavor
	}
	return adviceFlavor
}

// adviceStart is the sentence an advice round's record opens with: where the
// build starts, and that the attempt it follows was accepted.
const adviceStart = startsOnItsBranch + " That attempt was accepted: what follows is not a failure but a reviewer's concerns about it."

// adviceSettleBound is how long an advice round left in flight is waited for
// before it is settled as halted: the longest one ticket build can run (one
// request step), and an hour more.
const adviceSettleBound = workflow.RequestStepStartToClose + time.Hour

// highFindings are the high-severity findings of runRecord's code review.
func highFindings(runRecord *run.Run) []run.CodeReviewFinding {
	if runRecord.CodeReview == nil {
		return nil
	}
	return codereview.Blocking(runRecord.CodeReview.Findings)
}

// adviceRoundApplies reports whether current, the ticket's last pushed run
// (its accepted run, or its last advice round's), is followed by an advice
// round: fewer advice rounds than review_advice_rounds are recorded, the last
// of them (if any) was pushed, none is in flight, the ticket's pull request is
// open on current's branch, and current's review recorded a high-severity
// finding.
func adviceRoundApplies(ticket *request.Ticket, current *run.Run, cfg WorkerConfig) bool {
	n, lastPushed := ticket.AdviceRounds()
	return n < cfg.Settings.ReviewAdviceRounds && (n == 0 || lastPushed) && ticket.Advice == nil &&
		cfg.OpenPullRequest && ticket.PRURL != "" && current.Branch != "" &&
		len(highFindings(current)) > 0
}

// acceptAfterAdvice accepts accepted as the ticket's outcome and advances the
// request (acceptTicketRun), after the challenge and advice rounds that
// apply. The pull request and branch recorded are accepted's: the rounds
// build on that branch and open nothing, so a pushed round's run, which has
// no pull request of its own, is the head the next round builds on and never
// the run the ticket's pull request is read from.
func acceptAfterAdvice(dp Deps, ctx context.Context, dataDir string, r *request.Request, ticket *request.Ticket, accepted *run.Run, cfg WorkerConfig, now time.Time) error {
	noteChallenge(ticket, accepted)
	if (cfg.Settings.ReviewAdviceRounds > 0 || challengeRoundApplies(ticket, accepted, cfg)) && accepted.PullRequestURL != "" {
		// The rounds push to the run's open pull request: the ticket records
		// it before the first, as acceptTicketRun would.
		ticket.Branch, ticket.PRURL = accepted.Branch, accepted.PullRequestURL
	}
	head := accepted
	if challengeRoundApplies(ticket, accepted, cfg) {
		pushed, stop, err := runChallengeRound(dp, ctx, dataDir, r, ticket, accepted, cfg, now)
		if stop {
			return err
		}
		if pushed != nil {
			head, now = pushed, time.Now()
		}
	}
	if _, stop, err := runAdviceRounds(dp, ctx, dataDir, r, ticket, head, cfg, now); stop {
		return err
	}
	return acceptTicketRun(dataDir, r, ticket, accepted, cfg.OpenPullRequest, time.Now())
}

// runAdviceRounds runs advice rounds after current while they apply
// (adviceRoundApplies), each after the round before it was pushed, and
// returns the last run whose result is the pull request's head. stop is true
// when the caller must return err at once (runAdviceRound).
func runAdviceRounds(dp Deps, ctx context.Context, dataDir string, r *request.Request, ticket *request.Ticket, current *run.Run, cfg WorkerConfig, now time.Time) (last *run.Run, stop bool, err error) {
	for adviceRoundApplies(ticket, current, cfg) {
		pushed, stop, err := runAdviceRound(dp, ctx, dataDir, r, ticket, current, cfg, now)
		if stop || pushed == nil {
			return current, stop, err
		}
		current, now = pushed, time.Now()
	}
	return current, false, nil
}

// runAdviceRound runs the advice round that follows current and settles it
// (concludeAdviceRound); pushed is the round's run when its result was
// pushed, nil otherwise. A round that cannot start (a launch budget is
// reached, cannot be read, or has less left than current spent; its
// arguments cannot be made) is skipped with a log line: current stands. stop
// is true when the caller must return err at once: the worker is stopping or
// the request left building mid-round (the round stays in flight on the
// ticket, Ticket.Advice, for the next pass to settle), or the request was
// halted.
func runAdviceRound(dp Deps, ctx context.Context, dataDir string, r *request.Request, ticket *request.Ticket, current *run.Run, cfg WorkerConfig, now time.Time) (pushed *run.Run, stop bool, err error) {
	if why := adviceBudgetRefusal(dataDir, r, current, cfg, now); why != "" {
		log.Printf("request %s: ticket %d/%d: no advice round: %s; run %s stands", r.ID, ticket.Index, r.TicketCount, why, current.ID)
		return nil, false, nil
	}
	n, _ := ticket.AdviceRounds()
	k := n + 1
	roundRunID := fmt.Sprintf("%s-%03d-advice%d", r.ID, ticket.Index, k)
	args, err := adviceRoundArgs(dataDir, r, ticket, current, cfg, roundRunID, k)
	if err != nil {
		log.Printf("request %s: ticket %d/%d: no advice round: %v; run %s stands", r.ID, ticket.Index, r.TicketCount, err, current.ID)
		return nil, false, nil
	}
	ticket.Advice = &request.TicketAdvice{Of: current.ID, Since: now.UTC().Format(time.RFC3339Nano)}
	if err := r.Save(dataDir); err != nil {
		return nil, true, err
	}
	log.Printf("request %s: ticket %d/%d: run %s accepted with %d high-severity review finding(s); advice round %d/%d", r.ID, ticket.Index, r.TicketCount, current.ID, len(highFindings(current)), k, cfg.Settings.ReviewAdviceRounds)
	launched, stop, err := launchCorrectiveRound(dp, ctx, dataDir, r, ticket, args, roundRunID, adviceRoundName, "", run.StackBase(dataDir, current), false, true)
	if stop {
		return nil, true, err
	}
	return concludeAdviceRound(dp, ctx, dataDir, r, ticket, current, launched, adviceFlavor, time.Now())
}

// adviceBudgetRefusal is why an advice round after current may not launch,
// "" when it may: a launch budget is reached or cannot be read
// (CheckLaunchBudget), or one has less left than current spent, the build
// the round repeats (budgetLeftFor). The round's spend counts toward the
// request's budget like any build's.
func adviceBudgetRefusal(dataDir string, r *request.Request, current *run.Run, cfg WorkerConfig, now time.Time) string {
	check, reason, err := CheckLaunchBudget(dataDir, r, cfg.Settings, now)
	if err == nil && check == "" {
		reason, err = budgetLeftFor(dataDir, r, cfg.Settings, current, now)
	}
	if err != nil {
		return "read the launch budget: " + err.Error()
	}
	return reason
}

// adviceRoundArgs is advice round k's build argv: the ticket's unchanged
// build spec, current's branch and diff base, no pull request of its own,
// the execution role's model, and the record of current with its
// high-severity findings (handoff.Advice) as -earlier-attempt.
func adviceRoundArgs(dataDir string, r *request.Request, ticket *request.Ticket, current *run.Run, cfg WorkerConfig, roundRunID string, k int) ([]string, error) {
	roundDir := filepath.Join(request.Dir(dataDir, r.ID), "rounds", fmt.Sprintf("%03d-advice%d", ticket.Index, k))
	recordPath, err := writeRecordFile(roundDir, handoff.Advice(current), adviceStart)
	if err != nil {
		return nil, err
	}
	buildSpecPath, err := writeTicketBuildInput(dataDir, r, *ticket)
	if err != nil {
		return nil, fmt.Errorf("build spec: %w", err)
	}
	argv, err := correctiveBuildArgs(dataDir, r, *ticket, cfg, buildSpecPath, roundRunID, current.Branch, current.DiffBase(), true, false)
	if err != nil {
		return nil, err
	}
	return withRoundSession(append(argv, "-earlier-attempt", recordPath), cfg, current), nil
}

// concludeAdviceRound records the advice round that followed current (ended
// is how its build ended) and settles which run is the ticket's: the
// round's, returned as pushed, once it was accepted and its result is
// confirmed on the pull request's branch (pushRoundResult, afterRoundPushed),
// after which the pull request gets one comment saying so (adviceComment);
// current otherwise, with the local branch put back on current's result
// (restoreUnpushed). A push check that refuses leaves the remote untouched
// and current standing. A push or read-back that failed once the push was
// sent halts the request, as for a PR-review round: the commit may be on the
// remote, and a human reconciles the branch. Nothing is saved here: the
// caller's next save records the round with the ticket's next step, and
// until then the round stays in flight on disk, so a pass that is lost in
// between settles it again (a repeated push of the same commit is a no-op).
func concludeAdviceRound(dp Deps, ctx context.Context, dataDir string, r *request.Request, ticket *request.Ticket, current *run.Run, ended correctiveLaunch, fl roundFlavor, now time.Time) (pushed *run.Run, stop bool, err error) {
	slice := recordAdviceRound(dp, dataDir, r, ticket, current, ended, fl, now)
	if ended.outcome != request.RoundAccepted {
		log.Printf("request %s: ticket %d/%d: %s %s %s; run %s stands", r.ID, ticket.Index, r.TicketCount, fl.name, ended.loadID, ended.outcome, current.ID)
		if fl.declined != nil {
			roundRun, _ := run.Load(dataDir, ended.loadID)
			commentDeclined(dp, ctx, dataDir, r, ticket, fl, current, roundRun)
		}
		stop, err := restoreUnpushed(dp, dataDir, r, ticket, slice, current, now)
		return nil, stop, err
	}
	roundRun, err := run.Load(dataDir, ended.loadID)
	if err == nil && fl.replaces != nil && !fl.replaces(roundRun) {
		ticket.Rounds[slice].Error = "the round was accepted but its challenge is not green"
		log.Printf("request %s: ticket %d/%d: %s %s accepted without a green challenge; run %s stands", r.ID, ticket.Index, r.TicketCount, fl.name, roundRun.ID, current.ID)
		commentDeclined(dp, ctx, dataDir, r, ticket, fl, current, roundRun)
		stop, err := restoreUnpushed(dp, dataDir, r, ticket, slice, current, now)
		return nil, stop, err
	}
	if err != nil {
		ticket.Rounds[slice].Error = fmt.Sprintf("load the round's run: %v", err)
		log.Printf("request %s: ticket %d/%d: advice round %s: %s; run %s stands", r.ID, ticket.Index, r.TicketCount, ended.loadID, ticket.Rounds[slice].Error, current.ID)
		stop, err := restoreUnpushed(dp, dataDir, r, ticket, slice, current, now)
		return nil, stop, err
	}
	refused, failed := pushRoundResult(dp, ctx, dataDir, r, ticket, roundRun, current.Branch, fl.name)
	switch {
	case failed != "" && fl.pushFailureEndsRound:
		ticket.Rounds[slice].Error = "no verdict: " + failed
		log.Printf("request %s: ticket %d/%d: %s %s: %s; run %s stands", r.ID, ticket.Index, r.TicketCount, fl.name, roundRun.ID, failed, current.ID)
		stop, err := restoreUnpushed(dp, dataDir, r, ticket, slice, current, now)
		return nil, stop, err
	case failed != "":
		return nil, true, HaltRequest(dataDir, r, failed, now)
	case refused != "":
		ticket.Rounds[slice].Error = refused
		log.Printf("request %s: ticket %d/%d: advice round %s accepted but not pushed: %s; run %s stands", r.ID, ticket.Index, r.TicketCount, roundRun.ID, refused, current.ID)
		stop, err := restoreUnpushed(dp, dataDir, r, ticket, slice, current, now)
		return nil, stop, err
	}
	afterRoundPushed(dp, ctx, r, ticket, slice, roundRun)
	ticket.RunID = roundRun.ID
	log.Printf("request %s: ticket %d/%d: advice round %s accepted and pushed to %s as %s", r.ID, ticket.Index, r.TicketCount, roundRun.ID, current.Branch, roundRun.ResultSHA)
	commentCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := dp.CommentOnPullRequest(commentCtx, ticket.PRURL, fl.comment(current, roundRun)); err != nil {
		log.Printf("request %s: ticket %d: comment on %s about advice round %s: %v", r.ID, ticket.Index, ticket.PRURL, roundRun.ID, err)
	}
	return roundRun, false, nil
}

// recordAdviceRound records the advice round that followed current, ended
// as ended, on ticket, ends it in flight, makes current the ticket's run
// again and tags the round's commit; it returns the round's index in
// ticket.Rounds.
func recordAdviceRound(dp Deps, dataDir string, r *request.Request, ticket *request.Ticket, current *run.Run, ended correctiveLaunch, fl roundFlavor, now time.Time) int {
	ticket.Advice = nil
	ticket.RunID = current.ID
	ticket.Rounds = append(ticket.Rounds, request.Round{
		Index: ticket.NextRoundIndex(), Kind: fl.kind, RunID: ended.loadID, Outcome: ended.outcome,
		StartFailure: ended.startFailure, Unresolved: ended.unresolved, Error: ended.errText,
		At: now.UTC().Format(time.RFC3339Nano),
	})
	tagRoundByID(dp, dataDir, r, ticket, ended.loadID, fl.tag, now)
	return len(ticket.Rounds) - 1
}

// restoreUnpushed puts the local branch an unpushed advice round built on
// back on current's result: the round's commits moved it, and a later
// PR-review round builds on the local branch and would carry them into the
// pull request. The move is a compare-and-swap of a branch ahead of that
// commit (Deps.RestoreBranch). A move that fails is recorded on the round
// (ticket.Rounds[slice].Error); it halts the request only when the branch
// still points past current's result, the one case in which a later round
// would push the rejected commits, and stop is then true. A branch that
// could not be read or was moved elsewhere is left to the checks of the
// next push, which refuse a result that does not descend from the remote
// head.
func restoreUnpushed(dp Deps, dataDir string, r *request.Request, ticket *request.Ticket, slice int, current *run.Run, now time.Time) (stop bool, err error) {
	repo := roundRepoDir(r, current)
	if repo == "" || current.ResultSHA == "" || current.Branch == "" {
		return false, nil
	}
	ahead, rerr := dp.RestoreBranch(repo, current.Branch, current.ResultSHA)
	if rerr == nil {
		return false, nil
	}
	note := fmt.Sprintf("the local branch %s was not put back on %s: %v", current.Branch, current.ResultSHA, rerr)
	ticket.Rounds[slice].Error = strings.TrimPrefix(ticket.Rounds[slice].Error+"; "+note, "; ")
	log.Printf("request %s: ticket %d/%d: advice round %s: %s", r.ID, ticket.Index, r.TicketCount, ticket.Rounds[slice].RunID, note)
	if !ahead {
		return false, nil
	}
	return true, HaltRequest(dataDir, r, fmt.Sprintf("ticket %d/%d: advice round %s was not pushed, and %s; point the branch back at that commit (`git update-ref refs/heads/%s %s`) before `factoryd retry %s`", ticket.Index, r.TicketCount, ticket.Rounds[slice].RunID, note, current.Branch, current.ResultSHA, r.ID), now)
}

// maxCommentFindings bounds the findings an advice round's comment lists.
const maxCommentFindings = 20

// adviceComment is the comment an advice round's confirmed push leaves on the
// pull request: the high-severity findings the round was given, its run and
// commit, that every gate and a fresh review passed again, and its own
// review's findings. Each finding is its severity and summary, one line,
// cut and cleaned as the pull request body's advice is (handoff.CleanWord,
// handoff.CleanSentence); never its file, failure scenario or reproduction.
func adviceComment(current, roundRun *run.Run) string {
	var b strings.Builder
	fmt.Fprintf(&b, "**Advice round** (buildgate). The code review of run `%s` raised these high-severity concerns, which one more build of this ticket on this branch was given to check:\n\n", current.ID)
	writeFindingLines(&b, highFindings(current))
	fmt.Fprintf(&b, "\nThat build, run `%s`, passed every gate and a fresh review again; its commit `%s` is this pull request's head.\n\n", roundRun.ID, roundRun.ResultSHA)
	var own []run.CodeReviewFinding
	if roundRun.CodeReview != nil {
		own = roundRun.CodeReview.Findings
	}
	if len(own) == 0 {
		b.WriteString("Its own review recorded no findings.\n")
		return b.String()
	}
	b.WriteString("Its own review's findings:\n\n")
	writeFindingLines(&b, own)
	return b.String()
}

// writeFindingLines writes each finding (at most maxCommentFindings) as one
// list line: severity and summary, cleaned.
func writeFindingLines(b *strings.Builder, findings []run.CodeReviewFinding) {
	for i, f := range findings {
		if i == maxCommentFindings {
			fmt.Fprintf(b, "- and %d more\n", len(findings)-maxCommentFindings)
			return
		}
		fmt.Fprintf(b, "- **%s** %s\n", handoff.CleanWord(f.Severity), handoff.CleanSentence(f.Summary))
	}
}

// settleInterruptedAdviceRound settles the advice round a pass left in flight
// (Ticket.Advice: the worker stopped or was lost mid-round), never starting
// it again: its run, once ended, is concluded as any round's
// (concludeAdviceRound); one that never started is a start failure; and one
// whose run has not ended, or whose outcome cannot be read, is waited for
// (the pass ends with an error and the step is tried again) until
// adviceSettleBound has passed since the round started, then settled as
// halted. The rounds that still apply follow a pushed one (runAdviceRounds),
// and the request then advances. A decision a lost round's resume_review
// asked for is used up, and a kept worktree is reaped.
func settleInterruptedAdviceRound(dp Deps, ctx context.Context, dataDir string, r *request.Request, ticket *request.Ticket, cfg WorkerConfig, now time.Time) error {
	current, err := run.Load(dataDir, ticket.Advice.Of)
	if err != nil {
		return fmt.Errorf("request %s: ticket %d: load run %s the advice round followed: %w", r.ID, ticket.Index, ticket.Advice.Of, err)
	}
	ended, wait := interruptedAdviceOutcome(dataDir, ticket.Advice, now)
	if wait != nil {
		return fmt.Errorf("request %s: ticket %d: %w", r.ID, ticket.Index, wait)
	}
	log.Printf("request %s: ticket %d/%d: settling the advice round left in flight (run %s, %s)", r.ID, ticket.Index, r.TicketCount, ended.loadID, ended.outcome)
	r.ConsumeResumeDecision()
	if err := ClearKeptRunsOfRequest(dataDir, r.ID); err != nil {
		log.Printf("request %s: ticket %d: discard the advice round's kept worktree: %v", r.ID, ticket.Index, err)
	}
	fl := flavorOf(ticket.Advice)
	pushed, stop, err := concludeAdviceRound(dp, ctx, dataDir, r, ticket, current, ended, fl, now)
	if stop {
		return err
	}
	last := current
	if pushed == nil && fl.kind == request.ChallengeRoundKind {
		pushed = current
	}
	if pushed != nil {
		if last, stop, err = runAdviceRounds(dp, ctx, dataDir, r, ticket, pushed, cfg, time.Now()); stop {
			return err
		}
	}
	return advanceAcceptedTicket(dataDir, r, ticket, last, cfg.OpenPullRequest, time.Now())
}

// interruptedAdviceOutcome is how the in-flight round adv ended, or, in
// wait, why it cannot be told yet: its run has not ended or its outcome
// cannot be read, and adviceSettleBound has not passed since adv.Since (a
// Since that does not parse has passed it). Past the bound such a round is
// halted, its reason saying so.
func interruptedAdviceOutcome(dataDir string, adv *request.TicketAdvice, now time.Time) (ended correctiveLaunch, wait error) {
	ended.loadID = adv.RunID
	if adv.RunID == "" {
		ended.roundOutcome = roundOutcome{outcome: request.RoundHalted, startFailure: true, errText: "the worker stopped before the advice round's run started"}
		return ended, nil
	}
	var why string
	rec, err := run.Load(dataDir, adv.RunID)
	switch {
	case err != nil:
		why = fmt.Sprintf("the advice round's run %s cannot be read: %v", adv.RunID, err)
	case !finishedRun(rec):
		why = fmt.Sprintf("the advice round's run %s has not ended", adv.RunID)
	default:
		outcome, oerr := correctiveRoundOutcome(dataDir, rec.ID, nil)
		if oerr == nil {
			ended.roundOutcome = outcome
			return ended, nil
		}
		why = fmt.Sprintf("the advice round's run %s's outcome cannot be read: %v", adv.RunID, oerr)
	}
	since, perr := time.Parse(time.RFC3339Nano, adv.Since)
	if perr == nil && now.Sub(since) < adviceSettleBound {
		return ended, fmt.Errorf("%s; waiting up to %s from %s", why, adviceSettleBound, adv.Since)
	}
	ended.roundOutcome = roundOutcome{outcome: request.RoundHalted, errText: fmt.Sprintf("%s after %s: settled as halted", why, adviceSettleBound)}
	return ended, nil
}

// abandonInFlightAdvice ends every advice round a request left in flight when
// it left building (a halt, then a retry into pr_review): once the round's
// run has ended (interruptedAdviceOutcome: waited for, up to
// adviceSettleBound), the round is recorded with its outcome, never pushed,
// and its branch put back on the run it followed (restoreUnpushed), so
// PRHeadRunID and every later round build on that run. stop is true when the request was halted for a branch that could
// not be put back. r is saved when anything changed.
func abandonInFlightAdvice(dp Deps, dataDir string, r *request.Request, now time.Time) (stop bool, err error) {
	changed := false
	for i := range r.Tickets {
		ticket := &r.Tickets[i]
		if ticket.Advice == nil {
			continue
		}
		current, lerr := run.Load(dataDir, ticket.Advice.Of)
		if lerr != nil {
			return false, fmt.Errorf("request %s: ticket %d: load run %s the advice round followed: %w", r.ID, ticket.Index, ticket.Advice.Of, lerr)
		}
		// The branch is put back only once the round's run can no longer
		// move it: while that run has not ended the poll waits, as a
		// settle does, up to adviceSettleBound.
		ended, wait := interruptedAdviceOutcome(dataDir, ticket.Advice, now)
		if wait != nil {
			return false, fmt.Errorf("request %s: ticket %d: %w", r.ID, ticket.Index, wait)
		}
		log.Printf("request %s: ticket %d: the advice round left in flight (run %s, %s) is recorded as not pushed; run %s stands", r.ID, ticket.Index, ended.loadID, ended.outcome, current.ID)
		slice := recordAdviceRound(dp, dataDir, r, ticket, current, ended, flavorOf(ticket.Advice), now)
		ticket.Rounds[slice].Error = strings.TrimPrefix(ticket.Rounds[slice].Error+"; the request left building before the advice round was settled, so nothing was pushed", "; ")
		if stop, err := restoreUnpushed(dp, dataDir, r, ticket, slice, current, now); stop {
			return true, err
		}
		changed = true
	}
	if !changed {
		return false, nil
	}
	return false, r.Save(dataDir)
}

// finishedRun reports whether rec is in a terminal state.
func finishedRun(rec *run.Run) bool {
	return rec.State == run.StateAccepted || rec.State == run.StateQuarantined || rec.State == run.StateHalted
}
