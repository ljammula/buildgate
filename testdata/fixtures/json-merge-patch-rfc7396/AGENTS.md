# AGENTS.md

A Go module with one package: RFC 7396 JSON Merge Patch in `merge.go`. No
dependencies outside the standard library.

- Setup: none. The Go toolchain named in `go.mod` is all it needs.
- Build: `go build ./...`
- Test: `go test .`
- Lint: `go vet ./...`

`verify/` holds the reference oracle test. Do not edit it.
