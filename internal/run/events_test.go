package run

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"buildgate/internal/store"
)

func TestEventsDBPath(t *testing.T) {
	got := EventsDBPath("data")
	want := "data/events.db"
	if got != want {
		t.Fatalf("EventsDBPath = %q, want %q", got, want)
	}
}

// TestRecordEventAppendsToStore proves RecordEvent durably appends an
// event carrying this run's current state, readable back via
// internal/store.Store.List — the whole point of this being a real,
// separately-verifiable evidence trail, not merely "didn't error".
func TestRecordEventAppendsToStore(t *testing.T) {
	dataDir := t.TempDir()
	r := &Run{ID: "run-1", Ticket: "t", State: StateReady}
	if err := r.RecordEvent(dataDir); err != nil {
		t.Fatalf("RecordEvent: %v", err)
	}
	r.State = StateAccepted
	if err := r.RecordEvent(dataDir); err != nil {
		t.Fatalf("RecordEvent: %v", err)
	}

	s, err := store.Open(EventsDBPath(dataDir))
	if err != nil {
		t.Fatalf("open event store: %v", err)
	}
	defer s.Close()
	events, err := s.List(context.Background(), "run-1")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("events = %+v, want 2", events)
	}
	if events[0].Kind != string(StateReady) || events[1].Kind != string(StateAccepted) {
		t.Fatalf("event kinds = [%q, %q], want [%q, %q]", events[0].Kind, events[1].Kind, StateReady, StateAccepted)
	}
}

// TestRecordEventSeparatesRuns proves events from different run IDs don't
// bleed into each other's history in the shared per-data-dir store.
func TestRecordEventSeparatesRuns(t *testing.T) {
	dataDir := t.TempDir()
	if err := (&Run{ID: "run-a", State: StateReady}).RecordEvent(dataDir); err != nil {
		t.Fatalf("RecordEvent run-a: %v", err)
	}
	if err := (&Run{ID: "run-b", State: StateReady}).RecordEvent(dataDir); err != nil {
		t.Fatalf("RecordEvent run-b: %v", err)
	}

	s, err := store.Open(EventsDBPath(dataDir))
	if err != nil {
		t.Fatalf("open event store: %v", err)
	}
	defer s.Close()
	eventsA, err := s.List(context.Background(), "run-a")
	if err != nil {
		t.Fatalf("List run-a: %v", err)
	}
	if len(eventsA) != 1 {
		t.Fatalf("run-a events = %+v, want 1", eventsA)
	}
}

// TestPersistStampsProvenanceSavesAndRecordsOneEvent proves Persist is
// the single funnel M4-K1 introduced it to be: it stamps r.UpdatedAt and
// r.FactorydVersion, writes the authoritative run.json (readable back via
// Load), and appends exactly one durable event carrying this run's
// current state.
func TestPersistStampsProvenanceSavesAndRecordsOneEvent(t *testing.T) {
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

	s, err := store.Open(EventsDBPath(dataDir))
	if err != nil {
		t.Fatalf("open event store: %v", err)
	}
	defer s.Close()
	events, err := s.List(context.Background(), "run-1")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("events = %+v, want 1", events)
	}
	if events[0].Kind != string(StateReady) {
		t.Errorf("event kind = %q, want %q", events[0].Kind, StateReady)
	}
}

// TestPersistLeavesFactorydVersionEmptyWhenUnset proves a test (or any
// caller) that never sets the package-level FactorydVersion gets the
// zero value "" stamped, matching a pre-this-field run record -- not a
// crash or a placeholder string.
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
// documented failure split: a Save failure (here, an unwritable run
// directory) is returned to the caller, matching save()'s own pre-M4-K1
// behavior of treating a Save failure as fatal to the whole persist.
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
