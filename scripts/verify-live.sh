#!/bin/sh
# verify-live.sh: the live counterpart to `make verify` -- runs every Go test
# gated on DOCKER_SANDBOX_LIVE=1 (real Docker sandbox, real git;
# see internal/sandbox/docker_test.go and friends) plus every Temporal-live
# test (real `temporal server`, no self-skip; see
# internal/workflow/temporal_live_test.go and the Temporal-gated cases woven
# through cmd/factoryd's own integration_*_test.go files), the same coverage
# .github/workflows/ci.yml's "make verify" + "Live Docker sandbox tests"
# steps get on a self-hosted runner.
#
# Two go test passes, not one, and deliberately not `go test -v ./...`:
#
#   1. `go test -race ./...` with no -v, both env vars set. DOCKER_SANDBOX_LIVE
#      and FACTORYD_TEST_TEMPORAL_ADDRESS are read by the tests themselves
#      (t.Skip when unset), so this sweeps up every live test in the module
#      automatically, including one added after this script was last touched
#      -- no hand-picked -run list to fall out of sync. Its exit code is the
#      authoritative pass/fail signal.
#   2. A second, -v pass scoped to the three packages that hold every live
#      test today (cmd/factoryd, internal/sandbox, internal/workflow) with
#      -run matching either the "Live"/"Temporal" naming convention
#      essentially all of them follow, or the two named outliers that don't
#      (TestDockerfileProjectBakesMissingDependency and
#      TestDockerfileProjectDoesNotLeakProjectSecrets). Exists only to get
#      ci.yml's own false-green guard below cheaply: go test with no -v
#      prints only "ok <pkg>" for a package where everything passed OR
#      skipped, dropping the one line that would say a live test actually
#      skipped instead of running. Pass 1 already covers correctness; pass 2
#      is bounded in size (dozens of tests, not the whole module) so its -v
#      log stays small enough to actually read -- go test -v across the
#      whole module here produces enough output that the log gets truncated
#      before the guard can see it, which is worse than no guard.
#      New package with a live test: add it to PKGS below (same maintenance
#      ci.yml's own hand-picked -run list already requires).
#
# Prerequisites (not checked beyond what's below): Go, and either Docker
# already logged in to pull any private image the live tests need, or local
# images built via `make local-images`.
#
# Temporal: the tests run through one test Temporal dev server this script
# starts on a free loopback port (scripts/temporal-test-server.sh, which
# cannot outlive this script) and exports as FACTORYD_TEST_TEMPORAL_ADDRESS,
# as scripts/test-sharded.sh does. The operator's localhost:7233 and an
# ambient TEMPORAL_ADDRESS are never used. Needs the `temporal` CLI.
#
# Usage: scripts/verify-live.sh
# Exit code: 0 iff every live test ran and passed; nonzero otherwise
# (including "Docker unavailable" and "no temporal CLI").

set -eu

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
LOG="$(mktemp -t verify-live-log.XXXXXX)"
TEMPORAL_TMP="$(mktemp -d -t verify-live-temporal.XXXXXX)"
temporal_pid=""
# Closing the lifeline (fd 9) is what stops the test Temporal server.
cleanup_temporal() {
	{ exec 9>&-; } 2>/dev/null || true
	if [ -n "$temporal_pid" ]; then
		wait "$temporal_pid" 2>/dev/null || true
	fi
	rm -rf "$TEMPORAL_TMP"
}
trap 'cleanup_temporal; rm -f "$LOG"' EXIT

if ! command -v docker >/dev/null 2>&1 || ! docker info >/dev/null 2>&1; then
	echo "verify-live: Docker is not available (checked 'docker info') -- the live tests need a real Docker daemon" >&2
	exit 1
fi

# Same reasoning as scripts/live-smoke.sh's own top-of-file comment: a Docker
# backend that only shares $HOME with its VM (colima's own default) accepts a bind mount under the OS temp dir
# (Go's own t.TempDir(), under /var/folders on macOS) without complaint, but
# the container then can't see anything written there -- every
# DOCKER_SANDBOX_LIVE=1 test in internal/sandbox/docker_test.go and friends
# reads DOCKER_SANDBOX_LIVE_ROOT for exactly this reason; default it here to
# a directory under ~/buildgate, the one directory colima shares read-write
# (USAGE.md), so this script works out of the box on a Mac dev machine, not
# just a Linux CI runner whose whole filesystem is shared.
export DOCKER_SANDBOX_LIVE_ROOT="${DOCKER_SANDBOX_LIVE_ROOT:-$HOME/buildgate/verify-live}"
mkdir -p "$DOCKER_SANDBOX_LIVE_ROOT"

