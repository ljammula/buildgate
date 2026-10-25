package requestdriver_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"buildgate/internal/request"
	"buildgate/internal/requestdriver"
	"buildgate/internal/requestdriver/requestdrivertest"
)

func TestPlanningTakesHandedOverTicketsWithoutThePlanningJob(t *testing.T) {
	dp := newFakeDeps(t)
	ticket := requestdrivertest.ValidBrownfieldTicket(requestdrivertest.HandoverVerify, 1, 2)
	dataDir, id := requestdrivertest.HandedOverPlanFixture(t, map[string]string{"001.spec.md": ticket})

	if err := requestdrivertest.DriveRequests(dp, context.Background(), dataDir, requestdriver.WorkerConfig{}, requestdrivertest.FailingSpecDraftRunner(t), requestdrivertest.FailingPlanTicketsRunner(t), requestdrivertest.FailingOracleDraftRunner(t), requestdrivertest.FailingBuildRunner(t)); err != nil {
		t.Fatalf("driveRequests: %v", err)
	}
	loaded, err := request.Load(dataDir, id)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.State != request.StatePlanReview || loaded.TicketCount != 1 {
		t.Fatalf("State = %q, TicketCount = %d (Error: %s)", loaded.State, loaded.TicketCount, loaded.Error)
	}
	got, err := os.ReadFile(filepath.Join(request.Dir(dataDir, id), "tickets", "001.spec.md"))
	if err != nil || string(got) != ticket {
		t.Fatalf("tickets/001.spec.md differs from the handed-over ticket (%v)", err)
	}
	if last := loaded.History[len(loaded.History)-1]; last.Reason != "plan handed over by the operator: 1 tickets" {
		t.Errorf("history reason = %q", last.Reason)
	}
	if loaded.PlanEvidence != nil {
		t.Errorf("PlanEvidence = %+v, want none: no planning job ran", loaded.PlanEvidence)
	}
}

// A handed-over plan gets the checks a drafted plan gets: one that names the
// wrong verify command halts the request and never reaches plan_review.
func TestPlanningHaltsOnAHandedOverPlanThatFailsValidation(t *testing.T) {
	dp := newFakeDeps(t)
	dataDir, id := requestdrivertest.HandedOverPlanFixture(t, map[string]string{"001.spec.md": requestdrivertest.ValidBrownfieldTicket("make something-else", 1, 2)})
	if err := requestdrivertest.DriveRequests(dp, context.Background(), dataDir, requestdriver.WorkerConfig{}, requestdrivertest.FailingSpecDraftRunner(t), requestdrivertest.FailingPlanTicketsRunner(t), requestdrivertest.FailingOracleDraftRunner(t), requestdrivertest.FailingBuildRunner(t)); err != nil {
		t.Fatalf("driveRequests: %v", err)
	}
	loaded, err := request.Load(dataDir, id)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.State != request.StateHalted || !strings.Contains(loaded.Error, "declares Verify-Command") {
		t.Fatalf("State = %q, Error = %q; want halted on the verify command", loaded.State, loaded.Error)
	}
}

func TestPlanningStopsBeforeTakingImportedTicketsWhenSpecCriteriaChanged(t *testing.T) {
	dp := newFakeDeps(t)
	dataDir, id := requestdrivertest.HandedOverPlanFixture(t, map[string]string{
		"001.spec.md": requestdrivertest.ValidBrownfieldTicket(requestdrivertest.HandoverVerify, 1, 2),
	})
	r, err := request.Load(dataDir, id)
	if err != nil {
		t.Fatal(err)
	}
	changed := strings.Replace(requestdrivertest.TwoCriteriaSpec, "1. A retried POST /refunds with the same idempotency key returns the original result.", "1. Changed.", 1)
	if err := os.WriteFile(requestdriver.RequestSpecPath(dataDir, id), []byte(changed), 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(changed))
	r.ApprovedSHA256["spec.md"] = hex.EncodeToString(sum[:])
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}

	if err := requestdrivertest.DriveRequests(dp, context.Background(), dataDir, requestdriver.WorkerConfig{}, requestdrivertest.FailingSpecDraftRunner(t), requestdrivertest.FailingPlanTicketsRunner(t), requestdrivertest.FailingOracleDraftRunner(t), requestdrivertest.FailingBuildRunner(t)); err != nil {
		t.Fatalf("driveRequests: %v", err)
	}
	loaded, err := request.Load(dataDir, id)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.State != request.StateHalted || !strings.Contains(loaded.Error, "the spec's criteria changed after the plan was handed over") || !strings.Contains(loaded.Error, "resubmit the spec and plan") {
		t.Fatalf("State = %q, Error = %q; want the criteria-change halt and choices", loaded.State, loaded.Error)
	}
	if loaded.TicketCount != 0 {
		t.Fatalf("TicketCount = %d, want zero because imported tickets were not taken", loaded.TicketCount)
	}
}

// After a plan_review rejection the planning job runs, to revise the tickets.
func TestPlanningRunsThePlanningJobAfterAPlanRejection(t *testing.T) {
	dp := newFakeDeps(t)
	ticket := requestdrivertest.ValidBrownfieldTicket(requestdrivertest.HandoverVerify, 1, 2)
	dataDir, id := requestdrivertest.HandedOverPlanFixture(t, map[string]string{"001.spec.md": ticket})
	r, err := request.Load(dataDir, id)
	if err != nil {
		t.Fatal(err)
	}
	r.Rejections = append(r.Rejections, request.Rejection{By: "op", At: "2026-10-04T00:00:00Z", Reason: "name the migration file", FromState: request.StatePlanReview})
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	runner, gotVerify := requestdrivertest.StubPlanTicketsRunner([]requestdriver.DraftedTicket{{Filename: "001.spec.md", Content: ticket}}, &request.PlanEvidence{}, nil)
	if err := requestdrivertest.DriveRequests(dp, context.Background(), dataDir, requestdriver.WorkerConfig{}, requestdrivertest.FailingSpecDraftRunner(t), runner, requestdrivertest.FailingOracleDraftRunner(t), requestdrivertest.FailingBuildRunner(t)); err != nil {
		t.Fatalf("driveRequests: %v", err)
	}
	loaded, err := request.Load(dataDir, id)
	if err != nil {
		t.Fatal(err)
	}
	if *gotVerify != requestdrivertest.HandoverVerify || loaded.State != request.StatePlanReview {
		t.Fatalf("planning job called with %q, State = %q (Error: %s)", *gotVerify, loaded.State, loaded.Error)
	}
}
