package requestdriver_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"buildgate/internal/request"
	"buildgate/internal/requestdriver"
	"buildgate/internal/requestdriver/requestdrivertest"
	"buildgate/internal/unresolved"
)

// driveOracleDrafting runs one driver pass with runner as the oracle job.
func driveOracleDrafting(t *testing.T, dp requestdriver.Deps, ctx context.Context, dataDir string, runner requestdriver.OracleDraftRunner) error {
	t.Helper()
	return requestdrivertest.DriveRequests(dp, ctx, dataDir, requestdriver.WorkerConfig{}, requestdrivertest.FailingSpecDraftRunner(t), requestdrivertest.FailingPlanTicketsRunner(t), runner, requestdrivertest.FailingBuildRunner(t))
}

// resumeOracleDrafting is `factoryd resume <id>` with gate's checks.
func resumeOracleDrafting(dataDir, id string, gate requestdriver.ResumeGate) (*request.Request, error) {
	return request.ResumeRequest(dataDir, id, request.ResumeRound, "alice", time.Now(), func(r *request.Request) ([]string, error) {
		return gate.RequestResumeRefusals(context.Background(), dataDir, "docker", r)
	})
}

// An oracle-drafting job that ended before it produced a draft is no draft
// to review: the request waits in resume_review with the job's cause, one
// reminder goes out, and nothing drafts again on its own.
func TestAnOracleDraftingJobFailureWaitsInResumeReviewWithItsCause(t *testing.T) {
	dp := newFakeDeps(t)
	cases := map[string]struct {
		cause, want unresolved.Cause
	}{
		"sandbox never started":       {unresolved.SandboxLaunch, unresolved.SandboxLaunch},
		"no model request answered":   {unresolved.ModelUnavailable, unresolved.ModelUnavailable},
		"job ended with no draft":     {unresolved.DraftingJob, unresolved.DraftingJob},
		"a cause outside the set":     {"something a job made up", unresolved.DraftingJob},
		"no cause reads as the job's": {"", unresolved.DraftingJob},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			dataDir, id := requestdrivertest.OracleStageFixture(t, true)
			failure := &requestdriver.OracleDraftJobFailure{Cause: tc.cause, Err: errors.New("the gateway refused the sandbox\nsecond line")}
			runner, calls := requestdrivertest.StubOracleDraftRunner(request.OracleDraft{}, fmt.Errorf("oracle job: %w", failure))
			if err := driveOracleDrafting(t, dp, context.Background(), dataDir, runner); err != nil {
				t.Fatal(err)
			}
			got := requestdrivertest.LoadRequest(t, dataDir, id)
			if got.State != request.StateResumeReview {
				t.Fatalf("State = %q, want resume_review (OracleDraft %+v)", got.State, got.OracleDraft)
			}
			if got.Resume == nil || got.Resume.FromState != request.StateOracleDrafting || got.Resume.Cause != tc.want || got.Resume.Generation != 1 || got.Resume.LostRunID != "" {
				t.Fatalf("Resume = %+v, want oracle_drafting, cause %q, Generation 1, no run", got.Resume, tc.want)
			}
			if got.OracleDraft != nil {
				t.Errorf("OracleDraft = %+v, want none: a job failure is not a draft", got.OracleDraft)
			}
			for _, want := range []string{tc.want.What("oracle_drafting"), "factoryd resume " + id, "the gateway refused the sandbox"} {
				if !strings.Contains(got.Error, want) {
					t.Errorf("Error = %q, want it to contain %q", got.Error, want)
				}
			}
			if strings.Contains(got.Error, "\n") {
				t.Errorf("Error = %q, want one line", got.Error)
			}
			if got.NotifyCount != 1 || requestdrivertest.CountNotificationLogLines(t, dataDir, id) != 1 {
				t.Errorf("NotifyCount = %d, log lines = %d, want the one immediate reminder", got.NotifyCount, requestdrivertest.CountNotificationLogLines(t, dataDir, id))
			}
			// A human wait: the next pass must not draft again.
			if err := driveOracleDrafting(t, dp, context.Background(), dataDir, requestdrivertest.FailingOracleDraftRunner(t)); err != nil {
				t.Fatal(err)
			}
			if len(*calls) != 1 {
				t.Errorf("runner calls = %d, want 1", len(*calls))
			}
			if got := requestdrivertest.LoadRequest(t, dataDir, id); got.State != request.StateResumeReview {
				t.Errorf("State after another pass = %q, want resume_review", got.State)
			}
		})
	}
}

