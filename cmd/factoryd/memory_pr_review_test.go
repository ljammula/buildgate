package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"buildgate/internal/forge"
	"buildgate/internal/request"
	"buildgate/internal/requestdriver"
	"buildgate/internal/run"
)

// A trusted reviewer's comment on a memory request's pull request starts no
// corrective build: a build that reworded a line would be refused at release
// after its cost. The comment is answered once, saying how a memory change is
// edited, and recorded as seen. The same comment on any other request starts
// a round.
func TestPRReviewCommentOnAMemoryRequestStartsNoRoundAndIsAnswered(t *testing.T) {
	threads := []forge.Thread{{ID: "thread-1", Path: "AGENTS.md", Line: 3, Author: "alice", Body: "reword this line", CommentID: 5}}
	state := forge.ReviewState{State: "OPEN", BlocksReadyThreads: threads, ActionableThreads: threads}
	cfg := requestdriver.WorkerConfig{PrPollInterval: time.Minute, MaxReviewRounds: 2}
	for _, memoryRequest := range []bool{true, false} {
		dp := newTestDeps(t)
		r, dataDir := stubPRReviewTestFixture(t, 1)
		if memoryRequest {
			r.Source = request.Source{Kind: request.SourceMemory}
			if err := r.Save(dataDir); err != nil {
				t.Fatal(err)
			}
		}
		stubPRReviewDeps(dp, t, state, nil)
		builds := 0
		requestdriver.PrReviewCorrectiveRunner = func(ctx context.Context, args []string, onReady func(*run.Run)) error {
			builds++
			return reviewFlaggedRoundRun(argValue(args, "-ticket"), "x").Save(dataDir)
		}
		var replies []string
		failReply := memoryRequest
		fakeForgeOf(dp).replyToReviewCommentFn = func(ctx context.Context, prURL string, commentID int64, body string) error {
			if failReply {
				failReply = false
				return errors.New("gh: network unreachable")
			}
			replies = append(replies, body)
			return nil
		}
		fakeForgeOf(dp).listReviewCommentsFn = func(ctx context.Context, prURL string) ([]requestdriver.ReviewComment, error) {
			return nil, nil
		}
		now := time.Now()
		for i := 0; i < 3; i++ {
			if err := requestdriver.AdvancePRReview(dp, context.Background(), dataDir, r, cfg, now.Add(time.Duration(i)*2*time.Minute)); err != nil {
				t.Fatal(err)
			}
		}
		loaded, err := request.Load(dataDir, r.ID)
		if err != nil {
			t.Fatal(err)
		}
		ticket := loaded.Tickets[0]
		if !memoryRequest {
			if builds == 0 || len(ticket.Rounds) == 0 {
				t.Fatalf("an ordinary request's comment started no round (builds %d, rounds %d): the control proves nothing", builds, len(ticket.Rounds))
			}
			continue
		}
		if builds != 0 || len(ticket.Rounds) != 0 {
			t.Errorf("a comment on a memory request started %d build(s) and recorded %d round(s)", builds, len(ticket.Rounds))
		}
		// The first reply failed, so the thread was answered on the next
		// poll, and once.
		if len(replies) != 1 || replies[0] != requestdriver.MemoryRequestReviewReply {
			t.Errorf("replies = %q, want the memory reply once", replies)
		}
		if len(ticket.SeenThreadIDs) != 1 || ticket.SeenThreadIDs[0] != forge.ThreadSeenKey(threads[0]) {
			t.Errorf("seen threads = %v", ticket.SeenThreadIDs)
		}
		if loaded.State != request.StatePRReview {
			t.Errorf("state = %s, want pr_review", loaded.State)
		}
	}
}
