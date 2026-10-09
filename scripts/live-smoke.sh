#!/bin/sh
# live-smoke.sh: the standing "does factoryd actually work end to end"
# benchmark -- see AGENTS.md's "Live validation" section for why this
# exists. Runs a small, fixed set of real tickets through factoryd's own
# single-ticket run path (Temporal by default) against fresh, disposable clones of their
# target repos, and reports a pass/fail count -- not a defect count.
#
# Deliberately NOT run via `factoryd submit`/`worker` (the
# request-driver path): that path drafts a fresh spec from a raw request
# every time (LLM variance) and needs two human approvals
# (spec_review/plan_review) before it ever builds anything, which is
# exactly the automation this script's whole purpose is to avoid. The
# fixtures below already are approved, ticket-shaped spec.md files
# (Allowed-Files:/Verify-Command: headers, in data/tickets/) meant for
# `factoryd`'s bare single-ticket invocation: -spec/-ticket/-workspace only,
# no drafting, no HITL gate, nothing to approve.
#
# Oracle fixtures (a fixture's optional 4th field names a directory under
# data/oracles/, the optional 5th is accept (default) or reject): the approved-
# oracle path. The directory holds the operator-approved oracle tests, a
# MANIFEST.json declaring each one's target_path, and RUN_COMMAND.txt (the
# operator-authored reference_oracle command, read here and passed as
# -reference-oracle-command). Both use the math_ops repo and ticket, one build
# each, roughly 3-6 minutes apiece (build + verify + oracle gate + canary):
#   live-smoke-mathops-oracle (accept): PASS only if the run is accepted AND
#     run.json records a passed reference_oracle gate with its oracle tree hash,
#     a TRUSTWORTHY runtime canary, and a passing verify_after_oracle_commit
#     attempt, AND the result commit (the run's isolated branch) holds the manifest's target_path file and
#     .buildgate/oracles.json (committed by the host). The host never executes target-repo code; factoryd's own sandboxed
#     verify_after_oracle_commit attempt is the proof the committed tree verifies.
#   live-smoke-mathops-oracle-reject (reject): the oracle asserts 3*4 == 13, which
#     no build that follows the ticket can satisfy. PASS only if the run is
#     quarantined with a FAILED reference_oracle gate in run.json and neither
#     the oracle nor .buildgate/oracles.json reached the result commit -- the negative that
#     proves the gate is real. (The oracle is mounted read-only from a per-run
#     copy outside the workspace, so "agent edits the oracle" is a structural
#     impossibility rather than a fixture; it is covered by Go tests.)
# The oracles are plain-function Python (test_oracle_*.py) run by a stdlib-only
# runner in RUN_COMMAND.txt, because the default sandbox image has no pytest and
# unittest cannot collect the canary's non-TestCase classes.
# LIVE_SMOKE_ONLY=<substring> runs only fixtures whose ticket prefix contains it
# (e.g. LIVE_SMOKE_ONLY=oracle); a FAIL copies its runs/ evidence to
# ~/buildgate/live-smoke/failed-<ticket>/ before scratch cleanup; --list prints the fixtures and exits without
# cloning, building or needing Docker/factoryd.
#
# Track record: every fixture run (not a SKIP) appends one JSON line to
# LIVE_SMOKE_RESULTS_FILE (default ~/buildgate/live-smoke/
# results.jsonl) -- {date, git_sha, factoryd_version, fixture, outcome,
# expected, duration_s} -- so a track record
# survives past this run's own stdout. `scripts/live-smoke.sh --results [N]`
# (or `make live-smoke-results`) prints the last N (default 20) recorded runs
# as a table, reading no further than that file.
#
# Prerequisites (not checked here -- this script assumes the operator's
# usual factoryd setup already works, the same way `make verify` assumes
# Go is installed):
#   - `factoryd` built from the commit under test (this repo's own
#     `make install`, or an explicit FACTORYD_BIN override below).
#   - `~/.config/factoryd/config.yml` (or equivalent flags) already
#     configured with a working model route and sandbox runtime -- `factoryd doctor`
#     should report all-green before running this.
#   - Docker running.
#
# Execution path: every build runs on Temporal (factoryd uses localhost:7233,
# starting it with Docker when down). LIVE_SMOKE_TEMPORAL=<address> names
# another server.
#
# LIVE_SMOKE_CONFIG=<path> passes -config <path> to every fixture's factoryd
# invocation, the same session-config file every run then resolves its
# model route and sandbox settings from, instead of the operator's default
# ~/.config/factoryd/config.yml search. Optional -- unset behaves exactly as
# before this existed.
#
# LIVE_SMOKE_CODE_REVIEW_POLICY=<off|advisory|required> passes
# -code-review-policy to every fixture's factoryd invocation (default:
# required) -- this is the standing gate for the standalone AI code-review
# pass, run against a real gateway and model route the way `make live-smoke`
# already exists to catch what unit tests and code review alone can't.
#
# Usage: scripts/live-smoke.sh [--list | --results [N]]
# Exit code: 0 iff every fixture met its expectation (accepted; or, for a reject
# fixture, quarantined by the reference_oracle gate), nonzero otherwise.

