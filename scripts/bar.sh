#!/bin/bash
# bar.sh: the standing entry point for the staged-oracle default-on bar
# (`make bar`) -- the measurement that decided, on 2026-09-21, that
# `-draft-oracles` stays opt-in. It replaces an ad-hoc harness that lived
# outside the repo. See AGENTS.md's "Live
# validation" section; unlike live-smoke.sh this is a measurement with a
# human in the loop, not a pass/fail gate, and takes hours, not minutes.
#
# What it does: builds factoryd from THIS tree (recorded by commit SHA; a dirty
# tree is refused unless BAR_ALLOW_DIRTY=1, so a result is always attributable
# to a commit), then runs BAR_ROUNDS rounds (default 2) of the fixture list in
# scripts/bar/fixtures.json. A round runs every fixture twice -- flag off, then
# flag on (`factoryd submit -draft-oracles`) -- ONE RUN AT A TIME: the local
# model is single-instance and concurrent runs distort every timing and
# failure. Each run is a real `factoryd submit` + `worker` on a fresh,
# disposable clone; spec_review and plan_review are auto-approved, and
# oracle_review is NEVER auto-approved (see below).
#
# oracle_review: interactive by default. The script prints the drafted oracle
# directory and the exact `factoryd approve` command, then waits (BAR_ORACLE_WAIT
# seconds) for you to read the oracle, author RUN_COMMAND.txt if the drafter
# gave none (multi-file oracles), fix defects, and approve. Whether you edited
# the drafted files is recorded (hash before vs at approval) because an oracle
# that needed fixing is a criterion-2 caveat. BAR_ORACLE_HOOK=<script> automates
# this for a hand-written oracle: it is run as `<script> <run-dir> <request-id>`
# (replace <run-dir>/data/requests/<id>/oracle with the hand-written files, MANIFEST.json
# and RUN_COMMAND.txt; build manifest criterion text with i.strip() -- NOT
# whitespace-collapsing -- or the criterion-text match against spec.md
# refuses it), then approves. A fixture may name its own "oracle_hook" in the
# fixtures file, e.g. a hand-written hard oracle meant to make the in-loop retry
# fire; repeat such a fixture over rounds until criterion 4 is proven.
#
# Mutation check: for a flag-on run that was accepted, a fixture with a
# "mutants" file (a MUTS=[(name, old, new), ...] Python list) gets each mutant
# applied to the built implementation ("impl") and the accepted oracle run via
# RUN_COMMAND.txt; a good oracle KILLS every mutant. The unmutated tree must
# first PASS the oracle, else the check is INVALID (a run that fails for
# environment reasons would otherwise "kill" everything), and a mutant that
# does not compile is INVALID, not KILLED. This executes agent-built code, so
# it runs ONLY inside Docker (--network none, all caps dropped, non-root), never
# on the host; module dependencies come from the host's module cache, mounted
# read-only and filled by `go mod download` in the pristine clone.
# BAR_MUTATION_IMAGE overrides the image (default: the canonical worker image in
# internal/sandbox/canonical_image.go). Mutants are exact-text replacements, so
# one written against a different implementation reports INVALID (did not
# apply) rather than a verdict.
#
# Output: a dated markdown results table plus a verdict on the four bar criteria
# (zero false passes; zero false gates; acceptance with flag >= without; one
# recovered in-loop retry), each MET, UNMET or UNPROVEN, to BAR_OUT (default
# ~/buildgate/bar/bar-<date>-<sha>.md). Per-run records go to
# the same path with .records.jsonl, written as each run finishes, so an
# interrupted bar keeps what it measured and the report can be regenerated with
# scripts/bar_lib.py report. Evidence (runs/, requests/, states.log) of any
# run that was not accepted or whose mutants survived is kept under
# ~/buildgate/bar/failed-<label>-<stamp>/.
#
# Environment:
#   BAR_ROUNDS=2          rounds (positive integer)
#   BAR_EXTRA_FIXTURES    a fixtures file appended to scripts/bar/fixtures.json
#                         (same schema; relative paths resolve against its
#                         directory), for fixtures on repos that are not public
#   BAR_ONLY=<substr>     only fixtures whose label contains it
#   BAR_ORACLE_HOOK=path  automate oracle_review (see above)
#   BAR_ORACLE_WAIT=3600  seconds to wait for you at oracle_review, per run
#   BAR_TEMPORAL=localhost:7233   Temporal address for the worker (required; empty is an error)
#   BAR_TIMEOUT=4200      seconds per run, excluding time at oracle_review
#   BAR_ALLOW_DIRTY=1     run on a dirty tree (result is recorded as "-dirty")
#   BAR_OUT=path          markdown results file
#   BAR_MUTATION_IMAGE    image for the mutation check
# Prerequisites (not checked here): Go, Docker, `factoryd doctor` all-green, the
# fixture source repos on disk, and Temporal up (`make temporal-up`) at
# BAR_TEMPORAL.
#
# Usage: scripts/bar.sh [--list]
# Exit code: 0 unless a criterion is UNMET (UNPROVEN alone exits 0 -- read the
# verdict), 2 on bad arguments, 3 on a refused dirty tree, 1 on a setup failure.

