// Package pgstore is the durable, Postgres-backed side of this service:
// the write target cmd/consumer materializes TodoCreated events into. Kept
// as its own importable package, separate from package main at the repo
// root, because both the API's own integration tests and cmd/consumer
// (a second, separate main package) need it, and a package named "main"
// cannot be imported from elsewhere.
package pgstore

import (
	"database/sql"
	"fmt"
	"time"

	_ "github.com/lib/pq"
)

// Todo is pgstore's own row shape -- deliberately not the API's Todo type
// in package main (which this package cannot import), since persistence
// and the HTTP response shape are different concerns that happen to look
// alike today.
type Todo struct {
	ID        string
	Title     string
	Done      bool
	CreatedAt time.Time
}

// Store is a connection pool against one Postgres database whose schema
// Migrate has already brought up to date.
type Store struct {
	db *sql.DB
}

// New opens a connection pool against dsn and applies any pending
// migrations (see Migrate). dsn is a standard postgres:// connection string, e.g.
// "postgres://todo:todo@localhost:5433/todo?sslmode=disable".
func New(dsn string) (*Store, error) {
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, fmt.Errorf("open postgres: %w", err)
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping postgres: %w", err)
	}
	if err := Migrate(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// List returns every todo, oldest first.
func (s *Store) List() ([]Todo, error) {
	rows, err := s.db.Query(`SELECT id, title, done, created_at FROM todos ORDER BY created_at, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []Todo{}
	for rows.Next() {
		var t Todo
		if err := rows.Scan(&t.ID, &t.Title, &t.Done, &t.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// AddWithID inserts a todo under an id the caller already generated (the
// API generates it once, publishes it in the Kafka event, and this is the
// consumer writing the same id back) rather than letting Postgres assign
// one, so the id a client saw in the POST response is the same id that
// eventually lands durably. ON CONFLICT DO NOTHING makes this idempotent
// against Kafka's own at-least-once redelivery -- cmd/consumer relies on
// that to safely retry a message it isn't sure committed. A zero createdAt
// means "unknown" and records the insert time instead.
func (s *Store) AddWithID(id, title string, createdAt time.Time) error {
	at := sql.NullTime{Time: createdAt, Valid: !createdAt.IsZero()}
	_, err := s.db.Exec(
		`INSERT INTO todos (id, title, done, created_at) VALUES ($1, $2, false, COALESCE($3, now())) ON CONFLICT (id) DO NOTHING`,
		id, title, at,
	)
	return err
}

func (s *Store) Get(id string) (Todo, bool, error) {
	var t Todo
	t.ID = id
	err := s.db.QueryRow(`SELECT title, done, created_at FROM todos WHERE id = $1`, id).Scan(&t.Title, &t.Done, &t.CreatedAt)
	if err == sql.ErrNoRows {
		return Todo{}, false, nil
	}
	if err != nil {
		return Todo{}, false, err
	}
	return t, true, nil
}

func (s *Store) Update(id, title string, done bool) (Todo, bool, error) {
	t := Todo{ID: id, Title: title, Done: done}
	err := s.db.QueryRow(
		`UPDATE todos SET title = $1, done = $2 WHERE id = $3 RETURNING created_at`,
		title, done, id,
	).Scan(&t.CreatedAt)
	if err == sql.ErrNoRows {
		return Todo{}, false, nil
	}
	if err != nil {
		return Todo{}, false, err
	}
	return t, true, nil
}

func (s *Store) Delete(id string) (bool, error) {
	res, err := s.db.Exec(`DELETE FROM todos WHERE id = $1`, id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}
