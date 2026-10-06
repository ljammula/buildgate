#!/bin/sh
# console-walk: the standing operator-usability check for the embedded
# console. Where live-smoke proves the pipeline builds
# code, this proves an operator can drive one real request from submit to a
# correctly explained terminal state using ONLY the console: no factoryd CLI
# call beyond `submit` and `serve`/`worker`, and no manual Refresh.
#
# What it does:
#   1. Fresh scratch dir under $HOME (Docker Desktop/colima must be able to
#      mount it), a disposable clone of CONSOLE_WALK_REPO, and a session
#      config copied from CONSOLE_WALK_CONFIG with data_dir pointed at the
#      scratch dir, open_pull_request: false, and a workspaces: entry for
#      the scratch clone (POST /requests' own allowlist -- see
#      internal/api's workspaceAllowed doc comment -- needs this to accept
#      a request submitted against it from the console).
#   2. `factoryd serve` on loopback with NO API token (the single-machine
#      rule: same-origin loopback writes need none) and `factoryd worker`
#      on Temporal. This no longer calls `factoryd submit` itself --
#      walk.mjs's own first "submit" step does that through the console's
#      New request form instead, so the walk proves that path works too.
#   3. walk.mjs drives headless Chromium through the console (see its own
#      header for the steps and what each one asserts).
#
# Output: PASS/FAIL per step on stdout, screenshots and a results.json under
# the scratch dir, and one JSON line appended to CONSOLE_WALK_RESULTS_FILE.
#
# Needs: Docker, a working model route in CONSOLE_WALK_CONFIG (the live
# route used 2026-09-24: pi + chatgpt-codex gpt-5.6-luna), Temporal at
# CONSOLE_WALK_TEMPORAL, node + npm, and FACTORYD_BIN built WITH the console
# embedded (`make console-build` before `go build`). Takes ~10-20 min.
set -eu

REPO_ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
HERE="$REPO_ROOT/scripts/console-walk"
FACTORYD_BIN="${FACTORYD_BIN:-factoryd}"
# The walk starts its own serve and worker: nothing else may autostart.
export FACTORYD_AUTOSTART=0
CONSOLE_WALK_CONFIG="${CONSOLE_WALK_CONFIG:-$HOME/.config/factoryd/config.yml}"
CONSOLE_WALK_TEMPORAL="${CONSOLE_WALK_TEMPORAL:-localhost:7233}"
CONSOLE_WALK_REPO="${CONSOLE_WALK_REPO:-$REPO_ROOT/testdata/fixtures/math_ops}"
CONSOLE_WALK_PORT="${CONSOLE_WALK_PORT:-8097}"
CONSOLE_WALK_TEMPORAL_UI="${CONSOLE_WALK_TEMPORAL_UI:-http://localhost:8233}"
export CONSOLE_WALK_TEMPORAL_UI
CONSOLE_WALK_RESULTS_FILE="${CONSOLE_WALK_RESULTS_FILE:-$HOME/buildgate/console-walk/results.jsonl}"
SCRATCH="${CONSOLE_WALK_SCRATCH:-$HOME/buildgate/console-walk/$(date +%Y%m%d-%H%M%S)}"

for v in FACTORYD_API_START_TOKEN FACTORYD_API_OVERRIDE_TOKEN FACTORYD_API_READ_TOKEN FACTORYD_API_AUTH_TOKEN; do
	if eval "[ -n \"\${$v:-}\" ]"; then
		echo "console-walk: unset $v first -- the walk proves the no-token loopback path, and worker refuses to start with it set" >&2
		exit 2
	fi
done
[ -f "$CONSOLE_WALK_CONFIG" ] || { echo "console-walk: no session config at $CONSOLE_WALK_CONFIG" >&2; exit 2; }

mkdir -p "$SCRATCH/xdg/factoryd" "$SCRATCH/data" "$SCRATCH/shots" "$(dirname "$CONSOLE_WALK_RESULTS_FILE")"
grep -vE '^(data_dir|open_pull_request|workspaces):' "$CONSOLE_WALK_CONFIG" > "$SCRATCH/xdg/factoryd/config.yml"
# workspaces: makes $SCRATCH/repo an allowed POST /requests target (see
# internal/api's workspaceAllowed doc comment) -- without it, walk.mjs's
# own "submit" step would get a 403 the first time it tries to submit a
# request against this scratch clone, since nothing has been submitted
# against it before.
printf 'data_dir: %s\nopen_pull_request: false\nworkspaces:\n  - %s\n' \
	"$SCRATCH/data" "$SCRATCH/repo" >> "$SCRATCH/xdg/factoryd/config.yml"
# The same backfill quickstart does for a reused config: an operator config
# with no release_* keys otherwise denies every PR.
for kv in 'release_max_files_changed: 25' 'release_max_insertions: 1000' 'release_rollback_plan: git revert the merge commit on main'; do
	grep -q "^${kv%%:*}:" "$SCRATCH/xdg/factoryd/config.yml" || printf '%s\n' "$kv" >> "$SCRATCH/xdg/factoryd/config.yml"
done
"$REPO_ROOT/scripts/fixture-repo.sh" "$CONSOLE_WALK_REPO" "$SCRATCH/repo"
export XDG_CONFIG_HOME="$SCRATCH/xdg"

pids=""
cleanup() {
	for p in $pids; do kill "$p" 2>/dev/null || true; done
}
trap cleanup EXIT INT TERM

"$FACTORYD_BIN" serve -addr "127.0.0.1:$CONSOLE_WALK_PORT" -temporal-ui-url "$CONSOLE_WALK_TEMPORAL_UI" > "$SCRATCH/serve.log" 2>&1 &
pids="$pids $!"
"$FACTORYD_BIN" worker -temporal-address "$CONSOLE_WALK_TEMPORAL" > "$SCRATCH/worker.log" 2>&1 &
pids="$pids $!"

base="http://127.0.0.1:$CONSOLE_WALK_PORT"
i=0
until curl -fsS "$base/healthz" >/dev/null 2>&1; do
	i=$((i + 1))
	[ "$i" -lt 60 ] || { echo "console-walk: serve never came up; see $SCRATCH/serve.log" >&2; exit 1; }
	sleep 1
done

echo "console-walk: scratch $SCRATCH, workspace $SCRATCH/repo"

if [ ! -d "$HERE/node_modules/playwright" ]; then
	(cd "$HERE" && npm install --no-audit --no-fund --silent && npx playwright install chromium >/dev/null)
fi

set +e
node "$HERE/walk.mjs" "$base" "$SCRATCH/repo" "$SCRATCH"
status=$?
set -e

sha="$(git -C "$REPO_ROOT" rev-parse --short HEAD 2>/dev/null || echo unknown)"
outcome=PASS
[ "$status" -eq 0 ] || outcome=FAIL
# walk.mjs's own "submit" step derives the request id from the console's
# URL after submitting through the New request form (see its own header) --
# no longer known to this script's own shell variables, so it's read back
# from the results.json walk.mjs writes on every exit, PASS or FAIL, rather
# than re-derived here.
request_id="$(node -e 'console.log(JSON.parse(require("fs").readFileSync(process.argv[1], "utf8")).requestId || "")' "$SCRATCH/results.json" 2>/dev/null || echo "")"
printf '{"date":"%s","git_sha":"%s","request_id":"%s","outcome":"%s","scratch":"%s"}\n' \
	"$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$sha" "$request_id" "$outcome" "$SCRATCH" >> "$CONSOLE_WALK_RESULTS_FILE"
echo "console-walk: $outcome (results: $SCRATCH/results.json)"
exit "$status"
