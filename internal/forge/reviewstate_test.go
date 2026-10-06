package forge

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func readTestdata(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read testdata/%s: %v", name, err)
	}
	return data
}

// resetSelfLoginCache undoes resolveSelfLogin's own process-wide caching
// between tests -- otherwise the first test to resolve a self login would
// leak its cached value (or error) into every later test in this package,
// since selfLoginOnce is a package-level var.
func resetSelfLoginCache(t *testing.T) {
	t.Helper()
	selfLoginMu.Lock()
	selfLoginValue = ""
	selfLoginKnown = false
	selfLoginMu.Unlock()
	t.Cleanup(func() {
		selfLoginMu.Lock()
		selfLoginValue = ""
		selfLoginKnown = false
		selfLoginMu.Unlock()
	})
}

func TestParsePRView(t *testing.T) {
	cases := []struct {
		file              string
		wantIsDraft       bool
		wantState         string
		wantMergedAt      bool // whether mergedAt should be non-nil
		wantDecision      ReviewDecision
		wantChecksPassing *bool
	}{
		{"prview_no_reviews.json", false, "OPEN", false, ReviewDecisionNone, boolPtr(true)},
		{"prview_approved.json", false, "OPEN", false, ReviewDecisionApproved, boolPtr(true)},
		{"prview_merged.json", false, "MERGED", true, ReviewDecisionApproved, boolPtr(true)},
		{"prview_changes_requested.json", false, "OPEN", false, ReviewDecisionChangesRequested, boolPtr(true)},
		{"prview_checks_failing.json", false, "OPEN", false, ReviewDecisionReviewRequired, boolPtr(false)},
		{"prview_no_checks.json", true, "OPEN", false, ReviewDecisionNone, nil},
		{"prview_closed.json", false, "CLOSED", false, ReviewDecisionNone, boolPtr(true)},
	}
	for _, c := range cases {
		t.Run(c.file, func(t *testing.T) {
			isDraft, state, mergedAt, decision, checksPassing, _, _, err := parsePRView(readTestdata(t, c.file))
			if err != nil {
				t.Fatalf("parsePRView: %v", err)
			}
			if isDraft != c.wantIsDraft {
				t.Errorf("isDraft = %v, want %v", isDraft, c.wantIsDraft)
			}
			if state != c.wantState {
				t.Errorf("state = %q, want %q", state, c.wantState)
			}
			if (mergedAt != nil) != c.wantMergedAt {
				t.Errorf("mergedAt = %v, want non-nil: %v", mergedAt, c.wantMergedAt)
			}
			if decision != c.wantDecision {
				t.Errorf("reviewDecision = %q, want %q", decision, c.wantDecision)
			}
			if (checksPassing == nil) != (c.wantChecksPassing == nil) {
				t.Fatalf("checksPassing = %v, want nil: %v", checksPassing, c.wantChecksPassing == nil)
			}
			if checksPassing != nil && *checksPassing != *c.wantChecksPassing {
				t.Errorf("checksPassing = %v, want %v", *checksPassing, *c.wantChecksPassing)
			}
		})
	}
}

// TestParsePRViewParsesBaseRefName proves baseRefName -- what a caller
// uses to tell a still-stacked PR (base is a prior ticket's own branch)
// apart from an ordinary one -- actually comes back from parsePRView,
// not just headRefOid.
func TestParsePRViewParsesBaseRefName(t *testing.T) {
	_, _, _, _, _, headSHA, baseRefName, err := parsePRView(readTestdata(t, "prview_stacked_base.json"))
	if err != nil {
		t.Fatalf("parsePRView: %v", err)
	}
	if headSHA != "cafef00d" {
		t.Errorf("headSHA = %q, want %q", headSHA, "cafef00d")
	}
	if baseRefName != "factoryd/run-1" {
		t.Errorf("baseRefName = %q, want %q", baseRefName, "factoryd/run-1")
	}
}

func boolPtr(b bool) *bool { return &b }

