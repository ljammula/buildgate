package requestdriver_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"buildgate/internal/forge"
	"buildgate/internal/request"
	"buildgate/internal/requestdriver"
	"buildgate/internal/requestdriver/requestdrivertest"
	"buildgate/internal/run"
	"buildgate/internal/unresolved"
)

func acceptingStackedBuildRunner(t *testing.T, dataDir string, built *[][]string) requestdriver.TicketRunner {
	t.Helper()
	return func(ctx context.Context, args []string, onReady func(*run.Run)) error {
		ticketID := requestdrivertest.ArgValue(args, "-ticket")
		*built = append(*built, append([]string(nil), args...))
		started := &run.Run{ID: ticketID, State: run.StateReady}
		if onReady != nil {
			onReady(started)
		}
		baseSHA := strings.Repeat("1", 40)
		if priorID := requestdrivertest.ArgValue(args, "-prior-run"); priorID != "" {
			prior, err := run.Load(dataDir, priorID)
			if err != nil {
				t.Fatalf("load prior run %q: %v", priorID, err)
			}
			baseSHA = prior.ResultSHA
		}
		accepted := &run.Run{
			ID:             ticketID,
			Project:        "widget",
			State:          run.StateAccepted,
			Branch:         "factoryd/" + ticketID,
			BaseSHA:        baseSHA,
			ResultSHA:      "result-" + ticketID,
			StackBaseSHA:   started.StackBaseSHA,
			PullRequestURL: "https://github.com/acme/widget/pull/" + ticketID,
		}
		if err := accepted.Save(dataDir); err != nil {
			t.Fatalf("save accepted run %q: %v", ticketID, err)
		}
		return nil
	}
}

// earlierPROpenAt makes the earlier ticket's pull request read open, with
// its branch on origin at head.
func earlierPROpenAt(dp *fakeDeps, head string) {
	dp.readReviewStateFn = func(context.Context, string, forge.AuthorPolicy) (forge.ReviewState, error) {
		return forge.ReviewState{State: forge.PRStateOpen, IsDraft: true, HeadSHA: head}, nil
	}
	dp.remoteBranchHeadSHAFn = func(context.Context, string, string) (string, error) {
		return head, nil
	}
}

// buildFirstTicket builds ticket 1 of a two-ticket request with runner and
// returns the request, now building ticket 2.
func buildFirstTicket(t *testing.T, dp *fakeDeps, dataDir, id string, runner requestdriver.TicketRunner) *request.Request {
	t.Helper()
	r, err := request.Load(dataDir, id)
	if err != nil {
		t.Fatal(err)
	}
	if err := requestdriver.AdvanceBuilding(dp, context.Background(), dataDir, r, stackedCfg, runner, time.Now()); err != nil {
		t.Fatalf("build ticket 1: %v", err)
	}
	if r.TicketIndex != 2 || r.State != request.StateBuilding {
		t.Fatalf("after ticket 1: state=%q index=%d, want building ticket 2", r.State, r.TicketIndex)
	}
	return r
}

var stackedCfg = requestdriver.WorkerConfig{OpenPullRequest: true}

func addPushedRound(t *testing.T, dataDir string, ticket *request.Ticket, roundID, branch, result, stackBase string) {
	t.Helper()
	if err := (&run.Run{
		ID:           roundID,
		Project:      "widget",
		State:        run.StateAccepted,
		Branch:       branch,
		BaseSHA:      "round-base-" + roundID,
		ResultSHA:    result,
		StackBaseSHA: stackBase,
	}).Save(dataDir); err != nil {
		t.Fatalf("save round run: %v", err)
	}
	ticket.Rounds = append(ticket.Rounds, request.Round{
		Index:   len(ticket.Rounds) + 1,
		RunID:   roundID,
		Outcome: request.RoundAccepted,
		Pushed:  true,
	})
}

// TestAdvanceBuildingChainsFromEarlierTicketPRHeadRound: once a review round
// of ticket 1 was pushed, ticket 2 chains from that round's run, the commit
// on ticket 1's pull request, not from ticket 1's first build.
func TestAdvanceBuildingChainsFromEarlierTicketPRHeadRound(t *testing.T) {
	dp := newFakeDeps(t)
	dataDir, id := requestdrivertest.BuildingFixture(dp, t, 2)
	var built [][]string
	runner := acceptingStackedBuildRunner(t, dataDir, &built)
	r := buildFirstTicket(t, dp, dataDir, id, runner)

	const (
		roundID     = "req-1-001-review1"
		roundResult = "result-ticket-1-round"
	)
	addPushedRound(t, dataDir, &r.Tickets[0], roundID, "factoryd/req-1-001", roundResult, "")
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	earlierPROpenAt(dp, roundResult)

	if err := requestdriver.AdvanceBuilding(dp, context.Background(), dataDir, r, stackedCfg, runner, time.Now()); err != nil {
		t.Fatalf("build ticket 2: %v", err)
	}
	if len(built) != 2 {
		t.Fatalf("built %d tickets, want 2", len(built))
	}
	if got := requestdrivertest.ArgValue(built[1], "-prior-run"); got != roundID {
		t.Fatalf("ticket 2 -prior-run = %q, want current PR-head round %q", got, roundID)
	}
}

