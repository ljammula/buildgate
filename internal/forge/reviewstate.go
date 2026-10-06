package forge

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ReviewDecision mirrors gh's own reviewDecision values (GitHub's
// PullRequestReviewDecision enum), normalized to this package's own
// lowercase constants; gh's "" (no review submitted yet) becomes
// ReviewDecisionNone.
type ReviewDecision string

const (
	ReviewDecisionApproved         ReviewDecision = "approved"
	ReviewDecisionChangesRequested ReviewDecision = "changes_requested"
	ReviewDecisionReviewRequired   ReviewDecision = "review_required"
	ReviewDecisionNone             ReviewDecision = "none"
)

// Thread is one unresolved PR review thread, described by its latest
// comment -- the comment whose author decides whether the thread is
// hard-excluded (see isHardExcludedAuthor) or actionable (see
// isActionableAuthor) and whose body is what a caller would act on. ID is
// GitHub's own review-thread node id (stable across polls), so a caller
// can persist it as a "last seen" marker (see NewUnresolvedThreads).
type Thread struct {
	ID        string
	Path      string
	Line      int
	Author    string
	Body      string
	CreatedAt time.Time
	// CommentID is the latest comment's REST-numeric id (GraphQL's own
	// databaseId for a PullRequestReviewComment) -- the PR-review poll's
	// reply mechanism needs this to target GitHub's REST "reply to a review
	// comment" endpoint (POST .../pulls/comments/{id}/replies), which
	// takes a comment id, not this Thread's own GraphQL thread node id
	// (ID, above). Zero when GraphQL never returned one (never expected
	// in practice, but a caller must not treat 0 as a valid id).
	CommentID int64
}

// ReviewState is a snapshot of one pull request's review status, as of
// one ReadReviewState call.
type ReviewState struct {
	IsDraft        bool
	State          string // gh's own PullRequestState: OPEN, CLOSED, MERGED
	MergedAt       *time.Time
	ReviewDecision ReviewDecision
	// ChecksPassing is nil when the PR has no configured status checks at
	// all -- an "unknown" state a caller must not treat as either passing
	// or failing -- and otherwise reports whether every configured check
	// succeeded.
	ChecksPassing *bool
	// HeadSHA is GitHub's own current head commit SHA for this PR
	// (headRefOid) -- a caller gating a side effect on a locally-recorded
	// run's own release.Decision must confirm that run's ResultSHA is
	// actually what the remote branch's real HEAD is right now before
	// acting on that decision: a maintainer pushing directly to the PR
	// branch, or a crash between a confirmed `git push` and this file's
	// own durable Round.Pushed/ticket save, can otherwise leave a locally
	// recorded decision describing a commit the PR has already moved past
	// (found via review, GitHub Codex App, PR #154 round 5).
	HeadSHA string
	// BaseRefName is GitHub's own current base branch name for this PR --
	// what `gh pr create --base`/`gh pr edit --base` last set it to, not
	// necessarily the repo default branch. A caller checks this against
	// another ticket's own recorded Branch to tell a still-stacked PR (its
	// base is a prior ticket's own branch, not merged yet) apart from an
	// ordinary one (see cmd/factoryd's advancePRReadyOrApproved and
	// handleClosedOrMergedTicket, the two callers this exists for).
	BaseRefName string
	// BlocksReadyThreads is every unresolved thread not hard-excluded
	// (isHardExcludedAuthor: a bot or the factory's own account) -- what a
	// caller must check before ever flipping a PR out of draft or treating
	// it as approved. A thread from an author who is not (yet) trusted
	// still belongs here: an untrusted human's still-open comment must
	// keep blocking the ready-flip even though it will never trigger a
	// corrective round (see ActionableThreads below) -- an operator who
	// has not listed a reviewer as trusted has not thereby told the
	// factory to ignore that reviewer's open comments altogether.
	BlocksReadyThreads []Thread
	// ActionableThreads is the subset of BlocksReadyThreads whose author is
	// actionable under the AuthorPolicy passed to ReadReviewState
	// (isActionableAuthor) -- the only threads a caller should ever turn
	// into a corrective build round. On a repo with any outside
	// visibility, treating every non-bot commenter as actionable would let
	// anyone who can comment on the PR steer the factory into making
	// commits -- ActionableThreads is this package's allow-list fix for
	// that.
	ActionableThreads []Thread
}

