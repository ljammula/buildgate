---
name: go-service
description: Go implementation in a buildgate build round. Use when the repository has go.mod and the ticket changes Go handlers, workers, concurrency, dependencies or network exposure. Not for repositories without go.mod.
---

# Go service (buildgate)


Read `go.mod`, the repository's instructions and its task-runner commands before editing. Use the repository's own format, generate and test commands.

- Add focused table-driven tests for the behaviour the ticket changes, and run them after each coherent edit.
- Use `go test -race` on the packages you touched when the change involves goroutines, workers, caches or shared state.
- The sandbox has no network: do not fetch modules or run tools that download data (such as `govulncheck`'s database). Do not add a dependency the module cache does not already have.
- Do not invent a second build workflow; the verify command named in the build prompt's checklist is the project-wide check.
