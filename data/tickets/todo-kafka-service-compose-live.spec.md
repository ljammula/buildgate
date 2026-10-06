# Ticket: Add GET /health/deps, verified against real Postgres, Kafka and Redis

This is an existing repo.

## Goal

Target repo: the todo-kafka-service fixture
(`testdata/fixtures/todo-kafka-service`) (main.go at the repo root,
`pgstore.Store` for the durable side, a Kafka publisher/consumer, a
Redis cache and idempotency store, and `docker-compose.yml` declaring
`postgres`, `kafka` and `redis` as dependency services). This ticket is
`scripts/live-compose.sh`'s fixture: its `Verify-Command` runs `make
verify-integration`, which assumes the three services are already
reachable (via `BG_SERVICE_*` inside a sandboxed run) and runs real
round-trip tests against them (`integration_test.go`). It cannot pass
unless the sandboxed run has a live, reachable Postgres, Kafka and Redis;
there is no fake fallback.

Add `GET /health/deps`, a small liveness endpoint that reports whether
each dependency is currently reachable.

## Required changes

**`main.go`**:

1. Add `GET /health/deps`, returning `200` with a JSON body
   `{"postgres": true/false, "kafka": true/false, "redis": true/false}`
   reflecting a live reachability check of each dependency, not a cached
   value: for Postgres a trivial query (e.g. `SELECT 1`) on the pool
   `pgstore.Store` already holds; for Kafka whatever `kafka.go` already
   exposes for broker connectivity (add a minimal method there if nothing
   fits); for Redis a `PING` on the client the idempotency store holds.
2. Return `503` (with the same JSON body) if any dependency is
   unreachable.
3. Any method other than `GET` on this path: `405`.

## Out of scope

- No change to any existing endpoint's behavior.
- No change to `make verify`, `make verify-integration` or
  `docker-compose.yml`.

Allowed-Files: main.go, main_test.go, kafka.go, integration_test.go

Required-Changed-Files: main.go

Required-Content: /health/deps
Required-Content: "redis"

## Acceptance / verification

Verify-Command: GOFLAGS=-v make verify-integration

- Add at least two tests: `GET /health/deps` against the real Postgres,
  Kafka and Redis (wired the same way `integration_test.go` wires them,
  behind `//go:build integration`) returns `200` with all three fields
  `true`; `POST /health/deps` returns `405`. Do not tear down a real
  service mid-test to fake an unreachable case.
- All pre-existing tests continue to pass unchanged.