func TestParseReviewThreadsNoReviews(t *testing.T) {
	blocksReady, actionable, err := parseReviewThreads(readTestdata(t, "reviewthreads_none.json"), AuthorPolicy{}, "")
	if err != nil {
		t.Fatalf("parseReviewThreads: %v", err)
	}
	if len(blocksReady) != 0 || len(actionable) != 0 {
		t.Fatalf("blocksReady = %+v, actionable = %+v, want none", blocksReady, actionable)
	}
}

// TestParseReviewThreadsOneUnresolved: under the default (empty) policy,
// an author with no special status (not trusted, not ignored) surfaces in
// BlocksReadyThreads -- it must still block the ready-flip -- but not in
// ActionableThreads, since nothing has opted this login in as trusted.
func TestParseReviewThreadsOneUnresolved(t *testing.T) {
	blocksReady, actionable, err := parseReviewThreads(readTestdata(t, "reviewthreads_one_unresolved.json"), AuthorPolicy{}, "")
	if err != nil {
		t.Fatalf("parseReviewThreads: %v", err)
	}
	if len(blocksReady) != 1 {
		t.Fatalf("blocksReady = %+v, want exactly one", blocksReady)
	}
	got := blocksReady[0]
	want := Thread{
		ID:        "PRT_kwDOAbc123",
		Path:      "internal/forge/reviewstate.go",
		Line:      42,
		Author:    "alice",
		Body:      "please handle this edge case",
		CreatedAt: time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC),
	}
	if got != want {
		t.Errorf("thread = %+v, want %+v", got, want)
	}
	if len(actionable) != 0 {
		t.Fatalf("actionable = %+v, want none: alice is not in policy.Trusted", actionable)
	}
}

// TestParseReviewThreadsUntrustedAuthorBlocksReadyButIsNotActionable is
// the core new-behavior test for the allow-list fix: a non-bot,
// non-ignored, non-trusted author's still-open comment must still block
// the ready-flip (BlocksReadyThreads) but must never trigger a
// corrective round (ActionableThreads).
func TestParseReviewThreadsUntrustedAuthorBlocksReadyButIsNotActionable(t *testing.T) {
	blocksReady, actionable, err := parseReviewThreads(readTestdata(t, "reviewthreads_one_unresolved.json"), AuthorPolicy{Trusted: []string{"someone-else"}}, "")
	if err != nil {
		t.Fatalf("parseReviewThreads: %v", err)
	}
	if len(blocksReady) != 1 || blocksReady[0].Author != "alice" {
		t.Fatalf("blocksReady = %+v, want alice's thread still present", blocksReady)
	}
	if len(actionable) != 0 {
		t.Fatalf("actionable = %+v, want none: alice is not in policy.Trusted", actionable)
	}
}

// TestParseReviewThreadsTrustedAuthorIsActionable proves the allow-list's
// positive case: a login explicitly in policy.Trusted is actionable.
func TestParseReviewThreadsTrustedAuthorIsActionable(t *testing.T) {
	blocksReady, actionable, err := parseReviewThreads(readTestdata(t, "reviewthreads_one_unresolved.json"), AuthorPolicy{Trusted: []string{"alice"}}, "")
	if err != nil {
		t.Fatalf("parseReviewThreads: %v", err)
	}
	if len(blocksReady) != 1 || len(actionable) != 1 || actionable[0].Author != "alice" {
		t.Fatalf("blocksReady = %+v, actionable = %+v, want alice actionable", blocksReady, actionable)
	}
}

func TestParseReviewThreadsExcludesResolved(t *testing.T) {
	blocksReady, actionable, err := parseReviewThreads(readTestdata(t, "reviewthreads_resolved.json"), AuthorPolicy{}, "")
	if err != nil {
		t.Fatalf("parseReviewThreads: %v", err)
	}
	if len(blocksReady) != 0 || len(actionable) != 0 {
		t.Fatalf("blocksReady = %+v, actionable = %+v, want a resolved thread excluded from both", blocksReady, actionable)
	}
}

