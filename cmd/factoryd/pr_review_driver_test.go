package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"buildgate/internal/forge"
	"buildgate/internal/release"
	"buildgate/internal/requestdriver"
	"buildgate/internal/requestdriver/requestdrivertest"
	"buildgate/internal/run"
)

// stubPRReviewDeps is stubPRReviewHooks over this package's forge fake.
func stubPRReviewDeps(dp *deps, t *testing.T, state forge.ReviewState, readErr error) {
	t.Helper()
	f := fakeForgeOf(dp)
	requestdrivertest.StubPRReviewHooks(requestdrivertest.PrReviewHooks{
		ReadReviewState: &f.readReviewStateFn, MarkPullRequestReady: &f.markPullRequestReadyFn,
		UndoMarkPullRequestReady: &f.undoMarkPullRequestReadyFn, PushExistingBranch: &f.pushExistingBranchFn,
		RemoteBranchHeadSHA: &f.remoteBranchHeadSHAFn, RoundResultDescendsFromHead: &f.roundResultDescendsFromHeadFn,
		ReplyToReviewComment: &f.replyToReviewCommentFn,
	}, t, state, readErr)
}

// TestWorkerConfigAcceptsMaxReviewRoundsFlag proves -max-review-rounds is
// parsed through the real flag set (see the plan's own "any new argv is
// parsed through the real flag set" ground rule).
func TestWorkerConfigAcceptsMaxReviewRoundsFlag(t *testing.T) {
	dp := newTestDeps(t)
	isolateSessionConfig(t)
	_, _, err := loadTestWorkerConfig(dp, []string{
		"-data-dir", t.TempDir(),
		"-max-review-rounds", "5",
		"-registry-proxy", "-registry-proxy-image", "registry.example/org/rp@sha256:deadbeef",
	})
	if err == nil {
		t.Fatal("workerMain(-max-review-rounds without -sandbox-image) = nil, want an error")
	}
	if strings.Contains(err.Error(), "not defined") {
		t.Fatalf("workerMain rejected -max-review-rounds as an unknown flag: %v", err)
	}
	if !strings.Contains(err.Error(), "-registry-proxy requires -sandbox-image") {
		t.Fatalf("err = %v, want it to name the missing -sandbox-image (proving flag parsing got past -max-review-rounds)", err)
	}
}

// TestWorkerConfigRejectsMaxReviewRoundsBelowOne covers -max-review-rounds'
// own validation.
func TestWorkerConfigRejectsMaxReviewRoundsBelowOne(t *testing.T) {
	dp := newTestDeps(t)
	isolateSessionConfig(t)
	_, _, err := loadTestWorkerConfig(dp, []string{"-data-dir", t.TempDir(), "-sandbox-image", "img@sha256:aaaa", "-max-review-rounds", "0"})
	if err == nil {
		t.Fatal("workerMain(-max-review-rounds 0) = nil, want an error")
	}
	if !strings.Contains(err.Error(), "-max-review-rounds must be at least 1") {
		t.Fatalf("err = %v, want it to name the -max-review-rounds validation", err)
	}
}