// TestAdvanceBuildingWaitsWhenEarlierTicketRemoteHeadDiffers: ticket 1's
// branch on origin is not at its PR-head run's commit, so ticket 2 is not
// built: the request stays building with a stacked_ticket wait, and the next
// pass, with the branch back at that commit, builds it and drops the wait.
func TestAdvanceBuildingWaitsWhenEarlierTicketRemoteHeadDiffers(t *testing.T) {
	dp := newFakeDeps(t)
	dataDir, id := requestdrivertest.BuildingFixture(dp, t, 2)
	var built [][]string
	runner := acceptingStackedBuildRunner(t, dataDir, &built)
	r := buildFirstTicket(t, dp, dataDir, id, runner)
	const roundID, roundResult = "req-1-001-review1", "result-ticket-1-round"
	addPushedRound(t, dataDir, &r.Tickets[0], roundID, "factoryd/req-1-001", roundResult, "")
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	earlierPROpenAt(dp, "some-direct-push")

	if err := requestdriver.AdvanceBuilding(dp, context.Background(), dataDir, r, stackedCfg, runner, time.Now()); err != nil {
		t.Fatalf("advance while predecessor remote head differs: %v", err)
	}
	if len(built) != 1 {
		t.Fatalf("built %d tickets, want ticket 2 withheld", len(built))
	}
	loaded, err := request.Load(dataDir, id)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.State != request.StateBuilding || loaded.TicketIndex != 2 {
		t.Fatalf("state=%q index=%d, want building at ticket 2", loaded.State, loaded.TicketIndex)
	}
	w := request.WaitFor(loaded, request.WaitFacts{WorkerLive: true})
	if w == nil || w.Kind != request.WaitStackedTicket || !strings.Contains(w.On, roundID) || !strings.Contains(loaded.NextAction(), "every PR poll") {
		t.Fatalf("wait=%+v next action=%q, want a stacked_ticket wait naming %s and the next check", w, loaded.NextAction(), roundID)
	}
	for _, remedy := range []string{"move the branch back to", "--force-with-lease=refs/heads/factoryd/req-1-001:some-direct-push", "merge ticket 1's pull request", "factoryd cancel " + id} {
		if !strings.Contains(loaded.NextAction(), remedy) {
			t.Errorf("next action %q does not name the remedy %q", loaded.NextAction(), remedy)
		}
	}

	earlierPROpenAt(dp, roundResult)
	if err := requestdriver.AdvanceBuilding(dp, context.Background(), dataDir, loaded, stackedCfg, runner, time.Now()); err != nil {
		t.Fatalf("advance once the predecessor branch is back: %v", err)
	}
	if len(built) != 2 || loaded.StackedWait != nil {
		t.Fatalf("built %d tickets, wait %+v; want ticket 2 built and the wait gone", len(built), loaded.StackedWait)
	}
}

// TestAdvanceBuildingParksWhenEarlierTicketBranchIsGone: ticket 1's pull
// request is open but its branch is gone from origin: the request parks in
// resume_review and ticket 2 is not built.
func TestAdvanceBuildingParksWhenEarlierTicketBranchIsGone(t *testing.T) {
	dp := newFakeDeps(t)
	dataDir, id := requestdrivertest.BuildingFixture(dp, t, 2)
	var built [][]string
	runner := acceptingStackedBuildRunner(t, dataDir, &built)
	r := buildFirstTicket(t, dp, dataDir, id, runner)
	earlierPROpenAt(dp, "")
	dp.remoteBranchHeadSHAFn = func(context.Context, string, string) (string, error) {
		return "", fmt.Errorf("%w: git ls-remote origin refs/heads/x", requestdriver.ErrRemoteBranchNotFound)
	}

	if err := requestdriver.AdvanceBuilding(dp, context.Background(), dataDir, r, stackedCfg, runner, time.Now()); err != nil {
		t.Fatalf("advance after predecessor branch disappeared: %v", err)
	}
	if len(built) != 1 {
		t.Fatalf("built %d tickets, want ticket 2 withheld", len(built))
	}
	if r.State != request.StateResumeReview || !r.Resume.Parked() || !strings.Contains(r.Resume.Reason, "is gone from origin") {
		t.Fatalf("state=%q resume=%+v, want parked with the branch-gone reason", r.State, r.Resume)
	}
}

