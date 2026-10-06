package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"buildgate/internal/request"
	"buildgate/internal/requestdriver"
)

// TestAdvancePlanningClearsStaleTicketArtifactsOnReplan: a re-plan that
// drafts fewer tickets than its predecessor must not leave the old plan's
// NNN.spec.md / NNN.criteria.md / NNN.oracle/ behind (they would later be
// pinned at approval or mounted at build), and the cleanup must not follow a
// symlink out of the request's tickets directory.
func TestAdvancePlanningClearsStaleTicketArtifactsOnReplan(t *testing.T) {
	dp := newTestDeps(t)
	verify := "python3 -m unittest tests/test_product_lab.py"
	dataDir, id := approvedPlanningFixture(t, twoCriteriaSpec, verify)

	ticketsDir := filepath.Join(request.Dir(dataDir, id), "tickets")
	if err := os.MkdirAll(filepath.Join(ticketsDir, "002.oracle"), 0o750); err != nil {
		t.Fatal(err)
	}
	stale := []string{"002.spec.md", "001.criteria.md", "002.criteria.md", filepath.Join("002.oracle", "RUN_COMMAND.txt")}
	for _, name := range stale {
		if err := os.WriteFile(filepath.Join(ticketsDir, name), []byte("stale\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	outside := t.TempDir()
	keep := filepath.Join(outside, "keep.txt")
	if err := os.WriteFile(keep, []byte("keep\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(ticketsDir, "003.oracle")); err != nil {
		t.Fatal(err)
	}

	tickets := []requestdriver.DraftedTicket{{Filename: "001.spec.md", Content: validBrownfieldTicket(verify, 1, 2)}}
	runner, _ := stubPlanTicketsRunner(tickets, &request.PlanEvidence{}, nil)
	if err := driveRequests(dp, context.Background(), dataDir, requestdriver.WorkerConfig{}, failingSpecDraftRunner(t), runner, failingOracleDraftRunner(t), failingBuildRunner(t)); err != nil {
		t.Fatalf("driveRequests: %v", err)
	}

	loaded, err := request.Load(dataDir, id)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.State != request.StatePlanReview {
		t.Fatalf("State = %q, want plan_review (Error: %s)", loaded.State, loaded.Error)
	}
	entries, err := os.ReadDir(ticketsDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "001.spec.md" {
		names := []string{}
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("tickets dir = %v, want only the new 001.spec.md", names)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Errorf("cleanup followed a symlink out of the tickets dir: %v", err)
	}
}
