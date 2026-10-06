package main

import (
	"context"

	"todo-service/event"
)

// EventPublisher is the seam between handleTodos' POST case and the real
// Kafka producer, so main_test.go's unit tests never need a live broker --
// the same injectable-dependency shape this repo's own reason for existing
// (as a software-factory fixture) already uses throughout, e.g.
// software-factory's queueRunner.
type EventPublisher interface {
	Publish(ctx context.Context, evt event.TodoCreated) error
}

// publisher defaults to noopPublisher so `go test ./...` and any run
// without KAFKA_BROKERS set behave exactly as before Kafka was added.
// main wires in a real KafkaPublisher only when KAFKA_BROKERS is set.
var publisher EventPublisher = noopPublisher{}

type noopPublisher struct{}

func (noopPublisher) Publish(context.Context, event.TodoCreated) error { return nil }
