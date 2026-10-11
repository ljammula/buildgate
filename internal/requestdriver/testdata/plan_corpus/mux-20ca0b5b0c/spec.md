# Spec

## Problem

When a request matches a route's path, host and queries but not its method, `Router.ServeHTTP` in `mux.go` answers `405 Method Not Allowed` with no `Allow` header. RFC 9110 requires a 405 response to carry an `Allow` header listing the methods the resource supports, and clients and API tools rely on it. The router already knows the answer, but nothing exposes it: `getAllMethodsForRoute` in `middleware.go` only looks at a router's top-level routes and is unexported.

## Scope

Add an exported method `Router.AllowedMethods` in a new file `allow.go` that reports, for a request, which HTTP methods the router would accept for it, subrouters included. Make `Router.ServeHTTP` in `mux.go` set the `Allow` response header from it whenever the match failed with `ErrMethodMismatch`, both for the built-in 405 response and before a custom `MethodNotAllowedHandler` runs.

The scope includes doc comments for the new method and an added sentence in the doc comment of the `MethodNotAllowedHandler` field, and tests in a new file `allow_test.go`.

## Non-goals

- Changing which requests get a 405 or a 404, or the 405 response body or status.
- Answering `OPTIONS` requests automatically, or adding `HEAD` where a route allows `GET`.
- Changing `CORSMethodMiddleware` or `getAllMethodsForRoute` in `middleware.go`.
- Setting `Allow` on any response other than a method mismatch.
- A `README.md` change: no README section covers 405 responses, so the documentation is the doc comments.

## Affected services and packages

The `github.com/gorilla/mux` package: a new file `allow.go`, `Router.ServeHTTP` and the `Router.MethodNotAllowedHandler` field comment in `mux.go`, and a new test file `allow_test.go`. No dependency is added.

## Acceptance criteria

1. `allow.go` declares `func (r *Router) AllowedMethods(req *http.Request) []string`. Its candidates are every method named by a `Methods` matcher on any route that `r.Walk` visits (routes of subrouters included). It returns the candidates `m` for which a copy of `req` with its method set to `m` matches, meaning `r.Match` returns `true` and leaves `MatchErr` nil.
2. `Router.AllowedMethods` in `allow.go` returns each method once, sorted in increasing byte order (`sort.Strings`), and returns a non-nil slice of length 0 when no candidate matches or the router has no routes.
3. `Router.AllowedMethods` in `allow.go` does not modify `req` (its `Method` is unchanged afterwards) and does not call any handler or middleware.
4. With routes `/items` `Methods("POST", "PUT")`, `/items` `Methods("GET")` and `/other` `Methods("DELETE")` on one router, `Router.AllowedMethods` in `allow.go` returns `["GET", "POST", "PUT"]` for a `DELETE /items` request and also for a `GET /items` request, and an empty slice for `GET /missing`.
5. `Router.AllowedMethods` in `allow.go` honours every other matcher: with a route `/search` `Methods("GET").Queries("q", "{q}")` it returns `["GET"]` for `POST /search?q=x` and an empty slice for `POST /search`; with a subrouter `PathPrefix("/api").Subrouter()` holding `/users/{id:[0-9]+}` `Methods("PATCH", "GET")` and `/users/{id:[0-9]+}` `Methods("DELETE")` it returns `["DELETE", "GET", "PATCH"]` for `PUT /api/users/7` and an empty slice for `PUT /api/users/abc`; with `PathPrefix("/ro").Methods("GET", "HEAD").Subrouter()` holding `/docs` (no methods) it returns `["GET", "HEAD"]` for `POST /ro/docs`.
6. For a request whose path is served by a route without a method matcher, `Router.AllowedMethods` in `allow.go` returns every candidate, because every candidate matches: on a router whose only method matchers are `Methods("GET")` and `Methods("POST")` on other paths plus a route `/open` with no methods, it returns `["GET", "POST"]` for `GET /open`.
7. `Router.AllowedMethods` in `allow.go` gives the same results when `NotFoundHandler` and `MethodNotAllowedHandler` are set on the router: those fallbacks never count as a match.
8. `Router.ServeHTTP` in `mux.go`: when matching the request ends with `ErrMethodMismatch` and `AllowedMethods` for the request is not empty, the response has exactly one `Allow` header whose value is the allowed methods joined with `", "` (comma and one space). With the routes of criterion 4, `DELETE /items` gets status 405 and `Allow: GET, POST, PUT`.
9. `Router.ServeHTTP` in `mux.go` sets that header before it calls the handler, so a custom `MethodNotAllowedHandler` reads the value with `w.Header().Get("Allow")` and may replace or delete it; the custom handler is still called exactly as before and its status and body are unchanged.
10. `Router.ServeHTTP` in `mux.go` sets no `Allow` header on a response that is not a method mismatch: a matched request (200), an unknown path (404, with or without `NotFoundHandler`) and a path-clean or strict-slash redirect have no `Allow` header.
11. The doc comment of `Router.AllowedMethods` in `allow.go` states the rule of criterion 1 and the consequence in criterion 6; the comment on the `MethodNotAllowedHandler` field in `mux.go` says the `Allow` header is already set when the handler is called.
12. A new file `allow_test.go` in package `mux` covers criteria 2 to 10 with table-driven tests that send requests through `Router.ServeHTTP` with `httptest.NewRecorder`. All existing tests pass unchanged, `go test ./...` passes and `gofmt -l .` prints nothing.

## Risks

- `AllowedMethods` runs the router's matchers once per candidate method, so a `MatcherFunc` with side effects is called more often on a 405 than before. The cost is paid only on method mismatches.
- A custom `MethodNotAllowedHandler` that already sets `Allow` with `Header().Add` would now send two values; `Header().Set` is unaffected. The field comment tells handler authors the header is preset.
- For a path served by a method-less route the result lists the methods the router knows about, not every method the route would accept. This is deliberate (criterion 6) and never reaches a response, because such a request does not produce a 405.
- The probe request must be a copy; changing `req.Method` in place would be visible to the handler that later serves the request.

## Open questions

None.
