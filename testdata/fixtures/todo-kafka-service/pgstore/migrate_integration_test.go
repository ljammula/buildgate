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

func appliedVersion(t *testing.T, db *sql.DB) (int, bool) {
	t.Helper()
	var version int
	var dirty bool
	if err := db.QueryRow(`SELECT version, dirty FROM todo_service_migrations`).Scan(&version, &dirty); err != nil {
		t.Fatalf("read todo_service_migrations: %v", err)
	}
	return version, dirty
}

func wantAllApplied(t *testing.T, db *sql.DB) {
	t.Helper()
	version, dirty := appliedVersion(t, db)
	versions := migrationVersions(t)
	if version != versions[len(versions)-1] || dirty {
		t.Fatalf("migration state version=%d dirty=%v, want version=%d dirty=false", version, dirty, versions[len(versions)-1])
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

func TestMigrateDownRollsBackAllVersions(t *testing.T) {
	db := scratchDB(t)
	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if err := MigrateDown(db); err != nil {
		t.Fatalf("MigrateDown: %v", err)
	}
	var todosAbsent bool
	if err := db.QueryRow(`SELECT to_regclass('todos') IS NULL`).Scan(&todosAbsent); err != nil {
		t.Fatalf("check rolled-back table: %v", err)
	}
	if !todosAbsent {
		t.Fatal("todos table still exists after rolling back all migrations")
	}
	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate after rollback: %v", err)
	}
	wantAllApplied(t, db)
}

func TestMigrateForceRecoversDirtyVersion(t *testing.T) {
	db := scratchDB(t)
	if _, err := db.Exec(`CREATE TABLE todos (title TEXT NOT NULL)`); err != nil {
		t.Fatalf("create incompatible legacy schema: %v", err)
	}
	if err := Migrate(db); err == nil {
		t.Fatal("Migrate should fail when migration 2 cannot create its index")
	}
	version, dirty := appliedVersion(t, db)
	if version != 2 || !dirty {
		t.Fatalf("migration state version=%d dirty=%v, want version=2 dirty=true", version, dirty)
	}
	var hasCreatedAt bool
	if err := db.QueryRow(`SELECT EXISTS (
		SELECT 1 FROM information_schema.columns
		WHERE table_name = 'todos' AND column_name = 'created_at'
	)`).Scan(&hasCreatedAt); err != nil {
		t.Fatalf("check failed migration side effects: %v", err)
	}
	if hasCreatedAt {
		t.Fatal("failed migration left created_at behind")
	}
	if err := MigrateForce(db, 1); err != nil {
		t.Fatalf("MigrateForce: %v", err)
	}
	if _, err := db.Exec(`ALTER TABLE todos ADD COLUMN id TEXT`); err != nil {
		t.Fatalf("repair legacy schema: %v", err)
	}
	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate after force: %v", err)
	}
	wantAllApplied(t, db)
}

// TestMigrateAdoptsPreMigrationsDatabase covers a database created before
// the migration runner existed: its rows must survive and gain created_at.
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
	if _, err := db.Exec(`UPDATE todo_service_migrations SET version = 999, dirty = false`); err != nil {
		t.Fatalf("record future migration: %v", err)
	}
	if err := Migrate(db); err == nil {
		t.Fatal("Migrate should refuse a schema newer than its own migrations")
	}
	version, dirty := appliedVersion(t, db)
	if version != 999 || dirty {
		t.Fatalf("migration state version=%d dirty=%v, want version=999 dirty=false", version, dirty)
	}
}
