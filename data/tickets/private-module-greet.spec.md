# Ticket: add Greet to the private-module fixture

This is an existing repo. Deliberately tiny: this ticket exists as the fixture
of `make live-private-module` (see AGENTS.md's "Live validation" section), not
to ship a product feature. Target repo: the private-module-go fixture
(`testdata/fixtures/private-module-go`), a Go package whose one dependency,
`private.example/acme/shout`, is a module no public proxy has.

## Goal

`greet.go` declares `Greet(name string) string`, which panics. Implement it
with the module the package already depends on, and test it.

## Required changes

**`greet.go`**:

1. `Greet(name)` returns `shout.Loud("hello " + name)`, importing
   `private.example/acme/shout` as `banner.go` does. `Greet("ada")` is
   `"HELLO ADA!"`.
2. Remove the "not yet implemented" comment and the panic.

**`greet_test.go`** (new file):

1. A table-driven test of `Greet` covering at least `"ada"` and the empty
   name (`Greet("")` is `"HELLO !"`).

## Out of scope

- No change to `go.mod`, `go.sum`, `Makefile`, `banner.go` or `banner_test.go`.
- No new dependency of any kind.

Allowed-Files: greet.go, greet_test.go

Required-Changed-Files: greet.go, greet_test.go

Required-Content: shout.Loud

## Acceptance / verification

Verify-Command: make test

- `make test` (build, vet and `go test ./...`) passes, including the new
  `Greet` cases and the existing `TestBanner`.

Success criterion: `Greet("ada") == "HELLO ADA!"`, verified by a real test.
