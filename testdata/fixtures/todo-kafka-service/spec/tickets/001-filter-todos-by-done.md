This is an existing repo.

## Goal

Add an optional `done` query-param filter to `GET /todos`, so a caller
can list only completed or only incomplete todos without filtering
client-side.

## Required changes

- In `handleTodos`'s `GET` case for the list path (`path == "" || path
  == "/"`), read an optional `done` query parameter
  (`r.URL.Query().Get("done")`).
  - Absent: return the full list, exactly as today (no behavior change
    for existing callers).
  - `"true"` or `"false"`: return only todos whose `Done` field matches.
  - Any other non-empty value: `400 Bad Request`, no body change beyond
    the existing status-only error shape this handler already uses
    elsewhere (e.g. the `POST` bad-body case).
- Implement the filtering as a small exported or unexported helper next
  to `Store.List` in `main.go` (e.g. `filterByDone(todos []Todo, done
  bool) []Todo`) rather than inline in the handler, so it's directly
  testable.
- This only changes the in-memory `/todos` listing path; leave
  `handleTodosDurable` (`/todos/durable`) untouched.

Update `ARCHITECTURE.md` and append a `PROGRESS.md` entry before finishing.
Neither file exists yet in this repo -- create both: `ARCHITECTURE.md`
with a brief repo-layout/verification overview (matching this repo's own
real structure, not a goal_pilot-drafted template), `PROGRESS.md` with
one dated entry for this ticket.

## Out of scope

- No change to POST/PUT/PATCH/DELETE handling, to the Kafka publisher,
  or to `pgstore`/`cmd/consumer`.
- No pagination or additional filters (e.g. by title).

## Verification

- `go build ./... && go test ./...` must pass -- this ticket's own tests
  are pure in-memory, no Postgres/Kafka required. (This repo's own
  `make verify` also brings up the real Postgres/Kafka compose services
  for the integration-tagged suite; this ticket does not touch that
  path and does not require it to be re-run.)
- `curl 'localhost:<port>/todos?done=true'` returns only todos with
  `"done":true`.
- `curl 'localhost:<port>/todos?done=bogus'` returns `400`.
- `curl 'localhost:<port>/todos'` (no query param) is byte-for-byte
  unchanged from before this ticket.

Verify-Command: go build ./... && go test ./...
Allowed-Files: main.go, main_test.go, ARCHITECTURE.md, PROGRESS.md
Required-Changed-Files: main.go, main_test.go, ARCHITECTURE.md, PROGRESS.md
Required-Content: done

## Commit

Commit once both pass. Commit message must be exactly:
`ticket(001): filter-todos-by-done`