// ShouldStopPolling reports whether r's PR has left the state a poll loop
// tracks: OPEN is the only state still worth polling; CLOSED and MERGED
// both mean stop (MergedAt is set only in the MERGED case).
func (r ReviewState) ShouldStopPolling() bool {
	return r.State != "OPEN"
}

// DefaultIgnoreAuthors is the built-in default-ignore list for review-
// thread authors whose comments never need a human's (or the factory's
// own) attention unless an operator explicitly opts back in by listing
// the same login in AuthorPolicy.Trusted (rule 3 of isActionableAuthor's
// own doc comment). A login ending in "[bot]" -- GitHub's own convention
// for a GitHub App's associated user -- is hard-excluded regardless of
// whether it appears here (see isHardExcludedAuthor).
var DefaultIgnoreAuthors = []string{"github-actions", "codex", "copilot", "dependabot"}

// AuthorPolicy is the operator-configured part of deciding which review-
// thread authors the factory reacts to: Trusted is the allow-list
// (-pr-trusted-authors) whose comments trigger a corrective build round,
// and Ignore is the explicit deny-list (-pr-ignore-authors) that always
// wins over Trusted. Bot logins and the factory's own gh account are
// never part of this policy -- see isHardExcludedAuthor, which applies
// before AuthorPolicy is ever consulted.
type AuthorPolicy struct {
	Trusted []string
	Ignore  []string
}

// containsFold reports whether list contains s, matched case-
// insensitively -- GitHub logins are themselves case-insensitive, and a
// casing mismatch in an operator's -pr-trusted-authors/-pr-ignore-authors
// list must not silently produce "nothing is ever actionable" or "the
// ignore list did nothing".
func containsFold(list []string, s string) bool {
	for _, v := range list {
		if strings.EqualFold(v, s) {
			return true
		}
	}
	return false
}

// isHardExcludedAuthor reports whether login is excluded from every list
// this package ever returns, with no operator override possible: a login
// ending in "[bot]", or the factory's own resolved gh account
// (selfLogin; "" to skip that check). This is rule 1 of the precedence
// this package implements -- it always wins, before AuthorPolicy (rules
// 2-4, isActionableAuthor) is ever consulted, and it is the only rule
// that removes a thread from BlocksReadyThreads as well as
// ActionableThreads: the factory must never treat its own PR replies, or
// any bot's comments, as either a corrective-round trigger or a
// ready-flip blocker.
func isHardExcludedAuthor(login, selfLogin string) bool {
	if login == "" {
		return false
	}
	if strings.HasSuffix(login, "[bot]") {
		return true
	}
	return selfLogin != "" && strings.EqualFold(login, selfLogin)
}

// isActionableAuthor reports whether login's still-open review comments
// should trigger a corrective build round, applying AuthorPolicy's
// precedence (rules 2-4; rule 1, bot/self, is isHardExcludedAuthor's own
// job and must already have been checked by the caller):
//
//  2. policy.Ignore -- the operator's own explicit deny-list -- always
//     wins: a login named there is never actionable, even if it is also
//     in policy.Trusted.
//  3. DefaultIgnoreAuthors (github-actions/codex/copilot/dependabot) is
//     excluded unless the same login is also explicitly in
//     policy.Trusted -- an operator who wants the factory to act on, say,
//     Copilot's own review comments can opt back in for that one login
//     without changing this package's own defaults.
//  4. everything else is actionable only if it is explicitly in
//     policy.Trusted -- the allow-list this rule exists for: on a repo
//     with any outside visibility, an untrusted human (or app) commenting
//     on the PR must not be able to steer a corrective build round just
//     by being neither a bot nor on the ignore list.
func isActionableAuthor(login string, policy AuthorPolicy) bool {
	if containsFold(policy.Ignore, login) {
		return false
	}
	if containsFold(DefaultIgnoreAuthors, login) && !containsFold(policy.Trusted, login) {
		return false
	}
	return containsFold(policy.Trusted, login)
}

