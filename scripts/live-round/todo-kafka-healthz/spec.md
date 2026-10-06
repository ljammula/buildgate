# Spec

## Problem

The root HTTP API has no dependency-free liveness endpoint. Operators and platform probes need a small, deterministic way to confirm that the API process is serving requests without requiring Postgres, Kafka, or Redis to be available. Without this endpoint, a liveness probe must target an existing business route or infer process health from dependency-backed behavior.

## Scope

Add `/healthz` to the root HTTP API. `main.go` must register it beside the existing routes. A GET request must synchronously return HTTP 200 with the JSON object `{"status":"ok"}` and `Content-Type: application/json`; every method other than GET must return HTTP 405.

This is a local, bounded response whose result is needed immediately by the caller, so it uses the synchronous delivery choice and performs no asynchronous or Kafka-backed work. The response is constant and needs no cache. The only new infrastructure is the HTTP endpoint itself: no consumer, topic, table, column, cache key, configuration variable, or external dependency is added. The endpoint makes no upstream calls, so there are no dependency timeouts, fallback responses, or partial results to define. It does not alter stored data, events, migrations, or compatibility with older rows, events, or service versions. Handler coverage for this behavior belongs in `main_test.go`.

## Non-goals

- Reporting the readiness or health of Postgres, Kafka, Redis, or any other dependency.
- Changing the behavior, status codes, or response formats of existing routes.
- Adding authentication, rate limiting, metrics, caching, configuration, persistence, migrations, or a new process.
- Publishing events, warming caches, or performing any work in the background for a health request.
- Defining a readiness endpoint or a dependency-specific diagnostic response.

## Affected services and packages

- The API process in package `main`, including the root HTTP route registration in `main.go` and its handler behavior.
- The handler tests in `main_test.go`.
- No behavior or data changes to `cmd/consumer`, `pgstore`, `cache`, `event`, Kafka, Postgres, or Redis.

## Acceptance criteria

1. `main.go` registers `/healthz` on the existing root HTTP API, and a request to the exact path `GET /healthz` reaches that endpoint rather than a 404 or an existing todo route.
2. For `GET /healthz`, the root API returns HTTP 200, sets `Content-Type` to `application/json`, and returns a JSON document representing exactly the object `{"status":"ok"}` with no additional fields.
3. For every HTTP method other than GET, including `HEAD`, `POST`, `PUT`, `PATCH`, `DELETE`, and `OPTIONS`, a request to `/healthz` returns HTTP 405.
4. `GET /healthz` returns the specified 200 response when Postgres, Kafka, and Redis are unavailable or unconfigured, and handling the request makes no connection, query, publish, cache, or other call to any of those services.
5. Running the handler tests in `main_test.go` without Postgres, Kafka, or Redis available observes the successful GET status/body/content type and the 405 result for a non-GET method, so those endpoint behaviors are detected independently of the external services.

## Risks

- The endpoint is deliberately a liveness signal, not a readiness signal. If an operator uses it to decide whether the API can serve dependency-backed todo operations, the probe can remain successful while a required dependency is down and traffic may still receive failures from other routes.
- The endpoint must remain on the existing root route set. If it is registered under a different prefix or router, probes aimed at `/healthz` will receive 404 and the liveness contract will be unusable.
- The no-dependency guarantee applies to handling `/healthz`; if existing API process startup independently requires an external service, that pre-existing startup behavior can still prevent the process from serving the endpoint. Treating the new handler as a replacement for startup or readiness checks would therefore give an incomplete health signal.

## Open questions

None.
