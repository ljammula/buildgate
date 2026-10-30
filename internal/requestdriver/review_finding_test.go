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

// SC-022: severity never refuses. A high finding starts no corrective build:
// an accepted run stays accepted, and a run whose code_review failed with a
// verdict (a record from a worker whose findings still failed it) is not
// handed to a build.
func TestHighSeverityFindingStartsNoCorrectiveRound(t *testing.T) {
	high := &run.CodeReviewResult{Policy: "required", Available: true, Findings: []run.CodeReviewFinding{
		{Severity: "high", File: "a.go", Line: 1, Summary: "nil deref", FailureScenario: "crash"},
	}}
	for name, tc := range map[string]struct {
		state     run.State
		failed    []string
		wantState request.State
	}{
		"accepted with a high finding":      {run.StateAccepted, nil, request.StatePRReview},
		"code_review failed with a verdict": {run.StateQuarantined, []string{"code_review"}, request.StateQuarantined},
	} {
		t.Run(name, func(t *testing.T) {
			dp := newFakeDeps(t)
			dataDir, id := requestdrivertest.BuildingFixture(dp, t, 1)
			buildRunner := func(ctx context.Context, args []string, onReady func(*run.Run)) error {
				ticket := requestdrivertest.ArgValue(args, "-ticket")
				if onReady != nil {
					onReady(&run.Run{ID: ticket})
				}
				rr := requestdrivertest.QuarantinedOn(t, dataDir, ticket, "factoryd/"+id+"-001", fmt.Sprintf("%040d", 1), fmt.Sprintf("%040d", 2), tc.failed...)
				rr.CodeReview, rr.State = high, tc.state
				if tc.state == run.StateAccepted {
					rr.HandoffSHA256, rr.PullRequestURL = "", "https://github.com/acme/app/pull/1"
				}
				return rr.Save(dataDir)
			}
			calls, _ := requestdrivertest.StubReviewCorrectiveRunner(t, dataDir, func(dataDir, roundRunID string) *run.Run {
				return &run.Run{ID: roundRunID, State: run.StateAccepted}
			})
			cfg := requestdriver.WorkerConfig{ReviewCorrectiveRounds: 1, OpenPullRequest: true}
			if err := requestdrivertest.DriveRequests(dp, context.Background(), dataDir, cfg, requestdrivertest.FailingSpecDraftRunner(t), requestdrivertest.FailingPlanTicketsRunner(t), buildRunner); err != nil {
				t.Fatalf("driveRequests: %v", err)
			}
			if *calls != 0 {
				t.Errorf("corrective runner calls = %d, want 0", *calls)
			}
			loaded := requestdrivertest.LoadRequest(t, dataDir, id)
			if loaded.State != tc.wantState || loaded.QuarantineCheck != "" || len(loaded.Tickets[0].Rounds) != 0 {
				t.Errorf("State %q, QuarantineCheck %q, Rounds %+v, want %q with no check and no round", loaded.State, loaded.QuarantineCheck, loaded.Tickets[0].Rounds, tc.wantState)
			}
		})
	}
}

// A ticket run with only a review that gave no verdict is not judged: no
// corrective build, and the request waits in resume_review with the cause.
func TestCodeReviewWithNoVerdictStartsNoRoundAndWaitsInResumeReview(t *testing.T) {
	dp := newFakeDeps(t)
	dataDir, id := requestdrivertest.BuildingFixture(dp, t, 1)
	buildRunner := func(ctx context.Context, args []string, onReady func(*run.Run)) error {
		ticket := requestdrivertest.ArgValue(args, "-ticket")
		if onReady != nil {
			onReady(&run.Run{ID: ticket})
		}
		rr := requestdrivertest.QuarantinedOn(t, dataDir, ticket, "factoryd/"+id+"-001", fmt.Sprintf("%040d", 1), fmt.Sprintf("%040d", 2), "code_review")
		rr.CodeReview = &run.CodeReviewResult{Policy: "required", Available: false}
		return rr.Save(dataDir)
	}
	calls, _ := requestdrivertest.StubReviewCorrectiveRunner(t, dataDir, func(dataDir, roundRunID string) *run.Run {
		return &run.Run{ID: roundRunID, State: run.StateAccepted}
	})
	cfg := requestdriver.WorkerConfig{ReviewCorrectiveRounds: 1}
	if err := requestdrivertest.DriveRequests(dp, context.Background(), dataDir, cfg, requestdrivertest.FailingSpecDraftRunner(t), requestdrivertest.FailingPlanTicketsRunner(t), buildRunner); err != nil {
		t.Fatalf("driveRequests: %v", err)
	}
	if *calls != 0 {
		t.Fatalf("corrective runner calls = %d, want 0", *calls)
	}
	loaded := requestdrivertest.LoadRequest(t, dataDir, id)
	if loaded.State != request.StateResumeReview || loaded.Resume == nil || loaded.Resume.Cause != unresolved.NoVerdict {
		t.Fatalf("state %s resume %+v, want resume_review with cause %s", loaded.State, loaded.Resume, unresolved.NoVerdict)
	}
}

// A PR-review round ends after its one build whatever the run's review
// found: a finding is advice, so no fix attempt follows a quarantined round.
func TestRunCorrectiveRoundNoFixAttemptWithoutAReviewFinding(t *testing.T) {
	dp := newFakeDeps(t)
	threads := []forge.Thread{{ID: "thread-1", Path: "a.go", Line: 1, Author: "alice", Body: "fix", CommentID: 5}}
	r, dataDir := requestdrivertest.StubPRReviewTestFixture(t, 1)
	stubPRReviewDeps(dp, t, forge.ReviewState{State: "OPEN", BlocksReadyThreads: threads, ActionableThreads: threads}, nil)
	builds := 0
	requestdriver.PrReviewCorrectiveRunner = func(ctx context.Context, args []string, onReady func(*run.Run)) error {
		builds++
		rr := requestdrivertest.CorrectableQuarantine(t, dataDir, requestdrivertest.ArgValue(args, "-ticket"), "", "", "")
		rr.CodeReview = &run.CodeReviewResult{Policy: "required", Available: true, Findings: []run.CodeReviewFinding{{Severity: "high", File: "a.go", Line: 2, Summary: "x"}}}
		return rr.Save(dataDir)
	}
	cfg := requestdriver.WorkerConfig{PrPollInterval: time.Minute, MaxReviewRounds: 3, ReviewCorrectiveRounds: 2}
	if err := requestdriver.AdvancePRReview(dp, context.Background(), dataDir, r, cfg, time.Now()); err != nil {
		t.Fatal(err)
	}
	if rounds := r.Tickets[0].Rounds; builds != 1 || len(rounds) != 1 || len(rounds[0].PriorRunIDs) != 0 {
		t.Fatalf("builds = %d, rounds = %+v; want one build and no fix attempt", builds, rounds)
	}
}
