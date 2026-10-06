package run

import (
	"context"
	"fmt"
	"path/filepath"

	"buildgate/internal/store"
)

// EventsDBPath returns where the durable, append-only evidence-event log
// lives on disk: one shared SQLite WAL database per data directory (not
// per run — internal/store.Store's schema is already keyed by run_id),
// colocated with the runs/ directory tree the rest of this package reads
// and writes.
func EventsDBPath(dataDir string) string {
	return filepath.Join(dataDir, "events.db")
}

// RecordEvent durably appends an audit-trail snapshot of r's current
// state to the WAL-backed event store at EventsDBPath(dataDir),
// alongside — never instead of — the authoritative run.json record Save
// writes. The event's kind is r.State at the moment of the call, and its
// payload is the full run record, so replaying a run's events
// reconstructs its complete state history even if run.json itself is
// ever lost or corrupted; run.json remains the single source of truth
// for a run's *current* state, exactly as before this existed.
//
// Opens and closes the store on every call rather than holding a
// persistent handle — this package already has no persistent state of
// its own (Save/Load/WithLock all open exactly what they need per call),
// and SQLite's WAL mode is specifically designed to support many
// independent short-lived writers, including from separate processes
// sharing one -data-dir, the same way run.json's own flock-based
// WithLock already supports concurrent writers. Event writes are
// comparatively rare (state transitions, not per-log-line), so the
// per-call open/close overhead is not a meaningful cost.
//
// Callers must treat a returned error as informational, not fatal: this
// is supplementary evidence, and a store failure must never block or
// fail the primary Save it accompanies (see this function's callers'
// own comments for how they log-and-continue).
func (r *Run) RecordEvent(dataDir string) error {
	s, err := store.Open(EventsDBPath(dataDir))
	if err != nil {
		return fmt.Errorf("open event store: %w", err)
	}
	defer s.Close()
	if _, err := s.Append(context.Background(), r.ID, string(r.State), r); err != nil {
		return fmt.Errorf("append run event: %w", err)
	}
	return nil
}
