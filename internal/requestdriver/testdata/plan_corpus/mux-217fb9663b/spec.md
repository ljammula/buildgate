# Spec

## Problem

There is no direct way to ask a router what it serves. The README's "Walking Routes" section shows the workaround: call `Router.Walk` and, for every route, call `GetPathTemplate`, `GetMethods`, `GetQueriesTemplates` and the other accessors one by one, each returning an error that only means "not set". Every project that wants a route table for a startup log, a debug endpoint, generated API documentation or a test that pins its routes rewrites that loop, and most get subrouters wrong by listing the grouping route as if it were an endpoint.

## Scope

Add a route-listing feature in three parts that build on each other.

Part 1, the description of one route: an exported struct `RouteInfo` and a method `Route.Info` in a new file `routeinfo.go`. `RouteInfo` is a plain snapshot of what a route matches: name, methods, schemes, host template, path template, whether the path is a prefix, query templates, variable names and the build-only flag.

Part 2, the route table of a router, in the same file: `Router.Routes`, which returns the `RouteInfo` of every endpoint route of a router and its subrouters in `Walk` order, and `Router.WriteRoutes`, which writes that list to an `io.Writer` as an aligned text table.

Part 3, documentation: a runnable example `ExampleRouter_WriteRoutes` in a new file `example_routes_test.go` and a new "Listing Routes" section in `README.md`.

Tests for parts 1 and 2 go in a new file `routeinfo_test.go`.

## Non-goals

- Describing header matchers (`Headers`, `HeadersRegexp`), custom `MatcherFunc`s, middleware, handlers or metadata in `RouteInfo`.
- Changing `Router.Walk`, `WalkFunc` or any existing `Get*` accessor on `Route`.
- JSON tags, a `String` method or any output format other than the text table.
- Sorting or de-duplicating routes; the order is registration order.
- Combining several method or scheme matchers on one route into an intersection.

## Affected services and packages

The `github.com/gorilla/mux` package: new files `routeinfo.go`, `routeinfo_test.go` and `example_routes_test.go`, and `README.md` (table of contents and one new section). Existing source files are not changed. No dependency is added.

## Acceptance criteria

