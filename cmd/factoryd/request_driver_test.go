package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"buildgate/internal/request"
	"buildgate/internal/requestdriver"
	"buildgate/internal/run"
	"buildgate/internal/store"
)

// errUnreachableRelay stands in for a real spec-drafting job failure
// (e.g. the sandboxed worker's relay being unreachable) in the tests
// below -- its own text is what those tests look for in the resulting
// halt reason and notification.
var errUnreachableRelay = errors.New("relay unreachable")

const canonicalValidSpec = `# Spec

## Problem

Refunds can be double-processed on retry.

## Scope

The /refunds endpoint only.

## Non-goals

Not touching /charges.

## Affected services and packages

internal/payments

## Acceptance criteria

1. A retried POST /refunds with the same idempotency key returns the original result.

## Risks

None known.

## Open questions

None.
`

// stubSpecDraftRunner returns a fixed (specMD, evidence, err) for every
// call, recording how many times (and with which request) it was
// invoked -- the same "inject a stub instead of a real subprocess" shape
// worker_config_test.go's own stub ticketRunner uses for the worker.
func stubSpecDraftRunner(specMD string, evidence *request.SpecEvidence, err error) (requestdriver.SpecDraftRunner, *int) {
	calls := 0
	runner := func(ctx context.Context, dataDir string, r *request.Request, cfg requestdriver.WorkerConfig) (string, *request.SpecEvidence, error) {
		calls++
		return specMD, evidence, err
	}
	return runner, &calls
}

// failingSpecDraftRunner fails the test outright if ever called -- used
// where driveRequests must not touch the spec-drafting job at all (e.g. a
// request already past spec_drafting, or the pure submitted->
// spec_drafting move which needs no job).
func failingSpecDraftRunner(t *testing.T) requestdriver.SpecDraftRunner {
	return func(ctx context.Context, dataDir string, r *request.Request, cfg requestdriver.WorkerConfig) (string, *request.SpecEvidence, error) {
		t.Fatal("specDraftRunner must not be called")
		return "", nil, nil
	}
}

// failingPlanTicketsRunner is failingSpecDraftRunner's own sibling for
// the plan-drafting job -- used everywhere driveRequests must not touch
// planning at all.
func failingPlanTicketsRunner(t *testing.T) requestdriver.PlanTicketsRunner {
	return func(ctx context.Context, dataDir string, r *request.Request, cfg requestdriver.WorkerConfig, verifyCommand string) ([]requestdriver.DraftedTicket, *request.PlanEvidence, error) {
		t.Fatal("planTicketsRunner must not be called")
		return nil, nil, nil
	}
}

// failingOracleDraftRunner is failingSpecDraftRunner's sibling for the oracle
// drafting job -- used everywhere driveRequests must not touch
// oracle_drafting (every pre-existing test: no request there sets
// -draft-oracles).
func failingOracleDraftRunner(t *testing.T) requestdriver.OracleDraftRunner {
	return func(ctx context.Context, in requestdriver.OracleDraftInput) (request.OracleDraft, error) {
		t.Fatal("oracleDraftRunner must not be called")
		return request.OracleDraft{}, nil
	}
}

// failingBuildRunner is failingSpecDraftRunner's own sibling for a
// ticket build (ticketRunner) -- used everywhere driveRequests must not
// touch building at all.
func failingBuildRunner(t *testing.T) requestdriver.TicketRunner {
	return func(ctx context.Context, args []string, onReady func(*run.Run)) error {
		t.Fatal("ticketRunner (ticket build) must not be called")
		return nil
	}
}

// TestDriveRequestsAdvancesSubmittedToSpecReview covers the full
// submitted -> spec_drafting -> spec_review move across two
// driveRequests calls (the worker's loop calls this once per poll
// iteration -- see its own doc comment), with spec.md ending up as
// exactly what the (stubbed) spec-drafting job produced.
func TestDriveRequestsAdvancesSubmittedToSpecReview(t *testing.T) {
	dp := newTestDeps(t)
	dataDir := t.TempDir()
	if err := request.SaveText(dataDir, "req-1", "Add idempotency keys to POST /refunds"); err != nil {
		t.Fatal(err)
	}
	r := request.New("req-1", "/repos/app", "app", request.Source{Kind: request.SourceText}, time.Now())
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}

	runner, calls := stubSpecDraftRunner(canonicalValidSpec, &request.SpecEvidence{AgentExitCode: 0, DurationS: 1.5}, nil)

	if err := driveRequests(dp, context.Background(), dataDir, requestdriver.WorkerConfig{}, failingSpecDraftRunner(t), failingPlanTicketsRunner(t), failingOracleDraftRunner(t), failingBuildRunner(t)); err != nil {
		t.Fatalf("driveRequests (1st call): %v", err)
	}
	loaded, err := request.Load(dataDir, "req-1")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.State != request.StateSpecDrafting {
		t.Fatalf("State after 1st call = %q, want %q", loaded.State, request.StateSpecDrafting)
	}

	if err := driveRequests(dp, context.Background(), dataDir, requestdriver.WorkerConfig{}, runner, failingPlanTicketsRunner(t), failingOracleDraftRunner(t), failingBuildRunner(t)); err != nil {
		t.Fatalf("driveRequests (2nd call): %v", err)
	}
	if *calls != 1 {
		t.Fatalf("spec-draft runner called %d times, want 1", *calls)
	}
	loaded, err = request.Load(dataDir, "req-1")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.State != request.StateSpecReview {
		t.Fatalf("State after 2nd call = %q, want %q", loaded.State, request.StateSpecReview)
	}
	if loaded.SpecEvidence == nil || loaded.SpecEvidence.DurationS != 1.5 {
		t.Errorf("SpecEvidence = %+v, want the stubbed evidence recorded on the request", loaded.SpecEvidence)
	}

	specBytes, err := os.ReadFile(requestdriver.RequestSpecPath(dataDir, "req-1"))
	if err != nil {
		t.Fatalf("read spec.md: %v", err)
	}
	if string(specBytes) != canonicalValidSpec {
		t.Errorf("spec.md = %q, want the drafted spec verbatim", string(specBytes))
	}
}

// TestDriveRequestsIsANoOpWhenNothingIsInADrivenState covers a request
// already in spec_review (or any later state): driveRequests must leave
// it untouched -- approval/planning/building/PR review are later work
// packages' job. The spec-draft runner must never be invoked.
func TestDriveRequestsIsANoOpWhenNothingIsInADrivenState(t *testing.T) {
	dp := newTestDeps(t)
	dataDir := t.TempDir()
	if err := request.SaveText(dataDir, "req-1", "text"); err != nil {
		t.Fatal(err)
	}
	r := request.New("req-1", "/repos/app", "app", request.Source{Kind: request.SourceText}, time.Now())
	r.State = request.StateSpecReview
	before := *r
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}

	if err := driveRequests(dp, context.Background(), dataDir, requestdriver.WorkerConfig{}, failingSpecDraftRunner(t), failingPlanTicketsRunner(t), failingOracleDraftRunner(t), failingBuildRunner(t)); err != nil {
		t.Fatalf("driveRequests: %v", err)
	}

	loaded, err := request.Load(dataDir, "req-1")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.State != before.State {
		t.Errorf("State = %q, want unchanged %q", loaded.State, before.State)
	}
}

// TestDriveRequestsPicksOldestSubmittedFirst covers the "pick the oldest
// request in a machine state" part of the plan's design: with two
// requests both in a driven state, the older (by SubmittedAt) advances
// first. Both are in StateSubmitted, so the spec-draft runner is never
// called (that's a pure state move).
func TestDriveRequestsPicksOldestSubmittedFirst(t *testing.T) {
	dp := newTestDeps(t)
	dataDir := t.TempDir()
	older := request.New("older", "/repos/app", "app", request.Source{Kind: request.SourceText}, time.Now().Add(-time.Hour))
	newer := request.New("newer", "/repos/app", "app", request.Source{Kind: request.SourceText}, time.Now())
	for _, r := range []*request.Request{older, newer} {
		if err := request.SaveText(dataDir, r.ID, "text"); err != nil {
			t.Fatal(err)
		}
		if err := r.Save(dataDir); err != nil {
			t.Fatal(err)
		}
	}

	if err := driveRequests(dp, context.Background(), dataDir, requestdriver.WorkerConfig{}, failingSpecDraftRunner(t), failingPlanTicketsRunner(t), failingOracleDraftRunner(t), failingBuildRunner(t)); err != nil {
		t.Fatalf("driveRequests: %v", err)
	}

	loadedOlder, err := request.Load(dataDir, "older")
	if err != nil {
		t.Fatal(err)
	}
	loadedNewer, err := request.Load(dataDir, "newer")
	if err != nil {
		t.Fatal(err)
	}
	if loadedOlder.State != request.StateSpecDrafting {
		t.Errorf("older.State = %q, want %q (the oldest request should advance first)", loadedOlder.State, request.StateSpecDrafting)
	}
	if loadedNewer.State != request.StateSubmitted {
		t.Errorf("newer.State = %q, want unchanged %q", loadedNewer.State, request.StateSubmitted)
	}
}

// TestAdvanceSpecDraftingStampsCompletionAfterJobReturns is a regression
// test: the spec_review transition's timestamp must reflect when the
// drafting job actually finished, not when driveRequests started it, so
// WaitingSince and the "spec drafted" History entry don't over-report the
// operator's wait.
func TestAdvanceSpecDraftingStampsCompletionAfterJobReturns(t *testing.T) {
	dp := newTestDeps(t)
	dataDir := t.TempDir()
	if err := request.SaveText(dataDir, "req-1", "text"); err != nil {
		t.Fatal(err)
	}
	r := request.New("req-1", "/repos/app", "app", request.Source{Kind: request.SourceText}, time.Now())
	r.State = request.StateSpecDrafting
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}

	const jobDuration = 50 * time.Millisecond
	before := time.Now()
	runner := func(ctx context.Context, dataDir string, r *request.Request, cfg requestdriver.WorkerConfig) (string, *request.SpecEvidence, error) {
		time.Sleep(jobDuration)
		return canonicalValidSpec, &request.SpecEvidence{AgentExitCode: 0}, nil
	}
	if err := driveRequests(dp, context.Background(), dataDir, requestdriver.WorkerConfig{}, runner, failingPlanTicketsRunner(t), failingOracleDraftRunner(t), failingBuildRunner(t)); err != nil {
		t.Fatalf("driveRequests: %v", err)
	}

	loaded, err := request.Load(dataDir, "req-1")
	if err != nil {
		t.Fatal(err)
	}
	entered, err := time.Parse(time.RFC3339Nano, loaded.EnteredAt)
	if err != nil {
		t.Fatalf("parse EnteredAt %q: %v", loaded.EnteredAt, err)
	}
	if entered.Before(before.Add(jobDuration)) {
		t.Errorf("EnteredAt = %s, want it stamped after the %s job returned (started at %s)", entered, jobDuration, before)
	}
}

// TestAdvanceSpecDraftingAccumulatesSpendAcrossRedraft covers the
// re-draft accumulation request.JobSpend.Add exists for: a spec_review
// rejection sends the request back through spec_drafting a second time,
// which replaces r.SpecEvidence wholesale (advanceSpecDrafting's own
// `r.SpecEvidence = evidence`) -- without accumulating the first pass's
// own Spend into the second's before that replacement, a real,
// already-incurred relay spend from the first attempt would simply
// vanish from the record the moment the redraft succeeded.
func TestAdvanceSpecDraftingAccumulatesSpendAcrossRedraft(t *testing.T) {
	dataDir := t.TempDir()
	if err := request.SaveText(dataDir, "req-1", "text"); err != nil {
		t.Fatal(err)
	}
	r := request.New("req-1", "/repos/app", "app", request.Source{Kind: request.SourceText}, time.Now())
	r.State = request.StateSpecDrafting
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}

	firstSpend := &request.JobSpend{Role: "planning", Model: "gpt-5.6-luna", InputTokens: 100, OutputTokens: 50, CostMicroUSD: 1000}
	firstRunner := func(ctx context.Context, dataDir string, r *request.Request, cfg requestdriver.WorkerConfig) (string, *request.SpecEvidence, error) {
		return canonicalValidSpec, &request.SpecEvidence{AgentExitCode: 0, Spend: firstSpend}, nil
	}
	if err := requestdriver.AdvanceSpecDrafting(context.Background(), dataDir, r, requestdriver.WorkerConfig{}, firstRunner, time.Now()); err != nil {
		t.Fatalf("advanceSpecDrafting (first pass): %v", err)
	}
	if r.SpecEvidence == nil || r.SpecEvidence.Spend == nil {
		t.Fatalf("SpecEvidence.Spend = nil after the first pass, want %+v", firstSpend)
	}
	if got := *r.SpecEvidence.Spend; got.InputTokens != 100 || got.OutputTokens != 50 || got.CostMicroUSD != 1000 {
		t.Fatalf("SpecEvidence.Spend after first pass = %+v, want %+v", got, *firstSpend)
	}

	if err := r.RejectSpec("operator", "please redo the scope section", time.Now()); err != nil {
		t.Fatalf("RejectSpec: %v", err)
	}
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}

	secondSpend := &request.JobSpend{Role: "planning", Model: "gpt-5.6-luna", InputTokens: 40, OutputTokens: 20, CostMicroUSD: 400, SpendPartial: true}
	secondRunner := func(ctx context.Context, dataDir string, r *request.Request, cfg requestdriver.WorkerConfig) (string, *request.SpecEvidence, error) {
		return canonicalValidSpec, &request.SpecEvidence{AgentExitCode: 0, Spend: secondSpend}, nil
	}
	if err := requestdriver.AdvanceSpecDrafting(context.Background(), dataDir, r, requestdriver.WorkerConfig{}, secondRunner, time.Now()); err != nil {
		t.Fatalf("advanceSpecDrafting (second pass): %v", err)
	}

	if r.SpecEvidence == nil || r.SpecEvidence.Spend == nil {
		t.Fatal("SpecEvidence.Spend = nil after the redraft, want the two passes summed")
	}
	got := *r.SpecEvidence.Spend
	wantInput, wantOutput, wantCost := int64(140), int64(70), int64(1400)
	if got.InputTokens != wantInput || got.OutputTokens != wantOutput || got.CostMicroUSD != wantCost {
		t.Errorf("SpecEvidence.Spend after redraft = %+v, want input=%d output=%d cost=%d (both passes summed)", got, wantInput, wantOutput, wantCost)
	}
	if !got.SpendPartial {
		t.Errorf("SpecEvidence.Spend.SpendPartial = false, want true (sticky once any contributing pass was partial)")
	}
}

// TestAdvanceSpecDraftingWritesFeedbackFileFromSpecRejection is a
// regression test: a spec_review rejection's reason must be written to
// request.SpecFeedbackPath before
// the drafting job runs, capped and stage-scoped exactly like
// advanceOracleDrafting's own oracle-feedback.md (a plan_review or
// oracle_review rejection recorded on the same request must not leak in).
func TestAdvanceSpecDraftingWritesFeedbackFileFromSpecRejection(t *testing.T) {
	dp := newTestDeps(t)
	dataDir := t.TempDir()
	if err := request.SaveText(dataDir, "req-1", "text"); err != nil {
		t.Fatal(err)
	}
	r := request.New("req-1", "/repos/app", "app", request.Source{Kind: request.SourceText}, time.Now())
	r.State = request.StateSpecDrafting
	r.Rejections = []request.Rejection{
		{By: "alice", At: "2026-09-24T00:00:00Z", Reason: "require TypeError; name files test_sub.py/test_div.py", FromState: request.StateSpecReview},
		{By: "bob", At: "2026-09-23T00:00:00Z", Reason: "unrelated plan reason", FromState: request.StatePlanReview},
	}
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}

	runner, _ := stubSpecDraftRunner(canonicalValidSpec, &request.SpecEvidence{AgentExitCode: 0}, nil)
	if err := driveRequests(dp, context.Background(), dataDir, requestdriver.WorkerConfig{}, runner, failingPlanTicketsRunner(t), failingOracleDraftRunner(t), failingBuildRunner(t)); err != nil {
		t.Fatalf("driveRequests: %v", err)
	}

	got, err := os.ReadFile(request.SpecFeedbackPath(dataDir, "req-1"))
	if err != nil {
		t.Fatalf("read spec-feedback.md: %v", err)
	}
	if !strings.Contains(string(got), "require TypeError; name files test_sub.py/test_div.py") {
		t.Errorf("spec-feedback.md = %q, want the spec_review rejection reason", got)
	}
	if strings.Contains(string(got), "unrelated plan reason") {
		t.Errorf("spec-feedback.md = %q, want no plan_review rejection reason", got)
	}
}

// TestAdvanceSpecDraftingWritesNoFeedbackFileWithoutASpecRejection covers
// the common case: a request in spec_drafting for the first time (no
// rejections at all) writes no feedback file.
func TestAdvanceSpecDraftingWritesNoFeedbackFileWithoutASpecRejection(t *testing.T) {
	dp := newTestDeps(t)
	dataDir := t.TempDir()
	if err := request.SaveText(dataDir, "req-1", "text"); err != nil {
		t.Fatal(err)
	}
	r := request.New("req-1", "/repos/app", "app", request.Source{Kind: request.SourceText}, time.Now())
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}

	runner, _ := stubSpecDraftRunner(canonicalValidSpec, &request.SpecEvidence{AgentExitCode: 0}, nil)
	if err := driveRequests(dp, context.Background(), dataDir, requestdriver.WorkerConfig{}, failingSpecDraftRunner(t), failingPlanTicketsRunner(t), failingOracleDraftRunner(t), failingBuildRunner(t)); err != nil {
		t.Fatalf("driveRequests (1st call): %v", err)
	}
	if err := driveRequests(dp, context.Background(), dataDir, requestdriver.WorkerConfig{}, runner, failingPlanTicketsRunner(t), failingOracleDraftRunner(t), failingBuildRunner(t)); err != nil {
		t.Fatalf("driveRequests (2nd call): %v", err)
	}

	if _, err := os.Stat(request.SpecFeedbackPath(dataDir, "req-1")); !os.IsNotExist(err) {
		t.Errorf("spec-feedback.md stat err = %v, want IsNotExist", err)
	}
}

// TestAdvanceSpecDraftingJobFailureHaltsWithReasonAndNotification covers
// the plan's own "an empty or malformed draft halts the request with a
// reason and a notification" requirement for the job-failure half of
// that: the spec-drafting job itself returning an error moves the
// request to halted, naming why, and a durable notification is recorded
// before DispatchExternal ever runs.
func TestAdvanceSpecDraftingJobFailureHaltsWithReasonAndNotification(t *testing.T) {
	dp := newTestDeps(t)
	dataDir := t.TempDir()
	if err := request.SaveText(dataDir, "req-1", "text"); err != nil {
		t.Fatal(err)
	}
	r := request.New("req-1", "/repos/app", "app", request.Source{Kind: request.SourceText}, time.Now())
	r.State = request.StateSpecDrafting
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}

	runner, _ := stubSpecDraftRunner("", nil, errUnreachableRelay)
	if err := driveRequests(dp, context.Background(), dataDir, requestdriver.WorkerConfig{}, runner, failingPlanTicketsRunner(t), failingOracleDraftRunner(t), failingBuildRunner(t)); err != nil {
		t.Fatalf("driveRequests: %v", err)
	}

	loaded, err := request.Load(dataDir, "req-1")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.State != request.StateHalted {
		t.Fatalf("State = %q, want %q", loaded.State, request.StateHalted)
	}
	if !strings.Contains(loaded.Error, "spec drafting failed") || !strings.Contains(loaded.Error, errUnreachableRelay.Error()) {
		t.Errorf("Error = %q, want it to name the job failure", loaded.Error)
	}

	logBytes, err := os.ReadFile(filepath.Join(request.Dir(dataDir, "req-1"), "notifications.log"))
	if err != nil {
		t.Fatalf("read notifications.log: %v", err)
	}
	if !strings.Contains(string(logBytes), "spec drafting failed") {
		t.Errorf("notifications.log = %q, want it to contain the halt reason", string(logBytes))
	}
	// A request's own notification carries RequestID, never its ID in
	// RunID -- see run.NotificationRecord.RequestID.
	if !strings.Contains(string(logBytes), `"request_id":"req-1"`) || strings.Contains(string(logBytes), `"run_id":"req-1"`) {
		t.Errorf("notifications.log = %q, want request_id req-1 and no run_id req-1", string(logBytes))
	}
}

// TestAdvanceSpecDraftingMalformedSkeletonHaltsNamingHeading covers the
// other half of "an empty or malformed draft halts the request with a
// reason": a job that succeeds but returns a spec.md missing a required
// heading must halt the request, naming that heading.
func TestAdvanceSpecDraftingMalformedSkeletonHaltsNamingHeading(t *testing.T) {
	dp := newTestDeps(t)
	dataDir := t.TempDir()
	if err := request.SaveText(dataDir, "req-1", "text"); err != nil {
		t.Fatal(err)
	}
	r := request.New("req-1", "/repos/app", "app", request.Source{Kind: request.SourceText}, time.Now())
	r.State = request.StateSpecDrafting
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}

	malformed := "# Spec\n\n## Problem\n\nx\n" // missing every later heading
	runner, _ := stubSpecDraftRunner(malformed, &request.SpecEvidence{}, nil)
	if err := driveRequests(dp, context.Background(), dataDir, requestdriver.WorkerConfig{}, runner, failingPlanTicketsRunner(t), failingOracleDraftRunner(t), failingBuildRunner(t)); err != nil {
		t.Fatalf("driveRequests: %v", err)
	}

	loaded, err := request.Load(dataDir, "req-1")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.State != request.StateHalted {
		t.Fatalf("State = %q, want %q", loaded.State, request.StateHalted)
	}
	if !strings.Contains(loaded.Error, "## Scope") {
		t.Errorf("Error = %q, want it to name the missing heading %q", loaded.Error, "## Scope")
	}
	if _, err := os.Stat(requestdriver.RequestSpecPath(dataDir, "req-1")); !os.IsNotExist(err) {
		t.Errorf("spec.md should not be written for an invalid draft (stat err = %v)", err)
	}
}

// TestAdvanceSpecDraftingStripsCommitMessageCriterionBeforePersisting is a
// regression for the 2026-09-17 multi-repo validation finding: a drafted
// spec asking the reviewer to check the commit message/subject line
// itself is unfulfillable by factoryd's own safety-net commit (see
// internal/request.StripCommitMessageCriteria's own doc comment), so the
// mechanical backstop must run before spec.md is ever persisted -- never
// leaving a criterion on disk that a human approves but no later layer
// actually checks.
func TestAdvanceSpecDraftingStripsCommitMessageCriterionBeforePersisting(t *testing.T) {
	dp := newTestDeps(t)
	dataDir := t.TempDir()
	if err := request.SaveText(dataDir, "req-1", "text"); err != nil {
		t.Fatal(err)
	}
	r := request.New("req-1", "/repos/app", "app", request.Source{Kind: request.SourceText}, time.Now())
	r.State = request.StateSpecDrafting
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}

	draft := `# Spec

## Problem

Refunds can be double-processed on retry.

## Scope

The /refunds endpoint only.

## Non-goals

Not touching /charges.

## Affected services and packages

internal/payments

## Acceptance criteria

1. A retried POST /refunds with the same idempotency key returns the original result.
2. The commit subject line begins with ` + "`ticket(refunds):`" + `.

## Risks

None known.

## Open questions

None.
`
	runner, _ := stubSpecDraftRunner(draft, &request.SpecEvidence{}, nil)
	if err := driveRequests(dp, context.Background(), dataDir, requestdriver.WorkerConfig{}, runner, failingPlanTicketsRunner(t), failingOracleDraftRunner(t), failingBuildRunner(t)); err != nil {
		t.Fatalf("driveRequests: %v", err)
	}

	persisted, err := os.ReadFile(requestdriver.RequestSpecPath(dataDir, "req-1"))
	if err != nil {
		t.Fatalf("read persisted spec.md: %v", err)
	}
	criteria, err := request.SpecAcceptanceCriteria(string(persisted))
	if err != nil {
		t.Fatalf("SpecAcceptanceCriteria(persisted): %v", err)
	}
	if len(criteria) != 1 {
		t.Fatalf("persisted spec.md has %d criteria, want 1 (commit-message criterion should have been stripped): %v", len(criteria), criteria)
	}
	if strings.Contains(strings.ToLower(criteria[0]), "commit") {
		t.Errorf("surviving criterion still mentions commit: %q", criteria[0])
	}
}