// TestPRPollChecksTheReadyToMergeBar: each poll that finds no new thread
// records whether the pull request is ready to merge, and every reason it
// is not.
func TestPRPollChecksTheReadyToMergeBar(t *testing.T) {
	yes, no := true, false
	head := requestdrivertest.TestTicketRunResultSHA(1)
	reviewed := run.GateResult{Check: "code_review", Passed: true}
	untrusted := []forge.Thread{{ID: "t1", Author: "mallory", Body: "hm", CommentID: 9}}
	cases := []struct {
		name      string
		state     forge.ReviewState
		gates     []run.GateResult
		deny      bool
		wantReady bool
		wantBlock []string
	}{
		{"everything in place", forge.ReviewState{State: "OPEN", ChecksPassing: &yes, HeadSHA: head}, []run.GateResult{reviewed}, false, true, nil},
		{"no checks configured counts as none failing", forge.ReviewState{State: "OPEN", HeadSHA: head}, []run.GateResult{reviewed}, false, true, nil},
		{"a draft that just went ready", forge.ReviewState{State: "OPEN", IsDraft: true, ChecksPassing: &yes, HeadSHA: head}, []run.GateResult{reviewed}, false, true, nil},
		{"checks failing", forge.ReviewState{State: "OPEN", ChecksPassing: &no, HeadSHA: head}, []run.GateResult{reviewed}, false, false, []string{"its checks are pending or failing"}},
		{"a draft with failing checks stays a draft", forge.ReviewState{State: "OPEN", IsDraft: true, ChecksPassing: &no, HeadSHA: head}, []run.GateResult{reviewed}, false, false, []string{"it is still a draft", "its checks are pending or failing"}},
		{"an untrusted reviewer's open thread", forge.ReviewState{State: "OPEN", ChecksPassing: &yes, HeadSHA: head, BlocksReadyThreads: untrusted}, []run.GateResult{reviewed}, false, false, []string{"1 review thread(s) are open"}},
		{"changes requested", forge.ReviewState{State: "OPEN", ChecksPassing: &yes, HeadSHA: head, ReviewDecision: forge.ReviewDecisionChangesRequested}, []run.GateResult{reviewed}, false, false, []string{"a reviewer requested changes"}},
		{"someone pushed to the branch", forge.ReviewState{State: "OPEN", ChecksPassing: &yes, HeadSHA: "ffffffffffffffffffffffffffffffffffffffff"}, []run.GateResult{reviewed}, false, false, []string{"its head ffffffffffff is not the commit the factory last built and reviewed"}},
		{"built with code review off", forge.ReviewState{State: "OPEN", ChecksPassing: &yes, HeadSHA: head}, nil, false, false, []string{"has no code review of the whole diff on record"}},
		{"release decision withdrawn", forge.ReviewState{State: "OPEN", ChecksPassing: &yes, HeadSHA: head}, []run.GateResult{reviewed}, true, false, []string{"the release decision for run run-1 does not allow it"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dp := newTestDeps(t)
			r, dataDir := requestdrivertest.StubPRReviewTestFixture(t, 1)
			requestdrivertest.AcceptAndReview(t, dataDir, 1, tc.gates...)
			if tc.deny {
				if err := release.SaveDecision(dataDir, release.Decision{RunID: "run-1", Project: "widget", Allowed: false, Evaluated: time.Now().UTC().Format(time.RFC3339)}); err != nil {
					t.Fatal(err)
				}
			}
			stubPRReviewDeps(dp, t, tc.state, nil)
			fakeForgeOf(dp).markPullRequestReadyFn = func(ctx context.Context, prURL string) error { return nil }
			if err := requestdriver.AdvancePRReview(dp, context.Background(), dataDir, r, requestdriver.WorkerConfig{PrPollInterval: time.Minute, MaxReviewRounds: 3}, time.Now()); err != nil {
				t.Fatal(err)
			}
			got := r.Tickets[0].MergeReadiness
			if got == nil {
				t.Fatal("the poll recorded no readiness check")
			}
			if got.Ready != tc.wantReady || got.HeadSHA != tc.state.HeadSHA || got.CheckedAt == "" {
				t.Errorf("readiness = %+v, want ready=%v for head %s", got, tc.wantReady, tc.state.HeadSHA)
			}
			if len(got.Blockers) != len(tc.wantBlock) {
				t.Fatalf("blockers = %q, want %q", got.Blockers, tc.wantBlock)
			}
			for i, want := range tc.wantBlock {
				if !strings.Contains(got.Blockers[i], want) {
					t.Errorf("blocker %d = %q, want it to say %q", i, got.Blockers[i], want)
				}
			}
			if summary := requestTicketPRSummary(r); strings.Contains(summary, "ready-to-merge") != tc.wantReady {
				t.Errorf("status summary = %q, want ready-to-merge shown only when ready", summary)
			}
		})
	}
}
