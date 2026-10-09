# Spec

## Problem

The root HTTP API cannot say which service is answering. An operator or a platform probe that reaches the API through a shared gateway needs a small, deterministic way to confirm it has reached this service, without requiring Postgres, Kafka, or Redis to be available. Without such an endpoint the caller must infer the service from the shape of a business route's response.

## Scope

Add `/version` to the root HTTP API. `main.go` must register it beside the existing routes. A GET request must synchronously return HTTP 200 with the JSON object `{"service":"todo-kafka-service"}` and `Content-Type: application/json`; every method other than GET must return HTTP 405 with an `Allow: GET` header, as the existing `/healthz` endpoint does.

This is a local, bounded response whose result is needed immediately by the caller, so it uses the synchronous delivery choice and performs no asynchronous or Kafka-backed work. The response is constant and needs no cache. The only new infrastructure is the HTTP endpoint itself: no consumer, topic, table, column, cache key, configuration variable, or external dependency is added. The endpoint makes no upstream calls, so there are no dependency timeouts, fallback responses, or partial results to define. It does not alter stored data, events, migrations, or compatibility with older rows, events, or service versions. Handler coverage for this behavior belongs in `main_test.go`.

## Non-goals

- Reporting a build number, commit, release tag or any value that changes between builds.
- Changing the behavior, status codes, or response formats of existing routes, `/healthz` included.
- Adding authentication, rate limiting, metrics, caching, configuration, persistence, migrations, or a new process.
- Publishing events, warming caches, or performing any work in the background for a version request.

## Affected services and packages

- The API process in package `main`, including the root HTTP route registration in `main.go` and its handler behavior.
- The handler tests in `main_test.go`.
- No behavior or data changes to `cmd/consumer`, `pgstore`, `cache`, `event`, Kafka, Postgres, or Redis.

## Acceptance criteria

1. `main.go` registers `/version` on the existing root HTTP API, and a request to the exact path `GET /version` reaches that endpoint rather than a 404 or an existing route.
2. For `GET /version`, the root API returns HTTP 200, sets `Content-Type` to `application/json`, and returns a JSON document representing exactly the object `{"service":"todo-kafka-service"}` with no additional fields.
3. For every HTTP method other than GET, including `HEAD`, `POST`, `PUT`, `PATCH`, `DELETE`, and `OPTIONS`, a request to `/version` returns HTTP 405 with the header `Allow: GET`.
4. `GET /version` returns the specified 200 response when Postgres, Kafka, and Redis are unavailable or unconfigured, and handling the request makes no connection, query, publish, cache, or other call to any of those services.
5. Running the handler tests in `main_test.go` without Postgres, Kafka, or Redis available observes the successful GET status/body/content type and the 405 result with its `Allow` header for a non-GET method, so those endpoint behaviors are detected independently of the external services.

## Risks

- The endpoint names the service, not a build. A caller that needs to tell two deployed builds apart gets the same answer from both.
- The endpoint must remain on the existing root route set. If it is registered under a different prefix or router, callers aimed at `/version` will receive 404.
- The no-dependency guarantee applies to handling `/version`; if existing API process startup independently requires an external service, that pre-existing startup behavior can still prevent the process from serving the endpoint.

## Open questions

None.