set -eu

FACTORYD_BIN="${FACTORYD_BIN:-factoryd}"
REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
RESULTS_FILE="${LIVE_SMOKE_RESULTS_FILE:-$HOME/buildgate/live-smoke/results.jsonl}"
LIVE_SMOKE_CODE_REVIEW_POLICY="${LIVE_SMOKE_CODE_REVIEW_POLICY:-required}"

if [ "${1:-}" = "--results" ]; then
	n="${2:-20}"
	if [ ! -f "$RESULTS_FILE" ]; then
		echo "live-smoke: no results recorded yet at $RESULTS_FILE" >&2
		exit 1
	fi
	tail -n "$n" "$RESULTS_FILE" | python3 -c '
import json, sys
rows = [json.loads(l) for l in sys.stdin if l.strip()]
if not rows:
    sys.exit(0)
cols = ["date", "git_sha", "factoryd_version", "fixture", "outcome", "expected", "duration_s"]
widths = {c: max(len(c), *(len(str(r.get(c, ""))) for r in rows)) for c in cols}
def fmt(r):
    return "  ".join(str(r.get(c, "")).ljust(widths[c]) for c in cols)
print(fmt({c: c for c in cols}))
for r in rows:
    print(fmt(r))
'
	exit 0
fi
# Under ~/buildgate, not /tmp: found on this script's own second real run,
# 2026-09-17 -- a Docker backend whose VM shares only some directories
# (colima: ~/buildgate read-write, USAGE.md) accepts a bind mount
# elsewhere without complaint, but the container then can't
# actually see anything written there, which factoryd's own mount-
# visibility probe correctly catches and reports as an infrastructure
# failure rather than silently producing an empty workspace.
# Each fixture: "<source repo>:<ticket spec file>:<ticket id prefix>".
# Kept deliberately small (2 fixtures, no project-specific sandbox image,
# no docker-compose dependency services) so this is cheap enough to run
# on every relevant PR, not just periodically -- see AGENTS.md's own
# reasoning for why "fast enough to actually get run" mattered more than
# broader coverage here. Add a fixture only if it stays in that spirit;
# broader/slower validation belongs in a periodic proving-ground run,
# not this one.
# LIVE_SMOKE_FIXTURES overrides the fixture list wholesale -- used only by
# scripts/tests/test_live_smoke_recording.py to drive this script against a
# throwaway git fixture and a stub FACTORYD_BIN, offline, to test the
# result-recording logic below without Docker, a real model route, or the
# real fixture repos under testdata/fixtures/.
FIXTURES="${LIVE_SMOKE_FIXTURES:-"
$REPO_ROOT/testdata/fixtures/json-merge-patch-rfc7396:$REPO_ROOT/data/tickets/json-merge-patch-rfc7396.spec.md:live-smoke-jsonmerge
$REPO_ROOT/testdata/fixtures/math_ops:$REPO_ROOT/data/tickets/math-ops-multiply.spec.md:live-smoke-mathops
$REPO_ROOT/testdata/fixtures/math_ops:$REPO_ROOT/data/tickets/math-ops-multiply.spec.md:live-smoke-mathops-oracle:$REPO_ROOT/data/oracles/math-ops-multiply:accept
$REPO_ROOT/testdata/fixtures/math_ops:$REPO_ROOT/data/tickets/math-ops-multiply.spec.md:live-smoke-mathops-oracle-reject:$REPO_ROOT/data/oracles/math-ops-multiply-unsatisfiable:reject
"}"