// NewUnresolvedThreads returns the entries of unresolved whose ID is not
// in seen -- the "only surface threads not already reported" filter a
// poll loop applies against the thread ids it persisted from its
// previous poll (the PR-review poll's own design calls this the
// "LastSeenThreadIDs filter").
func NewUnresolvedThreads(unresolved []Thread, seen []string) []Thread {
	seenSet := make(map[string]bool, len(seen))
	for _, id := range seen {
		seenSet[id] = true
	}
	var result []Thread
	for _, t := range unresolved {
		// A bare thread id in seen (records written before ThreadSeenKey
		// existed) still counts as seen; a keyed entry counts only while
		// the thread's latest comment is unchanged.
		if !seenSet[t.ID] && !seenSet[ThreadSeenKey(t)] {
			result = append(result, t)
		}
	}
	return result
}

// ThreadSeenKey identifies a thread AT its latest comment: the same thread
// with a newer reply yields a different key, so a follow-up comment on an
// already-addressed thread surfaces as new to NewUnresolvedThreads.
func ThreadSeenKey(t Thread) string {
	return t.ID + "#" + strconv.FormatInt(t.CommentID, 10)
}

// ghStatusCheck is one entry of gh pr view's own statusCheckRollup array:
// either a CheckRun (Conclusion set, State empty) or a StatusContext
// (State set, Conclusion empty) -- gh's own --json output mixes both
// shapes in the same array.
type ghStatusCheck struct {
	State      string `json:"state"`
	Conclusion string `json:"conclusion"`
}

// checkPassingStates are the CheckRun conclusions / StatusContext states
// that count as a successful check.
var checkPassingStates = map[string]bool{
	"SUCCESS": true, "NEUTRAL": true, "SKIPPED": true,
}

// checksPassingFromRollup reports whether every entry of rollup succeeded
// (true), whether any did not -- failed, pending, or still running (false)
// -- or nil when rollup is empty: a PR with no configured checks at all,
// which must never be reported as either passing or failing.
func checksPassingFromRollup(rollup []ghStatusCheck) *bool {
	if len(rollup) == 0 {
		return nil
	}
	passing := true
	for _, c := range rollup {
		status := c.Conclusion
		if status == "" {
			status = c.State
		}
		if !checkPassingStates[status] {
			passing = false
		}
	}
	return &passing
}

// ghPRView is the JSON shape of `gh pr view <url> --json
// isDraft,state,mergedAt,reviewDecision,statusCheckRollup,headRefOid,baseRefName`.
type ghPRView struct {
	IsDraft           bool            `json:"isDraft"`
	State             string          `json:"state"`
	MergedAt          *time.Time      `json:"mergedAt"`
	ReviewDecision    string          `json:"reviewDecision"`
	StatusCheckRollup []ghStatusCheck `json:"statusCheckRollup"`
	HeadRefOid        string          `json:"headRefOid"`
	BaseRefName       string          `json:"baseRefName"`
}

// normalizeReviewDecision maps gh's own reviewDecision strings to this
// package's ReviewDecision constants; anything gh didn't set (an empty
// string -- no review submitted yet) becomes ReviewDecisionNone.
func normalizeReviewDecision(raw string) ReviewDecision {
	switch raw {
	case "APPROVED":
		return ReviewDecisionApproved
	case "CHANGES_REQUESTED":
		return ReviewDecisionChangesRequested
	case "REVIEW_REQUIRED":
		return ReviewDecisionReviewRequired
	default:
		return ReviewDecisionNone
	}
}

