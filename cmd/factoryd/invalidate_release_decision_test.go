package main

import (
	"testing"

	"buildgate/internal/release"
	"buildgate/internal/run"
)

// TestInvalidatePriorRunOnFullSuiteRegressionInvalidatesStoredDecision is
// the regression test for a real GitHub Codex App review finding on this
// PR: InvalidatedByRunID is attribution added to a run's own record only
// after that run was already accepted, by which point recordReleaseDecision
// had typically already evaluated and durably saved an Allowed: true
// release.Decision. Without also invalidating that stored decision, GET
// /runs/{id}/release kept reporting allowed: true for a run a later
// regression had just discredited.
func TestInvalidatePriorRunOnFullSuiteRegressionInvalidatesStoredDecision(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	prior := &run.Run{ID: "run-prior", ProjectPath: "/repos/myapp/workspace", Project: "myapp"}
	if err := prior.Save(dataDir); err != nil {
		t.Fatalf("seed prior run: %v", err)
	}
	if err := release.SaveDecision(dataDir, release.Decision{RunID: prior.ID, Project: "myapp", Allowed: true, Evaluated: "2026-09-05T00:00:00Z"}); err != nil {
		t.Fatalf("seed prior decision: %v", err)
	}

	invalidatePriorRunOnFullSuiteRegression(dataDir, "run-successor", prior.ID, []string{"full_suite_verify"})

	decision, err := release.LoadDecision(dataDir, "myapp", prior.ID)
	if err != nil {
		t.Fatalf("LoadDecision: %v", err)
	}
	if decision == nil {
		t.Fatal("decision = nil, want the previously-saved decision to still exist (invalidated, not deleted)")
	}
	if decision.Allowed {
		t.Error("decision.Allowed = true after invalidation, want false")
	}
	if len(decision.Reasons) == 0 {
		t.Error("decision.Reasons is empty after invalidation, want a reason naming the regression")
	}
}

// TestRecordSpecDriftIfDetectedDoesNotInvalidateStoredDecision is the
// regression test for a real GitHub Codex App review finding on this PR:
// an earlier version of this fix also had recordSpecDriftIfDetected
// invalidate the prior run's stored release.Decision, exactly like a
// proven full-suite regression does. But recordSpecDriftIfDetected runs
// *before* the -repository path's own chain validation
// (ValidateSliceChainActivity) confirms the declared successor is even a
// legitimate one (see this function's own doc comment: "a chain that
// later turns out stale... may still have been marked drifted here" was
// already an accepted approximation before this PR, back when spec drift
// was purely advisory). Immediately flipping an already-published
// Allowed: true decision to denied on that same speculative signal would
// have made a previously-harmless approximation actively wrong. Only the
// run.json field write is unconditional; the stored decision must be
// left untouched.
func TestRecordSpecDriftIfDetectedDoesNotInvalidateStoredDecision(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	prior := &run.Run{
		ID: "run-prior", State: run.StateAccepted, ProjectPath: "/repos/myapp/workspace", Project: "myapp",
		ProductSpecSHA256: "base-hash",
	}
	if err := prior.Save(dataDir); err != nil {
		t.Fatalf("seed prior run: %v", err)
	}
	if err := release.SaveDecision(dataDir, release.Decision{RunID: prior.ID, Project: "myapp", Allowed: true, Evaluated: "2026-09-05T00:00:00Z"}); err != nil {
		t.Fatalf("seed prior decision: %v", err)
	}

	recordSpecDriftIfDetected(dataDir, "run-successor", prior.ProjectPath, prior, "different-hash", "")

	reloaded, err := run.Load(dataDir, prior.ID)
	if err != nil {
		t.Fatalf("load prior run: %v", err)
	}
	if reloaded.SpecDriftDetectedByRunID != "run-successor" {
		t.Errorf("SpecDriftDetectedByRunID = %q, want %q", reloaded.SpecDriftDetectedByRunID, "run-successor")
	}

	decision, err := release.LoadDecision(dataDir, "myapp", prior.ID)
	if err != nil {
		t.Fatalf("LoadDecision: %v", err)
	}
	if decision == nil {
		t.Fatal("decision = nil, want the previously-saved decision to still exist")
	}
	if !decision.Allowed {
		t.Error("decision.Allowed = false after spec drift was recorded, want true (unchanged) -- spec drift must not retroactively invalidate an already-published decision")
	}
	if decision.Invalidated {
		t.Error("decision.Invalidated = true after spec drift was recorded, want false")
	}
}

// TestInvalidatePriorRunOnFullSuiteRegressionWritesATombstoneWhenNoDecisionExistsYet
// is the regression test for the other half of the same review finding:
// an earlier version silently dropped the invalidation entirely when no
// decision had been recorded for the prior run yet (a real, if narrow,
// race -- a run's own run.json is saved as accepted before its decision
// is recorded, and a very fast successor chain could reach this call
// inside that window). InvalidateDecision must write a tombstone instead.
func TestInvalidatePriorRunOnFullSuiteRegressionWritesATombstoneWhenNoDecisionExistsYet(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	prior := &run.Run{ID: "run-prior", ProjectPath: "/repos/myapp/workspace", Project: "myapp"}
	if err := prior.Save(dataDir); err != nil {
		t.Fatalf("seed prior run: %v", err)
	}
	// Deliberately no release.SaveDecision call: no decision exists yet.

	invalidatePriorRunOnFullSuiteRegression(dataDir, "run-successor", prior.ID, []string{"full_suite_verify"})

	decision, err := release.LoadDecision(dataDir, "myapp", prior.ID)
	if err != nil {
		t.Fatalf("LoadDecision: %v", err)
	}
	if decision == nil {
		t.Fatal("decision = nil, want a tombstone decision to have been written")
	}
	if decision.Allowed {
		t.Error("decision.Allowed = true, want false")
	}
	if !decision.Invalidated {
		t.Error("decision.Invalidated = false, want true")
	}

	// A subsequent RecordDecision for the same run (e.g. the predecessor's
	// own recordReleaseDecision call finally reaching this point) must not
	// silently clear the tombstone with a fresh Allowed: true evaluation.
	if _, err := release.RecordDecision(dataDir, "myapp", *prior, release.MergePolicy{
		RollbackPlan: "reviewed and reversible", AllowUnsandboxed: true,
	}); err != nil {
		t.Fatalf("RecordDecision: %v", err)
	}
	afterRecord, err := release.LoadDecision(dataDir, "myapp", prior.ID)
	if err != nil {
		t.Fatalf("LoadDecision after RecordDecision: %v", err)
	}
	if afterRecord == nil {
		t.Fatal("decision = nil after RecordDecision")
	}
	if afterRecord.Allowed {
		t.Error("decision.Allowed = true after RecordDecision ran over an existing tombstone, want it to stay false")
	}
	if !afterRecord.Invalidated {
		t.Error("decision.Invalidated = false after RecordDecision ran over an existing tombstone, want it to stay true")
	}
}
