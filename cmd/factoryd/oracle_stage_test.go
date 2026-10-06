package main

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
)

// oracleStageFixture writes a spec_review request (spec.md present, workspace
// with a .factory.yml verify_command) and approves the spec, so the returned
// request sits wherever spec approval routes it: oracle_drafting when
// draftOracles, planning otherwise.
func oracleStageFixture(t *testing.T, draftOracles bool) (dataDir, id string) {
	t.Helper()
	dataDir = t.TempDir()
	id = "req-1"
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, ".factory.yml"), []byte("verify_command: make verify\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := request.SaveText(dataDir, id, "some request text"); err != nil {
		t.Fatal(err)
	}
	r := request.New(id, workspace, "app", request.Source{Kind: request.SourceText}, time.Now())
	r.State = request.StateSpecReview
	r.DraftOracles = draftOracles
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(requestdriver.RequestSpecPath(dataDir, id), []byte(twoCriteriaSpec), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := request.Approve(dataDir, id, "alice", time.Now(), nil); err != nil {
		t.Fatalf("approve spec: %v", err)
	}
	return dataDir, id
}

// noOracleScriptCfg points the production drafting runner at a script that
// does not exist, so it records not_implemented instead of launching a
// sandbox: the state-machine tests here exercise routing, not drafting.
var noOracleScriptCfg = requestdriver.WorkerConfig{OracleDraftScript: "/nonexistent/draft_acceptance_oracles.py"}

