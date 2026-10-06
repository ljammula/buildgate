//go:build integration

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/segmentio/kafka-go"

	"todo-service/cache"
	"todo-service/event"
	"todo-service/pgstore"
)

// This file backs `make verify-integration` (see the Makefile): real
// Postgres/Kafka round trips through the same DurableLister (main.go)
// and EventPublisher (publisher.go) seams the rest of this repo already
// uses to keep `go test ./...` Docker-free, wired here to their real
// implementations -- pgstore.Store and KafkaPublisher -- instead of the
// fakes/noop those unit tests use. Excluded from the default `go test
// ./...` run (no "integration" build tag).
//
// TestMain dials all three addresses with a short timeout before running
// any test, so a caller that forgot to launch Postgres/Kafka/Redis (or a
// sandboxed run whose ComposeServicesLifecycle sidecars failed to come
// up) gets one clear line naming which service is unreachable, not a
// hang or a raw driver stack trace.

const dialTimeout = 5 * time.Second

func TestMain(m *testing.M) {
	pgAddr := os.Getenv("PG_ADDR")
	kafkaAddr := firstBroker(os.Getenv("KAFKA_BROKERS"))
	redisAddr := os.Getenv("REDIS_ADDR")
	if pgAddr == "" || kafkaAddr == "" || redisAddr == "" {
		fmt.Fprintln(os.Stderr, "verify-integration: PG_ADDR, KAFKA_BROKERS and REDIS_ADDR must all be set -- run via `make verify-integration`, not `go test` directly")
		os.Exit(1)
	}
	for _, svc := range []struct{ name, addr string }{
		{"postgres", pgAddr},
		{"kafka", kafkaAddr},
		{"redis", redisAddr},
	} {
		conn, err := net.DialTimeout("tcp", svc.addr, dialTimeout)
		if err != nil {
			fmt.Fprintf(os.Stderr, "verify-integration: %s unreachable at %s: %v -- was it launched (docker compose, or a sandboxed run's ComposeServicesLifecycle) before this target ran?\n", svc.name, svc.addr, err)
			os.Exit(1)
		}
		conn.Close()
	}
	os.Exit(m.Run())
}

func firstBroker(brokers string) string {
	if brokers == "" {
		return ""
	}
	return strings.SplitN(brokers, ",", 2)[0]
}

// TestDurableStoreRoundTrip writes one row through the real pgstore.Store
// -- the same type main.go wires in as DurableLister when
// TODO_DATABASE_URL is set -- and reads it back through the DurableLister
// interface, exercising the real seam rather than a fake.
func TestDurableStoreRoundTrip(t *testing.T) {
	dsn := requireEnv(t, "TODO_DATABASE_URL")

	ds, err := pgstore.New(dsn)
	if err != nil {
		t.Fatalf("connect to postgres at %s: %v", dsn, err)
	}
	// A cleanup, not a defer: defers run before t.Cleanup callbacks, so
	// the Delete below would otherwise hit a closed pool and leak the row.
	t.Cleanup(func() { ds.Close() })

	var lister DurableLister = ds

	id := fmt.Sprintf("verify-integration-%d", time.Now().UnixNano())
	const title = "integration round trip"
	if err := ds.AddWithID(id, title, time.Time{}); err != nil {
		t.Fatalf("AddWithID: %v", err)
	}
	t.Cleanup(func() { ds.Delete(id) })

	todos, err := lister.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, td := range todos {
		if td.ID == id && td.Title == title {
			return
		}
	}
	t.Fatalf("round-tripped todo %s not found in List(): %+v", id, todos)
}

