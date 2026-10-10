package main

import (
	"buildgate/internal/requestdriver"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"buildgate/internal/forge"
	"buildgate/internal/release"
	"buildgate/internal/request"
	"buildgate/internal/run"
)

// stubPRReviewTestFixture builds a minimal request in pr_review with one
// ticket whose PR is already open, plus that ticket's own spec file on
// disk (needed only by the corrective-round path, which reads
// ticket.SpecPath via ticketspec.ParseVerifyCommand) -- returns the
// request and dataDir.
// testTicketRunResultSHA is stubPRReviewTestFixture's own ticket i's
// ResultSHA -- a package-level helper (not just a literal inline) so
// every test constructing its own forge.ReviewState{HeadSHA: ...} to
// exercise a "marked ready"/"approved" path can compute the exact value
// advancePRReadyOrApproved's own state.HeadSHA-vs-ResultSHA check (found
// via review, GitHub Codex App, PR #154 round 5) requires them to match,
// without duplicating the literal or risking it drifting from the
// fixture that sets it.
func testTicketRunResultSHA(i int) string {
	return fmt.Sprintf("%040d", 900+i)
}

// testLastPushedSHA is stubPRReviewDeps' own default forge.pushExistingBranch/
// forge.remoteBranchHeadSHA pairing's shared state -- see stubPRReviewDeps'
// own doc comment. Reset at the start of every stubPRReviewDeps call, so
// tests never leak state into one another despite this package's
// non-parallel convention.
var testLastPushedSHA map[string]string

func stubPRReviewTestFixture(t *testing.T, ticketCount int) (*request.Request, string) {
	t.Helper()
	dataDir := t.TempDir()
	workspace := t.TempDir()
	tickets := make([]request.Ticket, 0, ticketCount)
	for i := 1; i <= ticketCount; i++ {
		specPath := filepath.Join(dataDir, fmt.Sprintf("ticket-%d.spec.md", i))
		if err := os.WriteFile(specPath, []byte("Verify-Command: make verify\n\n## Goal\n\nDo the thing.\n"), 0o600); err != nil {
			t.Fatalf("write ticket spec: %v", err)
		}
		runID := fmt.Sprintf("run-%d", i)
		tickets = append(tickets, request.Ticket{
			Index:    i,
			SpecPath: specPath,
			RunID:    runID,
			PRURL:    fmt.Sprintf("https://github.com/acme/widget/pull/%d", 100+i),
		})
		// runCorrectiveRound loads this record (run.Load(dataDir,
		// ticket.RunID)) to read its BaseSHA for -diff-base -- without it
		// on disk, every corrective-round test below would fail at that
		// load, not at whatever it's actually testing. Project: "widget"
		// (matching the request's own Project field below) so
		// release.ProjectOf resolves to the same project the recorded
		// decision just below is saved under.
		ticketRun := &run.Run{ID: runID, Project: "widget", BaseSHA: fmt.Sprintf("%040d", i), ResultSHA: testTicketRunResultSHA(i)}
		if err := ticketRun.Save(dataDir); err != nil {
			t.Fatalf("save ticket run fixture: %v", err)
		}
		// advancePRReadyOrApproved's own release-decision gate (added
		// alongside the "check the decision before opening a PR" fix)
		// requires an Allowed decision on file before it will ever call
		// `gh pr ready` -- without this, every "marked ready" case in
		// TestAdvancePRReviewStateTable below would silently stop calling
		// it. Tests that specifically want a denied/missing decision seed
		// their own fixture instead of using this default.
		if err := release.SaveDecision(dataDir, release.Decision{RunID: runID, Project: "widget", Allowed: true, Evaluated: time.Now().UTC().Format(time.RFC3339)}); err != nil {
			t.Fatalf("save ticket run's allowed release decision fixture: %v", err)
		}
	}
	r := &request.Request{
		ID:          "req-1",
		Workspace:   workspace,
		Project:     "widget",
		State:       request.StatePRReview,
		TicketIndex: 1,
		TicketCount: ticketCount,
		Tickets:     tickets,
		SubmittedAt: time.Now().UTC().Format(time.RFC3339Nano),
		UpdatedAt:   time.Now().UTC().Format(time.RFC3339Nano),
		EnteredAt:   time.Now().UTC().Format(time.RFC3339Nano),
	}
	if err := r.Save(dataDir); err != nil {
		t.Fatalf("save fixture request: %v", err)
	}
	return r, dataDir
}

