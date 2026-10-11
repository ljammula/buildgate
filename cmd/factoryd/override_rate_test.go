package main

import (
	"os"
	"path/filepath"
	"testing"
)

// A run directory with no run.json (a run that never saved its record) is
// skipped, not a failure of the whole report.
func TestOverrideRateSkipsARunDirectoryWithNoRecord(t *testing.T) {
	dataDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dataDir, "runs", "never-saved", "launches"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := overrideRateMain([]string{"-data-dir", dataDir}); err != nil {
		t.Fatalf("override-rate failed on a run directory with no run.json: %v", err)
	}
}