if [ "${1:-}" = "--list" ]; then
	for fixture in $FIXTURES; do
		[ -z "$fixture" ] && continue
		echo "$fixture" | awk -F: '{ printf "%-32s repo=%s oracle=%s expect=%s\n", $3, $1, ($4 == "" ? "-" : $4), ($5 == "" ? "accept" : $5) }'
	done
	exit 0
fi

# Up to LIVE_SMOKE_JOBS fixtures run at once: each as its own run of this
# script on that one fixture, with its own scratch directory and data dir, its
# output held back and printed whole in fixture order.
#
# Unset, the number comes from the machine as it is now (smoke_jobs below):
# 6 GiB of free host memory and 4 GiB of the Docker VM's memory per fixture,
# at most 4. A 16 GiB Mac gets 1, the sequential run. LIVE_SMOKE_JOBS=<n>
# overrides it; set 1 on a single-instance local model route, which serves
# one build at a time whatever the machine has.
smoke_jobs() {
	host="$("$REPO_ROOT/scripts/parallel-jobs.sh" 6 4 2>/dev/null || echo 1)"
	vm_bytes="$(docker info --format '{{.MemTotal}}' 2>/dev/null || echo 0)"
	case "$vm_bytes" in '' | *[!0-9]*) vm_bytes=0 ;; esac
	vm=$((vm_bytes / 1024 / 1024 / 1024 / 4))
	[ "$vm" -ge 1 ] || vm=1
	[ "$host" -le "$vm" ] || host="$vm"
	echo "$host"
}
if [ -z "${LIVE_SMOKE_JOBS:-}" ]; then
	LIVE_SMOKE_JOBS="$(smoke_jobs)"
	echo "live-smoke: $LIVE_SMOKE_JOBS fixture(s) at a time, sized from this machine's free memory (LIVE_SMOKE_JOBS=<n> overrides)"
