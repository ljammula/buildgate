# AGENTS.md

A Go module that imports a private module, `private.example/acme/shout`
(`go.mod`). No public proxy or repository has that module, and the repository
has no README.

## Commands

- Setup: none beyond Go, in an environment whose Go module proxy serves the
  private module.
- Build, vet and test: `make test`
  (`go build ./... && go vet ./... && go test ./...`)

`make test` passes only where `GOPROXY` reaches the private module and
`GONOPROXY=none` sends it through that proxy: the `Makefile` exports
`GOPRIVATE=private.example`, which would otherwise make Go fetch it straight
from its host. A buildgate build sets both. Elsewhere the command fails with
`unrecognized import path`.

Do not vendor the private module, replace it in `go.mod`, or change the
`GOPRIVATE` line.
