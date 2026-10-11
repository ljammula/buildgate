# Spec

## Problem

The `middleware` package has no reusable middleware for requiring a request header on a route or router group. Applications currently have to implement this validation themselves, which can lead to inconsistent handling of missing, empty, or whitespace-only header values and inconsistent client-facing error messages.

## Scope

Add the exported `middleware.RequireHeader` function with signature `func RequireHeader(name string) func(http.Handler) http.Handler`.

For each request, `middleware.RequireHeader` must retrieve the requested header using `http.Header.Get` semantics, so header-name matching is case-insensitive. It must trim spaces from the retrieved value. If the trimmed value is non-empty, it must invoke the next handler with the request unchanged. If the trimmed value is empty, including when the header is absent, it must not invoke the next handler and must return status `400 Bad Request` with the exact plain-text body `missing required header: <Name>\n`, where `<Name>` is `http.CanonicalHeaderKey(name)`.

Add table-driven coverage in the `middleware` package for a non-empty present header, an absent header, an empty or whitespace-only header, and a lower-case `name` argument.

## Non-goals

- Validating the header value beyond trimming spaces and checking whether the result is empty.
- Normalizing, replacing, or otherwise mutating the request or its headers when the header is accepted.
- Adding configurable status codes, error formats, fallback values, or header-value predicates.
- Changing router behavior or adding a requirement for headers to routes that do not opt into `middleware.RequireHeader`.
- Adding new dependencies or changing unrelated middleware behavior.

## Affected services and packages

The `middleware` package is affected through the new exported `middleware.RequireHeader` API and its request/response behavior. Consumers that use the middleware can apply the requirement to any standard `net/http` handler chain. No external service, persistent data, or wire protocol is changed.

## Acceptance criteria

1. `middleware.RequireHeader("X-API-Version")` invokes the next handler for a request whose `X-API-Version` value is non-empty after trimming, and the next handler receives the original request unchanged.
2. `middleware.RequireHeader("X-API-Version")` rejects a request with no matching header by returning status `400 Bad Request`, writing exactly `missing required header: X-Api-Version\n`, and never invoking the next handler.
3. `middleware.RequireHeader("X-API-Version")` rejects both an explicitly empty value and a value containing only spaces after trimming with status `400 Bad Request`, the same exact canonical-name body, and no call to the next handler.
4. `middleware.RequireHeader("x-api-version")` matches a request carrying `X-API-Version` with a non-empty value, demonstrating case-insensitive header lookup, and uses `X-Api-Version` in the missing-header body when the value is not available.
5. Running the `middleware` package's table-driven tests exercises `middleware.RequireHeader` for the present, absent, empty/whitespace-only, and lower-case-name cases and verifies the corresponding next-handler, status, and exact-body outcomes.

## Risks

- The behavior depends on the standard library definitions of `http.Header.Get`, `http.CanonicalHeaderKey`, and whitespace trimming. If those semantics are bypassed or replaced, case-insensitive lookup or the required canonical error text can differ from the contract.
- The middleware must preserve the standard `net/http` handler contract and avoid mutating an accepted request. If it clones or changes the request, downstream handlers may observe different identity or header state; if it calls the next handler after rejection, routes intended to be protected can execute.
- The exact response body, including its trailing newline and canonical header name, is client-visible. Any extra text or alternate capitalization will make callers that rely on the documented error format unable to recognize the response.

## Open questions

None.