// parsePRView parses `gh pr view --json
// isDraft,state,mergedAt,reviewDecision,statusCheckRollup,headRefOid,baseRefName`'s
// own stdout. A pure function, taking gh's JSON bytes directly, so it is
// testable against recorded fixtures without ever invoking gh.
func parsePRView(data []byte) (isDraft bool, state string, mergedAt *time.Time, decision ReviewDecision, checksPassing *bool, headSHA, baseRefName string, err error) {
	var v ghPRView
	if err := json.Unmarshal(data, &v); err != nil {
		return false, "", nil, "", nil, "", "", fmt.Errorf("parse gh pr view output: %w", err)
	}
	return v.IsDraft, v.State, v.MergedAt, normalizeReviewDecision(v.ReviewDecision), checksPassingFromRollup(v.StatusCheckRollup), v.HeadRefOid, v.BaseRefName, nil
}

// ghReviewThreadsResponse is the JSON shape of reviewThreadsQuery's own
// `gh api graphql` response.
type ghReviewThreadsResponse struct {
	Data struct {
		Repository struct {
			PullRequest struct {
				ReviewThreads struct {
					Nodes    []ghReviewThreadNode `json:"nodes"`
					PageInfo struct {
						HasNextPage bool   `json:"hasNextPage"`
						EndCursor   string `json:"endCursor"`
					} `json:"pageInfo"`
				} `json:"reviewThreads"`
			} `json:"pullRequest"`
		} `json:"repository"`
	} `json:"data"`
}

type ghReviewThreadNode struct {
	ID         string `json:"id"`
	IsResolved bool   `json:"isResolved"`
	Comments   struct {
		Nodes []ghReviewComment `json:"nodes"`
	} `json:"comments"`
}

type ghReviewComment struct {
	Author struct {
		Login string `json:"login"`
	} `json:"author"`
	Body       string    `json:"body"`
	Path       string    `json:"path"`
	Line       int       `json:"line"`
	CreatedAt  time.Time `json:"createdAt"`
	DatabaseID int64     `json:"databaseId"`
}

// parseReviewThreads parses reviewThreadsQuery's own `gh api graphql`
// response into the two lists ReviewState carries: blocksReady is every
// unresolved thread not hard-excluded (isHardExcludedAuthor: a bot or the
// factory's own account, selfLogin; "" to skip that check), and
// actionable is the subset of those whose author is actionable under
// policy (isActionableAuthor). A pure function, taking the JSON bytes
// directly, so it is testable against recorded fixtures without ever
// invoking gh.
func parseReviewThreads(data []byte, policy AuthorPolicy, selfLogin string) (blocksReady, actionable []Thread, err error) {
	var resp ghReviewThreadsResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, nil, fmt.Errorf("parse gh api graphql reviewThreads output: %w", err)
	}
	for _, node := range resp.Data.Repository.PullRequest.ReviewThreads.Nodes {
		if node.IsResolved {
			continue
		}
		comments := node.Comments.Nodes
		if len(comments) == 0 {
			continue
		}
		latest := comments[len(comments)-1]
		login := latest.Author.Login
		if isHardExcludedAuthor(login, selfLogin) {
			continue
		}
		t := Thread{
			ID:        node.ID,
			Path:      latest.Path,
			Line:      latest.Line,
			Author:    login,
			Body:      latest.Body,
			CreatedAt: latest.CreatedAt,
			CommentID: latest.DatabaseID,
		}
		blocksReady = append(blocksReady, t)
		if isActionableAuthor(login, policy) {
			actionable = append(actionable, t)
		}
	}
	return blocksReady, actionable, nil
}

