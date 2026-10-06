// Package cache is the Redis read-through cache in front of the durable
// todo list: the API (main.go) reads through it, and cmd/consumer
// invalidates it after each Kafka event it writes into Postgres. Both
// processes need DurableListKey, so it lives here rather than in either
// main package -- the same reason package event exists.
package cache

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/redis/go-redis/v9"

	"todo-service/pgstore"
)

// redisCallTimeout caps each cache GET/SET, so an unreachable Redis adds
// at most this much to a read that Postgres then serves.
const redisCallTimeout = 100 * time.Millisecond

// DurableListKey holds the JSON-encoded result of pgstore.Store.List.
const DurableListKey = "todos:durable:list"

// Lister is the read ListCache sits in front of; pgstore.Store satisfies it.
type Lister interface {
	List() ([]pgstore.Todo, error)
}

// ListCache serves List from Redis when it can and from next otherwise.
//
// Staleness is bounded by TTL, not eliminated: a List that reads Postgres
// just before the consumer inserts a row and invalidates, then writes its
// (now old) result back, leaves that old result cached until TTL expires.
// Redis being down or slow only ever costs the cache, never the read:
// each Redis call is capped at redisCallTimeout, every Redis error falls
// through to next, and a failed GET skips the write-back.
type ListCache struct {
	rdb  *redis.Client
	next Lister
	ttl  time.Duration
}

func NewListCache(rdb *redis.Client, next Lister, ttl time.Duration) *ListCache {
	return &ListCache{rdb: rdb, next: next, ttl: ttl}
}

func (c *ListCache) List() ([]pgstore.Todo, error) {
	getCtx, cancel := context.WithTimeout(context.Background(), redisCallTimeout)
	cached, err := c.rdb.Get(getCtx, DurableListKey).Bytes()
	cancel()
	redisUp := err == nil || errors.Is(err, redis.Nil)
	if err == nil {
		var todos []pgstore.Todo
		if err := json.Unmarshal(cached, &todos); err == nil {
			return todos, nil
		}
		log.Printf("cache: discard undecodable %s: %v", DurableListKey, err)
	} else if !errors.Is(err, redis.Nil) {
		log.Printf("cache: get %s: %v -- reading postgres", DurableListKey, err)
	}

	todos, err := c.next.List()
	if err != nil {
		return nil, err
	}
	if !redisUp {
		return todos, nil
	}
	if body, err := json.Marshal(todos); err == nil {
		setCtx, cancel := context.WithTimeout(context.Background(), redisCallTimeout)
		defer cancel()
		if err := c.rdb.Set(setCtx, DurableListKey, body, c.ttl).Err(); err != nil {
			log.Printf("cache: set %s: %v", DurableListKey, err)
		}
	}
	return todos, nil
}

// Connect opens a client against addr (host:port) and pings it, so a
// wrong or unreachable address fails at startup rather than on the first
// request. Redis is required by both the API and cmd/consumer.
func Connect(addr string) (*redis.Client, error) {
	rdb := newClient(addr)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := rdb.Ping(ctx).Err(); err != nil {
		rdb.Close()
		return nil, fmt.Errorf("ping redis at %s: %w", addr, err)
	}
	return rdb, nil
}

// newClient builds the one Redis client configuration every caller uses.
// ContextTimeoutEnabled makes go-redis honour a context's deadline
// (redisCallTimeout, for instance); without it, go-redis ignores the
// deadline and waits out its own multi-second read timeouts and retries.
func newClient(addr string) *redis.Client {
	return redis.NewClient(&redis.Options{Addr: addr, ContextTimeoutEnabled: true})
}

// InvalidateDurableList drops the cached durable list so the next List
// reads Postgres. Capped at redisCallTimeout so an unresponsive Redis
// can't stall the consumer's Kafka loop, which calls it per message.
func InvalidateDurableList(ctx context.Context, rdb *redis.Client) error {
	ctx, cancel := context.WithTimeout(ctx, redisCallTimeout)
	defer cancel()
	return rdb.Del(ctx, DurableListKey).Err()
}
