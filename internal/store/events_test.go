package store

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestStoreUsesWALAndSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	first, err := s.Append(context.Background(), "run-1", "ready", map[string]any{"state": "ready"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.Append(context.Background(), "run-1", "halted", map[string]any{"state": "halted"})
	if err != nil {
		t.Fatal(err)
	}
	if first.ID >= second.ID {
		t.Fatalf("event ids %d, %d not ordered", first.ID, second.ID)
	}
	var mode string
	if err := s.db.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil {
		t.Fatal(err)
	}
	if mode != "wal" {
		t.Fatalf("journal mode %q, want wal", mode)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	events, err := s.List(context.Background(), "run-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[0].Kind != "ready" || events[1].Kind != "halted" {
		t.Fatalf("events = %+v", events)
	}
	var payload map[string]string
	if err := json.Unmarshal(events[1].Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["state"] != "halted" {
		t.Fatalf("payload = %+v", payload)
	}
}

func TestStoreListSeparatesRuns(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.Append(context.Background(), "a", "x", nil); err != nil {
		t.Fatal(err)
	}
	got, err := s.List(context.Background(), "b")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("events = %+v", got)
	}
}

func TestStoreRejectsNonDurableSQLiteDSNs(t *testing.T) {
	for _, path := range []string{"", ":memory:", "file:events?mode=memory&cache=shared"} {
		t.Run(path, func(t *testing.T) {
			if s, err := Open(path); err == nil {
				s.Close()
				t.Fatalf("Open(%q) succeeded for a non-durable store", path)
			}
		})
	}
}

// TestStoreRejectsNonDurableSQLiteDSNsLeavesNoStrayFile is the regression
// test for a real bug the fix for TestOpenRestrictsFilePermissions itself
// introduced (found via review before it ever shipped): a naive
// os.OpenFile(path, ...) pre-create step has no concept of ":memory:" or
// a "file:" URI as anything but a literal filename, so it would silently
// create (and leak) a stray file with that exact name in the current
// working directory even though sql.Open itself still correctly
// recognizes and rejects the special DSN. Proven by running from a fresh
// temp working directory and asserting nothing new appears in it.
func TestStoreRejectsNonDurableSQLiteDSNsLeavesNoStrayFile(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	for _, path := range []string{":memory:", "file:events?mode=memory&cache=shared"} {
		if s, err := Open(path); err == nil {
			s.Close()
		}
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read working directory: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("working directory = %v, want empty — a special DSN leaked a stray file", entries)
	}
}

// TestOpenRestrictsFilePermissions proves the event database (and its
// WAL-mode -wal/-shm sidecar files) are created with the same restrictive
// permissions run.Run.Save already uses for run.json (0o600) — this
// store's payload is the full run record, including workspace paths and
// command arguments, exactly what run.json's own mode protects.
func TestOpenRestrictsFilePermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	if _, err := s.Append(context.Background(), "run-1", "ready", map[string]any{"state": "ready"}); err != nil {
		t.Fatalf("Append: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat event db: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("event db mode = %o, want 0600", perm)
	}

	for _, suffix := range []string{"-wal", "-shm"} {
		sidecar := path + suffix
		info, err := os.Stat(sidecar)
		if err != nil {
			if os.IsNotExist(err) {
				continue // not every SQLite build materializes -shm without shared-cache access
			}
			t.Fatalf("stat %s: %v", sidecar, err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Errorf("%s mode = %o, want 0600", sidecar, perm)
		}
	}
}

// TestOpenPreservesExistingFilePermissions proves a pre-existing database
// file's own permissions are never loosened by a later Open call — only
// a newly created file gets the restrictive default.
func TestOpenPreservesExistingFilePermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.db")
	if err := os.WriteFile(path, nil, 0o640); err != nil {
		t.Fatalf("pre-create event db: %v", err)
	}

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat event db: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o640 {
		t.Errorf("event db mode = %o, want unchanged 0640", perm)
	}
}

// Two builds that start in the same second each open the store, append one
// event and close it, from separate processes (separate connections here). An
// open that loses the race for a lock must wait for it rather than fail: a
// failed open drops the event, since callers treat this store as supplementary.
// "new" starts with no database, where the openers race to switch it to WAL;
// "existing" starts from one already in WAL mode, where an open meets the
// recovery and close-time checkpoint of the others.
func TestConcurrentOpenAppendCloseLosesNoEvent(t *testing.T) {
	for _, existing := range []bool{false, true} {
		name := "new"
		if existing {
			name = "existing"
		}
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "events.db")
			if existing {
				s, err := Open(path)
				if err != nil {
					t.Fatal(err)
				}
				if err := s.Close(); err != nil {
					t.Fatal(err)
				}
			}
			const writers, perWriter = 16, 5
			errs := make(chan error, 3*writers*perWriter)
			var wg sync.WaitGroup
			for w := 0; w < writers; w++ {
				wg.Add(1)
				go func(w int) {
					defer wg.Done()
					for i := 0; i < perWriter; i++ {
						s, err := Open(path)
						if err != nil {
							errs <- err
							continue
						}
						if _, err := s.Append(context.Background(), fmt.Sprintf("run-%d", w), "state", map[string]int{"i": i}); err != nil {
							errs <- err
						}
						if err := s.Close(); err != nil {
							errs <- err
						}
					}
				}(w)
			}
			wg.Wait()
			close(errs)
			for err := range errs {
				t.Errorf("concurrent writer: %v", err)
			}
			s, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			for w := 0; w < writers; w++ {
				events, err := s.List(context.Background(), fmt.Sprintf("run-%d", w))
				if err != nil {
					t.Fatal(err)
				}
				if len(events) != perWriter {
					t.Errorf("run-%d has %d events, want %d", w, len(events), perWriter)
				}
			}
		})
	}
}
