.PHONY: test verify verify-integration

# Fast unit tests only -- the in-memory Store with the noop publisher, no
# Docker required.
test:
	go test ./...

# Full suite: brings up the docker-compose Postgres, Kafka and Redis
# dependencies, waits for all three to be healthy, runs the Postgres-backed integration
# tests against them, then tears everything down. This is the target a
# sandboxed run's compose-services provisioner needs to be able to
# satisfy without the worker ever holding a Postgres/Kafka credential
# itself -- see docker-compose.yml.
verify: test
	docker compose up -d --wait postgres kafka redis
	PG_ADDR=localhost:5433 KAFKA_BROKERS=localhost:9094 REDIS_ADDR=localhost:6380 \
	TODO_DATABASE_URL="postgres://todo:todo@localhost:5433/todo?sslmode=disable" \
		go test -p 1 -tags=integration ./... ; \
		status=$$?; \
		docker compose down -v; \
		exit $$status

# Same build/vet/test as `verify`, but against a Postgres/Kafka/Redis the
# caller already launched -- this target never runs `docker compose
# up`/`down` itself. Exists for factoryd's sandboxed worker, which has no
# Docker socket and can never satisfy `verify` above: its
# ComposeServicesLifecycle instead launches this repo's own
# docker-compose.yml services as sidecars on a private network before the
# worker starts, then hands in BG_SERVICE_POSTGRES / BG_SERVICE_KAFKA /
# BG_SERVICE_REDIS -- aliases reachable on the ports below (postgres:5432,
# redis:6379, kafka:19092 -- see
# docker-compose.yml's own "x-bg-service-port" on the kafka service for
# why 19092, not the 9092 its ports: mapping declares), not the
# host-published ports docker-compose.yml maps those to for local dev
# (5433, 6380, 9094).
#
# Resolution order per address (PG_ADDR/KAFKA_BROKERS/REDIS_ADDR):
#   1. PG_ADDR / KAFKA_BROKERS / REDIS_ADDR, if the caller set it
#      explicitly -- wins even when BG_SERVICE_* is also set.
#   2. BG_SERVICE_POSTGRES / BG_SERVICE_KAFKA / BG_SERVICE_REDIS, if set --
#      the sandboxed alias, paired with its BG_SERVICE_*_PORT when factoryd
#      set one (it always does for kafka, via x-bg-service-port; postgres
#      and redis have no override and so get their ports:-derived 5432 /
#      6379), else this target's own hardcoded default.
#   3. localhost:5433 / localhost:9094 / localhost:6380 -- this repo's
#      docker-compose.yml host-published ports, for plain local dev
#      against a manually started compose stack.
#
# The integration test binary itself (see integration_test.go) dials
# all three addresses with a short timeout before running anything, so an
# unreachable service fails fast with a clear cause instead of hanging
# or surfacing a raw driver stack trace.
#
# -p 1 (here and in `verify`): packages share one todos table, and
# pgstore's tests TRUNCATE it, so they must not run alongside the root
# package's integration tests.
verify-integration:
	go build ./...
	go vet ./...
	go test ./...
	@pg_addr="$${PG_ADDR:-localhost:5433}"; \
	if [ -n "$$BG_SERVICE_POSTGRES" ] && [ -z "$$PG_ADDR" ]; then \
		pg_addr="$$BG_SERVICE_POSTGRES:$${BG_SERVICE_POSTGRES_PORT:-5432}"; \
	fi; \
	kafka_brokers="$${KAFKA_BROKERS:-localhost:9094}"; \
	if [ -n "$$BG_SERVICE_KAFKA" ] && [ -z "$$KAFKA_BROKERS" ]; then \
		kafka_brokers="$$BG_SERVICE_KAFKA:$${BG_SERVICE_KAFKA_PORT:-9092}"; \
	fi; \
	redis_addr="$${REDIS_ADDR:-localhost:6380}"; \
	if [ -n "$$BG_SERVICE_REDIS" ] && [ -z "$$REDIS_ADDR" ]; then \
		redis_addr="$$BG_SERVICE_REDIS:$${BG_SERVICE_REDIS_PORT:-6379}"; \
	fi; \
	echo "verify-integration: resolved PG_ADDR=$$pg_addr KAFKA_BROKERS=$$kafka_brokers REDIS_ADDR=$$redis_addr"; \
	PG_ADDR="$$pg_addr" \
	KAFKA_BROKERS="$$kafka_brokers" \
	REDIS_ADDR="$$redis_addr" \
	TODO_DATABASE_URL="postgres://todo:todo@$$pg_addr/todo?sslmode=disable" \
		go test -p 1 -tags=integration ./...
