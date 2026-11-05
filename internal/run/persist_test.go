package run

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestPersistStampsProvenanceAndSaves proves Persist stamps r.UpdatedAt and
// r.FactorydVersion and writes run.json (readable back via Load).
func TestPersistStampsProvenanceAndSaves(t *testing.T) {
	dataDir := t.TempDir()
	old := FactorydVersion
	FactorydVersion = "v1.2.3-test"
	defer func() { FactorydVersion = old }()

	r := &Run{ID: "run-1", Ticket: "t", State: StateReady}
	before := time.Now()
	if err := r.Persist(dataDir); err != nil {
		t.Fatalf("Persist: %v", err)
	}

	if r.FactorydVersion != "v1.2.3-test" {
		t.Errorf("r.FactorydVersion = %q, want %q", r.FactorydVersion, "v1.2.3-test")
	}
	updated, err := time.Parse(time.RFC3339, r.UpdatedAt)
	if err != nil {
		t.Fatalf("parse r.UpdatedAt %q: %v", r.UpdatedAt, err)
	}
	if updated.Before(before.Add(-time.Second)) {
		t.Errorf("r.UpdatedAt = %v, want at or after %v", updated, before)
	}

	loaded, err := Load(dataDir, "run-1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.FactorydVersion != "v1.2.3-test" {
		t.Errorf("loaded.FactorydVersion = %q, want %q", loaded.FactorydVersion, "v1.2.3-test")
	}

}

// TestPersistLeavesFactorydVersionEmptyWhenUnset proves a test (or any
// caller) that never sets the package-level FactorydVersion gets the
// zero value "" stamped -- not a crash or a placeholder string.
func TestPersistLeavesFactorydVersionEmptyWhenUnset(t *testing.T) {
	dataDir := t.TempDir()
	old := FactorydVersion
	FactorydVersion = ""
	defer func() { FactorydVersion = old }()

	r := &Run{ID: "run-2", State: StateReady}
	if err := r.Persist(dataDir); err != nil {
		t.Fatalf("Persist: %v", err)
	}
	if r.FactorydVersion != "" {
		t.Errorf("r.FactorydVersion = %q, want empty", r.FactorydVersion)
	}
}

// TestPersistReturnsSaveErrorOnUnwritableDataDir proves Persist's
// documented failure: a Save failure (here, an unwritable run directory)
// is returned to the caller.
func TestPersistReturnsSaveErrorOnUnwritableDataDir(t *testing.T) {
	dataDir := t.TempDir()
	// Make the runs/ parent read-only so Save's own os.MkdirAll fails --
	// the simplest reliable way to force a real Save error without
	// mocking the filesystem.
	if err := os.MkdirAll(filepath.Join(dataDir, "runs"), 0o750); err != nil {
		t.Fatalf("mkdir runs: %v", err)
	}
	if err := os.Chmod(filepath.Join(dataDir, "runs"), 0o500); err != nil {
		t.Fatalf("chmod runs read-only: %v", err)
	}
	defer os.Chmod(filepath.Join(dataDir, "runs"), 0o750)

	r := &Run{ID: "run-3", State: StateReady}
	if err := r.Persist(dataDir); err == nil {
		t.Fatal("Persist: want error on an unwritable run directory, got nil")
	}
}
