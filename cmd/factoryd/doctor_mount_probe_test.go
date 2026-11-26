package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestDoctorMountProbeNeverTouchesAPreExistingFile is the regression test
// for a Codex P1: the mount probe runs against an operator-owned directory,
// and a fixed marker name that already existed there was truncated and then
// deleted. The probe must leave any file it did not create alone, whatever
// the Docker outcome, and leave none of its own behind.
func TestDoctorMountProbeNeverTouchesAPreExistingFile(t *testing.T) {
	dir := t.TempDir()
	existing := filepath.Join(dir, ".factoryd-doctor-mount-probe")
	if err := os.WriteFile(existing, []byte("operator data\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// A nonexistent docker binary: the probe fails after the marker step,
	// the path that used to remove the fixed file.
	_, markerErr, probeErr := doctorRunMarkerProbe(ctx, filepath.Join(dir, "no-such-docker"), "img", dir)
	if markerErr != nil {
		t.Fatalf("the marker was not written: %v", markerErr)
	}
	if probeErr == nil {
		t.Fatal("the probe against a missing docker binary succeeded")
	}
	got, err := os.ReadFile(existing)
	if err != nil {
		t.Fatalf("pre-existing file was removed: %v", err)
	}
	if string(got) != "operator data\n" {
		t.Fatalf("pre-existing file was rewritten: %q", got)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("probe left files behind: %v", entries)
	}
}