// TestAdvanceBuildingParksWhenEarlierTicketPRClosed: ticket 1's pull request
// was closed without merging while ticket 2 waited to build: the request
// parks, and a resume after the operator reopened it checks again and builds
// ticket 2.
func TestAdvanceBuildingParksWhenEarlierTicketPRClosed(t *testing.T) {
	dp := newFakeDeps(t)
	dataDir, id := requestdrivertest.BuildingFixture(dp, t, 2)
	var built [][]string
	runner := acceptingStackedBuildRunner(t, dataDir, &built)
	r := buildFirstTicket(t, dp, dataDir, id, runner)
	first, err := run.Load(dataDir, r.Tickets[0].RunID)
	if err != nil {
		t.Fatal(err)
	}
	dp.readReviewStateFn = func(context.Context, string, forge.AuthorPolicy) (forge.ReviewState, error) {
		return forge.ReviewState{State: forge.PRStateClosed}, nil
	}

	if err := requestdriver.AdvanceBuilding(dp, context.Background(), dataDir, r, stackedCfg, runner, time.Now()); err != nil {
		t.Fatalf("advance after predecessor closed: %v", err)
	}
	if len(built) != 1 || r.State != request.StateResumeReview || !strings.Contains(r.Resume.Reason, "closed without merging") || r.Tickets[0].PRState != "closed" {
		t.Fatalf("built=%d state=%q resume=%+v ticket 1=%q, want parked on the closed pull request", len(built), r.State, r.Resume, r.Tickets[0].PRState)
	}
	if refusals, err := (requestdriver.ResumeGate{}).RequestResumeRefusals(context.Background(), dataDir, "", r); err != nil || len(refusals) != 0 {
		t.Fatalf("resume refusals = %v, %v; a park keeps nothing to check", refusals, err)
	}
	resumed, err := request.ResumeRequest(dataDir, id, request.ResumeRound, "alice", time.Now(), nil)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	earlierPROpenAt(dp, first.ResultSHA)
	if err := requestdriver.AdvanceBuilding(dp, context.Background(), dataDir, resumed, stackedCfg, runner, time.Now()); err != nil {
		t.Fatalf("advance after the operator reopened the pull request: %v", err)
	}
	if len(built) != 2 || resumed.Tickets[0].PRState != "draft" {
		t.Fatalf("built=%d ticket 1=%q, want ticket 2 built on the reopened draft pull request", len(built), resumed.Tickets[0].PRState)
	}
}

// TestAdvanceBuildingIgnoresDeletedBranchAfterEarlierTicketMerged: a merged
// ticket 1 (its branch auto-deleted) is no reason to wait or park: ticket 2
// builds, and the branch is not read.
func TestAdvanceBuildingIgnoresDeletedBranchAfterEarlierTicketMerged(t *testing.T) {
	dp := newFakeDeps(t)
	dataDir, id := requestdrivertest.BuildingFixture(dp, t, 2)
	var built [][]string
	runner := acceptingStackedBuildRunner(t, dataDir, &built)
	r := buildFirstTicket(t, dp, dataDir, id, runner)
	mergedAt := time.Now().UTC()
	dp.readReviewStateFn = func(context.Context, string, forge.AuthorPolicy) (forge.ReviewState, error) {
		return forge.ReviewState{State: forge.PRStateMerged, MergedAt: &mergedAt}, nil
	}

	if err := requestdriver.AdvanceBuilding(dp, context.Background(), dataDir, r, stackedCfg, runner, time.Now()); err != nil {
		t.Fatalf("advance after predecessor merge deleted its branch: %v", err)
	}
	if len(built) != 2 || r.State == request.StateResumeReview || r.Tickets[0].PRState != "merged" {
		t.Fatalf("built=%d state=%q predecessor=%q, want ticket 2 built and merged predecessor accepted", len(built), r.State, r.Tickets[0].PRState)
	}
}

