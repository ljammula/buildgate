package requestdriver_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"buildgate/internal/request"
	"buildgate/internal/requestdriver"
)

func TestReplyToReviewCommentPathIsScopedByPullNumber(t *testing.T) {
	owner, repo, number, err := requestdriver.ParsePullRequestOwnerRepo("https://github.com/acme/widget/pull/271")
	if err != nil {
		t.Fatal(err)
	}
	got := requestdriver.ReplyToReviewCommentPath(owner, repo, number, 987)
	if got != "repos/acme/widget/pulls/271/comments/987/replies" {
		t.Fatalf("path = %q", got)
	}
}

// TestCurrentPRHeadRunIDPrefersLatestAcceptedRound is the direct unit
// test for the helper: it must skip non-accepted rounds (which never
// move the branch) and return the ticket's original RunID when no round
// has been accepted yet.
func TestCurrentPRHeadRunIDPrefersLatestAcceptedRound(t *testing.T) {
	cases := []struct {
		name   string
		ticket request.Ticket
		want   string
	}{
		{"no rounds", request.Ticket{RunID: "run-1"}, "run-1"},
		{"only a halted round", request.Ticket{RunID: "run-1", Rounds: []request.Round{{Index: 1, RunID: "run-1-round-1", Outcome: request.RoundHalted}}}, "run-1"},
		{"one accepted, confirmed-pushed round", request.Ticket{RunID: "run-1", Rounds: []request.Round{{Index: 1, RunID: "run-1-round-1", Outcome: request.RoundAccepted, Pushed: true}}}, "run-1-round-1"},
		{
			"accepted but NOT pushed: falls back to ticket.RunID, not the unpushed round",
			request.Ticket{RunID: "run-1", Rounds: []request.Round{{Index: 1, RunID: "run-1-round-1", Outcome: request.RoundAccepted, Pushed: false}}},
			"run-1",
		},
		{"accepted then quarantined: latest accepted+pushed still wins", request.Ticket{RunID: "run-1", Rounds: []request.Round{
			{Index: 1, RunID: "run-1-round-1", Outcome: request.RoundAccepted, Pushed: true},
			{Index: 2, RunID: "run-1-round-2", Outcome: request.RoundQuarantined},
		}}, "run-1-round-1"},
		{"two accepted+pushed rounds: the later one wins", request.Ticket{RunID: "run-1", Rounds: []request.Round{
			{Index: 1, RunID: "run-1-round-1", Outcome: request.RoundAccepted, Pushed: true},
			{Index: 2, RunID: "run-1-round-2", Outcome: request.RoundAccepted, Pushed: true},
		}}, "run-1-round-2"},
		{
			"latest accepted round push failed: falls back to the earlier, confirmed-pushed round",
			request.Ticket{RunID: "run-1", Rounds: []request.Round{
				{Index: 1, RunID: "run-1-round-1", Outcome: request.RoundAccepted, Pushed: true},
				{Index: 2, RunID: "run-1-round-2", Outcome: request.RoundAccepted, Pushed: false},
			}},
			"run-1-round-1",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := requestdriver.CurrentPRHeadRunID(&tc.ticket); got != tc.want {
				t.Errorf("currentPRHeadRunID(...) = %q, want %q", got, tc.want)
			}
		})
	}
}

// prReplyComments is Flutter + Go app PR #331's review thread as GitHub listed it
// (2026-09-28): the factory replied to 4122296002, and GitHub filed that
// reply (4122411995) under the thread root 4121343639 while gh exited 1.
var prReplyComments = []requestdriver.ReviewComment{
	{ID: 4121343639, Body: "`from` and `to` are concatenated unescaped"},
	{ID: 4121463880, InReplyToID: 4121343639, Body: "Addressed in 3e4c78f."},
	{ID: 4122296002, InReplyToID: 4121343639, Body: "The PR head is still c90bbaf"},
	{ID: 4122411995, InReplyToID: 4121343639, Body: "Addressed in 1610b47."},
	{ID: 5000000001, Body: "another thread"},
}

func TestReplyLandedMatchesThreadRootAndBody(t *testing.T) {
	cases := []struct {
		name      string
		commentID int64
		body      string
		want      bool
	}{
		{"reply_to_a_reply_filed_under_the_root", 4122296002, "Addressed in 1610b47.", true},
		{"reply_to_the_root_itself", 4121343639, "Addressed in 1610b47.", true},
		{"different_body", 4122296002, "Addressed in deadbee.", false},
		{"same_body_other_thread", 5000000001, "Addressed in 1610b47.", false},
		{"unknown_comment", 42, "Addressed in 1610b47.", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := requestdriver.ReplyLanded(prReplyComments, tc.commentID, tc.body); got != tc.want {
				t.Fatalf("replyLanded(%d, %q) = %v, want %v", tc.commentID, tc.body, got, tc.want)
			}
		})
	}
}

func TestPostReviewReplyConfirmsAfterGhError(t *testing.T) {
	dp := newFakeDeps(t)
	origReply, origList := dp.replyToReviewCommentFn, dp.listReviewCommentsFn
	t.Cleanup(func() {
		dp.replyToReviewCommentFn, dp.listReviewCommentsFn = origReply, origList
	})
	const prURL = "https://github.com/example/example-app/pull/331"
	ghErr := errors.New("exit status 1: unexpected end of JSON input")

	cases := []struct {
		name    string
		replyOK bool
		list    []requestdriver.ReviewComment
		listErr error
		body    string
		wantErr string
	}{
		{name: "gh_ok", replyOK: true, body: "Addressed in 1610b47."},
		{name: "gh_error_but_reply_posted", list: prReplyComments, body: "Addressed in 1610b47."},
		{name: "gh_error_reply_missing", list: prReplyComments, body: "Addressed in deadbee.", wantErr: "unexpected end of JSON input"},
		{name: "gh_error_and_list_fails", listErr: errors.New("rate limited"), body: "Addressed in 1610b47.", wantErr: "checking whether it posted anyway: rate limited"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			listed := false
			dp.replyToReviewCommentFn = func(context.Context, string, int64, string) error {
				if tc.replyOK {
					return nil
				}
				return ghErr
			}
			dp.listReviewCommentsFn = func(context.Context, string) ([]requestdriver.ReviewComment, error) {
				listed = true
				return tc.list, tc.listErr
			}
			err := requestdriver.PostReviewReply(dp, context.Background(), prURL, 4122296002, tc.body)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("postReviewReply: %v, want nil", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("postReviewReply error = %v, want it to contain %q", err, tc.wantErr)
			}
			if tc.replyOK && listed {
				t.Fatal("listed comments after a successful reply")
			}
		})
	}
}
