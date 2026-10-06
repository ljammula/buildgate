// Package requestdriver advances a request through its states: spec
// drafting, planning, the ticket builds, the automatic review-corrective
// rounds and the pull-request review loop. Each call does one step and saves
// the request.
//
// It runs model jobs and builds through the runner functions its callers
// pass, and reaches GitHub, git push and the build entry point only through
// Deps, which the command supplies: the real one runs gh and git, and a test
// passes fakes. The worker (cmd/factoryd) decides when a step runs.
package requestdriver