func TestPRReviewCorrectiveRoundUsesCurrentPRHeadRun(t *testing.T) {
	dp := newFakeDeps(t)
	r, dataDir, _ := stackedRetargetFixture(dp, t)
	ticket := &r.Tickets[1]
	first, err := run.Load(dataDir, ticket.RunID)
	if err != nil {
		t.Fatal(err)
	}
	first.Branch = "factoryd/ticket-2-pr"
	first.DiffBaseSHA = "ticket-2-original-base"
	first.StackBaseSHA = "ticket-1-tip"
	if err := first.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	const currentHead = "ticket-2-round-head"
	const currentRoundID = "ticket-2-round-accepted"
	if err := (&run.Run{
		ID:           currentRoundID,
		Project:      "widget",
		State:        run.StateAccepted,
		Branch:       first.Branch,
		BaseSHA:      "ticket-2-round-base",
		DiffBaseSHA:  first.DiffBaseSHA,
		ResultSHA:    currentHead,
		StackBaseSHA: first.StackBaseSHA,
	}).Save(dataDir); err != nil {
		t.Fatal(err)
	}
	ticket.Rounds = append(ticket.Rounds, request.Round{Index: 1, RunID: currentRoundID, Outcome: request.RoundAccepted, Pushed: true})
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}

	threads := []forge.Thread{{ID: "thread-current-head", Path: "a.go", Line: 1, Author: "reviewer", Body: "fix this", CommentID: 1}}
	dp.readReviewStateFn = func(context.Context, string, forge.AuthorPolicy) (forge.ReviewState, error) {
		return forge.ReviewState{State: "OPEN", HeadSHA: currentHead, BlocksReadyThreads: threads, ActionableThreads: threads}, nil
	}
	dp.remoteBranchHeadSHAFn = func(context.Context, string, string) (string, error) {
		return currentHead, nil
	}
	dp.roundResultDescendsFromHeadFn = func(string, string, string) (bool, error) { return true, nil }
	dp.pushExistingBranchFn = func(context.Context, string, string, string) error { return nil }
	dp.replyToReviewCommentFn = func(context.Context, string, int64, string) error { return nil }

	origRunner := requestdriver.PrReviewCorrectiveRunner
	t.Cleanup(func() { requestdriver.PrReviewCorrectiveRunner = origRunner })
	var gotBranch, gotDiffBase, gotStackBase string
	requestdriver.PrReviewCorrectiveRunner = func(ctx context.Context, args []string, onReady func(*run.Run)) error {
		gotBranch = requestdrivertest.ArgValue(args, "-on-branch")
		gotDiffBase = requestdrivertest.ArgValue(args, "-diff-base")
		runID := requestdrivertest.ArgValue(args, "-ticket")
		started := &run.Run{ID: runID, State: run.StateReady, Branch: gotBranch}
		onReady(started)
		gotStackBase = started.StackBaseSHA
		return saveAcceptedRoundRun(t, dataDir, &run.Run{
			ID:            runID,
			Project:       "widget",
			State:         run.StateAccepted,
			WorkspacePath: t.TempDir(),
			Branch:        gotBranch,
			BaseSHA:       "new-round-base",
			DiffBaseSHA:   gotDiffBase,
			ResultSHA:     "new-round-result",
			StackBaseSHA:  started.StackBaseSHA,
		})
	}

	if err := requestdriver.AdvancePRReview(dp, context.Background(), dataDir, r, requestdriver.WorkerConfig{PrPollInterval: time.Minute, MaxReviewRounds: 2}, time.Now()); err != nil {
		t.Fatalf("advancePRReview: %v", err)
	}
	if gotBranch != first.Branch || gotDiffBase != first.DiffBaseSHA || gotStackBase != first.StackBaseSHA {
		t.Fatalf("corrective round inputs branch=%q diff-base=%q stack-base=%q, want %q/%q/%q from current PR-head run", gotBranch, gotDiffBase, gotStackBase, first.Branch, first.DiffBaseSHA, first.StackBaseSHA)
	}
}

func TestApprovalOnOlderCommitDoesNotAdvancePRReview(t *testing.T) {
	dp := newFakeDeps(t)
	r, dataDir := requestdrivertest.StubPRReviewTestFixture(t, 1)
	checks := true
	stubPRReviewDeps(dp, t, forge.ReviewState{
		State:          forge.PRStateOpen,
		ChecksPassing:  &checks,
		HeadSHA:        "new-head",
		ReviewDecision: forge.ReviewDecisionApproved,
		Reviews:        []forge.Review{{Author: "reviewer", CommitOID: "old-head", State: "APPROVED"}},
	}, nil)
	called := 0
	requestdriver.ContinueAfterPRApproval = func(string, *request.Request, time.Time) error {
		called++
		return nil
	}

	if err := requestdriver.AdvancePRReview(dp, context.Background(), dataDir, r, requestdriver.WorkerConfig{PrPollInterval: time.Minute}, time.Now()); err != nil {
		t.Fatalf("advancePRReview: %v", err)
	}
	if called != 0 || r.Tickets[0].PRState == "approved" {
		t.Fatalf("approval on old commit advanced: calls=%d PRState=%q", called, r.Tickets[0].PRState)
	}
}

