#!/usr/bin/env bash
# Request parity: the Flutter console and the React console, each embedded in
# its own factoryd binary and serving its own identical data directory, are
# driven through the same operator actions; every write request each page
# sends is recorded and compared.
#
#   console-react/test/parity/run.sh [parity.mjs arguments, e.g. --only approve-spec]
#
# Environment: PARITY_DIR (default ~/buildgate/console-parity), PARITY_PORT_FLUTTER
# (18096), PARITY_PORT_REACT (18097), PARITY_FLUTTER_DIST (default
# ~/buildgate/console-flutter-dist), PARITY_BUILD (default `npm run build`),
# WALK_BROWSER_CHANNEL=chrome for an installed Google Chrome.
#
# The embedded bundle (internal/consoleweb/dist, gitignored) is swapped under
# the lock the walk uses, once per binary, and always put back.
set -euo pipefail

root="$(cd "$(dirname "$0")/../../.." && pwd)"
pdir="${PARITY_DIR:-$HOME/buildgate/console-parity}"
port_flutter="${PARITY_PORT_FLUTTER:-18096}"
port_react="${PARITY_PORT_REACT:-18097}"
flutter_dist="${PARITY_FLUTTER_DIST:-$HOME/buildgate/console-flutter-dist}"
export FACTORYD_AUTOSTART=0

mkdir -p "$pdir"
rm -rf "$pdir/data-flutter" "$pdir/data-react" "$pdir/workspace-flutter" "$pdir/workspace-react" "$pdir/shots"
mkdir -p "$pdir/shots"

echo "parity: building the React bundle"
(cd "$root/console-react" && ${PARITY_BUILD:-npm run build} >"$pdir/build.log" 2>&1) || {
  tail -30 "$pdir/build.log"
  exit 1
}

dist="$root/internal/consoleweb/dist"
saved="$pdir/dist.saved"
lock="${TMPDIR:-/tmp}/buildgate-consoleweb-dist.lock"
have_lock=0
server_pids=""
restore_dist() {
  if [ -d "$saved" ]; then
    rm -rf "$dist"
    cp -R "$saved" "$dist"
    rm -rf "$saved"
  fi
  if [ "$have_lock" = 1 ]; then
    rmdir "$lock" 2>/dev/null || true
    have_lock=0
  fi
}
cleanup() {
  restore_dist
  for pid in $server_pids; do kill "$pid" 2>/dev/null || true; done
}
trap cleanup EXIT

# build_with <bundle dir> <binary>: swap the bundle in, build, put it back.
build_with() {
  until mkdir "$lock" 2>/dev/null; do sleep 1; done
  have_lock=1
  rm -rf "$saved"
  cp -R "$dist" "$saved"
  rm -rf "$dist"
  mkdir -p "$dist"
  cp -R "$1/." "$dist/"
  touch "$dist/.gitkeep"
  (cd "$root" && go build -o "$2" ./cmd/factoryd)
  restore_dist
}

echo "parity: building factoryd-flutter"
build_with "$flutter_dist" "$pdir/factoryd-flutter"
echo "parity: building factoryd-react"
build_with "$root/console-react/dist" "$pdir/factoryd-react"

seed() { # <name>
  local data="$pdir/data-$1" ws="$pdir/workspace-$1"
  (cd "$root" && FACTORYD_CONTRACT_FIXTURE_DIR="$data" \
    FACTORYD_CONTRACT_FIXTURE_WORKSPACE="$ws" \
    go test ./internal/api -run TestWriteContractFixtureDataDir -count=1 >/dev/null)
  for id in req-spec-review req-halted; do
    cp -R "$data/requests/$id" "$data/requests/$id-b"
    sed -i '' "s/\"id\": \"$id\"/\"id\": \"$id-b\"/; s#requests/$id/#requests/$id-b/#g" \
      "$data/requests/$id-b/request.json"
  done
  if [ ! -d "$ws/.git" ]; then
    git -C "$ws" init -q
    git -C "$ws" -c user.name=parity -c user.email=parity@example.invalid \
      commit -q --allow-empty -m "parity workspace"
  fi
}
echo "parity: seeding both data directories"
seed flutter
seed react

serve() { # <name> <port>
  "$pdir/factoryd-$1" serve -addr "127.0.0.1:$2" -data-dir "$pdir/data-$1" \
    >"$pdir/serve-$1.log" 2>&1 &
  local pid=$!
  server_pids="$server_pids $pid"
  until curl -sf -o /dev/null "http://127.0.0.1:$2/healthz"; do
    kill -0 "$pid" 2>/dev/null || { cat "$pdir/serve-$1.log"; exit 1; }
    sleep 0.5
  done
}
serve flutter "$port_flutter"
serve react "$port_react"
token_of() { grep -o '#t=[A-Za-z0-9_-]*' "$pdir/serve-$1.log" | head -1 | cut -c4-; }

cd "$root/console-react"
set +e
node test/parity/parity.mjs "http://127.0.0.1:$port_flutter" "$(token_of flutter)" \
  "http://127.0.0.1:$port_react" "$(token_of react)" "$pdir" "$@"
status=$?
exit $status
