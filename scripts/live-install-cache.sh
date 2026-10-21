#!/bin/sh
# live-install-cache.sh: the install image end to end (`make
# live-install-cache`; see AGENTS.md's "Live validation" section).
#
# The fixture testdata/fixtures/install-cache-node is a Node package with one
# npm dependency, a lockfile, `node_modules` in .gitignore, `verify_command:
# npm test` and a `sandbox:` block whose install is `npm ci`. The script makes
# a disposable repository of it, approves the block in a data dir of its own
# and runs one ticket (data/tickets/install-cache-triple.spec.md) through
# factoryd's single-ticket run path four times:
#
#   run 1: builds the install image: an `install` attempt, install.cache_hit
#          false.
#   run 2: the same key: install.cache_hit true and no `install` attempt; its
#          baseline log starts with `buildgate: install restored <key12>`,
#          and the time from run start to baseline start of both runs is
#          printed beside run 1's install duration_ms, for information.
#   run 3: after a committed lockfile change: a new key, built again.
#   run 4: after an edit of the block that nobody approved: refused before
#          any sandbox starts (no attempt recorded), saying the block is not
#          approved.
#
# Every exit, an abort included, evicts the images it built.
#
# Prerequisites are live-smoke.sh's: a factoryd built from the commit under
# test (`make install`, or FACTORYD_BIN), images from `make install`, the
# registry proxy on, a working model route, Docker, the OpenShell gateway
# and meter (`factoryd doctor -fix`). LIVE_SMOKE_CONFIG and
# LIVE_SMOKE_TEMPORAL apply as in live-smoke.sh. Run `make live-smoke` and
# `make live-private-module` too.
set -eu

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
FACTORYD_BIN="${FACTORYD_BIN:-factoryd}"
FIXTURE="$REPO_ROOT/testdata/fixtures/install-cache-node"
SPEC="$REPO_ROOT/data/tickets/install-cache-triple.spec.md"
# Under ~/buildgate: the sandbox VM shares it (USAGE.md, "Data directory and colima").
mkdir -p "$HOME/buildgate/live-install-cache"
SCRATCH="$(mktemp -d "$HOME/buildgate/live-install-cache/run.XXXXXX")"
CLONE="$SCRATCH/install-cache-node"
DATA="$SCRATCH/data"
mkdir -p "$DATA"

config_args_line() {
	if [ -n "${LIVE_SMOKE_CONFIG:-}" ]; then
		echo "-config $LIVE_SMOKE_CONFIG"
	fi
}

# On every exit, an abort included: the images this script built go, then
# its scratch directory.
cleanup() {
	if [ -d "$CLONE/.git" ]; then
		# shellcheck disable=SC2046
		"$FACTORYD_BIN" sandbox evict -repo "$CLONE" -data-dir "$DATA" $(config_args_line) >/dev/null 2>&1 || true
	fi
	rm -rf "$SCRATCH"
}
trap cleanup EXIT
trap 'exit 130' INT TERM

config_args() {
	if [ -n "${LIVE_SMOKE_CONFIG:-}" ]; then
		printf '%s\n' -config "$LIVE_SMOKE_CONFIG"
	fi
}

git_fixture() { git -C "$CLONE" -c core.hooksPath=/dev/null -c commit.gpgsign=false -c user.name=buildgate-fixture -c user.email=fixture@example.invalid "$@"; }

"$REPO_ROOT/scripts/fixture-repo.sh" "$FIXTURE" "$CLONE"

approve() {
	# shellcheck disable=SC2046
	hash="$("$FACTORYD_BIN" sandbox show -repo "$CLONE" -data-dir "$DATA" $(config_args) | sed -n 's/^sha256: *//p')"
	[ -n "$hash" ] || { echo "FAIL: sandbox show printed no hash" >&2; exit 1; }
	# shellcheck disable=SC2046
	"$FACTORYD_BIN" sandbox approve -repo "$CLONE" -data-dir "$DATA" -sha256 "$hash" $(config_args)
}

# run_ticket N: runs the ticket once; prints the run.json path.
run_ticket() {
	ticket="live-install-cache-$1-$(date +%Y%m%d-%H%M%S)"
	set -- -ticket "$ticket" -spec "$SPEC" -workspace "$CLONE" -data-dir "$DATA" \
		-preflight-profile brownfield -open-pull-request=false -code-review-policy off
	if [ -n "${LIVE_SMOKE_TEMPORAL:-}" ]; then
		set -- "$@" -temporal-address "$LIVE_SMOKE_TEMPORAL"
	fi
	if [ -n "${LIVE_SMOKE_CONFIG:-}" ]; then
		set -- "$@" -config "$LIVE_SMOKE_CONFIG"
	fi
	"$FACTORYD_BIN" "$@" >"$SCRATCH/$ticket.out" 2>&1 || true
	cat "$SCRATCH/$ticket.out" >&2
	cp "$SCRATCH/$ticket.out" "$SCRATCH/last-run.out"
	dir="$(find "$DATA/runs" -maxdepth 1 -name "${ticket}*" -type d 2>/dev/null | head -1)"
	echo "${dir:-$DATA/runs/$ticket}/run.json"
}