fi
case "$LIVE_SMOKE_JOBS" in '' | *[!0-9]* | 0) echo "live-smoke: LIVE_SMOKE_JOBS must be a positive integer, got '$LIVE_SMOKE_JOBS'" >&2; exit 2 ;; esac
if [ "$LIVE_SMOKE_JOBS" -gt 1 ]; then
	mkdir -p "$HOME/buildgate/live-smoke"
	JOBS_DIR="$(mktemp -d "$HOME/buildgate/live-smoke/jobs.XXXXXX")"
	trap 'rm -rf "$JOBS_DIR"' EXIT
	n=0
	running=0
	for fixture in $FIXTURES; do
		[ -z "$fixture" ] && continue
		if [ -n "${LIVE_SMOKE_ONLY:-}" ]; then
			case "$(echo "$fixture" | cut -d: -f3)" in *"$LIVE_SMOKE_ONLY"*) ;; *) continue ;; esac
		fi
		n=$((n + 1))
		(
			# The child's own LIVE_SMOKE_ONLY filter is spent: it gets one fixture.
			status=0
			LIVE_SMOKE_JOBS=1 LIVE_SMOKE_ONLY= LIVE_SMOKE_FIXTURES="$fixture" "$0" >"$JOBS_DIR/$n.log" 2>&1 || status=$?
			echo "$status" >"$JOBS_DIR/$n.exit"
		) &
		running=$((running + 1))
		if [ "$running" -ge "$LIVE_SMOKE_JOBS" ]; then
			wait
			running=0
		fi
	done
	wait
	if [ "$n" -eq 0 ]; then
		echo "live-smoke: no fixture selected (LIVE_SMOKE_ONLY='${LIVE_SMOKE_ONLY:-}'); see --list" >&2
		exit 1
	fi
	pass=0
	fail=0
	results=""
	i=1
	while [ "$i" -le "$n" ]; do
		# Everything but the child's own one-fixture summary.
		sed '/^=== live-smoke summary: /,$d' "$JOBS_DIR/$i.log"
		line="$(sed -n '/^=== live-smoke summary: /,$p' "$JOBS_DIR/$i.log" | grep -E '^(PASS|FAIL|SKIP)  ' | head -1 || true)"
		if [ "$(cat "$JOBS_DIR/$i.exit" 2>/dev/null || echo 1)" = 0 ] && [ -n "$line" ]; then
			case "$line" in PASS*) pass=$((pass + 1)) ;; esac
		else
			fail=$((fail + 1))
			[ -n "$line" ] || line="FAIL  fixture $i (no summary line; its output is above)"
		fi
		results="$results\n$line"
		i=$((i + 1))
	done
	echo ""
	echo "=== live-smoke summary: $pass passed, $fail failed ==="
	printf '%b\n' "$results"
	[ "$fail" -eq 0 ] || exit 1
	exit 0
fi

mkdir -p "$HOME/buildgate/live-smoke"
SCRATCH="$(mktemp -d "$HOME/buildgate/live-smoke/run.XXXXXX")"
trap 'rm -rf "$SCRATCH"' EXIT
KEEP_DIR="$HOME/buildgate/live-smoke"
mkdir -p "$(dirname "$RESULTS_FILE")"

# Recorded once per script run, not per fixture -- both are properties of the
# binary under test, not of any one fixture.
GIT_SHA="$(git -C "$REPO_ROOT" rev-parse HEAD 2>/dev/null || echo unknown)"
FACTORYD_VERSION="$("$FACTORYD_BIN" version 2>/dev/null | head -1 || echo unknown)"

pass=0
fail=0
selected=0
results=""

for fixture in $FIXTURES; do
	[ -z "$fixture" ] && continue
	IFS=: read -r source_repo spec_file ticket_prefix oracle_dir expect <<EOF_FIXTURE
