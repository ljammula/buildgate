# AGENTS.md

A small Go todo API whose writes flow through Kafka into Postgres, with
Redis as cache and idempotency store (`README.md`). Two processes: the API
(`main.go`) and the consumer (`cmd/consumer`). `ARCHITECTURE.md` has the
design, and the README the services, configuration and endpoints.

## Commands

- Setup: none for unit tests beyond Go. Integration tests need the Postgres,
  Kafka and Redis services of `docker-compose.yml`.
- Build: `go build ./...`
- Vet: `go vet ./...`
- Test: `make test` (unit tests, needs nothing)
- Full suite: `make verify-integration`, against services that are already
  running (`BG_SERVICE_*`, or `PG_ADDR`, `KAFKA_BROKERS` and `REDIS_ADDR`).
  It is the `verify_command` in `.factory.yml`.
- `make verify` starts and removes the compose stack itself; it needs Docker.

## Rules for a change

From `README.md` and the `Makefile`:

- A migration is the next number in `pgstore/migrations/`
  (`NNN_description.sql`, no gaps). Never edit one that has shipped.
- Integration tests carry the `integration` build tag and run with `-p 1`:
  packages share one `todos` table.
