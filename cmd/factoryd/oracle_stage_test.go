package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"buildgate/internal/request"
	"buildgate/internal/requestdriver"
	"buildgate/internal/requestdriver/requestdrivertest"
)

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

// The interim default runner records not_implemented, writes no files, and
// lands in oracle_review with the first reminder sent.
func TestOracleDraftingDefaultRunnerMovesToReviewWithoutFiles(t *testing.T) {
	dp := newTestDeps(t)
	dataDir, id := requestdrivertest.OracleStageFixture(t, true)
	if got := requestdrivertest.LoadRequest(t, dataDir, id); got.State != request.StateOracleDrafting {
		t.Fatalf("State after spec approval = %q, want oracle_drafting", got.State)
	}
	if err := driveRequests(dp, context.Background(), dataDir, requestdrivertest.NoOracleScriptCfg, requestdrivertest.FailingSpecDraftRunner(t), requestdrivertest.FailingPlanTicketsRunner(t), runOracleDraftJob, requestdrivertest.FailingBuildRunner(t)); err != nil {
		t.Fatal(err)
	}
	got := requestdrivertest.LoadRequest(t, dataDir, id)
	if got.State != request.StateOracleReview {
		t.Fatalf("State = %q, want oracle_review", got.State)
	}
	if got.OracleDraft == nil || got.OracleDraft.Status != request.OracleNotImplemented {
		t.Fatalf("OracleDraft = %+v, want not_implemented", got.OracleDraft)
	}
	if _, err := os.Stat(filepath.Join(request.Dir(dataDir, id), request.RequestOracleDirName)); !os.IsNotExist(err) {
		t.Errorf("default runner must write no oracle/ directory (stat err = %v)", err)
	}
	if got.NotifyCount != 1 || requestdrivertest.CountNotificationLogLines(t, dataDir, id) != 1 {
		t.Errorf("NotifyCount = %d, log lines = %d, want the immediate oracle_review reminder", got.NotifyCount, requestdrivertest.CountNotificationLogLines(t, dataDir, id))
	}
	// oracle_review is a human wait: further passes leave it alone.
	if err := driveRequests(dp, context.Background(), dataDir, requestdriver.WorkerConfig{}, requestdrivertest.FailingSpecDraftRunner(t), requestdrivertest.FailingPlanTicketsRunner(t), requestdrivertest.FailingOracleDraftRunner(t), requestdrivertest.FailingBuildRunner(t)); err != nil {
		t.Fatal(err)
	}
	if got := requestdrivertest.LoadRequest(t, dataDir, id); got.State != request.StateOracleReview {
		t.Errorf("State = %q, want oracle_review untouched", got.State)
	}
}