$fixture
EOF_FIXTURE
	expect="${expect:-accept}"
	if [ -n "${LIVE_SMOKE_ONLY:-}" ]; then
		case "$ticket_prefix" in *"$LIVE_SMOKE_ONLY"*) ;; *) continue ;; esac
	fi
	selected=$((selected + 1))
	repo_name="$(basename "$source_repo")"
	if [ -n "$oracle_dir" ]; then
		repo_name="${repo_name}-oracle"
		[ "$expect" = reject ] && repo_name="${repo_name}-reject"
	fi
	ticket_id="${ticket_prefix}-$(date +%Y%m%d-%H%M%S)"
	clone_dir="$SCRATCH/$repo_name"
	data_dir="$SCRATCH/data-$repo_name"

	if [ ! -d "$source_repo" ]; then
		echo "SKIP $repo_name: source repo not found at $source_repo"
		results="$results\nSKIP  $repo_name (source repo missing)"
		continue
	fi
	if [ ! -f "$spec_file" ]; then
		echo "SKIP $repo_name: ticket spec not found at $spec_file"
		results="$results\nSKIP  $repo_name (ticket spec missing)"
		continue
	fi

	echo "=== $repo_name: fresh repo from $source_repo into $clone_dir ==="
	"$REPO_ROOT/scripts/fixture-repo.sh" "$source_repo" "$clone_dir"

	echo "=== $repo_name: running ticket $ticket_id ==="
	# -preflight-profile brownfield: found on this script's own first real
	# run, 2026-09-17 -- the default (strict) preflight
	# requires spec/spec.md, spec/contract.md, or ARCHITECTURE.md, none of
	# which these bare fixture repos have (and never will, being fresh
	# disposable clones each run); brownfield skips that requirement
	# without disabling the rest of the preflight the way -skip-project-check
	# would.
	# The oracle directory must live outside -workspace (the agent can write
	# the workspace); a per-run copy under $SCRATCH keeps the fixture pristine.
	set -- -ticket "$ticket_id" -spec "$spec_file" -workspace "$clone_dir" \
		-data-dir "$data_dir" -preflight-profile brownfield -open-pull-request=false \
		-code-review-policy "$LIVE_SMOKE_CODE_REVIEW_POLICY"
	if [ -n "${LIVE_SMOKE_TEMPORAL:-}" ]; then
		set -- "$@" -temporal-address "$LIVE_SMOKE_TEMPORAL"
	fi
	if [ -n "${LIVE_SMOKE_CONFIG:-}" ]; then
		set -- "$@" -config "$LIVE_SMOKE_CONFIG"
	fi
	if [ -n "$oracle_dir" ]; then
		oracle_copy="$SCRATCH/oracle-$repo_name"
		mkdir -p "$oracle_copy" && cp "$oracle_dir"/* "$oracle_copy"/
		set -- "$@" -reference-oracle-dir "$oracle_copy" -reference-oracle-mount-path .oracle \
			-reference-oracle-command "$(cat "$oracle_copy/RUN_COMMAND.txt")"
	fi
	run_start="$(date +%s)"
	if "$FACTORYD_BIN" "$@"; then
		run_exit=0
	else
		run_exit=$?
	fi
	duration_s=$(($(date +%s) - run_start))

	run_json="$(find "$data_dir/runs" -maxdepth 1 -name "${ticket_id}*" -type d 2>/dev/null | head -1)/run.json"
	state="unknown"
	if [ -f "$run_json" ]; then
		state="$(python3 -c "import json,sys; print(json.load(open(sys.argv[1])).get('state','unknown'))" "$run_json" 2>/dev/null || echo "unknown")"
	fi

	# ok=1 only when the run met this fixture's expectation.
	ok=0
	problems=""
	if [ -z "$oracle_dir" ]; then
		[ "$state" = "accepted" ] || problems="state=$state, want accepted"
	else
		target_path="$(python3 -c "import json,sys; print(json.load(open(sys.argv[1]))[0]['target_path'])" "$oracle_dir/MANIFEST.json")"
		# factoryd isolates each run in its own worktree branch (recorded in
		# run.json), so the result lives on that branch, not at the clone's HEAD.
		# Git object reads only; nothing from the result tree is executed.
		branch="$(python3 -c "import json,sys; print(json.load(open(sys.argv[1])).get('branch') or '')" "$run_json" 2>/dev/null || true)"
		ref="${branch:-HEAD}"
		has_oracle=0
		has_index=0
		git -C "$clone_dir" cat-file -e "$ref:$target_path" 2>/dev/null && has_oracle=1
		git -C "$clone_dir" cat-file -e "$ref:.buildgate/oracles.json" 2>/dev/null && has_index=1
		# Prints one line per failed check; empty output means every check held.
		problems="$(python3 - "$run_json" "$expect" "$target_path" <<'PY'
import json, sys
path, expect, target = sys.argv[1:4]
try:
    r = json.load(open(path))
except Exception as e:
    print("run.json unreadable: %s" % e); sys.exit(0)
gates = [g for g in r.get("gate_results") or [] if g.get("check") == "reference_oracle"]
attempts = r.get("attempts") or []
gate = gates[-1] if gates else None
if expect == "reject":
    if r.get("state") != "quarantined":
        print("state=%s, want quarantined" % r.get("state"))
    if not gate or gate.get("passed"):
        print("reference_oracle gate not recorded as failed")
    oa = [a for a in attempts if a.get("kind") == "reference_oracle"]
    try:
        log = open(oa[-1]["log_path"], errors="replace").read()
    except Exception as e:
        log = ""
        print("reference_oracle log unreadable: %s" % e)
    if not log:
        print("reference_oracle log is empty: cannot tell an assertion failure from an environment failure")
    elif "AssertionError" not in log and "FAILED (failures=" not in log:
        print("reference_oracle failed but not with a test assertion failure (environment error?)")
else:
    if r.get("state") != "accepted":
        print("state=%s, want accepted" % r.get("state"))
    if not gate or not gate.get("passed") or not gate.get("reference_oracle_sha256"):
        print("reference_oracle gate not recorded as passed with an oracle hash")
    if (r.get("oracle_canary") or {}).get("verdict") != "TRUSTWORTHY":
        print("oracle_canary verdict is not TRUSTWORTHY")
    if not any(a.get("kind") == "verify_after_oracle_commit" and a.get("exit_code") == 0 for a in attempts):
        print("no passing verify_after_oracle_commit attempt")
    authored = [a.get("path") for a in (r.get("oracles") or {}).get("authored") or []]
    for want in (target, ".buildgate/oracles.json"):
        if want not in authored:
            print("run.json oracles.authored lacks %s" % want)
PY
)"
		if [ "$expect" = reject ]; then
			[ "$has_oracle" = 0 ] || problems="$problems
oracle test $target_path reached the result commit ($ref) of a run that must not commit it"
			[ "$has_index" = 0 ] || problems="$problems
.buildgate/oracles.json reached the result commit ($ref) of a run that must not commit it"
		else
			[ "$has_oracle" = 1 ] || problems="$problems
oracle test $target_path missing from the result commit ($ref)"
			[ "$has_index" = 1 ] || problems="$problems
.buildgate/oracles.json missing from the result commit ($ref)"
		fi
	fi
	if ! python3 -c "import json,sys; sys.exit(0 if json.load(open(sys.argv[1])).get('temporal_workflow_id') else 1)" "$run_json" 2>/dev/null; then
		problems="$problems
Temporal path expected but run.json has no temporal_workflow_id (did the run reach Temporal?)"
	fi
	problems="$(printf '%s' "$problems" | sed '/^$/d')"
	if [ -z "$problems" ]; then ok=1; else printf '%s\n' "$problems"; fi

	python3 -c '
import json, sys
date, sha, ver, fixture, outcome, expected, dur = sys.argv[1:8]
row = {"date": date, "git_sha": sha, "factoryd_version": ver, "fixture": fixture,
       "outcome": outcome, "expected": expected, "duration_s": int(dur)}
with open(sys.argv[8], "a") as f:
    f.write(json.dumps(row) + "\n")
' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$GIT_SHA" "$FACTORYD_VERSION" "$repo_name" "$state" "$expect" "$duration_s" "$RESULTS_FILE"

	if [ "$ok" = 1 ]; then
		echo "PASS $repo_name (state=$state, expect=$expect)"
		results="$results\nPASS  $repo_name"
		pass=$((pass + 1))
	else
		kept="$KEEP_DIR/failed-$ticket_id"
		mkdir -p "$kept" && cp -R "$data_dir/runs" "$kept/" 2>/dev/null || true
		echo "FAIL $repo_name (state=$state, expect=$expect, factoryd exit=$run_exit) -- evidence kept at $kept"
		results="$results\nFAIL  $repo_name (state=$state, expect=$expect)"
		fail=$((fail + 1))
	fi
done

if [ "$selected" -eq 0 ]; then
	echo "live-smoke: no fixture selected (LIVE_SMOKE_ONLY='${LIVE_SMOKE_ONLY:-}'); see --list" >&2
	exit 1
fi

echo ""
echo "=== live-smoke summary: $pass passed, $fail failed ==="
printf '%b\n' "$results"

if [ "$fail" -gt 0 ]; then
	exit 1
fi
