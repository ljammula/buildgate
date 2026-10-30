package requestdriver_test

import (
	"buildgate/internal/forge"
	"buildgate/internal/request"
	"buildgate/internal/requestdriver"
	"buildgate/internal/requestdriver/requestdrivertest"
	"buildgate/internal/run"
	"buildgate/internal/unresolved"
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// unresolvedHalts are the ways a run's record says its halt is no verdict on
// the work, for every cause the factory can record on a branch round (which
// keeps no worktree).
func unresolvedHalts(id string) map[unresolved.Cause]*run.Run {
	return map[unresolved.Cause]*run.Run{
		unresolved.SandboxLaunch: {ID: id, State: run.StateHalted, HaltReasonCode: string(unresolved.SandboxLaunch), HaltError: "sandbox attempts exhausted: create sandbox: gateway unreachable", Attempts: []run.Attempt{{Kind: "build", ExitCode: -1}}},
		unresolved.WorkerLost:    {ID: id, State: run.StateHalted, HaltReasonCode: string(unresolved.WorkerLost), HaltError: "activity error: heartbeat timeout", Attempts: []run.Attempt{{Kind: "build", ExitCode: -1}}},
	}
}

// A ticket build whose sandbox failed to launch says nothing about the work:
// the request waits in resume_review with the cause, not in halted, where
// `retry` would delete the kept worktree.
func TestAdvanceBuildingAKeptRunCarriesItsCauseIntoResumeReview(t *testing.T) {
	dp := newFakeDeps(t)
	dataDir, id := requestdrivertest.BuildingFixture(dp, t, 1)
	runner := func(_ context.Context, args []string, onReady func(*run.Run)) error {
		rid := requestdrivertest.ArgValue(args, "-ticket")
		onReady(&run.Run{ID: rid, State: run.StateReady})
		return (&run.Run{ID: rid, State: run.StateHalted, KeptForResume: true, HaltReasonCode: string(unresolved.SandboxLaunch), HaltError: "create sandbox: gateway unreachable"}).Save(dataDir)
	}
	got := requestdrivertest.AdvanceBuildingOnce(dp, t, dataDir, id, requestdriver.ResumeGate{}, runner)
	if got.State != request.StateResumeReview || got.Resume == nil || got.Resume.Cause != unresolved.SandboxLaunch {
		t.Fatalf("state %s resume %+v, want resume_review with the launch failure as its cause", got.State, got.Resume)
	}
	if strings.Contains(got.Error, "worker stopped") || !strings.Contains(got.Error, "could not be started") || !strings.Contains(got.Error, "factoryd resume "+id) {
		t.Errorf("Error = %q, want the launch failure named and the one command", got.Error)
	}
	if next := got.NextAction(); !strings.Contains(next, "could not be started") || !strings.Contains(next, "kept worktree") {
		t.Errorf("NextAction = %q, want the cause and the kept worktree", next)
	}
}

// A corrective round that ended with no verdict used none of the budget and
// is no quarantine: the request waits for a human, and the sentence says the
// only thing left to do, since a branch round keeps no worktree.
func TestAnUnresolvedCorrectiveRoundIsNotCountedAndWaitsInResumeReview(t *testing.T) {
	for cause := range unresolvedHalts("") {
		t.Run(string(cause), func(t *testing.T) {
			dp := newFakeDeps(t)
			dataDir, id := requestdrivertest.BuildingFixture(dp, t, 1)
			branch := "factoryd/" + id + "-001"
			buildRunner := checkQuarantinedBuildRunner(t, dataDir, branch, fmt.Sprintf("%040d", 1), fmt.Sprintf("%040d", 2), "repo-lint")
			calls, _ := requestdrivertest.StubReviewCorrectiveRunner(t, dataDir, func(_, roundRunID string) *run.Run {
				return unresolvedHalts(roundRunID)[cause]
			})
			cfg := requestdriver.WorkerConfig{ReviewCorrectiveRounds: 1, OpenPullRequest: true}
			if err := requestdrivertest.DriveRequests(dp, context.Background(), dataDir, cfg, requestdrivertest.FailingSpecDraftRunner(t), requestdrivertest.FailingPlanTicketsRunner(t), buildRunner); err != nil {
				t.Fatalf("driveRequests: %v", err)
			}
			if *calls != 1 {
				t.Fatalf("corrective runner calls = %d, want 1: nothing reruns on its own", *calls)
			}
			loaded, err := request.Load(dataDir, id)
			if err != nil {
				t.Fatal(err)
			}
			if loaded.State != request.StateResumeReview || loaded.Resume == nil || loaded.Resume.Cause != cause || loaded.Resume.FromState != request.StateBuilding {
				t.Fatalf("state %s resume %+v (%s), want resume_review from building with cause %s", loaded.State, loaded.Resume, loaded.Error, cause)
			}
			if loaded.QuarantineCheck != "" {
				t.Errorf("QuarantineCheck = %q: no check judged anything", loaded.QuarantineCheck)
			}
			rounds := loaded.Tickets[0].Rounds
			if len(rounds) != 1 || rounds[0].Unresolved != cause || rounds[0].Counted() || rounds[0].Outcome != request.RoundHalted {
				t.Fatalf("Rounds = %+v, want one halted, uncounted round carrying the cause", rounds)
			}
			if next := loaded.NextAction(); !strings.Contains(next, "factoryd resume -from scratch "+id) || strings.Contains(next, "kept worktree") {
				t.Errorf("NextAction = %q, want only the rebuild and cancel: nothing was kept", next)
			}
		})
	}
}

// A PR-review round with no verdict uses no max_review_rounds slot, and the
// next poll does not start another on its own: with the provider or the
// gateway down that would repeat without end.
func TestAnUnresolvedPRReviewRoundIsNotCountedAndWaitsInResumeReview(t *testing.T) {
	dp := newFakeDeps(t)
	r, dataDir := requestdrivertest.StubPRReviewTestFixture(t, 1)
	ticket := &r.Tickets[0]
	threads := []forge.Thread{{ID: "thread-1", Path: "a.go", Line: 1, Author: "dave", Body: "needs work", CommentID: 444}}
	replies := 0
	dp.replyToReviewCommentFn = func(context.Context, string, int64, string) error { replies++; return nil }
	requestdriver.PrReviewCorrectiveRunner = func(_ context.Context, args []string, onReady func(*run.Run)) error {
		halted := unresolvedHalts(requestdrivertest.ArgValue(args, "-ticket"))[unresolved.SandboxLaunch]
		onReady(&run.Run{ID: halted.ID, State: run.StateReady})
		return halted.Save(dataDir)
	}
	cfg := requestdriver.WorkerConfig{MaxReviewRounds: 1}
	if err := requestdriver.RunCorrectiveRound(dp, context.Background(), dataDir, r, ticket, threads, cfg, time.Now()); err != nil {
		t.Fatalf("RunCorrectiveRound: %v", err)
	}
	if r.State != request.StateResumeReview || r.Resume == nil || r.Resume.Cause != unresolved.SandboxLaunch || r.Resume.FromState != request.StatePRReview {
		t.Fatalf("state %s resume %+v, want resume_review from pr_review with the launch failure", r.State, r.Resume)
	}
	if len(ticket.Rounds) != 1 || ticket.Rounds[0].Counted() || ticket.Rounds[0].Unresolved != unresolved.SandboxLaunch {
		t.Fatalf("Rounds = %+v, want one uncounted round carrying the cause", ticket.Rounds)
	}
	if replies != 0 {
		t.Errorf("the reviewer was told about a round that judged nothing (%d replies)", replies)
	}
	// The operator resumes: the request is back in pr_review, and the one
	// round max_review_rounds allows is still there to use.
	if _, err := r.ResumeDecide(request.ResumeRound, "operator", time.Now()); err != nil {
		t.Fatalf("ResumeDecide: %v", err)
	}
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	requestdriver.PrReviewCorrectiveRunner = func(_ context.Context, args []string, onReady func(*run.Run)) error {
		id := requestdrivertest.ArgValue(args, "-ticket")
		onReady(&run.Run{ID: id, State: run.StateReady})
		return (&run.Run{ID: id, State: run.StateQuarantined, GateResults: []run.GateResult{{Check: "canonical_verify"}}}).Save(dataDir)
	}
	if err := requestdriver.RunCorrectiveRound(dp, context.Background(), dataDir, r, ticket, threads, cfg, time.Now()); err != nil {
		t.Fatalf("RunCorrectiveRound after the resume: %v", err)
	}
	if r.State != request.StatePRReview || len(ticket.Rounds) != 2 || !ticket.Rounds[1].Counted() {
		t.Fatalf("state %s (%s), rounds %+v: want the real round run and counted, not `review rounds exhausted`", r.State, r.Error, ticket.Rounds)
	}
}

// A build that continues on an earlier attempt's branch keeps no worktree.
// Unresolved, it still waits for a human with its cause, and is offered only
// what can be done: before, it fell to `halted` with the bare code.
func TestAdvanceBuildingAnUnresolvedRunWithNothingKeptStillWaitsInResumeReview(t *testing.T) {
	dp := newFakeDeps(t)
	dataDir, id := requestdrivertest.BuildingFixture(dp, t, 1)
	runner := func(_ context.Context, args []string, onReady func(*run.Run)) error {
		halted := unresolvedHalts(requestdrivertest.ArgValue(args, "-ticket"))[unresolved.SandboxLaunch]
		onReady(&run.Run{ID: halted.ID, State: run.StateReady})
		return halted.Save(dataDir)
	}
	got := requestdrivertest.AdvanceBuildingOnce(dp, t, dataDir, id, requestdriver.ResumeGate{}, runner)
	if got.State != request.StateResumeReview || got.Resume == nil || got.Resume.Cause != unresolved.SandboxLaunch || got.Resume.LostRunID != "" {
		t.Fatalf("state %s resume %+v (%s), want resume_review with the cause and no kept run", got.State, got.Resume, got.Error)
	}
	for _, text := range []string{got.Error, got.NextAction()} {
		if !strings.Contains(text, "factoryd resume -from scratch "+id) || strings.Contains(text, "continues it") {
			t.Errorf("%q: want only the rebuild offered, nothing was kept", text)
		}
	}
}