func TestClosedUnmergedEarlierTicketParksRequest(t *testing.T) {
	dp := newFakeDeps(t)
	r, dataDir := requestdrivertest.StubPRReviewTestFixture(t, 2)
	r.TicketIndex = 2
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	dp.readReviewStateFn = func(_ context.Context, prURL string, _ forge.AuthorPolicy) (forge.ReviewState, error) {
		if strings.HasSuffix(prURL, "/101") {
			return forge.ReviewState{State: forge.PRStateClosed}, nil
		}
		return forge.ReviewState{State: forge.PRStateOpen}, nil
	}

	if err := requestdriver.AdvancePRReview(dp, context.Background(), dataDir, r, requestdriver.WorkerConfig{PrPollInterval: time.Minute}, time.Now()); err != nil {
		t.Fatalf("advancePRReview: %v", err)
	}
	if r.State != request.StateResumeReview || r.Resume == nil || !strings.Contains(r.Resume.Reason, "closed without merging") {
		t.Fatalf("state=%q resume=%+v, want parked with closed-unmerged reason", r.State, r.Resume)
	}
	if action := r.NextAction(); !strings.Contains(action, "factoryd resume "+r.ID) || !strings.Contains(action, "factoryd cancel "+r.ID) {
		t.Fatalf("NextAction=%q, want resume and cancel choices", action)
	}

	// The operator reopens it and resumes: the poll reads it open again and
	// handles it as the draft it is.
	resumed, err := request.ResumeRequest(dataDir, r.ID, request.ResumeRound, "alice", time.Now(), nil)
	if err != nil || resumed.State != request.StatePRReview {
		t.Fatalf("resume: state %q, %v; want pr_review", resumed.State, err)
	}
	dp.readReviewStateFn = func(context.Context, string, forge.AuthorPolicy) (forge.ReviewState, error) {
		return forge.ReviewState{State: forge.PRStateOpen, IsDraft: true}, nil
	}
	if err := requestdriver.AdvancePRReview(dp, context.Background(), dataDir, resumed, requestdriver.WorkerConfig{PrPollInterval: time.Minute}, time.Now().Add(2*time.Minute)); err != nil {
		t.Fatalf("advancePRReview after resume: %v", err)
	}
	if resumed.State != request.StatePRReview || resumed.Tickets[0].PRState != "draft" {
		t.Fatalf("after resume: state %q, ticket 1 %q; want pr_review with a draft pull request", resumed.State, resumed.Tickets[0].PRState)
	}
}

// reviewRoundOn runs one PR-review round on ticket 2 of stackedRetargetFixture
// whose rounds are rounds and whose current PR-head run is head (saved), and
// returns the round's -on-branch and -diff-base and the branch its result was
// pushed to; ran is false when no round build started. Ticket 2's pull
// request is on factoryd/ticket-2-pr.
func reviewRoundOn(t *testing.T, head *run.Run, rounds []request.Round, maxRounds int) (branch, diffBase, pushed string, ran bool) {
	t.Helper()
	dp := newFakeDeps(t)
	r, dataDir, _ := stackedRetargetFixture(dp, t)
	ticket := &r.Tickets[1]
	ticket.Branch = "factoryd/ticket-2-pr"
	head.Project, head.State = "widget", run.StateAccepted
	if err := head.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	ticket.Rounds = append(rounds, request.Round{Index: len(rounds) + 1, Kind: request.RestackRoundKind, RunID: head.ID, Outcome: request.RoundAccepted, Pushed: true})
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	threads := []forge.Thread{{ID: "thread-1", Path: "a.go", Line: 1, Author: "reviewer", Body: "fix this", CommentID: 1}}
	dp.readReviewStateFn = func(context.Context, string, forge.AuthorPolicy) (forge.ReviewState, error) {
		return forge.ReviewState{State: "OPEN", HeadSHA: head.ResultSHA, BlocksReadyThreads: threads, ActionableThreads: threads}, nil
	}
	dp.remoteBranchHeadSHAFn = func(context.Context, string, string) (string, error) { return head.ResultSHA, nil }
	dp.roundResultDescendsFromHeadFn = func(string, string, string) (bool, error) { return true, nil }
	dp.pushExistingBranchFn = func(_ context.Context, _, _, to string) error {
		pushed = to
		return nil
	}
	dp.replyToReviewCommentFn = func(context.Context, string, int64, string) error { return nil }
	origRunner := requestdriver.PrReviewCorrectiveRunner
	t.Cleanup(func() { requestdriver.PrReviewCorrectiveRunner = origRunner })
	requestdriver.PrReviewCorrectiveRunner = func(ctx context.Context, args []string, onReady func(*run.Run)) error {
		ran = true
		branch, diffBase = requestdrivertest.ArgValue(args, "-on-branch"), requestdrivertest.ArgValue(args, "-diff-base")
		runID := requestdrivertest.ArgValue(args, "-ticket")
		onReady(&run.Run{ID: runID, State: run.StateReady, Branch: branch})
		return saveAcceptedRoundRun(t, dataDir, &run.Run{ID: runID, Project: "widget", State: run.StateAccepted, WorkspacePath: t.TempDir(), Branch: branch, BaseSHA: head.ResultSHA, DiffBaseSHA: diffBase, ResultSHA: strings.Repeat("e", 40)})
	}
	if err := requestdriver.AdvancePRReview(dp, context.Background(), dataDir, r, requestdriver.WorkerConfig{PrPollInterval: time.Minute, MaxReviewRounds: maxRounds}, time.Now()); err != nil {
		t.Fatalf("advancePRReview: %v", err)
	}
	return branch, diffBase, pushed, ran
}

