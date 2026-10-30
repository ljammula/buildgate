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

// The rounds after the pull request opens (session key review_advice_rounds,
// 0 to 3, 3 by default; SC-022).
//
// A finding of a ticket run's code review refuses the run only through a
// reproduction the factory runs; every other finding is advice, listed in the
// pull request body. A challenge's held test that failed on an accepted run is
// evidence the factory ran. Both are open items of the run at the head of the
// pull request's branch, and one loop of rounds works on them: each round is
// one build on that branch, given one brief (the red held tests, then the
// high-severity review findings; handoff.Review) as -earlier-attempt, with the
// ticket's unchanged spec, every gate, a fresh review and the held tests run
// again, on the execution role's model. A round never refuses anything:
//
//	round accepted, decision allowed, no held test green on the head now red,
//	push confirmed                                      -> the round's run is the head
//	anything else (not accepted, a regression, a push check refuses)
//	                                                    -> the head stands, the local branch
//	                                                       is put back on it
//	a push that failed once sent                        -> halt, as a PR-review round's
//	                                                       (a no-verdict note when the head had a red test)
//
// Rounds follow while open items remain, after a pushed round, up to the key
// (at least one for a head with a red held test, even at 0). A round whose
// head had no red held test and that left no fewer high-severity findings may
// be pushed and ends the loop. The request stays in building throughout, so
// pr_review still means the factory's work on every ticket is done. Rounds
// count against neither round budget, and run only where the run's pull
// request is open (pull requests on): with them off nothing is pushed, so
// there is nothing to replace the accepted run with.

// adviceRoundName names an advice round in logs and push reasons.
const adviceRoundName = "advice round"

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

// heldReds are the held challenge files that failed on current, the head of
// the ticket's pull request (its own challenge record, or the held files'
// re-run on it); none unless the ticket is challenged.
func heldReds(ticket *request.Ticket, current *run.Run) []run.ChallengeFile {
	if !ticket.Challenged || current.Challenge == nil || current.Challenge.Verdict != run.ChallengeRed {
		return nil
	}
	var reds []run.ChallengeFile
	for _, f := range current.Challenge.Files {
		if f.Verdict == run.ChallengeRed {
			reds = append(reds, f)
		}
	}
	return reds
}

// heldRegressed reports whether a held file that was green on current is red
// on round: a round that fixes one thing and breaks another is not pushed.
func heldRegressed(current, round *run.Run) bool {
	if current.Challenge == nil || round.Challenge == nil {
		return false
	}
	green := map[string]bool{}
	for _, f := range current.Challenge.Files {
		if f.Verdict == run.ChallengeGreen {
			green[f.Path] = true
		}
	}
	for _, f := range round.Challenge.Files {
		if f.Verdict == run.ChallengeRed && green[f.Path] {
			return true
		}
	}
	return false
}

// heldCommitted reports whether the held files are committed on the ticket's
// branch: the head, the run that ran the challenger, or a pushed round of the
// ticket recorded the commit that holds them (ChallengeRecord.CommittedSHA).
func heldCommitted(dataDir string, ticket *request.Ticket, head *run.Run) bool {
	if head.Challenge != nil && head.Challenge.CommittedSHA != "" {
		return true
	}
	ids := []string{ticket.ChallengeRunID}
	for _, rnd := range ticket.Rounds {
		if rnd.Pushed {
			ids = append(ids, rnd.RunID)
		}
	}
	for _, id := range ids {
		if id == "" {
			continue
		}
		if rec, err := run.Load(dataDir, id); err == nil && rec.Challenge != nil && rec.Challenge.CommittedSHA != "" {
			return true
		}
	}
	return false
}

// roundBudget is the rounds the ticket may have: review_advice_rounds, and at
// least one for a head with a red held test that has had no round (the one
// round a red challenge always had).
func roundBudget(ticket *request.Ticket, n int, reds []run.ChallengeFile, cfg WorkerConfig) int {
	if n == 0 && len(reds) > 0 && !ticket.ChallengeRounded {
		return max(cfg.Settings.ReviewAdviceRounds, 1)
	}
	return cfg.Settings.ReviewAdviceRounds
}