// TestKafkaPublisherRoundTrip publishes one event through the real
// KafkaPublisher -- the same type main.go wires in as EventPublisher when
// KAFKA_BROKERS is set -- and reads it back with a plain Kafka reader,
// exercising the real seam rather than the noopPublisher unit tests use.
func TestKafkaPublisherRoundTrip(t *testing.T) {
	brokersEnv := requireEnv(t, "KAFKA_BROKERS")
	brokers := strings.Split(brokersEnv, ",")
	// A topic distinct from event.TodosCreatedTopic so this test never
	// collides with a real API/consumer pair also running against the
	// same broker, and unique per run: the reader below never commits, so
	// a fixed topic hands a rerun against a still-running broker the
	// previous run's message instead of this one's.
	topic := fmt.Sprintf("todo-service.verify-integration.%d", time.Now().UnixNano())

	// Not created up front: KafkaPublisher.Publish must create it on
	// demand, the path a fresh broker exercises.

	pub := NewKafkaPublisher(brokers, topic)
	defer pub.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	want := event.TodoCreated{
		ID:        fmt.Sprintf("verify-integration-%d", time.Now().UnixNano()),
		Title:     "kafka round trip",
		CreatedAt: time.Now().UTC(),
	}
	if err := pub.Publish(ctx, want); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	reader := kafka.NewReader(kafka.ReaderConfig{
		Brokers: brokers,
		Topic:   topic,
		GroupID: "todo-service-verify-integration",
		// See cmd/consumer's own comment on this field: without it, a
		// reader that joins the group before the topic exists can
		// stabilize with zero partitions and never notice the topic
		// appear.
		WatchPartitionChanges: true,
	})
	defer reader.Close()

	msg, err := reader.FetchMessage(ctx)
	if err != nil {
		t.Fatalf("FetchMessage: %v", err)
	}

	var got event.TodoCreated
	if err := json.Unmarshal(msg.Value, &got); err != nil {
		t.Fatalf("unmarshal round-tripped message: %v", err)
	}
	if got.ID != want.ID || got.Title != want.Title || !got.CreatedAt.Equal(want.CreatedAt) {
		t.Fatalf("round-tripped event mismatch: want %+v got %+v", want, got)
	}
}

// TestDurableStatsHTTPThroughListCache verifies the stats endpoint uses the
// production route and the same Redis-backed list cache as /todos/durable,
// rather than a separate aggregate query or cache.
func TestDurableStatsHTTPThroughListCache(t *testing.T) {
	rdb := connectRedis(t)
	ctx := context.Background()
	ds, err := pgstore.New(requireEnv(t, "TODO_DATABASE_URL"))
	if err != nil {
		t.Fatalf("connect to postgres: %v", err)
	}
	t.Cleanup(func() { ds.Close() })
	if err := cache.InvalidateDurableList(ctx, rdb); err != nil {
		t.Fatalf("clear cache: %v", err)
	}
	t.Cleanup(func() { cache.InvalidateDurableList(ctx, rdb) })

	base, err := ds.List()
	if err != nil {
		t.Fatalf("read baseline: %v", err)
	}
	ids := []string{
		fmt.Sprintf("verify-integration-stats-a-%d", time.Now().UnixNano()),
		fmt.Sprintf("verify-integration-stats-b-%d", time.Now().UnixNano()),
		fmt.Sprintf("verify-integration-stats-c-%d", time.Now().UnixNano()),
	}
	for i, id := range ids {
		if err := ds.AddWithID(id, fmt.Sprintf("stats %d", i), time.Time{}); err != nil {
			t.Fatalf("AddWithID: %v", err)
		}
		t.Cleanup(func() { ds.Delete(id) })
	}
	if _, _, err := ds.Update(ids[0], "stats 0", true); err != nil {
		t.Fatalf("mark first done: %v", err)
	}
	if _, _, err := ds.Update(ids[2], "stats 2", true); err != nil {
		t.Fatalf("mark third done: %v", err)
	}

	lister := cache.NewListCache(rdb, ds, time.Minute)
	old := durableStore
	durableStore = lister
	t.Cleanup(func() { durableStore = old })
	mux := http.NewServeMux()
	registerRoutes(mux)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/todos/durable/stats", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("stats GET expected 200, got %d", w.Code)
	}
	var fields map[string]json.RawMessage
	if err := json.NewDecoder(w.Body).Decode(&fields); err != nil {
		t.Fatalf("decode stats: %v", err)
	}
	if len(fields) != 2 {
		t.Fatalf("expected exactly total and done fields, got %v", fields)
	}
	var total, done int
	if err := json.Unmarshal(fields["total"], &total); err != nil {
		t.Fatalf("decode total: %v", err)
	}
	if err := json.Unmarshal(fields["done"], &done); err != nil {
		t.Fatalf("decode done: %v", err)
	}
	baseDone := 0
	for _, todo := range base {
		if todo.Done {
			baseDone++
		}
	}
	if total != len(base)+len(ids) || done != baseDone+2 {
		t.Fatalf("stats = total %d done %d, want total %d done %d", total, done, len(base)+len(ids), baseDone+2)
	}

	w = httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/todos/durable/stats", nil))
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("stats POST expected 405, got %d", w.Code)
	}
}

