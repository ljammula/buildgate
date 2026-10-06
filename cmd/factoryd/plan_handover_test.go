package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"buildgate/internal/request"
	"buildgate/internal/requestdriver"
)

const handoverVerify = "python3 -m unittest tests/test_product_lab.py"

// handedOverPlanFixture is a request in planning (spec approved) whose
// tickets the operator handed over, stored where Submit stores them.
func handedOverPlanFixture(t *testing.T, tickets map[string]string) (dataDir, id string) {
	t.Helper()
	dataDir, id = approvedPlanningFixtureNoFactoryYML(t, twoCriteriaSpec, handoverVerify)
	r, err := request.Load(dataDir, id)
	if err != nil {
		t.Fatal(err)
	}
	r.PlanImported = true
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	dir := request.ImportedTicketsDir(dataDir, id)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	for name, content := range tickets {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dataDir, id
}

func TestPlanningTakesHandedOverTicketsWithoutThePlanningJob(t *testing.T) {
	dp := newTestDeps(t)
	ticket := validBrownfieldTicket(handoverVerify, 1, 2)
	dataDir, id := handedOverPlanFixture(t, map[string]string{"001.spec.md": ticket})

	if err := driveRequests(dp, context.Background(), dataDir, requestdriver.WorkerConfig{}, failingSpecDraftRunner(t), failingPlanTicketsRunner(t), failingOracleDraftRunner(t), failingBuildRunner(t)); err != nil {
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
	dp := newTestDeps(t)
	dataDir, id := handedOverPlanFixture(t, map[string]string{"001.spec.md": validBrownfieldTicket("make something-else", 1, 2)})
	if err := driveRequests(dp, context.Background(), dataDir, requestdriver.WorkerConfig{}, failingSpecDraftRunner(t), failingPlanTicketsRunner(t), failingOracleDraftRunner(t), failingBuildRunner(t)); err != nil {
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

// After a plan_review rejection the planning job runs, to revise the tickets.
func TestPlanningRunsThePlanningJobAfterAPlanRejection(t *testing.T) {
	dp := newTestDeps(t)
	ticket := validBrownfieldTicket(handoverVerify, 1, 2)
	dataDir, id := handedOverPlanFixture(t, map[string]string{"001.spec.md": ticket})
	r, err := request.Load(dataDir, id)
	if err != nil {
		t.Fatal(err)
	}
	r.Rejections = append(r.Rejections, request.Rejection{By: "op", At: "2026-10-04T00:00:00Z", Reason: "name the migration file", FromState: request.StatePlanReview})
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	runner, gotVerify := stubPlanTicketsRunner([]requestdriver.DraftedTicket{{Filename: "001.spec.md", Content: ticket}}, &request.PlanEvidence{}, nil)
	if err := driveRequests(dp, context.Background(), dataDir, requestdriver.WorkerConfig{}, failingSpecDraftRunner(t), runner, failingOracleDraftRunner(t), failingBuildRunner(t)); err != nil {
		t.Fatalf("driveRequests: %v", err)
	}
	loaded, err := request.Load(dataDir, id)
	if err != nil {
		t.Fatal(err)
	}
	if *gotVerify != handoverVerify || loaded.State != request.StatePlanReview {
		t.Fatalf("planning job called with %q, State = %q (Error: %s)", *gotVerify, loaded.State, loaded.Error)
	}
}

func TestStagePreviousPlanPrefersTheReviewedTicketsThenTheHandedOverOnes(t *testing.T) {
	dataDir, id := handedOverPlanFixture(t, map[string]string{"001.spec.md": "handed over 1\n", "002.spec.md": "handed over 2\n", "notes.txt": "ignored"})
	r, err := request.Load(dataDir, id)
	if err != nil {
		t.Fatal(err)
	}
	scratch := filepath.Join(t.TempDir(), "scratch")

	// No reviewed plan yet (the factory removed an infeasible import).
	path, err := stagePreviousPlan(dataDir, r, scratch, "")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	if want := "=== 001.spec.md ===\nhanded over 1\n\n=== 002.spec.md ===\nhanded over 2\n\n"; string(got) != want {
		t.Fatalf("staged %q, want %q", got, want)
	}

	// A reviewed plan exists: it is the document to revise.
	tickets := filepath.Join(request.Dir(dataDir, id), "tickets")
	if err := os.MkdirAll(tickets, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tickets, "001.spec.md"), []byte("reviewed 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	path, err = stagePreviousPlan(dataDir, r, scratch, "")
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(path); string(got) != "=== 001.spec.md ===\nreviewed 1\n\n" {
		t.Fatalf("staged %q, want the reviewed ticket", got)
	}

	r.PlanImported = false
	if path, err := stagePreviousPlan(dataDir, r, scratch, ""); err != nil || path != "" {
		t.Fatalf("a drafted plan staged %q (%v); want nothing", path, err)
	}
}

func TestReadSubmitPlanDir(t *testing.T) {
	dir := t.TempDir()
	if tickets, err := readSubmitPlanDir(""); err != nil || tickets != nil {
		t.Fatalf("no -plan-dir gave %v, %v", tickets, err)
	}
	if _, err := readSubmitPlanDir(dir); err == nil || !strings.Contains(err.Error(), "holds no ticket file") {
		t.Fatalf("empty dir: err = %v", err)
	}
	if _, err := readSubmitPlanDir(filepath.Join(dir, "absent")); err == nil || !strings.Contains(err.Error(), "read -plan-dir") {
		t.Fatalf("missing dir: err = %v", err)
	}
	for name, content := range map[string]string{"002.spec.md": "two", "001.spec.md": "one", "README.md": "skip"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	tickets, err := readSubmitPlanDir(dir)
	if err != nil || len(tickets) != 2 || tickets[0].Filename != "001.spec.md" || tickets[1].Content != "two" {
		t.Fatalf("tickets = %+v (%v)", tickets, err)
	}
}