// TestParseReviewThreadsDefaultIgnoreAuthorStillBlocksReady proves rule 3:
// "github-actions" (in DefaultIgnoreAuthors, but not a "[bot]"-suffixed
// login and not the factory's own account) is excluded from
// ActionableThreads by default but still appears in BlocksReadyThreads --
// only a "[bot]"-suffixed login or the factory's own account is hard-
// excluded from both (see TestIsHardExcludedAuthorMatchesBotSuffixRegardlessOfPolicy).
func TestParseReviewThreadsDefaultIgnoreAuthorStillBlocksReady(t *testing.T) {
	blocksReady, actionable, err := parseReviewThreads(readTestdata(t, "reviewthreads_bot_authored.json"), AuthorPolicy{}, "")
	if err != nil {
		t.Fatalf("parseReviewThreads: %v", err)
	}
	if len(blocksReady) != 1 || blocksReady[0].Author != "github-actions" {
		t.Fatalf("blocksReady = %+v, want github-actions' thread still present", blocksReady)
	}
	if len(actionable) != 0 {
		t.Fatalf("actionable = %+v, want none: github-actions is in DefaultIgnoreAuthors and not trusted", actionable)
	}
}

// TestParseReviewThreadsIgnoredHumanAuthorStillBlocksReady proves an
// author on policy.Ignore is excluded from ActionableThreads but still
// appears in BlocksReadyThreads -- an operator's explicit ignore-list
// entry means "don't act on this", not "this comment doesn't exist".
func TestParseReviewThreadsIgnoredHumanAuthorStillBlocksReady(t *testing.T) {
	blocksReady, actionable, err := parseReviewThreads(readTestdata(t, "reviewthreads_one_unresolved.json"), AuthorPolicy{Trusted: []string{"alice"}, Ignore: []string{"alice"}}, "")
	if err != nil {
		t.Fatalf("parseReviewThreads: %v", err)
	}
	if len(blocksReady) != 1 || blocksReady[0].Author != "alice" {
		t.Fatalf("blocksReady = %+v, want alice's thread still present", blocksReady)
	}
	if len(actionable) != 0 {
		t.Fatalf("actionable = %+v, want none: policy.Ignore wins over policy.Trusted", actionable)
	}
}

// TestIsActionableAuthorDefaultIgnoreOverriddenByTrusted covers rule 3's
// override: a login in DefaultIgnoreAuthors (e.g. "copilot") that is also
// explicitly in policy.Trusted is actionable.
func TestIsActionableAuthorDefaultIgnoreOverriddenByTrusted(t *testing.T) {
	if !isActionableAuthor("copilot", AuthorPolicy{Trusted: []string{"copilot"}}) {
		t.Error("copilot should be actionable once explicitly trusted, overriding DefaultIgnoreAuthors")
	}
	if isActionableAuthor("copilot", AuthorPolicy{}) {
		t.Error("copilot should not be actionable by default (DefaultIgnoreAuthors, not trusted)")
	}
}

// TestIsActionableAuthorIgnoreWinsOverTrusted covers rule 2's precedence:
// a login in both policy.Ignore and policy.Trusted is excluded -- the
// operator's explicit deny wins.
func TestIsActionableAuthorIgnoreWinsOverTrusted(t *testing.T) {
	if isActionableAuthor("alice", AuthorPolicy{Trusted: []string{"alice"}, Ignore: []string{"alice"}}) {
		t.Error("alice should not be actionable: policy.Ignore must win over policy.Trusted")
	}
}

// TestIsActionableAuthorCaseInsensitive proves logins are matched
// case-insensitively -- GitHub logins are themselves case-insensitive.
func TestIsActionableAuthorCaseInsensitive(t *testing.T) {
	if !isActionableAuthor("alice", AuthorPolicy{Trusted: []string{"Alice"}}) {
		t.Error("alice should be actionable: policy.Trusted has a case-differing entry")
	}
	if !isActionableAuthor("ALICE", AuthorPolicy{Trusted: []string{"alice"}}) {
		t.Error("ALICE should be actionable: policy.Trusted has a case-differing entry")
	}
}