// TestDurableEndpointFiltering exercises the endpoint's filtering over the
// real Postgres-backed read-through cache. The filtered requests deliberately
// follow the cold complete read so they must use the same cached complete list.
func TestDurableEndpointFiltering(t *testing.T) {
	rdb := connectRedis(t)
	ctx := context.Background()
	ds, err := pgstore.New(requireEnv(t, "TODO_DATABASE_URL"))
	if err != nil {
		t.Fatalf("connect to postgres: %v", err)
	}
	t.Cleanup(func() { ds.Close() })

	if err := cache.InvalidateDurableList(ctx, rdb); err != nil {
		t.Fatalf("clear cache: %v", err)
	}
	t.Cleanup(func() { cache.InvalidateDurableList(ctx, rdb) })

	suffix := time.Now().UnixNano()
	openID := fmt.Sprintf("verify-integration-filter-open-%d", suffix)
	doneID := fmt.Sprintf("verify-integration-filter-done-%d", suffix)
	created := time.Now().Add(-time.Second)
	if err := ds.AddWithID(openID, "filter open", created); err != nil {
		t.Fatalf("AddWithID open: %v", err)
	}
	if err := ds.AddWithID(doneID, "filter done", created.Add(time.Second)); err != nil {
		t.Fatalf("AddWithID done: %v", err)
	}
	if _, _, err := ds.Update(doneID, "filter done", true); err != nil {
		t.Fatalf("Update done: %v", err)
	}
	t.Cleanup(func() { ds.Delete(openID); ds.Delete(doneID) })

	prev := durableStore
	durableStore = cache.NewListCache(rdb, ds, time.Minute)
	t.Cleanup(func() { durableStore = prev })

	request := func(target string) []pgstore.Todo {
		req := httptest.NewRequest(http.MethodGet, target, nil)
		w := httptest.NewRecorder()
		handleTodosDurable(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("GET %s: status %d body %q", target, w.Code, w.Body.String())
		}
		var todos []pgstore.Todo
		if err := json.NewDecoder(w.Body).Decode(&todos); err != nil {
			t.Fatalf("GET %s: decode: %v", target, err)
		}
		return todos
	}
	contains := func(todos []pgstore.Todo, id string) (int, bool) {
		for i, todo := range todos {
			if todo.ID == id {
				return i, true
			}
		}
		return -1, false
	}

	complete := request("/todos/durable")
	if rdb.Exists(ctx, cache.DurableListKey).Val() != 1 {
		t.Fatal("cold complete read did not populate the durable-list cache key")
	}
	open := request("/todos/durable?done=false")
	done := request("/todos/durable?done=true")
	_, ok := contains(open, openID)
	if !ok {
		t.Fatalf("done=false response omitted %s: %+v", openID, open)
	}
	if _, ok := contains(open, doneID); ok {
		t.Fatalf("done=false response contained completed row: %+v", open)
	}
	_, ok = contains(done, doneID)
	if !ok {
		t.Fatalf("done=true response omitted %s: %+v", doneID, done)
	}
	if _, ok := contains(done, openID); ok {
		t.Fatalf("done=true response contained open row: %+v", done)
	}
	for _, todo := range open {
		if todo.Done {
			t.Fatalf("done=false response contained a completed row: %+v", open)
		}
	}
	for _, todo := range done {
		if !todo.Done {
			t.Fatalf("done=true response contained an open row: %+v", done)
		}
	}
	completeOpenAt, _ := contains(complete, openID)
	completeDoneAt, _ := contains(complete, doneID)
	if completeOpenAt >= completeDoneAt {
		t.Fatalf("durable order is unexpected: open=%d done=%d", completeOpenAt, completeDoneAt)
	}

	warmComplete := request("/todos/durable")
	if _, ok := contains(warmComplete, openID); !ok {
		t.Fatalf("warm complete response omitted %s", openID)
	}
	if _, ok := contains(warmComplete, doneID); !ok {
		t.Fatalf("warm complete response omitted %s", doneID)
	}
	if len(warmComplete) != len(complete) {
		t.Fatalf("filtered reads replaced complete cache: cold=%d warm=%d", len(complete), len(warmComplete))
	}
}

func requireEnv(t *testing.T, key string) string {
	t.Helper()
	v := os.Getenv(key)
	if v == "" {
		t.Fatalf("%s not set; run via `make verify-integration`", key)
	}
	return v
}

func connectRedis(t *testing.T) *redis.Client {
	t.Helper()
	rdb, err := cache.Connect(requireEnv(t, "REDIS_ADDR"))
	if err != nil {
		t.Fatalf("connect to redis: %v", err)
	}
	t.Cleanup(func() { rdb.Close() })
	return rdb
}