func loadRequest(t *testing.T, dataDir, id string) *request.Request {
	t.Helper()
	r, err := request.Load(dataDir, id)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func stubOracleDraftRunner(draft request.OracleDraft, err error) (requestdriver.OracleDraftRunner, *[]requestdriver.OracleDraftInput) {
	var calls []requestdriver.OracleDraftInput
	return func(ctx context.Context, in requestdriver.OracleDraftInput) (request.OracleDraft, error) {
		calls = append(calls, in)
		return draft, err
	}, &calls
}

func writeHandOracle(t *testing.T, dataDir, id string) {
	t.Helper()
	dir := filepath.Join(request.Dir(dataDir, id), request.RequestOracleDirName)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "oracle_test.go"), []byte("package x\n\nimport \"testing\"\n\nfunc TestOracleX(t *testing.T) {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "RUN_COMMAND.txt"), []byte("go test ./.oracle/...\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// A request submitted without -draft-oracles behaves exactly as before the
// oracle stage existed: spec approval lands in planning, the oracle runner is
// never called, and request.json carries no oracle keys.
func TestFlaglessRequestSkipsOracleStage(t *testing.T) {
	dp := newTestDeps(t)
	dataDir, id := oracleStageFixture(t, false)
	if got := loadRequest(t, dataDir, id); got.State != request.StatePlanning {
		t.Fatalf("State after spec approval = %q, want planning", got.State)
	}
	runner, _ := stubPlanTicketsRunner([]requestdriver.DraftedTicket{{Filename: "001.spec.md", Content: validBrownfieldTicket("make verify", 1, 2)}}, &request.PlanEvidence{}, nil)
	if err := driveRequests(dp, context.Background(), dataDir, requestdriver.WorkerConfig{}, failingSpecDraftRunner(t), runner, failingOracleDraftRunner(t), failingBuildRunner(t)); err != nil {
		t.Fatal(err)
	}
	if got := loadRequest(t, dataDir, id); got.State != request.StatePlanReview {
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

// The interim default runner records not_implemented, writes no files, and
// lands in oracle_review with the first reminder sent.
func TestOracleDraftingDefaultRunnerMovesToReviewWithoutFiles(t *testing.T) {
	dp := newTestDeps(t)
	dataDir, id := oracleStageFixture(t, true)
	if got := loadRequest(t, dataDir, id); got.State != request.StateOracleDrafting {
		t.Fatalf("State after spec approval = %q, want oracle_drafting", got.State)
	}
	if err := driveRequests(dp, context.Background(), dataDir, noOracleScriptCfg, failingSpecDraftRunner(t), failingPlanTicketsRunner(t), runOracleDraftJob, failingBuildRunner(t)); err != nil {
		t.Fatal(err)
	}
	got := loadRequest(t, dataDir, id)
	if got.State != request.StateOracleReview {
		t.Fatalf("State = %q, want oracle_review", got.State)
	}
	if got.OracleDraft == nil || got.OracleDraft.Status != request.OracleNotImplemented {
		t.Fatalf("OracleDraft = %+v, want not_implemented", got.OracleDraft)
	}
	if _, err := os.Stat(filepath.Join(request.Dir(dataDir, id), request.RequestOracleDirName)); !os.IsNotExist(err) {
		t.Errorf("default runner must write no oracle/ directory (stat err = %v)", err)
	}
	if got.NotifyCount != 1 || countNotificationLogLines(t, dataDir, id) != 1 {
		t.Errorf("NotifyCount = %d, log lines = %d, want the immediate oracle_review reminder", got.NotifyCount, countNotificationLogLines(t, dataDir, id))
	}
	// oracle_review is a human wait: further passes leave it alone.
	if err := driveRequests(dp, context.Background(), dataDir, requestdriver.WorkerConfig{}, failingSpecDraftRunner(t), failingPlanTicketsRunner(t), failingOracleDraftRunner(t), failingBuildRunner(t)); err != nil {
		t.Fatal(err)
	}
	if got := loadRequest(t, dataDir, id); got.State != request.StateOracleReview {
		t.Errorf("State = %q, want oracle_review untouched", got.State)
	}
}

// A drafter failure (error or timeout) or a bogus status never halts the
// request: it lands in oracle_review as failed.
func TestOracleDraftingFailureLandsInReviewNotHalted(t *testing.T) {
	dp := newTestDeps(t)
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
			dataDir, id := oracleStageFixture(t, true)
			runner, calls := stubOracleDraftRunner(tc.draft, tc.err)
			if err := driveRequests(dp, context.Background(), dataDir, requestdriver.WorkerConfig{}, failingSpecDraftRunner(t), failingPlanTicketsRunner(t), runner, failingBuildRunner(t)); err != nil {
				t.Fatal(err)
			}
			if len(*calls) != 1 {
				t.Fatalf("runner calls = %d, want 1", len(*calls))
			}
			got := loadRequest(t, dataDir, id)
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
	dp := newTestDeps(t)
	dataDir, id := oracleStageFixture(t, true)
	ctx, cancel := context.WithCancel(context.Background())
	runner := func(ctx context.Context, in requestdriver.OracleDraftInput) (request.OracleDraft, error) {
		cancel()
		return request.OracleDraft{}, ctx.Err()
	}
	if err := driveRequests(dp, ctx, dataDir, requestdriver.WorkerConfig{}, failingSpecDraftRunner(t), failingPlanTicketsRunner(t), runner, failingBuildRunner(t)); err == nil {
		t.Fatal("want the cancellation surfaced")
	}
	got := loadRequest(t, dataDir, id)
	if got.State != request.StateOracleDrafting || got.OracleDraft != nil {
		t.Errorf("State = %q, OracleDraft = %+v; want untouched oracle_drafting", got.State, got.OracleDraft)
	}
}

// A spec edited after approval halts oracle_drafting (the drafter's input
// changed), like planning -- and `retry` resumes into oracle_drafting rather
// than skipping ahead to planning, because the approved spec.md on disk would
// otherwise send it there.
func TestHaltedOracleDraftingIsRetryableIntoOracleDrafting(t *testing.T) {
	dp := newTestDeps(t)
	dataDir, id := oracleStageFixture(t, true)
	if err := os.WriteFile(requestdriver.RequestSpecPath(dataDir, id), []byte(twoCriteriaSpec+"\nedited\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := driveRequests(dp, context.Background(), dataDir, requestdriver.WorkerConfig{}, failingSpecDraftRunner(t), failingPlanTicketsRunner(t), failingOracleDraftRunner(t), failingBuildRunner(t)); err != nil {
		t.Fatal(err)
	}
	halted := loadRequest(t, dataDir, id)
	if halted.State != request.StateHalted {
		t.Fatalf("State = %q, want halted", halted.State)
	}
	handled, err := retryRequest(dp, dataDir, halted, "", time.Now())
	if err != nil || !handled {
		t.Fatalf("retryRequest: handled=%v err=%v", handled, err)
	}
	if got := loadRequest(t, dataDir, id); got.State != request.StateOracleDrafting || got.Error != "" {
		t.Errorf("after retry: State = %q, Error = %q; want oracle_drafting with the error cleared", got.State, got.Error)
	}
}

// A request halted in planning after a completed oracle stage still resumes
// into planning: the HaltedFrom rule must not drag it back into the oracle
// stage.
func TestHaltedPlanningAfterOracleStageResumesIntoPlanning(t *testing.T) {
	dp := newTestDeps(t)
	dataDir, id := oracleStageFixture(t, true)
	if err := driveRequests(dp, context.Background(), dataDir, noOracleScriptCfg, failingSpecDraftRunner(t), failingPlanTicketsRunner(t), runOracleDraftJob, failingBuildRunner(t)); err != nil {
		t.Fatal(err)
	}
	if _, err := request.Approve(dataDir, id, "alice", time.Now(), nil); err != nil {
		t.Fatal(err)
	}
	r := loadRequest(t, dataDir, id)
	if err := r.Halt("plan drafting failed: boom", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	if handled, err := retryRequest(dp, dataDir, loadRequest(t, dataDir, id), "", time.Now()); err != nil || !handled {
		t.Fatalf("retryRequest: handled=%v err=%v", handled, err)
	}
	if got := loadRequest(t, dataDir, id); got.State != request.StatePlanning {
		t.Errorf("State = %q, want planning", got.State)
	}
}

// Rejecting at oracle_review returns to oracle_drafting; the redraft's runner
// input carries the operator's reasons on a separate feedback file, request.md
// is untouched, and the previous oracle/ files are snapshotted.
func TestOracleRejectFeedsTheDrafterThroughFeedbackPath(t *testing.T) {
	dp := newTestDeps(t)
	dataDir, id := oracleStageFixture(t, true)
	if err := driveRequests(dp, context.Background(), dataDir, noOracleScriptCfg, failingSpecDraftRunner(t), failingPlanTicketsRunner(t), runOracleDraftJob, failingBuildRunner(t)); err != nil {
		t.Fatal(err)
	}
	writeHandOracle(t, dataDir, id)
	requestMD, err := os.ReadFile(request.TextPath(dataDir, id))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := request.Reject(dataDir, id, "bob", "assert the idempotency key is reused", time.Now()); err != nil {
		t.Fatal(err)
	}
	if got := loadRequest(t, dataDir, id); got.State != request.StateOracleDrafting {
		t.Fatalf("State = %q, want oracle_drafting", got.State)
	}
	if after, _ := os.ReadFile(request.TextPath(dataDir, id)); string(after) != string(requestMD) {
		t.Errorf("request.md changed by an oracle rejection: %q", after)
	}

	runner, calls := stubOracleDraftRunner(request.OracleDraft{Status: request.OracleDrafted}, nil)
	if err := driveRequests(dp, context.Background(), dataDir, requestdriver.WorkerConfig{}, failingSpecDraftRunner(t), failingPlanTicketsRunner(t), runner, failingBuildRunner(t)); err != nil {
		t.Fatal(err)
	}
	if len(*calls) != 1 {
		t.Fatalf("runner calls = %d, want 1", len(*calls))
	}
	in := (*calls)[0]
	if in.FeedbackPath == "" || filepath.Base(in.FeedbackPath) == "request.md" {
		t.Fatalf("FeedbackPath = %q, want a separate feedback file", in.FeedbackPath)
	}
	fb, err := os.ReadFile(in.FeedbackPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(fb), "assert the idempotency key is reused") {
		t.Errorf("feedback file = %q, want the rejection reason", fb)
	}
	if !strings.HasSuffix(in.OracleDir, "/oracle") {
		t.Errorf("OracleDir = %q, want the request-level oracle/ directory", in.OracleDir)
	}
	if got := loadRequest(t, dataDir, id); got.State != request.StateOracleReview || got.OracleDraft.Status != request.OracleDrafted {
		t.Errorf("State = %q, OracleDraft = %+v", got.State, got.OracleDraft)
	}
	revs, err := request.ListRevisions(dataDir, id)
	if err != nil || len(revs) != 1 || len(revs[0].Files) != 2 {
		t.Errorf("revisions = %+v, %v; want one snapshot of the two oracle files", revs, err)
	}
}

// With no rejections the drafter gets no feedback path at all.
func TestOracleDraftingWithoutRejectionsHasNoFeedbackPath(t *testing.T) {
	dp := newTestDeps(t)
	dataDir, _ := oracleStageFixture(t, true)
	runner, calls := stubOracleDraftRunner(request.OracleDraft{Status: request.OracleDrafted}, nil)
	if err := driveRequests(dp, context.Background(), dataDir, requestdriver.WorkerConfig{}, failingSpecDraftRunner(t), failingPlanTicketsRunner(t), runner, failingBuildRunner(t)); err != nil {
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
	if got := countNotificationLogLines(t, dataDir, "req-1"); got != 1 {
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

// The HaltedFrom rule must not change retry routing for a flag-less request:
// halted in planning resumes into planning, halted in spec_drafting (no
// approved spec) resumes into spec_drafting.
func TestFlaglessHaltedRetryRoutingUnchanged(t *testing.T) {
	dp := newTestDeps(t)
	t.Parallel()
	t.Run("planning", func(t *testing.T) {
		dataDir, id := oracleStageFixture(t, false)
		r := loadRequest(t, dataDir, id)
		if err := r.Halt("plan drafting failed", time.Now()); err != nil {
			t.Fatal(err)
		}
		if err := r.Save(dataDir); err != nil {
			t.Fatal(err)
		}
		if handled, err := retryRequest(dp, dataDir, loadRequest(t, dataDir, id), "", time.Now()); err != nil || !handled {
			t.Fatalf("retryRequest: handled=%v err=%v", handled, err)
		}
		if got := loadRequest(t, dataDir, id); got.State != request.StatePlanning {
			t.Errorf("State = %q, want planning", got.State)
		}
	})
	t.Run("spec_drafting", func(t *testing.T) {
		dataDir := t.TempDir()
		if err := request.SaveText(dataDir, "req-1", "text"); err != nil {
			t.Fatal(err)
		}
		r := request.New("req-1", t.TempDir(), "app", request.Source{Kind: request.SourceText}, time.Now())
		if err := r.StartSpecDrafting(time.Now()); err != nil {
			t.Fatal(err)
		}
		if err := r.Halt("spec drafting failed", time.Now()); err != nil {
			t.Fatal(err)
		}
		if err := r.Save(dataDir); err != nil {
			t.Fatal(err)
		}
		if handled, err := retryRequest(dp, dataDir, loadRequest(t, dataDir, "req-1"), "", time.Now()); err != nil || !handled {
			t.Fatalf("retryRequest: handled=%v err=%v", handled, err)
		}
		if got := loadRequest(t, dataDir, "req-1"); got.State != request.StateSpecDrafting {
			t.Errorf("State = %q, want spec_drafting", got.State)
		}
	})
}

// The whole staged path: spec_review -> oracle_drafting -> oracle_review (the
// default runner writes nothing) -> operator hand-writes oracle/ and approves
// -> planning -> plan_review, with the request-level oracle files hash-pinned
// through the approval.
func TestOracleStageFullLifecycle(t *testing.T) {
	dp := newTestDeps(t)
	dataDir, id := oracleStageFixture(t, true)
	drive := func(plan requestdriver.PlanTicketsRunner) {
		t.Helper()
		if err := driveRequests(dp, context.Background(), dataDir, noOracleScriptCfg, failingSpecDraftRunner(t), plan, runOracleDraftJob, failingBuildRunner(t)); err != nil {
			t.Fatal(err)
		}
	}
	drive(failingPlanTicketsRunner(t))
	if got := loadRequest(t, dataDir, id); got.State != request.StateOracleReview {
		t.Fatalf("State = %q, want oracle_review", got.State)
	}
	writeHandOracle(t, dataDir, id)
	// Planning assigns the oracle to tickets by manifest entry, so a
	// request-level oracle must carry one (it halts planning otherwise).
	manifest := `[{"criterion": "A retried POST /refunds with the same idempotency key returns the original result.", "oracle_file": "oracle_test.go", "criterion_index": 1}]`
	if err := os.WriteFile(filepath.Join(request.Dir(dataDir, id), request.RequestOracleDirName, "MANIFEST.json"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := request.Approve(dataDir, id, "alice", time.Now(), nil); err != nil {
		t.Fatal(err)
	}
	approved := loadRequest(t, dataDir, id)
	if approved.State != request.StatePlanning {
		t.Fatalf("State = %q, want planning", approved.State)
	}
	for _, rel := range []string{"oracle/oracle_test.go", "oracle/RUN_COMMAND.txt"} {
		if approved.ApprovedSHA256[rel] == "" {
			t.Errorf("%s not pinned: %v", rel, approved.ApprovedSHA256)
		}
	}
	plan, _ := stubPlanTicketsRunner([]requestdriver.DraftedTicket{{Filename: "001.spec.md", Content: validBrownfieldTicket("make verify", 1, 2)}}, &request.PlanEvidence{}, nil)
	drive(plan)
	got := loadRequest(t, dataDir, id)
	if got.State != request.StatePlanReview {
		t.Fatalf("State = %q, want plan_review (Error: %q)", got.State, got.Error)
	}
	// The pin survives planning: the oracle stays tamper-checked.
	if got.ApprovedSHA256["oracle/RUN_COMMAND.txt"] == "" {
		t.Errorf("request-level oracle pin lost by planning: %v", got.ApprovedSHA256)
	}
}

func moveToOracleReview(t *testing.T, dataDir, id string, draft request.OracleDraft) {
	t.Helper()
	r := loadRequest(t, dataDir, id)
	if err := r.CompleteOracleDrafting(draft, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}
}

// Approving after a failed draft with no oracle files is a legitimate skip but
// never silent: history, the request record and the CLI output all say so.
func TestApprovingAFailedDraftWithNoOracleWarnsThatTheStageIsSkipped(t *testing.T) {
	dp := newTestDeps(t)
	dataDir, id := oracleStageFixture(t, true)
	moveToOracleReview(t, dataDir, id, request.OracleDraft{Status: request.OracleDraftFailed, Detail: "boom"})
	var approveErr error
	out := captureStdout(t, func() { approveErr = approveMain(dp, []string{"-data-dir", dataDir, id}) })
	if approveErr != nil {
		t.Fatal(approveErr)
	}
	if !strings.Contains(out, "warning: "+request.OracleSkippedWarning(request.OracleDraftFailed)) {
		t.Errorf("CLI output lacks the warning: %q", out)
	}
	r := loadRequest(t, dataDir, id)
	if r.State != request.StatePlanning || r.OracleSkipWarning != request.OracleSkippedWarning(request.OracleDraftFailed) {
		t.Fatalf("state %q warning %q", r.State, r.OracleSkipWarning)
	}
	last := r.History[len(r.History)-1]
	if !strings.Contains(last.Reason, "oracle stage is being skipped") {
		t.Errorf("history reason %q does not say the oracle stage was skipped", last.Reason)
	}
	if got := loadRequest(t, dataDir, id); got.OracleSkipWarning == "" {
		t.Error("warning not persisted")
	}
}

func TestApprovingANonFailedDraftDoesNotWarn(t *testing.T) {
	t.Parallel()
	dataDir, id := oracleStageFixture(t, true)
	moveToOracleReview(t, dataDir, id, request.OracleDraft{Status: request.OracleNoneEligible, Detail: "none"})
	r, err := request.Approve(dataDir, id, "alice", time.Now(), nil)
	if err != nil {
		t.Fatalf("none_eligible skip: %v", err)
	}
	if r.OracleSkipWarning != "" {
		t.Fatalf("warned for none_eligible: %q", r.OracleSkipWarning)
	}
}

// The skip warning is cleared on every exit from the skipped state, so a
// request that later gets an oracle does not keep (or re-print) it.
func TestOracleSkipWarningIsClearedOnEveryLaterTransition(t *testing.T) {
	t.Parallel()
	stale := func() *request.Request {
		return &request.Request{ID: "r", State: request.StateOracleReview, OracleSkipWarning: "stale", OracleDraft: &request.OracleDraft{Status: request.OracleDraftFailed}}
	}
	now := time.Now()
	r := stale()
	if err := r.ApproveOracle("a", now); err != nil || r.OracleSkipWarning != "" {
		t.Errorf("ApproveOracle: err %v warning %q", err, r.OracleSkipWarning)
	}
	r = stale()
	if err := r.RejectOracle("a", "redo", now); err != nil || r.OracleSkipWarning != "" {
		t.Errorf("RejectOracle: err %v warning %q", err, r.OracleSkipWarning)
	}
	r = stale()
	r.State = request.StateOracleDrafting
	if err := r.CompleteOracleDrafting(request.OracleDraft{Status: request.OracleDrafted}, now); err != nil || r.OracleSkipWarning != "" {
		t.Errorf("CompleteOracleDrafting: err %v warning %q", err, r.OracleSkipWarning)
	}
	r = stale()
	r.State, r.HaltKind = request.StateHalted, request.HaltOracleMaterialize
	if err := r.ReturnToOracleReview("op", "", now); err != nil || r.OracleSkipWarning != "" {
		t.Errorf("ReturnToOracleReview: err %v warning %q", err, r.OracleSkipWarning)
	}
}

// A draft whose files were all deleted before approval is a skip too.
func TestApprovingADraftedStatusWithNoFilesWarns(t *testing.T) {
	t.Parallel()
	dataDir, id := oracleStageFixture(t, true)
	moveToOracleReview(t, dataDir, id, request.OracleDraft{Status: request.OracleDrafted, Files: []string{"gone_test.go"}})
	r, err := request.Approve(dataDir, id, "alice", time.Now(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if r.OracleSkipWarning != request.OracleSkippedWarning(request.OracleDrafted) {
		t.Errorf("warning = %q", r.OracleSkipWarning)
	}
}

// A rejection that is refused (wrong state) must leave the warning alone.
func TestRefusedOracleRejectionKeepsTheSkipWarning(t *testing.T) {
	t.Parallel()
	r := &request.Request{ID: "r", State: request.StatePlanning, OracleSkipWarning: "keep me"}
	if err := r.RejectOracle("a", "redo", time.Now()); err == nil {
		t.Fatal("expected the rejection to be refused outside oracle_review")
	}
	if r.OracleSkipWarning != "keep me" {
		t.Errorf("warning cleared by a failed rejection: %q", r.OracleSkipWarning)
	}
}
