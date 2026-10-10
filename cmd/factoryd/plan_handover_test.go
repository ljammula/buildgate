package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"buildgate/internal/request"
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