set -eu

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
FIXTURES_FILE="$REPO_ROOT/scripts/bar/fixtures.json"
LIB="$REPO_ROOT/scripts/bar_lib.py"
ROUNDS="${BAR_ROUNDS:-2}"
TEMPORAL="${BAR_TEMPORAL-localhost:7233}"
# The bar controls exactly which processes run: submit must not start its own
# worker or serve, and the worker must not start Temporal.
export FACTORYD_AUTOSTART=0
RUN_TIMEOUT="${BAR_TIMEOUT:-4200}"
ORACLE_WAIT="${BAR_ORACLE_WAIT:-3600}"

usage() {
	echo "usage: scripts/bar.sh [--list]   (configuration is by BAR_* environment variables; see the top of this file)" >&2
}

case "${1:-}" in
"") ;;
--list) ;;
-h | --help)
	usage
	exit 0
	;;
*)
	echo "bar: unknown argument '$1'" >&2
	usage
	exit 2
	;;
esac
if [ "$#" -gt 1 ]; then
	echo "bar: too many arguments" >&2
	usage
	exit 2
fi
case "$ROUNDS" in
"" | *[!0-9]* | 0)
	echo "bar: BAR_ROUNDS must be a positive integer, got '$ROUNDS'" >&2
	exit 2
	;;
esac

if [ "${1:-}" = "--list" ]; then
	echo "rounds=$ROUNDS (BAR_ROUNDS); each round runs every fixture flag-off then flag-on, one at a time"
	python3 "$LIB" list "$FIXTURES_FILE"
	exit 0
fi

# The worker is the only request driver and needs Temporal; an empty address
# is an error.
if [ -z "$TEMPORAL" ]; then
	echo "bar: BAR_TEMPORAL is empty; the worker needs a Temporal address (default localhost:7233; start it with 'make temporal-up')" >&2
	exit 1
fi

# Refuse a dirty tree: the recorded SHA must be the code that ran. Untracked
# files count -- a stray .go file changes the build.
SHA="$(git -C "$REPO_ROOT" rev-parse --short HEAD)"
BINARY_NOTE="built from a clean tree"
if [ -n "$(git -C "$REPO_ROOT" status --porcelain)" ]; then
	if [ "${BAR_ALLOW_DIRTY:-}" != 1 ]; then
		echo "bar: working tree is dirty; results must be attributable to a commit. Commit or stash, or set BAR_ALLOW_DIRTY=1 (recorded as ${SHA}-dirty)." >&2
		exit 3
	fi
	SHA="${SHA}-dirty"
	BINARY_NOTE="built from a DIRTY tree (BAR_ALLOW_DIRTY=1)"