# check RUN_JSON MODE: prints one line per failed expectation and the
# numbers; MODE is built, hit or refused.
check() {
	python3 -I - "$1" "$2" <<'PY'
import json, os, sys
from datetime import datetime
path, mode = sys.argv[1:3]
if not os.path.exists(path):
    if mode != "refused":
        print("no run record")
    sys.exit(0)
r = json.load(open(path))
attempts = r.get("attempts") or []
install = r.get("install") or {}
kinds = [a.get("kind") for a in attempts]
problems = []
def ts(s):
    return datetime.fromisoformat(s.replace("Z", "+00:00"))
if mode == "refused":
    if attempts:
        problems.append("an unapproved block launched %d sandbox(es)" % len(attempts))
    if r.get("state") == "accepted":
        problems.append("an unapproved block was accepted")
elif mode == "built":
    if install.get("cache_hit") is not False or "install" not in kinds:
        problems.append("want an install attempt and cache_hit false, got %s and %s" % (kinds, install.get("cache_hit")))
    print("NUMBERS install key=%s duration_ms=%s" % (install.get("key", "")[:12], install.get("duration_ms")))
    base = [a for a in attempts if a.get("kind") == "baseline_verify"]
    if base:
        gap = (ts(base[0]["started_at"]) - ts(r["created_at"])).total_seconds()
        print("NUMBERS run start to baseline start %.1f s" % gap)
        print("GAP %.1f" % gap)
elif mode == "hit":
    if install.get("cache_hit") is not True or "install" in kinds:
        problems.append("want cache_hit true and no install attempt, got %s and %s" % (install.get("cache_hit"), kinds))
    base = [a for a in attempts if a.get("kind") == "baseline_verify"]
    if base:
        log = base[0].get("log_path", "")
        head = open(log, errors="replace").readlines()[:20] if os.path.exists(log) else []
        want = "buildgate: install restored " + install.get("key", "")[:12]
        if not any(line.startswith(want) for line in head):
            problems.append("the baseline log's first lines have no %r" % want)
        gap = (ts(base[0]["started_at"]) - ts(r["created_at"])).total_seconds()
        print("NUMBERS run start to baseline start %.1f s" % gap)
        print("GAP %.1f" % gap)
    else:
        problems.append("no baseline_verify attempt")
if mode != "refused" and r.get("state") != "accepted":
    problems.append("state=%s, want accepted" % r.get("state"))
for p in problems:
    print(p)
PY
}

fail=0
report() {
	label="$1"
	shift
	out="$(check "$@")"
	echo "$out" | grep '^NUMBERS' | sed 's/^NUMBERS /  /' || true
	last_gap="$(echo "$out" | sed -n 's/^GAP //p')"
	problems="$(echo "$out" | grep -v '^NUMBERS' | grep -v '^GAP' | grep -v '^$' || true)"
	if [ -n "$problems" ]; then
		echo "FAIL $label:"
		echo "$problems" | sed 's/^/  /'
		fail=1
	else
		echo "PASS $label"
	fi
}

approve
run1="$(run_ticket 1)"
report "run 1 builds the install image" "$run1" built
gap1="$last_gap"
run2="$(run_ticket 2)"
report "run 2 reuses it" "$run2" hit
gap2="$last_gap"
# Information only (the verdict is cache_hit and no install attempt): the
# target is a restore that costs under 10 s over a run with no install.
echo "  run 1 reached its baseline in ${gap1:-?} s, run 2 (cache hit) in ${gap2:-?} s"
key1="$(python3 -I -c 'import json,sys; print((json.load(open(sys.argv[1])).get("install") or {}).get("key",""))' "$run1" 2>/dev/null || true)"

# A committed lockfile change: a new key.
python3 -I - "$CLONE/package-lock.json" <<'PY'
import json, sys
p = sys.argv[1]
lock = json.load(open(p))
lock["packages"][""]["description"] = "lockfile changed by live-install-cache"
json.dump(lock, open(p + ".new", "w"), indent=2)
PY
mv "$CLONE/package-lock.json.new" "$CLONE/package-lock.json"
git_fixture commit --quiet -am "change the lockfile"
run3="$(run_ticket 3)"
report "run 3 builds a new key" "$run3" built
key3="$(python3 -I -c 'import json,sys; print((json.load(open(sys.argv[1])).get("install") or {}).get("key",""))' "$run3" 2>/dev/null || true)"
if [ -z "$key3" ] || [ "$key3" = "$key1" ]; then
	echo "FAIL run 3's key ${key3:-none} is run 1's"
	fail=1
fi

# An edit of the block nobody approved: refused before any sandbox.
sed 's/npm ci --ignore-scripts/npm ci --ignore-scripts --no-audit/' "$CLONE/.factory.yml" >"$CLONE/.factory.yml.new"
mv "$CLONE/.factory.yml.new" "$CLONE/.factory.yml"
git_fixture commit --quiet -am "edit the sandbox block"
run4="$(run_ticket 4)"
report "run 4 is refused (unapproved block)" "$run4" refused
if ! grep -q "is not approved" "$SCRATCH/last-run.out"; then
	echo "FAIL run 4's output does not say the sandbox block is not approved"
	fail=1
fi

exit "$fail"
