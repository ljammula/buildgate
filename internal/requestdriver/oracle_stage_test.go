package requestdriver_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"buildgate/internal/request"
	"buildgate/internal/requestdriver"
	"buildgate/internal/requestdriver/requestdrivertest"
)

// A request submitted without -draft-oracles behaves exactly as before the
// oracle stage existed: spec approval lands in planning, the oracle runner is
// never called, and request.json carries no oracle keys.
func TestFlaglessRequestSkipsOracleStage(t *testing.T) {
	dp := newFakeDeps(t)
	dataDir, id := requestdrivertest.OracleStageFixture(t, false)
	if got := requestdrivertest.LoadRequest(t, dataDir, id); got.State != request.StatePlanning {
		t.Fatalf("State after spec approval = %q, want planning", got.State)
	}
	runner, _ := requestdrivertest.StubPlanTicketsRunner([]requestdriver.DraftedTicket{{Filename: "001.spec.md", Content: requestdrivertest.ValidBrownfieldTicket("make verify", 1, 2)}}, &request.PlanEvidence{}, nil)
	if err := requestdrivertest.DriveRequests(dp, context.Background(), dataDir, requestdriver.WorkerConfig{}, requestdrivertest.FailingSpecDraftRunner(t), runner, requestdrivertest.FailingOracleDraftRunner(t), requestdrivertest.FailingBuildRunner(t)); err != nil {
		t.Fatal(err)
	}
	if got := requestdrivertest.LoadRequest(t, dataDir, id); got.State != request.StatePlanReview {
		t.Fatalf("State = %q, want plan_review", got.State)
	}
	b, err := os.ReadFile(filepath.Join(request.Dir(dataDir, id), "request.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "oracle") {
		t.Errorf("flag-less request.json mentions oracle: %s", b)
	}
}

// A drafter failure (error or timeout) or a bogus status never halts the
// request: it lands in oracle_review as failed.
func TestOracleDraftingFailureLandsInReviewNotHalted(t *testing.T) {
	dp := newFakeDeps(t)
	cases := map[string]struct {
		draft request.OracleDraft
		err   error
		want  string
	}{
		"runner error":    {err: errors.New("relay unreachable"), want: "relay unreachable"},
		"runner timeout":  {err: context.DeadlineExceeded, want: "deadline exceeded"},
		"invalid status":  {draft: request.OracleDraft{Status: "bogus"}, want: "invalid status"},
		"empty status":    {draft: request.OracleDraft{}, want: "invalid status"},
		"none eligible":   {draft: request.OracleDraft{Status: request.OracleNoneEligible, Detail: "nothing testable"}},
		"over cap status": {draft: request.OracleDraft{Status: request.OracleDraftOverCap}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			dataDir, id := requestdrivertest.OracleStageFixture(t, true)
			runner, calls := requestdrivertest.StubOracleDraftRunner(tc.draft, tc.err)
			if err := requestdrivertest.DriveRequests(dp, context.Background(), dataDir, requestdriver.WorkerConfig{}, requestdrivertest.FailingSpecDraftRunner(t), requestdrivertest.FailingPlanTicketsRunner(t), runner, requestdrivertest.FailingBuildRunner(t)); err != nil {
				t.Fatal(err)
			}
			if len(*calls) != 1 {
				t.Fatalf("runner calls = %d, want 1", len(*calls))
			}
			got := requestdrivertest.LoadRequest(t, dataDir, id)
			if got.State != request.StateOracleReview {
				t.Fatalf("State = %q, want oracle_review (Error: %q)", got.State, got.Error)
			}
			wantStatus := tc.draft.Status
			if tc.err != nil || !wantStatus.Valid() {
				wantStatus = request.OracleDraftFailed
			}
			if got.OracleDraft == nil || got.OracleDraft.Status != wantStatus {
				t.Fatalf("OracleDraft = %+v, want status %q", got.OracleDraft, wantStatus)
			}
			if tc.want != "" && !strings.Contains(got.OracleDraft.Detail, tc.want) {
				t.Errorf("Detail = %q, want it to contain %q", got.OracleDraft.Detail, tc.want)
			}
		})
	}
}

