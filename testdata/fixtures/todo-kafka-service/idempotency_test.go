package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"todo-service/event"
)

// fakeIdempotency is an IdempotencyStore for unit tests -- no live Redis.
// The real Redis-backed store is exercised by integration_test.go.
type fakeIdempotency struct {
	records  map[string]idempotencyRecord
	claimErr error
}

func (f *fakeIdempotency) Claim(_ context.Context, key, title string) (*idempotencyRecord, error) {
	if f.claimErr != nil {
		return nil, f.claimErr
	}
	if rec, ok := f.records[key]; ok {
		return &rec, nil
	}
	f.records[key] = idempotencyRecord{Title: title}
	return nil, nil
}

func (f *fakeIdempotency) Complete(ctx context.Context, key string, rec idempotencyRecord) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.records[key] = rec
	return nil
}

func (f *fakeIdempotency) Release(_ context.Context, key string) error {
	delete(f.records, key)
	return nil
}

// countingPublisher records every event it's asked to publish, failing
// with err when set.
type countingPublisher struct {
	published []event.TodoCreated
	err       error
}

func (p *countingPublisher) Publish(_ context.Context, evt event.TodoCreated) error {
	if p.err != nil {
		return p.err
	}
	p.published = append(p.published, evt)
	return nil
}

func withIdempotencyFakes(t *testing.T) (*fakeIdempotency, *countingPublisher) {
	t.Helper()
	resetStore()
	store, pub := &fakeIdempotency{records: map[string]idempotencyRecord{}}, &countingPublisher{}
	prevStore, prevPub := idempotency, publisher
	idempotency, publisher = store, pub
	t.Cleanup(func() { idempotency, publisher = prevStore, prevPub })
	return store, pub
}

func postWithKey(key, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/todos", bytes.NewBufferString(body))
	req.Header.Set("Idempotency-Key", key)
	w := httptest.NewRecorder()
	handleTodos(w, req)
	return w
}

func TestIdempotentPostReplaysWithoutRepublishing(t *testing.T) {
	_, pub := withIdempotencyFakes(t)

	first := postWithKey("k1", `{"title":"buy milk"}`)
	if first.Code != http.StatusCreated {
		t.Fatalf("first POST expected 201, got %d", first.Code)
	}
	var created Todo
	json.NewDecoder(first.Body).Decode(&created)

	retry := postWithKey("k1", `{"title":"buy milk"}`)
	if retry.Code != http.StatusCreated {
		t.Fatalf("retry expected 201, got %d", retry.Code)
	}
	if retry.Header().Get("Idempotent-Replayed") != "true" {
		t.Fatal("retry should be marked Idempotent-Replayed")
	}
	var replayed Todo
	json.NewDecoder(retry.Body).Decode(&replayed)
	if replayed != created {
		t.Fatalf("retry returned %+v, want the original %+v", replayed, created)
	}
	if len(pub.published) != 1 {
		t.Fatalf("expected exactly 1 todo.created event, got %d", len(pub.published))
	}
	if len(store.List()) != 1 {
		t.Fatalf("expected 1 todo in the store, got %d", len(store.List()))
	}
}

func TestIdempotentPostDistinctKeysCreateDistinctTodos(t *testing.T) {
	_, pub := withIdempotencyFakes(t)

	postWithKey("k1", `{"title":"same"}`)
	postWithKey("k2", `{"title":"same"}`)
	if len(pub.published) != 2 {
		t.Fatalf("expected 2 events for 2 keys, got %d", len(pub.published))
	}
}

func TestIdempotentPostKeyReusedForDifferentTitle(t *testing.T) {
	_, pub := withIdempotencyFakes(t)

	postWithKey("k1", `{"title":"first"}`)
	w := postWithKey("k1", `{"title":"second"}`)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d", w.Code)
	}
	if len(pub.published) != 1 {
		t.Fatalf("expected no second event, got %d events", len(pub.published))
	}
}

func TestIdempotentPostWhileFirstRequestInFlight(t *testing.T) {
	fake, pub := withIdempotencyFakes(t)
	fake.records["k1"] = idempotencyRecord{Title: "buy milk"} // claimed, not completed

	w := postWithKey("k1", `{"title":"buy milk"}`)
	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d", w.Code)
	}
	if len(pub.published) != 0 {
		t.Fatalf("expected no event, got %d", len(pub.published))
	}
}

