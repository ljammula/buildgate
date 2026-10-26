package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"buildgate/internal/request"
	"buildgate/internal/requestdriver"
	"buildgate/internal/requestdriver/requestdrivertest"
)

func TestStagePreviousPlanPrefersTheReviewedTicketsThenTheHandedOverOnes(t *testing.T) {
	dataDir, id := requestdrivertest.HandedOverPlanFixture(t, map[string]string{"001.spec.md": "handed over 1\n", "002.spec.md": "handed over 2\n", "notes.txt": "ignored"})
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

	draftedData, draftedID := requestdrivertest.ApprovedPlanningFixture(t, requestdrivertest.TwoCriteriaSpec, "make verify")
	drafted, err := request.Load(draftedData, draftedID)
	if err != nil {
		t.Fatal(err)
	}
	if path, err := stagePreviousPlan(draftedData, drafted, scratch, ""); err != nil || path != "" {
		t.Fatalf("a drafted plan staged %q (%v); want nothing", path, err)
	}
}

func TestStagePreviousPlanAfterPlanReviewRejectUsesHandedOverTickets(t *testing.T) {
	ticket := requestdrivertest.ValidBrownfieldTicket(requestdrivertest.HandoverVerify, 1, 2)
	dataDir, id := requestdrivertest.HandedOverPlanFixture(t, map[string]string{"001.spec.md": ticket})
	r, err := request.Load(dataDir, id)
	if err != nil {
		t.Fatal(err)
	}
	if err := requestdriver.AdvancePlanning(newTestDeps(t), context.Background(), dataDir, r, requestdriver.WorkerConfig{}, requestdrivertest.FailingPlanTicketsRunner(t), time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := request.Reject(dataDir, id, "operator", "revise the tickets", time.Now()); err != nil {
		t.Fatalf("reject plan review: %v", err)
	}
	rejected, err := request.Load(dataDir, id)
	if err != nil {
		t.Fatal(err)
	}
	if !rejected.PlanImported || !rejected.PlanImportEnded {
		t.Fatalf("after reject: PlanImported = %v, PlanImportEnded = %v; want import history and ended path", rejected.PlanImported, rejected.PlanImportEnded)
	}
	path, err := stagePreviousPlan(dataDir, rejected, filepath.Join(t.TempDir(), "scratch"), "")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != "=== 001.spec.md ===\n"+ticket+"\n" {
		t.Fatalf("staged %q (%v), want the handed-over plan", got, err)
	}
}

func TestStagePreviousPlanAfterCriteriaChangeSendBackUsesHandedOverTickets(t *testing.T) {
	dataDir, id := requestdrivertest.HandedOverPlanFixture(t, map[string]string{"001.spec.md": "handed over after criteria change\n"})
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
	if err := requestdriver.AdvancePlanning(newTestDeps(t), context.Background(), dataDir, r, requestdriver.WorkerConfig{}, requestdrivertest.FailingPlanTicketsRunner(t), time.Now()); err != nil {
		t.Fatal(err)
	}
	halted, err := request.Load(dataDir, id)
	if err != nil || halted.State != request.StateHalted {
		t.Fatalf("criteria-change pass: state = %q, err = %v; want halted", halted.State, err)
	}
	if _, err := request.SendBack(dataDir, id, "operator", "replan for the changed criteria", request.SendBackToPlan, time.Now()); err != nil {
		t.Fatalf("send back to plan: %v", err)
	}
	replanned, err := request.Load(dataDir, id)
	if err != nil {
		t.Fatal(err)
	}
	path, err := stagePreviousPlan(dataDir, replanned, filepath.Join(t.TempDir(), "scratch"), "")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != "=== 001.spec.md ===\nhanded over after criteria change\n\n" {
		t.Fatalf("staged %q (%v), want the handed-over plan after send-back", got, err)
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