// TestAdvanceSpecDraftingHaltsWhenStrippingLeavesNoCriteria is a
// regression for an adversarial-review finding on this same branch:
// ValidateSpecSkeleton only requires the "## Acceptance criteria" section
// to have at least one non-blank line, not a valid numbered item, so a
// draft whose ONLY criterion is commit-message-related would pass that
// first check, then StripCommitMessageCriteria removes it, leaving zero
// criteria -- which must halt the request with a clear reason here, not
// persist an invalid spec.md that only fails confusingly later.
func TestAdvanceSpecDraftingHaltsWhenStrippingLeavesNoCriteria(t *testing.T) {
	dp := newTestDeps(t)
	dataDir := t.TempDir()
	if err := request.SaveText(dataDir, "req-1", "text"); err != nil {
		t.Fatal(err)
	}
	r := request.New("req-1", "/repos/app", "app", request.Source{Kind: request.SourceText}, time.Now())
	r.State = request.StateSpecDrafting
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}

	draft := `# Spec

## Problem

Refunds can be double-processed on retry.

## Scope

The /refunds endpoint only.

## Non-goals

Not touching /charges.

## Affected services and packages

internal/payments

## Acceptance criteria

1. The commit subject line begins with ` + "`ticket(refunds):`" + `.

## Risks

None known.

## Open questions

None.
`
	runner, _ := stubSpecDraftRunner(draft, &request.SpecEvidence{}, nil)
	if err := driveRequests(dp, context.Background(), dataDir, requestdriver.WorkerConfig{}, runner, failingPlanTicketsRunner(t), failingOracleDraftRunner(t), failingBuildRunner(t)); err != nil {
		t.Fatalf("driveRequests: %v", err)
	}

	loaded, err := request.Load(dataDir, "req-1")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.State != request.StateHalted {
		t.Fatalf("State = %q, want %q", loaded.State, request.StateHalted)
	}
	if !strings.Contains(loaded.Error, "no acceptance criteria left") {
		t.Errorf("Error = %q, want it to explain that stripping left no criteria", loaded.Error)
	}
	if _, err := os.Stat(requestdriver.RequestSpecPath(dataDir, "req-1")); !os.IsNotExist(err) {
		t.Errorf("spec.md should not be persisted when stripping leaves it invalid (stat err = %v)", err)
	}
}

// TestRequestFromEachSourceReachesSpecReviewWithValidSpec covers the
// plan's own "a request from each source (issue URL, text, file) reaches
// spec_review with a structurally valid spec.md" done-when item.
func TestRequestFromEachSourceReachesSpecReviewWithValidSpec(t *testing.T) {
	dp := newTestDeps(t)
	sources := []request.Source{
		{Kind: request.SourceIssue, IssueRef: "org/repo#42"},
		{Kind: request.SourceText},
		{Kind: request.SourceFile},
	}
	for _, source := range sources {
		t.Run(string(source.Kind), func(t *testing.T) {
			dataDir := t.TempDir()
			id := "req-" + string(source.Kind)
			if err := request.SaveText(dataDir, id, "some request text"); err != nil {
				t.Fatal(err)
			}
			r := request.New(id, "/repos/app", "app", source, time.Now())
			r.State = request.StateSpecDrafting
			if err := r.Save(dataDir); err != nil {
				t.Fatal(err)
			}

			runner, _ := stubSpecDraftRunner(canonicalValidSpec, &request.SpecEvidence{}, nil)
			if err := driveRequests(dp, context.Background(), dataDir, requestdriver.WorkerConfig{}, runner, failingPlanTicketsRunner(t), failingOracleDraftRunner(t), failingBuildRunner(t)); err != nil {
				t.Fatalf("driveRequests: %v", err)
			}

			loaded, err := request.Load(dataDir, id)
			if err != nil {
				t.Fatal(err)
			}
			if loaded.State != request.StateSpecReview {
				t.Fatalf("State = %q, want %q", loaded.State, request.StateSpecReview)
			}
			specBytes, err := os.ReadFile(requestdriver.RequestSpecPath(dataDir, id))
			if err != nil {
				t.Fatalf("read spec.md: %v", err)
			}
			if err := request.ValidateSpecSkeleton(string(specBytes)); err != nil {
				t.Errorf("ValidateSpecSkeleton(spec.md) = %v, want nil", err)
			}
		})
	}
}

