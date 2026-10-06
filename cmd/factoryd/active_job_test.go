package main

import (
	"bytes"
	"os"
	"testing"
	"time"

	"buildgate/internal/modelrole"
	"buildgate/internal/request"
	"buildgate/internal/sandbox"
)

// TestStartActiveJobRecordsAndClears: a drafting job's role, model,
// effort and route are recorded when it launches (so the console can show
// them mid-job) and removed by the returned func -- in their own file,
// never in request.json, which another writer (an operator's cancel or
// approve) may be updating concurrently.
func TestStartActiveJobRecordsAndClears(t *testing.T) {
	dataDir := t.TempDir()
	r := request.New("req-active", "/repo", "repo", request.Source{Kind: request.SourceText}, time.Now())
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(request.Path(dataDir, r.ID))
	if err != nil {
		t.Fatal(err)
	}
	override := requestJobRoleOverride{
		Thinking: "max",
		Role:     modelrole.RolePlanning,
		RouteSelection: &modelrole.Selection{
			ModelName: "luna",
			RouteName: "codex",
			Policy:    sandbox.RoutePolicy{WorkerModelID: "gpt-5.6-luna"},
		},
	}
	started := time.Date(2026, 9, 28, 2, 30, 0, 0, time.UTC)
	clear := startActiveJob(dataDir, r.ID, modelrole.StageSpecDrafting, override, started)

	want := request.ActiveJob{Stage: "spec_drafting", Role: "planning", Model: "luna", ModelID: "gpt-5.6-luna", Thinking: "max", Route: "codex", StartedAt: "2026-09-28T02:30:00Z"}
	if got := request.LoadActiveJob(dataDir, r.ID); got == nil || *got != want {
		t.Fatalf("active job = %+v, want %+v", got, want)
	}
	after, err := os.ReadFile(request.Path(dataDir, r.ID))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("request.json changed while recording the active job:\nbefore %s\nafter  %s", before, after)
	}

	clear()
	if got := request.LoadActiveJob(dataDir, r.ID); got != nil {
		t.Fatalf("active job after clear = %+v, want nil", got)
	}
}