func TestIsHardExcludedAuthorMatchesBotSuffixRegardlessOfPolicy(t *testing.T) {
	if !isHardExcludedAuthor("some-app[bot]", "") {
		t.Error("a [bot]-suffixed login should be hard-excluded even with no selfLogin")
	}
	if isHardExcludedAuthor("alice", "") {
		t.Error("a plain human login should not be hard-excluded")
	}
	if !isHardExcludedAuthor("factory-bot", "factory-bot") {
		t.Error("the factory's own selfLogin should be hard-excluded")
	}
	if !isHardExcludedAuthor("Factory-Bot", "factory-bot") {
		t.Error("selfLogin should match case-insensitively")
	}
}

func TestNewUnresolvedThreadsFiltersSeenIDs(t *testing.T) {
	threads := []Thread{{ID: "a"}, {ID: "b"}, {ID: "c"}}
	got := NewUnresolvedThreads(threads, []string{"a", "c"})
	if len(got) != 1 || got[0].ID != "b" {
		t.Fatalf("NewUnresolvedThreads = %+v, want only thread b", got)
	}
}

func TestNewUnresolvedThreadsNoneSeenReturnsAll(t *testing.T) {
	threads := []Thread{{ID: "a"}, {ID: "b"}}
	got := NewUnresolvedThreads(threads, nil)
	if len(got) != 2 {
		t.Fatalf("NewUnresolvedThreads = %+v, want both threads unfiltered", got)
	}
}

func TestShouldStopPolling(t *testing.T) {
	cases := []struct {
		state string
		want  bool
	}{
		{"OPEN", false},
		{"CLOSED", true},
		{"MERGED", true},
	}
	for _, c := range cases {
		got := ReviewState{State: c.state}.ShouldStopPolling()
		if got != c.want {
			t.Errorf("ReviewState{State: %q}.ShouldStopPolling() = %v, want %v", c.state, got, c.want)
		}
	}
}

// fakeGHDispatcher writes a fake `gh` executable at dir/fake-gh that
// dispatches on its own argv shape (pr view / api graphql / api user)
// exactly the three calls ReadReviewState makes, each printing the given
// fixture verbatim -- the same fake-binary-injection pattern forge_test.go
// already establishes, extended with a dispatch since ReadReviewState
// shells out to gh three times per call instead of OpenDraftPullRequest's
// two.
func fakeGHDispatcher(t *testing.T, dir string, prViewOut, graphqlOut, userOut string) string {
	t.Helper()
	script := `#!/bin/sh
case "$1 $2" in
  "pr view")
    cat <<'EOF'
` + prViewOut + `
EOF
    ;;
  "api graphql")
    cat <<'EOF'
` + graphqlOut + `
EOF
    ;;
  "api user")
    printf '%s' "` + userOut + `"
    ;;
  *)
    echo "fake-gh: unexpected invocation: $@" >&2
    exit 1
    ;;
esac
`
	path := filepath.Join(dir, "fake-gh")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake-gh: %v", err)
	}
	return path
}

func TestReadReviewStateCombinesPRViewAndReviewThreads(t *testing.T) {
	resetSelfLoginCache(t)
	dir := t.TempDir()
	ghBinary := fakeGHDispatcher(t, dir,
		string(readTestdata(t, "prview_approved.json")),
		string(readTestdata(t, "reviewthreads_one_unresolved.json")),
		"factory-bot",
	)

	opener := GHPullRequestOpener{GHBinary: ghBinary}
	state, err := opener.ReadReviewState(context.Background(), "https://github.com/acme/widgets/pull/42", AuthorPolicy{})
	if err != nil {
		t.Fatalf("ReadReviewState: %v", err)
	}
	if state.State != "OPEN" || state.ReviewDecision != ReviewDecisionApproved {
		t.Errorf("state = %+v, want OPEN/approved", state)
	}
	if len(state.BlocksReadyThreads) != 1 || state.BlocksReadyThreads[0].Author != "alice" {
		t.Errorf("BlocksReadyThreads = %+v, want alice's thread", state.BlocksReadyThreads)
	}
}