if ! command -v temporal >/dev/null 2>&1; then
	echo "verify-live: no 'temporal' CLI on PATH to start the test Temporal server -- install the Temporal CLI" >&2
	exit 1
fi
port="$(python3 -c 'import socket; s = socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1])')"
ADDR="127.0.0.1:$port"
mkfifo "$TEMPORAL_TMP/lifeline"
sh "$REPO_ROOT/scripts/temporal-test-server.sh" factoryd-test-temporal "$port" "$TEMPORAL_TMP/temporal.log" <"$TEMPORAL_TMP/lifeline" &
temporal_pid=$!
exec 9>"$TEMPORAL_TMP/lifeline"
ok=0
for _ in $(seq 1 150); do
	if temporal operator cluster health --address "$ADDR" >/dev/null 2>&1; then
		ok=1
		break
	fi
	sleep 0.2
done
if [ "$ok" -ne 1 ]; then
	echo "verify-live: test Temporal server at $ADDR did not become healthy within 30s:" >&2
	cat "$TEMPORAL_TMP/temporal.log" >&2
	exit 1
fi
export FACTORYD_TEST_TEMPORAL_ADDRESS="$ADDR"
echo "verify-live: test Temporal server at $ADDR"

# -p 1: one test package at a time. Live tests in cmd/factoryd,
# internal/sandbox and internal/workflow share one Docker daemon and its
# shared registry-proxy egress network; run as parallel packages,
# one package's orphan reconciliation can remove a shared network another
# is launching on (observed 2026-09-24 under the concurrent sweep, never
# alone). Real
# factoryd runs are serialized the same way.
status=0
echo "verify-live: pass 1/2 -- go test -race <all non-data packages> (DOCKER_SANDBOX_LIVE=1, FACTORYD_TEST_TEMPORAL_ADDRESS=$ADDR)"
(
	cd "$REPO_ROOT"
	# Scoped away from gitignored data/, same as the Makefile's GO_PACKAGES:
	# run artifacts there are not this module's source.
	# shellcheck disable=SC2046 -- deliberate word-split package list
	DOCKER_SANDBOX_LIVE=1 GOFLAGS="-count=1" go test -race -p 1 $(go list ./... | grep -v /data/)
) >"$LOG" 2>&1 || status=1
tail -100 "$LOG"
if [ "$status" -ne 0 ]; then
	echo "verify-live: pass 1 failed -- full log at $LOG (not cleaned up)" >&2
	trap 'cleanup_temporal' EXIT
	exit 1
fi

PKGS="./cmd/factoryd/... ./internal/sandbox/... ./internal/workflow/..."
RUN='Live|Temporal|TestDockerfileProjectBakesMissingDependency|TestDockerfileProjectDoesNotLeakProjectSecrets'
echo "verify-live: pass 2/2 -- -v -run '$RUN' over $PKGS (false-green skip guard)"
(
	cd "$REPO_ROOT"
	# shellcheck disable=SC2086 -- PKGS is a deliberate word-split package list
	DOCKER_SANDBOX_LIVE=1 GOFLAGS="-count=1 -v" go test -race -p 1 -run "$RUN" $PKGS
) >"$LOG" 2>&1 || status=1
grep -E '^(--- (PASS|FAIL|SKIP)|ok|FAIL)' "$LOG" || true

# Same anchored phrase as ci.yml's own false-green guard: a live Temporal
# test silently t.Skip-ing (no test server address reached it) must fail this
# script explicitly rather than pass on an exit code a silent skip never
# affects.
if grep -qE "Temporal (Service|server) at \S+ is unreachable:" "$LOG"; then
	echo "verify-live: one or more live Temporal tests skipped (no test Temporal server reached them) -- failing explicitly, see the log above" >&2
	status=1
fi

# Same class of guard for the DOCKER_SANDBOX_LIVE=1 tests: with the env var
# set, none of them should hit their own "set DOCKER_SANDBOX_LIVE=1" skip
# message -- if one did, something (a build tag, a nested t.Run losing the
# env, a test bug) silently dropped coverage instead of running it.
if grep -q "set DOCKER_SANDBOX_LIVE=1 to run the real Docker" "$LOG"; then
	echo "verify-live: one or more DOCKER_SANDBOX_LIVE=1 tests skipped despite the env var being set -- failing explicitly, see the log above" >&2
	status=1
fi

if [ "$status" -ne 0 ]; then
	echo "verify-live: pass 2 failed -- log at $LOG (not cleaned up)" >&2
	trap 'cleanup_temporal' EXIT
	exit 1
fi

exit "$status"
