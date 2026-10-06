//go:build integration

package pgstore

import (
	"os"
	"testing"
	"time"
)

// These tests require the docker-compose Postgres service to be up
// (`docker compose up -d postgres`) and TODO_DATABASE_URL set to reach it
// -- see the repo's own Makefile "verify" target. Excluded from the
// default `go test ./...` run (no "integration" build tag) so nothing
// else needs Docker.
func openTestStore(t *testing.T) *Store {
	t.Helper()
	dsn := os.Getenv("TODO_DATABASE_URL")
	if dsn == "" {
		t.Skip("TODO_DATABASE_URL not set; run via `make verify`")
	}
	s, err := New(dsn)
	if err != nil {
		t.Fatalf("connect to postgres: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	if _, err := s.db.Exec(`TRUNCATE todos`); err != nil {
		t.Fatalf("reset todos table: %v", err)
	}
	return s
}

func TestAddWithIDAndGet(t *testing.T) {
	s := openTestStore(t)

	if err := s.AddWithID("t1", "buy milk", time.Time{}); err != nil {
		t.Fatalf("AddWithID: %v", err)
	}

	got, ok, err := s.Get("t1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !ok || got.Title != "buy milk" {
		t.Fatalf("unexpected get result: ok=%v got=%+v", ok, got)
	}
	if got.CreatedAt.IsZero() {
		t.Fatal("expected created_at to be set by the database")
	}
}

func TestAddWithIDIsIdempotent(t *testing.T) {
	s := openTestStore(t)

	if err := s.AddWithID("t1", "first title", time.Time{}); err != nil {
		t.Fatalf("AddWithID (first): %v", err)
	}
	// Simulates Kafka redelivering the same message -- must not error and
	// must not overwrite the existing row.
	if err := s.AddWithID("t1", "second title", time.Time{}); err != nil {
		t.Fatalf("AddWithID (redelivery): %v", err)
	}

	got, ok, err := s.Get("t1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !ok || got.Title != "first title" {
		t.Fatalf("expected the first write to win, got ok=%v got=%+v", ok, got)
	}
}

func TestUpdateAndDelete(t *testing.T) {
	s := openTestStore(t)

	if err := s.AddWithID("t1", "old title", time.Time{}); err != nil {
		t.Fatalf("AddWithID: %v", err)
	}

	updated, ok, err := s.Update("t1", "new title", true)
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if !ok || updated.Title != "new title" || !updated.Done {
		t.Fatalf("unexpected update result: ok=%v updated=%+v", ok, updated)
	}

	deleted, err := s.Delete("t1")
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if !deleted {
		t.Fatal("expected Delete to report a row was removed")
	}

	_, ok, err = s.Get("t1")
	if err != nil {
		t.Fatalf("Get after delete: %v", err)
	}
	if ok {
		t.Fatal("expected todo to be gone after Delete")
	}
}

// TestAddWithIDKeepsGivenCreatedAt: the event's own timestamp, not the
// insert time, orders the list -- so "b", created first but inserted
// second (a consumer backlog), still lists first.
func TestAddWithIDKeepsGivenCreatedAt(t *testing.T) {
	s := openTestStore(t)
	earlier := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	if err := s.AddWithID("a", "a", time.Time{}); err != nil {
		t.Fatalf("AddWithID a: %v", err)
	}
	if err := s.AddWithID("b", "b", earlier); err != nil {
		t.Fatalf("AddWithID b: %v", err)
	}

	got, ok, err := s.Get("b")
	if err != nil || !ok || !got.CreatedAt.Equal(earlier) {
		t.Fatalf("Get b: ok=%v err=%v created_at=%v, want %v", ok, err, got.CreatedAt, earlier)
	}
	list, err := s.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 2 || list[0].ID != "b" || list[1].ID != "a" {
		t.Fatalf("expected b (created earlier) before a, got %+v", list)
	}
}

func TestListOrdering(t *testing.T) {
	s := openTestStore(t)

	if err := s.AddWithID("a", "a", time.Time{}); err != nil {
		t.Fatalf("AddWithID a: %v", err)
	}
	if err := s.AddWithID("b", "b", time.Time{}); err != nil {
		t.Fatalf("AddWithID b: %v", err)
	}

	list, err := s.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 2 || list[0].Title != "a" || list[1].Title != "b" {
		t.Fatalf("unexpected list: %+v", list)
	}
}