// A spec edited after approval halts oracle_drafting (the drafter's input
// changed), like planning -- and `retry` resumes into oracle_drafting rather
// than skipping ahead to planning, because the approved spec.md on disk would
// otherwise send it there.
func TestHaltedOracleDraftingIsRetryableIntoOracleDrafting(t *testing.T) {
	dp := newTestDeps(t)
	dataDir, id := requestdrivertest.OracleStageFixture(t, true)
	if err := os.WriteFile(requestdriver.RequestSpecPath(dataDir, id), []byte(requestdrivertest.TwoCriteriaSpec+"\nedited\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := driveRequests(dp, context.Background(), dataDir, requestdriver.WorkerConfig{}, requestdrivertest.FailingSpecDraftRunner(t), requestdrivertest.FailingPlanTicketsRunner(t), requestdrivertest.FailingOracleDraftRunner(t), requestdrivertest.FailingBuildRunner(t)); err != nil {
		t.Fatal(err)
	}
	halted := requestdrivertest.LoadRequest(t, dataDir, id)
	if halted.State != request.StateHalted {
		t.Fatalf("State = %q, want halted", halted.State)
	}
	handled, err := retryRequest(dp, dataDir, halted, "", time.Now())
	if err != nil || !handled {
		t.Fatalf("retryRequest: handled=%v err=%v", handled, err)
	}
	if got := requestdrivertest.LoadRequest(t, dataDir, id); got.State != request.StateOracleDrafting || got.Error != "" {
		t.Errorf("after retry: State = %q, Error = %q; want oracle_drafting with the error cleared", got.State, got.Error)
	}
}

// A request halted in planning after a completed oracle stage still resumes
// into planning: the HaltedFrom rule must not drag it back into the oracle
// stage.
func TestHaltedPlanningAfterOracleStageResumesIntoPlanning(t *testing.T) {
	dp := newTestDeps(t)
	dataDir, id := requestdrivertest.OracleStageFixture(t, true)
	if err := driveRequests(dp, context.Background(), dataDir, requestdrivertest.NoOracleScriptCfg, requestdrivertest.FailingSpecDraftRunner(t), requestdrivertest.FailingPlanTicketsRunner(t), runOracleDraftJob, requestdrivertest.FailingBuildRunner(t)); err != nil {
		t.Fatal(err)
	}
	if _, err := request.Approve(dataDir, id, "alice", time.Now(), nil); err != nil {
		t.Fatal(err)
	}
	r := requestdrivertest.LoadRequest(t, dataDir, id)
	if err := r.Halt("plan drafting failed: boom", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	if handled, err := retryRequest(dp, dataDir, requestdrivertest.LoadRequest(t, dataDir, id), "", time.Now()); err != nil || !handled {
		t.Fatalf("retryRequest: handled=%v err=%v", handled, err)
	}
	if got := requestdrivertest.LoadRequest(t, dataDir, id); got.State != request.StatePlanning {
		t.Errorf("State = %q, want planning", got.State)
	}
}

// Rejecting at oracle_review returns to oracle_drafting; the redraft's runner
// input carries the operator's reasons on a separate feedback file, request.md
// is untouched, and the previous oracle/ files are snapshotted.
func TestOracleRejectFeedsTheDrafterThroughFeedbackPath(t *testing.T) {
	dp := newTestDeps(t)
	dataDir, id := requestdrivertest.OracleStageFixture(t, true)
	if err := driveRequests(dp, context.Background(), dataDir, requestdrivertest.NoOracleScriptCfg, requestdrivertest.FailingSpecDraftRunner(t), requestdrivertest.FailingPlanTicketsRunner(t), runOracleDraftJob, requestdrivertest.FailingBuildRunner(t)); err != nil {
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
	if got := requestdrivertest.LoadRequest(t, dataDir, id); got.State != request.StateOracleDrafting {
		t.Fatalf("State = %q, want oracle_drafting", got.State)
	}
	if after, _ := os.ReadFile(request.TextPath(dataDir, id)); string(after) != string(requestMD) {
		t.Errorf("request.md changed by an oracle rejection: %q", after)
	}

	runner, calls := requestdrivertest.StubOracleDraftRunner(request.OracleDraft{Status: request.OracleDrafted}, nil)
	if err := driveRequests(dp, context.Background(), dataDir, requestdriver.WorkerConfig{}, requestdrivertest.FailingSpecDraftRunner(t), requestdrivertest.FailingPlanTicketsRunner(t), runner, requestdrivertest.FailingBuildRunner(t)); err != nil {
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
	if got := requestdrivertest.LoadRequest(t, dataDir, id); got.State != request.StateOracleReview || got.OracleDraft.Status != request.OracleDrafted {
		t.Errorf("State = %q, OracleDraft = %+v", got.State, got.OracleDraft)
	}
	revs, err := request.ListRevisions(dataDir, id)
	if err != nil || len(revs) != 1 || len(revs[0].Files) != 2 {
		t.Errorf("revisions = %+v, %v; want one snapshot of the two oracle files", revs, err)
	}
}

// The HaltedFrom rule must not change retry routing for a flag-less request:
// halted in planning resumes into planning, halted in spec_drafting (no
// approved spec) resumes into spec_drafting.
func TestFlaglessHaltedRetryRoutingUnchanged(t *testing.T) {
	dp := newTestDeps(t)
	t.Parallel()
	t.Run("planning", func(t *testing.T) {
		dataDir, id := requestdrivertest.OracleStageFixture(t, false)
		r := requestdrivertest.LoadRequest(t, dataDir, id)
		if err := r.Halt("plan drafting failed", time.Now()); err != nil {
			t.Fatal(err)
		}
		if err := r.Save(dataDir); err != nil {
			t.Fatal(err)
		}
		if handled, err := retryRequest(dp, dataDir, requestdrivertest.LoadRequest(t, dataDir, id), "", time.Now()); err != nil || !handled {
			t.Fatalf("retryRequest: handled=%v err=%v", handled, err)
		}
		if got := requestdrivertest.LoadRequest(t, dataDir, id); got.State != request.StatePlanning {
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
		if handled, err := retryRequest(dp, dataDir, requestdrivertest.LoadRequest(t, dataDir, "req-1"), "", time.Now()); err != nil || !handled {
			t.Fatalf("retryRequest: handled=%v err=%v", handled, err)
		}
		if got := requestdrivertest.LoadRequest(t, dataDir, "req-1"); got.State != request.StateSpecDrafting {
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
	dataDir, id := requestdrivertest.OracleStageFixture(t, true)
	drive := func(plan requestdriver.PlanTicketsRunner) {
		t.Helper()
		if err := driveRequests(dp, context.Background(), dataDir, requestdrivertest.NoOracleScriptCfg, requestdrivertest.FailingSpecDraftRunner(t), plan, runOracleDraftJob, requestdrivertest.FailingBuildRunner(t)); err != nil {
			t.Fatal(err)
		}
	}
	drive(requestdrivertest.FailingPlanTicketsRunner(t))
	if got := requestdrivertest.LoadRequest(t, dataDir, id); got.State != request.StateOracleReview {
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
	approved := requestdrivertest.LoadRequest(t, dataDir, id)
	if approved.State != request.StatePlanning {
		t.Fatalf("State = %q, want planning", approved.State)
	}
	for _, rel := range []string{"oracle/oracle_test.go", "oracle/RUN_COMMAND.txt"} {
		if approved.ApprovedSHA256[rel] == "" {
			t.Errorf("%s not pinned: %v", rel, approved.ApprovedSHA256)
		}
	}
	plan, _ := requestdrivertest.StubPlanTicketsRunner([]requestdriver.DraftedTicket{{Filename: "001.spec.md", Content: requestdrivertest.ValidBrownfieldTicket("make verify", 1, 2)}}, &request.PlanEvidence{}, nil)
	drive(plan)
	got := requestdrivertest.LoadRequest(t, dataDir, id)
	if got.State != request.StatePlanReview {
		t.Fatalf("State = %q, want plan_review (Error: %q)", got.State, got.Error)
	}
	// The pin survives planning: the oracle stays tamper-checked.
	if got.ApprovedSHA256["oracle/RUN_COMMAND.txt"] == "" {
		t.Errorf("request-level oracle pin lost by planning: %v", got.ApprovedSHA256)
	}
}

// Approving after a failed draft with no oracle files is a legitimate skip but
// never silent: history, the request record and the CLI output all say so.
func TestApprovingAFailedDraftWithNoOracleWarnsThatTheStageIsSkipped(t *testing.T) {
	dp := newTestDeps(t)
	dataDir, id := requestdrivertest.OracleStageFixture(t, true)
	requestdrivertest.MoveToOracleReview(t, dataDir, id, request.OracleDraft{Status: request.OracleDraftFailed, Detail: "boom"})
	var approveErr error
	out := captureStdout(t, func() { approveErr = approveMain(dp, []string{"-data-dir", dataDir, id}) })
	if approveErr != nil {
		t.Fatal(approveErr)
	}
	if !strings.Contains(out, "warning: "+request.OracleSkippedWarning(request.OracleDraftFailed)) {
		t.Errorf("CLI output lacks the warning: %q", out)
	}
	r := requestdrivertest.LoadRequest(t, dataDir, id)
	if r.State != request.StatePlanning || r.OracleSkipWarning != request.OracleSkippedWarning(request.OracleDraftFailed) {
		t.Fatalf("state %q warning %q", r.State, r.OracleSkipWarning)
	}
	last := r.History[len(r.History)-1]
	if !strings.Contains(last.Reason, "oracle stage is being skipped") {
		t.Errorf("history reason %q does not say the oracle stage was skipped", last.Reason)
	}
	if got := requestdrivertest.LoadRequest(t, dataDir, id); got.OracleSkipWarning == "" {
		t.Error("warning not persisted")
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
