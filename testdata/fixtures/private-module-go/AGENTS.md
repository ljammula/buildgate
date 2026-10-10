# AGENTS.md

A Go module that depends on a private module, `private.example/acme/shout`,
which only the build's module proxy has.

- Setup: none. Do not change `GOPRIVATE`, `GOPROXY` or `GONOPROXY`: the
  `Makefile` and the build environment set them.
- Build, test and lint: `make test` (`go build ./... && go vet ./... && go test ./...`)

Do not vendor the private module or replace it in `go.mod`.
