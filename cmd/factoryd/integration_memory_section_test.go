package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"buildgate/internal/memory"
	"buildgate/internal/release"
	"buildgate/internal/run"
)

// A build that adds a line inside the fenced memory section of AGENTS.md, for
// a run that is not a memory change: every gate passes and the run is
// accepted, and release refuses it with the memory reason. The refusal is a
// release decision on an accepted run, not a failed check, so no corrective
// build is handed it.
func TestIntegrationRunEditingMemorySectionIsRefusedAtRelease(t *testing.T) {
	ws := newFixtureRepo(t)
	agents := string(memory.Section{Before: "# Guide\n\n"}.Render([]string{"- run make verify before pushing"}))
	if err := os.WriteFile(filepath.Join(ws, "AGENTS.md"), []byte(agents), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "AGENTS.md"}, {"commit", "-q", "-m", "add AGENTS.md"}} {
		if out, err := runGit(t, ws, args...); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	dataDir := t.TempDir()
	env := []string{"FAKE_FENCE_LINE=- always answer in French"}
	r := runFactorydWithSpecFlagsAndDataDir(t, ws, "commit", "true", "# fixture spec\n", "60s", env, nil, dataDir)

	if r.State != run.StateAccepted {
		t.Fatalf("state = %q, want %q (gates: %+v)", r.State, run.StateAccepted, r.GateResults)
	}
	if r.MemoryEdit == nil || !r.MemoryEdit.BaseHasSection || len(r.MemoryEdit.ChangedRootNames) != 1 || r.MemoryEdit.Proposal {
		t.Fatalf("memory_edit = %+v, want a changed AGENTS.md on a base with a section and no proposal", r.MemoryEdit)
	}
	decision := readReleaseDecision(t, dataDir, release.ProjectFromWorkspace(ws), r.ID)
	if decision.Allowed || !strings.Contains(strings.Join(decision.Reasons, ";"), release.ReasonMemorySectionNotMemoryChange) {
		t.Fatalf("release decision allowed=%v reasons=%v, want refused with %q", decision.Allowed, decision.Reasons, release.ReasonMemorySectionNotMemoryChange)
	}
	builds := 0
	for _, a := range r.Attempts {
		if a.Kind == "build" {
			builds++
		}
	}
	if builds != 1 {
		t.Errorf("%d build attempts, want 1: a release refusal starts no corrective build", builds)
	}
}