// `factoryd resume` returns the request to oracle_drafting and the step
// drafts again: a second job failure waits in resume_review under a new
// Generation, and a draft that succeeds reaches oracle_review carrying what
// the failed passes spent.
func TestAResumedOracleDraftingStepDraftsAgainAndASecondFailureIsANewGeneration(t *testing.T) {
	dp := newFakeDeps(t)
	dataDir, id := requestdrivertest.OracleStageFixture(t, true)
	spend := func(cost int64) *request.JobSpend {
		return &request.JobSpend{Role: "review", Model: "model-a", CostMicroUSD: cost, At: time.Now()}
	}
	failing := func(cost int64) requestdriver.OracleDraftRunner {
		runner, _ := requestdrivertest.StubOracleDraftRunner(request.OracleDraft{}, &requestdriver.OracleDraftJobFailure{Cause: unresolved.DraftingJob, Err: errors.New("exited 1"), Spend: spend(cost)})
		return runner
	}
	gate := requestdriver.ResumeGate{Containers: &requestdrivertest.FakeJobContainers{}}

	if err := driveOracleDrafting(t, dp, context.Background(), dataDir, failing(10)); err != nil {
		t.Fatal(err)
	}
	// A container of the failed job still alive refuses the resume.
	alive := requestdriver.ResumeGate{Containers: &requestdrivertest.FakeJobContainers{IDs: []string{"abc123"}}}
	var refused *request.ResumeRefusedError
	if _, err := resumeOracleDrafting(dataDir, id, alive); !errors.As(err, &refused) || !strings.Contains(err.Error(), "oracle_drafting step is still alive") {
		t.Fatalf("resume beside a live container: %v, want a refusal naming the step", err)
	}
	if got := requestdrivertest.LoadRequest(t, dataDir, id); got.State != request.StateResumeReview {
		t.Fatalf("State after a refused resume = %q, want resume_review", got.State)
	}

	resumed, err := resumeOracleDrafting(dataDir, id, gate)
	if err != nil {
		t.Fatal(err)
	}
	if resumed.State != request.StateOracleDrafting || resumed.Error != "" {
		t.Fatalf("after resume: State = %q, Error = %q; want oracle_drafting and no error", resumed.State, resumed.Error)
	}

	if err := driveOracleDrafting(t, dp, context.Background(), dataDir, failing(5)); err != nil {
		t.Fatal(err)
	}
	second := requestdrivertest.LoadRequest(t, dataDir, id)
	if second.State != request.StateResumeReview || second.Resume == nil || second.Resume.Generation != 2 {
		t.Fatalf("second failure: State = %q, Resume = %+v; want resume_review, Generation 2", second.State, second.Resume)
	}
	if second.NotifyCount != 1 {
		t.Errorf("NotifyCount = %d, want the new entry's own first reminder", second.NotifyCount)
	}

	if _, err := resumeOracleDrafting(dataDir, id, gate); err != nil {
		t.Fatal(err)
	}
	drafted, calls := requestdrivertest.StubOracleDraftRunner(request.OracleDraft{Status: request.OracleDrafted, Files: []string{"oracle_001_test.go"}, Spend: spend(100)}, nil)
	if err := driveOracleDrafting(t, dp, context.Background(), dataDir, drafted); err != nil {
		t.Fatal(err)
	}
	got := requestdrivertest.LoadRequest(t, dataDir, id)
	if got.State != request.StateOracleReview || got.OracleDraft == nil || got.OracleDraft.Status != request.OracleDrafted {
		t.Fatalf("State = %q, OracleDraft = %+v; want a drafted oracle_review", got.State, got.OracleDraft)
	}
	if len(*calls) != 1 {
		t.Errorf("redraft calls = %d, want 1", len(*calls))
	}
	if got.OracleDraft.Spend == nil || got.OracleDraft.Spend.CostMicroUSD != 115 {
		t.Errorf("Spend = %+v, want the two failed passes and the draft: 115", got.OracleDraft.Spend)
	}
}

// A stop request wins over a job failure: the request stays in
// oracle_drafting for the next pass.
func TestAStoppedOracleDraftingJobFailureLeavesTheRequestInPlace(t *testing.T) {
	dp := newFakeDeps(t)
	dataDir, id := requestdrivertest.OracleStageFixture(t, true)
	ctx, cancel := context.WithCancel(context.Background())
	runner := func(context.Context, requestdriver.OracleDraftInput) (request.OracleDraft, error) {
		cancel()
		return request.OracleDraft{}, &requestdriver.OracleDraftJobFailure{Cause: unresolved.DraftingJob, Err: context.Canceled}
	}
	if err := driveOracleDrafting(t, dp, ctx, dataDir, runner); err == nil {
		t.Fatal("want the cancellation surfaced")
	}
	if got := requestdrivertest.LoadRequest(t, dataDir, id); got.State != request.StateOracleDrafting || got.Resume != nil {
		t.Errorf("State = %q, Resume = %+v; want untouched oracle_drafting", got.State, got.Resume)
	}
}
