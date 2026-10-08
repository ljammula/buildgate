#!/bin/sh
# live-private-module.sh: one real ticket on a repository whose dependency is
# a module only this machine can fetch (`make live-private-module`; see
# AGENTS.md's "Live validation" section).
#
# The fixture testdata/fixtures/private-module-go requires
# private.example/acme/shout, which no public proxy and no public repository
# has. This script serves it from a file GOPROXY on the host
# (scripts/private-module-proxy.py) and exports the Go settings an operator
# with a company's private modules has: a GOPROXY that reaches them and
# GONOSUMDB for their paths. factoryd fetches what the fixture's go.sum lists
# with those settings and the registry proxy serves it to the sandbox, which
# has neither the settings nor a route to the module.
#
# PASS means the run was accepted: its verify command compiled and tested
# code that imports the module, inside the sandbox. Without the fetch the
# same ticket is quarantined at canonical_verify on an unresolvable import.
#
# It is live-smoke.sh with this one fixture; every LIVE_SMOKE_* and
# FACTORYD_BIN setting of that script applies.
set -eu

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
SCRATCH="$(mktemp -d "${TMPDIR:-/tmp}/buildgate-private-module.XXXXXX")"
trap 'rm -rf "$SCRATCH"' EXIT

python3 -I "$REPO_ROOT/scripts/private-module-proxy.py" "$REPO_ROOT/testdata/fixtures/private-module-go-dep" "$SCRATCH/proxy"

# What an earlier run fetched for this fixture is removed first, so that every
# run exercises the fetch: the cached module, and the views that hold it.
MODULES="$HOME/buildgate/gomodules"
rm -rf "$MODULES/cache/cache/download/private.example"
for view in "$MODULES"/views/*; do
	[ -d "$view/private.example" ] && rm -rf "$view"
done

export GOPROXY="file://$SCRATCH/proxy,https://proxy.golang.org,direct"
export GONOSUMDB="private.example"
export LIVE_SMOKE_FIXTURES="$REPO_ROOT/testdata/fixtures/private-module-go:$REPO_ROOT/data/tickets/private-module-greet.spec.md:live-private-module"
exec "$REPO_ROOT/scripts/live-smoke.sh" "$@"
