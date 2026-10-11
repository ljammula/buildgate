# Spec

## Problem

A path variable matches one segment: `{name}` expands to `[^/]+`. To capture the rest of a path (a file path under `/files/`, a blob path after `/repos/{owner}/{repo}/blob/`) the caller has to know to write `{rest:.*}`. Go 1.22's `http.ServeMux` made `{name...}` the familiar spelling for this, and people moving to or from it write it by habit. In `newRouteRegexp` in `regexp.go` that spelling silently means something else: a one-segment variable whose name is literally `rest...`, so `/files/a/b` does not match and `mux.Vars(r)["rest"]` is empty.

## Scope

Teach `newRouteRegexp` in `regexp.go` the wildcard form `{name...}`: in a path or path-prefix template, a variable written with a trailing `...` and no pattern is a variable called `name` that matches the remainder of the path, slashes included, exactly as `{name:.*}` does today. A wildcard must be the last thing in the path template and is rejected in host and query templates; both mistakes are reported through `Route.GetError()` with exact messages.

The scope includes the documentation of the form in the `Route.Path` doc comment in `route.go`, in the package comment in `doc.go` and in the "Examples" section of `README.md`, and tests in a new file `wildcard_test.go`.

## Non-goals

- Changing the meaning of `{name}`, `{name:pattern}` or of a variable that has both a trailing `...` and an explicit pattern (`{name...:[a-z]+}` stays a variable named `name...`).
- The `{$}` end-of-path marker or any other `http.ServeMux` pattern syntax.
- Changing path cleaning, `StrictSlash` or `UseEncodedPath` behaviour; a wildcard behaves under each exactly as `{name:.*}` does.
- Wildcards in host or query templates.

## Affected services and packages

The `github.com/gorilla/mux` package: `newRouteRegexp` in `regexp.go`, the `Route.Path` doc comment in `route.go`, the package comment in `doc.go`, the "Examples" section of `README.md`, and a new test file `wildcard_test.go`. No dependency is added.

## Acceptance criteria