// roundApplies reports whether current, the ticket's last pushed run (its
// accepted run, or its last round's), is followed by a round: it has open
// items (a red held test, a high-severity finding), fewer rounds than the
// budget are recorded, the last of them (if any) was pushed, none is in
// flight, and the ticket's pull request is open on current's branch.
func roundApplies(ticket *request.Ticket, current *run.Run, cfg WorkerConfig) bool {
	reds := heldReds(ticket, current)
	if len(reds) == 0 && len(highFindings(current)) == 0 {
		return false
	}
	n, lastPushed := ticket.AdviceRounds()
	return n < roundBudget(ticket, n, reds, cfg) && (n == 0 || lastPushed) && ticket.Advice == nil &&
		cfg.OpenPullRequest && (ticket.PRURL != "" || current.PullRequestURL != "") && current.Branch != ""
}

// acceptAfterAdvice accepts accepted as the ticket's outcome and advances the
// request (acceptTicketRun), after the rounds that apply. The pull request and
// branch recorded are accepted's: the rounds build on that branch and open
// nothing, so a pushed round's run, which has no pull request of its own, is
// the head the next round builds on and never the run the ticket's pull
// request is read from.
func acceptAfterAdvice(dp Deps, ctx context.Context, dataDir string, r *request.Request, ticket *request.Ticket, accepted *run.Run, cfg WorkerConfig, now time.Time) error {
	noteChallenge(ticket, accepted)
	if roundApplies(ticket, accepted, cfg) && accepted.PullRequestURL != "" {
		// The rounds push to the run's open pull request: the ticket records
		// it before the first, as acceptTicketRun would.
		ticket.Branch, ticket.PRURL = accepted.Branch, accepted.PullRequestURL
	}
	if _, stop, err := runRounds(dp, ctx, dataDir, r, ticket, accepted, cfg, now); stop {
		return err
	}
	return acceptTicketRun(dataDir, r, ticket, accepted, cfg.OpenPullRequest, time.Now())
}

// runRounds runs rounds after current while they apply (roundApplies), each
// after the round before it was pushed, and returns the last run whose result
// is the pull request's head. stop is true when the caller must return err at
// once (runRound). A pushed round that followed a head with no red held test
// and left no fewer high-severity findings ends the loop (noProgress).
func runRounds(dp Deps, ctx context.Context, dataDir string, r *request.Request, ticket *request.Ticket, current *run.Run, cfg WorkerConfig, now time.Time) (last *run.Run, stop bool, err error) {
	for roundApplies(ticket, current, cfg) {
		pushed, stop, err := runRound(dp, ctx, dataDir, r, ticket, current, cfg, now)
		if stop || pushed == nil {
			return current, stop, err
		}
		if noProgress(ticket, current, pushed) {
			return pushed, false, nil
		}
		current, now = pushed, time.Now()
	}
	return current, false, nil
}

// noProgress reports whether pushed, a round's run that replaced head, only
// chased review advice without result: head had no red held test and pushed's
// review left no fewer high-severity findings. A red test is evidence the
// factory ran, a finding is an opinion.
func noProgress(ticket *request.Ticket, head, pushed *run.Run) bool {
	return len(heldReds(ticket, head)) == 0 && len(highFindings(pushed)) >= len(highFindings(head))
}

