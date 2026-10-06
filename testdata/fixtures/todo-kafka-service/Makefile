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
# BG_SERVICE_REDIS -- each service's address on the run's private network
# (the worker is given addresses and resolves no service name) -- and
# forwards the ports docker-compose.yml publishes (5433, 9094, 6380) from
# the worker's own localhost.
#
# Resolution order for PG_ADDR and REDIS_ADDR:
#   1. PG_ADDR / REDIS_ADDR, if the caller set it explicitly -- wins even
#      when BG_SERVICE_* is also set.
#   2. BG_SERVICE_POSTGRES / BG_SERVICE_REDIS, if set, paired with its
#      BG_SERVICE_*_PORT when factoryd set one, else 5432 / 6379.
#   3. localhost:5433 / localhost:6380 -- docker-compose.yml's published
#      ports, for local dev against a manually started compose stack.
#
# Kafka is KAFKA_BROKERS if set, else localhost:9094 everywhere, never
# BG_SERVICE_KAFKA: a Kafka client connects twice, first to the address it
# was given and then to the address the broker advertises for that
# listener. The listener behind BG_SERVICE_KAFKA advertises the name
# "kafka", which a sandboxed worker cannot resolve, so the second
# connection fails. The published listener advertises localhost:9094,
# which is the forwarded port in the sandbox and the mapped port on a
# developer's machine.
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
