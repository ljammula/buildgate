#!/bin/sh
# live-compose.sh: the standing live check of the compose-services path --
# a target repo's own docker-compose.yml dependencies launched as sidecars
# for a real ticket, end to end. live-smoke.sh deliberately has no compose
# fixture (it stays cheap enough to run on every relevant PR); this one is
# slower and runs periodically, and before merging a change to
# internal/composeservices, the compose lifecycle, or the BG_SERVICE_*
# worker wiring.
#
# One real ticket (data/tickets/todo-kafka-service-compose-live.spec.md)
# runs through factoryd's default (Temporal) path against a fresh, disposable clone of
# testdata/fixtures/todo-kafka-service (postgres, kafka, redis). The run uses the
# model route from the operator's config. PASS requires:
#   - the run accepted;
#   - compose/services.build.json and services.verify.json enabled, with
#     exactly kafka, postgres and redis;
#   - each integration test name passing in the verify log (the ticket's
#     verify command sets GOFLAGS=-v so `go test` prints them);
#   - the worker told of the services (build_app.py's "compose services:"
#     line in the build log);
#   - run.json compose_services and the rendered PR body's "Compose
#     services:" risk-header line naming all three.
#
# The PR body is captured, never published: the clone's origin is a local
# bare repository and a `gh` shim first on PATH saves `gh pr create`'s
# --body and refuses every other gh call, so nothing reaches GitHub.
#
# Environment:
#   FACTORYD_BIN          factoryd built from the commit under test (default: factoryd)
#   LIVE_COMPOSE_CONFIG   passes -config <path> to factoryd
#   LIVE_COMPOSE_TEMPORAL <address> (default: Temporal at localhost:7233; every
#                         build runs on Temporal)
#   LIVE_COMPOSE_REPO     the fixture repo (default: testdata/fixtures/todo-kafka-service)
#   LIVE_COMPOSE_RESULTS_FILE  JSON-lines track record
#                         (default: ~/buildgate/live-compose/results.jsonl)
#
# Prerequisites: Docker running and `factoryd doctor -target-repo
# testdata/fixtures/todo-kafka-service` all green (no FAIL).
#
# Usage: scripts/live-compose.sh [--list]
# Exit code: 0 iff every check above held.

set -eu

FACTORYD_BIN="${FACTORYD_BIN:-factoryd}"
REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
SOURCE_REPO="${LIVE_COMPOSE_REPO:-$REPO_ROOT/testdata/fixtures/todo-kafka-service}"
SPEC_FILE="$REPO_ROOT/data/tickets/todo-kafka-service-compose-live.spec.md"
VERIFY_COMMAND="GOFLAGS=-v make verify-integration"
SERVICES="kafka postgres redis"
INTEGRATION_TESTS="TestDurableStoreRoundTrip TestKafkaPublisherRoundTrip TestRedisIdempotencyStoreRoundTrip TestDurableListCacheReadThroughAndInvalidate"
CACHE_DIR="$HOME/buildgate/live-compose"
RESULTS_FILE="${LIVE_COMPOSE_RESULTS_FILE:-$CACHE_DIR/results.jsonl}"

if [ "${1:-}" = "--list" ]; then
	echo "repo=$SOURCE_REPO spec=$SPEC_FILE services=[$SERVICES] verify=$VERIFY_COMMAND"
	exit 0
fi
[ -d "$SOURCE_REPO" ] || { echo "live-compose: fixture repo not found at $SOURCE_REPO" >&2; exit 1; }

# Under ~/buildgate, not /tmp: colima shares only ~/buildgate read-write
# with its VM (see live-smoke.sh).
mkdir -p "$CACHE_DIR" "$(dirname "$RESULTS_FILE")"
SCRATCH="$(mktemp -d "$CACHE_DIR/run.XXXXXX")"
trap 'rm -rf "$SCRATCH"' EXIT
clone_dir="$SCRATCH/todo-kafka-service"
data_dir="$SCRATCH/data"
ticket_id="live-compose-$(date +%Y%m%d-%H%M%S)"

"$REPO_ROOT/scripts/fixture-repo.sh" "$SOURCE_REPO" "$clone_dir"
git clone --quiet --bare "$clone_dir" "$SCRATCH/origin.git"
git -C "$clone_dir" remote remove origin 2>/dev/null || true
git -C "$clone_dir" remote add origin "$SCRATCH/origin.git"

mkdir -p "$SCRATCH/bin"
cat > "$SCRATCH/bin/gh" <<EOF_GH
#!/bin/sh
# live-compose.sh's gh shim: saves a draft PR's body, refuses all else.
if [ "\$1 \$2" != "pr create" ]; then
	echo "live-compose gh shim: refusing gh \$*" >&2
	exit 1