func TestIdempotentPostPublishFailureReleasesKey(t *testing.T) {
	fake, pub := withIdempotencyFakes(t)
	pub.err = errors.New("broker down")

	w := postWithKey("k1", `{"title":"buy milk"}`)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("expected 502, got %d", w.Code)
	}
	if _, held := fake.records["k1"]; held {
		t.Fatal("a failed publish must release the key")
	}

	pub.err = nil
	retry := postWithKey("k1", `{"title":"buy milk"}`)
	if retry.Code != http.StatusCreated || retry.Header().Get("Idempotent-Replayed") != "" {
		t.Fatalf("retry after a failed publish should create fresh: code %d, replayed %q", retry.Code, retry.Header().Get("Idempotent-Replayed"))
	}
	if len(pub.published) != 1 {
		t.Fatalf("expected 1 event after the retry, got %d", len(pub.published))
	}
}

func TestIdempotentPostStoreUnavailable(t *testing.T) {
	fake, pub := withIdempotencyFakes(t)
	fake.claimErr = errors.New("redis down")

	w := postWithKey("k1", `{"title":"buy milk"}`)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", w.Code)
	}
	if len(pub.published) != 0 {
		t.Fatalf("expected no event, got %d", len(pub.published))
	}
}

func TestIdempotentPostKeyTooLong(t *testing.T) {
	_, pub := withIdempotencyFakes(t)

	w := postWithKey(strings.Repeat("k", maxIdempotencyKeyLen+1), `{"title":"buy milk"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
	if len(pub.published) != 0 {
		t.Fatalf("expected no event, got %d", len(pub.published))
	}
}

// TestPostStampsCreatedAt: the event carries the API's own creation time,
// which the consumer stores as created_at -- without it the consumer falls
// back to its insert time and a backlog reorders the durable list.
func TestPostStampsCreatedAt(t *testing.T) {
	_, pub := withIdempotencyFakes(t)
	before := time.Now()
	handler("", "POST", `{"title":"buy milk"}`)
	after := time.Now()

	if len(pub.published) != 1 {
		t.Fatalf("expected 1 event, got %d", len(pub.published))
	}
	if at := pub.published[0].CreatedAt; at.Before(before) || at.After(after) {
		t.Fatalf("event CreatedAt = %v, want between %v and %v", at, before, after)
	}
}

func TestPublishTimeoutFitsInsideClaimTTL(t *testing.T) {
	if publishTimeout*2 > idempotencyClaimTTL {
		t.Fatalf("publishTimeout %v must be well under idempotencyClaimTTL %v", publishTimeout, idempotencyClaimTTL)
	}
}

// cancelAwarePublisher fails like kafka-go does when its context is
// cancelled mid-write.
type cancelAwarePublisher struct{ countingPublisher }

func (p *cancelAwarePublisher) Publish(ctx context.Context, evt event.TodoCreated) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return p.countingPublisher.Publish(ctx, evt)
}

// TestIdempotentPostPublishOutlivesClientDisconnect: a client that has
// already gone must not cancel the publish, or the handler would release
// the key for an event the broker may already hold.
func TestIdempotentPostPublishOutlivesClientDisconnect(t *testing.T) {
	fake, _ := withIdempotencyFakes(t)
	pub := &cancelAwarePublisher{}
	publisher = pub
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	req := httptest.NewRequest(http.MethodPost, "/todos", bytes.NewBufferString(`{"title":"buy milk"}`)).WithContext(ctx)
	req.Header.Set("Idempotency-Key", "k1")
	handleTodos(httptest.NewRecorder(), req)

	if len(pub.published) != 1 {
		t.Fatalf("expected the publish to go through, got %d events", len(pub.published))
	}
	if rec, ok := fake.records["k1"]; !ok || rec.Todo == nil {
		t.Fatalf("key should hold the completed todo, got %+v (present=%v)", rec, ok)
	}
}

func TestPostWithoutKeyNeverTouchesIdempotencyStore(t *testing.T) {
	fake, _ := withIdempotencyFakes(t)
	fake.claimErr = errors.New("must not be called")

	w := handler("", "POST", `{"title":"buy milk"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d", w.Code)
	}
}

// TestIdempotentPostCompletesAfterClientDisconnect: the client's context is
// cancelled by the time the todo is recorded under its key. The record must
// still be written, or a later retry would publish a duplicate.
func TestIdempotentPostCompletesAfterClientDisconnect(t *testing.T) {
	fake, _ := withIdempotencyFakes(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	req := httptest.NewRequest(http.MethodPost, "/todos", bytes.NewBufferString(`{"title":"buy milk"}`)).WithContext(ctx)
	req.Header.Set("Idempotency-Key", "k1")
	handleTodos(httptest.NewRecorder(), req)

	if rec, ok := fake.records["k1"]; !ok || rec.Todo == nil {
		t.Fatalf("key should hold the completed todo, got %+v (present=%v)", rec, ok)
	}
}
