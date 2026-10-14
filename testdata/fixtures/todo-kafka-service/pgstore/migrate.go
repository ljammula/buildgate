package pgstore

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"

	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database/postgres"
	"github.com/golang-migrate/migrate/v4/source/iofs"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

const migrationsTable = "todo_service_migrations"

// Migrate applies every pending migration in order.
func Migrate(db *sql.DB) error {
	return runMigrations(db, (*migrate.Migrate).Up)
}

// MigrateDown rolls back every applied migration. It is intended for explicit
// operator use; application startup expects the schema to be migrated already.
func MigrateDown(db *sql.DB) error {
	return runMigrations(db, (*migrate.Migrate).Down)
}

// MigrateForce sets the migration version without running SQL. Use it only
// after confirming the database schema matches version.
func MigrateForce(db *sql.DB, version int) error {
	if version < -1 || version == 0 {
		return fmt.Errorf("migration version must be -1 or positive, got %d", version)
	}
	return runMigrations(db, func(m *migrate.Migrate) error {
		return m.Force(version)
	})
}

func runMigrations(db *sql.DB, run func(*migrate.Migrate) error) error {
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("get migration connection: %w", err)
	}
	database, err := postgres.WithConnection(ctx, conn, &postgres.Config{
		MigrationsTable:       migrationsTable,
		MultiStatementEnabled: false,
	})
	if err != nil {
		conn.Close()
		return fmt.Errorf("configure Postgres migrations: %w", err)
	}
	source, err := iofs.New(migrationFiles, "migrations")
	if err != nil {
		database.Close()
		return fmt.Errorf("open embedded migrations: %w", err)
	}
	m, err := migrate.NewWithInstance("iofs", source, "postgres", database)
	if err != nil {
		source.Close()
		database.Close()
		return fmt.Errorf("create migrator: %w", err)
	}
	err = run(m)
	sourceErr, databaseErr := m.Close()
	if errors.Is(err, migrate.ErrNoChange) {
		err = nil
	}
	if err != nil {
		return fmt.Errorf("run database migrations: %w", err)
	}
	if sourceErr != nil || databaseErr != nil {
		return errors.Join(sourceErr, databaseErr)
	}
	return nil
}
