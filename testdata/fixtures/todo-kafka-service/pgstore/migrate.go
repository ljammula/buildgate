package pgstore

import (
	"database/sql"
	"embed"
	"fmt"
	"path"
	"sort"
	"strconv"
	"strings"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

// migrationLockID is the pg_advisory_xact_lock key Migrate holds while it
// runs. The API and cmd/consumer both call New at startup, often at the
// same moment against a fresh database; the lock makes the second one wait
// and then find every migration already applied, instead of both racing to
// run the same DDL.
const migrationLockID = 7_311_2026

type migration struct {
	version int
	name    string
	sql     string
}

// loadMigrations reads migrations/NNN_name.sql in version order. Versions
// must be unique, start at 1 and have no gaps, so a file added out of order
// or with a typo'd prefix fails loudly at startup instead of being applied
// in a surprising position.
func loadMigrations() ([]migration, error) {
	entries, err := migrationFiles.ReadDir("migrations")
	if err != nil {
		return nil, fmt.Errorf("read migrations: %w", err)
	}
	var out []migration
	for _, e := range entries {
		prefix, _, ok := strings.Cut(e.Name(), "_")
		version, err := strconv.Atoi(prefix)
		if !ok || err != nil || version <= 0 {
			return nil, fmt.Errorf("migration %q: name must be NNN_description.sql", e.Name())
		}
		body, err := migrationFiles.ReadFile(path.Join("migrations", e.Name()))
		if err != nil {
			return nil, fmt.Errorf("read migration %q: %w", e.Name(), err)
		}
		out = append(out, migration{version: version, name: e.Name(), sql: string(body)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].version < out[j].version })
	for i, m := range out {
		if m.version != i+1 {
			return nil, fmt.Errorf("migration %q: expected version %d (versions must be 1..N with no gaps or duplicates)", m.name, i+1)
		}
	}
	return out, nil
}

// Migrate applies every migration not yet recorded in schema_migrations,
// in one transaction: Postgres DDL is transactional, so a failing
// migration leaves the schema exactly as it was, with nothing recorded.
func Migrate(db *sql.DB) error {
	migrations, err := loadMigrations()
	if err != nil {
		return err
	}

	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("begin migration transaction: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`SELECT pg_advisory_xact_lock($1)`, migrationLockID); err != nil {
		return fmt.Errorf("acquire migration lock: %w", err)
	}
	if _, err := tx.Exec(`
CREATE TABLE IF NOT EXISTS schema_migrations (
	version    INTEGER PRIMARY KEY,
	name       TEXT NOT NULL,
	applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
)`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	var current int
	if err := tx.QueryRow(`SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&current); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	if current > len(migrations) {
		return fmt.Errorf("database schema is at version %d, newer than this binary's latest migration %d", current, len(migrations))
	}

	for _, m := range migrations[current:] {
		if _, err := tx.Exec(m.sql); err != nil {
			return fmt.Errorf("apply migration %s: %w", m.name, err)
		}
		if _, err := tx.Exec(`INSERT INTO schema_migrations (version, name) VALUES ($1, $2)`, m.version, m.name); err != nil {
			return fmt.Errorf("record migration %s: %w", m.name, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit migrations: %w", err)
	}
	return nil
}
