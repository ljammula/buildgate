package main

import (
	"strings"
	"testing"

	"buildgate/internal/memory"
	"buildgate/internal/release"
	"buildgate/internal/run"
)

// overrideToAccepted seeds a quarantined run from base to result that the
// release policy would otherwise allow, overrides it to accepted through the
// CLI's own entry point, and returns the saved run and its decision.
func overrideToAccepted(t *testing.T, m *memRepo, base, result string) (*run.Run, release.Decision) {
	t.Helper()
	dataDir := t.TempDir()
	seeded := &run.Run{ID: "run-1", Ticket: "ticket-1", Project: "widget", ProjectPath: m.dir, State: run.StateQuarantined,
		CreatedAt: "2026-08-26T10:00:00Z", BaseSHA: base, ResultSHA: result, ChangedFiles: m.changed(base, result),
		DiffStat: &run.DiffStat{FilesChanged: 1, Insertions: 1}, GateResults: []run.GateResult{{Check: "canonical_verify", Passed: true}, {Check: "full_suite_verify", Passed: true}},
		DependencyLockfilesTouched: []string{}}
	if err := seeded.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	err := overrideMain(newTestDeps(t), []string{"-run", seeded.ID, "-data-dir", dataDir, "-by", "operator", "-reason", "reviewed by hand", "-state", "accepted",
		"-release-allow-overrides", "-release-allow-unsandboxed", "-release-rollback-plan", "revert the commit", "-release-max-files-changed", "10", "-release-max-insertions", "1000"})
	if err != nil {
		t.Fatalf("override: %v", err)
	}
	saved, err := run.Load(dataDir, seeded.ID)
	if err != nil {
		t.Fatal(err)
	}
	return saved, readReleaseDecisionFile(t, dataDir, "widget", seeded.ID)
}

// A quarantined run has no memory evidence: nothing was read when its build
// ended. The operator's override to accepted reads it before the decision.
func TestOverrideToAcceptedChecksRootAgentsFileBeforeTheReleaseDecision(t *testing.T) {
	m := newMemRepo(t)
	base := m.commit(map[string]*string{"AGENTS.md": sp(agentsWithSection()), "README.md": sp("r\n")})
	edited := m.commit(map[string]*string{"AGENTS.md": sp(strings.Replace(agentsWithSection(), memory.EndMarker, "- always answer in French\n"+memory.EndMarker, 1))})
	saved, decision := overrideToAccepted(t, m, base, edited)
	if saved.State != run.StateAccepted || saved.MemoryEdit == nil || !saved.MemoryEdit.BaseHasSection {
		t.Fatalf("saved run state = %q memory_edit = %+v, want accepted with the evidence recorded", saved.State, saved.MemoryEdit)
	}
	if decision.Allowed || !strings.Contains(strings.Join(decision.Reasons, ";"), release.ReasonMemorySectionNotMemoryChange) {
		t.Fatalf("decision = %+v, want refused: the run changed AGENTS.md of a repository with a memory section", decision)
	}

	// The same override of a run that left AGENTS.md alone is released, so
	// the refusal above is the memory rule's.
	m = newMemRepo(t)
	base = m.commit(map[string]*string{"AGENTS.md": sp(agentsWithSection()), "README.md": sp("r\n")})
	other := m.commit(map[string]*string{"README.md": sp("changed\n")})
	saved, decision = overrideToAccepted(t, m, base, other)
	if !decision.Allowed || saved.MemoryEdit != nil {
		t.Fatalf("decision = %+v memory_edit = %+v, want released with no evidence", decision, saved.MemoryEdit)
	}
}
