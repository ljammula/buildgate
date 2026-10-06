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
#      and TEMPORAL_ADDRESS are read by the tests themselves (t.Skip when
#      unset/unreachable; cmd/factoryd's end-to-end ticket tests ignore
#      TEMPORAL_ADDRESS and run their own leak-proof test server, see
#      scripts/temporal-test-server.sh), so this sweeps up every live test in the module
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
# Usage: scripts/verify-live.sh
# Env:
#   TEMPORAL_ADDRESS   Temporal server address (default localhost:7233).
#                       If unreachable and the `temporal` CLI is on PATH, a
#                       throwaway dev server is started and torn down after.
# Exit code: 0 iff every live test ran and passed; nonzero otherwise
# (including "Docker unavailable" and "no reachable/startable Temporal").

set -eu

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
LOG="$(mktemp -t verify-live-log.XXXXXX)"
trap 'rm -f "$LOG"' EXIT

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

ADDR="${TEMPORAL_ADDRESS:-localhost:7233}"
STARTED_TEMPORAL=0

temporal_reachable() {
	if command -v temporal >/dev/null 2>&1; then
		temporal operator cluster health --address "$ADDR" >/dev/null 2>&1
	else
		host="$(echo "$ADDR" | cut -d: -f1)"
		port="$(echo "$ADDR" | cut -d: -f2)"
		python3 -c "import socket,sys; s=socket.socket(); s.settimeout(1); sys.exit(0 if s.connect_ex((sys.argv[1], int(sys.argv[2])))==0 else 1)" "$host" "$port"
	fi
}

if ! temporal_reachable; then
	if command -v temporal >/dev/null 2>&1; then
		echo "verify-live: starting a throwaway Temporal dev server on $ADDR..."
		port="$(echo "$ADDR" | cut -d: -f2)"
		nohup temporal server start-dev --headless --port "$port" --log-level warn >"$REPO_ROOT/.verify-live-temporal.log" 2>&1 &
		disown
		STARTED_TEMPORAL=1
		ok=0
		for _ in $(seq 1 30); do
			if temporal_reachable; then
				ok=1
				break
			fi
			sleep 1
		done
		if [ "$ok" -ne 1 ]; then
			echo "verify-live: Temporal dev server did not become healthy within 30s -- see $REPO_ROOT/.verify-live-temporal.log" >&2
			exit 1
		fi
	else
		echo "verify-live: no Temporal server reachable at $ADDR, and no 'temporal' CLI on PATH to start one -- run 'make temporal-up' or install the Temporal CLI" >&2
		exit 1
	fi
fi

cleanup_temporal() {
	if [ "$STARTED_TEMPORAL" = 1 ] && command -v temporal >/dev/null 2>&1; then
		pkill -f "temporal server start-dev.*--port ${ADDR#*:}" >/dev/null 2>&1 || true
	fi
}
trap 'cleanup_temporal; rm -f "$LOG"' EXIT

# -p 1: one test package at a time. Live tests in cmd/factoryd,
# internal/sandbox and internal/workflow share one Docker daemon and its
# shared registry-proxy egress network; run as parallel packages,
# one package's orphan reconciliation can remove a shared network another
# is launching on (observed 2026-09-24 under the concurrent sweep, never
# alone). Real
# factoryd runs are serialized the same way.
status=0
echo "verify-live: pass 1/2 -- go test -race <all non-data packages> (DOCKER_SANDBOX_LIVE=1, TEMPORAL_ADDRESS=$ADDR)"
(
	cd "$REPO_ROOT"
	# Scoped away from gitignored data/, same as the Makefile's GO_PACKAGES:
	# run artifacts there are not this module's source.
	# shellcheck disable=SC2046 -- deliberate word-split package list
	TEMPORAL_ADDRESS="$ADDR" DOCKER_SANDBOX_LIVE=1 GOFLAGS="-count=1" go test -race -p 1 $(go list ./... | grep -v /data/)
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
	TEMPORAL_ADDRESS="$ADDR" DOCKER_SANDBOX_LIVE=1 GOFLAGS="-count=1 -v" go test -race -p 1 -run "$RUN" $PKGS
) >"$LOG" 2>&1 || status=1
grep -E '^(--- (PASS|FAIL|SKIP)|ok|FAIL)' "$LOG" || true

# Same anchored phrase as ci.yml's own false-green guard: a live Temporal
# test silently t.Skip-ing (server died mid-run) must fail this script
# explicitly rather than pass on an exit code a silent skip never affects.
if grep -qE "Temporal (Service|server) at \S+ is unreachable:" "$LOG"; then
	echo "verify-live: one or more live Temporal tests skipped (the dev server became unreachable mid-run) -- failing explicitly, see the log above" >&2
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