// runRound runs the round that follows current and settles it
// (concludeRound); pushed is the round's run when its result was pushed, nil
// otherwise. A round that cannot start (a launch budget is reached, cannot be
// read, or has less left than current spent; its arguments cannot be made) is
// skipped with a log line: current stands. stop is true when the caller must
// return err at once: the worker is stopping or the request left building
// mid-round (the round stays in flight on the ticket, Ticket.Advice, for the
// next pass to settle), or the request was halted.
func runRound(dp Deps, ctx context.Context, dataDir string, r *request.Request, ticket *request.Ticket, current *run.Run, cfg WorkerConfig, now time.Time) (pushed *run.Run, stop bool, err error) {
	if why := adviceBudgetRefusal(dataDir, r, current, cfg, now); why != "" {
		log.Printf("request %s: ticket %d/%d: no %s: %s; run %s stands", r.ID, ticket.Index, r.TicketCount, adviceRoundName, why, current.ID)
		return nil, false, nil
	}
	n, _ := ticket.AdviceRounds()
	k := n + 1
	roundRunID := fmt.Sprintf("%s-%03d-advice%d", r.ID, ticket.Index, k)
	args, err := adviceRoundArgs(dataDir, r, ticket, current, cfg, roundRunID, k)
	if err != nil {
		log.Printf("request %s: ticket %d/%d: no %s: %v; run %s stands", r.ID, ticket.Index, r.TicketCount, adviceRoundName, err, current.ID)
		return nil, false, nil
	}
	ticket.Advice = &request.TicketAdvice{Of: current.ID, Since: now.UTC().Format(time.RFC3339Nano)}
	if err := r.Save(dataDir); err != nil {
		return nil, true, err
	}
	log.Printf("request %s: ticket %d/%d: run %s accepted with %d red held test file(s) and %d high-severity review finding(s); %s %d/%d", r.ID, ticket.Index, r.TicketCount, current.ID, len(heldReds(ticket, current)), len(highFindings(current)), adviceRoundName, k, roundBudget(ticket, n, heldReds(ticket, current), cfg))
	launched, stop, err := launchCorrectiveRound(dp, ctx, dataDir, r, ticket, args, roundRunID, adviceRoundName, "", run.StackBase(dataDir, current), false, true)
	if stop {
		return nil, true, err
	}
	return concludeRound(dp, ctx, dataDir, r, ticket, current, launched, request.AdviceRoundKind, time.Now())
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

// adviceRoundArgs is round k's build argv: the ticket's unchanged build spec,
// current's branch and diff base, no pull request of its own, the execution
// role's model, and the one record of current (handoff.Review: its red held
// tests, then its high-severity findings) as -earlier-attempt. While the held
// files are not committed on the branch the round also gets them mounted
// read-only with -challenge-round, which commits them once all are green.
func adviceRoundArgs(dataDir string, r *request.Request, ticket *request.Ticket, current *run.Run, cfg WorkerConfig, roundRunID string, k int) ([]string, error) {
	roundDir := filepath.Join(request.Dir(dataDir, r.ID), "rounds", fmt.Sprintf("%03d-advice%d", ticket.Index, k))
	reds := heldReds(ticket, current)
	start := adviceStart
	if len(reds) > 0 {
		start = challengeStart
	}
	recordPath, err := writeRecordFile(roundDir, handoff.Review(current), start)
	if err != nil {
		return nil, err
	}
	buildSpecPath, err := writeTicketBuildInput(dataDir, r, *ticket)
	if err != nil {
		return nil, fmt.Errorf("build spec: %w", err)
	}
	uncommitted := ticket.Challenged && !heldCommitted(dataDir, ticket, current)
	argv, err := correctiveBuildArgs(dataDir, r, *ticket, cfg, buildSpecPath, roundRunID, current.Branch, current.DiffBase(), true, uncommitted)
	if err != nil {
		return nil, err
	}
	if len(reds) > 0 && uncommitted && !hasFlagArg(argv, "-challenge-round") {
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

// challengeStart is the sentence a round's record opens with when it carries
// red held tests: where the build starts, and that the attempt was accepted.
const challengeStart = startsOnItsBranch + " That attempt was accepted: what follows is not a failure of a check but tests written against it that fail, and any reviewer's concerns about it."

// concludeRound records the round that followed current (ended is how its
// build ended; kind is its Round kind) and settles which run is the ticket's:
// the round's, returned as pushed, once it was accepted, no held test green
// on current is red on it, and its result is confirmed on the pull request's
// branch (pushRoundResult, afterRoundPushed), after which the pull request
// gets one comment saying what it fixed (roundComment); current otherwise,
// with the local branch put back on current's result (restoreUnpushed) and a
// declined comment when current had a red held test. A push check that
// refuses leaves the remote untouched and current standing. A push or
// read-back that failed once the push was sent halts the request, as for a
// PR-review round (a head with a red held test ends the round as no verdict
// instead: a challenge never halts, SC-022): the commit may be on the remote,
// and a human reconciles the branch. Nothing is saved here: the caller's next
// save records the round with the ticket's next step, and until then the
// round stays in flight on disk, so a pass that is lost in between settles it
// again (a repeated push of the same commit is a no-op).
func concludeRound(dp Deps, ctx context.Context, dataDir string, r *request.Request, ticket *request.Ticket, current *run.Run, ended correctiveLaunch, kind string, now time.Time) (pushed *run.Run, stop bool, err error) {
	slice := recordAdviceRound(dp, dataDir, r, ticket, current, ended, kind, now)
	notPushed := func(roundRun *run.Run, note string) (*run.Run, bool, error) {
		if note != "" {
			ticket.Rounds[slice].Error = note
		}
		commentDeclined(dp, ctx, dataDir, r, ticket, current, roundRun)
		stop, err := restoreUnpushed(dp, dataDir, r, ticket, slice, current, now)
		return nil, stop, err
	}
	if ended.outcome != request.RoundAccepted {
		log.Printf("request %s: ticket %d/%d: %s %s %s; run %s stands", r.ID, ticket.Index, r.TicketCount, adviceRoundName, ended.loadID, ended.outcome, current.ID)
		roundRun, _ := run.Load(dataDir, ended.loadID)
		return notPushed(roundRun, "")
	}
	roundRun, err := run.Load(dataDir, ended.loadID)
	if err != nil {
		note := fmt.Sprintf("load the round's run: %v", err)
		log.Printf("request %s: ticket %d/%d: %s %s: %s; run %s stands", r.ID, ticket.Index, r.TicketCount, adviceRoundName, ended.loadID, note, current.ID)
		return notPushed(nil, note)
	}
	if heldRegressed(current, roundRun) {
		log.Printf("request %s: ticket %d/%d: %s %s accepted but a held test green on run %s is red on it; run %s stands", r.ID, ticket.Index, r.TicketCount, adviceRoundName, roundRun.ID, current.ID, current.ID)
		return notPushed(roundRun, "the round was accepted but a held test that passed on the run it followed fails on it")
	}
	refused, failed := pushRoundResult(dp, ctx, dataDir, r, ticket, roundRun, current.Branch, adviceRoundName)
	switch {
	case failed != "" && len(heldReds(ticket, current)) > 0:
		log.Printf("request %s: ticket %d/%d: %s %s: %s; run %s stands", r.ID, ticket.Index, r.TicketCount, adviceRoundName, roundRun.ID, failed, current.ID)
		ticket.Rounds[slice].Error = "no verdict: " + failed
		stop, err := restoreUnpushed(dp, dataDir, r, ticket, slice, current, now)
		return nil, stop, err
	case failed != "":
		return nil, true, HaltRequest(dataDir, r, failed, now)
	case refused != "":
		log.Printf("request %s: ticket %d/%d: %s %s accepted but not pushed: %s; run %s stands", r.ID, ticket.Index, r.TicketCount, adviceRoundName, roundRun.ID, refused, current.ID)
		ticket.Rounds[slice].Error = refused
		stop, err := restoreUnpushed(dp, dataDir, r, ticket, slice, current, now)
		return nil, stop, err
	}
	afterRoundPushed(dp, ctx, r, ticket, slice, roundRun)
	ticket.RunID = roundRun.ID
	log.Printf("request %s: ticket %d/%d: %s %s accepted and pushed to %s as %s", r.ID, ticket.Index, r.TicketCount, adviceRoundName, roundRun.ID, current.Branch, roundRun.ResultSHA)
	commentCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := dp.CommentOnPullRequest(commentCtx, ticket.PRURL, roundComment(ticket, current, roundRun)); err != nil {
		log.Printf("request %s: ticket %d: comment on %s about %s %s: %v", r.ID, ticket.Index, ticket.PRURL, adviceRoundName, roundRun.ID, err)
	}
	return roundRun, false, nil
}

// recordAdviceRound records the round (of Round kind kind) that followed
// current, ended as ended, on ticket, ends it in flight, makes current the ticket's run
// again and tags the round's commit; it returns the round's index in
// ticket.Rounds.
func recordAdviceRound(dp Deps, dataDir string, r *request.Request, ticket *request.Ticket, current *run.Run, ended correctiveLaunch, kind string, now time.Time) int {
	ticket.Advice = nil
	ticket.RunID = current.ID
	ticket.Rounds = append(ticket.Rounds, request.Round{
		Index: ticket.NextRoundIndex(), Kind: kind, RunID: ended.loadID, Outcome: ended.outcome,
		StartFailure: ended.startFailure, Unresolved: ended.unresolved, Error: ended.errText,
		At: now.UTC().Format(time.RFC3339Nano),
	})
	tagRoundByID(dp, dataDir, r, ticket, ended.loadID, kind, now)
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

// roundComment is the one comment a round's confirmed push leaves on the pull
// request, saying what it fixed: the held tests that failed on current and
// pass on the round (challengeComment) and the high-severity findings current
// was given (adviceComment), whichever there were.
func roundComment(ticket *request.Ticket, current, roundRun *run.Run) string {
	var parts []string
	if reds := heldReds(ticket, current); len(reds) > 0 {
		parts = append(parts, challengeComment(reds, current, roundRun))
	}
	if len(highFindings(current)) > 0 {
		parts = append(parts, adviceComment(current, roundRun))
	}
	return strings.Join(parts, "\n\n")
}

// challengeComment is the part of a round's comment about its held tests:
// which of current's red files are green on the round (path and criterion,
// never content), whether they are committed on the branch, and which are
// still red.
func challengeComment(reds []run.ChallengeFile, current, roundRun *run.Run) string {
	verdict := map[string]string{}
	if roundRun.Challenge != nil {
		for _, f := range roundRun.Challenge.Files {
			verdict[f.Path] = f.Verdict
		}
	}
	var fixed, still strings.Builder
	for _, f := range reds {
		line := fmt.Sprintf("\n- `%s` (%s)", handoff.CleanSentence(f.Path), handoff.CleanWord(f.Criterion))
		if verdict[f.Path] == run.ChallengeGreen {
			fixed.WriteString(line)
		} else {
			still.WriteString(line)
		}
	}
	var b strings.Builder
	if fixed.Len() > 0 {
		fmt.Fprintf(&b, "A round fixed the change: it now passes the held challenge tests that failed on the earlier head (%s):\n%s", shortSHA(current.ResultSHA), fixed.String())
		if roundRun.Challenge != nil && roundRun.Challenge.CommittedSHA != "" {
			b.WriteString("\n\nThe tests are committed on this branch.")
		}
	}
	if still.Len() > 0 {
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		fmt.Fprintf(&b, "These held challenge tests still fail on it, and a person decides whether the code or the test is wrong:\n%s", still.String())
	}
	return b.String()
}

// challengeDeclinedComment is the comment for a round that left the run it
// followed as the ticket's while that run had red held tests: each red file's
// path, the criterion it cites and the build's one-line reason, cleaned;
// never a file's content.
func challengeDeclinedComment(dataDir string, reds []run.ChallengeFile, roundRun *run.Run) string {
	reason := "the round gave no reason"
	if roundRun != nil {
		if line := handoff.NoteLine(run.Dir(dataDir, roundRun.ID)); line != "" {
			reason = line
		}
	}
	var b strings.Builder
	b.WriteString("A challenge test the factory ran against this change still fails; a round did not make it pass, so the tests are not committed. For each one a person decides whether the code or the test is wrong:\n")
	for _, f := range reds {
		fmt.Fprintf(&b, "\n- `%s` (%s): %s", handoff.CleanSentence(f.Path), handoff.CleanWord(f.Criterion), reason)
	}
	return b.String()
}

// commentDeclined posts the declined comment on the ticket's pull request
// when current had red held tests (a round that only followed review advice
// posts nothing); a failed comment is logged.
func commentDeclined(dp Deps, ctx context.Context, dataDir string, r *request.Request, ticket *request.Ticket, current, roundRun *run.Run) {
	reds := heldReds(ticket, current)
	if len(reds) == 0 {
		return
	}
	commentCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := dp.CommentOnPullRequest(commentCtx, ticket.PRURL, challengeDeclinedComment(dataDir, reds, roundRun)); err != nil {
		log.Printf("request %s: ticket %d: comment on %s about the %s: %v", r.ID, ticket.Index, ticket.PRURL, adviceRoundName, err)
	}
}

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

// roundKindOf is the Round kind of the round left in flight: a record written
// before the rounds were one loop may mark it a challenge round, which is
// settled as any round and recorded as it was.
func roundKindOf(adv *request.TicketAdvice) string {
	if adv != nil && adv.Challenge {
		return request.ChallengeRoundKind
	}
	return request.AdviceRoundKind
}

// settleInterruptedAdviceRound settles the advice round a pass left in flight
// (Ticket.Advice: the worker stopped or was lost mid-round), never starting
// it again: its run, once ended, is concluded as any round's
// (concludeRound); one that never started is a start failure; and one
// whose run has not ended, or whose outcome cannot be read, is waited for
// (the pass ends with an error and the step is tried again) until
// adviceSettleBound has passed since the round started, then settled as
// halted. The rounds that still apply follow a pushed one (runRounds),
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
	kind := roundKindOf(ticket.Advice)
	pushed, stop, err := concludeRound(dp, ctx, dataDir, r, ticket, current, ended, kind, now)
	if stop {
		return err
	}
	last := current
	if pushed == nil && kind == request.ChallengeRoundKind {
		pushed = current
	}
	if pushed != nil && (pushed == current || !noProgress(ticket, current, pushed)) {
		if last, stop, err = runRounds(dp, ctx, dataDir, r, ticket, pushed, cfg, time.Now()); stop {
			return err
		}
	} else if pushed != nil {
		last = pushed
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
		slice := recordAdviceRound(dp, dataDir, r, ticket, current, ended, roundKindOf(ticket.Advice), now)
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