1. `newRouteRegexp` in `regexp.go`: in a template of type `regexpTypePath` or `regexpTypePrefix`, a variable tag `{name...}` (no colon, text ending in `...`) defines a variable named `name` (the three dots removed) with the pattern `.*`. A router with `Path("/files/{rest...}")` serves `/files/a/b/c.txt` with `Vars` equal to `{"rest": "a/b/c.txt"}`, `/files/a` with `{"rest": "a"}`, `/files/a/b/` with `{"rest": "a/b/"}` and `/files/` with `{"rest": ""}`, and answers 404 for `/files` and `/other/a`.
2. A wildcard combines with ordinary variables and literals in `newRouteRegexp` in `regexp.go`: `Path("/repos/{owner}/{repo:[a-z]+}/blob/{path...}")` serves `/repos/gorilla/mux/blob/docs/img/logo.png` with `owner=gorilla`, `repo=mux`, `path=docs/img/logo.png` and answers 404 for `/repos/a/b/c/blob/x`; `Path("/dl/v{version...}")` serves `/dl/v1.2/linux/amd64` with `version=1.2/linux/amd64`; `Path("/{all...}")` serves `/x/y` with `all=x/y`; `PathPrefix("/static/{rest...}")` serves `/static/css/site.css` with `rest=css/site.css`.
3. For a route built with `Path("/files/{dir}/{rest...}")`, `Route.GetPathTemplate` in `route.go` returns `/files/{dir}/{rest...}` (the template as written), `Route.GetPathRegexp` returns `^/files/(?P<v0>[^/]+)/(?P<v1>.*)$` and `Route.GetVarNames` returns `["dir", "rest"]`. For `PathPrefix("/static/{rest...}")`, `GetPathRegexp` returns `^/static/(?P<v0>.*)`.
4. URL building accepts slashes in a wildcard value: for `Path("/files/{dir}/{rest...}")`, `Route.URL("dir", "d", "rest", "a/b/c.txt")` and `Route.URLPath` with the same pairs return a URL whose `Path` is `/files/d/a/b/c.txt`; with `"rest", ""` the path is `/files/d/`; without `rest` the error is `mux: missing route variable "rest"`.
5. `newRouteRegexp` in `regexp.go` returns an error when a wildcard tag is not the last thing in a path or prefix template, that is when any character of the template as written follows the tag's closing brace. The message is `mux: wildcard %q must end the path template %q`, formatted with the tag (braces included) and the full template. `Path("/files/{rest...}/meta").GetError()` is `mux: wildcard "{rest...}" must end the path template "/files/{rest...}/meta"`; `Path("/files/{rest...}/")` fails the same way with or without `StrictSlash(true)`; `Path("/{a...}/{b}")` gives `mux: wildcard "{a...}" must end the path template "/{a...}/{b}"`.
6. The rule of criterion 5 applies to the combined template of a subrouter route: `PathPrefix("/files/{rest...}").Subrouter().Path("/meta").GetError()` is `mux: wildcard "{rest...}" must end the path template "/files/{rest...}/meta"`.
7. `newRouteRegexp` in `regexp.go` returns an error for a wildcard tag in a host or query template, wherever it stands. The message is `mux: wildcard %q is only allowed in a path template, got %q`, formatted with the tag and the template. `Host("{sub...}.example.com").GetError()` is `mux: wildcard "{sub...}" is only allowed in a path template, got "{sub...}.example.com"` and `Queries("q", "{v...}").GetError()` is `mux: wildcard "{v...}" is only allowed in a path template, got "q={v...}"`.
8. In `newRouteRegexp` in `regexp.go` a path tag `{...}` (no name before the dots) is rejected with the existing error `mux: missing name or pattern in "{...}"`, and a tag with an explicit pattern is never a wildcard: `Path("/x/{name...:[a-z]+}")` serves `/x/abc` with `Vars` equal to `{"name...": "abc"}` and answers 404 for `/x/abc/def`.
9. A wildcard route behaves exactly as the same route written with `{name:.*}`: for each of `/files/a/b`, `/files/a/b/`, `/files/` and `/files`, with `StrictSlash` false and true, a router with `Path("/files/{rest...}")` gives the same status code and `Vars` as one with `Path("/files/{rest:.*}")`; with `UseEncodedPath()`, `/files/a%2Fb/c` gives `rest=a%2Fb/c`.
10. The doc comment of `Route.Path` in `route.go` gains a third bullet, after the `{name:pattern}` bullet: `- {name...} matches the rest of the path, slashes included. It must end the template.`
11. The package comment in `doc.go` and the "Examples" section of `README.md` each gain, directly after the paragraph that introduces `mux.Vars()` and its code sample (in `README.md`, before the sentence beginning "And this is all you need to know"), a short paragraph that introduces `{name...}`, says it must be the last element of the path and is equivalent to `{name:.*}`, and shows `r.HandleFunc("/files/{filepath...}", FileHandler)`. No other part of `README.md` changes.
12. A new file `wildcard_test.go` in package `mux` covers criteria 1 to 9 with table-driven tests. All existing tests pass unchanged, `go test ./...` passes and `gofmt -l .` prints nothing.

## Risks

- A template that today uses a variable whose name ends in `...` without a pattern changes meaning: the variable loses the dots from its name and matches across slashes, or the route now fails to build if the tag is not last or is in a host or query. Such names are legal today but serve no purpose.
- The "last thing in the template" check must use the template as written. Under `StrictSlash(true)` `newRouteRegexp` strips a trailing slash before parsing, and a check against the stripped string would wrongly accept `/files/{rest...}/`.
- A wildcard is greedy and also matches the empty remainder, so `/files/{rest...}` shadows more specific routes registered after it; registration order decides, as for any other route.
- `README.md` is also edited by other changes in other sections; the edit here is confined to the "Examples" section.

## Open questions

None.