// TestReadReviewStateExcludesFactorysOwnAccount proves the factory's own
// gh login (from `gh api user`) is hard-excluded from both
// BlocksReadyThreads and ActionableThreads, even though it was never
// passed as an explicit policy.Ignore entry.
func TestReadReviewStateExcludesFactorysOwnAccount(t *testing.T) {
	resetSelfLoginCache(t)
	dir := t.TempDir()
	selfAuthoredThreads := `{
  "data": {
    "repository": {
      "pullRequest": {
        "reviewThreads": {
          "nodes": [
            {
              "id": "PRT_self",
              "isResolved": false,
              "comments": {
                "nodes": [
                  {"author": {"login": "factory-bot"}, "body": "self comment", "path": "x.go", "line": 1, "createdAt": "2026-09-10T12:00:00Z"}
                ]
              }
            }
          ]
        }
      }
    }
  }
}`
	ghBinary := fakeGHDispatcher(t, dir,
		string(readTestdata(t, "prview_no_reviews.json")),
		selfAuthoredThreads,
		"factory-bot",
	)

	opener := GHPullRequestOpener{GHBinary: ghBinary}
	state, err := opener.ReadReviewState(context.Background(), "https://github.com/acme/widgets/pull/42", AuthorPolicy{})
	if err != nil {
		t.Fatalf("ReadReviewState: %v", err)
	}
	if len(state.BlocksReadyThreads) != 0 || len(state.ActionableThreads) != 0 {
		t.Errorf("BlocksReadyThreads = %+v, ActionableThreads = %+v, want the factory's own thread excluded from both", state.BlocksReadyThreads, state.ActionableThreads)
	}
}

func TestReadReviewStateFailsClosedWhenGHPRViewFails(t *testing.T) {
	dir := t.TempDir()
	ghBinary := filepath.Join(dir, "fake-gh")
	if err := os.WriteFile(ghBinary, []byte("#!/bin/sh\necho 'gh: pull request not found' >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	opener := GHPullRequestOpener{GHBinary: ghBinary}
	_, err := opener.ReadReviewState(context.Background(), "https://github.com/acme/widgets/pull/42", AuthorPolicy{})
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("ReadReviewState = %v, want an error naming gh's own real failure", err)
	}
}

func TestReadReviewStateRejectsNonPRURL(t *testing.T) {
	opener := GHPullRequestOpener{}
	_, err := opener.ReadReviewState(context.Background(), "not-a-url", AuthorPolicy{})
	if err == nil {
		t.Fatal("ReadReviewState(non-PR URL) succeeded, want an error")
	}
}

// TestParseReviewThreadsCapturesCommentID proves Thread.CommentID is
// populated from GraphQL's own databaseId field -- the PR-review
// poll's reply mechanism needs this REST-numeric id (not this Thread's
// own GraphQL thread node id) to target GitHub's "reply to a review
// comment" REST endpoint.
func TestParseReviewThreadsCapturesCommentID(t *testing.T) {
	blocksReady, _, err := parseReviewThreads(readTestdata(t, "reviewthreads_with_comment_id.json"), AuthorPolicy{}, "")
	if err != nil {
		t.Fatalf("parseReviewThreads: %v", err)
	}
	if len(blocksReady) != 1 {
		t.Fatalf("blocksReady = %+v, want exactly one", blocksReady)
	}
	if blocksReady[0].CommentID != 123456789 {
		t.Errorf("CommentID = %d, want 123456789", blocksReady[0].CommentID)
	}
}

// TestParseReviewThreadsZeroCommentIDWhenAbsent proves a fixture with no
// databaseId field (every existing fixture predating this field) decodes
// to CommentID 0, not an error -- additive JSON fields must never break an
// existing caller.
func TestParseReviewThreadsZeroCommentIDWhenAbsent(t *testing.T) {
	blocksReady, _, err := parseReviewThreads(readTestdata(t, "reviewthreads_one_unresolved.json"), AuthorPolicy{}, "")
	if err != nil {
		t.Fatalf("parseReviewThreads: %v", err)
	}
	if len(blocksReady) != 1 {
		t.Fatalf("blocksReady = %+v, want exactly one", blocksReady)
	}
	if blocksReady[0].CommentID != 0 {
		t.Errorf("CommentID = %d, want 0 (no databaseId in this fixture)", blocksReady[0].CommentID)
	}
}

func TestNewUnresolvedThreadsTreatsANewerCommentOnASeenThreadAsNew(t *testing.T) {
	seen := []string{ThreadSeenKey(Thread{ID: "t1", CommentID: 5}), "t2"}
	threads := []Thread{{ID: "t1", CommentID: 5}, {ID: "t1", CommentID: 9}, {ID: "t2", CommentID: 1}}
	got := NewUnresolvedThreads(threads, seen)
	if len(got) != 1 || got[0].CommentID != 9 {
		t.Fatalf("got %+v, want only t1 at comment 9", got)
	}
}

// countingFailThenSucceedGH writes a fake `gh` whose "api user" call fails
// on its first invocation and succeeds (printing login) on every
// invocation after that -- proving resolveSelfLogin retries instead of
// caching the transient failure forever.
func countingFailThenSucceedGH(t *testing.T, dir, login string) string {
	t.Helper()
	counter := filepath.Join(dir, "call-count")
	script := `#!/bin/sh
n=0
if [ -f "` + counter + `" ]; then n=$(cat "` + counter + `"); fi
n=$((n + 1))
echo "$n" > "` + counter + `"
if [ "$n" -eq 1 ]; then
  echo "gh: transient failure" >&2
  exit 1
fi
printf '%s' "` + login + `"
`
	path := filepath.Join(dir, "fake-gh")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake-gh: %v", err)
	}
	return path
}