fi
while [ \$# -gt 0 ]; do
	if [ "\$1" = "--body" ]; then printf '%s\n' "\$2" > "$SCRATCH/pr-body.md"; fi
	shift
done
echo "https://example.invalid/live-compose/pull/1"
EOF_GH
chmod +x "$SCRATCH/bin/gh"

set -- -ticket "$ticket_id" -spec "$SPEC_FILE" -workspace "$clone_dir" \
	-data-dir "$data_dir" -preflight-profile brownfield -open-pull-request \
	-verify-command "$VERIFY_COMMAND"
[ -n "${LIVE_COMPOSE_CONFIG:-}" ] && set -- "$@" -config "$LIVE_COMPOSE_CONFIG"
[ -n "${LIVE_COMPOSE_TEMPORAL:-}" ] && set -- "$@" -temporal-address "$LIVE_COMPOSE_TEMPORAL"

echo "=== live-compose: running $ticket_id against a clone of $SOURCE_REPO ==="
run_start="$(date +%s)"
run_exit=0
PATH="$SCRATCH/bin:$PATH" "$FACTORYD_BIN" "$@" || run_exit=$?
duration_s=$(($(date +%s) - run_start))

run_dir="$(find "$data_dir/runs" -maxdepth 1 -name "${ticket_id}*" -type d 2>/dev/null | head -1)"
# Prints one line per failed check; empty output means every check held.
problems="$(LIVE_COMPOSE_TEMPORAL="${LIVE_COMPOSE_TEMPORAL:-}" python3 - "$run_dir" "$SCRATCH/pr-body.md" "$SERVICES" "$INTEGRATION_TESTS" <<'PY'
import json, os, sys
run_dir, pr_body_path, services, tests = sys.argv[1], sys.argv[2], sys.argv[3].split(), sys.argv[4].split()

def read(path):
    try:
        return open(path, errors="replace").read()
    except OSError:
        return None

if not run_dir:
    print("no run directory: factoryd did not start the run")
    sys.exit(0)
r = json.loads(read(os.path.join(run_dir, "run.json")) or "{}")
if r.get("state") != "accepted":
    print("state=%s, want accepted (halt: %s)" % (r.get("state"), r.get("triage") or r.get("halt_error")))
if not r.get("temporal_workflow_id"):
    print("Temporal path expected but run.json has no temporal_workflow_id (did the run reach Temporal?)")
for phase in ("build", "verify"):
    report = json.loads(read(os.path.join(run_dir, "compose", "services.%s.json" % phase)) or "{}")
    names = sorted(s.get("name") for s in report.get("services") or [])
    if not report.get("enabled") or names != services:
        print("services.%s.json: enabled=%s services=%s, want enabled with %s (%s)" % (phase, report.get("enabled"), names, services, report.get("disabled_reason", "")))

attempts = r.get("attempts") or []
def last_log(kind):
    logs = [a.get("log_path") for a in attempts if a.get("kind") == kind]
    return read(logs[-1]) if logs else None
verify_log = last_log("verify")
if verify_log is None:
    print("no verify attempt log")
else:
    for name in tests:
        if "--- PASS: %s " % name not in verify_log:
            print("verify log lacks a passing %s" % name)
build_log = last_log("build") or ""
if "compose services: The repository's compose services" not in build_log or not all("%s:" % s in build_log or "%s at " % s in build_log for s in services):
    print("build log lacks build_app.py's compose services line naming %s" % services)

if r.get("compose_services") != services:
    print("run.json compose_services=%s, want %s" % (r.get("compose_services"), services))
body = read(pr_body_path)
want = "- Compose services: %s (launched from the base commit)" % ", ".join("`%s`" % s for s in services)
if body is None:
    print("no PR body captured (pr_open_error: %s)" % r.get("pr_open_error"))
elif want not in body:
    print("PR body lacks %r" % want)
PY
)"

outcome=pass
[ -n "$problems" ] && outcome=fail
python3 -c '
import json, sys
keys = ("date", "git_sha", "factoryd_version", "ticket", "outcome", "duration_s")
row = dict(zip(keys, sys.argv[1:7]))
row["duration_s"] = int(row["duration_s"])
open(sys.argv[7], "a").write(json.dumps(row) + "\n")
' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$(git -C "$REPO_ROOT" rev-parse HEAD 2>/dev/null || echo unknown)" \
	"$("$FACTORYD_BIN" version 2>/dev/null | head -1 || echo unknown)" "$ticket_id" "$outcome" "$duration_s" "$RESULTS_FILE"

if [ "$outcome" = pass ]; then
	echo "PASS live-compose $ticket_id (${duration_s}s)"
	exit 0
fi
printf '%s\n' "$problems"
kept="$CACHE_DIR/failed-$ticket_id"
mkdir -p "$kept"
cp -R "$data_dir/runs" "$kept/" 2>/dev/null || true
cp "$SCRATCH/pr-body.md" "$kept/" 2>/dev/null || true
echo "FAIL live-compose $ticket_id (factoryd exit=$run_exit) -- evidence kept at $kept"
exit 1