// TestBuildRequestBuildArgsIsAcceptedByRunMainWithReadysOwnFlagSet is the
// plan's own required test ("argv the request driver hands to the run
// path is parsed through the REAL flag set"), mirroring
// TestBuildTicketRunArgsIsAcceptedByRunMainWithReadysOwnFlagSet: the argv
// buildRequestBuildArgs produces for one ticket must parse cleanly
// through runMainWithReady's real flag.FlagSet, stopping only at the
// required-flags check (empty -workspace here), never at "flag provided
// but not defined".
func TestBuildRequestBuildArgsIsAcceptedByRunMainWithReadysOwnFlagSet(t *testing.T) {
	dp := newTestDeps(t)
	isolateSessionConfig(t)
	workspace := t.TempDir()
	specPath := filepath.Join(t.TempDir(), "001.spec.md")
	if err := os.WriteFile(specPath, []byte("Verify-Command: make verify\n\n## Goal\n\ndo the thing\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	r := &request.Request{ID: "req-1", Workspace: workspace, Project: "payments"}
	ticket := request.Ticket{Index: 1, SpecPath: specPath}
	cfg := requestdriver.WorkerConfig{
		BuildAppScript: "/harness/build_app.py",
		SandboxImage:   "registry.example/org/img@sha256:deadbeef",
	}

	args, err := requestdriver.BuildRequestBuildArgs(t.TempDir(), r, ticket, cfg)
	if err != nil {
		t.Fatalf("buildRequestBuildArgs: %v", err)
	}

	err = runMainWithReady(dp, context.Background(), args, nil)
	if err == nil {
		t.Fatal("runMainWithReady(buildRequestBuildArgs(...)) = nil, want the required-flags error it stops at")
	}
	if strings.Contains(err.Error(), "not defined") {
		t.Fatalf("runMainWithReady rejected an argv buildRequestBuildArgs produced: %v", err)
	}
}

// TestBuildRequestBuildArgsUsesTicketVerifyCommand covers the plan's own
// "the ticket's spec becomes -spec" requirement: the ticket's own
// Verify-Command: line, not anything from the request itself (which has
// no verify command field at all), is what ends up in the argv. -spec
// itself now names the ticket's derived build-spec file (sibling
// <ticket>.build.md, see writeTicketBuildSpecFile), not ticket.SpecPath
// verbatim -- but its content still carries the ticket's own text,
// unmodified (this ticket declares no covered criteria and no approved
// spec.md exists here, so ticketBuildSpecContent appends nothing).
func TestBuildRequestBuildArgsUsesTicketVerifyCommand(t *testing.T) {
	specPath := filepath.Join(t.TempDir(), "001.spec.md")
	ticketContent := "Verify-Command: pytest\n\n## Goal\n\ndo the thing\n"
	if err := os.WriteFile(specPath, []byte(ticketContent), 0o644); err != nil {
		t.Fatal(err)
	}
	r := &request.Request{ID: "req-1", Workspace: "/repos/app", Project: "app"}
	ticket := request.Ticket{Index: 3, SpecPath: specPath}

	args, err := requestdriver.BuildRequestBuildArgs(withFirstTicketRun(t, r), r, ticket, requestdriver.WorkerConfig{})
	if err != nil {
		t.Fatalf("buildRequestBuildArgs: %v", err)
	}

	var gotSpec, gotVerify, gotTicket string
	for i, a := range args {
		switch a {
		case "-spec":
			gotSpec = args[i+1]
		case "-verify-command":
			gotVerify = args[i+1]
		case "-ticket":
			gotTicket = args[i+1]
		}
	}
	wantSpec := strings.TrimSuffix(specPath, ".spec.md") + ".build.md"
	if gotSpec != wantSpec {
		t.Errorf("-spec = %q, want %q (the ticket's own derived build spec)", gotSpec, wantSpec)
	}
	gotContent, err := os.ReadFile(gotSpec)
	if err != nil {
		t.Fatalf("read build spec: %v", err)
	}
	if string(gotContent) != ticketContent {
		t.Errorf("build spec content = %q, want the ticket spec verbatim %q", gotContent, ticketContent)
	}
	if gotVerify != "pytest" {
		t.Errorf("-verify-command = %q, want %q (parsed from the ticket's own spec)", gotVerify, "pytest")
	}
	if gotTicket != "req-1-003" {
		t.Errorf("-ticket = %q, want %q", gotTicket, "req-1-003")
	}
}

// TestBuildRequestBuildArgsRejectsMalformedPlanCriteria covers the other
// half of writeTicketCriteriaFile's "## Plan" check: a ticket that HAS a
// "## Plan" section but whose "### Acceptance criteria covered" list is
// malformed must halt the build with an error naming the ticket file,
// not silently omit -spec-acceptance-criteria (which would silently
// disable the required per-criterion conformity review).
func TestBuildRequestBuildArgsRejectsMalformedPlanCriteria(t *testing.T) {
	specPath := filepath.Join(t.TempDir(), "001.spec.md")
	malformed := "Verify-Command: pytest\n\n## Goal\n\ndo the thing\n\n## Plan\n\n### Acceptance criteria covered\n\nnot-a-number\n"
	if err := os.WriteFile(specPath, []byte(malformed), 0o644); err != nil {
		t.Fatal(err)
	}
	r := &request.Request{ID: "req-1", Workspace: "/repos/app", Project: "app"}
	ticket := request.Ticket{Index: 1, SpecPath: specPath}

	_, err := requestdriver.BuildRequestBuildArgs("data", r, ticket, requestdriver.WorkerConfig{})
	if err == nil {
		t.Fatal("buildRequestBuildArgs = nil error, want an error naming the malformed ticket")
	}
	if !strings.Contains(err.Error(), specPath) {
		t.Errorf("error = %q, want it to name the ticket file %q", err.Error(), specPath)
	}
}

// TestBuildRequestBuildArgsEmitsPrClosesIssueForIssueSource covers the
// live bug where a request submitted with -issue never carried its
// r.Source.IssueRef onto the synthetic QueueEntry buildRequestBuildArgs
// builds, so buildTicketRunArgs never emitted -pr-closes-issue and the
// ticket's draft PR body lacked its "Closes owner/repo#N" line.
func TestBuildRequestBuildArgsEmitsPrClosesIssueForIssueSource(t *testing.T) {
	specPath := filepath.Join(t.TempDir(), "001.spec.md")
	if err := os.WriteFile(specPath, []byte("Verify-Command: pytest\n\n## Goal\n\ndo the thing\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ticket := request.Ticket{Index: 1, SpecPath: specPath}

	withIssue := &request.Request{ID: "req-1", Workspace: "/repos/app", Project: "app", Source: request.Source{Kind: request.SourceIssue, IssueRef: "acme/widgets#42"}}
	args, err := requestdriver.BuildRequestBuildArgs("data", withIssue, ticket, requestdriver.WorkerConfig{})
	if err != nil {
		t.Fatalf("buildRequestBuildArgs: %v", err)
	}
	if !containsArg(args, "-pr-closes-issue", "acme/widgets#42") {
		t.Errorf("args = %v, want -pr-closes-issue acme/widgets#42", args)
	}

	withoutIssue := &request.Request{ID: "req-2", Workspace: "/repos/app", Project: "app", Source: request.Source{Kind: request.SourceText}}
	args, err = requestdriver.BuildRequestBuildArgs("data", withoutIssue, ticket, requestdriver.WorkerConfig{})
	if err != nil {
		t.Fatalf("buildRequestBuildArgs: %v", err)
	}
	for _, a := range args {
		if a == "-pr-closes-issue" {
			t.Errorf("args = %v, want no -pr-closes-issue flag when the request has no issue source", args)
		}
	}
}

// TestBuildRequestBuildArgsEmitsPRBaseForOpenPredecessorPR covers the
// stacked-PR mechanism's own ticketQueueEntry half: ticket N (N>1) of a
// multi-ticket request must stack its draft PR on ticket N-1's own branch
// (-pr-base) while N-1's PR is still open and unmerged, so ticket N's PR
// shows only its own delta instead of repeating ticket N-1's already-open
// commits (found live: a Flutter + Go app repo #312 repeating #311) -- but
// only then: a merged, PR-less, or branch-less predecessor, or ticket 1
// itself (no predecessor at all), must forward no -pr-base flag.
func TestBuildRequestBuildArgsEmitsPRBaseForOpenPredecessorPR(t *testing.T) {
	specPath := filepath.Join(t.TempDir(), "002.spec.md")
	if err := os.WriteFile(specPath, []byte("Verify-Command: pytest\n\n## Goal\n\ndo the thing\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ticket2 := request.Ticket{Index: 2, SpecPath: specPath}

	cases := []struct {
		name       string
		prevTicket request.Ticket
		wantBase   string
	}{
		{"open unmerged predecessor", request.Ticket{Index: 1, Branch: "factoryd/run-1", PRURL: "https://github.com/acme/widgets/pull/1", PRState: "open"}, "factoryd/run-1"},
		{"merged predecessor", request.Ticket{Index: 1, Branch: "factoryd/run-1", PRURL: "https://github.com/acme/widgets/pull/1", PRState: "merged"}, ""},
		{"predecessor with no PR yet", request.Ticket{Index: 1, Branch: "factoryd/run-1"}, ""},
		{"predecessor with no recorded branch", request.Ticket{Index: 1, PRURL: "https://github.com/acme/widgets/pull/1", PRState: "open"}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := &request.Request{ID: "req-1", Workspace: "/repos/app", Project: "app", Tickets: []request.Ticket{c.prevTicket, ticket2}}
			args, err := requestdriver.BuildRequestBuildArgs(withFirstTicketRun(t, r), r, ticket2, requestdriver.WorkerConfig{})
			if err != nil {
				t.Fatalf("buildRequestBuildArgs: %v", err)
			}
			if c.wantBase != "" {
				if !containsArg(args, "-pr-base", c.wantBase) {
					t.Errorf("args = %v, want -pr-base %s", args, c.wantBase)
				}
				return
			}
			for _, a := range args {
				if a == "-pr-base" {
					t.Errorf("args = %v, want no -pr-base flag", args)
				}
			}
		})
	}

	// Ticket 1 has no predecessor at all -- never forwards -pr-base
	// regardless of what (if anything) follows it in r.Tickets.
	ticket1 := request.Ticket{Index: 1, SpecPath: specPath}
	r := &request.Request{ID: "req-2", Workspace: "/repos/app", Project: "app", Tickets: []request.Ticket{ticket1}}
	args, err := requestdriver.BuildRequestBuildArgs("data", r, ticket1, requestdriver.WorkerConfig{})
	if err != nil {
		t.Fatalf("buildRequestBuildArgs: %v", err)
	}
	for _, a := range args {
		if a == "-pr-base" {
			t.Errorf("args = %v, want no -pr-base flag for ticket 1 (no predecessor)", args)
		}
	}
}

// TestBuildRequestBuildArgsCarriesRequestPreflightProfile pins the live
// bug this guards against: a request submitted with -preflight-profile
// set (explicitly, or via .factory.yml) must have that profile reach the
// ticket's own build argv, exactly like its verify command does --
// otherwise the build silently falls back to the strict default profile
// and halts a brownfield workspace's preflight on artifacts convention
// never asked it to produce. Covers both the present and absent cases in
// one test, table-style, since they're the same code path with only
// r.PreflightProfile varying.
func TestBuildRequestBuildArgsCarriesRequestPreflightProfile(t *testing.T) {
	specPath := filepath.Join(t.TempDir(), "001.spec.md")
	if err := os.WriteFile(specPath, []byte("Verify-Command: pytest\n\n## Goal\n\ndo the thing\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ticket := request.Ticket{Index: 1, SpecPath: specPath}

	for _, profile := range []string{"brownfield", ""} {
		r := &request.Request{ID: "req-1", Workspace: "/repos/app", Project: "app", PreflightProfile: profile}
		args, err := requestdriver.BuildRequestBuildArgs("data", r, ticket, requestdriver.WorkerConfig{})
		if err != nil {
			t.Fatalf("buildRequestBuildArgs(PreflightProfile=%q): %v", profile, err)
		}
		var got string
		var found bool
		for i, a := range args {
			if a == "-preflight-profile" {
				found = true
				got = args[i+1]
			}
		}
		if profile == "" {
			if found {
				t.Errorf("PreflightProfile=%q: argv unexpectedly carries -preflight-profile %q", profile, got)
			}
			continue
		}
		if !found || got != profile {
			t.Errorf("PreflightProfile=%q: -preflight-profile = %q (found=%v), want %q", profile, got, found, profile)
		}
	}
}

// TestTicketQueueEntryCarriesExecutionModel proves r.Models["execution"]
// (`factoryd submit -model execution=...`, already validated at submit
// time against roles.execution.allowed) reaches the ticket's own
// QueueEntry.ExecutionModel (ticketQueueEntry), and from there the
// ticket's own build argv as -execution-model (buildTicketRunArgs) --
// exercised together through buildRequestBuildArgs, the same way
// TestBuildRequestBuildArgsCarriesRequestHarness below covers Harness.
// An unset r.Models leaves the argv with no -execution-model at all, no
// behavior change from before this field existed.
func TestTicketQueueEntryCarriesExecutionModel(t *testing.T) {
	specPath := filepath.Join(t.TempDir(), "001.spec.md")
	if err := os.WriteFile(specPath, []byte("Verify-Command: pytest\n\n## Goal\n\ndo the thing\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ticket := request.Ticket{Index: 1, SpecPath: specPath}

	for _, model := range []string{"sonnet", ""} {
		r := &request.Request{ID: "req-1", Workspace: "/repos/app", Project: "app"}
		if model != "" {
			r.Models = map[string]string{"execution": model}
		}
		args, err := requestdriver.BuildRequestBuildArgs("data", r, ticket, requestdriver.WorkerConfig{})
		if err != nil {
			t.Fatalf("buildRequestBuildArgs(Models[execution]=%q): %v", model, err)
		}
		var got string
		var found bool
		for i, a := range args {
			if a == "-execution-model" {
				found = true
				got = args[i+1]
			}
		}
		if model == "" {
			if found {
				t.Errorf("Models unset: argv unexpectedly carries -execution-model %q", got)
			}
			continue
		}
		if !found || got != model {
			t.Errorf("Models[execution]=%q: -execution-model = %q (found=%v), want %q", model, got, found, model)
		}
	}
}

// TestBuildTicketRunArgsForwardsExecutionModel is buildTicketRunArgs' own
// unit-level proof (rather than through buildRequestBuildArgs above): a
// QueueEntry.ExecutionModel forwards as -execution-model, and an empty
// one forwards nothing.
func TestBuildTicketRunArgsForwardsExecutionModel(t *testing.T) {
	entry := &requestdriver.QueueEntry{ID: "t", Workspace: "/repos/app", SpecPath: "/repos/app/spec.md", VerifyCommand: "make verify", ExecutionModel: "sonnet"}
	args := requestdriver.BuildTicketRunArgs("data", entry, requestdriver.WorkerConfig{})
	var got string
	var found bool
	for i, a := range args {
		if a == "-execution-model" {
			found = true
			got = args[i+1]
		}
	}
	if !found || got != "sonnet" {
		t.Errorf("-execution-model = %q (found=%v), want %q", got, found, "sonnet")
	}

	entry.ExecutionModel = ""
	args = requestdriver.BuildTicketRunArgs("data", entry, requestdriver.WorkerConfig{})
	for _, a := range args {
		if a == "-execution-model" {
			t.Errorf("argv unexpectedly carries -execution-model with an empty QueueEntry.ExecutionModel")
		}
	}
}

// TestBuildRequestBuildArgsCarriesRequestHarness is a regression test:
// `factoryd submit -harness execution=<name>`, recorded on request.Request,
// must reach the ticket's own build argv as -execution-harness -- and an
// unset choice (or a planning-only one) must leave
// the argv byte-for-byte identical to a request that never had the field
// (no behavior change when unset, by design).
func TestBuildRequestBuildArgsCarriesRequestHarness(t *testing.T) {
	specPath := filepath.Join(t.TempDir(), "001.spec.md")
	if err := os.WriteFile(specPath, []byte("Verify-Command: pytest\n\n## Goal\n\ndo the thing\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ticket := request.Ticket{Index: 1, SpecPath: specPath}
	cfg := requestdriver.WorkerConfig{}

	withOverrides := &request.Request{ID: "req-1", Workspace: "/repos/app", Project: "app", Harnesses: map[string]string{"execution": "pifork", "planning": "pi"}}
	args, err := requestdriver.BuildRequestBuildArgs("data", withOverrides, ticket, cfg)
	if err != nil {
		t.Fatalf("buildRequestBuildArgs: %v", err)
	}
	if !containsArg(args, "-execution-harness", "pifork") {
		t.Errorf("args = %v, want -execution-harness pifork", args)
	}

	unset := &request.Request{ID: "req-2", Workspace: "/repos/app", Project: "app"}
	unsetArgs, err := requestdriver.BuildRequestBuildArgs("data", unset, ticket, cfg)
	if err != nil {
		t.Fatalf("buildRequestBuildArgs: %v", err)
	}
	for _, a := range unsetArgs {
		if a == "-execution-harness" {
			t.Errorf("unset Harnesses: args = %v, want no -execution-harness flag", unsetArgs)
		}
	}
}

// twoCriteriaSpec is canonicalValidSpec's own sibling with two acceptance
// criteria, used by the planning tests below to exercise coverage across
// one or two tickets.
const twoCriteriaSpec = `# Spec

## Problem

Refunds can be double-processed on retry.

## Scope

The /refunds endpoint only.

## Non-goals

Not touching /charges.

## Affected services and packages

internal/payments

## Acceptance criteria

1. A retried POST /refunds with the same idempotency key returns the original result.
2. A non-idempotent POST /refunds still processes normally.

## Risks

None known.

## Open questions

None.
`

func validBrownfieldTicket(verifyCommand string, criteria ...int) string {
	criteriaLines := ""
	for _, n := range criteria {
		criteriaLines += fmt.Sprintf("- %d\n", n)
	}
	return fmt.Sprintf(`Verify-Command: %s
Allowed-Files: internal/payments/refunds.go, internal/payments/refunds_test.go
Required-Changed-Files: internal/payments/refunds.go

## Goal

Fix double-processing.

## Plan

### Files to touch

- internal/payments/refunds.go

### Steps

1. Add an idempotency check.

### Tests to add

- internal/payments/refunds_test.go

### Acceptance criteria covered

%s
## Out of scope

Nothing else.
`, verifyCommand, criteriaLines)
}

// stubPlanTicketsRunner mirrors stubSpecDraftRunner's own shape for the
// plan-drafting job, recording the verifyCommand it was actually called
// with alongside a fixed (tickets, evidence, err) return.
func stubPlanTicketsRunner(tickets []requestdriver.DraftedTicket, evidence *request.PlanEvidence, err error) (requestdriver.PlanTicketsRunner, *string) {
	var gotVerifyCommand string
	runner := func(ctx context.Context, dataDir string, r *request.Request, cfg requestdriver.WorkerConfig, verifyCommand string) ([]requestdriver.DraftedTicket, *request.PlanEvidence, error) {
		gotVerifyCommand = verifyCommand
		return tickets, evidence, err
	}
	return runner, &gotVerifyCommand
}

// approvedPlanningFixture creates a request already in planning (spec.md
// written and approved, exactly what verifyApprovedHashes needs to pass)
// with a workspace declaring verifyCommand in .factory.yml, returning
// dataDir and the request id.
func approvedPlanningFixture(t *testing.T, specContent, verifyCommand string) (dataDir, id string) {
	t.Helper()
	dataDir = t.TempDir()
	id = "req-1"
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, ".factory.yml"), []byte("verify_command: "+verifyCommand+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := request.SaveText(dataDir, id, "some request text"); err != nil {
		t.Fatal(err)
	}
	r := request.New(id, workspace, "app", request.Source{Kind: request.SourceText}, time.Now())
	r.State = request.StateSpecReview
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(requestdriver.RequestSpecPath(dataDir, id), []byte(specContent), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := request.Approve(dataDir, id, "alice", time.Now(), nil); err != nil {
		t.Fatalf("approve spec: %v", err)
	}
	return dataDir, id
}

// approvedPlanningFixtureNoFactoryYML mirrors approvedPlanningFixture
// exactly, except the workspace has no .factory.yml at all -- the request
// itself carries verifyCommand as r.VerifyCommand instead, exactly as
// `factoryd submit -verify-command ...` would record it against a
// workspace with no verify_command of its own. Used to pin that
// advancePlanning prefers r.VerifyCommand over re-reading (a nonexistent)
// .factory.yml.
func approvedPlanningFixtureNoFactoryYML(t *testing.T, specContent, verifyCommand string) (dataDir, id string) {
	t.Helper()
	dataDir = t.TempDir()
	id = "req-1"
	workspace := t.TempDir()
	if err := request.SaveText(dataDir, id, "some request text"); err != nil {
		t.Fatal(err)
	}
	r := request.New(id, workspace, "app", request.Source{Kind: request.SourceText}, time.Now())
	r.VerifyCommand = verifyCommand
	r.State = request.StateSpecReview
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(requestdriver.RequestSpecPath(dataDir, id), []byte(specContent), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := request.Approve(dataDir, id, "alice", time.Now(), nil); err != nil {
		t.Fatalf("approve spec: %v", err)
	}
	return dataDir, id
}

// TestAdvancePlanningUsesRequestVerifyCommandWithoutFactoryYML pins the
// live bug this guards against: a workspace with no .factory.yml at all,
// where the operator supplied -verify-command explicitly at submit time.
// Before r.VerifyCommand existed, planning always re-read .factory.yml
// fresh and halted with "planning requires verify_command configured in
// .factory.yml" even though the operator's choice was already known.
func TestAdvancePlanningUsesRequestVerifyCommandWithoutFactoryYML(t *testing.T) {
	dp := newTestDeps(t)
	dataDir, id := approvedPlanningFixtureNoFactoryYML(t, twoCriteriaSpec, "python3 -m unittest tests/test_product_lab.py")
	tickets := []requestdriver.DraftedTicket{
		{Filename: "001.spec.md", Content: validBrownfieldTicket("python3 -m unittest tests/test_product_lab.py", 1, 2)},
	}
	runner, gotVerifyCommand := stubPlanTicketsRunner(tickets, &request.PlanEvidence{}, nil)

	if err := driveRequests(dp, context.Background(), dataDir, requestdriver.WorkerConfig{}, failingSpecDraftRunner(t), runner, failingOracleDraftRunner(t), failingBuildRunner(t)); err != nil {
		t.Fatalf("driveRequests: %v", err)
	}
	if *gotVerifyCommand != "python3 -m unittest tests/test_product_lab.py" {
		t.Errorf("verifyCommand passed to plan job = %q, want the request's own VerifyCommand", *gotVerifyCommand)
	}

	loaded, err := request.Load(dataDir, id)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.State != request.StatePlanReview {
		t.Fatalf("State = %q, want %q (Error: %s)", loaded.State, request.StatePlanReview, loaded.Error)
	}
}

// TestAdvancePlanningWritesFeedbackFileFromPlanRejection is a regression
// test: a plan_review rejection's reason must be written to
// request.PlanFeedbackPath before the plan-drafting job runs, stage-scoped
// like the spec/oracle stages.
func TestAdvancePlanningWritesFeedbackFileFromPlanRejection(t *testing.T) {
	dp := newTestDeps(t)
	dataDir, id := approvedPlanningFixtureNoFactoryYML(t, twoCriteriaSpec, "python3 -m unittest tests/test_product_lab.py")
	loaded, err := request.Load(dataDir, id)
	if err != nil {
		t.Fatal(err)
	}
	loaded.Rejections = []request.Rejection{
		{By: "alice", At: "2026-09-24T00:00:00Z", Reason: "split ticket 1 -- it touches two unrelated packages", FromState: request.StatePlanReview},
		{By: "bob", At: "2026-09-23T00:00:00Z", Reason: "unrelated spec reason", FromState: request.StateSpecReview},
	}
	if err := loaded.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	tickets := []requestdriver.DraftedTicket{
		{Filename: "001.spec.md", Content: validBrownfieldTicket("python3 -m unittest tests/test_product_lab.py", 1, 2)},
	}
	runner, _ := stubPlanTicketsRunner(tickets, &request.PlanEvidence{}, nil)

	if err := driveRequests(dp, context.Background(), dataDir, requestdriver.WorkerConfig{}, failingSpecDraftRunner(t), runner, failingOracleDraftRunner(t), failingBuildRunner(t)); err != nil {
		t.Fatalf("driveRequests: %v", err)
	}

	got, err := os.ReadFile(request.PlanFeedbackPath(dataDir, id))
	if err != nil {
		t.Fatalf("read plan-feedback.md: %v", err)
	}
	if !strings.Contains(string(got), "split ticket 1 -- it touches two unrelated packages") {
		t.Errorf("plan-feedback.md = %q, want the plan_review rejection reason", got)
	}
	if strings.Contains(string(got), "unrelated spec reason") {
		t.Errorf("plan-feedback.md = %q, want no spec_review rejection reason", got)
	}
}

// TestAdvancePlanningHaltsWhenNoVerifyCommandAnywhere covers the halt
// path: neither r.VerifyCommand nor the workspace's .factory.yml supply
// one.
func TestAdvancePlanningHaltsWhenNoVerifyCommandAnywhere(t *testing.T) {
	dp := newTestDeps(t)
	dataDir, id := approvedPlanningFixtureNoFactoryYML(t, twoCriteriaSpec, "")

	if err := driveRequests(dp, context.Background(), dataDir, requestdriver.WorkerConfig{}, failingSpecDraftRunner(t), failingPlanTicketsRunner(t), failingOracleDraftRunner(t), failingBuildRunner(t)); err != nil {
		t.Fatalf("driveRequests: %v", err)
	}

	loaded, err := request.Load(dataDir, id)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.State != request.StateHalted {
		t.Fatalf("State = %q, want %q", loaded.State, request.StateHalted)
	}
	if !strings.Contains(loaded.Error, "verify command") {
		t.Errorf("Error = %q, want it to mention a verify command", loaded.Error)
	}
}

// TestAdvancePlanningSinglePackageYieldsOneTicket covers the plan's own
// "a single-package spec yields exactly one ticket" done-when item.
func TestAdvancePlanningSinglePackageYieldsOneTicket(t *testing.T) {
	dp := newTestDeps(t)
	dataDir, id := approvedPlanningFixture(t, twoCriteriaSpec, "make verify")
	tickets := []requestdriver.DraftedTicket{
		{Filename: "001.spec.md", Content: validBrownfieldTicket("make verify", 1, 2)},
	}
	runner, gotVerifyCommand := stubPlanTicketsRunner(tickets, &request.PlanEvidence{AgentExitCode: 0, DurationS: 2.5}, nil)

	if err := driveRequests(dp, context.Background(), dataDir, requestdriver.WorkerConfig{}, failingSpecDraftRunner(t), runner, failingOracleDraftRunner(t), failingBuildRunner(t)); err != nil {
		t.Fatalf("driveRequests: %v", err)
	}
	if *gotVerifyCommand != "make verify" {
		t.Errorf("verifyCommand passed to plan job = %q, want %q", *gotVerifyCommand, "make verify")
	}

	loaded, err := request.Load(dataDir, id)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.State != request.StatePlanReview {
		t.Fatalf("State = %q, want %q", loaded.State, request.StatePlanReview)
	}
	if len(loaded.Tickets) != 1 || loaded.TicketCount != 1 {
		t.Fatalf("Tickets = %+v, TicketCount = %d, want exactly one", loaded.Tickets, loaded.TicketCount)
	}
	if loaded.PlanEvidence == nil || loaded.PlanEvidence.DurationS != 2.5 {
		t.Errorf("PlanEvidence = %+v, want the stubbed evidence recorded on the request", loaded.PlanEvidence)
	}
}

// TestAdvancePlanningTwoServiceYieldsTwoTicketsInOrder covers the plan's
// own "a two-service spec yields two tickets in dependency order" done-
// when item.
func TestAdvancePlanningTwoServiceYieldsTwoTicketsInOrder(t *testing.T) {
	dp := newTestDeps(t)
	dataDir, id := approvedPlanningFixture(t, twoCriteriaSpec, "make verify")
	tickets := []requestdriver.DraftedTicket{
		{Filename: "001.spec.md", Content: validBrownfieldTicket("make verify", 1)},
		{Filename: "002.spec.md", Content: validBrownfieldTicket("make verify", 2)},
	}
	runner, _ := stubPlanTicketsRunner(tickets, &request.PlanEvidence{}, nil)

	if err := driveRequests(dp, context.Background(), dataDir, requestdriver.WorkerConfig{}, failingSpecDraftRunner(t), runner, failingOracleDraftRunner(t), failingBuildRunner(t)); err != nil {
		t.Fatalf("driveRequests: %v", err)
	}

	loaded, err := request.Load(dataDir, id)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.State != request.StatePlanReview {
		t.Fatalf("State = %q, want %q", loaded.State, request.StatePlanReview)
	}
	if len(loaded.Tickets) != 2 {
		t.Fatalf("Tickets = %+v, want exactly two", loaded.Tickets)
	}
	if loaded.Tickets[0].Index != 1 || loaded.Tickets[1].Index != 2 {
		t.Errorf("Tickets indexes = %d, %d, want 1, 2 in dependency order", loaded.Tickets[0].Index, loaded.Tickets[1].Index)
	}
	if !strings.HasSuffix(loaded.Tickets[0].SpecPath, "001.spec.md") || !strings.HasSuffix(loaded.Tickets[1].SpecPath, "002.spec.md") {
		t.Errorf("Tickets spec paths = %q, %q, want 001.spec.md then 002.spec.md", loaded.Tickets[0].SpecPath, loaded.Tickets[1].SpecPath)
	}
}

// TestAdvancePlanningUnclaimedCriteriaHalts covers the plan's own
// "unclaimed acceptance criteria halt the request" done-when item.
func TestAdvancePlanningUnclaimedCriteriaHalts(t *testing.T) {
	dp := newTestDeps(t)
	dataDir, id := approvedPlanningFixture(t, twoCriteriaSpec, "make verify")
	tickets := []requestdriver.DraftedTicket{
		{Filename: "001.spec.md", Content: validBrownfieldTicket("make verify", 1)}, // criterion 2 never claimed
	}
	runner, _ := stubPlanTicketsRunner(tickets, &request.PlanEvidence{}, nil)

	if err := driveRequests(dp, context.Background(), dataDir, requestdriver.WorkerConfig{}, failingSpecDraftRunner(t), runner, failingOracleDraftRunner(t), failingBuildRunner(t)); err != nil {
		t.Fatalf("driveRequests: %v", err)
	}

	loaded, err := request.Load(dataDir, id)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.State != request.StateHalted {
		t.Fatalf("State = %q, want %q", loaded.State, request.StateHalted)
	}
	if !strings.Contains(loaded.Error, "claimed by no ticket") || !strings.Contains(loaded.Error, "2") {
		t.Errorf("Error = %q, want it to name the unclaimed criterion", loaded.Error)
	}
}

// infeasibleTestsAddedTicket mirrors validBrownfieldTicket's shape but
// declares an Allowed-Files/Required-Changed-Files that names only one
// concrete, non-test Go file and no Tests-Required opt-out -- the exact
// live shape (Flutter + Go app run 3, 2026-09-28) that could never pass the
// tests_added gate no matter what a build round does, since TestsAdded
// only ever looks at ChangedFiles and a run's diff_scope gate already
// confines those to Allowed-Files.
func infeasibleTestsAddedTicket(verifyCommand string, criteria ...int) string {
	criteriaLines := ""
	for _, n := range criteria {
		criteriaLines += fmt.Sprintf("- %d\n", n)
	}
	return fmt.Sprintf(`Verify-Command: %s
Allowed-Files: backend/internal/handler/dispatch_mux.go
Required-Changed-Files: backend/internal/handler/dispatch_mux.go

## Goal

Wire the new route into the dispatch mux.

## Plan

### Files to touch

- backend/internal/handler/dispatch_mux.go

### Steps

1. Register the new route.

### Tests to add

No separate handler test file; the route will be exercised by another
ticket's dispatch test.

### Acceptance criteria covered

%s
## Out of scope

Nothing else.
`, verifyCommand, criteriaLines)
}

// TestAdvancePlanningInfeasibleTestsAddedHaltsNamingTicket covers the live
// bug (Flutter + Go app run 3, 2026-09-28): a drafted ticket whose own Allowed-Files
// could never satisfy the tests_added gate must halt planning before the
// plan ever reaches plan_review -- not be approved, built (burning a real
// round), and only THEN quarantined on a gate it was structurally unable
// to pass. See policy.TicketTestsAddedFeasible's own doc comment for the
// full incident this closes.
func TestAdvancePlanningInfeasibleTestsAddedHaltsNamingTicket(t *testing.T) {
	dp := newTestDeps(t)
	dataDir, id := approvedPlanningFixture(t, twoCriteriaSpec, "make verify")
	tickets := []requestdriver.DraftedTicket{
		{Filename: "001.spec.md", Content: infeasibleTestsAddedTicket("make verify", 1, 2)},
	}
	runner, _ := stubPlanTicketsRunner(tickets, &request.PlanEvidence{}, nil)

	if err := driveRequests(dp, context.Background(), dataDir, requestdriver.WorkerConfig{}, failingSpecDraftRunner(t), runner, failingOracleDraftRunner(t), failingBuildRunner(t)); err != nil {
		t.Fatalf("driveRequests: %v", err)
	}

	loaded, err := request.Load(dataDir, id)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.State != request.StateHalted {
		t.Fatalf("State = %q, want %q (must not reach plan_review)", loaded.State, request.StateHalted)
	}
	if !strings.Contains(loaded.Error, "001.spec.md") || !strings.Contains(loaded.Error, "tests_added") {
		t.Errorf("Error = %q, want it to name the infeasible ticket and the tests_added gate", loaded.Error)
	}
}

// sequencedPlanTicketsRunner returns a planTicketsRunner that plays back
// calls[0], calls[1], ... in order (one entry per launch), for tests
// exercising advancePlanning's own bounded automatic re-plan. It also
// records, for each call, the content of plan-feedback.md as it stood
// the moment that call ran (read from disk before the fake even looks at
// its own args, mirroring what the real runPlanTicketsJob reads --
// cmd/factoryd/plan_tickets_job.go), and the total number of calls made.
func sequencedPlanTicketsRunner(t *testing.T, dataDir string, calls ...func() ([]requestdriver.DraftedTicket, *request.PlanEvidence, error)) (runner requestdriver.PlanTicketsRunner, callCount *int, feedbackByCall *[]string) {
	t.Helper()
	n := 0
	var feedbacks []string
	runner = func(ctx context.Context, dd string, r *request.Request, cfg requestdriver.WorkerConfig, verifyCommand string) ([]requestdriver.DraftedTicket, *request.PlanEvidence, error) {
		b, _ := os.ReadFile(request.PlanFeedbackPath(dataDir, r.ID))
		feedbacks = append(feedbacks, string(b))
		if n >= len(calls) {
			t.Fatalf("sequencedPlanTicketsRunner: call %d exceeds the %d scripted calls", n+1, len(calls))
		}
		next := calls[n]
		n++
		return next()
	}
	return runner, &n, &feedbacks
}

// TestAdvancePlanningInfeasibleTestsAddedAutoReplanSucceeds covers the
// automatic re-plan (advancePlanning's own bounded retry, see its doc
// comment and maxPlanningAttempts): a first drafted plan that is
// tests_added-infeasible must not halt outright -- it triggers exactly
// one more planning launch, fed the ticket/gate name as plan_review-style
// feedback, and a feasible second plan reaches plan_review normally.
func TestAdvancePlanningInfeasibleTestsAddedAutoReplanSucceeds(t *testing.T) {
	dp := newTestDeps(t)
	dataDir, id := approvedPlanningFixture(t, twoCriteriaSpec, "make verify")
	infeasible := []requestdriver.DraftedTicket{
		{Filename: "001.spec.md", Content: infeasibleTestsAddedTicket("make verify", 1, 2)},
	}
	feasible := []requestdriver.DraftedTicket{
		{Filename: "001.spec.md", Content: validBrownfieldTicket("make verify", 1, 2)},
	}
	runner, callCount, feedbackByCall := sequencedPlanTicketsRunner(t, dataDir,
		func() ([]requestdriver.DraftedTicket, *request.PlanEvidence, error) {
			return infeasible, &request.PlanEvidence{}, nil
		},
		func() ([]requestdriver.DraftedTicket, *request.PlanEvidence, error) {
			return feasible, &request.PlanEvidence{}, nil
		},
	)

	if err := driveRequests(dp, context.Background(), dataDir, requestdriver.WorkerConfig{}, failingSpecDraftRunner(t), runner, failingOracleDraftRunner(t), failingBuildRunner(t)); err != nil {
		t.Fatalf("driveRequests: %v", err)
	}

	if *callCount != 2 {
		t.Fatalf("planning launches = %d, want exactly 2", *callCount)
	}
	if got := (*feedbackByCall)[0]; got != "" {
		t.Errorf("first launch's plan-feedback.md = %q, want empty (no prior rejection)", got)
	}
	second := (*feedbackByCall)[1]
	if !strings.Contains(second, "001.spec.md") || !strings.Contains(second, "tests_added") {
		t.Errorf("second launch's plan-feedback.md = %q, want it to name the infeasible ticket and the tests_added gate", second)
	}

	loaded, err := request.Load(dataDir, id)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.State != request.StatePlanReview {
		t.Fatalf("State = %q, want %q (the second, feasible plan)", loaded.State, request.StatePlanReview)
	}
	if loaded.TicketCount != 1 {
		t.Fatalf("TicketCount = %d, want 1", loaded.TicketCount)
	}
	if len(loaded.Rejections) != 1 {
		t.Fatalf("Rejections = %+v, want exactly one factory-authored rejection", loaded.Rejections)
	}
	rej := loaded.Rejections[0]
	if rej.By != "factoryd" {
		t.Errorf("Rejections[0].By = %q, want %q", rej.By, "factoryd")
	}
	if rej.Stage() != request.StatePlanReview {
		t.Errorf("Rejections[0].Stage() = %q, want %q (routes into PlanFeedback)", rej.Stage(), request.StatePlanReview)
	}
	if !strings.Contains(rej.Reason, "001.spec.md") || !strings.Contains(rej.Reason, "tests_added") {
		t.Errorf("Rejections[0].Reason = %q, want it to name the infeasible ticket and the tests_added gate", rej.Reason)
	}
}

// TestAdvancePlanningInfeasibleTestsAddedTwiceHaltsAfterTwoLaunches covers
// the bound on advancePlanning's automatic re-plan: a second
// tests_added-infeasible plan halts exactly like today, after exactly two
// launches -- never a third, unbounded retry.
func TestAdvancePlanningInfeasibleTestsAddedTwiceHaltsAfterTwoLaunches(t *testing.T) {
	dp := newTestDeps(t)
	dataDir, id := approvedPlanningFixture(t, twoCriteriaSpec, "make verify")
	infeasible := []requestdriver.DraftedTicket{
		{Filename: "001.spec.md", Content: infeasibleTestsAddedTicket("make verify", 1, 2)},
	}
	spent := func() *request.PlanEvidence {
		return &request.PlanEvidence{Spend: &request.JobSpend{Role: "planning", InputTokens: 100, CostMicroUSD: 1_000_000}}
	}
	runner, callCount, _ := sequencedPlanTicketsRunner(t, dataDir,
		func() ([]requestdriver.DraftedTicket, *request.PlanEvidence, error) {
			return infeasible, spent(), nil
		},
		func() ([]requestdriver.DraftedTicket, *request.PlanEvidence, error) {
			return infeasible, spent(), nil
		},
	)

	if err := driveRequests(dp, context.Background(), dataDir, requestdriver.WorkerConfig{}, failingSpecDraftRunner(t), runner, failingOracleDraftRunner(t), failingBuildRunner(t)); err != nil {
		t.Fatalf("driveRequests: %v", err)
	}

	if *callCount != 2 {
		t.Fatalf("planning launches = %d, want exactly 2 (bounded, no unbounded loop)", *callCount)
	}

	loaded, err := request.Load(dataDir, id)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.State != request.StateHalted {
		t.Fatalf("State = %q, want %q (must not reach plan_review)", loaded.State, request.StateHalted)
	}
	if !strings.Contains(loaded.Error, "001.spec.md") || !strings.Contains(loaded.Error, "tests_added") {
		t.Errorf("Error = %q, want it to name the infeasible ticket and the tests_added gate", loaded.Error)
	}
	if len(loaded.Rejections) != 1 {
		t.Fatalf("Rejections = %+v, want exactly one factory-authored rejection (from the first, auto-replanned attempt)", loaded.Rejections)
	}
	if loaded.Rejections[0].By != "factoryd" {
		t.Errorf("Rejections[0].By = %q, want %q", loaded.Rejections[0].By, "factoryd")
	}
	// Both paid attempts stay on the halted request, so `factoryd cost`
	// counts them (live, 2026-09-28: plan drafting showed $0.00 after two
	// paid drafts).
	if loaded.PlanEvidence == nil || loaded.PlanEvidence.Spend == nil {
		t.Fatalf("PlanEvidence = %+v, want both attempts' spend recorded on the halted request", loaded.PlanEvidence)
	}
	if got := loaded.PlanEvidence.Spend.CostMicroUSD; got != 2_000_000 {
		t.Errorf("PlanEvidence.Spend.CostMicroUSD = %d, want 2000000 (both attempts)", got)
	}
	if got := loaded.PlanEvidence.Spend.InputTokens; got != 200 {
		t.Errorf("PlanEvidence.Spend.InputTokens = %d, want 200", got)
	}
}

// TestCriterionFilesFeasibleAcceptsModuleRelativePath: a criterion naming
// `cmd/server/main.go` is covered by a ticket allowed to change
// backend/cmd/server/main.go (live, Flutter + Go app, 2026-09-28: the literal
// comparison halted a correct plan). A path no covering ticket owns under
// any prefix is still flagged.
func TestCriterionFilesFeasibleAcceptsModuleRelativePath(t *testing.T) {
	criteria := []string{
		"The route is registered in `cmd/server/main.go`.",
		"The tool is registered in `internal/mcptools/habit_tools.go`.",
	}
	allowed := []string{"backend/cmd/server/main.go", "backend/internal/handler/habit.go"}
	tickets := []string{habitTicket("make verify", allowed, allowed, 1, 2)}

	reasons := requestdriver.CriterionFilesFeasible(criteria, tickets, [][]string{allowed}, "")
	if len(reasons) != 1 {
		t.Fatalf("reasons = %q, want exactly one (criterion 2's habit_tools.go)", reasons)
	}
	if !strings.Contains(reasons[0], "criterion 2 names internal/mcptools/habit_tools.go") {
		t.Errorf("reasons[0] = %q, want it to name criterion 2's uncovered file", reasons[0])
	}
}

// habitInsightsCriterionSpec mirrors the live shape (Flutter + Go app
// habit-insights request, 2026-09-28,
// data/requests/feature-habit-insights-endpoint-and-mcp-20260928-121607):
// criterion 1 names files owned by three different tickets, backticked,
// alongside a `cd backend && ...` verify command (also backticked) that
// must never be mistaken for a path; criterion 2 is a plain, single-file
// criterion so both criteria can be satisfied without touching criterion
// 1's own scenario.
const habitInsightsCriterionSpec = "# Spec\n\n" +
	"## Problem\n\nAdd habit insights.\n\n" +
	"## Scope\n\nHabit insights endpoint and MCP tool.\n\n" +
	"## Non-goals\n\nNone.\n\n" +
	"## Affected services and packages\n\nbackend/internal/habit, backend/internal/handler, backend/internal/mcptools\n\n" +
	"## Acceptance criteria\n\n" +
	"1. Running `cd backend && go vet ./... && go test ./...` exits successfully, with unit tests covering `backend/internal/habit/insights.go`, `backend/internal/handler/habit.go`, and `backend/internal/mcptools/habit_tools.go`.\n" +
	"2. The `habit_insights` MCP tool is registered.\n\n" +
	"## Risks\n\nNone known.\n\n" +
	"## Open questions\n\nNone.\n"

// habitTicket builds one brownfield ticket for habitInsightsCriterionSpec's
// own scenario: allowed/required are the ticket's own Allowed-Files/
// Required-Changed-Files (comma-joined), and criteria is the list of
// acceptance-criterion numbers it claims.
func habitTicket(verifyCommand string, allowed, required []string, criteria ...int) string {
	criteriaLines := ""
	for _, n := range criteria {
		criteriaLines += fmt.Sprintf("- %d\n", n)
	}
	return fmt.Sprintf(`Verify-Command: %s
Allowed-Files: %s
Required-Changed-Files: %s

## Goal

Implement habit insights.

## Plan

### Files to touch

- %s

### Steps

1. Implement it.

### Tests to add

- %s

### Acceptance criteria covered

%s
## Out of scope

Nothing else.
`, verifyCommand, strings.Join(allowed, ", "), strings.Join(required, ", "), required[0], allowed[len(allowed)-1], criteriaLines)
}

// TestAdvancePlanningCriterionFilesInfeasibleAutoReplanSucceeds covers the
// live bug this closes (see habitInsightsCriterionSpec's own doc
// comment): criterion 1 names three tickets' own files, but the first
// drafted plan lists it as covered only by the ticket owning
// mcptools/habit_tools.go. That must not reach plan_review -- it
// triggers exactly one automatic re-plan naming criterion 1 and the two
// uncovered paths (never the backticked verify command, which is not a
// path), and a second, feasible plan (all three tickets listing
// criterion 1) reaches plan_review normally.
func TestAdvancePlanningCriterionFilesInfeasibleAutoReplanSucceeds(t *testing.T) {
	dp := newTestDeps(t)
	dataDir, id := approvedPlanningFixture(t, habitInsightsCriterionSpec, "make verify")
	insightsTicket := habitTicket("make verify",
		[]string{"backend/internal/habit/insights.go", "backend/internal/habit/insights_test.go"},
		[]string{"backend/internal/habit/insights.go"}, 2)
	handlerTicket := habitTicket("make verify",
		[]string{"backend/internal/handler/habit.go", "backend/internal/handler/habit_test.go"},
		[]string{"backend/internal/handler/habit.go"}, 2)
	mcpTicketCoveringAlone := habitTicket("make verify",
		[]string{"backend/internal/mcptools/habit_tools.go", "backend/internal/mcptools/habit_insights_tools_test.go"},
		[]string{"backend/internal/mcptools/habit_tools.go"}, 1)
	infeasible := []requestdriver.DraftedTicket{
		{Filename: "001.spec.md", Content: insightsTicket},
		{Filename: "002.spec.md", Content: handlerTicket},
		{Filename: "003.spec.md", Content: mcpTicketCoveringAlone},
	}
	mcpTicketCoveringWithOthers := habitTicket("make verify",
		[]string{"backend/internal/mcptools/habit_tools.go", "backend/internal/mcptools/habit_insights_tools_test.go"},
		[]string{"backend/internal/mcptools/habit_tools.go"}, 1, 2)
	feasible := []requestdriver.DraftedTicket{
		{Filename: "001.spec.md", Content: habitTicket("make verify",
			[]string{"backend/internal/habit/insights.go", "backend/internal/habit/insights_test.go"},
			[]string{"backend/internal/habit/insights.go"}, 1, 2)},
		{Filename: "002.spec.md", Content: habitTicket("make verify",
			[]string{"backend/internal/handler/habit.go", "backend/internal/handler/habit_test.go"},
			[]string{"backend/internal/handler/habit.go"}, 1, 2)},
		{Filename: "003.spec.md", Content: mcpTicketCoveringWithOthers},
	}
	runner, callCount, feedbackByCall := sequencedPlanTicketsRunner(t, dataDir,
		func() ([]requestdriver.DraftedTicket, *request.PlanEvidence, error) {
			return infeasible, &request.PlanEvidence{}, nil
		},
		func() ([]requestdriver.DraftedTicket, *request.PlanEvidence, error) {
			return feasible, &request.PlanEvidence{}, nil
		},
	)

	if err := driveRequests(dp, context.Background(), dataDir, requestdriver.WorkerConfig{}, failingSpecDraftRunner(t), runner, failingOracleDraftRunner(t), failingBuildRunner(t)); err != nil {
		t.Fatalf("driveRequests: %v", err)
	}

	if *callCount != 2 {
		t.Fatalf("planning launches = %d, want exactly 2", *callCount)
	}
	second := (*feedbackByCall)[1]
	if !strings.Contains(second, "criterion 1 names") {
		t.Errorf("second launch's plan-feedback.md = %q, want it to name criterion 1", second)
	}
	if !strings.Contains(second, "backend/internal/habit/insights.go") || !strings.Contains(second, "backend/internal/handler/habit.go") {
		t.Errorf("second launch's plan-feedback.md = %q, want it to name both uncovered paths", second)
	}
	if strings.Contains(second, "cd backend") {
		t.Errorf("second launch's plan-feedback.md = %q, must not treat the backticked verify command as a named path", second)
	}

	loaded, err := request.Load(dataDir, id)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.State != request.StatePlanReview {
		t.Fatalf("State = %q, want %q (the second, feasible plan)", loaded.State, request.StatePlanReview)
	}
	if loaded.TicketCount != 3 {
		t.Fatalf("TicketCount = %d, want 3", loaded.TicketCount)
	}
}

// TestAdvancePlanningCriterionFilesFeasibleWhenAllOwningTicketsCoverIt
// covers the feasible counterpart directly (no re-plan involved): the
// exact same three-ticket plan as the feasible half above, drafted on
// the first attempt, reaches plan_review with no factory-authored
// rejection at all.
func TestAdvancePlanningCriterionFilesFeasibleWhenAllOwningTicketsCoverIt(t *testing.T) {
	dp := newTestDeps(t)
	dataDir, id := approvedPlanningFixture(t, habitInsightsCriterionSpec, "make verify")
	tickets := []requestdriver.DraftedTicket{
		{Filename: "001.spec.md", Content: habitTicket("make verify",
			[]string{"backend/internal/habit/insights.go", "backend/internal/habit/insights_test.go"},
			[]string{"backend/internal/habit/insights.go"}, 1, 2)},
		{Filename: "002.spec.md", Content: habitTicket("make verify",
			[]string{"backend/internal/handler/habit.go", "backend/internal/handler/habit_test.go"},
			[]string{"backend/internal/handler/habit.go"}, 1, 2)},
		{Filename: "003.spec.md", Content: habitTicket("make verify",
			[]string{"backend/internal/mcptools/habit_tools.go", "backend/internal/mcptools/habit_insights_tools_test.go"},
			[]string{"backend/internal/mcptools/habit_tools.go"}, 1, 2)},
	}
	runner, _ := stubPlanTicketsRunner(tickets, &request.PlanEvidence{}, nil)

	if err := driveRequests(dp, context.Background(), dataDir, requestdriver.WorkerConfig{}, failingSpecDraftRunner(t), runner, failingOracleDraftRunner(t), failingBuildRunner(t)); err != nil {
		t.Fatalf("driveRequests: %v", err)
	}

	loaded, err := request.Load(dataDir, id)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.State != request.StatePlanReview {
		t.Fatalf("State = %q, want %q", loaded.State, request.StatePlanReview)
	}
	if len(loaded.Rejections) != 0 {
		t.Fatalf("Rejections = %+v, want none (feasible on the first attempt)", loaded.Rejections)
	}
}

// TestAdvancePlanningCriterionFilesFeasibleWithDirectoryAllowedFiles
// covers matchesAllowed's directory-prefix form (via
// policy.PathCoveredByAllowed): a ticket declaring
// "backend/internal/habit/" (trailing slash) as Allowed-Files still
// covers criterion 1's own "backend/internal/habit/insights.go" without
// naming the file exactly.
func TestAdvancePlanningCriterionFilesFeasibleWithDirectoryAllowedFiles(t *testing.T) {
	dp := newTestDeps(t)
	dataDir, id := approvedPlanningFixture(t, habitInsightsCriterionSpec, "make verify")
	tickets := []requestdriver.DraftedTicket{
		{Filename: "001.spec.md", Content: habitTicket("make verify",
			[]string{"backend/internal/habit/", "backend/internal/habit/insights_test.go"},
			[]string{"backend/internal/habit/insights.go"}, 1, 2)},
		{Filename: "002.spec.md", Content: habitTicket("make verify",
			[]string{"backend/internal/handler/habit.go", "backend/internal/handler/habit_test.go"},
			[]string{"backend/internal/handler/habit.go"}, 1, 2)},
		{Filename: "003.spec.md", Content: habitTicket("make verify",
			[]string{"backend/internal/mcptools/habit_tools.go", "backend/internal/mcptools/habit_insights_tools_test.go"},
			[]string{"backend/internal/mcptools/habit_tools.go"}, 1, 2)},
	}
	runner, _ := stubPlanTicketsRunner(tickets, &request.PlanEvidence{}, nil)

	if err := driveRequests(dp, context.Background(), dataDir, requestdriver.WorkerConfig{}, failingSpecDraftRunner(t), runner, failingOracleDraftRunner(t), failingBuildRunner(t)); err != nil {
		t.Fatalf("driveRequests: %v", err)
	}

	loaded, err := request.Load(dataDir, id)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.State != request.StatePlanReview {
		t.Fatalf("State = %q, want %q (directory Allowed-Files entry covers the named file)", loaded.State, request.StatePlanReview)
	}
}

// TestAdvancePlanningCriterionFilesTwiceHaltsAfterTwoLaunches covers the
// bound on advancePlanning's automatic re-plan for the criterion-paths
// check specifically (the tests_added-infeasibility bound is pinned
// separately by TestAdvancePlanningInfeasibleTestsAddedTwiceHaltsAfterTwoLaunches):
// a second criterion-files-infeasible plan halts after exactly two
// launches, never a third.
func TestAdvancePlanningCriterionFilesTwiceHaltsAfterTwoLaunches(t *testing.T) {
	dp := newTestDeps(t)
	dataDir, id := approvedPlanningFixture(t, habitInsightsCriterionSpec, "make verify")
	insightsTicket := habitTicket("make verify",
		[]string{"backend/internal/habit/insights.go", "backend/internal/habit/insights_test.go"},
		[]string{"backend/internal/habit/insights.go"}, 2)
	handlerTicket := habitTicket("make verify",
		[]string{"backend/internal/handler/habit.go", "backend/internal/handler/habit_test.go"},
		[]string{"backend/internal/handler/habit.go"}, 2)
	mcpTicketCoveringAlone := habitTicket("make verify",
		[]string{"backend/internal/mcptools/habit_tools.go", "backend/internal/mcptools/habit_insights_tools_test.go"},
		[]string{"backend/internal/mcptools/habit_tools.go"}, 1)
	infeasible := []requestdriver.DraftedTicket{
		{Filename: "001.spec.md", Content: insightsTicket},
		{Filename: "002.spec.md", Content: handlerTicket},
		{Filename: "003.spec.md", Content: mcpTicketCoveringAlone},
	}
	runner, callCount, _ := sequencedPlanTicketsRunner(t, dataDir,
		func() ([]requestdriver.DraftedTicket, *request.PlanEvidence, error) {
			return infeasible, &request.PlanEvidence{}, nil
		},
		func() ([]requestdriver.DraftedTicket, *request.PlanEvidence, error) {
			return infeasible, &request.PlanEvidence{}, nil
		},
	)

	if err := driveRequests(dp, context.Background(), dataDir, requestdriver.WorkerConfig{}, failingSpecDraftRunner(t), runner, failingOracleDraftRunner(t), failingBuildRunner(t)); err != nil {
		t.Fatalf("driveRequests: %v", err)
	}

	if *callCount != 2 {
		t.Fatalf("planning launches = %d, want exactly 2 (bounded, no unbounded loop)", *callCount)
	}

	loaded, err := request.Load(dataDir, id)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.State != request.StateHalted {
		t.Fatalf("State = %q, want %q (must not reach plan_review)", loaded.State, request.StateHalted)
	}
	if !strings.Contains(loaded.Error, "criterion 1 names") {
		t.Errorf("Error = %q, want it to name criterion 1", loaded.Error)
	}
}

// TestAdvancePlanningCombinesTestsAddedAndCriterionFilesReasons covers
// #351's own combined-message requirement: a plan that is simultaneously
// tests_added-infeasible (one ticket) and criterion-files-infeasible (a
// different criterion, a different ticket) gets exactly one re-plan
// message naming BOTH infeasibilities, not just the first one found.
func TestAdvancePlanningCombinesTestsAddedAndCriterionFilesReasons(t *testing.T) {
	dp := newTestDeps(t)
	const combinedSpec = "# Spec\n\n" +
		"## Problem\n\nRefunds can be double-processed on retry.\n\n" +
		"## Scope\n\nThe /refunds endpoint only.\n\n" +
		"## Non-goals\n\nNot touching /charges.\n\n" +
		"## Affected services and packages\n\ninternal/payments, backend/internal/handler\n\n" +
		"## Acceptance criteria\n\n" +
		"1. The route is wired into the dispatch mux.\n" +
		"2. Unit tests cover `internal/payments/refunds.go`.\n\n" +
		"## Risks\n\nNone known.\n\n" +
		"## Open questions\n\nNone.\n"
	wiringOnlyTicket := infeasibleTestsAddedTicket("make verify", 1)
	mismatchedTicket := habitTicket("make verify",
		[]string{"internal/payments/other.go", "internal/payments/other_test.go"},
		[]string{"internal/payments/other.go"}, 2)

	dataDir, id := approvedPlanningFixture(t, combinedSpec, "make verify")
	tickets := []requestdriver.DraftedTicket{
		{Filename: "001.spec.md", Content: wiringOnlyTicket},
		{Filename: "002.spec.md", Content: mismatchedTicket},
	}
	runner, _ := stubPlanTicketsRunner(tickets, &request.PlanEvidence{}, nil)

	if err := driveRequests(dp, context.Background(), dataDir, requestdriver.WorkerConfig{}, failingSpecDraftRunner(t), runner, failingOracleDraftRunner(t), failingBuildRunner(t)); err != nil {
		t.Fatalf("driveRequests: %v", err)
	}

	loaded, err := request.Load(dataDir, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Rejections) != 1 {
		t.Fatalf("Rejections = %+v, want exactly one factory-authored rejection", loaded.Rejections)
	}
	reason := loaded.Rejections[0].Reason
	if !strings.Contains(reason, "tests_added") {
		t.Errorf("Rejections[0].Reason = %q, want it to name the tests_added infeasibility", reason)
	}
	if !strings.Contains(reason, "criterion 2 names") || !strings.Contains(reason, "internal/payments/refunds.go") {
		t.Errorf("Rejections[0].Reason = %q, want it to name the criterion 2 infeasibility", reason)
	}
}

// TestAdvancePlanningHashMismatchHaltsNamingSpec covers the plan's own
// requirement that verifyApprovedHashes runs before planning: an edit to
// the approved spec.md after approval must halt, naming spec.md, without
// ever calling the plan job.
func TestAdvancePlanningHashMismatchHaltsNamingSpec(t *testing.T) {
	dp := newTestDeps(t)
	dataDir, id := approvedPlanningFixture(t, twoCriteriaSpec, "make verify")
	// Edit spec.md after approval -- the exact post-approval-edit scenario
	// VerifyApprovedHashes exists to catch.
	if err := os.WriteFile(requestdriver.RequestSpecPath(dataDir, id), []byte(twoCriteriaSpec+"\nedited after approval\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := driveRequests(dp, context.Background(), dataDir, requestdriver.WorkerConfig{}, failingSpecDraftRunner(t), failingPlanTicketsRunner(t), failingOracleDraftRunner(t), failingBuildRunner(t)); err != nil {
		t.Fatalf("driveRequests: %v", err)
	}

	loaded, err := request.Load(dataDir, id)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.State != request.StateHalted {
		t.Fatalf("State = %q, want %q", loaded.State, request.StateHalted)
	}
	if !strings.Contains(loaded.Error, "spec.md") {
		t.Errorf("Error = %q, want it to name spec.md", loaded.Error)
	}
}

// countNotificationLogLines counts the JSON-lines entries in a request's
// own notifications.log, or 0 if the file doesn't exist yet.
func countNotificationLogLines(t *testing.T, dataDir, id string) int {
	t.Helper()
	b, err := os.ReadFile(requestdriver.RequestNotificationLogPath(dataDir, id))
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		t.Fatalf("read notifications.log: %v", err)
	}
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if len(lines) == 1 && lines[0] == "" {
		return 0
	}
	return len(lines)
}

// TestHaltRequestNotificationHasRetryNextAndConsoleLink proves a halted
// request's notification points the operator at retrying this request
// and, when FACTORYD_CONSOLE_URL is configured, at its console page.
func TestHaltRequestNotificationHasRetryNextAndConsoleLink(t *testing.T) {
	t.Setenv(consoleLinkEnvVar, "https://console.example")
	dataDir := t.TempDir()
	base := time.Date(2026, 9, 11, 10, 0, 0, 0, time.UTC)
	r := request.New("req-1", "/repos/app", "app", request.Source{Kind: request.SourceText}, base)
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}

	if err := requestdriver.HaltRequest(dataDir, r, "spec drafting failed", base); err != nil {
		t.Fatalf("haltRequest: %v", err)
	}

	b, err := os.ReadFile(requestdriver.RequestNotificationLogPath(dataDir, "req-1"))
	if err != nil {
		t.Fatalf("read notifications.log: %v", err)
	}
	var got run.NotificationRecord
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal notification: %v", err)
	}
	if want := "factoryd retry req-1"; got.Next != want {
		t.Errorf("Next = %q, want %q", got.Next, want)
	}
	if want := "https://console.example/requests/req-1"; got.Link != want {
		t.Errorf("Link = %q, want %q", got.Link, want)
	}
}

// TestRemindRequestNotificationHasApproveNextAndConsoleLink proves a
// spec/plan review reminder points the operator at approving (or
// rejecting) this request and, when configured, its console page.
func TestRemindRequestNotificationHasApproveNextAndConsoleLink(t *testing.T) {
	t.Setenv(consoleLinkEnvVar, "https://console.example")
	dataDir := t.TempDir()
	base := time.Date(2026, 9, 11, 10, 0, 0, 0, time.UTC)
	r := request.New("req-1", "/repos/app", "app", request.Source{Kind: request.SourceText}, base)
	r.State = request.StateSpecReview

	requestdriver.RemindRequest(dataDir, r, base)

	b, err := os.ReadFile(requestdriver.RequestNotificationLogPath(dataDir, "req-1"))
	if err != nil {
		t.Fatalf("read notifications.log: %v", err)
	}
	var got run.NotificationRecord
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal notification: %v", err)
	}
	if !strings.Contains(got.Next, "factoryd approve req-1") {
		t.Errorf("Next = %q, want it to include factoryd approve req-1", got.Next)
	}
	if want := "https://console.example/requests/req-1"; got.Link != want {
		t.Errorf("Link = %q, want %q", got.Link, want)
	}
}

// TestRemindDueRequestsSendsImmediateThenEveryInterval is the plan's own
// required fake-clock test: a request left in spec_review for 31 minutes
// with a 15m reminder interval produces exactly three notifications --
// immediate (t+0), t+15m, and t+30m -- never a fourth at t+31m.
func TestRemindDueRequestsSendsImmediateThenEveryInterval(t *testing.T) {
	dataDir := t.TempDir()
	if err := request.SaveText(dataDir, "req-1", "text"); err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 9, 11, 10, 0, 0, 0, time.UTC)
	r := request.New("req-1", "/repos/app", "app", request.Source{Kind: request.SourceText}, base)
	r.State = request.StateSpecReview
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}

	// The driver's own immediate reminder on entering spec_review, at t+0.
	clock := base
	now := func() time.Time { return clock }
	loaded, err := request.Load(dataDir, "req-1")
	if err != nil {
		t.Fatal(err)
	}
	requestdriver.RemindRequest(dataDir, loaded, now())
	if err := loaded.Save(dataDir); err != nil {
		t.Fatal(err)
	}

	interval := 15 * time.Minute
	for minute := 1; minute <= 31; minute++ {
		clock = base.Add(time.Duration(minute) * time.Minute)
		if err := requestdriver.RemindDueRequests(dataDir, interval, now); err != nil {
			t.Fatalf("remindDueRequests at t+%dm: %v", minute, err)
		}
	}

	if got := countNotificationLogLines(t, dataDir, "req-1"); got != 3 {
		t.Errorf("notifications.log lines = %d, want 3 (immediate, +15m, +30m)", got)
	}
	final, err := request.Load(dataDir, "req-1")
	if err != nil {
		t.Fatal(err)
	}
	if final.NotifyCount != 3 {
		t.Errorf("NotifyCount = %d, want 3", final.NotifyCount)
	}
	if final.WaitingSince != base.UTC().Format(time.RFC3339Nano) {
		t.Errorf("WaitingSince = %q, want the original entry time %q (must not restart on later reminders)", final.WaitingSince, base.UTC().Format(time.RFC3339Nano))
	}
}

// TestRemindDueRequestsStopsAfterApproval is the plan's own "approval
// stops reminders within one tick" requirement: once Approve has moved
// the request out of spec_review (clearing its reminder state, see
// internal/request.Approve), the next remindDueRequests call, even well
// past the interval, sends nothing.
func TestRemindDueRequestsStopsAfterApproval(t *testing.T) {
	dataDir := t.TempDir()
	if err := request.SaveText(dataDir, "req-1", "text"); err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 9, 11, 10, 0, 0, 0, time.UTC)
	r := request.New("req-1", "/repos/app", "app", request.Source{Kind: request.SourceText}, base)
	r.State = request.StateSpecReview
	if err := os.WriteFile(filepath.Join(request.Dir(dataDir, "req-1"), "spec.md"), []byte("# Spec\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	requestdriver.RemindRequest(dataDir, r, base)
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}

	if _, err := request.Approve(dataDir, "req-1", "alice", base.Add(5*time.Minute), nil); err != nil {
		t.Fatalf("Approve: %v", err)
	}

	afterInterval := base.Add(time.Hour)
	if err := requestdriver.RemindDueRequests(dataDir, 15*time.Minute, func() time.Time { return afterInterval }); err != nil {
		t.Fatalf("remindDueRequests after approval: %v", err)
	}
	if got := countNotificationLogLines(t, dataDir, "req-1"); got != 1 {
		t.Errorf("notifications.log lines after approval = %d, want 1 (only the pre-approval reminder, none since)", got)
	}
}

// TestRemindDueRequestsIsRestartSafe is the plan's own restart-safety
// requirement: a request whose LastNotifiedAt is only 5 minutes old, read
// fresh off disk by a brand-new process (simulated here by simply calling
// remindDueRequests directly, with no prior in-process state at all),
// sends nothing under a 15m interval -- a restart never re-sends an
// already-current reminder.
func TestRemindDueRequestsIsRestartSafe(t *testing.T) {
	dataDir := t.TempDir()
	if err := request.SaveText(dataDir, "req-1", "text"); err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 9, 11, 10, 0, 0, 0, time.UTC)
	r := request.New("req-1", "/repos/app", "app", request.Source{Kind: request.SourceText}, base)
	r.State = request.StateSpecReview
	r.WaitingSince = base.UTC().Format(time.RFC3339Nano)
	r.LastNotifiedAt = base.UTC().Format(time.RFC3339Nano)
	r.NotifyCount = 1
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}

	restartNow := base.Add(5 * time.Minute)
	if err := requestdriver.RemindDueRequests(dataDir, 15*time.Minute, func() time.Time { return restartNow }); err != nil {
		t.Fatalf("remindDueRequests: %v", err)
	}
	if got := countNotificationLogLines(t, dataDir, "req-1"); got != 0 {
		t.Errorf("notifications.log lines = %d, want 0 (last reminder only 5m old, restart must not re-send)", got)
	}
	loaded, err := request.Load(dataDir, "req-1")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.NotifyCount != 1 {
		t.Errorf("NotifyCount = %d, want unchanged 1", loaded.NotifyCount)
	}
}

// buildingFixture drives a request all the way from an approved spec
// (approvedPlanningFixture) through planning (n tickets, each claiming
// twoCriteriaSpec's own first criterion, real files under
// <dataDir>/requests/<id>/tickets/) and a plan approval (plan_review ->
// building), returning it ready for advanceBuilding/driveRequests to pick
// up ticket 1.
func buildingFixture(dp *deps, t *testing.T, n int) (dataDir, id string) {
	t.Helper()
	dataDir, id = approvedPlanningFixture(t, twoCriteriaSpec, "make verify")
	tickets := make([]requestdriver.DraftedTicket, 0, n)
	for i := 1; i <= n; i++ {
		// Each ticket claims both of twoCriteriaSpec's own acceptance
		// criteria -- simpler than splitting them across tickets, and
		// ValidatePlanCoverage only requires every criterion be claimed by
		// at least one ticket, not by exactly one.
		tickets = append(tickets, requestdriver.DraftedTicket{
			Filename: fmt.Sprintf("%03d.spec.md", i),
			Content:  validBrownfieldTicket("make verify", 1, 2),
		})
	}
	runner, _ := stubPlanTicketsRunner(tickets, &request.PlanEvidence{}, nil)
	if err := driveRequests(dp, context.Background(), dataDir, requestdriver.WorkerConfig{}, failingSpecDraftRunner(t), runner, failingOracleDraftRunner(t), failingBuildRunner(t)); err != nil {
		t.Fatalf("driveRequests (planning): %v", err)
	}
	if _, err := request.Approve(dataDir, id, "alice", time.Now(), nil); err != nil {
		t.Fatalf("approve plan: %v", err)
	}
	return dataDir, id
}

// ticketRunID is buildingFixture's own ticket-id convention, matching
// buildRequestBuildArgs' "<request-id>-<index, %03d>".
func ticketRunID(id string, index int) string {
	return fmt.Sprintf("%s-%03d", id, index)
}

// argValue returns the value following flag in args, or "" if flag is
// absent or has no following value.
func argValue(args []string, flag string) string {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

// hasFlag reports whether the bare flag (a boolean flag with no value,
// e.g. -open-pull-request) is present in args.
func hasFlag(args []string, flag string) bool {
	for _, a := range args {
		if a == flag {
			return true
		}
	}
	return false
}

// acceptingBuildRunner is a stub ticketRunner that fires onReady with the
// run id runMainWithReady would actually assign (the -ticket argument
// itself -- see buildRequestBuildArgs), then durably records that run as
// accepted with a synthesized PR URL, mirroring what a real ticket build
// leaves behind for advanceBuilding to read back via run.Load.
func acceptingBuildRunner(t *testing.T, dataDir string) requestdriver.TicketRunner {
	return func(ctx context.Context, args []string, onReady func(*run.Run)) error {
		ticket := argValue(args, "-ticket")
		if onReady != nil {
			onReady(&run.Run{ID: ticket, State: run.StateReady})
		}
		rr := &run.Run{ID: ticket, State: run.StateAccepted, BaseSHA: fmt.Sprintf("%040d", 1), PullRequestURL: "https://github.com/acme/app/pull/" + ticket}
		if err := rr.Save(dataDir); err != nil {
			t.Fatalf("save stub run %q: %v", ticket, err)
		}
		return nil
	}
}

// TestAdvanceBuildingBuildsThreeTicketsInOrderWithPriorRunChaining covers
// the plan's own "a three-ticket fixture builds 1, 2, 3 in order with
// chained -prior-run" done-when item, argv proven through the real flag
// set the same way TestBuildRequestBuildArgsIsAcceptedByRunMainWithReadysOwnFlagSet
// does.
func TestAdvanceBuildingBuildsThreeTicketsInOrderWithPriorRunChaining(t *testing.T) {
	dp := newTestDeps(t)
	dataDir, id := buildingFixture(dp, t, 3)

	var gotArgs [][]string
	base := acceptingBuildRunner(t, dataDir)
	runner := func(ctx context.Context, args []string, onReady func(*run.Run)) error {
		gotArgs = append(gotArgs, append([]string(nil), args...))
		return base(ctx, args, onReady)
	}

	for i := 1; i <= 3; i++ {
		if err := driveRequests(dp, context.Background(), dataDir, requestdriver.WorkerConfig{OpenPullRequest: true}, failingSpecDraftRunner(t), failingPlanTicketsRunner(t), failingOracleDraftRunner(t), runner); err != nil {
			t.Fatalf("driveRequests (ticket %d): %v", i, err)
		}
	}

	if len(gotArgs) != 3 {
		t.Fatalf("build runner called %d times, want 3", len(gotArgs))
	}
	for i, args := range gotArgs {
		wantTicket := ticketRunID(id, i+1)
		if got := argValue(args, "-ticket"); got != wantTicket {
			t.Errorf("call %d: -ticket = %q, want %q", i, got, wantTicket)
		}
		if !hasFlag(args, "-open-pull-request") {
			t.Errorf("call %d: missing -open-pull-request", i)
		}
		if i == 0 {
			if got := argValue(args, "-prior-run"); got != "" {
				t.Errorf("call %d: -prior-run = %q, want none on the first ticket", i, got)
			}
		} else {
			wantPrior := ticketRunID(id, i)
			if got := argValue(args, "-prior-run"); got != wantPrior {
				t.Errorf("call %d: -prior-run = %q, want %q", i, got, wantPrior)
			}
		}
	}

	// Argv for the per-ticket run is parsed through the real flag set
	// (the plan's own Ground Rules requirement), mirroring
	// TestBuildRequestBuildArgsIsAcceptedByRunMainWithReadysOwnFlagSet:
	// checked on the first (no -prior-run) and last (-prior-run present)
	// calls, so -prior-run's own presence is proven accepted too.
	for _, args := range []([]string){gotArgs[0], gotArgs[2]} {
		err := runMainWithReady(dp, context.Background(), args, nil)
		if err == nil {
			t.Fatal("runMainWithReady(driveRequests' own argv) = nil, want the required-flags error it stops at")
		}
		if strings.Contains(err.Error(), "not defined") {
			t.Fatalf("runMainWithReady rejected an argv driveRequests produced: %v", err)
		}
	}

	loaded, err := request.Load(dataDir, id)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.State != request.StatePRReview {
		t.Fatalf("State = %q, want %q", loaded.State, request.StatePRReview)
	}
	if loaded.TicketIndex != 3 {
		t.Errorf("TicketIndex = %d, want 3", loaded.TicketIndex)
	}
	for i, tk := range loaded.Tickets {
		wantRunID := ticketRunID(id, i+1)
		if tk.RunID != wantRunID {
			t.Errorf("Tickets[%d].RunID = %q, want %q", i, tk.RunID, wantRunID)
		}
		if tk.PRURL == "" || tk.PRState != "draft" {
			t.Errorf("Tickets[%d] = %+v, want a PR URL and PRState %q", i, tk, "draft")
		}
	}

	// M4-K1 regression: advanceBuilding's own onReady callback used to
	// call startedRun.Save directly, bypassing RecordEvent entirely, so
	// a ticket's run never appended to events.db the moment it started.
	// It now goes through startedRun.Persist instead.
	s, err := store.Open(run.EventsDBPath(dataDir))
	if err != nil {
		t.Fatalf("open event store: %v", err)
	}
	defer s.Close()
	for i := 1; i <= 3; i++ {
		events, err := s.List(context.Background(), ticketRunID(id, i))
		if err != nil {
			t.Fatalf("list events for ticket %d's run: %v", i, err)
		}
		if len(events) == 0 {
			t.Errorf("ticket %d's run: events = [], want at least one durable event recorded when its run started", i)
		}
	}
}

// TestAdvanceBuildingQuarantineOfTicket2LeavesTicket1IntactAndRetryable
// covers the plan's own "quarantine of ticket 2 leaves ticket 1's PR and
// run intact and the request retryable from ticket 2" done-when item.
func TestAdvanceBuildingQuarantineOfTicket2LeavesTicket1IntactAndRetryable(t *testing.T) {
	dp := newTestDeps(t)
	dataDir, id := buildingFixture(dp, t, 3)
	accept := acceptingBuildRunner(t, dataDir)
	runner := func(ctx context.Context, args []string, onReady func(*run.Run)) error {
		ticket := argValue(args, "-ticket")
		if ticket != ticketRunID(id, 2) {
			return accept(ctx, args, onReady)
		}
		if onReady != nil {
			onReady(&run.Run{ID: ticket, State: run.StateReady})
		}
		rr := &run.Run{ID: ticket, State: run.StateQuarantined, HaltError: "gate failed: tests_added"}
		return rr.Save(dataDir)
	}

	for i := 1; i <= 2; i++ {
		if err := driveRequests(dp, context.Background(), dataDir, requestdriver.WorkerConfig{}, failingSpecDraftRunner(t), failingPlanTicketsRunner(t), failingOracleDraftRunner(t), runner); err != nil {
			t.Fatalf("driveRequests (ticket %d): %v", i, err)
		}
	}

	loaded, err := request.Load(dataDir, id)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.State != request.StateQuarantined {
		t.Fatalf("State = %q, want %q", loaded.State, request.StateQuarantined)
	}
	if !strings.Contains(loaded.Error, "ticket 2/3") || !strings.Contains(loaded.Error, "quarantined") || !strings.Contains(loaded.Error, "gate failed: tests_added") {
		t.Errorf("Error = %q, want it to name ticket 2/3, quarantined, and the run's own reason", loaded.Error)
	}
	if got := loaded.Tickets[0].RunID; got != ticketRunID(id, 1) {
		t.Errorf("Tickets[0].RunID = %q, want %q (ticket 1 untouched)", got, ticketRunID(id, 1))
	}
	if loaded.Tickets[0].PRURL == "" {
		t.Errorf("Tickets[0].PRURL is empty, want ticket 1's PR left intact")
	}
	if got := loaded.Tickets[1].RunID; got != ticketRunID(id, 2) {
		t.Errorf("Tickets[1].RunID = %q, want %q (recorded even though ticket 2 quarantined)", got, ticketRunID(id, 2))
	}

	handled, err := retryRequest(dp, dataDir, loaded, "", time.Now())
	if !handled {
		t.Fatal("retryRequest: handled = false, want true for a quarantined request with TicketIndex within range")
	}
	if err != nil {
		t.Fatalf("retryRequest: %v", err)
	}
	retried, err := request.Load(dataDir, id)
	if err != nil {
		t.Fatal(err)
	}
	if retried.State != request.StateBuilding {
		t.Fatalf("State after retry = %q, want %q", retried.State, request.StateBuilding)
	}
	if retried.TicketIndex != 2 {
		t.Errorf("TicketIndex after retry = %d, want 2 (retries from the failed ticket)", retried.TicketIndex)
	}
	if got := retried.Tickets[0].RunID; got != ticketRunID(id, 1) {
		t.Errorf("Tickets[0].RunID after retry = %q, want %q (still untouched)", got, ticketRunID(id, 1))
	}
}

// TestAdvanceBuildingQuarantineReportedViaRunnerErrorStillQuarantinesRequest
// covers the production shape TestAdvanceBuildingQuarantineOfTicket2Leaves...
// above does not: runMainWithReady (run_ticket.go) returns a non-nil
// "run quarantined: ..." error for *any* non-Accepted terminal run state,
// not just a genuine start failure -- so the real ticketRunner a ticket
// build uses in production returns runErr != nil for an ordinary,
// correctly-recorded quarantine. Found in review: advanceBuilding used to
// treat any non-nil runErr as an unconditional haltRequest, without ever
// consulting the run's own recorded state, making request-level
// `quarantined` (and its own console callout) unreachable via this path.
func TestAdvanceBuildingQuarantineReportedViaRunnerErrorStillQuarantinesRequest(t *testing.T) {
	dp := newTestDeps(t)
	dataDir, id := buildingFixture(dp, t, 3)
	accept := acceptingBuildRunner(t, dataDir)
	runner := func(ctx context.Context, args []string, onReady func(*run.Run)) error {
		ticket := argValue(args, "-ticket")
		if ticket != ticketRunID(id, 2) {
			return accept(ctx, args, onReady)
		}
		if onReady != nil {
			onReady(&run.Run{ID: ticket, State: run.StateReady})
		}
		rr := &run.Run{ID: ticket, State: run.StateQuarantined, HaltError: "gate failed: tests_added"}
		if err := rr.Save(dataDir); err != nil {
			return err
		}
		// Mirrors runMainWithReady's own final lines: it returns a
		// non-nil error whenever the run's terminal state isn't Accepted,
		// even though the quarantine above is already durable.
		return fmt.Errorf("run quarantined: gate failed: tests_added")
	}

	for i := 1; i <= 2; i++ {
		if err := driveRequests(dp, context.Background(), dataDir, requestdriver.WorkerConfig{}, failingSpecDraftRunner(t), failingPlanTicketsRunner(t), failingOracleDraftRunner(t), runner); err != nil {
			t.Fatalf("driveRequests (ticket %d): %v", i, err)
		}
	}

	loaded, err := request.Load(dataDir, id)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.State != request.StateQuarantined {
		t.Fatalf("State = %q, want %q (runErr != nil must not override the run's own recorded quarantine)", loaded.State, request.StateQuarantined)
	}
	if !strings.Contains(loaded.Error, "ticket 2/3") || !strings.Contains(loaded.Error, "quarantined") || !strings.Contains(loaded.Error, "gate failed: tests_added") {
		t.Errorf("Error = %q, want it to name ticket 2/3, quarantined, and the run's own reason", loaded.Error)
	}
	if got := loaded.Tickets[0].RunID; got != ticketRunID(id, 1) {
		t.Errorf("Tickets[0].RunID = %q, want %q (ticket 1 untouched)", got, ticketRunID(id, 1))
	}
}

// TestAdvanceBuildingRetryStartFailureDoesNotReplayStaleRunID covers a
// Codex review finding on PR #135: retryRequest rebuilds a
// quarantined/halted ticket without clearing its previous RunID, so a
// fresh runner call that fails *before* onReady fires this time must not
// let ticket.RunID's leftover value from the prior attempt cause
// advanceBuilding to load and replay that old (already-superseded)
// outcome instead of reporting the new start failure.
func TestAdvanceBuildingRetryStartFailureDoesNotReplayStaleRunID(t *testing.T) {
	dp := newTestDeps(t)
	dataDir, id := buildingFixture(dp, t, 3)
	accept := acceptingBuildRunner(t, dataDir)
	quarantineTicket2 := func(ctx context.Context, args []string, onReady func(*run.Run)) error {
		ticket := argValue(args, "-ticket")
		if ticket != ticketRunID(id, 2) {
			return accept(ctx, args, onReady)
		}
		if onReady != nil {
			onReady(&run.Run{ID: ticket, State: run.StateReady})
		}
		rr := &run.Run{ID: ticket, State: run.StateQuarantined, HaltError: "gate failed: tests_added"}
		if err := rr.Save(dataDir); err != nil {
			return err
		}
		return fmt.Errorf("run quarantined: gate failed: tests_added")
	}

	for i := 1; i <= 2; i++ {
		if err := driveRequests(dp, context.Background(), dataDir, requestdriver.WorkerConfig{}, failingSpecDraftRunner(t), failingPlanTicketsRunner(t), failingOracleDraftRunner(t), quarantineTicket2); err != nil {
			t.Fatalf("driveRequests (ticket %d): %v", i, err)
		}
	}

	loaded, err := request.Load(dataDir, id)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.State != request.StateQuarantined {
		t.Fatalf("State = %q, want %q", loaded.State, request.StateQuarantined)
	}

	if _, err := retryRequest(dp, dataDir, loaded, "", time.Now()); err != nil {
		t.Fatalf("retryRequest: %v", err)
	}
	retried, err := request.Load(dataDir, id)
	if err != nil {
		t.Fatal(err)
	}
	if retried.State != request.StateBuilding {
		t.Fatalf("State after retry = %q, want %q", retried.State, request.StateBuilding)
	}
	if got := retried.Tickets[1].RunID; got != ticketRunID(id, 2) {
		t.Fatalf("Tickets[1].RunID after retry = %q, want %q (retryRequest leaves the prior attempt's RunID in place)", got, ticketRunID(id, 2))
	}

	// The next runner call for ticket 2 fails before onReady fires at
	// all -- a fresh start failure, unrelated to the prior quarantine.
	startFailure := func(ctx context.Context, args []string, onReady func(*run.Run)) error {
		ticket := argValue(args, "-ticket")
		if ticket != ticketRunID(id, 2) {
			return accept(ctx, args, onReady)
		}
		return errors.New("sandbox image pull failed")
	}
	if err := driveRequests(dp, context.Background(), dataDir, requestdriver.WorkerConfig{}, failingSpecDraftRunner(t), failingPlanTicketsRunner(t), failingOracleDraftRunner(t), startFailure); err != nil {
		t.Fatalf("driveRequests after retry: %v", err)
	}

	final, err := request.Load(dataDir, id)
	if err != nil {
		t.Fatal(err)
	}
	if final.State != request.StateHalted {
		t.Fatalf("State after retry+start-failure = %q, want %q (must not replay the stale quarantine)", final.State, request.StateHalted)
	}
	if !strings.Contains(final.Error, "start failed") || !strings.Contains(final.Error, "sandbox image pull failed") {
		t.Errorf("Error = %q, want it to name the new start failure", final.Error)
	}
	if strings.Contains(final.Error, "tests_added") {
		t.Errorf("Error = %q, must not replay the stale quarantine's own reason", final.Error)
	}
}

// TestAdvanceBuildingHaltedRunWithEmptyRecordFallsBackToRunnerError covers
// a second Codex review finding on PR #135: a run that halts on a
// post-onReady infrastructure check (e.g. runMainWithReady's own "capture
// base SHA" failure) can persist StateHalted without ever populating
// HaltError/HaltReasonCode/GateResults, leaving statusReason empty. The
// request's own halted reason must still name the actual cause via
// runErr, not go silently blank.
func TestAdvanceBuildingHaltedRunWithEmptyRecordFallsBackToRunnerError(t *testing.T) {
	dp := newTestDeps(t)
	dataDir, id := buildingFixture(dp, t, 1)
	runner := func(ctx context.Context, args []string, onReady func(*run.Run)) error {
		ticket := argValue(args, "-ticket")
		if onReady != nil {
			onReady(&run.Run{ID: ticket, State: run.StateReady})
		}
		// A halted record with none of HaltError/HaltReasonCode/
		// GateResults set -- statusReason returns "" for this.
		rr := &run.Run{ID: ticket, State: run.StateHalted}
		if err := rr.Save(dataDir); err != nil {
			return err
		}
		return errors.New("capture base SHA: git rev-parse HEAD: exit status 128")
	}

	if err := driveRequests(dp, context.Background(), dataDir, requestdriver.WorkerConfig{}, failingSpecDraftRunner(t), failingPlanTicketsRunner(t), failingOracleDraftRunner(t), runner); err != nil {
		t.Fatalf("driveRequests: %v", err)
	}

	loaded, err := request.Load(dataDir, id)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.State != request.StateHalted {
		t.Fatalf("State = %q, want %q", loaded.State, request.StateHalted)
	}
	if !strings.Contains(loaded.Error, "capture base SHA") {
		t.Errorf("Error = %q, want it to fall back to runErr's text when the run record itself carries no reason", loaded.Error)
	}
}

// TestAdvanceBuildingRunnerErrorWithNoRunIDStillHalts covers the other
// half of the same fix: when the runner's onReady callback never fires at
// all -- no run record was ever minted -- runErr is the only signal
// available, and this must remain a genuine haltRequest, not something
// that tries (and fails) to load a nonexistent run record.
func TestAdvanceBuildingRunnerErrorWithNoRunIDStillHalts(t *testing.T) {
	dp := newTestDeps(t)
	dataDir, id := buildingFixture(dp, t, 1)
	runner := func(ctx context.Context, args []string, onReady func(*run.Run)) error {
		return errors.New("sandbox image pull failed")
	}

	if err := driveRequests(dp, context.Background(), dataDir, requestdriver.WorkerConfig{}, failingSpecDraftRunner(t), failingPlanTicketsRunner(t), failingOracleDraftRunner(t), runner); err != nil {
		t.Fatalf("driveRequests: %v", err)
	}

	loaded, err := request.Load(dataDir, id)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.State != request.StateHalted {
		t.Fatalf("State = %q, want %q", loaded.State, request.StateHalted)
	}
	if !strings.Contains(loaded.Error, "start failed") || !strings.Contains(loaded.Error, "sandbox image pull failed") {
		t.Errorf("Error = %q, want it to name the start failure", loaded.Error)
	}
	if got := loaded.Tickets[0].RunID; got != "" {
		t.Errorf("Tickets[0].RunID = %q, want empty (run never started)", got)
	}
}

// TestStartNextTicketOrFinishPRApprovedMovesToPRReviewAfterEveryTicket
// covers the plan's own "advance_on: pr_approved moves to pr_review
// after ticket 1 instead of starting ticket 2" done-when item.
func TestStartNextTicketOrFinishPRApprovedMovesToPRReviewAfterEveryTicket(t *testing.T) {
	dp := newTestDeps(t)
	dataDir, id := buildingFixture(dp, t, 2)
	previous := requestdriver.RequestAdvanceOn
	requestdriver.RequestAdvanceOn = requestdriver.AdvanceOnPRApproved
	defer func() { requestdriver.RequestAdvanceOn = previous }()

	runner := acceptingBuildRunner(t, dataDir)
	if err := driveRequests(dp, context.Background(), dataDir, requestdriver.WorkerConfig{}, failingSpecDraftRunner(t), failingPlanTicketsRunner(t), failingOracleDraftRunner(t), runner); err != nil {
		t.Fatalf("driveRequests: %v", err)
	}

	loaded, err := request.Load(dataDir, id)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.State != request.StatePRReview {
		t.Fatalf("State = %q, want %q (pr_approved moves to pr_review after every ticket, not just the last)", loaded.State, request.StatePRReview)
	}
	if loaded.TicketIndex != 1 {
		t.Errorf("TicketIndex = %d, want unchanged 1 -- advancing it is the PR-review driver's job, once the PR is approved", loaded.TicketIndex)
	}
}

// TestAdvanceBuildingHashMismatchHaltsNamingFile covers the plan's own
// "hash mismatch on entering building halts naming the file" done-when
// item, mirroring TestAdvancePlanningHashMismatchHaltsNamingSpec for a
// ticket spec instead of spec.md.
func TestAdvanceBuildingHashMismatchHaltsNamingFile(t *testing.T) {
	dp := newTestDeps(t)
	dataDir, id := buildingFixture(dp, t, 1)

	before, err := request.Load(dataDir, id)
	if err != nil {
		t.Fatal(err)
	}
	ticketPath := before.Tickets[0].SpecPath
	orig, err := os.ReadFile(ticketPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ticketPath, append(orig, []byte("\nedited after approval\n")...), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := driveRequests(dp, context.Background(), dataDir, requestdriver.WorkerConfig{}, failingSpecDraftRunner(t), failingPlanTicketsRunner(t), failingOracleDraftRunner(t), failingBuildRunner(t)); err != nil {
		t.Fatalf("driveRequests: %v", err)
	}

	after, err := request.Load(dataDir, id)
	if err != nil {
		t.Fatal(err)
	}
	if after.State != request.StateHalted {
		t.Fatalf("State = %q, want %q", after.State, request.StateHalted)
	}
	if !strings.Contains(after.Error, filepath.Base(ticketPath)) {
		t.Errorf("Error = %q, want it to name %s", after.Error, filepath.Base(ticketPath))
	}
}

// TestAdvancePlanningSendsImmediatePlanReviewReminder: entering
// plan_review fires the first reminder at once (durable log entry,
// NotifyCount 1), the same guarantee spec_review already has.
func TestAdvancePlanningSendsImmediatePlanReviewReminder(t *testing.T) {
	dp := newTestDeps(t)
	dataDir, id := approvedPlanningFixture(t, twoCriteriaSpec, "make verify")
	tickets := []requestdriver.DraftedTicket{
		{Filename: "001.spec.md", Content: validBrownfieldTicket("make verify", 1, 2)},
	}
	runner, _ := stubPlanTicketsRunner(tickets, &request.PlanEvidence{}, nil)

	if err := driveRequests(dp, context.Background(), dataDir, requestdriver.WorkerConfig{}, failingSpecDraftRunner(t), runner, failingOracleDraftRunner(t), failingBuildRunner(t)); err != nil {
		t.Fatalf("driveRequests: %v", err)
	}
	loaded, err := request.Load(dataDir, id)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.State != request.StatePlanReview {
		t.Fatalf("State = %q, want %q", loaded.State, request.StatePlanReview)
	}
	if loaded.NotifyCount != 1 || loaded.LastNotifiedAt == "" {
		t.Fatalf("NotifyCount = %d, LastNotifiedAt = %q; want 1 and set", loaded.NotifyCount, loaded.LastNotifiedAt)
	}
	if got := countNotificationLogLines(t, dataDir, id); got != 1 {
		t.Fatalf("notifications.log entries = %d, want 1", got)
	}
}

// TestAdvancePlanningHaltDoesNotStartReminders: a halted plan pass must
// never begin the plan_review reminder cycle.
func TestAdvancePlanningHaltDoesNotStartReminders(t *testing.T) {
	dp := newTestDeps(t)
	dataDir, id := approvedPlanningFixture(t, twoCriteriaSpec, "make verify")
	tickets := []requestdriver.DraftedTicket{
		{Filename: "001.spec.md", Content: validBrownfieldTicket("make verify", 1)}, // criterion 2 unclaimed
	}
	runner, _ := stubPlanTicketsRunner(tickets, &request.PlanEvidence{}, nil)

	if err := driveRequests(dp, context.Background(), dataDir, requestdriver.WorkerConfig{}, failingSpecDraftRunner(t), runner, failingOracleDraftRunner(t), failingBuildRunner(t)); err != nil {
		t.Fatalf("driveRequests: %v", err)
	}
	loaded, err := request.Load(dataDir, id)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.State != request.StateHalted {
		t.Fatalf("State = %q, want %q", loaded.State, request.StateHalted)
	}
	if loaded.NotifyCount != 0 || loaded.WaitingSince != "" {
		t.Fatalf("NotifyCount = %d, WaitingSince = %q; want 0 and empty", loaded.NotifyCount, loaded.WaitingSince)
	}
}

// TestBuildRequestBuildArgsWritesCoveredCriteriaFile: a plan-format
// ticket's build carries -spec-acceptance-criteria pointing at a file
// holding exactly the spec criteria the ticket claims, verbatim, so the
// required conformity review judges this ticket's own criteria only.
func TestBuildRequestBuildArgsWritesCoveredCriteriaFile(t *testing.T) {
	dataDir := t.TempDir()
	r := &request.Request{ID: "req-1", Workspace: "/repos/app", Project: "app"}
	if err := os.MkdirAll(filepath.Join(request.Dir(dataDir, r.ID), "tickets"), 0o750); err != nil {
		t.Fatal(err)
	}
	spec := "# Spec\n\n## Problem\n\nx\n\n## Scope\n\nx\n\n## Non-goals\n\nx\n\n## Affected services and packages\n\nx\n\n## Acceptance criteria\n\n1. First thing works.\n2. Second thing works.\n3. Third thing works.\n\n## Risks\n\nx\n\n## Open questions\n\nx\n"
	if err := os.WriteFile(requestdriver.RequestSpecPath(dataDir, r.ID), []byte(spec), 0o600); err != nil {
		t.Fatal(err)
	}
	ticketPath := filepath.Join(request.Dir(dataDir, r.ID), "tickets", "002.spec.md")
	if err := os.WriteFile(ticketPath, []byte(validBrownfieldTicket("make verify", 1, 3)), 0o600); err != nil {
		t.Fatal(err)
	}
	seedFirstTicketRun(t, dataDir, r)

	args, err := requestdriver.BuildRequestBuildArgs(dataDir, r, request.Ticket{Index: 2, SpecPath: ticketPath}, requestdriver.WorkerConfig{})
	if err != nil {
		t.Fatalf("buildRequestBuildArgs: %v", err)
	}
	var gotCriteria string
	for i, a := range args {
		if a == "-spec-acceptance-criteria" {
			gotCriteria = args[i+1]
		}
	}
	want := filepath.Join(request.Dir(dataDir, r.ID), "tickets", "002.criteria.md")
	if gotCriteria != want {
		t.Fatalf("-spec-acceptance-criteria = %q, want %q", gotCriteria, want)
	}
	content, err := os.ReadFile(gotCriteria)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "1. First thing works.\n3. Third thing works.\n" {
		t.Fatalf("criteria file = %q", content)
	}

	// A claim outside the spec's own numbering is refused, not silently
	// dropped: the build would otherwise review against the wrong set.
	if err := os.WriteFile(ticketPath, []byte(validBrownfieldTicket("make verify", 4)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := requestdriver.BuildRequestBuildArgs(dataDir, r, request.Ticket{Index: 2, SpecPath: ticketPath}, requestdriver.WorkerConfig{}); err == nil {
		t.Fatal("expected an error for a criterion the spec does not declare")
	}
}

// habitInsightsSpec mirrors the live incident's own shape (Flutter + Go app Track
// M-E1, 2026-09-28, feature-habit-insights-endpoint-and-mcp): an approved
// spec whose acceptance criteria name exact JSON field names a builder
// must reproduce verbatim, not paraphrase.
const habitInsightsSpec = `# Spec

## Problem

Habit insights need a stable JSON contract.

## Scope

The /habits/insights endpoint.

## Non-goals

Not touching /habits itself.

## Affected services and packages

internal/habits

## Acceptance criteria

1. First thing works.
2. Second thing works.
3. Third thing works.
4. Fourth thing works.
5. Fifth thing works.
6. Sixth thing works.
7. Seventh thing works.
8. The response body includes an integer field named exactly longest_streak.
9. The response body includes a weekly entries array whose objects each carry an integer field named exactly best_weekday_done_count, and a per-day field named exactly done_count.

## Risks

None known.

## Open questions

None.
`

// TestBuildRequestBuildArgsBuildSpecCarriesCriteriaText covers the live
// bug this closes: the file a ticket's build actually receives as -spec
// used to carry only the "### Acceptance criteria covered" NUMBERS
// (e.g. "8, 9"), never the approved spec's own exact criteria TEXT --
// forcing the builder to guess field names it could have read verbatim.
// -spec must now point at a derived build-spec file containing the
// ticket's own spec verbatim followed by the exact text of every
// criterion it covers, in the live case's own shape (covered "8, 9" ->
// the verbatim text of criteria 8 and 9, naming longest_streak,
// best_weekday_done_count and done_count exactly).
func TestBuildRequestBuildArgsBuildSpecCarriesCriteriaText(t *testing.T) {
	dataDir := t.TempDir()
	r := &request.Request{ID: "req-1", Workspace: "/repos/app", Project: "app"}
	if err := os.MkdirAll(filepath.Join(request.Dir(dataDir, r.ID), "tickets"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(requestdriver.RequestSpecPath(dataDir, r.ID), []byte(habitInsightsSpec), 0o600); err != nil {
		t.Fatal(err)
	}
	ticketPath := filepath.Join(request.Dir(dataDir, r.ID), "tickets", "002.spec.md")
	if err := os.WriteFile(ticketPath, []byte(validBrownfieldTicket("make verify", 8, 9)), 0o600); err != nil {
		t.Fatal(err)
	}
	seedFirstTicketRun(t, dataDir, r)

	args, err := requestdriver.BuildRequestBuildArgs(dataDir, r, request.Ticket{Index: 2, SpecPath: ticketPath}, requestdriver.WorkerConfig{})
	if err != nil {
		t.Fatalf("buildRequestBuildArgs: %v", err)
	}
	gotSpec := argValue(args, "-spec")
	wantSpec := filepath.Join(request.Dir(dataDir, r.ID), "tickets", "002.build.md")
	if gotSpec != wantSpec {
		t.Fatalf("-spec = %q, want %q", gotSpec, wantSpec)
	}
	content, err := os.ReadFile(gotSpec)
	if err != nil {
		t.Fatal(err)
	}
	got := string(content)
	if !strings.Contains(got, requestdriver.BuildSpecCriteriaHeading) {
		t.Errorf("build spec = %q, want the %q heading", got, requestdriver.BuildSpecCriteriaHeading)
	}
	if !strings.Contains(got, "longest_streak") {
		t.Errorf("build spec = %q, want criterion 8's exact field name longest_streak", got)
	}
	if !strings.Contains(got, "best_weekday_done_count") || !strings.Contains(got, "done_count") {
		t.Errorf("build spec = %q, want criterion 9's exact field names", got)
	}
	if strings.Contains(got, "Fourth thing works") {
		t.Errorf("build spec = %q, want only covered criteria (8, 9), not every criterion", got)
	}
	if !strings.Contains(got, requestdriver.BuildSpecCriteriaFooter) {
		t.Errorf("build spec = %q, want the requirements-not-suggestions footer", got)
	}
	// The ticket's own header (Verify-Command:) must still be the very
	// first line, parseable exactly the way ticketspec.ParseVerifyCommand
	// already parses ticket.SpecPath itself -- the appended section goes
	// at the end, never disturbing the top.
	if !strings.HasPrefix(got, "Verify-Command: make verify\n") {
		t.Errorf("build spec = %q, want Verify-Command: at the very top", got)
	}
}

// TestTicketBuildSpecContentListsAllCriteriaWhenNoneDeclared covers the
// fallback half of the same fix: a ticket that declares no covered
// criteria at all (the legacy/hand-written format, no "## Plan" section)
// gets every approved-spec criterion listed in its build spec instead of
// none -- safer than handing the builder no contract text at all.
func TestTicketBuildSpecContentListsAllCriteriaWhenNoneDeclared(t *testing.T) {
	dataDir := t.TempDir()
	r := &request.Request{ID: "req-1", Workspace: "/repos/app", Project: "app"}
	if err := os.MkdirAll(filepath.Join(request.Dir(dataDir, r.ID), "tickets"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(requestdriver.RequestSpecPath(dataDir, r.ID), []byte(twoCriteriaSpec), 0o600); err != nil {
		t.Fatal(err)
	}
	ticketPath := filepath.Join(request.Dir(dataDir, r.ID), "tickets", "001.spec.md")
	legacyTicket := "Verify-Command: make verify\n\n## Goal\n\ndo the thing\n"
	if err := os.WriteFile(ticketPath, []byte(legacyTicket), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := requestdriver.TicketBuildSpecContent(dataDir, r.ID, request.Ticket{Index: 1, SpecPath: ticketPath})
	if err != nil {
		t.Fatalf("ticketBuildSpecContent: %v", err)
	}
	if !strings.Contains(got, "A retried POST /refunds with the same idempotency key returns the original result.") {
		t.Errorf("build spec = %q, want criterion 1's text", got)
	}
	if !strings.Contains(got, "A non-idempotent POST /refunds still processes normally.") {
		t.Errorf("build spec = %q, want criterion 2's text", got)
	}
}

// TestBuildRequestBuildArgsBuildSpecDoesNotDisturbApprovedHashes proves
// VerifyApprovedHashes -- which hashes exactly the relative paths
// recorded at plan_review approval time (tickets/NNN.spec.md, never any
// derived file) -- still passes once a ticket's build has produced its
// own <NNN>.build.md sibling: the derived file is written beside the
// approved one, never in place of it, and is never itself hash-recorded.
func TestBuildRequestBuildArgsBuildSpecDoesNotDisturbApprovedHashes(t *testing.T) {
	dataDir := t.TempDir()
	r := &request.Request{ID: "req-1", Workspace: "/repos/app", Project: "app"}
	if err := os.MkdirAll(filepath.Join(request.Dir(dataDir, r.ID), "tickets"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(requestdriver.RequestSpecPath(dataDir, r.ID), []byte(twoCriteriaSpec), 0o600); err != nil {
		t.Fatal(err)
	}
	ticketPath := filepath.Join(request.Dir(dataDir, r.ID), "tickets", "001.spec.md")
	if err := os.WriteFile(ticketPath, []byte(validBrownfieldTicket("make verify", 1, 2)), 0o600); err != nil {
		t.Fatal(err)
	}
	specHash, err := request.HashFile(dataDir, r.ID, "spec.md")
	if err != nil {
		t.Fatal(err)
	}
	ticketHash, err := request.HashFile(dataDir, r.ID, filepath.Join("tickets", "001.spec.md"))
	if err != nil {
		t.Fatal(err)
	}
	r.ApprovedSHA256 = map[string]string{
		"spec.md":                               specHash,
		filepath.Join("tickets", "001.spec.md"): ticketHash,
	}

	if _, err := requestdriver.BuildRequestBuildArgs(dataDir, r, request.Ticket{Index: 1, SpecPath: ticketPath}, requestdriver.WorkerConfig{}); err != nil {
		t.Fatalf("buildRequestBuildArgs: %v", err)
	}
	if err := request.VerifyApprovedHashes(dataDir, r); err != nil {
		t.Errorf("VerifyApprovedHashes after a build spec was written: %v", err)
	}
}

// newApprovedOracleFixture writes a ticket spec plus a populated,
// plan_review-approved <NNN>.oracle/ directory (oracle_001.go +
// RUN_COMMAND.txt, both hash-recorded in r.ApprovedSHA256 the same way
// a real Approve call would) -- the well-formed baseline
// TestResolveTicketOracle*'s individual cases each perturb one way.
func newApprovedOracleFixture(t *testing.T, dataDir string) (*request.Request, request.Ticket) {
	t.Helper()
	r := &request.Request{ID: "req-1", Workspace: "/repos/app", Project: "app"}
	ticketPath := filepath.Join(request.Dir(dataDir, r.ID), "tickets", "001.spec.md")
	if err := os.MkdirAll(filepath.Dir(ticketPath), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ticketPath, []byte("Verify-Command: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	oracleDir := filepath.Join(request.Dir(dataDir, r.ID), "tickets", "001.oracle")
	if err := os.MkdirAll(oracleDir, 0o750); err != nil {
		t.Fatal(err)
	}
	r.ApprovedSHA256 = map[string]string{}
	for name, content := range map[string]string{
		"oracle_001.go": "package x\n",
		requestdriver.TicketOracleRunCommandFilename: "go test ./.oracle/...\n",
	} {
		path := filepath.Join(oracleDir, name)
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		relPath := filepath.Join("tickets", "001.oracle", name)
		hash, err := request.HashFile(dataDir, r.ID, relPath)
		if err != nil {
			t.Fatal(err)
		}
		r.ApprovedSHA256[relPath] = hash
	}
	return r, request.Ticket{Index: 1, SpecPath: ticketPath}
}

func TestResolveTicketOracleReturnsEmptyWhenNoOracleDirectory(t *testing.T) {
	dataDir := t.TempDir()
	r := &request.Request{ID: "req-1"}
	ticketPath := filepath.Join(request.Dir(dataDir, r.ID), "tickets", "001.spec.md")
	if err := os.MkdirAll(filepath.Dir(ticketPath), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ticketPath, []byte("Verify-Command: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	oracleDir, command, err := requestdriver.ResolveTicketOracle(dataDir, r, request.Ticket{Index: 1, SpecPath: ticketPath})
	if err != nil {
		t.Fatalf("resolveTicketOracle: %v", err)
	}
	if oracleDir != "" || command != "" {
		t.Fatalf("resolveTicketOracle = (%q, %q), want empty (no oracle directory exists)", oracleDir, command)
	}
}

func TestResolveTicketOracleReturnsDirAndCommandWhenApproved(t *testing.T) {
	dataDir := t.TempDir()
	r, ticket := newApprovedOracleFixture(t, dataDir)

	oracleDir, command, err := requestdriver.ResolveTicketOracle(dataDir, r, ticket)
	if err != nil {
		t.Fatalf("resolveTicketOracle: %v", err)
	}
	wantDir := filepath.Join(request.Dir(dataDir, r.ID), "tickets", "001.oracle")
	if oracleDir != wantDir {
		t.Errorf("oracleDir = %q, want %q", oracleDir, wantDir)
	}
	if command != "go test ./.oracle/..." {
		t.Errorf("command = %q, want %q", command, "go test ./.oracle/...")
	}
}

func TestResolveTicketOracleRefusesUnapprovedFile(t *testing.T) {
	dataDir := t.TempDir()
	r, ticket := newApprovedOracleFixture(t, dataDir)
	// A file added to the oracle directory after approval -- never
	// hash-recorded, must not be silently trusted.
	oracleDir := filepath.Join(request.Dir(dataDir, r.ID), "tickets", "001.oracle")
	if err := os.WriteFile(filepath.Join(oracleDir, "sneaked_in.go"), []byte("package x\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, _, err := requestdriver.ResolveTicketOracle(dataDir, r, ticket); err == nil {
		t.Fatal("resolveTicketOracle with an unapproved file present: want an error, got nil")
	}
}

func TestResolveTicketOracleRefusesHashMismatch(t *testing.T) {
	dataDir := t.TempDir()
	r, ticket := newApprovedOracleFixture(t, dataDir)
	oracleDir := filepath.Join(request.Dir(dataDir, r.ID), "tickets", "001.oracle")
	// Edited after approval -- the exact tamper VerifyApprovedHashes-style
	// re-verification exists to catch.
	if err := os.WriteFile(filepath.Join(oracleDir, "oracle_001.go"), []byte("package x // tampered\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, _, err := requestdriver.ResolveTicketOracle(dataDir, r, ticket)
	if err == nil {
		t.Fatal("resolveTicketOracle with a tampered file: want an error, got nil")
	}
	if !strings.Contains(err.Error(), "changed since plan_review approved") {
		t.Errorf("error = %v, want it to name the tamper", err)
	}
}

// TestResolveTicketOracleRefusesASubdirectory is the regression test for
// a real finding (found via review): the actual build-time mount
// (evidence.SnapshotTree, run_ticket.go's snapshotAndHashReferenceOracle)
// walks a ticket's own oracle directory RECURSIVELY, so silently
// skipping a subdirectory here (as an earlier version did) would let
// content that was never hash-verified reach a real build. A
// subdirectory must refuse resolution outright.
func TestResolveTicketOracleRefusesASubdirectory(t *testing.T) {
	dataDir := t.TempDir()
	r, ticket := newApprovedOracleFixture(t, dataDir)
	oracleDir := filepath.Join(request.Dir(dataDir, r.ID), "tickets", "001.oracle")
	if err := os.MkdirAll(filepath.Join(oracleDir, "nested"), 0o750); err != nil {
		t.Fatal(err)
	}

	_, _, err := requestdriver.ResolveTicketOracle(dataDir, r, ticket)
	if err == nil {
		t.Fatal("resolveTicketOracle with a subdirectory present: want an error, got nil")
	}
	if !strings.Contains(err.Error(), "subdirectory") {
		t.Errorf("error = %v, want it to name the subdirectory", err)
	}
}

func TestResolveTicketOracleRefusesMissingRunCommand(t *testing.T) {
	dataDir := t.TempDir()
	r := &request.Request{ID: "req-1"}
	ticketPath := filepath.Join(request.Dir(dataDir, r.ID), "tickets", "001.spec.md")
	if err := os.MkdirAll(filepath.Dir(ticketPath), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ticketPath, []byte("Verify-Command: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	oracleDir := filepath.Join(request.Dir(dataDir, r.ID), "tickets", "001.oracle")
	if err := os.MkdirAll(oracleDir, 0o750); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(oracleDir, "oracle_001.go")
	if err := os.WriteFile(path, []byte("package x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	relPath := filepath.Join("tickets", "001.oracle", "oracle_001.go")
	hash, err := request.HashFile(dataDir, r.ID, relPath)
	if err != nil {
		t.Fatal(err)
	}
	r.ApprovedSHA256 = map[string]string{relPath: hash}
	// RUN_COMMAND.txt deliberately never written.

	_, _, err = requestdriver.ResolveTicketOracle(dataDir, r, request.Ticket{Index: 1, SpecPath: ticketPath})
	if err == nil {
		t.Fatal("resolveTicketOracle with no RUN_COMMAND.txt: want an error, got nil")
	}
	if !strings.Contains(err.Error(), requestdriver.TicketOracleRunCommandFilename) {
		t.Errorf("error = %v, want it to name %s", err, requestdriver.TicketOracleRunCommandFilename)
	}
}

// TestBuildRequestBuildArgsForwardsReferenceOracleFlags is the
// end-to-end regression: an approved, hash-verified ticket oracle
// reaches the child factoryd <run> invocation's own reference-oracle
// flags, all four together.
func TestBuildRequestBuildArgsForwardsReferenceOracleFlags(t *testing.T) {
	dataDir := t.TempDir()
	r, ticket := newApprovedOracleFixture(t, dataDir)

	args, err := requestdriver.BuildRequestBuildArgs(dataDir, r, ticket, requestdriver.WorkerConfig{})
	if err != nil {
		t.Fatalf("buildRequestBuildArgs: %v", err)
	}
	got := map[string]string{}
	inLoopRetry := false
	for i, a := range args {
		switch a {
		case "-reference-oracle-dir", "-reference-oracle-mount-path", "-reference-oracle-command":
			got[a] = args[i+1]
		case "-reference-oracle-in-loop-retry":
			inLoopRetry = true
		}
	}
	wantDir := filepath.Join(request.Dir(dataDir, r.ID), "tickets", "001.oracle")
	if got["-reference-oracle-dir"] != wantDir {
		t.Errorf("-reference-oracle-dir = %q, want %q", got["-reference-oracle-dir"], wantDir)
	}
	if got["-reference-oracle-mount-path"] != requestdriver.TicketOracleMountPath {
		t.Errorf("-reference-oracle-mount-path = %q, want %q", got["-reference-oracle-mount-path"], requestdriver.TicketOracleMountPath)
	}
	if got["-reference-oracle-command"] != "go test ./.oracle/..." {
		t.Errorf("-reference-oracle-command = %q, want %q", got["-reference-oracle-command"], "go test ./.oracle/...")
	}
	if !inLoopRetry {
		t.Error("-reference-oracle-in-loop-retry was not set")
	}
	if containsFlag(args, "-no-commit-oracles") {
		t.Error("-no-commit-oracles forwarded although the request did not opt out")
	}
	r.NoCommitOracles = true
	args, err = requestdriver.BuildRequestBuildArgs(dataDir, r, ticket, requestdriver.WorkerConfig{})
	if err != nil {
		t.Fatalf("buildRequestBuildArgs: %v", err)
	}
	if !containsFlag(args, "-no-commit-oracles") {
		t.Error("-no-commit-oracles not forwarded for an opted-out request")
	}
}

// TestBuildRequestBuildArgsOmitsReferenceOracleFlagsWhenTicketHasNone is
// the converse: the ordinary case (no <NNN>.oracle/ directory at all)
// must not emit any reference-oracle flag, identical to a ticket built
// before this mechanism existed.
func TestBuildRequestBuildArgsOmitsReferenceOracleFlagsWhenTicketHasNone(t *testing.T) {
	dataDir := t.TempDir()
	r := &request.Request{ID: "req-1", Workspace: "/repos/app", Project: "app"}
	ticketPath := filepath.Join(request.Dir(dataDir, r.ID), "tickets", "001.spec.md")
	if err := os.MkdirAll(filepath.Dir(ticketPath), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ticketPath, []byte("Verify-Command: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	args, err := requestdriver.BuildRequestBuildArgs(dataDir, r, request.Ticket{Index: 1, SpecPath: ticketPath}, requestdriver.WorkerConfig{})
	if err != nil {
		t.Fatalf("buildRequestBuildArgs: %v", err)
	}
	for _, a := range args {
		if strings.HasPrefix(a, "-reference-oracle") {
			t.Fatalf("args = %v, want no -reference-oracle* flag for a ticket with no oracle directory", args)
		}
	}
}

// TestRemindIfDueDoesNotRevertAnApprovalMadeMeanwhile: the reminder ticker
// re-loads under the request lock, so an approval that landed between
// List and the reminder's own save is never overwritten by a stale copy
// (found by adversarial review).
func TestRemindIfDueDoesNotRevertAnApprovalMadeMeanwhile(t *testing.T) {
	dataDir := t.TempDir()
	if err := request.SaveText(dataDir, "req-1", "text"); err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 9, 11, 10, 0, 0, 0, time.UTC)
	r := request.New("req-1", "/repos/app", "app", request.Source{Kind: request.SourceText}, base)
	r.State = request.StateSpecReview
	if err := os.WriteFile(filepath.Join(request.Dir(dataDir, "req-1"), "spec.md"), []byte("# Spec\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	// Simulate the old race: a List-time snapshot says spec_review and a
	// reminder is due; the operator approves before the ticker acts.
	if _, err := request.Approve(dataDir, "req-1", "alice", base.Add(time.Minute), nil); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if err := requestdriver.RemindIfDue(dataDir, "req-1", 15*time.Minute, func() time.Time { return base.Add(time.Hour) }); err != nil {
		t.Fatalf("remindIfDue: %v", err)
	}
	loaded, err := request.Load(dataDir, "req-1")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.State != request.StatePlanning {
		t.Fatalf("State = %q, want planning (the approval must survive the reminder tick)", loaded.State)
	}
	if got := countNotificationLogLines(t, dataDir, "req-1"); got != 0 {
		t.Fatalf("notifications.log lines = %d, want 0 (nothing to remind about after approval)", got)
	}
}

// TestRetryRequestResumesReviewWhenTicketHasAPR: a request halted out of
// pr_review (rounds exhausted, push rejected) goes back to pr_review on
// retry, not to a fresh build that would open a second PR on the branch.
func TestRetryRequestResumesReviewWhenTicketHasAPR(t *testing.T) {
	dp := newTestDeps(t)
	dataDir := t.TempDir()
	if err := request.SaveText(dataDir, "req-1", "text"); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	r := request.New("req-1", "/repos/app", "app", request.Source{Kind: request.SourceText}, now)
	r.State = request.StateHalted
	r.Error = "review rounds exhausted for ticket 1/1"
	r.TicketIndex, r.TicketCount = 1, 1
	r.Tickets = []request.Ticket{{Index: 1, SpecPath: "/x/001.spec.md", RunID: "run-1", PRURL: "https://github.com/acme/w/pull/1"}}
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	handled, err := retryRequest(dp, dataDir, r, "", now)
	if !handled || err != nil {
		t.Fatalf("retryRequest: handled=%v err=%v", handled, err)
	}
	loaded, err := request.Load(dataDir, "req-1")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.State != request.StatePRReview || loaded.Error != "" {
		t.Fatalf("State = %q, Error = %q; want pr_review with the error cleared", loaded.State, loaded.Error)
	}
}

// TestRetryRequestResumesSpecDraftingWhenNoTicketExists: a request halted
// during spec drafting goes back to spec_drafting on retry.
func TestRetryRequestResumesSpecDraftingWhenNoTicketExists(t *testing.T) {
	dp := newTestDeps(t)
	dataDir := t.TempDir()
	if err := request.SaveText(dataDir, "req-1", "text"); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	r := request.New("req-1", "/repos/app", "app", request.Source{Kind: request.SourceText}, now)
	r.State = request.StateHalted
	r.Error = "spec drafting failed"
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	handled, err := retryRequest(dp, dataDir, r, "", now)
	if !handled || err != nil {
		t.Fatalf("retryRequest: handled=%v err=%v", handled, err)
	}
	loaded, err := request.Load(dataDir, "req-1")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.State != request.StateSpecDrafting || loaded.Error != "" {
		t.Fatalf("State = %q, Error = %q; want spec_drafting with the error cleared", loaded.State, loaded.Error)
	}
}

// A request halted at the very start of building (TicketIndex still 0 --
// e.g. an approved-oracle hash mismatch, refused before ticket 1 is
// marked started) must be retryable: it used to fall through to the
// legacy queue-entry path and fail with "no queue entry" (found live,
// 2026-09-19).
func TestRetryRequestHaltedBeforeFirstTicketStarts(t *testing.T) {
	dp := newTestDeps(t)
	dataDir, id := buildingFixture(dp, t, 1)
	r, err := request.Load(dataDir, id)
	if err != nil {
		t.Fatal(err)
	}
	r.TicketIndex = 0
	r.State = request.StateHalted
	r.Error = "tickets/001.oracle/x_test.go has changed since the operator approved it -- re-approve before continuing"
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}

	loaded, err := request.Load(dataDir, id)
	if err != nil {
		t.Fatal(err)
	}
	handled, err := retryRequest(dp, dataDir, loaded, "", time.Now())
	if err != nil || !handled {
		t.Fatalf("retryRequest = handled %v, err %v; want handled with no error", handled, err)
	}
	retried, err := request.Load(dataDir, id)
	if err != nil {
		t.Fatal(err)
	}
	if retried.State != request.StateBuilding || retried.Error != "" {
		t.Errorf("after retry: state %q, error %q; want building with the error cleared", retried.State, retried.Error)
	}
}

// A request parked in pr_review (a human-wait state) must not starve newer
// requests: driveRequests used to advance only the oldest active request,
// so one request waiting on PR approval blocked every later submission
// (found live, 2026-09-19).
func TestDriveRequestsDoesNotLetAPRReviewRequestStarveNewerOnes(t *testing.T) {
	dp := newTestDeps(t)
	dataDir, oldID := buildingFixture(dp, t, 1)
	old, err := request.Load(dataDir, oldID)
	if err != nil {
		t.Fatal(err)
	}
	old.State = request.StatePRReview
	old.TicketIndex = 1
	old.Tickets[0].PRURL = "https://example.invalid/pr/1"
	old.Tickets[0].PRState = "merged" // nothing left to poll: a step is a no-op
	if err := old.Save(dataDir); err != nil {
		t.Fatal(err)
	}

	newID := "req-2"
	if err := request.SaveText(dataDir, newID, "another request"); err != nil {
		t.Fatal(err)
	}
	newer := request.New(newID, t.TempDir(), "app", request.Source{Kind: request.SourceText}, time.Now().Add(time.Minute))
	if err := newer.Save(dataDir); err != nil {
		t.Fatal(err)
	}

	if err := driveRequests(dp, context.Background(), dataDir, requestdriver.WorkerConfig{}, failingSpecDraftRunner(t), failingPlanTicketsRunner(t), failingOracleDraftRunner(t), failingBuildRunner(t)); err != nil {
		t.Fatalf("driveRequests: %v", err)
	}
	got, err := request.Load(dataDir, newID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != request.StateSpecDrafting {
		t.Fatalf("newer request state = %q, want %q: it was starved behind a request awaiting PR review", got.State, request.StateSpecDrafting)
	}
}

// The auto-wired oracle mounts inside the workspace, so its directory name
// must be one Go's ./... wildcard and pytest skip (leading "." or "_"),
// or a flat-module repo's in-loop `go vet ./... && go test ./...` fails on
// the mounted oracle itself (found live on todo-service, 2026-09-19).
func TestTicketOracleMountPathIsHiddenFromWildcardsAndCollectors(t *testing.T) {
	if !strings.HasPrefix(requestdriver.TicketOracleMountPath, ".") && !strings.HasPrefix(requestdriver.TicketOracleMountPath, "_") {
		t.Fatalf("ticketOracleMountPath = %q; it must start with '.' or '_' so `go ./...` and pytest skip it", requestdriver.TicketOracleMountPath)
	}
}

// Defense in depth for the approval-time check: even an approved (hash-
// pinned) RUN_COMMAND.txt that never names the mount is refused at run
// start, so it can never be launched as a vacuous "oracle".
func TestResolveTicketOracleRefusesRunCommandThatNeverNamesTheMount(t *testing.T) {
	dataDir := t.TempDir()
	r, ticket := newApprovedOracleFixture(t, dataDir)
	path := filepath.Join(request.TicketOracleDir(ticket.SpecPath), requestdriver.TicketOracleRunCommandFilename)
	if err := os.WriteFile(path, []byte("go test ./...\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	relPath, err := filepath.Rel(request.Dir(dataDir, r.ID), path)
	if err != nil {
		t.Fatal(err)
	}
	hash, err := request.HashFile(dataDir, r.ID, relPath)
	if err != nil {
		t.Fatal(err)
	}
	r.ApprovedSHA256[relPath] = hash
	if _, _, err := requestdriver.ResolveTicketOracle(dataDir, r, ticket); err == nil || !strings.Contains(err.Error(), request.TicketOracleMountPath) {
		t.Fatalf("resolveTicketOracle with a repo-wide RUN_COMMAND: error = %v, want a refusal naming %q", err, request.TicketOracleMountPath)
	}
}

// --- An adversarial-review finding (2026-09-24): a cancel that lands
// while a job the driver is already running (spec/oracle/plan drafting,
// a ticket build) must not be resurrected by that job's own r.Save once
// it returns. Each test below cancels the request *from inside the stub
// runner*, mimicking cancelRequest (internal/api/server.go) or `factoryd
// cancel` taking request.Lock concurrently with driveRequests holding its
// own unlocked *request.Request from request.List -- see stillInState's
// doc comment (request_driver.go) for the mechanism these tests pin.

// cancelRequestForTest cancels id under request.Lock exactly the way
// cancelRequest (internal/api/server.go) and `factoryd cancel` do, for a
// test stub runner to call while driveRequests is still holding its own
// stale in-memory copy of the same request.
func cancelRequestForTest(t *testing.T, dataDir, id string) {
	t.Helper()
	unlock, err := request.Lock(dataDir, id)
	if err != nil {
		t.Fatalf("lock request for cancel: %v", err)
	}
	defer unlock()
	r, err := request.Load(dataDir, id)
	if err != nil {
		t.Fatalf("load request for cancel: %v", err)
	}
	if err := r.Cancel("operator", "changed my mind", time.Now()); err != nil {
		t.Fatalf("cancel request: %v", err)
	}
	if err := r.Save(dataDir); err != nil {
		t.Fatalf("save cancelled request: %v", err)
	}
}

// TestAdvanceSpecDraftingCancelledDuringJobDoesNotResurrectRequest covers
// the same cancel-during-job finding above, for the spec-drafting job.
func TestAdvanceSpecDraftingCancelledDuringJobDoesNotResurrectRequest(t *testing.T) {
	dp := newTestDeps(t)
	dataDir := t.TempDir()
	if err := request.SaveText(dataDir, "req-1", "text"); err != nil {
		t.Fatal(err)
	}
	r := request.New("req-1", "/repos/app", "app", request.Source{Kind: request.SourceText}, time.Now())
	r.State = request.StateSpecDrafting
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}

	runner := func(ctx context.Context, dataDir string, r *request.Request, cfg requestdriver.WorkerConfig) (string, *request.SpecEvidence, error) {
		cancelRequestForTest(t, dataDir, "req-1")
		return canonicalValidSpec, &request.SpecEvidence{AgentExitCode: 0}, nil
	}
	if err := driveRequests(dp, context.Background(), dataDir, requestdriver.WorkerConfig{}, runner, failingPlanTicketsRunner(t), failingOracleDraftRunner(t), failingBuildRunner(t)); err != nil {
		t.Fatalf("driveRequests: %v", err)
	}

	loaded, err := request.Load(dataDir, "req-1")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.State != request.StateCancelled {
		t.Fatalf("State = %q, want %q (a cancel mid-job must survive the job's own save)", loaded.State, request.StateCancelled)
	}
}

// TestAdvanceOracleDraftingCancelledDuringJobDoesNotResurrectRequest covers
// the same cancel-during-job finding above, for the oracle-drafting job.
func TestAdvanceOracleDraftingCancelledDuringJobDoesNotResurrectRequest(t *testing.T) {
	dp := newTestDeps(t)
	dataDir, id := oracleStageFixture(t, true)
	if got := loadRequest(t, dataDir, id); got.State != request.StateOracleDrafting {
		t.Fatalf("State after spec approval = %q, want oracle_drafting", got.State)
	}

	runner := func(ctx context.Context, in requestdriver.OracleDraftInput) (request.OracleDraft, error) {
		cancelRequestForTest(t, dataDir, id)
		return request.OracleDraft{Status: request.OracleNotImplemented}, nil
	}
	if err := driveRequests(dp, context.Background(), dataDir, noOracleScriptCfg, failingSpecDraftRunner(t), failingPlanTicketsRunner(t), runner, failingBuildRunner(t)); err != nil {
		t.Fatal(err)
	}

	loaded := loadRequest(t, dataDir, id)
	if loaded.State != request.StateCancelled {
		t.Fatalf("State = %q, want %q (a cancel mid-job must survive the job's own save)", loaded.State, request.StateCancelled)
	}
}

// TestAdvancePlanningCancelledDuringJobDoesNotResurrectRequest covers
// the same cancel-during-job finding above, for the plan-drafting job.
func TestAdvancePlanningCancelledDuringJobDoesNotResurrectRequest(t *testing.T) {
	dp := newTestDeps(t)
	dataDir, id := approvedPlanningFixture(t, twoCriteriaSpec, "make verify")
	if got := loadRequest(t, dataDir, id); got.State != request.StatePlanning {
		t.Fatalf("State after spec approval = %q, want planning", got.State)
	}

	runner := func(ctx context.Context, dataDir string, r *request.Request, cfg requestdriver.WorkerConfig, verifyCommand string) ([]requestdriver.DraftedTicket, *request.PlanEvidence, error) {
		cancelRequestForTest(t, dataDir, id)
		return []requestdriver.DraftedTicket{{Filename: "001.spec.md", Content: validBrownfieldTicket("make verify", 1, 2)}}, &request.PlanEvidence{}, nil
	}
	if err := driveRequests(dp, context.Background(), dataDir, requestdriver.WorkerConfig{}, failingSpecDraftRunner(t), runner, failingOracleDraftRunner(t), failingBuildRunner(t)); err != nil {
		t.Fatal(err)
	}

	loaded := loadRequest(t, dataDir, id)
	if loaded.State != request.StateCancelled {
		t.Fatalf("State = %q, want %q (a cancel mid-job must survive the job's own save)", loaded.State, request.StateCancelled)
	}
}

// TestAdvanceBuildingCancelledDuringJobDoesNotResurrectRequest covers
// the same cancel-during-job finding above, for a ticket build.
func TestAdvanceBuildingCancelledDuringJobDoesNotResurrectRequest(t *testing.T) {
	dp := newTestDeps(t)
	dataDir, id := buildingFixture(dp, t, 1)

	runner := func(ctx context.Context, args []string, onReady func(*run.Run)) error {
		ticket := argValue(args, "-ticket")
		if onReady != nil {
			onReady(&run.Run{ID: ticket, State: run.StateReady})
		}
		rr := &run.Run{ID: ticket, State: run.StateAccepted, PullRequestURL: "https://github.com/acme/app/pull/" + ticket}
		if err := rr.Save(dataDir); err != nil {
			t.Fatalf("save stub run %q: %v", ticket, err)
		}
		cancelRequestForTest(t, dataDir, id)
		return nil
	}
	if err := driveRequests(dp, context.Background(), dataDir, requestdriver.WorkerConfig{}, failingSpecDraftRunner(t), failingPlanTicketsRunner(t), failingOracleDraftRunner(t), runner); err != nil {
		t.Fatal(err)
	}

	loaded := loadRequest(t, dataDir, id)
	if loaded.State != request.StateCancelled {
		t.Fatalf("State = %q, want %q (a cancel mid-job must survive the job's own save)", loaded.State, request.StateCancelled)
	}
}

// TestAdvanceBuildingAcceptedWithoutPRRecordsNoPRState: an accepted run
// whose PR was never opened must not leave the ticket claiming a "draft"
// PR -- the console rendered that as a "PR draft" chip next to "accepted ·
// awaiting PR" (console walk, 2026-09-24).
// TestAdvanceBuildingStampsCompletionAfterRunReturns is the building-stage
// counterpart: found live, a request's History read "building -> pr_review
// | ticket 1/1 accepted" one second after "plan_review -> building", before
// the ticket's run had even started -- the accepted transition was stamped
// with the poll's own now, captured before the multi-minute build ran.
func TestAdvanceBuildingStampsCompletionAfterRunReturns(t *testing.T) {
	dp := newTestDeps(t)
	dataDir, id := buildingFixture(dp, t, 1)
	const jobDuration = 50 * time.Millisecond
	before := time.Now()
	runner := func(ctx context.Context, args []string, onReady func(*run.Run)) error {
		ticket := argValue(args, "-ticket")
		if onReady != nil {
			onReady(&run.Run{ID: ticket, State: run.StateReady})
		}
		time.Sleep(jobDuration)
		rr := &run.Run{ID: ticket, State: run.StateAccepted}
		if err := rr.Save(dataDir); err != nil {
			t.Fatalf("save stub run %q: %v", ticket, err)
		}
		return nil
	}
	if err := driveRequests(dp, context.Background(), dataDir, requestdriver.WorkerConfig{}, failingSpecDraftRunner(t), failingPlanTicketsRunner(t), failingOracleDraftRunner(t), runner); err != nil {
		t.Fatalf("driveRequests: %v", err)
	}
	loaded, err := request.Load(dataDir, id)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.State != request.StatePRReview {
		t.Fatalf("State = %q, want %q", loaded.State, request.StatePRReview)
	}
	last := loaded.History[len(loaded.History)-1]
	at, err := time.Parse(time.RFC3339Nano, last.At)
	if err != nil {
		t.Fatalf("parse History At %q: %v", last.At, err)
	}
	if at.Before(before.Add(jobDuration)) {
		t.Errorf("%s -> %s At = %s, want it stamped after the %s run returned (started at %s)", last.From, last.To, at, jobDuration, before)
	}
}

func TestAdvanceBuildingAcceptedWithoutPRRecordsNoPRState(t *testing.T) {
	dp := newTestDeps(t)
	dataDir, id := buildingFixture(dp, t, 1)
	runner := func(ctx context.Context, args []string, onReady func(*run.Run)) error {
		ticket := argValue(args, "-ticket")
		if onReady != nil {
			onReady(&run.Run{ID: ticket, State: run.StateReady})
		}
		rr := &run.Run{ID: ticket, State: run.StateAccepted}
		if err := rr.Save(dataDir); err != nil {
			t.Fatalf("save stub run %q: %v", ticket, err)
		}
		return nil
	}
	if err := driveRequests(dp, context.Background(), dataDir, requestdriver.WorkerConfig{}, failingSpecDraftRunner(t), failingPlanTicketsRunner(t), failingOracleDraftRunner(t), runner); err != nil {
		t.Fatalf("driveRequests: %v", err)
	}
	loaded, err := request.Load(dataDir, id)
	if err != nil {
		t.Fatal(err)
	}
	if tk := loaded.Tickets[0]; tk.PRURL != "" || tk.PRState != "" {
		t.Errorf("Tickets[0] = %+v, want no PR URL and no PR state", tk)
	}
}

// A plan-feedback.md left from an earlier planning pass is removed when
// the request now has no plan feedback -- e.g. after a send-back to spec,
// which empties PlanFeedback on purpose. runPlanTicketsJob reads the file
// straight from disk, so leaving it would feed notes written against the
// old spec to the new plan drafter (adversarial review of SendBack, round
// 2, 2026-09-26).
func TestAdvancePlanningRemovesStalePlanFeedbackFile(t *testing.T) {
	dp := newTestDeps(t)
	dataDir, id := approvedPlanningFixtureNoFactoryYML(t, twoCriteriaSpec, "python3 -m unittest tests/test_product_lab.py")
	stale := request.PlanFeedbackPath(dataDir, id)
	if err := os.WriteFile(stale, []byte("## Plan rejected\n\nsplit ticket 2 (old spec)\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tickets := []requestdriver.DraftedTicket{
		{Filename: "001.spec.md", Content: validBrownfieldTicket("python3 -m unittest tests/test_product_lab.py", 1, 2)},
	}
	runner, _ := stubPlanTicketsRunner(tickets, &request.PlanEvidence{}, nil)

	if err := driveRequests(dp, context.Background(), dataDir, requestdriver.WorkerConfig{}, failingSpecDraftRunner(t), runner, failingOracleDraftRunner(t), failingBuildRunner(t)); err != nil {
		t.Fatalf("driveRequests: %v", err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("stale plan-feedback.md still present (err=%v), want removed", err)
	}
}

// TestDriveRequestsPublishesTheActiveRequestDuringItsJob: the request whose
// job is in flight is published (for the worker heartbeat) while the job
// runs and cleared once it returns.
func TestDriveRequestsPublishesTheActiveRequestDuringItsJob(t *testing.T) {
	dp := newTestDeps(t)
	dataDir := t.TempDir()
	if err := request.SaveText(dataDir, "req-1", "text"); err != nil {
		t.Fatal(err)
	}
	r := request.New("req-1", "/repos/app", "app", request.Source{Kind: request.SourceText}, time.Now())
	r.State = request.StateSpecDrafting
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	var during []string
	spec := func(ctx context.Context, dataDir string, r *request.Request, cfg requestdriver.WorkerConfig) (string, *request.SpecEvidence, error) {
		during = currentActiveRequests()
		return "", nil, errors.New("stop here")
	}
	_ = driveRequests(dp, context.Background(), dataDir, requestdriver.WorkerConfig{}, spec, failingPlanTicketsRunner(t), failingOracleDraftRunner(t), failingBuildRunner(t))
	if len(during) != 1 || during[0] != "req-1" {
		t.Errorf("active requests during the job = %v, want [req-1]", during)
	}
	if got := currentActiveRequests(); len(got) != 0 {
		t.Errorf("active requests after the job = %v, want none", got)
	}
}

// TestSharedCriterionReviewedOnlyAtLastCoveringTicket: a criterion listed
// under several tickets is reviewed only at the last one (all its parts
// exist by then); earlier tickets' build specs still carry its text,
// marked as shared. Found live (Flutter + Go app habit-insights request,
// 2026-09-28): ticket 1 of 3 was reviewed against handler and MCP-tool
// criteria that tickets 2 and 3 build, and quarantined.
func TestSharedCriterionReviewedOnlyAtLastCoveringTicket(t *testing.T) {
	dataDir := t.TempDir()
	r := &request.Request{ID: "req-1", Workspace: "/repos/app", Project: "app"}
	ticketsDir := filepath.Join(request.Dir(dataDir, r.ID), "tickets")
	if err := os.MkdirAll(ticketsDir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(requestdriver.RequestSpecPath(dataDir, r.ID), []byte(habitInsightsSpec), 0o600); err != nil {
		t.Fatal(err)
	}
	t1 := filepath.Join(ticketsDir, "001.spec.md")
	t2 := filepath.Join(ticketsDir, "002.spec.md")
	if err := os.WriteFile(t1, []byte(validBrownfieldTicket("make verify", 1, 8)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(t2, []byte(validBrownfieldTicket("make verify", 8, 9)), 0o600); err != nil {
		t.Fatal(err)
	}
	r.Tickets = []request.Ticket{{Index: 1, SpecPath: t1}, {Index: 2, SpecPath: t2}}
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}

	crit1, err := requestdriver.WriteTicketCriteriaFile(dataDir, r, r.Tickets[0])
	if err != nil {
		t.Fatal(err)
	}
	got1, _ := os.ReadFile(crit1)
	if strings.Contains(string(got1), "longest_streak") || !strings.HasPrefix(string(got1), "1.") {
		t.Errorf("ticket 1 criteria = %q, want criterion 1 only (8 is shared with ticket 2, reviewed there)", got1)
	}
	crit2, err := requestdriver.WriteTicketCriteriaFile(dataDir, r, r.Tickets[1])
	if err != nil {
		t.Fatal(err)
	}
	got2, _ := os.ReadFile(crit2)
	if !strings.Contains(string(got2), "longest_streak") || !strings.Contains(string(got2), "best_weekday_done_count") {
		t.Errorf("ticket 2 criteria = %q, want criteria 8 and 9", got2)
	}

	spec1, err := requestdriver.TicketBuildSpecContent(dataDir, r.ID, r.Tickets[0])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(spec1, "longest_streak") || !strings.Contains(spec1, "Shared with ticket 2") {
		t.Errorf("ticket 1 build spec must carry criterion 8's text marked shared with ticket 2; got %q", spec1)
	}
}

// readFeedbackRunner is a plan-tickets runner that records the
// plan-feedback.md the driver wrote before launching it, then returns tickets.
func readFeedbackRunner(dataDir string, tickets []requestdriver.DraftedTicket, seen *string) requestdriver.PlanTicketsRunner {
	return func(ctx context.Context, dd string, r *request.Request, cfg requestdriver.WorkerConfig, verifyCommand string) ([]requestdriver.DraftedTicket, *request.PlanEvidence, error) {
		b, _ := os.ReadFile(request.PlanFeedbackPath(dataDir, r.ID))
		*seen = string(b)
		return tickets, &request.PlanEvidence{}, nil
	}
}

// readSpecFeedbackRunner is readFeedbackRunner for the spec drafter.
func readSpecFeedbackRunner(dataDir string, seen *string) requestdriver.SpecDraftRunner {
	return func(ctx context.Context, dd string, r *request.Request, cfg requestdriver.WorkerConfig) (string, *request.SpecEvidence, error) {
		b, _ := os.ReadFile(request.SpecFeedbackPath(dataDir, r.ID))
		*seen = string(b)
		return canonicalValidSpec, &request.SpecEvidence{}, nil
	}
}

func savedSpecDraftingRequest(t *testing.T) (dataDir string) {
	t.Helper()
	dataDir = t.TempDir()
	if err := request.SaveText(dataDir, "req-1", "text"); err != nil {
		t.Fatal(err)
	}
	r := request.New("req-1", "/repos/app", "app", request.Source{Kind: request.SourceText}, time.Now())
	r.State = request.StateSpecDrafting
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	return dataDir
}

func loadHalted(t *testing.T, dataDir, id string) *request.Request {
	t.Helper()
	loaded, err := request.Load(dataDir, id)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.State != request.StateHalted {
		t.Fatalf("State = %q, want %q (Error: %s)", loaded.State, request.StateHalted, loaded.Error)
	}
	return loaded
}

const malformedSpec = "# Spec\n\n## Problem\n\nx\n" // no ## Scope heading

// haltOnMalformedSpec drives a fresh spec_drafting request to a halt on malformedSpec.
func haltOnMalformedSpec(t *testing.T, dp *deps, dataDir string) *request.Request {
	t.Helper()
	runner, _ := stubSpecDraftRunner(malformedSpec, &request.SpecEvidence{}, nil)
	if err := driveRequests(dp, context.Background(), dataDir, requestdriver.WorkerConfig{}, runner, failingPlanTicketsRunner(t), failingOracleDraftRunner(t), failingBuildRunner(t)); err != nil {
		t.Fatalf("driveRequests: %v", err)
	}
	return loadHalted(t, dataDir, "req-1")
}

func TestPlanningHaltReasonReachesTheRetriedPlanner(t *testing.T) {
	dp := newTestDeps(t)
	dataDir, id := approvedPlanningFixture(t, twoCriteriaSpec, "make verify")
	bad := []requestdriver.DraftedTicket{{Filename: "001.spec.md", Content: validBrownfieldTicket("make wrong-command-xyz", 1, 2)}}
	badRunner, _ := stubPlanTicketsRunner(bad, &request.PlanEvidence{}, nil)
	if err := driveRequests(dp, context.Background(), dataDir, requestdriver.WorkerConfig{}, failingSpecDraftRunner(t), badRunner, failingOracleDraftRunner(t), failingBuildRunner(t)); err != nil {
		t.Fatalf("driveRequests: %v", err)
	}
	halted := loadHalted(t, dataDir, id)
	rejected := len(halted.Rejections)
	if _, err := request.Retry(dataDir, id, "alice", "", time.Now(), nil); err != nil {
		t.Fatalf("Retry: %v", err)
	}

	var seen string
	good := []requestdriver.DraftedTicket{{Filename: "001.spec.md", Content: validBrownfieldTicket("make verify", 1, 2)}}
	if err := driveRequests(dp, context.Background(), dataDir, requestdriver.WorkerConfig{}, failingSpecDraftRunner(t), readFeedbackRunner(dataDir, good, &seen), failingOracleDraftRunner(t), failingBuildRunner(t)); err != nil {
		t.Fatalf("driveRequests after retry: %v", err)
	}
	if !strings.HasPrefix(seen, "## Previous draft refused by the factory (") || !strings.Contains(seen, "make wrong-command-xyz") {
		t.Errorf("plan-feedback.md = %q, want the refused-draft section naming make wrong-command-xyz", seen)
	}
	after, err := request.Load(dataDir, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Rejections) != rejected || after.DraftHalt != nil {
		t.Errorf("Rejections = %d (was %d), DraftHalt = %+v; want rejections unchanged and the note cleared at plan_review", len(after.Rejections), rejected, after.DraftHalt)
	}
	if after.State != request.StatePlanReview {
		t.Errorf("State = %q, want plan_review", after.State)
	}
}

func TestSpecDraftingHaltReasonReachesTheRetriedDrafter(t *testing.T) {
	dp := newTestDeps(t)
	dataDir := savedSpecDraftingRequest(t)
	halted := haltOnMalformedSpec(t, dp, dataDir)
	if len(halted.Rejections) != 0 {
		t.Fatalf("Rejections = %+v, want none: a refused draft is not a rejection", halted.Rejections)
	}
	if _, err := request.Retry(dataDir, "req-1", "alice", "", time.Now(), nil); err != nil {
		t.Fatalf("Retry: %v", err)
	}
	var seen string
	if err := driveRequests(dp, context.Background(), dataDir, requestdriver.WorkerConfig{}, readSpecFeedbackRunner(dataDir, &seen), failingPlanTicketsRunner(t), failingOracleDraftRunner(t), failingBuildRunner(t)); err != nil {
		t.Fatalf("driveRequests after retry: %v", err)
	}
	if !strings.HasPrefix(seen, "## Previous draft refused by the factory (") || !strings.Contains(seen, "## Scope") {
		t.Errorf("spec-feedback.md = %q, want the refused-draft section naming the missing ## Scope heading", seen)
	}
	after, err := request.Load(dataDir, "req-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Rejections) != 0 || after.DraftHalt != nil {
		t.Errorf("Rejections = %+v, DraftHalt = %+v; want none and the note cleared at spec_review", after.Rejections, after.DraftHalt)
	}
}

func TestDraftJobFailureLeavesNoDraftNote(t *testing.T) {
	dp := newTestDeps(t)
	for _, text := range []string{"agent exited 2: 401 Unauthorized", "model route error: Connection error.", "draft_spec.py exited 3 (see /some/log)"} {
		dataDir := savedSpecDraftingRequest(t)
		runner, _ := stubSpecDraftRunner("", nil, errors.New(text))
		if err := driveRequests(dp, context.Background(), dataDir, requestdriver.WorkerConfig{}, runner, failingPlanTicketsRunner(t), failingOracleDraftRunner(t), failingBuildRunner(t)); err != nil {
			t.Fatalf("driveRequests: %v", err)
		}
		if halted := loadHalted(t, dataDir, "req-1"); halted.DraftHalt != nil || len(halted.Rejections) != 0 {
			t.Errorf("%q: DraftHalt = %+v, Rejections = %+v; want neither", text, halted.DraftHalt, halted.Rejections)
		}
	}
	dataDir, id := approvedPlanningFixture(t, twoCriteriaSpec, "make verify")
	planRunner, _ := stubPlanTicketsRunner(nil, nil, errors.New("agent exited 2: 401 Unauthorized"))
	if err := driveRequests(dp, context.Background(), dataDir, requestdriver.WorkerConfig{}, failingSpecDraftRunner(t), planRunner, failingOracleDraftRunner(t), failingBuildRunner(t)); err != nil {
		t.Fatalf("driveRequests: %v", err)
	}
	if halted := loadHalted(t, dataDir, id); halted.DraftHalt != nil {
		t.Errorf("plan job failure: DraftHalt = %+v, want nil", halted.DraftHalt)
	}
}

func TestTicketWriteErrorLeavesNoDraftNote(t *testing.T) {
	dp := newTestDeps(t)
	dataDir, id := approvedPlanningFixture(t, twoCriteriaSpec, "make verify")
	// The drafted ticket's name is a directory path that cannot exist, so
	// writing it fails with an I/O error, not a content refusal.
	bad := []requestdriver.DraftedTicket{{Filename: "no-such-dir/001.spec.md", Content: validBrownfieldTicket("make verify", 1, 2)}}
	runner, _ := stubPlanTicketsRunner(bad, &request.PlanEvidence{}, nil)
	if err := driveRequests(dp, context.Background(), dataDir, requestdriver.WorkerConfig{}, failingSpecDraftRunner(t), runner, failingOracleDraftRunner(t), failingBuildRunner(t)); err != nil {
		t.Fatalf("driveRequests: %v", err)
	}
	halted := loadHalted(t, dataDir, id)
	if !strings.Contains(halted.Error, "write ticket") {
		t.Fatalf("Error = %q, want the write failure", halted.Error)
	}
	if halted.DraftHalt != nil {
		t.Errorf("DraftHalt = %+v, want none for an I/O error", halted.DraftHalt)
	}
}

func TestInfrastructureHaltLeavesNoDraftNote(t *testing.T) {
	dp := newTestDeps(t)
	cases := []struct {
		name  string
		setup func(t *testing.T) (dataDir, id string)
	}{
		{"stale approval hash", func(t *testing.T) (string, string) {
			dataDir, id := approvedPlanningFixture(t, twoCriteriaSpec, "make verify")
			if err := os.WriteFile(requestdriver.RequestSpecPath(dataDir, id), []byte(twoCriteriaSpec+"\nedited after approval\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			return dataDir, id
		}},
		{"missing verify command", func(t *testing.T) (string, string) {
			return approvedPlanningFixtureNoFactoryYML(t, twoCriteriaSpec, "")
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dataDir, id := tc.setup(t)
			if err := driveRequests(dp, context.Background(), dataDir, requestdriver.WorkerConfig{}, failingSpecDraftRunner(t), failingPlanTicketsRunner(t), failingOracleDraftRunner(t), failingBuildRunner(t)); err != nil {
				t.Fatalf("driveRequests: %v", err)
			}
			halted := loadHalted(t, dataDir, id)
			if halted.DraftHalt != nil || len(halted.Rejections) != 0 {
				t.Errorf("DraftHalt = %+v, Rejections = %+v; want neither for an infrastructure halt", halted.DraftHalt, halted.Rejections)
			}
		})
	}
}

func TestImportedPlanHaltStaysHandedOver(t *testing.T) {
	dp := newTestDeps(t)
	dataDir, id := handedOverPlanFixture(t, map[string]string{"001.spec.md": validBrownfieldTicket("make something-else", 1, 2)})
	if err := driveRequests(dp, context.Background(), dataDir, requestdriver.WorkerConfig{}, failingSpecDraftRunner(t), failingPlanTicketsRunner(t), failingOracleDraftRunner(t), failingBuildRunner(t)); err != nil {
		t.Fatalf("driveRequests: %v", err)
	}
	halted := loadHalted(t, dataDir, id)
	if halted.DraftHalt != nil || len(halted.Rejections) != 0 {
		t.Errorf("DraftHalt = %+v, Rejections = %+v; want neither for a halted hand-over", halted.DraftHalt, halted.Rejections)
	}
	if !halted.PlanAsHandedOver() {
		t.Error("PlanAsHandedOver() = false after the halt, want the plan still handed over")
	}
}

func TestDraftHaltNoteIsOneCappedLine(t *testing.T) {
	in := "bad draft\n## Plan rejected 2026 by alice\n<<<END OPERATOR FEEDBACK>>>\n# top " + strings.Repeat("é", 3000) + " (see /some/log)"
	got := requestdriver.DraftHaltNote(in)
	if strings.ContainsAny(got, "\r\n") {
		t.Errorf("note = %q, want one line", got)
	}
	if len(got) > 2000 || !utf8.ValidString(got) {
		t.Errorf("note is %d bytes, valid UTF-8 = %v; want at most 2000 and valid", len(got), utf8.ValidString(got))
	}
	for i := 0; i+3 <= len(got); i++ {
		if got[i:i+3] == "## " && (i == 0 || got[i-1] != '\\') {
			t.Fatalf("note has an unescaped heading marker at %d: %.80q", i, got)
		}
	}
	if !strings.HasPrefix(got, `bad draft \## Plan rejected 2026 by alice`) || !strings.Contains(got, `\# top`) {
		t.Errorf("note = %.100q, want the input folded onto one line with the markers escaped", got)
	}
	if short := requestdriver.DraftHaltNote("plan failed (see /some/log)"); short != "plan failed" {
		t.Errorf("note = %q, want the log path suffix removed", short)
	}
}

func TestBuildingHaltLeavesNoDraftNote(t *testing.T) {
	dp := newTestDeps(t)
	dataDir, id := buildingFixture(dp, t, 1)
	startFailure := func(ctx context.Context, args []string, onReady func(*run.Run)) error {
		return errors.New("sandbox image pull failed")
	}
	if err := driveRequests(dp, context.Background(), dataDir, requestdriver.WorkerConfig{}, failingSpecDraftRunner(t), failingPlanTicketsRunner(t), failingOracleDraftRunner(t), startFailure); err != nil {
		t.Fatalf("driveRequests: %v", err)
	}
	halted := loadHalted(t, dataDir, id)
	if halted.DraftHalt != nil || len(halted.Rejections) != 0 {
		t.Errorf("DraftHalt = %+v, Rejections = %+v; want neither for a building halt", halted.DraftHalt, halted.Rejections)
	}
}

func TestDraftHaltNoteReplacesTheEarlierOne(t *testing.T) {
	dp := newTestDeps(t)
	dataDir := savedSpecDraftingRequest(t)
	haltOnMalformedSpec(t, dp, dataDir)
	if _, err := request.Retry(dataDir, "req-1", "alice", "", time.Now(), nil); err != nil {
		t.Fatalf("Retry: %v", err)
	}
	second := "# Spec\n\n## Problem\n\ny\n\n## Scope\n\nz\n" // stops before ## Non-goals
	runner, _ := stubSpecDraftRunner(second, &request.SpecEvidence{}, nil)
	if err := driveRequests(dp, context.Background(), dataDir, requestdriver.WorkerConfig{}, runner, failingPlanTicketsRunner(t), failingOracleDraftRunner(t), failingBuildRunner(t)); err != nil {
		t.Fatalf("driveRequests: %v", err)
	}
	halted := loadHalted(t, dataDir, "req-1")
	if halted.DraftHalt == nil || !strings.Contains(halted.DraftHalt.Reason, `heading "\## Non-goals"`) || strings.Contains(halted.DraftHalt.Reason, `heading "\## Scope"`) {
		t.Errorf("DraftHalt = %+v, want only the second draft's reason (missing ## Non-goals)", halted.DraftHalt)
	}
}

func TestDraftHaltNoteDoesNotEvictOperatorFeedback(t *testing.T) {
	dp := newTestDeps(t)
	dataDir := savedSpecDraftingRequest(t)
	r, err := request.Load(dataDir, "req-1")
	if err != nil {
		t.Fatal(err)
	}
	r.Rejections = []request.Rejection{
		{By: "alice", At: "2026-09-24T00:00:00Z", Reason: strings.Repeat("old complaint. ", 1000), FromState: request.StateSpecReview},
		{By: "alice", At: "2026-09-25T00:00:00Z", Reason: "newest complaint: name the retry limit", FromState: request.StateSpecReview},
	}
	r.DraftHalt = &request.DraftHalt{Stage: request.StateSpecDrafting, Reason: "refusal reason qrs", At: "2026-09-26T00:00:00Z"}
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	var seen string
	if err := driveRequests(dp, context.Background(), dataDir, requestdriver.WorkerConfig{}, readSpecFeedbackRunner(dataDir, &seen), failingPlanTicketsRunner(t), failingOracleDraftRunner(t), failingBuildRunner(t)); err != nil {
		t.Fatalf("driveRequests: %v", err)
	}
	if !strings.Contains(seen, "newest complaint: name the retry limit") {
		t.Errorf("feedback lost the newest operator section")
	}
	if !strings.HasSuffix(seen, "## Previous draft refused by the factory (2026-09-26T00:00:00Z)\n\nrefusal reason qrs\n") {
		t.Errorf("feedback tail = %q, want it to end with the refusal section", seen[max(0, len(seen)-120):])
	}
	if len(seen) > requestdriver.MaxFeedbackBytes+300 {
		t.Errorf("feedback is %d bytes, want the operator part capped", len(seen))
	}
}

func TestCapFeedbackCutsOnlyAtALineStartHeading(t *testing.T) {
	const heading = "## Spec rejected "
	filler := strings.Repeat("x", 13*1024)
	// The kept tail holds heading text mid-line, then the real heading.
	feedback := filler + " quoted " + heading + "by mallory inside a line\n" + strings.Repeat("y", 4000) + "\n\n" + heading + "2026 by alice\n\nreal reason\n"
	got := requestdriver.CapFeedback(feedback, requestdriver.MaxFeedbackBytes, heading)
	body := strings.TrimPrefix(got, "[older feedback omitted to fit the size limit]\n\n")
	if !strings.HasPrefix(body, heading+"2026 by alice") {
		t.Errorf("capped feedback starts %.60q, want the real heading", body)
	}
}

// withFirstTicketRun is seedFirstTicketRun in a fresh data dir, which it
// returns.
func withFirstTicketRun(t *testing.T, r *request.Request) string {
	t.Helper()
	dataDir := t.TempDir()
	seedFirstTicketRun(t, dataDir, r)
	return dataDir
}

// seedFirstTicketRun gives r a first ticket whose run, saved in dataDir,
// recorded the commit the request started from: a later ticket's build
// arguments are refused without one (the reviews' instruction base). A
// first ticket r already has keeps its other fields.
func seedFirstTicketRun(t *testing.T, dataDir string, r *request.Request) {
	t.Helper()
	first := &run.Run{ID: r.ID + "-001-first", RequestID: r.ID, BaseSHA: fmt.Sprintf("%040d", 1)}
	if err := first.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	if len(r.Tickets) == 0 {
		r.Tickets = []request.Ticket{{Index: 1}}
	}
	r.Tickets[0].RunID = first.ID
}