func TestResolveSelfLoginRetriesAfterTransientFailureThenCaches(t *testing.T) {
	resetSelfLoginCache(t)
	dir := t.TempDir()
	ghBinary := countingFailThenSucceedGH(t, dir, "factory-bot")

	if _, err := resolveSelfLogin(context.Background(), ghBinary); err == nil {
		t.Fatal("resolveSelfLogin (1st call) = nil error, want the transient failure surfaced")
	}
	login, err := resolveSelfLogin(context.Background(), ghBinary)
	if err != nil {
		t.Fatalf("resolveSelfLogin (2nd call): %v", err)
	}
	if login != "factory-bot" {
		t.Errorf("login = %q, want %q", login, "factory-bot")
	}

	countBefore, err := os.ReadFile(filepath.Join(dir, "call-count"))
	if err != nil {
		t.Fatalf("read call-count: %v", err)
	}
	if login, err = resolveSelfLogin(context.Background(), ghBinary); err != nil || login != "factory-bot" {
		t.Fatalf("resolveSelfLogin (3rd call) = %q, %v, want cached %q, nil", login, err, "factory-bot")
	}
	countAfter, err := os.ReadFile(filepath.Join(dir, "call-count"))
	if err != nil {
		t.Fatalf("read call-count: %v", err)
	}
	if string(countBefore) != string(countAfter) {
		t.Errorf("gh was invoked again after a successful resolve: count went from %s to %s, want unchanged (cached)", countBefore, countAfter)
	}
}

