#!/usr/bin/env bash
# The live walk of the React console: a real factoryd, built from this tree
# with the React bundle embedded, serving a freshly seeded data directory,
# driven by headless Chrome through every screen and every action.
#
#   console/test/walk/run.sh [walk.mjs arguments]
#
# It needs Go, Node and Playwright's Chromium (`npx playwright install
# chromium`), or WALK_BROWSER_CHANNEL=chrome for an installed Google Chrome.
# WALK_SCRIPT picks what drives the browser: walk.mjs (default) or shots.mjs,
# which only photographs every screen in both themes (at the sizes in
# WALK_VIEWPORTS), or layout.mjs, which measures the board against the bars in
# its header and exits 1 when one fails. WALK_SEED=demo is the dense seed with
# a worker that is alive and running one request and a release policy that is
# set, for pictures of a board at work. WALK_SEED=dense adds twenty more
# requests in two projects (seed-dense.mjs); the default seed is what walk.mjs
# is written against. It starts nothing but its own
# `factoryd serve` on WALK_PORT (default 18090) with FACTORYD_AUTOSTART=0, and
# stops it on exit. Everything it writes is under WALK_DIR (default
# ~/buildgate/console-browser-walk): the data directory, the workspace, the
# binary, the server log and the screenshots.
#
# The React bundle replaces internal/consoleweb/dist (gitignored) for the
# build and the previous contents are put back afterwards.
set -euo pipefail

root="$(cd "$(dirname "$0")/../../.." && pwd)"
walk_dir="${WALK_DIR:-$HOME/buildgate/console-browser-walk}"
port="${WALK_PORT:-18090}"
export FACTORYD_AUTOSTART=0

mkdir -p "$walk_dir"
rm -rf "$walk_dir/data" "$walk_dir/workspace" "$walk_dir/shots" "$walk_dir/config"
mkdir -p "$walk_dir/shots"

echo "walk: building the console bundle"
(cd "$root/console" && ${WALK_BUILD:-npm run build} >"$walk_dir/build.log" 2>&1) || {
  tail -30 "$walk_dir/build.log"
  exit 1
}

dist="$root/internal/consoleweb/dist"
saved="$walk_dir/dist.saved"
# The embedded bundle is one directory for the whole checkout: only one
# build may swap it at a time (the walk and the screenshots
# can be started side by side).
lock="${TMPDIR:-/tmp}/buildgate-consoleweb-dist.lock"
until mkdir "$lock" 2>/dev/null; do sleep 1; done
restore_dist() {
  if [ -d "$saved" ]; then
    rm -rf "$dist"
    cp -R "$saved" "$dist"
    rm -rf "$saved"
  fi
  rmdir "$lock" 2>/dev/null || true
}
cleanup() {
  restore_dist
  if [ -n "${server_pid:-}" ]; then kill "$server_pid" 2>/dev/null || true; fi
}
trap cleanup EXIT

rm -rf "$saved"
cp -R "$dist" "$saved"
rm -rf "$dist"
mkdir -p "$dist"
cp -R "$root/console/dist/." "$dist/"
touch "$dist/.gitkeep"
echo "walk: building factoryd with the React console embedded"
(cd "$root" && go build -o "$walk_dir/factoryd" ./cmd/factoryd)
restore_dist

echo "walk: seeding $walk_dir/data"
(cd "$root" && FACTORYD_CONTRACT_FIXTURE_DIR="$walk_dir/data" \
  FACTORYD_CONTRACT_FIXTURE_WORKSPACE="$walk_dir/workspace" \
  go test ./internal/api -run TestWriteContractFixtureDataDir -count=1 >/dev/null)
# A second copy of two requests, so the walk can take both exits from one
# state (approve and request changes; retry and cancel).
for id in req-spec-review req-halted; do
  cp -R "$walk_dir/data/requests/$id" "$walk_dir/data/requests/$id-b"
  sed -i '' "s/\"id\": \"$id\"/\"id\": \"$id-b\"/; s#requests/$id/#requests/$id-b/#g" \
    "$walk_dir/data/requests/$id-b/request.json"
done
if [ "${WALK_SEED:-}" = dense ] || [ "${WALK_SEED:-}" = demo ]; then
  node "$root/console/test/walk/seed-dense.mjs" "$walk_dir"
fi
# New request needs a workspace that is a git repository with an AGENTS.md
# committed at its root.
if [ ! -d "$walk_dir/workspace/.git" ]; then
  git -C "$walk_dir/workspace" init -q
  printf '# Walk workspace\n\nNothing is built here.\n' >"$walk_dir/workspace/AGENTS.md"
  git -C "$walk_dir/workspace" add AGENTS.md
  git -C "$walk_dir/workspace" -c user.name=walk -c user.email=walk@example.invalid \
    commit -q -m "walk workspace"
fi

# Its own empty config directory: with the operator's, a machine that has a
# stable start token (quickstart, install-service) prints no "#t=" link, and
# the walk would run under that operator's session config.
mkdir -p "$walk_dir/config"
if [ "${WALK_SEED:-}" = demo ]; then
  mkdir -p "$walk_dir/config/factoryd"
  printf '%s\n' 'release_max_files_changed: 25' 'release_max_insertions: 1000' \
    'release_rollback_plan: "git revert the merge commit on main"' \
    >"$walk_dir/config/factoryd/config.yml"
  now="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  printf '{"repository":"acme/app","task_queue":"factoryd","pid":%s,"started_at":"%s","updated_at":"%s","active_requests":["req-building"],"job_slots":2}\n' \
    "$$" "$now" "$now" >"$walk_dir/data/daemon-heartbeat-queue-run.json"
fi
XDG_CONFIG_HOME="$walk_dir/config" "$walk_dir/factoryd" serve -addr "127.0.0.1:$port" -data-dir "$walk_dir/data" \
  >"$walk_dir/serve.log" 2>&1 &
server_pid=$!
until curl -sf -o /dev/null "http://127.0.0.1:$port/healthz"; do
  kill -0 "$server_pid" 2>/dev/null || { cat "$walk_dir/serve.log"; exit 1; }
  sleep 0.5
done
token="$(grep -o '#t=[A-Za-z0-9_-]*' "$walk_dir/serve.log" | head -1 | cut -c4-)"

cd "$root/console"
node "test/walk/${WALK_SCRIPT:-walk.mjs}" "http://127.0.0.1:$port" "$token" "$walk_dir" "$@"
