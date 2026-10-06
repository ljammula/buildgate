// Package event holds the wire format shared between the API (main.go,
// which publishes) and cmd/consumer (which reads) -- the two processes
// can't share package main directly, so this is their contract.
package event

import "time"

// TodoCreated is published to TodosCreatedTopic on a successful
// POST /todos, and consumed by cmd/consumer to materialize the same todo
// into Postgres.
type TodoCreated struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	// CreatedAt is when the API accepted the todo, stored as the row's
	// created_at so a consumer backlog or replay can't reorder todos. Zero
	// for events published before the field existed; the consumer then
	// falls back to the insert time.
	CreatedAt time.Time `json:"created_at,omitzero"`
}

// TodosCreatedTopic is the Kafka topic TodoCreated events are published to.
const TodosCreatedTopic = "todos.created"
