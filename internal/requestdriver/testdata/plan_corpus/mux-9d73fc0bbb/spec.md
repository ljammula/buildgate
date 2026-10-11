# Spec

## Problem

`test_helpers.go` exports `SetURLVars` so a handler can be unit-tested without a router: the test injects the route variables into the request and calls the handler directly. A handler that calls `mux.CurrentRoute(r)` (to read the route name, its path template or its metadata) or `mux.CurrentRouter(r)` (to build URLs with `Get(name).URL(...)`) has no such helper. Both functions read unexported context keys, so outside the package the only way to test such a handler is to build a router and send the request through `ServeHTTP`.

## Scope

Add two exported test helpers to `test_helpers.go`, next to `SetURLVars`: `SetCurrentRoute`, which returns a copy of a request for which `CurrentRoute` returns a given route, and `SetCurrentRouter`, which does the same for `CurrentRouter` and a router. Each leaves the request it was given untouched and keeps whatever the other two helpers already stored, so the three can be combined in any order.

The scope includes a doc comment on each new function in the style of `SetURLVars` (what it does, and that it is meant for testing), tests in a new file `test_helpers_test.go`, and a short paragraph in the "Testing Handlers" section of `README.md`.

## Non-goals

- Changing `SetURLVars`, `Vars`, `CurrentRoute`, `CurrentRouter` or `Router.ServeHTTP`.
- A helper that sets a `RouteMatch`, or one that runs route matching.
- Exporting the context keys.
- Moving the helpers to a separate package.

## Affected services and packages

The `github.com/gorilla/mux` package: `test_helpers.go` (two new exported functions), a new test file `test_helpers_test.go`, and the "Testing Handlers" section of `README.md`. No dependency is added.

## Acceptance criteria

1. `test_helpers.go` declares `func SetCurrentRoute(r *http.Request, route *Route) *http.Request`. For a request `req` and a route `route`, `CurrentRoute(SetCurrentRoute(req, route))` returns exactly `route` (the same pointer).
2. `test_helpers.go` declares `func SetCurrentRouter(r *http.Request, router *Router) *http.Request`. For a request `req` and a router `router`, `CurrentRouter(SetCurrentRouter(req, router))` returns exactly `router` (the same pointer).
3. `SetCurrentRoute` and `SetCurrentRouter` in `test_helpers.go` each return a request that is a different `*http.Request` from the one passed in, and do not change the one passed in: after `got := SetCurrentRoute(req, route)`, `CurrentRoute(req)` still returns what it returned before the call (`nil` for a fresh request), and the same holds for `SetCurrentRouter` and `CurrentRouter`.
4. `SetCurrentRoute` in `test_helpers.go` sets only the route: on a fresh request `req`, `CurrentRouter(SetCurrentRoute(req, route))` is `nil`. `SetCurrentRouter` sets only the router: `CurrentRoute(SetCurrentRouter(req, router))` is `nil`.
5. Calling `SetCurrentRoute` in `test_helpers.go` on a request that already carries a route replaces it: `CurrentRoute(SetCurrentRoute(SetCurrentRoute(req, a), b))` returns `b`. A `nil` route clears it: `CurrentRoute(SetCurrentRoute(SetCurrentRoute(req, a), nil))` returns `nil`. `SetCurrentRouter` behaves the same way for routers, including `CurrentRouter(SetCurrentRouter(SetCurrentRouter(req, a), nil)) == nil`.
6. `SetURLVars`, `SetCurrentRoute` and `SetCurrentRouter` in `test_helpers.go` can be applied to one request in any order without losing a value: after all three, `Vars` returns the map given to `SetURLVars`, `CurrentRoute` returns the route and `CurrentRouter` returns the router.
7. Each of `SetCurrentRoute` and `SetCurrentRouter` in `test_helpers.go` has a doc comment that starts with the function name, says which accessor (`CurrentRoute` or `CurrentRouter`) will return the value, and says the function should only be used for testing.
8. A new file `test_helpers_test.go` in package `mux` tests criteria 1 to 6, including a test that calls an `http.HandlerFunc` directly (no router `ServeHTTP`) with a request prepared by the helpers and checks that the handler reads the route name through `CurrentRoute(r).GetName()` and the router through `CurrentRouter(r)`.
9. The "Testing Handlers" section of `README.md` ends, immediately before the `## Full Example` heading, with a paragraph that names `mux.SetCurrentRoute` and `mux.SetCurrentRouter` and says they do for `mux.CurrentRoute` and `mux.CurrentRouter` what `mux.SetURLVars` does for `mux.Vars`. No other part of `README.md` changes.
10. `go test ./...` passes and `gofmt -l .` prints nothing.

## Risks

- A request that is passed through `Router.ServeHTTP` after one of the helpers was applied gets the matched route and the serving router from the router, which replace the injected ones. That is the existing behaviour of the request context and is not changed.
- `CurrentRoute` and `CurrentRouter` must keep returning `nil`, not panic, after a `nil` value was injected; an implementation that stores an untyped value under the context key would break their type assertion.
- `README.md` is also edited by other changes in other sections; the edit here is confined to the end of "Testing Handlers".

## Open questions

None.