// A stop request mid-run (the daemon's own context cancelled) leaves the
// request in oracle_drafting so the next pass re-runs the job; it is not
// recorded as a failed draft.
func TestOracleDraftingStopRequestLeavesRequestInPlace(t *testing.T) {
	dp := newFakeDeps(t)
	dataDir, id := requestdrivertest.OracleStageFixture(t, true)
	ctx, cancel := context.WithCancel(context.Background())
	runner := func(ctx context.Context, in requestdriver.OracleDraftInput) (request.OracleDraft, error) {
		cancel()
		return request.OracleDraft{}, ctx.Err()
	}
	if err := requestdrivertest.DriveRequests(dp, ctx, dataDir, requestdriver.WorkerConfig{}, requestdrivertest.FailingSpecDraftRunner(t), requestdrivertest.FailingPlanTicketsRunner(t), runner, requestdrivertest.FailingBuildRunner(t)); err == nil {
		t.Fatal("want the cancellation surfaced")
	}
	got := requestdrivertest.LoadRequest(t, dataDir, id)
	if got.State != request.StateOracleDrafting || got.OracleDraft != nil {
		t.Errorf("State = %q, OracleDraft = %+v; want untouched oracle_drafting", got.State, got.OracleDraft)
	}
}

// With no rejections the drafter gets no feedback path at all.
func TestOracleDraftingWithoutRejectionsHasNoFeedbackPath(t *testing.T) {
	dp := newFakeDeps(t)
	dataDir, _ := requestdrivertest.OracleStageFixture(t, true)
	runner, calls := requestdrivertest.StubOracleDraftRunner(request.OracleDraft{Status: request.OracleDrafted}, nil)
	if err := requestdrivertest.DriveRequests(dp, context.Background(), dataDir, requestdriver.WorkerConfig{}, requestdrivertest.FailingSpecDraftRunner(t), requestdrivertest.FailingPlanTicketsRunner(t), runner, requestdrivertest.FailingBuildRunner(t)); err != nil {
		t.Fatal(err)
	}
	if len(*calls) != 1 || (*calls)[0].FeedbackPath != "" {
		t.Errorf("calls = %+v, want one call with an empty FeedbackPath", *calls)
	}
}

// oracle_review is a human-wait state: it is reminded like spec_review and
// plan_review, and the reminder names oracle/ and the approve command.
func TestRemindDueRequestsRemindsOracleReview(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	if err := request.SaveText(dataDir, "req-1", "text"); err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 9, 19, 10, 0, 0, 0, time.UTC)
	r := request.New("req-1", "/repos/app", "app", request.Source{Kind: request.SourceText}, base)
	r.State = request.StateOracleReview
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	if err := requestdriver.RemindDueRequests(dataDir, 15*time.Minute, func() time.Time { return base }); err != nil {
		t.Fatal(err)
	}
	if got := requestdrivertest.CountNotificationLogLines(t, dataDir, "req-1"); got != 1 {
		t.Fatalf("notifications.log lines = %d, want 1", got)
	}
	log, err := os.ReadFile(requestdriver.RequestNotificationLogPath(dataDir, "req-1"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"oracle_review", "/oracle", "factoryd approve req-1"} {
		if !strings.Contains(string(log), want) {
			t.Errorf("reminder %s does not contain %q", log, want)
		}
	}
	if !requestdriver.ReviewState(request.StateOracleReview) || requestdriver.RequestDriverOwnsState(request.StateOracleReview) || !requestdriver.RequestDriverOwnsState(request.StateOracleDrafting) {
		t.Error("oracle_review must be a review state the driver does not own; oracle_drafting must be driver-owned")
	}
}

func TestApprovingANonFailedDraftDoesNotWarn(t *testing.T) {
	t.Parallel()
	dataDir, id := requestdrivertest.OracleStageFixture(t, true)
	requestdrivertest.MoveToOracleReview(t, dataDir, id, request.OracleDraft{Status: request.OracleNoneEligible, Detail: "none"})
	r, err := request.Approve(dataDir, id, "alice", time.Now(), nil)
	if err != nil {
		t.Fatalf("none_eligible skip: %v", err)
	}
	if r.OracleSkipWarning != "" {
		t.Fatalf("warned for none_eligible: %q", r.OracleSkipWarning)
	}
}

// A draft whose files were all deleted before approval is a skip too.
func TestApprovingADraftedStatusWithNoFilesWarns(t *testing.T) {
	t.Parallel()
	dataDir, id := requestdrivertest.OracleStageFixture(t, true)
	requestdrivertest.MoveToOracleReview(t, dataDir, id, request.OracleDraft{Status: request.OracleDrafted, Files: []string{"gone_test.go"}})
	r, err := request.Approve(dataDir, id, "alice", time.Now(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if r.OracleSkipWarning != request.OracleSkippedWarning(request.OracleDrafted) {
		t.Errorf("warning = %q", r.OracleSkipWarning)
	}
}