fi

DATE="$(date +%F)"
KEEP_DIR="$HOME/buildgate/bar"
mkdir -p "$KEEP_DIR"
OUT="${BAR_OUT:-$KEEP_DIR/bar-$DATE-$SHA.md}"
RECORDS="${OUT%.md}.records.jsonl"
: >"$RECORDS"
# Scratch under $HOME, not /tmp: Docker's VM on macOS may not share /tmp (see
# live-smoke.sh's note), which makes a bind mount silently empty.
SCRATCH="$(mktemp -d "$KEEP_DIR/run.XXXXXX")"
QUEUE_PID=""
# shellcheck disable=SC2329 # invoked via trap
cleanup() {
	[ -n "$QUEUE_PID" ] && kill "$QUEUE_PID" 2>/dev/null || true
	rm -rf "$SCRATCH"
}
trap cleanup EXIT

BIN="$SCRATCH/factoryd"
echo "=== bar: building factoryd at $SHA ==="
(cd "$REPO_ROOT" && go build -o "$BIN" ./cmd/factoryd)

state_of() { # <run dir> <request id>
	python3 -c "import json,sys; print(json.load(open(sys.argv[1]))['state'])" "$1/data/requests/$2/request.json" 2>/dev/null || true
}

# mutation_check <run dir> <request id> <workspace> <impl> <mutants file> <out json>
mutation_check() {
	local d="$1" id="$2" ws="$3" impl="$4" muts="$5" out="$6"
	local oracle="$d/data/requests/$id/oracle" image tsv="$d/mutants.tsv" n i t name status rc
	image="${BAR_MUTATION_IMAGE:-$(grep -oE '"[a-z0-9.-]+/[a-zA-Z0-9._/-]+@sha256:[0-9a-f]+"' "$REPO_ROOT/internal/sandbox/canonical_image.go" | tr -d '"' | head -1)}"
	: >"$tsv"
	if [ -z "$image" ] || [ ! -f "$oracle/RUN_COMMAND.txt" ]; then
		printf '(setup)\tINVALID\n' >>"$tsv"
	else
		# The container has no network, so module dependencies come from the
		# host's module cache, mounted read-only. It is filled from the
		# pristine clone ($d/repo), not the agent-built tree; `go mod download`
		# only fetches, it never executes module code.
		local modcache
		modcache="$(go env GOMODCACHE)"
		git -C "$d/repo" ls-files '*go.mod' | while IFS= read -r m; do
			(cd "$d/repo/$(dirname "$m")" && go mod download) || true
		done
		# The build commits the accepted oracle into the repo (MANIFEST.json
		# target_path); those copies are removed before the .oracle overlay,
		# or every TestOracle* would be declared twice and nothing compiles.
		local committed
		committed="$(python3 -c 'import json,sys; print("\n".join(e["target_path"] for e in json.load(open(sys.argv[1])) if e.get("target_path")))' "$oracle/MANIFEST.json")"
		# run_in_container <tree>: run RUN_COMMAND.txt in the tree; exit status
		# is the oracle's, except 2 when the tree did not build (a mutant that
		# breaks compilation is INVALID, not KILLED). GOTMPDIR: the canonical
		# image sets it to /home/worker, unwritable as the host uid.
		run_in_container() {
			local out rc=0
			out="$(docker run --rm --network none --cap-drop ALL --security-opt no-new-privileges \
				--user "$(id -u):$(id -g)" -e HOME=/tmp -e GOCACHE=/tmp/gocache -e GOTMPDIR=/tmp \
				-v "$modcache:/hostmod:ro" -e GOMODCACHE=/hostmod -e GOPROXY=off \
				-v "$1:/work" -w /work --entrypoint bash "$image" -c "$(cat "$oracle/RUN_COMMAND.txt")" 2>&1)" || rc=$?
			if [ "$rc" != 0 ] && printf '%s\n' "$out" | grep -qE '\[(build|setup) failed\]'; then return 2; fi
			return "$rc"
		}
		prepare() { # <tree>
			rm -rf "$1" && cp -R "$ws" "$1" || return 1
			while IFS= read -r p; do [ -n "$p" ] && rm -f "$1/$p"; done <<<"$committed"
			mkdir -p "$1/.oracle" && cp "$oracle"/* "$1/.oracle/"
		}
		t="$d/mut-tree"
		prepare "$t"
		if run_in_container "$t"; then
			n="$(python3 "$LIB" mutants-count "$muts")"
			for i in $(seq 0 $((n - 1))); do
				prepare "$t"
				if name="$(python3 "$LIB" mutant-apply "$ws/$impl" "$t/$impl" "$muts" "$i")"; then
					rc=0
					run_in_container "$t" || rc=$?
					case "$rc" in 0) status=SURVIVED ;; 2) status=INVALID ;; *) status=KILLED ;; esac
				else
					name="mutant-$i"
					status=INVALID
				fi
				printf '%s\t%s\n' "$name" "$status" >>"$tsv"
				echo "  mutant[$name]: $status"
			done
		else
			printf '(baseline: oracle fails on the unmutated build)\tINVALID\n' >>"$tsv"
		fi
		rm -rf "$t"
	fi
	python3 -c 'import json,sys; print(json.dumps(dict(l.rstrip("\n").split("\t",1) for l in open(sys.argv[1]) if l.strip())))' "$tsv" >"$out"
}

# run_one <fixture label> <round> <on|off>
run_one() {
	local fx="$1" rnd="$2" flag="$3"
	local label="R${rnd}-${fx}-${flag}" d="$SCRATCH/R${rnd}-${fx}-${flag}"
	local repo request verify full muts impl hook
	repo="$(python3 "$LIB" field "$FIXTURES_FILE" "$fx" repo)"
	case "$repo" in /*) ;; *) repo="$REPO_ROOT/$repo" ;; esac # testdata/fixtures/...
	request="$(python3 "$LIB" field "$FIXTURES_FILE" "$fx" request)"
	verify="$(python3 "$LIB" field "$FIXTURES_FILE" "$fx" verify)"
	full="$(python3 "$LIB" field "$FIXTURES_FILE" "$fx" full_suite)"
	muts="$(python3 "$LIB" field "$FIXTURES_FILE" "$fx" mutants)"
	impl="$(python3 "$LIB" field "$FIXTURES_FILE" "$fx" impl)"
	hook="$(python3 "$LIB" field "$FIXTURES_FILE" "$fx" oracle_hook)"
	hook="${hook:-${BAR_ORACLE_HOOK:-}}"
	if [ ! -d "$repo" ] || [ ! -f "$request" ]; then
		echo "SKIP $label: source repo ($repo) or request file ($request) missing"
		return 0
	fi

	echo "=== $label: cloning $repo ==="
	mkdir -p "$d"
	"$REPO_ROOT/scripts/fixture-repo.sh" "$repo" "$d/repo"
	local flags=(-data-dir "$d/data" -preflight-profile brownfield -verify-command "$verify" -request-file "$request")
	[ -n "$full" ] && flags+=(-full-suite-command "$full")
	[ "$flag" = on ] && flags+=(-draft-oracles)
	local id
	id="$("$BIN" submit "${flags[@]}" "$d/repo" 2>"$d/submit.err" | head -1 | awk '{print $NF}')" || true
	if [ -z "$id" ]; then
		echo "FAIL $label: submit produced no request id:" >&2
		cat "$d/submit.err" >&2
		return 1
	fi
	echo "$id" >"$d/id"

	local qflags=(-data-dir "$d/data" -open-pull-request=false -temporal-address "$TEMPORAL")
	"$BIN" worker "${qflags[@]}" >"$d/queue.log" 2>&1 &
	QUEUE_PID=$!

	local s last="" approved="" start now human=0 human_done=0 oracle="$d/data/requests/$id/oracle"
	local base_hash="" cur_hash="" oracle_modified="" hook_used=0 wait_start="" oracle_seen=0
	start="$(date +%s)"
	while :; do
		s="$(state_of "$d" "$id")"
		if [ "$s" != "$last" ]; then
			echo "$(date +%H:%M:%S) $label: $s" | tee -a "$d/states.log"
			last="$s"
		fi
		case "$s" in
		spec_review | plan_review)
			if [ "$approved" != "$s" ]; then
				if "$BIN" approve -data-dir "$d/data" "$id" >>"$d/approve.log" 2>&1; then
					approved="$s"
				elif [ "$s" = spec_review ] && tail -n 5 "$d/approve.log" | grep -q "decisions left for you"; then
					# The draft left a choice to the operator: nobody is here, so
					# its own recommendation is the answer.
					"$BIN" reject -data-dir "$d/data" -reason "Take the recommended option for every open decision." "$id" >>"$d/approve.log" 2>&1 || sleep 20
				else
					sleep 20
				fi
			fi
			;;
		oracle_review)
			if [ "$oracle_seen" = 0 ]; then
				oracle_seen=1
				wait_start="$(date +%s)"
				base_hash="$(python3 "$LIB" oracle-hash "$oracle")"
				cur_hash="$base_hash"
				if [ -n "$hook" ]; then
					echo "--- $label: oracle_review, running hook $hook"
					"$hook" "$d" "$id"
					hook_used=1
					cur_hash="$(python3 "$LIB" oracle-hash "$oracle")"
					"$BIN" approve -data-dir "$d/data" "$id" >>"$d/approve.log" 2>&1 || true
				else
					cat <<EOF

--- $label: oracle_review -- YOUR turn (not auto-approved) ---
Drafted oracle : $oracle
  Read every file plus MANIFEST.json. If RUN_COMMAND.txt is absent, author it
  (the drafter gives none for multi-file oracles). Fix defects in place.
Approve with   : $BIN approve -data-dir $d/data $id
Reject/abandon : leave it; this run times out after ${ORACLE_WAIT}s and is recorded as such.

EOF
				fi
			elif [ -n "$wait_start" ]; then
				cur_hash="$(python3 "$LIB" oracle-hash "$oracle")"
				if [ $(($(date +%s) - wait_start)) -gt "$ORACLE_WAIT" ]; then
					echo "TIMEOUT $label: no oracle approval within ${ORACLE_WAIT}s" | tee -a "$d/states.log"
					s=timeout
					break
				fi
			fi
			;;
		pr_review | done | quarantined | halted | cancelled)
			echo "terminal: $s" | tee -a "$d/states.log"
			break
			;;
		esac
		# The oracle_review wait is a human's time, not the run's: excluded from the run budget.
		now="$(date +%s)"
		if [ "$oracle_seen" = 1 ] && [ "$s" = oracle_review ]; then
			human=$((human_done + now - wait_start))
		elif [ "$oracle_seen" = 1 ] && [ -n "$wait_start" ]; then
			# Final hash as the oracle leaves review: an edit made within one
			# poll interval of approval is otherwise never sampled.
			cur_hash="$(python3 "$LIB" oracle-hash "$oracle")"
			human_done=$((human_done + now - wait_start))
			human=$human_done
			wait_start=""
		fi
		if [ $((now - start - human)) -gt "$RUN_TIMEOUT" ]; then
			echo "TIMEOUT $label: exceeded ${RUN_TIMEOUT}s" | tee -a "$d/states.log"
			s=timeout
			break
		fi
		sleep 10
	done
	kill "$QUEUE_PID" 2>/dev/null || true
	wait "$QUEUE_PID" 2>/dev/null || true
	QUEUE_PID=""
	local minutes
	minutes="$(python3 -c "import sys; print(round((int(sys.argv[1]) - int(sys.argv[2]) - int(sys.argv[3])) / 60.0, 1))" "$(date +%s)" "$start" "$human")"

	if [ -n "$base_hash" ] && [ "$base_hash" != "$cur_hash" ]; then oracle_modified=1; elif [ -n "$base_hash" ]; then oracle_modified=0; fi

	# Mutation check (flag-on, accepted, fixture has mutants), then the record.
	local sargs=(--dir "$d" --label "$label" --round "$rnd" --flag "$flag" --fixture "$fx" --terminal "${s:-unknown}" --minutes "$minutes")
	[ -n "$oracle_modified" ] && sargs+=(--oracle-modified "$oracle_modified")
	[ "$hook_used" = 1 ] && sargs+=(--hook-used)
	local probe ws
	probe="$(python3 "$LIB" summarise "${sargs[@]}")"
	if [ "$flag" = on ] && [ -n "$muts" ] && [ "$(printf '%s' "$probe" | python3 -c 'import json,sys; print(json.load(sys.stdin)["accepted"])')" = True ]; then
		sargs+=(--mutants-expected)
		ws="$(find "$d/data/workspaces" -mindepth 1 -maxdepth 1 -type d 2>/dev/null | head -1)"
		if [ -n "$ws" ]; then
			echo "--- $label: mutation check ($muts)"
			mutation_check "$d" "$id" "$ws" "$impl" "$muts" "$d/mutants.json"
			sargs+=(--mutants-json "$d/mutants.json")
		fi
	fi
	python3 "$LIB" summarise "${sargs[@]}" >>"$RECORDS"

	# Evidence for anything not cleanly accepted (the run's own state, not the
	# request's, is what "accepted" means).
	local clean
	clean="$(tail -1 "$RECORDS" | python3 -c 'import json,sys; r=json.load(sys.stdin); m=r.get("mutants") or {}; print(int(r["accepted"] and all(v=="KILLED" for v in m.values())))')"
	if [ "$clean" != 1 ]; then
		local kept
		kept="$KEEP_DIR/failed-$label-$(date +%Y%m%d-%H%M%S)"
		mkdir -p "$kept"
		cp -R "$d/data/runs" "$d/data/requests" "$kept/" 2>/dev/null || true
		cp "$d"/*.log "$d"/*.err "$d"/mutants.tsv "$kept/" 2>/dev/null || true
		echo "NOTE $label not cleanly accepted -- evidence kept at $kept"
	fi
	rm -rf "$d"
}

echo "=== bar: $ROUNDS round(s), binary $SHA, records -> $RECORDS ==="
selected=0
for rnd in $(seq 1 "$ROUNDS"); do
	for fx in $(python3 "$LIB" labels "$FIXTURES_FILE" --only "${BAR_ONLY:-}"); do
		selected=$((selected + 1))
		for flag in off on; do
			run_one "$fx" "$rnd" "$flag" || echo "bar: $fx round $rnd flag $flag failed to run; continuing"
		done
	done
done
if [ "$selected" -eq 0 ]; then
	echo "bar: no fixture selected (BAR_ONLY='${BAR_ONLY:-}'); see --list" >&2
	exit 1
fi

if [ "$(wc -l <"$RECORDS")" -eq 0 ]; then
	echo "bar: no run completed; nothing to report" >&2
	exit 1
fi
PATH_NOTE="Path: worker -temporal-address $TEMPORAL, -open-pull-request=false."
echo ""
python3 "$LIB" report "$RECORDS" --sha "$SHA" --date "$DATE" --rounds "$ROUNDS" \
	--binary-note "$BINARY_NOTE" --path-note "$PATH_NOTE" --out "$OUT" || rc=$?
echo "bar: results written to $OUT"
exit "${rc:-0}"
