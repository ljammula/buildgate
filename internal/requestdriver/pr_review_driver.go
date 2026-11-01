package requestdriver

import (
	"buildgate/internal/unresolved"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"buildgate/internal/forge"
	"buildgate/internal/handoff"
	"buildgate/internal/notify"
	"buildgate/internal/release"
	"buildgate/internal/request"
	"buildgate/internal/run"
	"buildgate/internal/runner"
	"buildgate/internal/ticketspec"
	"buildgate/internal/workspace"
)

// This file is the PR-review driver's own: everything that happens while a
// request sits in request.StatePRReview. request_driver.go's own
// AdvanceRequest dispatches here for that one state (see its own switch);
// nothing else in this package reaches into a request's pr_review
// bookkeeping.
//
// The request driver (a separate, concurrently-developed piece) owns
// starting each ticket's build and the transition INTO pr_review -- by
// the time AdvancePRReview below is ever called, r.Tickets[r.TicketIndex-1].RunID
// and .PRURL are already set. ContinueAfterPRApproval (request_driver.go)
// is the hook out of pr_review once this file marks the current ticket
// "approved".

// readReviewState reads prURL's current review state -- a package-level
// var, not a direct forge.GHPullRequestOpener{}.ReadReviewState call, so a
// test can stub it without a real gh binary or network. Production always
// uses forge.GHPullRequestOpener{}, the same zero-value opener
// release_and_evidence.go's own accepted-run PR-open call site uses.
func RealReadReviewState(ctx context.Context, prURL string, policy forge.AuthorPolicy) (forge.ReviewState, error) {
	return forge.GHPullRequestOpener{}.ReadReviewState(ctx, prURL, policy)
}

// PrReviewCorrectiveRunner, when a test sets it, runs a corrective build in
// place of runMainWithReady (TicketRunner's own shape -- see
// worker_config.go), the same function the worker's queue drainer and the
// request driver's spec/plan jobs ultimately bottom out in.
var PrReviewCorrectiveRunner TicketRunner

// RealReadJobLog is the real body of Deps.ReadJobLog.
func RealReadJobLog(ctx context.Context, prURL string, jobID int64) (string, error) {
	return forge.GHPullRequestOpener{}.ReadJobLog(ctx, prURL, jobID)
}

// correctiveRunner is override when a test set one, else runMainWithReady
// reaching the outside through dp.
func correctiveRunner(dp Deps, override TicketRunner) TicketRunner {
	if override != nil {
		return override
	}
	return dp.RunTicket
}