// TestRedisIdempotencyStoreRoundTrip drives the real Redis-backed
// IdempotencyStore main wires in through a claim, a concurrent claim, a
// completion, a replay and a release.
func TestRedisIdempotencyStoreRoundTrip(t *testing.T) {
	rdb := connectRedis(t)
	store := redisIdempotencyStore{rdb: rdb}
	ctx := context.Background()
	key := fmt.Sprintf("verify-integration-%d", time.Now().UnixNano())
	t.Cleanup(func() { store.Release(ctx, key) })

	prior, err := store.Claim(ctx, key, "buy milk")
	if err != nil || prior != nil {
		t.Fatalf("first Claim: prior=%+v err=%v, want a fresh claim", prior, err)
	}
	if ttl := rdb.TTL(ctx, idempotencyKey(key)).Val(); ttl <= 0 || ttl > idempotencyClaimTTL {
		t.Fatalf("in-flight claim TTL = %v, want (0, %v]", ttl, idempotencyClaimTTL)
	}

	prior, err = store.Claim(ctx, key, "buy milk")
	if err != nil || prior == nil || prior.Todo != nil {
		t.Fatalf("Claim while in flight: prior=%+v err=%v, want a pending record", prior, err)
	}

	todo := Todo{ID: "abc", Title: "buy milk"}
	if err := store.Complete(ctx, key, idempotencyRecord{Title: "buy milk", Todo: &todo}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if ttl := rdb.TTL(ctx, idempotencyKey(key)).Val(); ttl <= idempotencyClaimTTL {
		t.Fatalf("completed key TTL = %v, want the longer %v", ttl, idempotencyTTL)
	}
	prior, err = store.Claim(ctx, key, "buy milk")
	if err != nil || prior == nil || prior.Todo == nil || *prior.Todo != todo {
		t.Fatalf("Claim after Complete: prior=%+v err=%v, want the stored todo", prior, err)
	}

	if err := store.Release(ctx, key); err != nil {
		t.Fatalf("Release: %v", err)
	}
	prior, err = store.Claim(ctx, key, "buy milk")
	if err != nil || prior != nil {
		t.Fatalf("Claim after Release: prior=%+v err=%v, want a fresh claim", prior, err)
	}
}

// TestDurableListCacheReadThroughAndInvalidate drives cache.ListCache
// against real Redis and Postgres: a miss reads Postgres, a hit is served
// from Redis even after Postgres changes, and InvalidateDurableList -- what
// cmd/consumer calls after each write -- makes the next read see it.
func TestDurableListCacheReadThroughAndInvalidate(t *testing.T) {
	rdb := connectRedis(t)
	ctx := context.Background()
	ds, err := pgstore.New(requireEnv(t, "TODO_DATABASE_URL"))
	if err != nil {
		t.Fatalf("connect to postgres: %v", err)
	}
	t.Cleanup(func() { ds.Close() })
	if err := cache.InvalidateDurableList(ctx, rdb); err != nil {
		t.Fatalf("clear cache: %v", err)
	}
	t.Cleanup(func() { cache.InvalidateDurableList(ctx, rdb) })

	lister := cache.NewListCache(rdb, ds, time.Minute)
	contains := func(todos []pgstore.Todo, id string) bool {
		for _, td := range todos {
			if td.ID == id {
				return true
			}
		}
		return false
	}

	first := fmt.Sprintf("verify-integration-cache-a-%d", time.Now().UnixNano())
	if err := ds.AddWithID(first, "cached", time.Time{}); err != nil {
		t.Fatalf("AddWithID: %v", err)
	}
	t.Cleanup(func() { ds.Delete(first) })
	todos, err := lister.List()
	if err != nil || !contains(todos, first) {
		t.Fatalf("List on a cold cache: err=%v, want %s in %+v", err, first, todos)
	}
	if n := rdb.Exists(ctx, cache.DurableListKey).Val(); n != 1 {
		t.Fatal("a miss should populate the cache")
	}

	second := fmt.Sprintf("verify-integration-cache-b-%d", time.Now().UnixNano())
	if err := ds.AddWithID(second, "not yet visible", time.Time{}); err != nil {
		t.Fatalf("AddWithID: %v", err)
	}
	t.Cleanup(func() { ds.Delete(second) })
	todos, err = lister.List()
	if err != nil || contains(todos, second) {
		t.Fatalf("List on a warm cache: err=%v, want the cached result without %s", err, second)
	}

	if err := cache.InvalidateDurableList(ctx, rdb); err != nil {
		t.Fatalf("InvalidateDurableList: %v", err)
	}
	todos, err = lister.List()
	if err != nil || !contains(todos, second) {
		t.Fatalf("List after invalidation: err=%v, want %s in %+v", err, second, todos)
	}
}