// reviewThreadsPageInfo extracts one reviewThreadsQuery response page's
// own pageInfo -- whether a caller must fetch another page (hasNextPage)
// and, if so, the cursor to fetch it with (endCursor). A fixture that
// predates pagination (no pageInfo field at all) decodes to
// hasNextPage=false, the same "nothing more to fetch" a real single-page
// PR reports.
func reviewThreadsPageInfo(data []byte) (hasNextPage bool, endCursor string, err error) {
	var resp ghReviewThreadsResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return false, "", fmt.Errorf("parse gh api graphql reviewThreads output: %w", err)
	}
	pageInfo := resp.Data.Repository.PullRequest.ReviewThreads.PageInfo
	return pageInfo.HasNextPage, pageInfo.EndCursor, nil
}

// pullRequestURLPattern extracts owner/repo/number from a GitHub PR URL,
// the shape run.Run.PRURL (opened by OpenDraftPullRequest, above) always
// has.
var pullRequestURLPattern = regexp.MustCompile(`^https://github\.com/([^/]+)/([^/]+)/pull/(\d+)`)

func parsePullRequestURL(prURL string) (owner, repo string, number int, err error) {
	m := pullRequestURLPattern.FindStringSubmatch(prURL)
	if m == nil {
		return "", "", 0, fmt.Errorf("not a GitHub pull request URL: %q", prURL)
	}
	n, convErr := strconv.Atoi(m[3])
	if convErr != nil {
		return "", "", 0, fmt.Errorf("pull request number in %q: %w", prURL, convErr)
	}
	return m[1], m[2], n, nil
}

// reviewThreadsQuery fetches every review thread's resolution state and
// latest comment -- gh pr view --json has no field for isResolved, only
// GraphQL exposes it, so this is the only way to tell an addressed thread
// from an outstanding one. $cursor is optional (omitted on the first
// page's request) so a PR with more than 100 threads is fetched a page at
// a time -- see ReadReviewState's own pagination loop.
const reviewThreadsQuery = `query($owner: String!, $repo: String!, $number: Int!, $cursor: String) {
  repository(owner: $owner, name: $repo) {
    pullRequest(number: $number) {
      reviewThreads(first: 100, after: $cursor) {
        pageInfo {
          hasNextPage
          endCursor
        }
        nodes {
          id
          isResolved
          comments(last: 1) {
            nodes {
              author { login }
              body
              path
              line
              createdAt
              databaseId
            }
          }
        }
      }
    }
  }
}`

// selfLogin caches `gh api user --jq .login`'s result process-wide, but
// only once it has actually succeeded: a transient failure (network, a
// rate limit, gh not yet authenticated) is retried on every subsequent
// call rather than cached forever, since a permanently cached error would
// leave every later ReadReviewState call unable to recognize the
// factory's own PR replies as its own (see resolveSelfLogin's own doc
// comment on ReadReviewState below for why that failure mode matters).
var (
	selfLoginMu    sync.Mutex
	selfLoginValue string
	selfLoginKnown bool
)

func resolveSelfLogin(ctx context.Context, ghBinary string) (string, error) {
	selfLoginMu.Lock()
	if selfLoginKnown {
		value := selfLoginValue
		selfLoginMu.Unlock()
		return value, nil
	}
	selfLoginMu.Unlock()

	cmd := exec.CommandContext(ctx, ghBinary, "api", "user", "--jq", ".login")
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("gh api user: %w", err)
	}
	login := strings.TrimSpace(string(out))

	selfLoginMu.Lock()
	selfLoginValue = login
	selfLoginKnown = true
	selfLoginMu.Unlock()
	return login, nil
}

// prViewJSONFields is the exact --json field list ReadReviewState asks
// `gh pr view` for -- named once so what's requested and what parsePRView
// actually reads can't drift apart silently.
const prViewJSONFields = "isDraft,state,mergedAt,reviewDecision,statusCheckRollup,headRefOid,baseRefName"