1. `routeinfo.go` declares the exported struct `RouteInfo` with exactly these fields, in this order: `Name string`, `Methods []string`, `Schemes []string`, `Host string`, `Path string`, `Prefix bool`, `Queries []string`, `Vars []string`, `BuildOnly bool`. The type and every field have a doc comment.
2. `routeinfo.go` declares `func (r *Route) Info() RouteInfo`. For a route without a build error it sets `Name` to the route name (`""` if none); `Host` to the host template (`""` if none); `Path` to the path template as `GetPathTemplate` returns it (`""` if none); `Prefix` to `true` only when the path was set with `PathPrefix`; `Queries` to the query templates as `GetQueriesTemplates` returns them (`"key=value"` form, in order; empty if none); `Vars` to the names `GetVarNames` returns (host, then path, then queries); `BuildOnly` to `true` only when `BuildOnly()` was called.
3. `Route.Info` in `routeinfo.go` sets `Methods` to the methods of the route's first method matcher, the same values `GetMethods` returns (upper-cased), and `Schemes` to the schemes of the route's first scheme matcher (lower-cased, in the order given). Each is empty (length 0) when the route has no such matcher. `Path("/m").Methods("GET", "POST").Methods("GET")` gives `Methods` `["GET", "POST"]`; `Path("/s").Schemes("https").Schemes("http", "https")` gives `Schemes` `["https"]`.
4. For the route `Host("{sub}.example.com").Path("/articles/{category}/{id:[0-9]+}").Methods("get", "PUT").Schemes("HTTPS", "http").Queries("page", "{page:[0-9]+}", "sort", "asc").Name("article")`, `Route.Info` in `routeinfo.go` returns `Name` `"article"`, `Methods` `["GET", "PUT"]`, `Schemes` `["https", "http"]`, `Host` `"{sub}.example.com"`, `Path` `"/articles/{category}/{id:[0-9]+}"`, `Prefix` `false`, `Queries` `["page={page:[0-9]+}", "sort=asc"]`, `Vars` `["sub", "category", "id", "page"]`, `BuildOnly` `false`. A route with nothing set returns a `RouteInfo` whose strings are empty, slices have length 0 and booleans are false.
5. `Route.Info` in `routeinfo.go` reflects what a subrouter route inherits: for `api := r.PathPrefix("/api").Host("api.example.com").Methods("GET", "PUT").Subrouter()`, the route `api.HandleFunc("/users/{id}", h)` has `Path` `"/api/users/{id}"`, `Prefix` `false`, `Host` `"api.example.com"`, `Methods` `["GET", "PUT"]` and `Vars` `["id"]`.
6. `Route.Info` in `routeinfo.go` returns the zero `RouteInfo` (all strings empty, all slices of length 0, all booleans false) for a route whose `GetError()` is not nil, for example `r.Path("no-slash").Name("bad").Methods("GET")`.
7. The slices in the value `Route.Info` returns are copies: changing an element of `Methods`, `Schemes`, `Queries` or `Vars` in the returned value changes neither a later `Info()` result nor what `GetMethods` returns.
8. `routeinfo.go` declares `func (r *Router) Routes() []RouteInfo`. It returns `Info()` of the routes `r.Walk` visits, in visiting order (registration order, subrouters depth-first), leaving out (a) every route whose `GetError()` is not nil and (b) every route that only leads to another router: one on which `Subrouter()` was called, or whose handler is a `*Router`. The routes of that other router are listed. A build-only route is listed. The result is a non-nil slice of length 0 for a router with no listed route.
9. `Router.Routes` in `routeinfo.go` called on a subrouter lists only that subrouter's routes (and those below it), with their full inherited templates. A router mounted as a handler (`r.PathPrefix("/mnt").Handler(other)`) contributes `other`'s routes with their own templates, not prefixed by `/mnt`.
10. `routeinfo.go` declares `func (r *Router) WriteRoutes(w io.Writer) error`. It writes one header line and then one line per element of `r.Routes()`, in order, each line ending with `"\n"`. The columns are, in order, `METHODS`, `PATH`, `HOST`, `QUERIES`, `NAME`; the header line holds those five words. It returns `nil`, or the error returned by `w` if a write fails.
11. The cells `Router.WriteRoutes` in `routeinfo.go` writes for a route are: `METHODS`, the methods joined with `","`, or `*` when there are none; `PATH`, the path template followed by `*` when `Prefix` is true, or `-` when that text is empty; `HOST`, the host template, or `-` when empty; `QUERIES`, the query templates joined with `"&"`, or `-` when there are none; `NAME`, the name, or `-` when empty.
12. `Router.WriteRoutes` in `routeinfo.go` aligns the table with spaces only: each of the first four cells of a line is padded on the right to the width of the widest cell of its column (the header word included), measured in runes, and followed by two spaces; the fifth cell is written unpadded, so no line has trailing whitespace. A router with no listed route writes exactly `"METHODS  PATH  HOST  QUERIES  NAME\n"`.
13. For the router built by `r.HandleFunc("/", h).Name("home")`, `r.HandleFunc("/articles", h).Methods("GET").Queries("page", "{page:[0-9]+}")`, `api := r.PathPrefix("/api").Host("api.example.com").Subrouter()`, `api.HandleFunc("/users/{id}", h).Methods("GET", "PUT").Name("user")` and `r.PathPrefix("/static/").Handler(http.NotFoundHandler())`, `Router.WriteRoutes` in `routeinfo.go` writes exactly these five lines:

    ```text
    METHODS  PATH             HOST             QUERIES             NAME
    *        /                -                -                   home
    GET      /articles        -                page={page:[0-9]+}  -
    GET,PUT  /api/users/{id}  api.example.com  -                   user
    *        /static/*        -                -                   -
    ```

14. A new file `example_routes_test.go` in package `mux_test` declares `ExampleRouter_WriteRoutes`, which builds the router of criterion 13, calls `r.WriteRoutes(os.Stdout)` and has an `// Output:` comment with the five lines of criterion 13, so `go test` verifies it.
15. `README.md` gains a section headed `### Listing Routes`, placed after the "Walking Routes" section and immediately before the `### Graceful Shutdown` heading, and the table of contents gains the line `* [Listing Routes](#listing-routes)` directly after the "Walking Routes" line. The section says that `Router.Routes` returns a `RouteInfo` for every route, subrouters included, and that `Router.WriteRoutes` prints them as a table; it shows the code of criterion 13 and its output, and lists the fields of `RouteInfo`. No other part of `README.md` changes.
16. A new file `routeinfo_test.go` in package `mux` covers criteria 2 to 13, including a write to a writer that always fails. All existing tests pass unchanged, `go test ./...` passes and `gofmt -l .` prints nothing.

## Risks

- `Methods` and `Schemes` describe only the first matcher of each kind. A subrouter route that narrows inherited methods with a second `Methods` call is listed with the inherited ones. This matches `GetMethods` and is stated in the field comments; computing the intersection is a non-goal.
- Detecting a route that leads to a subrouter depends on how `Route.Subrouter` registers the subrouter (as a matcher of the parent route) and on a `*Router` handler, the same two cases `Router.walk` follows. If `walk` learns a third case, `Routes` must follow it.
- The table width is measured in runes, which is wrong for wide or combining characters; route templates are almost always ASCII.
- `README.md` is also edited by other changes in other sections; the edit here is confined to the table of contents line and the new section.

## Open questions

None.
