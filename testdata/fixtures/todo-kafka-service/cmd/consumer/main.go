// Command consumer reads TodoCreated events off Kafka and materializes
// them into Postgres -- the worker half of POST /todos -> Kafka ->
// Postgres -- then invalidates the API's Redis cache of the durable list.
// Run alongside the API and the docker-compose kafka/postgres/redis
// services; see the repo's own Makefile "verify" target.
package main

import (
	"context"
	"encoding/json"
	"log"
	"os"
	"strings"

	"github.com/segmentio/kafka-go"

	"todo-service/cache"
	"todo-service/event"
	"todo-service/pgstore"
)

func main() {
	brokers := strings.Split(getenv("KAFKA_BROKERS", "localhost:9094"), ",")
	topic := getenv("KAFKA_TOPIC", event.TodosCreatedTopic)
	dsn := os.Getenv("TODO_DATABASE_URL")
	if dsn == "" {
		log.Fatal("TODO_DATABASE_URL is required (e.g. postgres://todo:todo@localhost:5433/todo?sslmode=disable)")
	}

	redisAddr := os.Getenv("REDIS_ADDR")
	if redisAddr == "" {
		log.Fatal("REDIS_ADDR is required (e.g. localhost:6380)")
	}
	rdb, err := cache.Connect(redisAddr)
	if err != nil {
		log.Fatalf("connect to redis: %v", err)
	}
	defer rdb.Close()

	store, err := pgstore.New(dsn)
	if err != nil {
		log.Fatalf("connect to postgres: %v", err)
	}
	defer store.Close()

	reader := kafka.NewReader(kafka.ReaderConfig{
		Brokers: brokers,
		Topic:   topic,
		GroupID: "todo-consumer",
		// Without this, a consumer that joins the group before the topic
		// exists (created on the API's first publish, since nothing
		// here creates it up front) can stabilize its group generation
		// with zero partitions and never notice the topic appear --
		// silently consuming nothing forever. Found live: exactly this
		// sequence, against the real docker-compose broker.
		WatchPartitionChanges: true,
	})
	defer reader.Close()

	log.Printf("todo-consumer: reading topic %q from %v, writing to postgres", topic, brokers)

	ctx := context.Background()
	for {
		msg, err := reader.FetchMessage(ctx)
		if err != nil {
			log.Fatalf("fetch message: %v", err)
		}

		var evt event.TodoCreated
		if err := json.Unmarshal(msg.Value, &evt); err != nil {
			// A malformed message can never become well-formed by
			// redelivery -- commit past it rather than retrying forever.
			log.Printf("skip malformed message at offset %d: %v", msg.Offset, err)
			if err := reader.CommitMessages(ctx, msg); err != nil {
				log.Printf("commit malformed message at offset %d: %v", msg.Offset, err)
			}
			continue
		}

		if err := store.AddWithID(evt.ID, evt.Title, evt.CreatedAt); err != nil {
			// Leave the offset uncommitted: this message (and everything
			// after it, for this partition) is redelivered on restart.
			// Safe because AddWithID is idempotent (ON CONFLICT DO
			// NOTHING) -- see pgstore.Store.AddWithID's own comment.
			log.Printf("persist todo %s: %v -- offset not committed, will retry", evt.ID, err)
			continue
		}

		// Logged, not retried: the row is already durable, and the
		// cache's own TTL bounds how long GET /todos/durable can miss it.
		if err := cache.InvalidateDurableList(ctx, rdb); err != nil {
			log.Printf("invalidate durable list cache after todo %s: %v", evt.ID, err)
		}

		if err := reader.CommitMessages(ctx, msg); err != nil {
			log.Printf("commit offset for todo %s: %v", evt.ID, err)
			continue
		}
		log.Printf("persisted todo %s (%q)", evt.ID, evt.Title)
	}
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