// TestReadReviewStateFailsClosedWhileSelfLoginUnresolved proves
// ReadReviewState returns an error -- and no threads -- when the
// factory's own gh login cannot be resolved, rather than proceeding
// without the self-exclusion (which would let the factory mistake its own
// PR replies for reviewer comments).
func TestReadReviewStateFailsClosedWhileSelfLoginUnresolved(t *testing.T) {
	resetSelfLoginCache(t)
	dir := t.TempDir()
	script := `#!/bin/sh
case "$1 $2" in
  "pr view")
    cat <<'EOF'
` + string(readTestdata(t, "prview_no_reviews.json")) + `
EOF
    ;;
  "api user")
    echo "gh: not authenticated" >&2
    exit 1
    ;;
  *)
    echo "fake-gh: unexpected invocation: $@" >&2
    exit 1
    ;;
esac
`
	ghBinary := filepath.Join(dir, "fake-gh")
	if err := os.WriteFile(ghBinary, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	opener := GHPullRequestOpener{GHBinary: ghBinary}
	state, err := opener.ReadReviewState(context.Background(), "https://github.com/acme/widgets/pull/42", AuthorPolicy{})
	if err == nil {
		t.Fatal("ReadReviewState = nil error while self-login is unresolved, want an error")
	}
	if len(state.BlocksReadyThreads) != 0 || len(state.ActionableThreads) != 0 {
		t.Errorf("BlocksReadyThreads = %+v, ActionableThreads = %+v, want none on a failed call", state.BlocksReadyThreads, state.ActionableThreads)
	}
}

// fakeGHPaginatedDispatcher writes a fake `gh` that serves reviewThreads
// two pages deep: an "api graphql" call with no "cursor=" argument gets
// page1Out, and one with a "cursor=" argument gets page2Out -- proving
// ReadReviewState's own pagination loop asks for, and merges, every page
// instead of stopping at GraphQL's first 100-thread page.
func fakeGHPaginatedDispatcher(t *testing.T, dir, prViewOut, page1Out, page2Out, userOut string) string {
	t.Helper()
	script := `#!/bin/sh
case "$1 $2" in
  "pr view")
    cat <<'EOF'
` + prViewOut + `
EOF
    ;;
  "api graphql")
    case "$*" in
      *cursor=*)
        cat <<'EOF'
` + page2Out + `
EOF
        ;;
      *)
        cat <<'EOF'
` + page1Out + `
EOF
        ;;
    esac
    ;;
  "api user")
    printf '%s' "` + userOut + `"
    ;;
  *)
    echo "fake-gh: unexpected invocation: $@" >&2
    exit 1
    ;;
esac
`
	path := filepath.Join(dir, "fake-gh")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake-gh: %v", err)
	}
	return path
}

func TestReadReviewStatePaginatesReviewThreads(t *testing.T) {
	resetSelfLoginCache(t)
	dir := t.TempDir()
	ghBinary := fakeGHPaginatedDispatcher(t, dir,
		string(readTestdata(t, "prview_no_reviews.json")),
		string(readTestdata(t, "reviewthreads_page1_of_2.json")),
		string(readTestdata(t, "reviewthreads_page2_of_2.json")),
		"factory-bot",
	)

	opener := GHPullRequestOpener{GHBinary: ghBinary}
	state, err := opener.ReadReviewState(context.Background(), "https://github.com/acme/widgets/pull/42", AuthorPolicy{})
	if err != nil {
		t.Fatalf("ReadReviewState: %v", err)
	}
	if len(state.BlocksReadyThreads) != 1 || state.BlocksReadyThreads[0].Author != "bob" {
		t.Fatalf("BlocksReadyThreads = %+v, want bob's thread from page 2", state.BlocksReadyThreads)
	}
}

func TestReviewThreadsPageInfoAbsentMeansNoNextPage(t *testing.T) {
	hasNextPage, endCursor, err := reviewThreadsPageInfo(readTestdata(t, "reviewthreads_one_unresolved.json"))
	if err != nil {
		t.Fatalf("reviewThreadsPageInfo: %v", err)
	}
	if hasNextPage || endCursor != "" {
		t.Errorf("hasNextPage=%v endCursor=%q, want false/\"\" for a fixture with no pageInfo", hasNextPage, endCursor)
	}
}

func TestReviewThreadsPageInfoReportsHasNextPage(t *testing.T) {
	hasNextPage, endCursor, err := reviewThreadsPageInfo(readTestdata(t, "reviewthreads_page1_of_2.json"))
	if err != nil {
		t.Fatalf("reviewThreadsPageInfo: %v", err)
	}
	if !hasNextPage || endCursor != "CURSOR_PAGE_1" {
		t.Errorf("hasNextPage=%v endCursor=%q, want true/%q", hasNextPage, endCursor, "CURSOR_PAGE_1")
	}
}
