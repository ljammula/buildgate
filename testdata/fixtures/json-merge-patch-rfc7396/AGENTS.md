# AGENTS.md

A Go module with one package, `mergepatch`, for RFC 7396 JSON Merge Patch.
`merge.go` declares `MergePatch`, which panics until it is implemented. No
dependencies outside the standard library, and no README.

## Commands

- Setup: none beyond Go. `go.mod` names the version.
- Build: `go build ./...`
- Vet: `go vet ./...`
- Test: `go test ./...`. The `verify/` suite fails while `MergePatch` is
  unimplemented.

Do not edit `verify/`: it is the conformance suite an implementation is judged by.
