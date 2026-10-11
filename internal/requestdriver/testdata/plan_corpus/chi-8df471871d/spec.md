# Spec

## Problem

The `middleware` package cannot answer conditional `GET` requests. A handler that wants clients and caches to revalidate with `If-None-Match` has to hash its own response, set `ETag` and write the `304 Not Modified` itself. The package only mentions these headers in `middleware/nocache.go`, which removes them.

## Scope

Add an entity-tag middleware in a new file `middleware/etag.go`: `ETag`, `ETagWithOptions`, the `ETagOptions` struct and the `DefaultETagMaxBufferBytes` constant. For a `GET` request the middleware buffers the response body of the next handler, and when the response has status 200 it sets an `ETag` header computed from the body and answers `304 Not Modified` with no body when the request's `If-None-Match` header matches. Responses with another status, requests with another method, and bodies larger than a configurable buffer limit pass through without a tag.

The scope also includes doc comments on every new exported identifier and tests in a new file `middleware/etag_test.go`.

## Non-goals

- Handling `If-Match`, `If-Modified-Since`, `If-Unmodified-Since`, `If-Range` or `Range`.
- Tagging responses to `HEAD` or any method other than `GET`, or responses with any status other than 200.
- Setting, recomputing or validating `Content-Length`, `Cache-Control`, `Last-Modified` or `Vary`.
- Supporting streaming through the buffer: `http.Flusher`, `http.Hijacker` and `http.Pusher` behaviour of the wrapped writer while a response is being buffered is not specified.
- Changing `NoCache` in `middleware/nocache.go`, `middleware/wrap_writer.go`, `middleware/compress.go`, `README.md`, `CHANGELOG.md` or any other existing file.
- Adding a dependency outside the Go standard library.

## Affected services and packages

The `github.com/go-chi/chi/v5/middleware` package gains two exported functions, one exported struct type and one exported constant in two new files. No existing file, external service, persistent data or runtime dependency is affected.

## Acceptance criteria

1. `middleware/etag.go` declares, in package `middleware`, the exported untyped integer constant `DefaultETagMaxBufferBytes` with the value `1048576`, and the exported type `ETagOptions` as a struct with exactly two exported fields: `Weak bool` and `MaxBufferBytes int`.
2. `middleware/etag.go` declares `ETagWithOptions` with the exact signature `func ETagWithOptions(opts ETagOptions) func(next http.Handler) http.Handler` and `ETag` with the exact signature `func ETag(next http.Handler) http.Handler`. `ETag(next)` behaves exactly as `ETagWithOptions(ETagOptions{})(next)`.
3. For a request whose method is not `GET` (including `HEAD`, `POST`, `PUT`, `PATCH`, `DELETE` and `OPTIONS`), the `ETagWithOptions` handler calls `next` and the response reaches the client with the status, headers and body `next` wrote and with no `ETag` header added, whatever the request's `If-None-Match` header.
4. For a `GET` request where `next` finishes with status 200, either by calling `WriteHeader(200)` or by never calling `WriteHeader`, has written a body of at most the buffer limit, and has not set an `ETag` response header, the `ETagWithOptions` handler sets the `ETag` response header to a double quote, the 64-character lower-case hexadecimal SHA-256 digest of the complete body, and a double quote. A body of `hello world`, written in one or several `Write` calls, gives exactly `"b94d27b9934d3e08a52e52d7da7dabfac484efe37a5380ee9088f7ace2efcde9"`; a handler that writes nothing gives exactly `"e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"`.
5. When `ETagOptions.Weak` is true, the tag computed in criterion 4 is prefixed with `W/`, so a body of `hello world` gives exactly `W/"b94d27b9934d3e08a52e52d7da7dabfac484efe37a5380ee9088f7ace2efcde9"`. When it is false the tag has no prefix.
6. When `next` has set a non-empty `ETag` response header itself on a buffered 200 response to a `GET`, the `ETagWithOptions` handler leaves that value unchanged, computes no digest, ignores `Weak`, and uses that value as the response tag in criteria 7 to 9.
7. In `middleware/etag.go`, the `If-None-Match` request header matches the response tag when, after splitting the header value on `,` and trimming spaces from each element, any element is exactly `*`, or any element with one leading `W/` removed is equal as a string to the response tag with one leading `W/` removed. So for the response tag `"abc"` the header values `"abc"`, `W/"abc"`, `*` and `"x" ,  W/"abc" , "y"` match, and `"x"`, `abc` (no quotes) and an absent or empty header do not; for the response tag `W/"abc"` the header value `"abc"` matches.
8. For a buffered 200 response to a `GET` whose `If-None-Match` matches under criterion 7, the `ETagWithOptions` handler responds with status 304, writes no body bytes, keeps the `ETag` response header, deletes the `Content-Type` and `Content-Length` response headers, and keeps every other header `next` set.
9. For a buffered 200 response to a `GET` whose `If-None-Match` does not match, the `ETagWithOptions` handler responds with status 200, the `ETag` header, every header `next` set, and the complete body `next` wrote, byte for byte.
10. For a `GET` request where `next` calls `WriteHeader` with any status other than 200 (for example 201, 204, 206, 301, 404 or 500), the `ETagWithOptions` handler sends that status, the headers `next` set and the body `next` wrote, adds no `ETag` header, and never answers 304, whatever the request's `If-None-Match` header.
11. The buffer limit of `ETagWithOptions` is `ETagOptions.MaxBufferBytes` when that is greater than zero and `DefaultETagMaxBufferBytes` when it is zero or negative. A body whose total length equals the limit is tagged under criterion 4: with `MaxBufferBytes: 4`, a body of `abcd` written as `ab` then `cd` gets the SHA-256 tag of `abcd`.
12. For a `GET` request where the total number of body bytes `next` writes exceeds the buffer limit, the `ETagWithOptions` handler responds with status 200, the headers `next` set, and the complete body in the order written with no byte lost or repeated, adds no `ETag` header, and never answers 304, whatever the request's `If-None-Match` header. With `MaxBufferBytes: 4`, bodies written as `abcde`, as `ab`, `cd`, `e`, and as `abc`, `defgh`, `ij` reach the client as `abcde`, `abcde` and `abcdefghij`. From the write that crosses the limit onwards, bytes go to the underlying `http.ResponseWriter` without further buffering.
13. `Write` on the writer that `ETagWithOptions` passes to `next` returns `len(p), nil` for bytes it buffers, and `Header()` on it returns the underlying `http.ResponseWriter`'s header map.
14. `middleware/etag_test.go`, in package `middleware`, contains tests that cover criteria 3 to 12, and `go test ./...` and `go vet ./...` pass.

## Risks

- Buffering holds up to the buffer limit of every tagged `GET` response in memory and delays the first byte until the handler returns, so a streaming or server-sent-events handler behind the middleware stops streaming until it has written more than the limit. The doc comment on `ETag` must say so.
- The tag is computed from the bytes the middleware sees. Placed outside a compressing middleware it hashes the compressed bytes, placed inside it hashes the identity bytes while the client receives another representation; the order of `Use` calls decides which, and the doc comment must name this.
- A handler that sets `Content-Length` and then gets a 304 would send a wrong length if the header were kept, which is why criterion 8 deletes it. On the over-limit path the status and headers are committed at the write that crosses the limit, so a handler that changes headers after that point loses those changes, as it would without the middleware.
- SHA-256 over the body adds CPU cost to every tagged response; the buffer limit bounds it per request.

## Open questions

None.
