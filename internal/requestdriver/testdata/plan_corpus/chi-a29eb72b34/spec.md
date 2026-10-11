# Spec

## Problem

HTML forms and some HTTP clients and proxies can only send `GET` and `POST`, so they cannot reach routes registered with `Put`, `Patch` or `Delete`. The `middleware` package has no way to let a `POST` request declare the method it stands for; `GetHead` in `middleware/get_head.go` is the only middleware that changes the method a request is routed by, and it only covers `HEAD`.

## Scope

Add a `MethodOverride` middleware in a new file `middleware/method_override.go`. For a `POST` request that carries the `X-HTTP-Method-Override` header with the value `PUT`, `PATCH` or `DELETE`, it rewrites the request method to that value before calling the next handler, so a chi router or subrouter that uses the middleware routes the request by the overriding method. Every other request passes through unchanged.

The scope also includes an exported constant for the header name, doc comments on both new identifiers, and tests in a new file `middleware/method_override_test.go`.

## Non-goals

- Reading the override from a form field, a query parameter or any header other than `X-HTTP-Method-Override`.
- Overriding the method of a request that is not `POST`, or overriding to any method other than `PUT`, `PATCH` and `DELETE`.
- Making the header name or the allowed methods configurable.
- Changing `GetHead`, the router in the repository root, `README.md`, `CHANGELOG.md` or any existing middleware file.
- Adding a dependency outside the Go standard library and `github.com/go-chi/chi/v5`.

## Affected services and packages

The `github.com/go-chi/chi/v5/middleware` package gains one exported function and one exported constant in two new files. The router package at the repository root is used but not changed. No external service, persistent data or runtime dependency is affected.

## Acceptance criteria

1. `middleware/method_override.go` declares, in package `middleware`, the exported untyped string constant `MethodOverrideHeader` with the exact value `"X-HTTP-Method-Override"`.
2. `middleware/method_override.go` declares `MethodOverride` with the exact signature `func MethodOverride(next http.Handler) http.Handler`, so it can be passed to `Use` and `With` of a `chi.Router`. The returned handler calls `next` exactly once for every request.
3. For a request whose method is `POST` and whose `X-HTTP-Method-Override` header, after trimming leading and trailing whitespace and converting to upper case, equals `PUT`, `PATCH` or `DELETE`, the handler returned by `MethodOverride` sets `r.Method` to that upper-case value before calling `next`. Header values `delete`, `Patch` and `  put  ` therefore give `next` the methods `DELETE`, `PATCH` and `PUT`.
4. In the case of criterion 3, `MethodOverride` removes the `X-HTTP-Method-Override` header from the request before calling `next`, so `r.Header.Get("X-HTTP-Method-Override")` returns `""` in `next`.
5. In the case of criterion 3, when `chi.RouteContext(r.Context())` is not nil, `MethodOverride` also sets that context's `RouteMethod` field to the same upper-case value. When it is nil (the middleware wraps a plain `http.Handler` outside a chi router), `MethodOverride` does not panic and still applies criteria 3 and 4.
6. For a `POST` request with no `X-HTTP-Method-Override` header, or with a value that after trimming and upper-casing is anything other than `PUT`, `PATCH` or `DELETE` (for example `GET`, `HEAD`, `POST`, `OPTIONS`, `TRACE`, `CONNECT`, `bogus` or the empty string), `MethodOverride` calls `next` with `r.Method` still `POST` and the header, if present, unchanged.
7. For a request whose method is not `POST` (for example `GET`, `HEAD`, `PUT`, `PATCH` or `DELETE`), `MethodOverride` calls `next` with `r.Method` and the `X-HTTP-Method-Override` header unchanged, whatever the header's value.
8. With `r := chi.NewRouter()`, `r.Use(middleware.MethodOverride)` and only `r.Delete("/items/{id}", h)` registered, a `POST /items/7` request with header `X-HTTP-Method-Override: DELETE` is served by `h` with status 200, `chi.URLParam(r, "id")` equal to `"7"` and `r.Method` equal to `"DELETE"`; a `POST /items/7` request without the header gets status 405; a `GET /items/7` request with the header gets status 405.
9. With `MethodOverride` installed by `Use` inside a subrouter created with `r.Route("/api", ...)` on a router that does not itself use the middleware, and only `Put("/things/{id}", h)` registered in that subrouter, a `POST /api/things/3` request with header `X-HTTP-Method-Override: put` is served by `h` with status 200 and `r.Method` equal to `"PUT"`, and the same request without the header gets status 405.
10. `middleware/method_override_test.go`, in package `middleware`, contains tests that cover criteria 3 to 9, and `go test ./...` and `go vet ./...` pass.

## Risks

- A method override lets a `POST` reach handlers that change or delete data. An application that relies on a proxy or firewall filtering by method can be bypassed once the middleware is installed; the doc comment on `MethodOverride` must say that it should only be used where `POST` callers are trusted to send `PUT`, `PATCH` and `DELETE`.
- The middleware mutates the incoming `*http.Request` in place, as `GetHead` mutates the route context. A middleware placed before it sees the original `POST`; one placed after it sees the overriding method, which affects loggers and method-based checks depending on their order.
- On a subrouter the route context already holds `POST` as its `RouteMethod` when the middleware runs, so routing depends on criterion 5; setting only `r.Method` would pass the top-level case and fail the subrouter case.

## Open questions

None.
