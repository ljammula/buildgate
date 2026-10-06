// Package store provides durable, append-only evidence storage.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

type Event struct {
	ID        int64
	RunID     string
	Kind      string
	Payload   json.RawMessage
	CreatedAt string
}

type Store struct{ db *sql.DB }

func Open(path string) (*Store, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("open event store: path is required")
	}
	// Pre-create the database file with the same restrictive permissions
	// run.Run.Save already uses for run.json (0o600) — found via review:
	// database/sql's own Open (and modernc.org/sqlite underneath it) has
	// no portable way to specify a file mode, so it creates the file
	// under whatever the process's default umask allows (typically
	// world-readable at 0o644). This store's payload is the full run
	// record, including workspace paths and command arguments — exactly
	// what run.json's own 0o600 protects; leaving the event log more
	// permissive than the authoritative record it supplements would
	// expose the same information to any other local user. A pre-existing
	// file (the common case — every save after the first) is left as-is:
	// O_CREATE only applies the given mode when actually creating a new
	// file, never on an existing one, so this can't loosen a mode a prior
	// call (or an operator) already set.
	//
	// Skipped for SQLite's own special non-file DSN forms (":memory:",
	// or any "file:" URI DSN, which carries its own query-parameter mode
	// like ?mode=memory) — found via review, fixing a real bug this
	// introduced: os.OpenFile has no concept of those as anything but a
	// literal filename, so it would create (and leak) a stray file with
	// that exact name in the working directory, while sql.Open itself
	// still correctly recognizes and honors the special DSN regardless —
	// this store's own TestStoreRejectsNonDurableSQLiteDSNs deliberately
	// exercises both forms.
	if path != ":memory:" && !strings.HasPrefix(path, "file:") {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
		if err != nil {
			return nil, fmt.Errorf("open event store: %w", err)
		}
		f.Close()
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open event store: %w", err)
	}
	db.SetMaxOpenConns(1)
	var journalMode string
	if err := db.QueryRow(`PRAGMA journal_mode=WAL`).Scan(&journalMode); err != nil {
		db.Close()
		return nil, fmt.Errorf("configure event store journal: %w", err)
	}
	if !strings.EqualFold(journalMode, "wal") {
		db.Close()
		return nil, fmt.Errorf("configure event store journal: got %q, want WAL", journalMode)
	}
	// WAL mode's own -wal and -shm sidecar files are created by SQLite
	// itself, not by the os.OpenFile pre-create above, so they inherit
	// the process umask the same way the main file would have without
	// it — chmod them explicitly too, for the same reason. Best-effort:
	// a sidecar not existing yet (nothing has been appended, so no -wal
	// content) is not an error.
	for _, suffix := range []string{"-wal", "-shm"} {
		if err := os.Chmod(path+suffix, 0o600); err != nil && !os.IsNotExist(err) {
			db.Close()
			return nil, fmt.Errorf("restrict event store %s permissions: %w", suffix, err)
		}
	}
	if _, err := db.Exec(`PRAGMA synchronous=FULL; PRAGMA busy_timeout=5000; CREATE TABLE IF NOT EXISTS events (id INTEGER PRIMARY KEY AUTOINCREMENT, run_id TEXT NOT NULL, kind TEXT NOT NULL, payload BLOB NOT NULL, created_at TEXT NOT NULL); CREATE INDEX IF NOT EXISTS events_run_id_id ON events(run_id, id);`); err != nil {
		db.Close()
		return nil, fmt.Errorf("initialize event store: %w", err)
	}
	// The CREATE TABLE/INDEX above is the first real write for a brand
	// new database — WAL mode may only materialize -wal/-shm at this
	// point, not during the PRAGMA above, so restrict them again now
	// that they're guaranteed to exist if they're going to at all.
	for _, suffix := range []string{"-wal", "-shm"} {
		if err := os.Chmod(path+suffix, 0o600); err != nil && !os.IsNotExist(err) {
			db.Close()
			return nil, fmt.Errorf("restrict event store %s permissions: %w", suffix, err)
		}
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) Append(ctx context.Context, runID, kind string, payload any) (Event, error) {
	if runID == "" || kind == "" {
		return Event{}, fmt.Errorf("run id and kind are required")
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return Event{}, fmt.Errorf("marshal event: %w", err)
	}
	created := time.Now().UTC().Format(time.RFC3339Nano)
	result, err := s.db.ExecContext(ctx, `INSERT INTO events(run_id, kind, payload, created_at) VALUES (?, ?, ?, ?)`, runID, kind, b, created)
	if err != nil {
		return Event{}, fmt.Errorf("append event: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return Event{}, fmt.Errorf("read event id: %w", err)
	}
	return Event{ID: id, RunID: runID, Kind: kind, Payload: b, CreatedAt: created}, nil
}

func (s *Store) List(ctx context.Context, runID string) ([]Event, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, run_id, kind, payload, created_at FROM events WHERE run_id = ? ORDER BY id`, runID)
	if err != nil {
		return nil, fmt.Errorf("list events: %w", err)
	}
	defer rows.Close()
	var events []Event
	for rows.Next() {
		var e Event
		if err := rows.Scan(&e.ID, &e.RunID, &e.Kind, &e.Payload, &e.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan event: %w", err)
		}
		events = append(events, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate events: %w", err)
	}
	if events == nil {
		events = []Event{}
	}
	return events, nil
}