// replayHeadRun is an accepted replay run of ticket 2 onto onto, judged on
// its local restack branch.
func replayHeadRun(onto string) *run.Run {
	head := strings.Repeat("c", 40)
	return &run.Run{ID: "ticket-2-restack1", Branch: "buildgate/req-1/t2-restack1", BaseSHA: head, ResultSHA: head, DiffBaseSHA: onto, StackBaseSHA: onto, Replay: &run.Replay{OfRunID: "run-2", SourceRunIDs: []string{"run-2"}, OntoSHA: onto, HeadSHA: head}}
}

// TestCurrentPRHeadRunIDFollowsAPushedRestackRound: a restack round becomes
// the pull request's head run once its push is confirmed, like any round.
func TestCurrentPRHeadRunIDFollowsAPushedRestackRound(t *testing.T) {
	ticket := &request.Ticket{RunID: "run-2", Rounds: []request.Round{{Index: 1, Kind: request.RestackRoundKind, RunID: "run-2-restack1", Outcome: request.RoundAccepted}}}
	if got := ticket.PRHeadRunID(); got != "run-2" {
		t.Fatalf("head run before the push is confirmed = %q, want run-2", got)
	}
	ticket.Rounds[0].Pushed = true
	if got := ticket.PRHeadRunID(); got != "run-2-restack1" {
		t.Fatalf("head run after the push = %q, want the restack round's run", got)
	}
}

// TestPRReviewRoundAfterAReplayRunUsesItsOnto: a review round on a ticket
// whose PR-head run is a replay run builds on the ticket's pull-request
// branch, not the replay run's local restack branch, pushes its result
// there, and measures from the commit the replay is onto.
func TestPRReviewRoundAfterAReplayRunUsesItsOnto(t *testing.T) {
	onto := strings.Repeat("a", 40)
	branch, diffBase, pushed, ran := reviewRoundOn(t, replayHeadRun(onto), nil, 3)
	if !ran || branch != "factoryd/ticket-2-pr" || diffBase != onto {
		t.Fatalf("round ran=%v on %q from %q, want factoryd/ticket-2-pr from %s", ran, branch, diffBase, onto)
	}
	if pushed != "factoryd/ticket-2-pr" {
		t.Fatalf("round pushed to %q, want the ticket's pull-request branch factoryd/ticket-2-pr", pushed)
	}
}

// TestRestackRoundsUseNoRoundBudget: restack rounds, however many, leave the
// review-round budget whole: with max_review_rounds 1 a review round still
// runs after two of them.
func TestRestackRoundsUseNoRoundBudget(t *testing.T) {
	earlier := []request.Round{{Index: 1, Kind: request.RestackRoundKind, RunID: "ticket-2-restack0", Outcome: request.RoundAccepted, Pushed: true}}
	if _, _, _, ran := reviewRoundOn(t, replayHeadRun(strings.Repeat("a", 40)), earlier, 1); !ran {
		t.Fatal("no review round ran: restack rounds used the review-round budget")
	}
}

// TestStackedWaitOnAnUnreadablePRNamesTheRemedy: a pull request gh cannot
// read is a wait whose sentence says to check gh's login.
func TestStackedWaitOnAnUnreadablePRNamesTheRemedy(t *testing.T) {
	dp := newFakeDeps(t)
	dataDir, id := requestdrivertest.BuildingFixture(dp, t, 2)
	var built [][]string
	runner := acceptingStackedBuildRunner(t, dataDir, &built)
	r := buildFirstTicket(t, dp, dataDir, id, runner)
	dp.readReviewStateFn = func(context.Context, string, forge.AuthorPolicy) (forge.ReviewState, error) {
		return forge.ReviewState{}, errors.New("gh: not logged in")
	}
	if err := requestdriver.AdvanceBuilding(dp, context.Background(), dataDir, r, stackedCfg, runner, time.Now()); err != nil {
		t.Fatal(err)
	}
	if len(built) != 1 || r.StackedWait == nil || !strings.Contains(r.NextAction(), "gh auth status") || !strings.Contains(r.NextAction(), "not logged in") {
		t.Fatalf("built=%d next action=%q, want ticket 2 withheld and the gh remedy named", len(built), r.NextAction())
	}
}

