package main

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/redis/go-redis/v9"
)

// IdempotencyStore records POST /todos results by client-supplied
// Idempotency-Key, so a client retrying a request whose response it never
// saw gets the original todo back instead of publishing a second
// todo.created event. It's the seam between handleTodos and Redis, so
// main_test.go's unit tests never need a live Redis -- the same shape as
// EventPublisher in publisher.go.
type IdempotencyStore interface {
	// Claim reserves key for a request creating title. A nil record means
	// the caller now owns key and must Complete or Release it; a non-nil
	// record is what an earlier request with the same key left behind.
	Claim(ctx context.Context, key, title string) (*idempotencyRecord, error)
	Complete(ctx context.Context, key string, rec idempotencyRecord) error
	Release(ctx context.Context, key string) error
}

// idempotencyRecord is the value stored under one key. Todo is nil while
// the claiming request is still in flight.
type idempotencyRecord struct {
	Title string `json:"title"`
	Todo  *Todo  `json:"todo,omitempty"`
}

// idempotency is always set by main (Redis is required); unit tests
// replace it with a fake.
var idempotency IdempotencyStore

// idempotencyTTL is how long a completed key is remembered. A retry after
// that creates a new todo.
const idempotencyTTL = 24 * time.Hour

// maxIdempotencyKeyLen caps the Idempotency-Key header so a client can't
// make the service store arbitrarily large Redis keys.
const maxIdempotencyKeyLen = 255

// idempotencyClaimTTL bounds how long a crashed request's in-flight claim
// blocks retries of the same key.
const idempotencyClaimTTL = 30 * time.Second

// idempotencyCallTimeout caps each Redis call the store makes. Complete
// and Release run on a context with no deadline of their own (see
// handleTodos), so without this a stalled Redis would hold a 201 that is
// already decided for go-redis's full timeouts and retries.
const idempotencyCallTimeout = 2 * time.Second

type redisIdempotencyStore struct {
	rdb *redis.Client
}

func idempotencyKey(key string) string { return "idempotency:post-todos:" + key }

func (s redisIdempotencyStore) Claim(ctx context.Context, key, title string) (*idempotencyRecord, error) {
	ctx, cancel := context.WithTimeout(ctx, idempotencyCallTimeout)
	defer cancel()
	pending, err := json.Marshal(idempotencyRecord{Title: title})
	if err != nil {
		return nil, err
	}
	// SET NX, then GET on failure: another request could complete or
	// release the key between the two calls. Losing that race just
	// reports the key as taken, or retries the claim once it's gone.
	for range 2 {
		claimed, err := s.rdb.SetNX(ctx, idempotencyKey(key), pending, idempotencyClaimTTL).Result()
		if err != nil {
			return nil, err
		}
		if claimed {
			return nil, nil
		}
		body, err := s.rdb.Get(ctx, idempotencyKey(key)).Bytes()
		if errors.Is(err, redis.Nil) {
			continue
		}
		if err != nil {
			return nil, err
		}
		var rec idempotencyRecord
		if err := json.Unmarshal(body, &rec); err != nil {
			return nil, err
		}
		return &rec, nil
	}
	return nil, errors.New("idempotency key released and re-claimed concurrently")
}

func (s redisIdempotencyStore) Complete(ctx context.Context, key string, rec idempotencyRecord) error {
	ctx, cancel := context.WithTimeout(ctx, idempotencyCallTimeout)
	defer cancel()
	body, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	return s.rdb.Set(ctx, idempotencyKey(key), body, idempotencyTTL).Err()
}

func (s redisIdempotencyStore) Release(ctx context.Context, key string) error {
	ctx, cancel := context.WithTimeout(ctx, idempotencyCallTimeout)
	defer cancel()
	return s.rdb.Del(ctx, idempotencyKey(key)).Err()
}
