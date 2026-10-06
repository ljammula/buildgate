package sandbox

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"buildgate/internal/evidence"
)

func writeOracleSource(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "expected.txt"), []byte("pristine\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestSnapshotReferenceOracleCopiesAndHashesTheSnapshot(t *testing.T) {
	src := writeOracleSource(t)
	want, err := evidence.SHA256Tree(src)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := filepath.Join(t.TempDir(), "snap")

	got, err := SnapshotReferenceOracle(t.TempDir(), src, snapshot)
	if err != nil {
		t.Fatalf("SnapshotReferenceOracle: %v", err)
	}
	if got != want {
		t.Errorf("hash = %q, want %q", got, want)
	}
	if b, err := os.ReadFile(filepath.Join(snapshot, "expected.txt")); err != nil || string(b) != "pristine\n" {
		t.Errorf("snapshot content = %q, %v; want the source's content", b, err)
	}
}

// The recorded hash must describe the snapshot, not a later state of the
// live source: editing the source after the call changes nothing in the
// snapshot or the returned hash (the TOCTOU property SC-012 needs).
func TestSnapshotReferenceOracleIsIndependentOfLaterSourceEdits(t *testing.T) {
	src := writeOracleSource(t)
	snapshot := filepath.Join(t.TempDir(), "snap")
	hash, err := SnapshotReferenceOracle(t.TempDir(), src, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "expected.txt"), []byte("tampered\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	after, err := evidence.SHA256Tree(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if after != hash {
		t.Errorf("snapshot hash drifted after a source edit: %q -> %q", hash, after)
	}
}

func TestSnapshotReferenceOracleRefusesASourceInsideTheWorkspace(t *testing.T) {
	workspace := t.TempDir()
	inside := filepath.Join(workspace, "oracle-src")
	if err := os.MkdirAll(inside, 0o750); err != nil {
		t.Fatal(err)
	}
	snapshot := filepath.Join(t.TempDir(), "snap")

	_, err := SnapshotReferenceOracle(workspace, inside, snapshot)
	if err == nil || !strings.Contains(err.Error(), "must not be inside the workspace") {
		t.Fatalf("error = %v, want the containment refusal", err)
	}
	if _, statErr := os.Stat(snapshot); !os.IsNotExist(statErr) {
		t.Errorf("a refused snapshot left %s behind: %v", snapshot, statErr)
	}
}

// A retried Temporal Activity attempt finds the prior attempt's leftover
// snapshot; evidence.SnapshotTree refuses an existing destination, so the
// helper must clear it first rather than fail the retry.
func TestSnapshotReferenceOracleClearsAStaleSnapshot(t *testing.T) {
	src := writeOracleSource(t)
	snapshot := filepath.Join(t.TempDir(), "snap")
	if err := os.MkdirAll(snapshot, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(snapshot, "leftover.txt"), []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := SnapshotReferenceOracle(t.TempDir(), src, snapshot); err != nil {
		t.Fatalf("SnapshotReferenceOracle over a stale snapshot: %v", err)
	}
	if _, err := os.Stat(filepath.Join(snapshot, "leftover.txt")); !os.IsNotExist(err) {
		t.Errorf("stale file survived into the new snapshot: %v", err)
	}
}

func TestSnapshotReferenceOracleRefusesASymlinkSource(t *testing.T) {
	real := writeOracleSource(t)
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if _, err := SnapshotReferenceOracle(t.TempDir(), link, filepath.Join(t.TempDir(), "snap")); err == nil {
		t.Fatal("SnapshotReferenceOracle over a symlinked source: want an error, got nil")
	}
}

func TestSnapshotReferenceOracleNamesAnUnreachableSource(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "not-here")
	_, err := SnapshotReferenceOracle(t.TempDir(), missing, filepath.Join(t.TempDir(), "snap"))
	if err == nil || !strings.Contains(err.Error(), "is not accessible on this host") {
		t.Fatalf("error = %v, want it to say the directory is not accessible, not a misleading containment refusal", err)
	}
	if strings.Contains(err.Error(), "inside the workspace") {
		t.Errorf("error %q misreports an absent directory as a containment violation", err)
	}
}
