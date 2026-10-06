# Todo Service — Architecture

## Repo layout

- Describe the repo layout here.

## Verification

- `make verify-integration`: buildgate's `verify_command`. Runs against the Postgres, Kafka and Redis sidecars buildgate starts from `docker-compose.yml` (`BG_SERVICE_*`); the sandboxed worker has no Docker, so `make verify`, which starts the stack itself, can't run there.

## Known deviations

- None yet.
