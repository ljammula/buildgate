//go:build integration

package pgstore

import (
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"sync"
	"testing"
	"time"
)

// scratchDB creates an empty database next to TODO_DATABASE_URL's own and
// drops it when the test ends, so migration tests start from a schema they
// control instead of the shared one the other tests TRUNCATE. The compose
// postgres user is a superuser, so CREATE DATABASE is allowed.
func scratchDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("TODO_DATABASE_URL")
	if dsn == "" {
		t.Fatal("TODO_DATABASE_URL not set; run via `make verify-integration`")
	}
	admin, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("open admin connection: %v", err)
	}
	t.Cleanup(func() { admin.Close() })

	name := fmt.Sprintf("migrate_test_%d", time.Now().UnixNano())
	if _, err := admin.Exec(`CREATE DATABASE ` + name); err != nil {
		t.Fatalf("create scratch database: %v", err)
	}
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse TODO_DATABASE_URL: %v", err)
	}
	u.Path = "/" + name
	db, err := sql.Open("postgres", u.String())
	if err != nil {
		t.Fatalf("open scratch database: %v", err)
	}
	t.Cleanup(func() {
		db.Close()
		admin.Exec(`DROP DATABASE IF EXISTS ` + name + ` WITH (FORCE)`)
	})
	return db
}

func appliedVersions(t *testing.T, db *sql.DB) []int {
	t.Helper()
	rows, err := db.Query(`SELECT version FROM schema_migrations ORDER BY version`)
	if err != nil {
		t.Fatalf("read schema_migrations: %v", err)
	}
	defer rows.Close()
	var out []int
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			t.Fatalf("scan version: %v", err)
		}
		out = append(out, v)
	}
	return out
}

func wantAllApplied(t *testing.T, db *sql.DB) {
	t.Helper()
	migrations, err := loadMigrations()
	if err != nil {
		t.Fatalf("loadMigrations: %v", err)
	}
	got := appliedVersions(t, db)
	if len(got) != len(migrations) {
		t.Fatalf("applied versions %v, want 1..%d", got, len(migrations))
	}
	for i, v := range got {
		if v != i+1 {
			t.Fatalf("applied versions %v, want 1..%d", got, len(migrations))
		}
	}
}

func TestMigrateFreshDatabase(t *testing.T) {
	db := scratchDB(t)
	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	wantAllApplied(t, db)

	var createdAt time.Time
	if err := db.QueryRow(`INSERT INTO todos (id, title) VALUES ('t1', 'x') RETURNING created_at`).Scan(&createdAt); err != nil {
		t.Fatalf("insert after migrate: %v", err)
	}
	if createdAt.IsZero() {
		t.Fatal("created_at should default to now()")
	}
}

func TestMigrateIsIdempotent(t *testing.T) {
	db := scratchDB(t)
	if err := Migrate(db); err != nil {
		t.Fatalf("first Migrate: %v", err)
	}
	if err := Migrate(db); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
	wantAllApplied(t, db)
}

// TestMigrateAdoptsPreMigrationsDatabase covers a database created by the
// old inline CREATE TABLE in New, before schema_migrations existed: its
// rows must survive and gain created_at.
func TestMigrateAdoptsPreMigrationsDatabase(t *testing.T) {
	db := scratchDB(t)
	if _, err := db.Exec(`
CREATE TABLE todos (
	id    TEXT PRIMARY KEY,
	title TEXT NOT NULL,
	done  BOOLEAN NOT NULL DEFAULT false
);
INSERT INTO todos (id, title, done) VALUES ('legacy', 'from before migrations', true)`); err != nil {
		t.Fatalf("create legacy schema: %v", err)
	}

	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	wantAllApplied(t, db)

	var title string
	var done bool
	var createdAt time.Time
	if err := db.QueryRow(`SELECT title, done, created_at FROM todos WHERE id = 'legacy'`).Scan(&title, &done, &createdAt); err != nil {
		t.Fatalf("read legacy row: %v", err)
	}
	if title != "from before migrations" || !done || createdAt.IsZero() {
		t.Fatalf("legacy row after migrate: title=%q done=%v created_at=%v", title, done, createdAt)
	}
}

// TestMigrateConcurrentStartup mirrors the API and cmd/consumer starting
// at once against a fresh database: every caller must succeed and each
// migration must be applied exactly once.
func TestMigrateConcurrentStartup(t *testing.T) {
	db := scratchDB(t)
	const callers = 5
	errs := make(chan error, callers)
	var wg sync.WaitGroup
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- Migrate(db)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent Migrate: %v", err)
		}
	}
	wantAllApplied(t, db)
}

func TestMigrateRefusesNewerSchema(t *testing.T) {
	db := scratchDB(t)
	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO schema_migrations (version, name) VALUES (999, '999_from_a_newer_binary.sql')`); err != nil {
		t.Fatalf("record future migration: %v", err)
	}
	if err := Migrate(db); err == nil {
		t.Fatal("Migrate should refuse a schema newer than its own migrations")
	}
}
