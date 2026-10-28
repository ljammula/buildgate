package requestdriver_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"buildgate/internal/forge"
	"buildgate/internal/request"
	"buildgate/internal/requestdriver"
	"buildgate/internal/requestdriver/requestdrivertest"
	"buildgate/internal/run"
	"buildgate/internal/unresolved"
)

// Every driver path into resume_review records its first reminder on the
// request as it is on disk: an operator's `factoryd resume` that lands
// between the review save and the reminder survives. These are the paths
// that hold no lock; the worker's own, which hold it, cannot be written
// between their saves.

func TestAResumeDecidedAfterAHaltedTicketRunEntersResumeReviewIsKept(t *testing.T) {
	dp := newFakeDeps(t)
	dataDir, id := requestdrivertest.BuildingFixture(dp, t, 1)
	after, decided := requestdriver.OperatorResumesBetweenTheSaves(t, id)
	dp.afterReviewSave(after)
	runner := func(_ context.Context, args []string, onReady func(*run.Run)) error {
		rid := requestdrivertest.ArgValue(args, "-ticket")
		onReady(&run.Run{ID: rid, State: run.StateReady})
		return (&run.Run{ID: rid, State: run.StateHalted, HaltError: "heartbeat lost", KeptForResume: true}).Save(dataDir)
	}
	requestdrivertest.AdvanceBuildingOnce(dp, t, dataDir, id, requestdriver.ResumeGate{}, runner)
	requestdriver.RequireOperatorResumeKept(t, dataDir, id, decided, request.StateBuilding)
}

func TestAResumeDecidedAfterAnUnresolvedCorrectiveRoundIsKept(t *testing.T) {
	dp := newFakeDeps(t)
	dataDir, id := requestdrivertest.BuildingFixture(dp, t, 1)
	after, decided := requestdriver.OperatorResumesBetweenTheSaves(t, id)
	dp.afterReviewSave(after)
	branch := "factoryd/" + id + "-001"
	buildRunner := checkQuarantinedBuildRunner(t, dataDir, branch, fmt.Sprintf("%040d", 1), fmt.Sprintf("%040d", 2), "repo-lint")
	requestdrivertest.StubReviewCorrectiveRunner(t, dataDir, func(_, roundRunID string) *run.Run {
		return unresolvedHalts(roundRunID)[unresolved.SandboxLaunch]
	})
	cfg := requestdriver.WorkerConfig{ReviewCorrectiveRounds: 1, OpenPullRequest: true}
	if err := requestdrivertest.DriveRequests(dp, context.Background(), dataDir, cfg, requestdrivertest.FailingSpecDraftRunner(t), requestdrivertest.FailingPlanTicketsRunner(t), buildRunner); err != nil {
		t.Fatalf("driveRequests: %v", err)
	}
	requestdriver.RequireOperatorResumeKept(t, dataDir, id, decided, request.StateBuilding)
}

func TestAResumeDecidedAfterAnUnresolvedPRReviewRoundIsKept(t *testing.T) {
	dp := newFakeDeps(t)
	r, dataDir := requestdrivertest.StubPRReviewTestFixture(t, 1)
	after, decided := requestdriver.OperatorResumesBetweenTheSaves(t, r.ID)
	dp.afterReviewSave(after)
	threads := []forge.Thread{{ID: "thread-1", Path: "a.go", Line: 1, Author: "dave", Body: "needs work", CommentID: 444}}
	dp.replyToReviewCommentFn = func(context.Context, string, int64, string) error { return nil }
	requestdriver.PrReviewCorrectiveRunner = func(_ context.Context, args []string, onReady func(*run.Run)) error {
		halted := unresolvedHalts(requestdrivertest.ArgValue(args, "-ticket"))[unresolved.SandboxLaunch]
		onReady(&run.Run{ID: halted.ID, State: run.StateReady})
		return halted.Save(dataDir)
	}
	t.Cleanup(func() { requestdriver.PrReviewCorrectiveRunner = nil })
	if err := requestdriver.RunCorrectiveRound(dp, context.Background(), dataDir, r, &r.Tickets[0], threads, requestdriver.WorkerConfig{MaxReviewRounds: 1}, time.Now()); err != nil {
		t.Fatalf("RunCorrectiveRound: %v", err)
	}
	requestdriver.RequireOperatorResumeKept(t, dataDir, r.ID, decided, request.StatePRReview)
	if r.State != request.StatePRReview {
		t.Errorf("the driver's copy is in %s, want the record as saved", r.State)
	}
}