// stubPRReviewDeps overrides every package-level seam pr_review_driver.go
// exposes, restoring each to its production value on test cleanup -- the
// same "swap and defer-restore" shape worker_config_test.go's own
// stubWorkerDoctorChecks helper uses.
func stubPRReviewDeps(dp *deps, t *testing.T, state forge.ReviewState, readErr error) {
	t.Helper()
	origRead := fakeForgeOf(dp).readReviewStateFn
	origRunner := requestdriver.PrReviewCorrectiveRunner
	origReady := fakeForgeOf(dp).markPullRequestReadyFn
	origUndoReady := fakeForgeOf(dp).undoMarkPullRequestReadyFn
	origPush := fakeForgeOf(dp).pushExistingBranchFn
	origRemoteHead := fakeForgeOf(dp).remoteBranchHeadSHAFn
	origDescendsFromHead := fakeForgeOf(dp).roundResultDescendsFromHeadFn
	origReply := fakeForgeOf(dp).replyToReviewCommentFn
	origStartNext := requestdriver.ContinueAfterPRApproval
	t.Cleanup(func() {
		fakeForgeOf(dp).readReviewStateFn = origRead
		requestdriver.PrReviewCorrectiveRunner = origRunner
		fakeForgeOf(dp).markPullRequestReadyFn = origReady
		fakeForgeOf(dp).undoMarkPullRequestReadyFn = origUndoReady
		fakeForgeOf(dp).pushExistingBranchFn = origPush
		fakeForgeOf(dp).remoteBranchHeadSHAFn = origRemoteHead
		fakeForgeOf(dp).roundResultDescendsFromHeadFn = origDescendsFromHead
		fakeForgeOf(dp).replyToReviewCommentFn = origReply
		requestdriver.ContinueAfterPRApproval = origStartNext
	})
	// Defaults to a no-op success: every case that doesn't specifically
	// exercise the post-ready-flip revert (decisionStillAllows still true
	// between the pre-flip check and forge.markPullRequestReady) never
	// reaches this seam at all, but stubbing it here rather than leaving
	// it at its production real-`gh`-binary value means a future test
	// that DOES reach it without overriding this default fails loudly
	// (a real `gh` binary invocation in a unit test) rather than
	// silently depending on one being installed.
	fakeForgeOf(dp).undoMarkPullRequestReadyFn = func(ctx context.Context, prURL string) error { return nil }
	fakeForgeOf(dp).readReviewStateFn = func(ctx context.Context, prURL string, policy forge.AuthorPolicy) (forge.ReviewState, error) {
		return state, readErr
	}
	// testLastPushedSHA/forge.remoteBranchHeadSHA/forge.roundResultDescendsFromHead
	// default to a consistent, always-succeeding trio for
	// pushAcceptedRoundAndReply's own pre-push branch/ancestry guards and
	// its post-push remote-verification read (see that function's own
	// doc comments, pr_review_driver.go): none of this package's fixture
	// workspaces are real git repositories, so these seams exist
	// precisely so a test never needs one just to reach an accepted
	// round's push. A test that specifically exercises one of these
	// guards overrides forge.roundResultDescendsFromHead or
	// forge.remoteBranchHeadSHA directly instead of using this default.
	testLastPushedSHA = map[string]string{}
	fakeForgeOf(dp).remoteBranchHeadSHAFn = func(ctx context.Context, workspaceDir, branch string) (string, error) {
		if sha, ok := testLastPushedSHA[branch]; ok {
			return sha, nil
		}
		return strings.Repeat("0", 40), nil
	}
	fakeForgeOf(dp).roundResultDescendsFromHeadFn = func(dir, ancestor, descendant string) (bool, error) { return true, nil }
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

// acceptAndReview makes ticket i's fixture run an accepted one whose
// code_review gate passed: the build the ready-to-merge bar wants behind a
// pull request's head.
func acceptAndReview(t *testing.T, dataDir string, i int, gates ...run.GateResult) {
	t.Helper()
	loaded, err := run.Load(dataDir, fmt.Sprintf("run-%d", i))
	if err != nil {
		t.Fatal(err)
	}
	loaded.State = run.StateAccepted
	loaded.GateResults = gates
	if err := loaded.Save(dataDir); err != nil {
		t.Fatal(err)
	}
}

// TestPRPollChecksTheReadyToMergeBar: each poll that finds no new thread
// records whether the pull request is ready to merge, and every reason it
// is not.
func TestPRPollChecksTheReadyToMergeBar(t *testing.T) {
	yes, no := true, false
	head := testTicketRunResultSHA(1)
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
			r, dataDir := stubPRReviewTestFixture(t, 1)
			acceptAndReview(t, dataDir, 1, tc.gates...)
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

// TestPollTicketPRCancelledDuringPollDoesNotResurrectRequest covers an
// adversarial-review finding (2026-09-24) for the pr_review poll: a
// cancel landing while forge.readReviewState's network call is in flight must
// survive pollTicketPR's own r.Save afterwards -- see stillInState's doc
// comment (request_driver.go).
func TestPollTicketPRCancelledDuringPollDoesNotResurrectRequest(t *testing.T) {
	dp := newTestDeps(t)
	r, dataDir := stubPRReviewTestFixture(t, 1)
	stubPRReviewDeps(dp, t, forge.ReviewState{State: "OPEN"}, nil)
	fakeForgeOf(dp).readReviewStateFn = func(ctx context.Context, prURL string, policy forge.AuthorPolicy) (forge.ReviewState, error) {
		cancelRequestForTest(t, dataDir, r.ID)
		return forge.ReviewState{State: "OPEN"}, nil
	}

	if err := requestdriver.AdvancePRReview(dp, context.Background(), dataDir, r, requestdriver.WorkerConfig{PrPollInterval: time.Minute}, time.Now()); err != nil {
		t.Fatalf("advancePRReview: %v", err)
	}

	loaded, err := request.Load(dataDir, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.State != request.StateCancelled {
		t.Fatalf("State = %q, want %q (a cancel mid-poll must survive pollTicketPR's own save)", loaded.State, request.StateCancelled)
	}
}