// ReadReviewState shells out to gh to build a snapshot of prURL's current
// review state: `gh pr view` for the PR-level fields, `gh api graphql`
// for unresolved review threads (see reviewThreadsQuery's own doc
// comment for why threads need GraphQL). policy decides which of those
// threads land in ReviewState.ActionableThreads (see isActionableAuthor);
// the factory's own gh account (resolveSelfLogin) is always excluded from
// both BlocksReadyThreads and ActionableThreads regardless of policy.
//
// A failure to resolve the factory's own gh login (e.g. gh not
// authenticated as any account at all, or a transient API error) fails
// the whole call: a caller that proceeded without self-exclusion would
// treat the factory's own PR replies as reviewer comments and launch
// corrective rounds against itself. cmd/factoryd's own pollTicketPR
// already logs a ReadReviewState error and retries on the next poll, so
// failing closed here is safe -- and is the only safe choice, since
// resolveSelfLogin no longer caches a failure (a cached failure would
// otherwise strand every later call in this same failure mode forever).
// A failure to read the PR itself (gh pr view or the graphql call) fails
// the call the same way: there is no partial result worth returning.
func (o GHPullRequestOpener) ReadReviewState(ctx context.Context, prURL string, policy AuthorPolicy) (ReviewState, error) {
	// Parsed before any gh invocation, not after the first one: a
	// malformed prURL must fail immediately rather than shell out to gh
	// (a real, installed gh included -- see the test for this) only to
	// fail parsing its own reply.
	owner, repo, number, err := parsePullRequestURL(prURL)
	if err != nil {
		return ReviewState{}, err
	}

	ghBinary := o.GHBinary
	if ghBinary == "" {
		ghBinary = "gh"
	}

	view := exec.CommandContext(ctx, ghBinary, "pr", "view", prURL, "--json", prViewJSONFields)
	viewOut, err := view.CombinedOutput()
	if err != nil {
		return ReviewState{}, fmt.Errorf("gh pr view %s: %w: %s", prURL, err, strings.TrimSpace(string(viewOut)))
	}
	isDraft, state, mergedAt, decision, checksPassing, headSHA, baseRefName, err := parsePRView(viewOut)
	if err != nil {
		return ReviewState{}, err
	}

	selfLogin, err := resolveSelfLogin(ctx, ghBinary)
	if err != nil {
		return ReviewState{}, fmt.Errorf("resolve factory's own gh login: %w", err)
	}

	// reviewThreads is paginated (GraphQL's first:100 caps one page):
	// looped here, one gh api graphql call per page, until GitHub reports
	// no more -- a PR with over 100 review threads would otherwise have
	// threads beyond the first page silently invisible to this call.
	var blocksReady, actionable []Thread
	cursor := ""
	for {
		args := []string{"api", "graphql",
			"-f", "query=" + reviewThreadsQuery,
			"-F", "owner=" + owner,
			"-F", "repo=" + repo,
			"-F", "number=" + strconv.Itoa(number),
		}
		if cursor != "" {
			args = append(args, "-F", "cursor="+cursor)
		}
		graphql := exec.CommandContext(ctx, ghBinary, args...)
		graphqlOut, err := graphql.CombinedOutput()
		if err != nil {
			return ReviewState{}, fmt.Errorf("gh api graphql reviewThreads for %s: %w: %s", prURL, err, strings.TrimSpace(string(graphqlOut)))
		}

		pageBlocksReady, pageActionable, err := parseReviewThreads(graphqlOut, policy, selfLogin)
		if err != nil {
			return ReviewState{}, err
		}
		blocksReady = append(blocksReady, pageBlocksReady...)
		actionable = append(actionable, pageActionable...)

		hasNextPage, endCursor, err := reviewThreadsPageInfo(graphqlOut)
		if err != nil {
			return ReviewState{}, err
		}
		if !hasNextPage {
			break
		}
		cursor = endCursor
	}

	return ReviewState{
		IsDraft:            isDraft,
		State:              state,
		MergedAt:           mergedAt,
		ReviewDecision:     decision,
		ChecksPassing:      checksPassing,
		HeadSHA:            headSHA,
		BaseRefName:        baseRefName,
		BlocksReadyThreads: blocksReady,
		ActionableThreads:  actionable,
	}, nil
}
