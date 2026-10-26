package requestdriver_test

import (
	"context"
	"errors"
	"fmt"
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

// Every driver path into resume_review records its first reminder on the
// request as it is on disk: an operator's `factoryd resume` that lands
// between the review save and the reminder survives. These are the paths
// that hold no lock; the worker's own, which hold it, cannot be written
// between their saves.

func TestAResumeDecidedAfterAHaltedTicketRunEntersResumeReviewIsKept(t *testing.T) {
	dp := newFakeDeps(t)
	dataDir, id := requestdrivertest.BuildingFixture(dp, t, 1)
	decided := requestdriver.OperatorResumesBetweenTheSaves(t, id)
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
	decided := requestdriver.OperatorResumesBetweenTheSaves(t, id)
	branch := "factoryd/" + id + "-001"
	buildRunner := checkQuarantinedBuildRunner(t, dataDir, branch, fmt.Sprintf("%040d", 1), fmt.Sprintf("%040d", 2), "lint")
	requestdrivertest.StubReviewCorrectiveRunner(t, dataDir, func(_, roundRunID string) *run.Run {
		return unresolvedHalts(roundRunID)[unresolved.SandboxLaunch]
	})
	cfg := requestdriver.WorkerConfig{ReviewCorrectiveRounds: 1, OpenPullRequest: true}
	if err := requestdrivertest.DriveRequests(dp, context.Background(), dataDir, cfg, requestdrivertest.FailingSpecDraftRunner(t), requestdrivertest.FailingPlanTicketsRunner(t), requestdrivertest.FailingOracleDraftRunner(t), buildRunner); err != nil {
		t.Fatalf("driveRequests: %v", err)
	}
	requestdriver.RequireOperatorResumeKept(t, dataDir, id, decided, request.StateBuilding)
}

func TestAResumeDecidedAfterAnUnresolvedPRReviewRoundIsKept(t *testing.T) {
	dp := newFakeDeps(t)
	r, dataDir := requestdrivertest.StubPRReviewTestFixture(t, 1)
	decided := requestdriver.OperatorResumesBetweenTheSaves(t, r.ID)
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

func TestAResumeDecidedAfterAnOracleDraftingJobFailureIsKept(t *testing.T) {
	dp := newFakeDeps(t)
	dataDir, id := requestdrivertest.OracleStageFixture(t, true)
	decided := requestdriver.OperatorResumesBetweenTheSaves(t, id)
	failure := &requestdriver.OracleDraftJobFailure{Cause: unresolved.SandboxLaunch, Err: errors.New("the gateway refused the sandbox")}
	runner, _ := requestdrivertest.StubOracleDraftRunner(request.OracleDraft{}, fmt.Errorf("oracle job: %w", failure))
	if err := driveOracleDrafting(t, dp, context.Background(), dataDir, runner); err != nil {
		t.Fatal(err)
	}
	requestdriver.RequireOperatorResumeKept(t, dataDir, id, decided, request.StateOracleDrafting)
}

// The job's own words are part of the transition, not a later write: the
// request is saved once in resume_review with them.
func TestAnOracleDraftingJobFailureIsSavedWithItsDetailBeforeTheReminder(t *testing.T) {
	dp := newFakeDeps(t)
	dataDir, id := requestdrivertest.OracleStageFixture(t, true)
	var atFirstSave string
	requestdriver.SetBetweenReviewSaves(t, func(dataDir, savedID string) {
		if r, err := request.Load(dataDir, savedID); err == nil && savedID == id {
			atFirstSave = r.Error
		}
	})
	failure := &requestdriver.OracleDraftJobFailure{Cause: unresolved.SandboxLaunch, Err: errors.New("the gateway refused the sandbox")}
	runner, _ := requestdrivertest.StubOracleDraftRunner(request.OracleDraft{}, fmt.Errorf("oracle job: %w", failure))
	if err := driveOracleDrafting(t, dp, context.Background(), dataDir, runner); err != nil {
		t.Fatal(err)
	}
	got := requestdrivertest.LoadRequest(t, dataDir, id)
	if want := "; the job reported: the gateway refused the sandbox"; atFirstSave == "" || got.Error != atFirstSave || !strings.HasSuffix(got.Error, want) {
		t.Fatalf("Error at the review save %q, after %q: want both to end %q", atFirstSave, got.Error, want)
	}
}