// TestStackedWaitRemindsAfterTheReminderInterval: a stacked wait is reminded
// like a review gate: not when it begins, then once each reminder interval,
// naming what to fix; a wait that ends drops its reminder state.
func TestStackedWaitRemindsAfterTheReminderInterval(t *testing.T) {
	dp := newFakeDeps(t)
	dataDir, id := requestdrivertest.BuildingFixture(dp, t, 2)
	var built [][]string
	runner := acceptingStackedBuildRunner(t, dataDir, &built)
	r := buildFirstTicket(t, dp, dataDir, id, runner)
	cfg := stackedCfg
	cfg.HitlReminderInterval = 15 * time.Minute
	earlierPROpenAt(dp, "some-direct-push")
	start := time.Now()
	for _, step := range []struct {
		after time.Duration
		want  int
	}{{0, 0}, {10 * time.Minute, 0}, {15 * time.Minute, 1}, {20 * time.Minute, 1}, {31 * time.Minute, 2}} {
		if err := requestdriver.AdvanceBuilding(dp, context.Background(), dataDir, r, cfg, runner, start.Add(step.after)); err != nil {
			t.Fatal(err)
		}
		if r.NotifyCount != step.want {
			t.Fatalf("after %s: %d reminder(s), want %d", step.after, r.NotifyCount, step.want)
		}
	}
	if !strings.Contains(r.LastAsk, "stacked ticket") {
		t.Errorf("last ask = %q, want the stacked-ticket reminder", r.LastAsk)
	}
	log, err := os.ReadFile(requestdriver.RequestNotificationLogPath(dataDir, id))
	if err != nil || !strings.Contains(string(log), "move the branch back") {
		t.Errorf("notification log = %q, %v; want the reminder to name the remedy", log, err)
	}
	first, err := run.Load(dataDir, r.Tickets[0].RunID)
	if err != nil {
		t.Fatal(err)
	}
	earlierPROpenAt(dp, first.ResultSHA)
	if err := requestdriver.AdvanceBuilding(dp, context.Background(), dataDir, r, cfg, runner, start.Add(40*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if len(built) != 2 || r.StackedWait != nil {
		t.Fatalf("built=%d wait=%+v, want ticket 2 built and the wait over", len(built), r.StackedWait)
	}
}

// threeTicketsBuildingTicket3 builds tickets 1 and 2 of a three-ticket
// request, each pull request open at its own run's commit, and returns the
// request building ticket 3 and each ticket's pull-request URL's state as
// the fake reads it (open at its head unless a test changes it).
func threeTicketsBuildingTicket3(t *testing.T, dp *fakeDeps) (r *request.Request, dataDir string, states map[string]forge.ReviewState, built *[][]string, runner requestdriver.TicketRunner) {
	t.Helper()
	dataDir, id := requestdrivertest.BuildingFixture(dp, t, 3)
	built = new([][]string)
	runner = acceptingStackedBuildRunner(t, dataDir, built)
	states = map[string]forge.ReviewState{}
	dp.readReviewStateFn = func(_ context.Context, prURL string, _ forge.AuthorPolicy) (forge.ReviewState, error) {
		if st, ok := states[prURL]; ok {
			return st, nil
		}
		return forge.ReviewState{State: forge.PRStateOpen, IsDraft: true}, nil
	}
	dp.remoteBranchHeadSHAFn = func(_ context.Context, _, branch string) (string, error) {
		return "result-" + strings.TrimPrefix(branch, "factoryd/"), nil
	}
	r = buildFirstTicket(t, dp, dataDir, id, runner)
	if err := requestdriver.AdvanceBuilding(dp, context.Background(), dataDir, r, stackedCfg, runner, time.Now()); err != nil {
		t.Fatalf("build ticket 2: %v", err)
	}
	if r.TicketIndex != 3 || len(*built) != 2 {
		t.Fatalf("after ticket 2: index=%d built=%d, want building ticket 3", r.TicketIndex, len(*built))
	}
	return r, dataDir, states, built, runner
}

// TestEveryEarlierTicketIsChecked: ticket 3 sits on tickets 1 and 2; ticket
// 1's pull request closed without merging, or its branch gone, parks the
// request though ticket 2's is fine.
func TestEveryEarlierTicketIsChecked(t *testing.T) {
	t.Run("ticket 1 closed", func(t *testing.T) {
		dp := newFakeDeps(t)
		r, dataDir, states, built, runner := threeTicketsBuildingTicket3(t, dp)
		states[r.Tickets[0].PRURL] = forge.ReviewState{State: forge.PRStateClosed}
		if err := requestdriver.AdvanceBuilding(dp, context.Background(), dataDir, r, stackedCfg, runner, time.Now()); err != nil {
			t.Fatal(err)
		}
		if len(*built) != 2 || r.State != request.StateResumeReview || !strings.Contains(r.Resume.Reason, "ticket 1/3's pull request") {
			t.Fatalf("built=%d state=%q resume=%+v, want parked on ticket 1's closed pull request", len(*built), r.State, r.Resume)
		}
	})
	t.Run("ticket 1's branch gone", func(t *testing.T) {
		dp := newFakeDeps(t)
		r, dataDir, _, built, runner := threeTicketsBuildingTicket3(t, dp)
		gone := r.Tickets[0].Branch
		dp.remoteBranchHeadSHAFn = func(_ context.Context, _, branch string) (string, error) {
			if branch == gone {
				return "", fmt.Errorf("%w: %s", requestdriver.ErrRemoteBranchNotFound, branch)
			}
			return "result-" + strings.TrimPrefix(branch, "factoryd/"), nil
		}
		if err := requestdriver.AdvanceBuilding(dp, context.Background(), dataDir, r, stackedCfg, runner, time.Now()); err != nil {
			t.Fatal(err)
		}
		if len(*built) != 2 || r.State != request.StateResumeReview || !strings.Contains(r.Resume.Reason, gone+" is gone from origin") {
			t.Fatalf("built=%d state=%q resume=%+v, want parked on ticket 1's missing branch", len(*built), r.State, r.Resume)
		}
	})
	t.Run("all fine", func(t *testing.T) {
		dp := newFakeDeps(t)
		r, dataDir, _, built, runner := threeTicketsBuildingTicket3(t, dp)
		if err := requestdriver.AdvanceBuilding(dp, context.Background(), dataDir, r, stackedCfg, runner, time.Now()); err != nil {
			t.Fatal(err)
		}
		if len(*built) != 3 {
			t.Fatalf("built=%d state=%q, want ticket 3 built", len(*built), r.State)
		}
	})
}

// TestStackedParkKeepsTheLostBuildToResume: the operator chose to continue
// ticket 2's lost build in its kept worktree, but ticket 1's pull request was
// closed meanwhile: the park keeps the lost run, says so, and the resume
// after the pull request is reopened continues that build, not a new one.
func TestStackedParkKeepsTheLostBuildToResume(t *testing.T) {
	dp := newFakeDeps(t)
	dataDir, id := requestdrivertest.BuildingFixture(dp, t, 2)
	var built [][]string
	runner := acceptingStackedBuildRunner(t, dataDir, &built)
	r := buildFirstTicket(t, dp, dataDir, id, runner)
	const lost = "req-1-002"
	if err := requestdriver.EnterResumeReview(dp, dataDir, r, request.LostStep{RunID: lost, Cause: unresolved.WorkerLost}, time.Now()); err != nil {
		t.Fatal(err)
	}
	resumed, err := request.ResumeRequest(dataDir, id, request.ResumeRound, "alice", time.Now(), nil)
	if err != nil {
		t.Fatal(err)
	}
	dp.readReviewStateFn = func(context.Context, string, forge.AuthorPolicy) (forge.ReviewState, error) {
		return forge.ReviewState{State: forge.PRStateClosed}, nil
	}
	cfg := stackedCfg
	cfg.Resume = requestdriver.ResumeGate{Preconditions: &requestdrivertest.FakeResumePreconditions{T: t, OK: true}}
	if err := requestdriver.AdvanceBuilding(dp, context.Background(), dataDir, resumed, cfg, runner, time.Now()); err != nil {
		t.Fatal(err)
	}
	if len(built) != 1 || resumed.State != request.StateResumeReview || !resumed.Resume.Parked() || resumed.Resume.LostRunID != lost || resumed.Resume.Cause != unresolved.WorkerLost {
		t.Fatalf("built=%d state=%q resume=%+v, want parked keeping lost run %s", len(built), resumed.State, resumed.Resume, lost)
	}
	if !strings.Contains(resumed.NextAction(), "continues the kept build of run "+lost) {
		t.Errorf("next action %q does not say the resume continues run %s", resumed.NextAction(), lost)
	}
	again, err := request.ResumeRequest(dataDir, id, request.ResumeRound, "alice", time.Now(), nil)
	if err != nil {
		t.Fatal(err)
	}
	first, err := run.Load(dataDir, again.Tickets[0].RunID)
	if err != nil {
		t.Fatal(err)
	}
	earlierPROpenAt(dp, first.ResultSHA)
	if err := requestdriver.AdvanceBuilding(dp, context.Background(), dataDir, again, cfg, runner, time.Now()); err != nil {
		t.Fatal(err)
	}
	if len(built) != 2 || requestdrivertest.ArgValue(built[1], "-resume-worktree-of") != lost {
		t.Fatalf("built=%d args=%v, want ticket 2's lost build %s continued", len(built), built[len(built)-1], lost)
	}
}
