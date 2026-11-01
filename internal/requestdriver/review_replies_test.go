package requestdriver_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"buildgate/internal/forge"
	"buildgate/internal/request"
	"buildgate/internal/requestdriver"
	"buildgate/internal/requestdriver/requestdrivertest"
	"buildgate/internal/run"
)

const replySHA = "cafef00dcafef00dcafef00dcafef00dcafef00d"

type postedReply struct {
	id   int64
	body string
}

// runReplyRound runs one accepted review round whose builder's last final
// message is final (evidence nil when withEvidence is false) over threads,
// and returns the replies and the pull request comments posted.
func runReplyRound(t *testing.T, threads []forge.Thread, final string, withEvidence bool) ([]postedReply, []string) {
	t.Helper()
	dp := newFakeDeps(t)
	r, dataDir := requestdrivertest.StubPRReviewTestFixture(t, 1)
	stubPRReviewDeps(dp, t, forge.ReviewState{State: "OPEN", BlocksReadyThreads: threads, ActionableThreads: threads}, nil)
	requestdriver.PrReviewCorrectiveRunner = func(ctx context.Context, args []string, onReady func(*run.Run)) error {
		a := &run.Run{ID: requestdrivertest.ArgValue(args, "-ticket"), State: run.StateAccepted, WorkspacePath: t.TempDir(), ResultSHA: replySHA, Branch: requestdrivertest.ArgValue(args, "-on-branch")}
		if withEvidence {
			a.AgentEvidence = &run.AgentEvidence{Rounds: []run.AgentEvidenceRound{{Index: 1, FinalMessage: "earlier"}, {Index: 2, FinalMessage: final}}}
		}
		return saveAcceptedRoundRun(t, dataDir, a)
	}
	dp.pushExistingBranchFn = func(ctx context.Context, workspaceDir, sha, branch string) error {
		requestdrivertest.LastPushedSHA[branch] = sha
		return nil
	}
	var replies []postedReply
	dp.replyToReviewCommentFn = func(ctx context.Context, prURL string, commentID int64, body string) error {
		replies = append(replies, postedReply{commentID, body})
		return nil
	}
	var comments []string
	dp.commentOnPullRequestFn = func(ctx context.Context, prURL, body string) error {
		comments = append(comments, body)
		return nil
	}
	if err := requestdriver.AdvancePRReview(dp, context.Background(), dataDir, r, requestdriver.WorkerConfig{PrPollInterval: time.Minute, MaxReviewRounds: 3}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if r.State == request.StateHalted {
		t.Fatalf("halted: %s", r.Error)
	}
	return replies, comments
}

var twoThreads = []forge.Thread{
	{ID: "t1", Path: "a.go", Line: 3, Author: "alice", Body: "first", CommentID: 11},
	{ID: "t2", Path: "b.go", Line: 9, Author: "alice", Body: "second", CommentID: 12},
}

func TestReviewReplySingleThreadCarriesTheBuildersWordsAndTheCommit(t *testing.T) {
	threads := []forge.Thread{{ID: "t1", Path: "a.go", Line: 3, Author: "alice", Body: "fix", CommentID: 11}}
	final := "Done.\nReply (elsewhere.go:99):\nChanged the id; formatId stays because a committed challenge test requires the decimal ids.\nSecond line."
	replies, comments := runReplyRound(t, threads, final, true)
	if len(replies) != 1 || len(comments) != 0 {
		t.Fatalf("replies = %+v, comments = %v", replies, comments)
	}
	want := "Changed the id; formatId stays because a committed challenge test requires the decimal ids.\nSecond line.\n\nPushed in " + replySHA + "."
	if replies[0].body != want {
		t.Errorf("body = %q, want %q", replies[0].body, want)
	}
}

func TestReviewReplyMapsByPathAndLine(t *testing.T) {
	final := "Reply (b.go:9):\nB done.\nReply (a.go:3): A done."
	replies, comments := runReplyRound(t, twoThreads, final, true)
	if len(replies) != 2 || len(comments) != 0 {
		t.Fatalf("replies = %+v, comments = %v", replies, comments)
	}
	for _, rp := range replies {
		switch rp.id {
		case 11:
			if !strings.HasPrefix(rp.body, "A done.\n\nPushed in ") {
				t.Errorf("thread a.go body = %q", rp.body)
			}
		case 12:
			if !strings.HasPrefix(rp.body, "B done.\n\nPushed in ") {
				t.Errorf("thread b.go body = %q", rp.body)
			}
		}
	}
}

func TestReviewReplyForNoThreadIsOnePullRequestComment(t *testing.T) {
	final := "Reply (a.go:3):\nA done.\nReply (zzz.go:1):\nStray."
	replies, comments := runReplyRound(t, twoThreads, final, true)
	if len(comments) != 1 || !strings.Contains(comments[0], "zzz.go:1") || !strings.Contains(comments[0], "Stray.") || !strings.Contains(comments[0], replySHA) {
		t.Fatalf("comments = %v", comments)
	}
	if len(replies) != 2 {
		t.Fatalf("replies = %+v", replies)
	}
	for _, rp := range replies {
		if rp.id == 12 && rp.body != "Addressed in "+replySHA+"." {
			t.Errorf("thread without a reply body = %q", rp.body)
		}
	}
}

func TestReviewReplyWithoutBlocksOrEvidenceIsAddressedIn(t *testing.T) {
	want := "Addressed in " + replySHA + "."
	for name, tc := range map[string]struct {
		final string
		ev    bool
	}{"no_blocks": {"All done, nothing to say.", true}, "no_evidence": {"Reply (a.go:3):\nx", false}} {
		replies, comments := runReplyRound(t, twoThreads, tc.final, tc.ev)
		if len(replies) != 2 || len(comments) != 0 {
			t.Fatalf("%s: replies = %+v, comments = %v", name, replies, comments)
		}
		for _, rp := range replies {
			if rp.body != want {
				t.Errorf("%s: body = %q, want %q", name, rp.body, want)
			}
		}
	}
}

func TestReviewReplyNeutralisesMentionsAndCapsLength(t *testing.T) {
	threads := []forge.Thread{{ID: "t1", Path: "a.go", Line: 3, Author: "alice", Body: "fix", CommentID: 11}}
	final := "Reply (a.go:3):\nthanks @octocat <b>x</b> " + strings.Repeat("y", 5000)
	replies, _ := runReplyRound(t, threads, final, true)
	if len(replies) != 1 {
		t.Fatalf("replies = %+v", replies)
	}
	b := replies[0].body
	if strings.Contains(b, "@octocat") || !strings.Contains(b, "@​octocat") || strings.Contains(b, "<b>") {
		t.Errorf("mention or HTML not neutralised: %q", b[:80])
	}
	if len(b) > 1700 || !strings.HasSuffix(b, "Pushed in "+replySHA+".") {
		t.Errorf("reply length = %d, want capped with the commit line kept", len(b))
	}
}

func TestRoundBriefAsksForPerCommentReplies(t *testing.T) {
	r, dataDir := requestdrivertest.StubPRReviewTestFixture(t, 1)
	path, err := requestdriver.WriteRoundAddendum(dataDir, r.ID, &r.Tickets[0], twoThreads, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	content, _ := os.ReadFile(path)
	if !strings.Contains(string(content), "`Reply (<path>:<line>):`") {
		t.Fatalf("brief lacks the reply instruction:\n%s", content)
	}
}
