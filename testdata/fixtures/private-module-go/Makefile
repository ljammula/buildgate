# GOPRIVATE as a repository with private modules sets it: the go command
# would then fetch them straight from their host, which a sandboxed build
# cannot reach. The build's own environment sends every module through the
# registry proxy all the same (GONOPROXY=none).
export GOPRIVATE := private.example

.PHONY: test
test:
	go build ./... && go vet ./... && go test ./...