// markPullRequestReady shells `gh pr ready <url>`, clearing a PR's draft
// status. a boundary method so a test can stub it without a real gh
// binary.
func RealMarkPullRequestReady(ctx context.Context, prURL string) error {
	cmd := exec.CommandContext(ctx, "gh", "pr", "ready", prURL)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("gh pr ready %s: %w: %s", prURL, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// undoMarkPullRequestReady shells `gh pr ready --undo <url>`, converting
// prURL back to draft -- advancePRReadyOrApproved's own narrow-the-race
// mitigation (see its own doc comment): the decision can be invalidated
// in the gap between this file's own check and forge.markPullRequestReady's
// network round-trip, so a second, immediate re-check right after the
// ready-flip catches that and reverts it rather than leaving a
// policy-denied PR sitting ready-for-review indefinitely. A package-level
// var so a test can stub it without a real gh binary.
func RealUndoMarkPullRequestReady(ctx context.Context, prURL string) error {
	cmd := exec.CommandContext(ctx, "gh", "pr", "ready", "--undo", prURL)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("gh pr ready --undo %s: %w: %s", prURL, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// retargetPullRequestBase shells `gh pr edit <url> --base <base>` --
// retargetOffMergedTicket's mechanism for moving a still-open
// stacked PR off a base branch that just merged, onto that merged
// ticket's own base. a boundary method so a test can stub it without a
// real gh binary.
func RealRetargetPullRequestBase(ctx context.Context, prURL, base string) error {
	cmd := exec.CommandContext(ctx, "gh", "pr", "edit", prURL, "--base", base)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("gh pr edit %s --base %s: %w: %s", prURL, base, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// pushExistingBranch shells `git push origin <sha>:refs/heads/<branch>`
// from workspaceDir -- deliberately never -f/--force (see this file's own
// doc comment on never force-pushing): a corrective round's own commits
// land as new commits on branch, pushed the ordinary way, exactly like a
// human pushing a fixup commit to their own open PR branch would. sha is
// pushed explicitly, not the bare branch name (found live, Flutter + Go app run 3,
// 2026-09-28, PR #331): `git push origin <branch>` pushes whatever the
// LOCAL ref named branch happens to point to in workspaceDir, which is a
// silent no-op ("Everything up-to-date", exit 0) if that local ref was
// never actually advanced by this round -- exactly what happened when
// -on-branch itself was still dropped by the Temporal path (see
// RunWorkflowInput.OnBranch's own doc comment for that root cause): the
// round's local branch ref in its own worktree stayed at the PR's
// pre-round head, and this push reported success without moving anything
// on the remote. Pushing `sha:refs/heads/branch` makes the intended
// result explicit regardless of what the local ref happens to be, and a
// non-fast-forward on the remote (someone else pushed to branch
// meanwhile) still fails exactly as before -- git refuses any non-FF
// update without -f. a boundary method so a test can stub it without a
// real git remote.
func RealPushExistingBranch(ctx context.Context, workspaceDir, sha, branch string) error {
	cmd := exec.CommandContext(ctx, "git", "push", "origin", sha+":refs/heads/"+branch)
	cmd.Dir = workspaceDir
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git push origin %s:refs/heads/%s: %w: %s", sha, branch, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// remoteBranchHeadSHA returns the SHA refs/heads/<branch> currently
// resolves to on the origin remote, read from workspaceDir -- the
// verification half of pushAcceptedRoundAndReply's defense in depth: a
// push that reports success is not itself proof the remote branch now
// points where this round intended (found live, Flutter + Go app run 3,
// 2026-09-28, PR #331 -- see forge.pushExistingBranch's own doc comment for
// the exact failure mode this closes). a boundary method so a test can
// stub it without a real git remote.
func RealRemoteBranchHeadSHA(ctx context.Context, workspaceDir, branch string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", "ls-remote", "origin", "refs/heads/"+branch)
	cmd.Dir = workspaceDir
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git ls-remote origin refs/heads/%s: %w", branch, err)
	}
	fields := strings.Fields(string(out))
	if len(fields) == 0 {
		return "", fmt.Errorf("%w: git ls-remote origin refs/heads/%s", ErrRemoteBranchNotFound, branch)
	}
	return fields[0], nil
}

// roundResultDescendsFromHead reports whether ancestor is an ancestor
// of (or equal to) descendant in dir -- pushAcceptedRoundAndReply's own
// pre-push check that an accepted round's result genuinely builds on top
// of the PR branch's current remote head, wrapping
// runner.GitIsAncestor (the same "git merge-base --is-ancestor" this
// package's -diff-base validation already uses, run_ticket.go). A
// boundary method, like every other git side effect in this file, so a
// test can stub it without a real git repository.
func RealRoundResultDescendsFromHead(dir, ancestor, descendant string) (bool, error) {
	return runner.GitIsAncestor(dir, ancestor, descendant)
}

// pullRequestOwnerRepoPattern extracts owner/repo from a GitHub PR URL --
// mirrors internal/forge's own unexported pullRequestURLPattern (that
// package's parsePullRequestURL is unexported too, so this is a small,
// deliberate duplication across the package boundary rather than exporting
// a forge-internal helper just for this one call site).
var pullRequestOwnerRepoPattern = regexp.MustCompile(`^https://github\.com/([^/]+)/([^/]+)/pull/(\d+)`)

func ParsePullRequestOwnerRepo(prURL string) (owner, repo string, number int, err error) {
	m := pullRequestOwnerRepoPattern.FindStringSubmatch(prURL)
	if m == nil {
		return "", "", 0, fmt.Errorf("not a GitHub pull request URL: %q", prURL)
	}
	number, err = strconv.Atoi(m[3])
	if err != nil {
		return "", "", 0, fmt.Errorf("pull request number in %q: %w", prURL, err)
	}
	return m[1], m[2], number, nil
}

// RealCommentOnPullRequest posts body as a comment on prURL's pull request
// (`gh pr comment <url> --body-file -`, the body on stdin).
func RealCommentOnPullRequest(ctx context.Context, prURL, body string) error {
	cmd := exec.CommandContext(ctx, "gh", "pr", "comment", prURL, "--body-file", "-")
	cmd.Stdin = strings.NewReader(body)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("gh pr comment %s: %w: %s", prURL, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// ReplyToReviewCommentPath is GitHub's REST "reply to a review comment"
// endpoint, which is scoped by pull number:
// POST /repos/{owner}/{repo}/pulls/{pull_number}/comments/{comment_id}/replies.
func ReplyToReviewCommentPath(owner, repo string, number int, commentID int64) string {
	return fmt.Sprintf("repos/%s/%s/pulls/%d/comments/%d/replies", owner, repo, number, commentID)
}

// replyToReviewComment posts body as a reply to the review comment
// identified by commentID (forge.Thread's own CommentID -- the GraphQL
// databaseId of that thread's latest comment, not the thread's own node
// id), via GitHub's REST "reply to a review comment" endpoint. A
// boundary method so a test can stub it without a real gh binary.
//
// This never resolves the thread itself (see this file's own doc comment
// on never resolving a thread the factory did not reply to) -- resolution
// is a human's call, made in GitHub's own UI once they see this reply.
func RealReplyToReviewComment(ctx context.Context, prURL string, commentID int64, body string) error {
	if commentID == 0 {
		return fmt.Errorf("thread has no recorded comment id; refusing to guess one")
	}
	owner, repo, number, err := ParsePullRequestOwnerRepo(prURL)
	if err != nil {
		return err
	}
	path := ReplyToReviewCommentPath(owner, repo, number, commentID)
	cmd := exec.CommandContext(ctx, "gh", "api", path, "-f", "body="+body)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("gh api %s: %w: %s", path, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// ReviewComment is one pull request review comment as GitHub's REST API
// lists it: InReplyToID is the thread's root comment for a reply, 0 for a
// root.
type ReviewComment struct {
	ID          int64  `json:"id"`
	InReplyToID int64  `json:"in_reply_to_id"`
	Body        string `json:"body"`
}

// listReviewComments lists every review comment on prURL's pull request.
// a boundary method so a test can stub it without a real gh binary.
func RealListReviewComments(ctx context.Context, prURL string) ([]ReviewComment, error) {
	owner, repo, number, err := ParsePullRequestOwnerRepo(prURL)
	if err != nil {
		return nil, err
	}
	path := fmt.Sprintf("repos/%s/%s/pulls/%d/comments", owner, repo, number)
	// One compact JSON object per line, across every page.
	out, err := exec.CommandContext(ctx, "gh", "api", "--paginate", path, "--jq", ".[] | {id, in_reply_to_id, body} | @json").Output()
	if err != nil {
		return nil, fmt.Errorf("gh api %s: %w", path, err)
	}
	var comments []ReviewComment
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		var c ReviewComment
		if err := json.Unmarshal([]byte(line), &c); err != nil {
			return nil, fmt.Errorf("gh api %s: parse comment: %w", path, err)
		}
		comments = append(comments, c)
	}
	return comments, nil
}

// ReplyLanded reports whether comments already hold a reply with exactly
// body in the thread containing commentID. GitHub files every reply under
// the thread's root comment, so the match is on that root, not on commentID
// itself; matching the thread as well as the body keeps one round's
// identical "Addressed in <sha>." replies on different threads apart.
func ReplyLanded(comments []ReviewComment, commentID int64, body string) bool {
	root := commentID
	for _, c := range comments {
		if c.ID == commentID && c.InReplyToID != 0 {
			root = c.InReplyToID
		}
	}
	for _, c := range comments {
		if c.InReplyToID == root && c.Body == body {
			return true
		}
	}
	return false
}

// PostReviewReply posts body as a reply to commentID's thread. gh can exit
// non-zero after GitHub has created the reply (seen live: "unexpected end of
// JSON input" on a reply that posted), so on a gh error it lists the PR's
// comments and reports success when the reply is there; the error is
// returned only when it is not, or when the check itself fails.
func PostReviewReply(dp Deps, ctx context.Context, prURL string, commentID int64, body string) error {
	err := dp.ReplyToReviewComment(ctx, prURL, commentID, body)
	if err == nil {
		return nil
	}
	comments, listErr := dp.ListReviewComments(ctx, prURL)
	if listErr != nil {
		return fmt.Errorf("%w (checking whether it posted anyway: %v)", err, listErr)
	}
	if ReplyLanded(comments, commentID, body) {
		log.Printf("reply to review comment %d posted; gh reported an error after posting it: %v", commentID, err)
		return nil
	}
	return err
}

// noPullRequestHaltReason builds AdvancePRReview's halt message for a
// ticket whose run was accepted but has no PRURL, distinguishing the two
// structurally different causes a human reading it needs to tell apart:
//
//   - the release decision denied it (release.LoadDecision returns a
//     non-nil, denied/invalidated decision): `factoryd retry` re-checks
//     the decision (see retryPullRequestOpener) against this run's own
//     recorded policy, finds it still denies, and falls back to
//     rebuilding the ticket under today's -release-* configuration -- the
//     message points at fixing the policy flags first, since retrying with them
//     unchanged rebuilds for nothing (the override endpoint is dropped
//     entirely, not offered as an option that can't work here).
//   - anything else (no decision on file, or a decision that was itself
//     Allowed -- meaning openEvidencePullRequest's own best-effort
//     push/open attempt was the thing that failed): `factoryd retry`
//     re-attempts only the push/PR-open against this same accepted run
//     and branch -- no rebuild, so a fresh attempt costs nothing but a
//     `gh pr create` call and may simply succeed this time.
//
// Falls back to the generic message on any error loading the run or its
// decision (run.Load/release.LoadDecision), rather than failing this
// halt outright -- the halt itself (a human needs to see *something*) is
// the load-bearing behavior; which of the two wordings it uses is not
// worth losing the halt over.
func noPullRequestHaltReason(dataDir string, r *request.Request, ticket *request.Ticket) string {
	generic := fmt.Sprintf("ticket %d/%d: run %s was accepted but no pull request was opened; `factoryd retry %s` re-attempts only the pull request open against this same accepted run and branch (no rebuild)", ticket.Index, r.TicketCount, ticket.RunID, r.ID)
	loaded, err := run.Load(dataDir, ticket.RunID)
	if err != nil {
		return generic
	}
	if loaded.PROpenError != "" {
		return fmt.Sprintf("ticket %d/%d: run %s was accepted but the pull request could not be opened: %s -- `factoryd retry %s` re-attempts only the pull request open (no rebuild); if that error is a permanent refusal (for example GitHub rejecting the branch) fix the cause first or the retry will hit it again", ticket.Index, r.TicketCount, ticket.RunID, loaded.PROpenError, r.ID)
	}
	decision, err := release.LoadDecision(dataDir, release.ProjectOf(loaded), ticket.RunID)
	if err != nil || decision == nil || decision.Allowed {
		return generic
	}
	// Neither "retry" nor "override" is unconditionally the wrong advice
	// here, but each needed a real correction (found via review, GitHub
	// Codex App, PR #154 round 2): run.ApplyOverride only accepts a
	// QUARANTINED run (TestApplyOverrideRejectsNotQuarantined) and this
	// run is ACCEPTED-but-denied, so the override endpoint always fails
	// here -- dropped entirely, not offered as an option that can't work.
	// `factoryd retry` DOES help, but only if the operator changes the
	// -release-* policy configuration first: retryPullRequestOpener
	// re-evaluates a fresh release.Decision against this run's OWN
	// recorded ReleasePolicy (mergePolicyFromRun) first, deterministically
	// denying again against the same evidence when that's unchanged --
	// but internal/request.Retry then falls back to rebuilding the
	// ticket from scratch for exactly this case, and a
	// fresh build picks up whatever -release-* flags are configured now.
	return fmt.Sprintf("ticket %d/%d: run %s was accepted but no pull request was opened: denied by release policy (%s) -- fix the -release-* policy configuration (rollback plan, size limits, required gates) first; retrying with it unchanged rebuilds the ticket for nothing (still denied), but `factoryd retry %s` after fixing it rebuilds under the new policy and can open a pull request then", ticket.Index, r.TicketCount, ticket.RunID, strings.Join(decision.Reasons, "; "), r.ID)
}

// nextUnbuiltTicket reports whether ticket, the lowest one with no pull
// request, is the ticket after the current one and was never built. With
// pull requests on, under advance_on: accepted, that happens one way: acceptTicketRun halted the
// request on the current ticket's release decision before building ticket,
// and a retry has since opened the current ticket's pull request. There is
// nothing to halt on: the build goes on from ticket.
func nextUnbuiltTicket(r *request.Request, ticket *request.Ticket) bool {
	return RequestAdvanceOn == AdvanceOnAccepted && ticket.RunID == "" && ticket.Index == r.TicketIndex+1
}

// AdvancePRReview is request_driver.go's own dispatch target for
// request.StatePRReview: reads the current ticket's PR review state (at
// most once per -pr-poll-interval, per ticket -- see prPollDue) and reacts
// to what it finds. Returns nil on every ordinary outcome (nothing to do
// yet, a round started, ready/approved recorded) -- only a durable-save
// failure or a structurally impossible ticket index is ever returned as an
// error; a request moving to halted on a real failure (rounds exhausted),
// or to closed on a PR closed unmerged, is itself a normal, nil-returning
// outcome, the same shape HaltRequest's own callers elsewhere in this
// package already use.
func AdvancePRReview(dp Deps, ctx context.Context, dataDir string, r *request.Request, cfg WorkerConfig, now time.Time) error {
	if len(r.Tickets) == 0 {
		return fmt.Errorf("request %s: in pr_review with no tickets", r.ID)
	}
	// An advice round left in flight when the request left building is
	// recorded as not pushed before anything reads a ticket's PR head.
	if stop, err := abandonInFlightAdvice(dp, dataDir, r, now); stop || err != nil {
		return err
	}
	// awaitingHalt is r.AwaitingPRTicket() (internal/request), the SAME
	// selection Retry uses to pick which ticket a `factoryd retry` acts
	// on: this loop used to pick its own halt target independently (an inline
	// "ticket.PRURL == """ check), which required RunID != "" over in
	// Retry's own now-removed private selection -- the two could disagree
	// about which ticket a halt was even about. Computed once, by pointer
	// identity against each ticket below, so this loop's own halt
	// decision and Retry's are structurally guaranteed to agree.
	awaitingHalt, hasAwaitingHalt := r.AwaitingPRTicket()
	// Every ticket with an open PR is watched, not only the current one:
	// under advance_on: accepted a request reaches pr_review after its
	// LAST ticket, with every earlier ticket's PR still open and still
	// able to draw reviewer comments or merge. Any ticket whose handling
	// moves the request out of pr_review (completed, halted, or resumed
	// building) ends this poll.
	for i := range r.Tickets {
		ticket := &r.Tickets[i]
		if ticket.PRState == "merged" {
			continue
		}
		if hasAwaitingHalt && ticket == awaitingHalt {
			// The ticket's run was accepted but no pull request exists for
			// it: nothing here can ever poll, merge, or complete it. Halt
			// with a reason a human sees, rather than skipping it on every
			// poll forever.
			//
			// Two structurally different causes get two different
			// messages (see noPullRequestHaltReason's own doc comment):
			// the release decision denied it outright -- `factoryd retry`
			// re-runs the exact same deterministic policy check and gets
			// denied again, burning a paid agent build for nothing -- or
			// openEvidencePullRequest's own best-effort push/open attempt
			// simply failed, where a retry (or just re-running the opener)
			// may well succeed.
			if cfg.OpenPullRequest && nextUnbuiltTicket(r, ticket) {
				r.TicketIndex = ticket.Index
				if err := r.ResumeBuilding(now); err != nil {
					return err
				}
				return r.Save(dataDir)
			}
			return haltRequestAcceptedNoPR(dataDir, r, noPullRequestHaltReason(dataDir, r, ticket), now)
		}
		if err := pollTicketPR(dp, ctx, dataDir, r, ticket, cfg, now); err != nil {
			return err
		}
		if r.State != request.StatePRReview {
			return nil
		}
	}
	return nil
}

// pollTicketPR is AdvancePRReview's per-ticket body: read the PR's review
// state (at most once per -pr-poll-interval) and react to it.
func pollTicketPR(dp Deps, ctx context.Context, dataDir string, r *request.Request, ticket *request.Ticket, cfg WorkerConfig, now time.Time) error {
	due, err := prPollDue(ticket.LastPolledAt, cfg.PrPollInterval, now)
	if err != nil {
		return fmt.Errorf("request %s: ticket %d: %w", r.ID, ticket.Index, err)
	}
	if !due {
		return nil
	}

	state, readErr := dp.ReadReviewState(ctx, ticket.PRURL, prAuthorPolicy(cfg))
	// stillInState (request_driver.go): the network read above can take
	// long enough for an operator's cancel to land mid-poll; discard the
	// result rather than let the r.Save calls below resurrect the request.
	// See AdvanceSpecDrafting's identical check for the full rationale.
	if ok, err := stillInState(dataDir, r.ID, request.StatePRReview); err != nil {
		return err
	} else if !ok {
		log.Printf("request %s: ticket %d: PR poll finished but the request left pr_review while it ran (e.g. cancelled) -- discarding the result", r.ID, ticket.Index)
		return nil
	}
	ticket.LastPolledAt = now.UTC().Format(time.RFC3339Nano)
	if readErr != nil {
		// Transient (network, gh auth, a rate limit): logged, retried next
		// poll: LastPolledAt above still advances so a broken PR URL
		// cannot spin this loop every tick.
		log.Printf("request %s: ticket %d: read PR review state: %v", r.ID, ticket.Index, readErr)
		return r.Save(dataDir)
	}

	if state.BaseRefName != "" {
		ticket.PRBase = state.BaseRefName
	}
	if state.ShouldStopPolling() {
		return handleClosedOrMergedTicket(dp, dataDir, r, ticket, state, now)
	}
	noteReopenedPR(ticket, state)
	// A stacked ticket whose base moved is restacked first (restack.go):
	// until its restack is pushed and confirmed it is neither retargeted nor
	// marked ready, whatever its base on GitHub now says.
	restacked, restackHeld, err := restackIfNeeded(dp, ctx, dataDir, r, ticket, state.HeadSHA, cfg, now)
	if err != nil || restacked {
		return err
	}
	if merged := mergedTicketOnBranch(r, ticket, state.BaseRefName); merged != nil && !restackHeld {
		if retargetOffMergedTicket(dp, ctx, dataDir, r, ticket, merged, state) {
			return r.Save(dataDir)
		}
		// Not retargeted this poll: carry on with comments and approval as
		// usual (a stall here would silently stop corrective rounds); the
		// ready flip still skips it, since its base is another ticket's
		// branch (stackedOnAnotherTicket).
	}

	if len(state.BlocksReadyThreads) > 0 && len(state.ActionableThreads) == 0 {
		// Real, still-open review comments exist, but none are from a
		// trusted author -- they will keep blocking the ready-flip (see
		// advancePRReadyOrApproved's own len(state.BlocksReadyThreads)
		// check) without ever triggering a corrective round. Logged every
		// poll this holds, not just once, since this is a log line an
		// operator needs to notice, not a notification to deduplicate.
		log.Printf("request %s: ticket %d: %d review thread(s) from untrusted authors are open but won't trigger a corrective round; set -pr-trusted-authors to act on them", r.ID, ticket.Index, len(state.BlocksReadyThreads))
	}

	newThreads := forge.NewUnresolvedThreads(state.ActionableThreads, ticket.SeenThreadIDs)
	if len(newThreads) > 0 && r.Source.Kind == request.SourceMemory {
		return answerMemoryRequestThreads(dp, ctx, dataDir, r, ticket, newThreads)
	}
	if len(newThreads) > 0 {
		return RunCorrectiveRound(dp, ctx, dataDir, r, ticket, newThreads, cfg, now)
	}
	if restackHeld {
		ticket.MergeReadiness = nil
		return r.Save(dataDir)
	}

	if handled, err := handleCIChecks(dp, ctx, dataDir, r, ticket, state, cfg, now); handled {
		return err
	}

	return advancePRReadyOrApproved(dp, ctx, dataDir, r, ticket, state, now)
}

// MemoryRequestReviewReply is what a trusted reviewer's comment on a memory
// request's pull request is answered with. It is public, so it names no path
// and no run.
const MemoryRequestReviewReply = "This pull request changes the repository memory section of `AGENTS.md`. Its text is the text the operator approved, and the factory releases only that exact text, so no corrective build is started for this comment. To change a line, close this pull request (that ends the request) and propose the change again with `factoryd memory propose`."

// answerMemoryRequestThreads handles new actionable threads on a memory
// request's pull request: each is replied to once and recorded as seen, and
// no corrective round starts. A build that reworded a line would be refused
// at release, after its cost, because the file would no longer be the text
// that was approved. A thread whose reply could not be posted is not recorded
// and is answered on the next poll.
func answerMemoryRequestThreads(dp Deps, ctx context.Context, dataDir string, r *request.Request, ticket *request.Ticket, threads []forge.Thread) error {
	replyCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	for _, t := range threads {
		if err := PostReviewReply(dp, replyCtx, ticket.PRURL, t.CommentID, MemoryRequestReviewReply); err != nil {
			log.Printf("request %s: ticket %d: reply on thread %s of a memory request: %v", r.ID, ticket.Index, t.ID, err)
			continue
		}
		ticket.SeenThreadIDs = append(ticket.SeenThreadIDs, forge.ThreadSeenKey(t))
	}
	return r.Save(dataDir)
}

// prPollDue reports whether a ticket last polled at lastPolledAt (RFC3339Nano,
// or "" if never) is due for another forge.ReadReviewState call at now:
// true when never polled, or when at least interval has elapsed since.
// Mirrors reminderDue's own shape (request_driver.go) for the same
// restart-safety reason: no in-memory ticker state for a crash or restart
// to lose, since LastPolledAt is read straight off disk on every call via
// r.Tickets itself.
func prPollDue(lastPolledAt string, interval time.Duration, now time.Time) (bool, error) {
	if lastPolledAt == "" {
		return true, nil
	}
	last, err := time.Parse(time.RFC3339Nano, lastPolledAt)
	if err != nil {
		return false, fmt.Errorf("parse last_polled_at %q: %w", lastPolledAt, err)
	}
	return now.Sub(last) >= interval, nil
}

// handleClosedOrMergedTicket handles a ticket's PR leaving OPEN. MERGED
// records the merge, and completes the request once every ticket has
// merged. CLOSED (closed without merging) ends the request as closed when it
// is the last ticket's: the factory never merges, and never re-opens a PR a
// person closed. An earlier ticket's, which a later ticket builds on, parks
// the request in resume_review instead, for the operator to reopen it and
// resume, or cancel. Any other state is a read the poll does not trust (an
// empty or unknown state from gh): it is logged and the next poll reads
// again, so a misread can never end a request.
func handleClosedOrMergedTicket(dp Deps, dataDir string, r *request.Request, ticket *request.Ticket, state forge.ReviewState, now time.Time) error {
	switch {
	case state.MergedAt != nil || state.State == forge.PRStateMerged:
		recordTicketMerged(ticket)
		allMerged := true
		for _, t := range r.Tickets {
			if t.PRState != "merged" {
				allMerged = false
				break
			}
		}
		if allMerged {
			if err := r.Complete(now); err != nil {
				return err
			}
		}
		return r.Save(dataDir)
	case state.State == forge.PRStateClosed:
		ticket.PRState = "closed"
		if ticket.Index < r.TicketCount {
			return parkRequest(dp, dataDir, r, fmt.Sprintf("ticket %d/%d's pull request %s was closed without merging, and ticket %d builds on it: reopen it", ticket.Index, r.TicketCount, ticket.PRURL, ticket.Index+1), now)
		}
		reason := fmt.Sprintf("ticket %d/%d: pull request %s closed without merging", ticket.Index, r.TicketCount, ticket.PRURL)
		if err := r.Close(reason, now); err != nil {
			return err
		}
		log.Printf("request %s: pr_review -> closed (%s)", r.ID, reason)
		return notifyClosedRequest(dataDir, r, ticket, now)
	default:
		log.Printf("request %s: ticket %d: pull request %s reported state %q, neither open, merged nor closed -- reading it again next poll", r.ID, ticket.Index, ticket.PRURL, state.State)
		return r.Save(dataDir)
	}
}

// mergedTicketOnBranch returns the ticket in r whose Branch is
// baseRefName and whose PR has merged: ticket's PR is stacked on a branch
// that is done, so it should move to that ticket's own base. nil when
// ticket isn't stacked on a merged ticket.
func mergedTicketOnBranch(r *request.Request, ticket *request.Ticket, baseRefName string) *request.Ticket {
	if baseRefName == "" {
		return nil
	}
	for i := range r.Tickets {
		other := &r.Tickets[i]
		if other.Index != ticket.Index && other.Branch == baseRefName && other.PRState == "merged" {
			return other
		}
	}
	return nil
}

// retargetOffMergedTicket moves ticket's stacked PR onto the base merged's
// PR targeted (merged.PRBase, recorded by its own polls) with `gh pr edit
// --base`, and reports whether it did. It requires the same two things as
// the ready flip: ticket's current head run's release decision still
// allows it (decisionStillAllows), and GitHub's PR head is that run's
// ResultSHA (prHeadIsRun). It runs on ticket's own poll, so a refusal or
// a gh error is logged and retried next poll. Nothing here merges anything.
func retargetOffMergedTicket(dp Deps, ctx context.Context, dataDir string, r *request.Request, ticket, merged *request.Ticket, state forge.ReviewState) bool {
	if merged.PRBase == "" {
		log.Printf("request %s: ticket %d: PR is stacked on merged ticket %d, whose base branch is not recorded; retarget it by hand (gh pr edit %s --base <default branch>)", r.ID, ticket.Index, merged.Index, ticket.PRURL)
		return false
	}
	headRunID := ticket.PRHeadRunID()
	allowed, err := decisionStillAllows(dataDir, headRunID)
	if err != nil {
		log.Printf("request %s: ticket %d: check release decision before retargeting off merged ticket %d: %v", r.ID, ticket.Index, merged.Index, err)
		return false
	}
	if !allowed {
		log.Printf("request %s: ticket %d: release decision denies this run; not retargeting off merged ticket %d", r.ID, ticket.Index, merged.Index)
		return false
	}
	if !prHeadIsRun(dataDir, headRunID, state.HeadSHA) {
		log.Printf("request %s: ticket %d: PR head %s is not run %s's result; not retargeting off merged ticket %d", r.ID, ticket.Index, state.HeadSHA, headRunID, merged.Index)
		return false
	}
	editCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	err = dp.RetargetPullRequestBase(editCtx, ticket.PRURL, merged.PRBase)
	cancel()
	if err != nil {
		log.Printf("request %s: ticket %d: gh pr edit --base %s (off merged ticket %d): %v", r.ID, ticket.Index, merged.PRBase, merged.Index, err)
		return false
	}
	ticket.Retarget = &request.TicketRetarget{From: state.BaseRefName, To: merged.PRBase, At: time.Now().UTC().Format(time.RFC3339Nano)}
	ticket.PRBase = merged.PRBase
	log.Printf("request %s: ticket %d: retargeted pull request %s to %s now that ticket %d merged", r.ID, ticket.Index, ticket.PRURL, merged.PRBase, merged.Index)
	return true
}

// prHeadIsRun reports whether headSHA (GitHub's headRefOid) is runID's
// ResultSHA, prefix-matched the same way as the ready flip's check;
// either SHA unknown fails closed.
func prHeadIsRun(dataDir, runID, headSHA string) bool {
	loaded, err := run.Load(dataDir, runID)
	if err != nil || headSHA == "" || loaded.ResultSHA == "" {
		return false
	}
	return strings.HasPrefix(headSHA, loaded.ResultSHA) || strings.HasPrefix(loaded.ResultSHA, headSHA)
}

// decisionStillAllows reports whether runID's release decision, loaded
// fresh from disk right now, still allows it: the run itself loads, its
// decision loads, and the decision is neither missing, invalidated, nor
// denied. Factored out of advancePRReadyOrApproved's own inline check so
// its post-ready-flip re-check (see that function's own doc comment on
// the narrow TOCTOU window between the pre-flip check and
// forge.markPullRequestReady's network round-trip) can ask the identical
// question a second time without duplicating the run.Load/
// release.LoadDecision plumbing.
func decisionStillAllows(dataDir, runID string) (bool, error) {
	loaded, err := run.Load(dataDir, runID)
	if err != nil {
		return false, fmt.Errorf("load run: %w", err)
	}
	decision, err := release.LoadDecision(dataDir, release.ProjectOf(loaded), runID)
	if err != nil {
		return false, fmt.Errorf("load release decision: %w", err)
	}
	return decision != nil && !decision.Invalidated && decision.Allowed, nil
}

// stackedOnAnotherTicket reports whether baseRefName is another ticket's
// Branch in r: a stacked draft PR (ticketQueueEntry's -pr-base). It stays
// draft while its base is another ticket's branch, merged or not, so a
// human can never be offered a PR that would merge into that branch
// instead of the real base; retargetOffMergedTicket moves it once the
// ticket underneath merges, and the next poll flips it as usual.
func stackedOnAnotherTicket(r *request.Request, ticket *request.Ticket, baseRefName string) bool {
	return r.StackedOnAnotherTicket(ticket, baseRefName)
}

// advancePRReadyOrApproved is the PR-review driver's own "ready for
// review, approved" handling, reached only once
// state.ShouldStopPolling() is false (still OPEN) and there are no new
// unresolved threads for RunCorrectiveRound to turn into a round.
//
// checksPassing == nil (ReviewState's own "no configured status checks at
// all" case) is treated as passing here, deliberately: a repository with no
// CI configured must not block a PR from ever leaving draft or ever
// advancing on approval. A pending/failing check (checksPassing != nil,
// false) does block both.
func advancePRReadyOrApproved(dp Deps, ctx context.Context, dataDir string, r *request.Request, ticket *request.Ticket, state forge.ReviewState, now time.Time) error {
	checksOK := state.ChecksPassing == nil || *state.ChecksPassing
	if state.IsDraft && stackedOnAnotherTicket(r, ticket, state.BaseRefName) {
		// A stacked draft stays draft -- reviewers can still approve it
		// (GitHub allows approving a draft PR, and ReviewDecision below is
		// unaffected), but flipping it ready-for-review here would put
		// ticket N-1's own still-open commits in front of a human reviewer
		// a second time, under ticket N's PR, before N-1 has even merged.
		// retargetOffMergedTicket (this PR's own poll) retargets it
		// once that ticket merges, and the next poll's own
		// stackedOnAnotherTicket check then sees an ordinary base and
		// flips it normally.
		log.Printf("request %s: ticket %d: PR is stacked on another ticket's branch %s; leaving it draft", r.ID, ticket.Index, state.BaseRefName)
		// Recorded so the console can tell this draft (waiting on a human
		// to merge its base) from one the factory is still checking.
		ticket.PRState = "stacked"
	} else if state.IsDraft && checksOK && len(state.BlocksReadyThreads) == 0 {
		// Re-check the release decision immediately before flipping the PR
		// out of draft: InvalidateDecision (invalidatePriorRunOnFullSuiteRegression)
		// can deny an already-open PR's decision after the fact, in
		// response to a later successor's own full_suite_verify
		// regression -- `gh pr ready` has no way to know or care about
		// that on its own, so this closes the gap by asking release.
		// LoadDecision the same question openEvidencePullRequest's own
		// gate already asks before ever opening the PR in the first
		// place. Missing/invalidated/denied all skip the ready-flip
		// (logged, not halted -- a human still sees this ticket's PR
		// stuck in draft via `factoryd status`/the PR itself).
		//
		// Ticket.PRHeadRunID, not ticket.RunID unconditionally: an
		// accepted corrective round (RunCorrectiveRound/
		// pushAcceptedRoundAndReply) moves the PR branch's real HEAD to
		// THAT round's own ResultSHA, which has its own, separately
		// evaluated release.Decision -- if the cumulative correction
		// itself now violates a protected-path or size policy, that
		// round's decision can be denied even while the ticket's
		// original decision (evaluated against a since-superseded SHA)
		// is still allowed. Checking ticket.RunID unconditionally would
		// ready a PR whose actual head was never re-cleared by policy
		// (found via review, GitHub Codex App, PR #154).
		headRunID := ticket.PRHeadRunID()
		loaded, loadErr := run.Load(dataDir, headRunID)
		var decision *release.Decision
		var decisionErr error
		if loadErr == nil {
			decision, decisionErr = release.LoadDecision(dataDir, release.ProjectOf(loaded), headRunID)
		}
		switch {
		case loadErr != nil:
			log.Printf("request %s: ticket %d: load run for release decision check: %v", r.ID, ticket.Index, loadErr)
		case decisionErr != nil:
			log.Printf("request %s: ticket %d: load release decision: %v", r.ID, ticket.Index, decisionErr)
		case decision == nil || decision.Invalidated || !decision.Allowed:
			log.Printf("request %s: ticket %d: release decision denies this run; skipping gh pr ready", r.ID, ticket.Index)
		case state.HeadSHA == "" || loaded.ResultSHA == "" || !strings.HasPrefix(state.HeadSHA, loaded.ResultSHA) && !strings.HasPrefix(loaded.ResultSHA, state.HeadSHA):
			// This whole decision-check exists to gate a side effect on
			// the release.Decision that actually describes the PR's real
			// current head -- but Ticket.PRHeadRunID's own selection is
			// built entirely from LOCALLY persisted Round.Pushed history.
			// A maintainer pushing directly to the branch, or a crash
			// between forge.pushExistingBranch's own confirmed success and
			// this file's durable Round.Pushed/ticket save, leaves that
			// history describing a commit GitHub's real head has already
			// moved past -- ReadReviewState's own state.HeadSHA (GitHub's
			// headRefOid) is compared against the selected run's own
			// ResultSHA here specifically to catch that, failing closed
			// (skip, not proceed) on any mismatch or on either SHA being
			// unknown (found via review, GitHub Codex App, PR #154 round
			// 5). Prefix-matched, not exact-equal: `gh`'s headRefOid is
			// always a full 40-hex SHA, but ResultSHA may be recorded
			// short by an older or third-party caller.
			log.Printf("request %s: ticket %d: PR head %s does not match evaluated run %s's own result %s; skipping gh pr ready", r.ID, ticket.Index, state.HeadSHA, headRunID, loaded.ResultSHA)
		default:
			readyCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			err := dp.MarkPullRequestReady(readyCtx, ticket.PRURL)
			cancel()
			if err != nil {
				log.Printf("request %s: ticket %d: gh pr ready: %v", r.ID, ticket.Index, err)
				break
			}
			// Immediate re-check: this file's own check-then-act gap
			// (release.LoadDecision above, then forge.markPullRequestReady's
			// own network round-trip) is a real, if narrow, TOCTOU window
			// -- InvalidateDecision (invalidatePriorRunOnFullSuiteRegression)
			// runs from a different accepted run's own completion and
			// takes no lock this file holds, so it can deny headRunID's
			// decision at any point during that round-trip (found via
			// review, GitHub Codex App, PR #154 round 2). A second
			// LoadDecision immediately after narrows, rather than
			// eliminates, the window: revert via `gh pr ready --undo`
			// when it turns out the decision was denied in that gap,
			// rather than leaving a policy-denied PR sitting
			// ready-for-review until some future poll happens to notice.
			stillAllowed, recheckErr := decisionStillAllows(dataDir, headRunID)
			switch {
			case recheckErr != nil:
				// The ready-flip itself already succeeded and this is
				// only a best-effort extra safety check -- a failure to
				// even re-check is logged, not treated as grounds to
				// undo a ready-flip that, as far as this function
				// verified a moment ago, was legitimate.
				log.Printf("request %s: ticket %d: re-check release decision after gh pr ready: %v", r.ID, ticket.Index, recheckErr)
				ticket.PRState = "ready"
			case !stillAllowed:
				undoCtx, undoCancel := context.WithTimeout(ctx, 30*time.Second)
				undoErr := dp.UndoMarkPullRequestReady(undoCtx, ticket.PRURL)
				undoCancel()
				if undoErr != nil {
					log.Printf("request %s: ticket %d: revert gh pr ready after release decision denied mid-flip: %v", r.ID, ticket.Index, undoErr)
					ticket.PRState = "ready"
				} else {
					log.Printf("request %s: ticket %d: release decision denied between the pre-flip check and gh pr ready; reverted the PR to draft", r.ID, ticket.Index)
				}
			default:
				ticket.PRState = "ready"
			}
		}
	}
	// len(state.BlocksReadyThreads) == 0 gates approval too, not just the
	// ready-flip above: GitHub's own reviewDecision can report APPROVED
	// (a required review satisfied by someone else, say) while a fresh,
	// unresolved thread from an untrusted or ignored human still sits on
	// the PR -- BlocksReadyThreads exists precisely so a real, unaddressed
	// human comment is never treated as resolved just because it isn't
	// actionable, and that contract has to hold here too, not only at the
	// `gh pr ready` check: without this, such a thread would never block
	// `ticket.PRState = "approved"`/`ContinueAfterPRApproval` starting the
	// next ticket, even though it visibly blocks readiness (found via
	// review, GitHub Codex App, PR #154 round 2).
	ticket.MergeReadiness = checkMergeReadiness(dataDir, ticket, state, now)
	notifyPullRequestWaiting(dataDir, r, ticket, now)
	if state.ApprovedOnCurrentHead() && len(state.BlocksReadyThreads) == 0 {
		ticket.PRState = "approved"
		if err := r.Save(dataDir); err != nil {
			return err
		}
		if ticket.Index == r.TicketIndex {
			return ContinueAfterPRApproval(dataDir, r, now)
		}
		return nil
	}
	return r.Save(dataDir)
}

// checkMergeReadiness checks ticket's open pull request against the
// ready-to-merge bar (request.MergeReadiness) and names everything it still
// lacks. It is called at the end of a poll that found no new thread to act
// on, after the ready flip, so ticket.PRState is this poll's.
//
// "The last code review of the whole diff" is the code_review gate of the
// run whose commit is the pull request's head (Ticket.PRHeadRunID): the
// ticket's first build, or the last corrective round or restack replay run
// that was accepted and pushed, each of which reviews the diff from the
// ticket's base (a replay run's, from the new base it was replayed onto). A head the
// factory did not build (someone pushed to the branch) has no such review,
// and neither has a run built with code review off: both are blockers, not
// passes. A repository with no checks configured has none failing.
func checkMergeReadiness(dataDir string, ticket *request.Ticket, state forge.ReviewState, now time.Time) *request.MergeReadiness {
	var blockers []string
	draft := false
	switch {
	case ticket.PRState == "stacked":
		blockers = append(blockers, "it is stacked on an earlier ticket's pull request, which must merge first")
	case state.IsDraft && ticket.PRState != "ready":
		blockers = append(blockers, "it is still a draft")
		draft = true
	}
	if state.ChecksPassing != nil && !*state.ChecksPassing {
		blockers = append(blockers, "its checks are pending or failing")
	}
	if n := len(state.BlocksReadyThreads); n > 0 {
		blockers = append(blockers, fmt.Sprintf("%d review thread(s) are open", n))
	}
	if state.ReviewDecision == forge.ReviewDecisionChangesRequested {
		blockers = append(blockers, "a reviewer requested changes")
	}
	blockers = append(blockers, headReviewBlockers(dataDir, ticket.PRHeadRunID(), state.HeadSHA)...)
	ciBlockers, notes := ciReadiness(state)
	blockers = append(blockers, ciBlockers...)
	return &request.MergeReadiness{
		Notes:     notes,
		Ready:     len(blockers) == 0,
		Draft:     draft,
		CheckedAt: now.UTC().Format(time.RFC3339Nano),
		HeadSHA:   state.HeadSHA,
		Blockers:  blockers,
	}
}

// headReviewBlockers is checkMergeReadiness's half about the build behind
// the pull request's head: headRunID must be an accepted run whose result
// is headSHA, whose release decision still allows it, and whose code_review
// gate ran and passed.
func headReviewBlockers(dataDir, headRunID, headSHA string) []string {
	loaded, err := run.Load(dataDir, headRunID)
	if err != nil {
		return []string{fmt.Sprintf("the run behind its head (%s) could not be read", headRunID)}
	}
	if loaded.State != run.StateAccepted {
		return []string{fmt.Sprintf("the run behind its head (%s) is %s, not accepted", headRunID, loaded.State)}
	}
	if !prHeadIsRun(dataDir, headRunID, headSHA) {
		return []string{fmt.Sprintf("its head %s is not the commit the factory last built and reviewed (%s)", shortSHA(headSHA), shortSHA(loaded.ResultSHA))}
	}
	var blockers []string
	if allowed, err := decisionStillAllows(dataDir, headRunID); err != nil || !allowed {
		blockers = append(blockers, fmt.Sprintf("the release decision for run %s does not allow it", headRunID))
	}
	reviewed := false
	for _, g := range loaded.GateResults {
		if g.Check != "code_review" {
			continue
		}
		reviewed = true
		if !g.Passed {
			blockers = append(blockers, "the last code review of the whole diff did not pass")
		}
	}
	if !reviewed {
		blockers = append(blockers, fmt.Sprintf("run %s has no code review of the whole diff on record (code review was off)", headRunID))
	}
	return blockers
}

// shortSHA is sha's first 12 characters, or "unknown" when it is empty.
func shortSHA(sha string) string {
	if sha == "" {
		return "unknown"
	}
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

// RunCorrectiveRound is the PR-review driver's own core: newThreads
// (already filtered to "not seen before" by AdvancePRReview's own
// forge.NewUnresolvedThreads call) becomes one build against ticket's
// existing PR branch.
//
// Cap enforcement happens BEFORE anything is written or run: a request
// already at max_review_rounds for this ticket halts immediately, with
// none of newThreads marked seen and no round recorded -- so an operator
// who raises the cap and un-halts (once that path exists) finds the same
// unresolved threads still waiting, not silently dropped.
//
// The cap counts only rounds whose run actually started (StartFailure ==
// false on the recorded Round) -- countedRounds below, not len(ticket.
// Rounds). A round that fails before its run ever executes (a start
// failure: PrepareOnBranch refusing the worktree, say, with no run record
// ever created, or one created but with zero recorded Attempts) is still
// appended to ticket.Rounds with its own Error, but never consumes a
// max_review_rounds slot -- found live: three consecutive start failures
// on the exact same infrastructure bug burned the whole cap before a
// single real review attempt ever ran, halting the request on "rounds
// exhausted" for a reason that had nothing to do with the review itself.
// roundIndex (raw sequence, len(ticket.Rounds)+1) is kept as-is for
// roundRunID/addendum-directory naming, which must stay globally unique
// per attempt regardless of whether it counts against the cap.
func RunCorrectiveRound(dp Deps, ctx context.Context, dataDir string, r *request.Request, ticket *request.Ticket, newThreads []forge.Thread, cfg WorkerConfig, now time.Time) error {
	return runPRReviewRound(dp, ctx, dataDir, r, ticket, roundTrigger{threads: newThreads}, cfg, now)
}

// runPRReviewRound is RunCorrectiveRound over either trigger of a PR-review
// round (roundTrigger): review threads, or one failed required CI check.
func runPRReviewRound(dp Deps, ctx context.Context, dataDir string, r *request.Request, ticket *request.Ticket, trig roundTrigger, cfg WorkerConfig, now time.Time) error {
	newThreads := trig.threads
	roundIndex := len(ticket.Rounds) + 1
	countedRounds := 0
	for _, rnd := range ticket.Rounds {
		// A round with a Kind is an automatic corrective round of the
		// ticket's build, counted separately against
		// -review-corrective-rounds (runCorrectiveRounds,
		// request_driver.go) -- excluded here so a ticket's PR-review
		// budget is never burned by a round that ran before any PR even
		// existed.
		if rnd.Kind == "" && rnd.Counted() {
			countedRounds++
		}
	}
	if countedRounds >= cfg.MaxReviewRounds {
		return HaltRequest(dataDir, r, fmt.Sprintf("review rounds exhausted for ticket %d/%d", ticket.Index, r.TicketCount), now)
	}

	if check, reason, err := CheckLaunchBudget(dataDir, r, cfg.Settings, now); err != nil {
		return err
	} else if check != "" {
		return quarantineRequestWithCheck(dataDir, r, reason, check, now)
	}

	roundRunID := fmt.Sprintf("%s-%03d-review%d", r.ID, ticket.Index, roundIndex)
	// An open thread is being acted on: the last readiness check no longer
	// describes the pull request. The poll after the round checks again.
	ticket.MergeReadiness = nil
	// Saved before the round's build starts, so `factoryd status` does not
	// show a pull request a round is about to push to as safe to merge.
	if err := r.Save(dataDir); err != nil {
		return err
	}

	// The round builds on the ticket's current PR-head run (the commit on
	// the pull request's branch now), on the branch that run pushed, and
	// its -diff-base is the ticket's original base carried on that run, so
	// the diff-shape gates (diff_scope, tests_red_on_base) judge the cumulative diff the PR as a
	// whole will merge, not just this round's own small delta from the
	// branch tip it starts on (see loadPRHead and -diff-base's flag help).
	head, err := loadPRHead(dataDir, ticket)
	if err != nil {
		return fmt.Errorf("request %s: ticket %d: round %d: %w", r.ID, ticket.Index, roundIndex, err)
	}
	branch, diffBase, stackBaseSHA := head.Branch, head.DiffBase, run.StackBase(dataDir, head.Run)

	// Threads are marked seen only once an accepted round's commit has
	// actually been pushed (pushAcceptedRoundAndReply), never before: a
	// quarantined or halted round leaves them eligible for the next poll,
	// so a fixable comment is retried (bounded by max_review_rounds,
	// which counts every round run) instead of being silently dropped
	// forever. The seen key includes the thread's latest comment id
	// (forge.ThreadSeenKey), so a reviewer replying "still not fixed" on
	// an already-addressed thread counts as new.

	// The round before this one, when it was quarantined: its commits are
	// the tip this round builds on (priorRoundRefusal), so its builder is
	// given that attempt's record, beside the reviewer's comments.
	refused := priorRoundRefusal(dataDir, r, ticket)
	ciRecord := trig.record(dp, ctx, head.Run, ticket)
	// One round is one build; it counts once against max_review_rounds.
	addendumPath, includedThreads, err := writeRoundAddendum(dataDir, r.ID, ticket, newThreads, roundIndex, 0)
	if err != nil {
		return fmt.Errorf("request %s: ticket %d: round %d: write addendum: %w", r.ID, ticket.Index, roundIndex, err)
	}
	attempt, err := runRoundAttempt(dp, ctx, dataDir, r, ticket, cfg, addendumPath, roundRunID, branch, diffBase, stackBaseSHA, refused, ciRecord)
	if err != nil {
		return fmt.Errorf("request %s: ticket %d: round %d: %w", r.ID, ticket.Index, roundIndex, err)
	}
	if attempt.errText != "" {
		log.Printf("request %s: ticket %d: round %d: %s: %s", r.ID, ticket.Index, roundIndex, attempt.outcome, attempt.errText)
	}
	// A cancel that landed while the attempt built must not be undone by the
	// saves below, nor answered with another paid build.
	if ok, err := stillInState(dataDir, r.ID, request.StatePRReview); err != nil {
		return err
	} else if !ok {
		log.Printf("request %s: ticket %d: round %d finished but the request left pr_review while it ran (e.g. cancelled) -- discarding the result", r.ID, ticket.Index, roundIndex)
		return nil
	}
	loadID, outcome, errText := attempt.runID, attempt.outcome, attempt.errText

	threadIDs := make([]string, len(includedThreads))
	for i, t := range includedThreads {
		threadIDs[i] = t.ID
	}
	ticket.Rounds = append(ticket.Rounds, request.Round{
		Index:        roundIndex,
		ThreadIDs:    threadIDs,
		CICheck:      trig.checkName(),
		RunID:        loadID,
		Outcome:      outcome,
		StartFailure: attempt.startFailure,
		Unresolved:   attempt.unresolved,
		At:           now.UTC().Format(time.RFC3339Nano),
		Error:        errText,
	})
	tagRoundByID(dp, dataDir, r, ticket, loadID, request.RoundTagPRReview, now)

	if attempt.unresolved != "" {
		// No verdict on the round's work: it used no round, and unlike a
		// start failure the next poll must not start another on its own
		// (an outage would repeat without end). A human decides; the
		// reviewer is told nothing, since nothing was judged.
		log.Printf("request %s: ticket %d: round %d gave no verdict (%s): %s", r.ID, ticket.Index, roundIndex, attempt.unresolved, errText)
		return EnterResumeReview(dp, dataDir, r, request.LostStep{Cause: attempt.unresolved, RetryAt: attempt.retryAt}, now)
	}
	if outcome != request.RoundAccepted {
		// Quarantined or halted: never push, request stays in pr_review,
		// one notification dispatched -- see this file's own doc comment.
		notifyRoundOutcome(dataDir, r, ticket, roundIndex, outcome, errText, now)
		if err := r.Save(dataDir); err != nil {
			return err
		}
		// The reviewer is told on their own thread, after the round is on
		// disk. A start failure is not: it used no round and is retried on
		// the next poll, so it would repeat the same reply every poll.
		if !attempt.startFailure {
			replyRoundNotPushed(dp, ctx, dataDir, r, ticket, includedThreads, attempt, countedRounds+1, cfg.MaxReviewRounds)
		}
		return nil
	}

	// len(ticket.Rounds)-1, not roundIndex: Round.Index is the 1-based
	// sequence number used for naming (roundIndex above), but the slice
	// position of the entry just appended is what pushAcceptedRoundAndReply
	// needs to mark Pushed on the correct element.
	return pushAcceptedRoundAndReply(dp, ctx, dataDir, r, ticket, len(ticket.Rounds)-1, loadID, branch, includedThreads, now)
}

// roundAttempt is how one build of a PR-review round ended: the run that
// holds its record, and correctiveRoundOutcome's reading of it.
type roundAttempt struct {
	runID string
	roundOutcome
}

// roundOutcome is how a corrective round's run ended, read from its record
// (correctiveRoundOutcome).
type roundOutcome struct {
	outcome request.RoundOutcome
	// errText is why a round that was not accepted failed, "" when its gate
	// results already say.
	errText string
	// startFailure: the run never started (no record, or no attempt).
	startFailure bool
	// unresolved: the run ended with no verdict on the work (it halted for
	// such a cause, or its review did not answer); retryAt is the time the
	// provider then said to try again, when it gave one.
	unresolved unresolved.Cause
	retryAt    string
}

// runRoundAttempt runs one build of a PR-review round on the pull request's
// branch, with specPath (the round's addendum) as its ticket, and reports
// how it ended. ticketID is the -ticket value; the run's own id comes from
// onReady. refused, when not nil, is the quarantined build before this one:
// this one is given its record as -earlier-attempt (SC-018), from a file
// beside the addendum.
func runRoundAttempt(dp Deps, ctx context.Context, dataDir string, r *request.Request, ticket *request.Ticket, cfg WorkerConfig, specPath, ticketID, branch, diffBase, stackBaseSHA string, refused *run.Run, ciRecord *handoff.Document) (roundAttempt, error) {
	// -open-pull-request is always forced off for a corrective round,
	// regardless of cfg.openPullRequest: this run's branch already has an
	// open PR (opened when the ticket was first built), so a second
	// -open-pull-request would either fail (branch already has a PR) or,
	// worse, attempt one against the wrong base.
	roundCfg := cfg
	roundCfg.OpenPullRequest = false
	// The same QueueEntry builder as the ticket's first build and its
	// automatic review round (ticketQueueEntry), so a PR-review round gets
	// what they get: the request's full-suite command
	// and its source, harness, model and preflight profile. SpecPath is the
	// round's addendum: the
	// ticket's own spec plus this round's appended sections.
	entry, err := ticketQueueEntry(dataDir, r, *ticket, cfg, ticketID, specPath)
	if err != nil {
		return roundAttempt{}, err
	}
	if err := withChallenge(entry, dataDir, r, *ticket, false); err != nil {
		return roundAttempt{}, err
	}
	// A round opens no PR of its own (roundCfg.OpenPullRequest above), so
	// it has no draft PR body for -pr-closes-issue to land in and no base
	// to stack on.
	entry.IssueRef = ""
	entry.PRBase = ""
	args := append(BuildTicketRunArgs(dataDir, entry, roundCfg), "-on-branch", branch, "-diff-base", diffBase)
	if args, err = withEarlierRecord(dataDir, filepath.Dir(specPath), refused, ciRecord, args); err != nil {
		return roundAttempt{}, err
	}

	// startedRunID is this attempt's own actual run id, captured the moment
	// the run itself exists (same onReady pattern advanceBuild uses for a
	// ticket's first build, request_driver.go's own ticket.RunID =
	// started.ID) -- not ticketID, which is only the -ticket value this
	// attempt's argv requests. run_ticket.go's id assignment
	// (`id := *runID; if id == "" { id = ticket-timestamp-pid }`) means
	// the run's real durable directory is never named exactly ticketID
	// unless -run-id is also passed, which this call never does: found
	// live (real worker, real corrective round) -- every round's own
	// correctiveRoundOutcome call below was resolving run.Load(dataDir,
	// roundRunID) against a directory that never existed, misreporting a
	// real quarantine (or acceptance) as a start failure every time.
	var startedRunID string
	runErr := correctiveRunner(dp, PrReviewCorrectiveRunner)(ctx, args, func(started *run.Run) {
		startedRunID = started.ID
		// RequestID back-reference, same as the corrective round's own
		// onReady callback (request_driver.go's launchCorrectiveRound)
		// -- without it a PR-review corrective round's run record carries no
		// link back to the request that spawned it, so findOwningRequest's
		// fast path (owning_request.go) and any rollup keyed off run.Run.
		// RequestID silently missed every PR-review round's own run.
		started.RequestID = r.ID
		started.TicketIndex = ticket.Index
		started.StackBaseSHA = stackBaseSHA
		if err := started.Persist(dataDir); err != nil {
			log.Printf("request %s: ticket %d: save run %s request id: %v", r.ID, ticket.Index, started.ID, err)
		}
	})
	// loadID falls back to ticketID only when the run never got far
	// enough to reach onReady (startedRunID still "") -- run.Load then
	// fails not-found, same as it always did for that case, and
	// correctiveRoundOutcome's own loadErr-with-runErr branch reports it
	// as a genuine start failure using runErr's text.
	loadID := startedRunID
	if loadID == "" {
		loadID = ticketID
	}
	ended, loadErr := correctiveRoundOutcome(dataDir, loadID, runErr)
	if loadErr != nil {
		return roundAttempt{}, loadErr
	}
	return roundAttempt{runID: loadID, roundOutcome: ended}, nil
}

// withEarlierRecord adds to a PR-review build's args, from the one file it
// writes in dir, the record of what the build follows (-earlier-attempt):
// refused's handoff when it is a failure a build is told about (opening by
// saying the attempt's commit is in the workspace), and the failed CI check
// of ciRecord when the round is a CI round. When both exist they are one
// record, the refused attempt's with the CI failure set on it, so the file
// stays within the limit (handoff.MaxCILogBytes beside a Markdown record of
// at most 8000 bytes, under the build's 32 KiB)..
func withEarlierRecord(dataDir, dir string, refused *run.Run, ciRecord *handoff.Document, args []string) ([]string, error) {
	var doc handoff.Document
	start := startsOnItsBranch
	ok := false
	if refused != nil {
		doc, ok = handoffForABuild(dataDir, refused)
	}
	switch {
	case ok && ciRecord != nil:
		doc.CIFailure = ciRecord.CIFailure
		start = ciAfterRefusalStart
	case ciRecord != nil:
		doc, ok, start = *ciRecord, true, ciStart
	}
	if !ok {
		return args, nil
	}
	path, err := writeRecordFile(dir, doc, start)
	if err != nil {
		return args, err
	}
	return append(args, "-earlier-attempt", path), nil
}

// correctiveRoundOutcome resolves a just-finished corrective run's real
// outcome from its own durable record (run.Load), never from
// PrReviewCorrectiveRunner's own returned error alone -- the same
// never-trust-self-report posture every other terminal-state decision in
// this codebase already takes. runErr is consulted only when the durable
// record itself cannot be loaded at all (an infrastructure failure so early
// nothing was ever saved): that case is reported as RoundHalted rather than
// failing this function outright, since a corrective round that never
// produced a record is exactly what "halted" already means for an ordinary
// run.
// correctiveRoundOutcome also returns errText: the underlying reason a
// non-accepted round actually failed, for the caller to record on
// request.Round.Error and surface in its own notification/log line --
// never silently dropped the way it previously was (found live: a round
// that halted at start, before a single command ran, produced a run
// record with State Halted but an empty HaltError, and runErr -- the
// only place the real reason existed -- was discarded once outcome was
// resolved). Prefers the run's own HaltError when set (the record itself
// pinpoints why); falls back to runErr's own text otherwise (covers
// exactly the halted-with-no-HaltError case above, and the
// record-never-loaded case below). Empty for an accepted round.
// startFailure reports whether this round never actually started: no run
// record was ever saved at all (loadErr != nil, checked by the caller
// before this function's own switch can run), or one was saved but with
// zero recorded Attempts -- the run's own execution never reached its
// first build/verify invocation. Only relevant for the halted case: an
// accepted or quarantined run can only reach that state after actually
// attempting work, so it is never a start failure. See RunCorrectiveRound's
// own doc comment for why this distinction exists (a start failure must
// not consume a max_review_rounds slot). unresolved is the halted run's
// recorded cause when it ended with no verdict on the work
// (run.UnresolvedCauseOf), or unresolved.RouteCredential for a round with
// no record whose runErr says its route credential could not be used
// (routeCredentialStep): such a round is not counted either, and its
// request waits in resume_review.
func correctiveRoundOutcome(dataDir, runID string, runErr error) (roundOutcome, error) {
	loaded, loadErr := run.Load(dataDir, runID)
	if loadErr != nil {
		if runErr != nil {
			// A round refused before its record existed because its route
			// credential could not be used is no verdict either: it waits
			// for a human, rather than starting again every poll.
			ended := roundOutcome{outcome: request.RoundHalted, errText: runErr.Error(), startFailure: true}
			if step, lost := routeCredentialStep(runErr); lost {
				ended.unresolved = step.Cause
			}
			return ended, nil
		}
		return roundOutcome{}, fmt.Errorf("load round run %q: %w", runID, loadErr)
	}
	if loaded.State == run.StateAccepted {
		return roundOutcome{outcome: request.RoundAccepted}, nil
	}
	errText := loaded.HaltError
	if errText == "" && runErr != nil {
		errText = runErr.Error()
	}
	cause := unresolvedCauseOf(loaded)
	step := lostStepOf(dataDir, loaded, cause, false)
	if loaded.State == run.StateQuarantined {
		return roundOutcome{outcome: request.RoundQuarantined, errText: errText, unresolved: cause, retryAt: step.RetryAt}, nil
	}
	return roundOutcome{outcome: request.RoundHalted, errText: errText, startFailure: len(loaded.Attempts) == 0, unresolved: cause, retryAt: step.RetryAt}, nil
}

// pushAcceptedRoundAndReply pushes an accepted corrective round's branch
// (never force -- see forge.pushExistingBranch's own doc comment) and replies
// on each thread the round addressed, naming the commit. The checks before
// the push, the push and its read-back are pushRoundResult's; a check that
// refuses the push, and a push or read-back that fails, halt the request
// with the reason, so a human reconciles the branch.
func pushAcceptedRoundAndReply(dp Deps, ctx context.Context, dataDir string, r *request.Request, ticket *request.Ticket, roundSliceIndex int, roundRunID, branch string, includedThreads []forge.Thread, now time.Time) error {
	roundRun, err := run.Load(dataDir, roundRunID)
	if err != nil {
		return fmt.Errorf("request %s: ticket %d: load accepted round run %q: %w", r.ID, ticket.Index, roundRunID, err)
	}
	if refused, failed := pushRoundResult(dp, ctx, dataDir, r, ticket, roundRun, branch, "review round"); refused != "" || failed != "" {
		return HaltRequest(dataDir, r, refused+failed, now)
	}
	afterRoundPushed(dp, ctx, r, ticket, roundSliceIndex, roundRun)

	replyCtx, cancelReply := context.WithTimeout(ctx, 30*time.Second)
	for _, t := range includedThreads {
		body := fmt.Sprintf("Addressed in %s.", roundRun.ResultSHA)
		if err := PostReviewReply(dp, replyCtx, ticket.PRURL, t.CommentID, body); err != nil {
			log.Printf("request %s: ticket %d: reply on thread %s: %v", r.ID, ticket.Index, t.ID, err)
			continue
		}
		ticket.SeenThreadIDs = append(ticket.SeenThreadIDs, forge.ThreadSeenKey(t))
	}
	cancelReply()
	return r.Save(dataDir)
}

// pushRoundResult pushes an accepted round's result (roundRun, a PR-review
// round's or an advice round's, named label in the reasons) to branch, the
// ticket's pull request branch, under every check a factory push to an open
// pull request takes, and reads the remote back. refused is the reason a
// check refused the push before anything was sent to the remote; failed is
// the reason the push, or its read-back, failed once it had been sent, when
// the commit may be on the remote. Both are "" for a push confirmed on the
// remote. It records nothing: what a refusal or a failure means is the
// caller's.
func pushRoundResult(dp Deps, ctx context.Context, dataDir string, r *request.Request, ticket *request.Ticket, roundRun *run.Run, branch, label string) (refused, failed string) {
	roundRunID := roundRun.ID
	// A corrective round is itself a full accepted run and gets its own
	// release.Decision recorded (recordReleaseDecision runs unconditionally
	// for every accepted run, not only when -open-pull-request is set --
	// see run_ticket.go's accepted branch); this push is exactly the
	// forge side effect that decision must gate, the same as an initial
	// build's own openEvidencePullRequest, but it had no such check at
	// all until this fix. An accepted correction that itself touches a
	// protected path or exceeds the release size limits must not land on
	// the PR branch just because its own build+verify succeeded (found
	// via review, GitHub Codex App, PR #154 round 4 -- this was
	// originally deferred as a documented residual in CLAIMS.md, but
	// deferring it left the "re-checked immediately before each side
	// effect" boundary this same PR documents in safety-contract.md
	// false for this one side effect).
	decision, decisionErr := release.LoadDecision(dataDir, release.ProjectOf(roundRun), roundRunID)
	switch {
	case decisionErr != nil:
		return fmt.Sprintf("ticket %d/%d: load release decision for accepted %s %s: %v", ticket.Index, r.TicketCount, label, roundRunID, decisionErr), ""
	case decision == nil:
		return fmt.Sprintf("ticket %d/%d: accepted %s %s has no recorded release decision; withholding push to %s -- a human must review before this can land on the PR branch", ticket.Index, r.TicketCount, label, roundRunID, branch), ""
	case decision.Invalidated:
		return fmt.Sprintf("ticket %d/%d: accepted %s %s's release decision was invalidated; withholding push to %s -- a human must review before this can land on the PR branch", ticket.Index, r.TicketCount, label, roundRunID, branch), ""
	case !decision.Allowed:
		return fmt.Sprintf("ticket %d/%d: accepted %s %s denied by release policy (%s); withholding push to %s -- a human must review before this can land on the PR branch", ticket.Index, r.TicketCount, label, roundRunID, strings.Join(decision.Reasons, "; "), branch), ""
	}

	// Refused before ever touching the remote, not just logged: found
	// live (Flutter + Go app run 3, 2026-09-28, PR #331) that the round which
	// produced roundRunID can land in a worktree that was never actually
	// isolated onto branch at all (the Temporal-path -on-branch bug --
	// see RunWorkflowInput.OnBranch's own doc comment). roundRun.Branch
	// is this round's own durable record of what it actually built on;
	// requiring it to equal the PR branch this call was told to push to
	// catches that class of bug even after the root cause above is fixed,
	// for any future path that reintroduces it.
	if roundRun.Branch != branch {
		return fmt.Sprintf("ticket %d/%d: accepted %s %s built on branch %q, not the PR branch %q -- refusing to push", ticket.Index, r.TicketCount, label, roundRunID, roundRun.Branch, branch), ""
	}

	// The round's result must actually descend from the PR branch's own
	// current remote head -- read fresh, right before push, from the
	// request's own shared workspace (r.Workspace: the round's isolated
	// worktree shares that same repository's object store, so any SHA
	// this check needs is already reachable there once it exists on
	// disk). A stale or wrong base (the same class of bug that motivated
	// roundRun.Branch's own check just above, or a human pushing to the
	// PR branch while this round was running) means pushing
	// roundRun.ResultSHA would silently discard whatever is only on the
	// remote's current head -- caught here instead of relying on the
	// push's own non-fast-forward rejection below, which a
	// sha:refs/heads/branch push can still pass in a case a plain
	// `git push origin branch` push would not (e.g. the remote head
	// moved to a commit that is itself an ancestor of roundRun.ResultSHA
	// through some other route).
	prHeadSHA, headErr := dp.RemoteBranchHeadSHA(ctx, r.Workspace, branch)
	if headErr != nil {
		return fmt.Sprintf("ticket %d/%d: accepted %s %s: read current head of PR branch %q before push: %v", ticket.Index, r.TicketCount, label, roundRunID, branch, headErr), ""
	}
	if isAncestor, ancErr := dp.RoundResultDescendsFromHead(r.Workspace, prHeadSHA, roundRun.ResultSHA); ancErr != nil {
		return fmt.Sprintf("ticket %d/%d: accepted %s %s: check %s is an ancestor of round result %s: %v", ticket.Index, r.TicketCount, label, roundRunID, prHeadSHA, roundRun.ResultSHA, ancErr), ""
	} else if !isAncestor {
		return fmt.Sprintf("ticket %d/%d: accepted %s %s: PR branch %q's current head %s is not an ancestor of round result %s -- refusing to push", ticket.Index, r.TicketCount, label, roundRunID, branch, prHeadSHA, roundRun.ResultSHA), ""
	}

	// No ready state crosses a head the factory changed: a ready pull
	// request goes back to draft before the push, and the ready flip runs
	// again on the new head (draftBeforeFactoryPush, as for a restack).
	if err := draftBeforeFactoryPush(dp, ctx, ticket); err != nil {
		return fmt.Sprintf("ticket %d/%d: turn pull request %s back to draft before pushing accepted %s %s: %v; nothing was pushed", ticket.Index, r.TicketCount, ticket.PRURL, label, roundRunID, err), ""
	}

	pushCtx, cancelPush := context.WithTimeout(ctx, 2*time.Minute)
	pushErr := dp.PushExistingBranch(pushCtx, roundRun.WorkspacePath, roundRun.ResultSHA, branch)
	cancelPush()
	if pushErr != nil {
		// A non-fast-forward (someone pushed to the PR branch) or a
		// persistent network failure: re-running the round next poll
		// would only pile up unpushed commits. Halt with the reason so a
		// human reconciles the branch, then `factoryd retry`. This
		// round's own Rounds entry stays Outcome: RoundAccepted with
		// Pushed left false (its zero value) -- exactly the state
		// Ticket.PRHeadRunID's own doc comment says a reader must not
		// mistake for "this round's commit is the PR's current head."
		return "", fmt.Sprintf("ticket %d/%d: accepted %s %s could not be pushed to %s: %v", ticket.Index, r.TicketCount, label, roundRunID, branch, pushErr)
	}
	// A push reporting success is not itself proof the remote branch now
	// points where this round intended (found live, Flutter + Go app run 3,
	// 2026-09-28, PR #331: a plain `git push origin <branch>` reported
	// "Everything up-to-date", exit 0, while pushing nothing at all).
	// Verified here by reading the remote back, not merely trusted.
	verifyCtx, cancelVerify := context.WithTimeout(ctx, 2*time.Minute)
	remoteHeadAfterPush, verifyErr := dp.RemoteBranchHeadSHA(verifyCtx, roundRun.WorkspacePath, branch)
	cancelVerify()
	if verifyErr != nil {
		return "", fmt.Sprintf("ticket %d/%d: accepted %s %s: verify push to %s: %v", ticket.Index, r.TicketCount, label, roundRunID, branch, verifyErr)
	}
	if remoteHeadAfterPush != roundRun.ResultSHA {
		return "", fmt.Sprintf("ticket %d/%d: accepted %s %s: pushed %s to %s but the remote branch now reads %s -- push did not land as expected", ticket.Index, r.TicketCount, label, roundRunID, roundRun.ResultSHA, branch, remoteHeadAfterPush)
	}
	return "", ""
}

// afterRoundPushed records what a confirmed push of roundRun, the round at
// ticket.Rounds[roundSliceIndex], leaves: the round is the pull request's
// head (Round.Pushed), the pull request is polled at once and turned back to
// draft if someone marked it ready meanwhile, and the round's worktree is
// removed.
func afterRoundPushed(dp Deps, ctx context.Context, r *request.Request, ticket *request.Ticket, roundSliceIndex int, roundRun *run.Run) {
	// Confirmed pushed -- see Round.Pushed's own doc comment for why
	// Outcome == RoundAccepted alone (already true on this entry, set
	// before this function was ever called) is not sufficient.
	ticket.Rounds[roundSliceIndex].Pushed = true
	// The next pass polls the pull request at once, so the ready flip on
	// the new head is not left a full poll interval behind.
	ticket.LastPolledAt = ""
	redraftAfterFactoryPush(dp, ctx, r, ticket)

	// The isolated worktree is no longer needed once its commit is pushed
	// -- removed here (worktree only; branch untouched, see
	// wsisolation.RemoveWorktreeOnly's own doc comment), rather than left
	// to accumulate one per accepted round for this ticket's PR.
	if err := workspace.RemoveWorktreeOnly(r.Workspace, roundRun.WorkspacePath); err != nil {
		log.Printf("request %s: ticket %d: remove round worktree: %v", r.ID, ticket.Index, err)
	}
}

// priorRoundRefusal returns ticket's most recent counted PR-review round's
// run when that round was quarantined, nil otherwise: a quarantined round is
// never pushed, but its commits stay on the local PR branch (PrepareOnBranch
// checks the branch itself out), so the next round builds on top of them and
// its builder is told why they were refused (withRefusedAttempt). Found live
// (2026-10-06, three rounds on one pull request): each round was given only
// the reviewer's comment, built on the refused commits, and was quarantined
// for the finding the round before it had already been quarantined for. It
// decides the record only.
func priorRoundRefusal(dataDir string, r *request.Request, ticket *request.Ticket) *run.Run {
	rnd, ok := lastCountedRound(ticket)
	if !ok || rnd.Outcome != request.RoundQuarantined {
		return nil
	}
	loaded, err := run.Load(dataDir, rnd.RunID)
	if err != nil {
		log.Printf("request %s: ticket %d: load quarantined round %d's run %s for its record: %v", r.ID, ticket.Index, rnd.Index, rnd.RunID, err)
		return nil
	}
	return loaded
}

// lastCountedRound is ticket's most recent PR-review round that ran (no
// Kind, counted).
func lastCountedRound(ticket *request.Ticket) (request.Round, bool) {
	for i := len(ticket.Rounds) - 1; i >= 0; i-- {
		if rnd := ticket.Rounds[i]; rnd.Kind == "" && rnd.Counted() {
			return rnd, true
		}
	}
	return request.Round{}, false
}

// maxReplyFindings bounds what a "not pushed" reply quotes from the
// review: it is a note to a reviewer on GitHub, not the evidence, which
// stays on the run.
const (
	maxReplyFindings = 5
	// MaxAddendumReviewThreads bounds the number of whole reviewer comments
	// carried into one build input; the shared renderer also applies the
	// remaining-byte cap.
	MaxAddendumReviewThreads = 20
)

// replyRoundNotPushed answers each thread a round was started for when the
// round did not end accepted: that it was attempted, that nothing was
// pushed, the gate's reason, and what happens to the thread next. Without
// it the reviewer saw nothing on the pull request at all: no commit, no
// reply, and after the last round a request that had quietly stopped (found
// live, 2026-10-06, three quarantined rounds on one pull request). A reply
// that cannot be posted is logged; the round's record is already saved.
// The reply does not make the thread the factory's own
// (forge.latestCommentNotBy), so it still blocks the ready flip and still
// starts the next round.
func replyRoundNotPushed(dp Deps, ctx context.Context, dataDir string, r *request.Request, ticket *request.Ticket, threads []forge.Thread, attempt roundAttempt, round, maxRounds int) {
	body := RoundNotPushedReply(dataDir, attempt.runID, attempt.outcome, round, maxRounds)
	replyCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	for _, t := range threads {
		if err := PostReviewReply(dp, replyCtx, ticket.PRURL, t.CommentID, body); err != nil {
			log.Printf("request %s: ticket %d: reply on thread %s that round %d pushed nothing: %v", r.ID, ticket.Index, t.ID, round, err)
		}
	}
}

// RoundNotPushedReply is the text replyRoundNotPushed posts. It is public
// on the pull request, so it carries only the round's number, the names of
// the gates that failed, and what the review flagged; never a halt error,
// which can hold local paths and command lines. The review's text comes
// from a model reading a worker-writable workspace, so each piece is
// flattened or capped and placed in code formatting, where GitHub renders
// no mention, link or markup.
func RoundNotPushedReply(dataDir, runID string, outcome request.RoundOutcome, round, maxRounds int) string {
	var b strings.Builder
	loaded, err := run.Load(dataDir, runID)
	if outcome == request.RoundQuarantined && err == nil {
		fmt.Fprintf(&b, "Attempted in corrective round %d of %d: a change was built and not pushed, because it did not pass %s.\n", round, maxRounds, failedGateNames(loaded))
	} else {
		fmt.Fprintf(&b, "Attempted in corrective round %d of %d: the build stopped before it was judged, so nothing was pushed.\n", round, maxRounds)
	}
	b.WriteString("\nThe pull request is unchanged. ")
	if round < maxRounds {
		fmt.Fprintf(&b, "While this thread is open, round %d starts on the next poll.", round+1)
	} else {
		b.WriteString("No rounds remain, so this comment now waits for a person.")
	}
	return b.String()
}

// failedGateNames lists runRecord's failed gates as inline code, in the
// order they ran.
func failedGateNames(runRecord *run.Run) string {
	var names []string
	for _, g := range runRecord.GateResults {
		if !g.Passed {
			names = append(names, "`"+replyInline(g.Check)+"`")
		}
	}
	if len(names) == 0 {
		return "every gate"
	}
	return strings.Join(names, ", ")
}

// replyInline makes s safe inside an inline code span: one bounded line
// (flattenAddendumLine) with no backtick to close the span.
func replyInline(s string) string {
	return strings.ReplaceAll(flattenAddendumLine(s, MaxAddendumLabelBytes), "`", "'")
}

func reviewThreadsSection(threads []forge.Thread) boundedAddendumSection {
	entries := make([]string, 0, len(threads))
	for _, t := range threads {
		path := flattenAddendumLine(t.Path, MaxAddendumLabelBytes)
		author := flattenAddendumLine(t.Author, MaxAddendumLabelBytes)
		body := capAddendumDetail(t.Body, MaxAddendumDetailBytes)
		entries = append(entries, fmt.Sprintf("- **%s:%d** (%s):\n\n%s\n", path, t.Line, author, ticketspec.FenceVerbatim(body)))
	}
	return boundedAddendumSection{
		heading: "\n## Reviewer comments to address", entries: entries,
		omittedLabel: "reviewer comments", maxEntries: MaxAddendumReviewThreads,
	}
}

func writeRoundAddendum(dataDir, requestID string, ticket *request.Ticket, threads []forge.Thread, roundIndex, fix int) (string, []forge.Thread, error) {
	// buildSpec, not the raw ticket spec: this round's own -spec must
	// carry the same acceptance-criteria text the ticket's first build
	// received, not just the "### Acceptance criteria covered" numbers --
	// see TicketBuildSpecContent's own doc comment (request_driver.go).
	buildSpec, err := TicketBuildSpecContent(dataDir, requestID, *ticket)
	if err != nil {
		return "", nil, fmt.Errorf("build spec for ticket %s: %w", ticket.SpecPath, err)
	}
	var b strings.Builder
	b.WriteString(buildSpec)
	if len(buildSpec) == 0 || buildSpec[len(buildSpec)-1] != '\n' {
		b.WriteString("\n")
	}
	// A spec that ends inside an unclosed fence would read the first
	// verbatim-fence opener below as that fence's close, leaving a comment
	// body as top-level lines.
	if closing := ticketspec.ClosingFenceIfOpen(buildSpec); closing != "" {
		b.WriteString(closing + "\n")
	}
	var sections []boundedAddendumSection
	if len(threads) > 0 {
		sections = append(sections, reviewThreadsSection(threads))
	}
	section, includedCounts := renderBoundedAddendumSectionsWithCounts(sections, MaxTicketBuildInputBytes-len([]byte(b.String())))
	if section != "" {
		b.WriteString(section)
	}
	includedThreads := threads
	if len(threads) > 0 && len(includedCounts) > 0 {
		includedThreads = threads[:includedCounts[0]]
	}

	name := fmt.Sprintf("%03d-%d", ticket.Index, roundIndex)
	if fix > 0 {
		name = fmt.Sprintf("%s-fix%d", name, fix)
	}
	dir := filepath.Join(request.Dir(dataDir, requestID), "rounds", name)
	path := filepath.Join(dir, "addendum.md")
	if err := writeTicketBuildSpecFile(path, b.String()); err != nil {
		return "", nil, fmt.Errorf("write %s: %w", path, err)
	}
	return path, includedThreads, nil
}

// notifyRoundOutcome sends one notification for a PR-review round that was
// not accepted. The request's state does not change (it stays in
// pr_review); the notification is appended to the request's log, recorded on
// r (LastNotifiedAt, LastAsk) and dispatched. The caller
// (RunCorrectiveRound) saves r and ticket.
func notifyRoundOutcome(dataDir string, r *request.Request, ticket *request.Ticket, roundIndex int, outcome request.RoundOutcome, errText string, now time.Time) {
	detail := fmt.Sprintf("Ticket %d/%d: corrective review round %d was %s; pull request %s needs attention", ticket.Index, r.TicketCount, roundIndex, outcome, ticket.PRURL)
	if errText != "" {
		detail += ": " + errText
	}
	n := prepareRequestNotification(dataDir, r, requestNotice{
		Ask:    fmt.Sprintf("Review round %s: pull request needs you", outcome),
		Detail: detail,
		Next:   fmt.Sprintf("review %s", ticket.PRURL),
	}, now)
	notify.DispatchExternal(dataDir, n)
}
