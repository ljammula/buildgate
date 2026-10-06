#!/usr/bin/env bash
# The live walk of the React console: a real factoryd, built from this tree
# with the React bundle embedded, serving a freshly seeded data directory,
# driven by headless Chrome through every screen and every action.
#
#   console-react/test/walk/run.sh [walk.mjs arguments]
#
# It needs Go, Node and Google Chrome. It starts nothing but its own
# `factoryd serve` on WALK_PORT (default 18090) with FACTORYD_AUTOSTART=0, and
# stops it on exit. Everything it writes is under WALK_DIR (default
# ~/buildgate/console-react-walk): the data directory, the workspace, the
# binary, the server log and the screenshots.
#
# The React bundle replaces internal/consoleweb/dist (gitignored) for the
# build and the previous contents are put back afterwards.
set -euo pipefail

root="$(cd "$(dirname "$0")/../../.." && pwd)"
walk_dir="${WALK_DIR:-$HOME/buildgate/console-react-walk}"
port="${WALK_PORT:-18090}"
export FACTORYD_AUTOSTART=0

mkdir -p "$walk_dir"
rm -rf "$walk_dir/data" "$walk_dir/workspace" "$walk_dir/shots"
mkdir -p "$walk_dir/shots"

echo "walk: building the console bundle"
(cd "$root/console-react" && ${WALK_BUILD:-npm run build} >"$walk_dir/build.log" 2>&1) || {
  tail -30 "$walk_dir/build.log"
  exit 1
}

dist="$root/internal/consoleweb/dist"
saved="$walk_dir/dist.saved"
rm -rf "$saved"
cp -R "$dist" "$saved"
restore() {
  rm -rf "$dist"
  cp -R "$saved" "$dist"
  rm -rf "$saved"
  if [ -n "${server_pid:-}" ]; then kill "$server_pid" 2>/dev/null || true; fi
}
trap restore EXIT

rm -rf "$dist"
mkdir -p "$dist"
cp -R "$root/console-react/dist/." "$dist/"
touch "$dist/.gitkeep"
echo "walk: building factoryd with the React console embedded"
(cd "$root" && go build -o "$walk_dir/factoryd" ./cmd/factoryd)

echo "walk: seeding $walk_dir/data"
(cd "$root" && FACTORYD_CONTRACT_FIXTURE_DIR="$walk_dir/data" \
  FACTORYD_CONTRACT_FIXTURE_WORKSPACE="$walk_dir/workspace" \
  go test ./internal/api -run TestWriteContractFixtureDataDir -count=1 >/dev/null)
# New request needs a workspace that is a git repository.
if [ ! -d "$walk_dir/workspace/.git" ]; then
  git -C "$walk_dir/workspace" init -q
  git -C "$walk_dir/workspace" -c user.name=walk -c user.email=walk@example.invalid \
    commit -q --allow-empty -m "walk workspace"
fi

"$walk_dir/factoryd" serve -addr "127.0.0.1:$port" -data-dir "$walk_dir/data" \
  >"$walk_dir/serve.log" 2>&1 &
server_pid=$!
until curl -sf -o /dev/null "http://127.0.0.1:$port/healthz"; do
  kill -0 "$server_pid" 2>/dev/null || { cat "$walk_dir/serve.log"; exit 1; }
  sleep 0.5
done
token="$(grep -o '#t=[A-Za-z0-9_-]*' "$walk_dir/serve.log" | head -1 | cut -c4-)"

cd "$root/console-react"
node test/walk/walk.mjs "http://127.0.0.1:$port" "$token" "$walk_dir" "$@"
