package main

import (
	"testing"

	"buildgate/internal/run"
)

// TestAnyRunRecordedUnderProjectFindsExplicitProject covers the ordinary
// case: a run recorded after run.Run.Project existed matches by that
// field directly.
func TestAnyRunRecordedUnderProjectFindsExplicitProject(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	r := &run.Run{ID: "run-1", ProjectPath: "/repos/myapp/workspace", Project: "myapp"}
	if err := r.Save(dataDir); err != nil {
		t.Fatalf("Save: %v", err)
	}

	found, err := anyRunRecordedUnderProject(dataDir, "myapp")
	if err != nil {
		t.Fatalf("anyRunRecordedUnderProject: %v", err)
	}
	if !found {
		t.Error("found = false, want true for a run whose Project field matches")
	}
}

// TestAnyRunRecordedUnderProjectFallsBackToDerivationForLegacyRuns covers
// a run recorded before run.Run.Project existed: its project id must
// still be found via the same derivation ProjectFromWorkspace applies, so
// a pre-existing run doesn't spuriously start reporting "no run has ever
// derived this id".
func TestAnyRunRecordedUnderProjectFallsBackToDerivationForLegacyRuns(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	r := &run.Run{ID: "run-1", ProjectPath: "/repos/myapp"} // Project left unset, as a legacy run would have it
	if err := r.Save(dataDir); err != nil {
		t.Fatalf("Save: %v", err)
	}

	found, err := anyRunRecordedUnderProject(dataDir, "myapp")
	if err != nil {
		t.Fatalf("anyRunRecordedUnderProject: %v", err)
	}
	if !found {
		t.Error("found = false, want true via fallback derivation from ProjectPath")
	}
}

// TestAnyRunRecordedUnderProjectReportsMismatch is the regression test for
// the 2026-09-05 Opus review finding S4: an operator's typed -project
// that no run ever actually derived must be detectable, not a silent
// no-op.
func TestAnyRunRecordedUnderProjectReportsMismatch(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	r := &run.Run{ID: "run-1", ProjectPath: "/repos/myapp/workspace", Project: "myapp"}
	if err := r.Save(dataDir); err != nil {
		t.Fatalf("Save: %v", err)
	}

	found, err := anyRunRecordedUnderProject(dataDir, "myapp-typo")
	if err != nil {
		t.Fatalf("anyRunRecordedUnderProject: %v", err)
	}
	if found {
		t.Error("found = true, want false for an id no run ever derived")
	}
}

func TestAnyRunRecordedUnderProjectNoRunsDirectory(t *testing.T) {
	t.Parallel()
	found, err := anyRunRecordedUnderProject(t.TempDir(), "myapp")
	if err != nil {
		t.Fatalf("anyRunRecordedUnderProject: %v, want no error for a data dir with no runs subdirectory yet", err)
	}
	if found {
		t.Error("found = true, want false with no runs recorded at all")
	}
}

// TestAnyRunRecordedUnderProjectPrefersRecordedProjectOverDerivation pins
// the fix for the audit's "project identity collides" finding: a run
// submitted from ~/code/payments is recorded under "payments", and the
// kill switch must be found under that id -- not under "code", the
// legacy parent-basename derivation every sibling checkout shares.
func TestAnyRunRecordedUnderProjectPrefersRecordedProjectOverDerivation(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	r := &run.Run{ID: "run-1", ProjectPath: "/home/u/code/payments", Project: "payments"}
	if err := r.Save(dataDir); err != nil {
		t.Fatalf("Save: %v", err)
	}

	if found, err := anyRunRecordedUnderProject(dataDir, "payments"); err != nil || !found {
		t.Errorf("anyRunRecordedUnderProject(payments) = %v, %v; want true under the recorded id", found, err)
	}
	if found, err := anyRunRecordedUnderProject(dataDir, "code"); err != nil || found {
		t.Errorf("anyRunRecordedUnderProject(code) = %v, %v; want false -- the legacy derivation must not shadow the recorded id", found, err)
	}
}
