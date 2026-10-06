#!/bin/sh
# proving-ground.sh: the standing proving-ground corpus (`make
# proving-ground`) -- AGENTS.md's "Live validation" section. Drives a FIXED
# corpus of pre-approved ticket specs (scripts/proving-ground/fixtures.json)
# through factoryd's direct single-ticket path, one fixture at a time, the
# same shape scripts/live-smoke.sh and scripts/bar.sh already use: fresh,
# disposable clones, -ticket/-spec/-workspace/-data-dir, no drafting, no
# human-approval gate (see live-smoke.sh's own doc comment for why this is
# deliberately NOT the submit/worker request-driver path).
#
# Unlike live-smoke.sh (pass/fail on a 2-fixture smoke test) this is a
# MEASUREMENT over a ~10-fixture corpus across several repos, including
# deliberate should-quarantine fixtures: every run is auto-classified
# (scripts/proving_ground_lib.py's classify()/cause_bucket()) against the
# fixture's declared `expect`, and the plan's own "100% confidence" bar
# (false accepts, factoryd-caused false quarantines, non-acceptance
# classification coverage, one-shot acceptance rate) is reported at the
# end. See that library's own doc comment for the classifier rules and
# which cause buckets are heuristic.
#
# PROVING_GROUND_TEMPORAL=<addr>: run every fixture against this Temporal
# server (-temporal-address <addr>) instead of factoryd's default (localhost:
# 7233, started with Docker when down).
#
# PROVING_GROUND_ONLY=<substring>: only fixtures whose label contains it.
# PROVING_GROUND_CONFIG=<path>: pass -config <path> to every fixture's factoryd
# run and read the route and execution harness from that file instead of the
# default session config -- how the same corpus is measured under different
# roles.<role>.harness settings (each run's rows record the harness).
# PROVING_GROUND_ALLOW_DIRTY=1: run on a dirty tree (recorded sha gets a
# "-dirty" suffix) -- refused otherwise, same rule as scripts/bar.sh, so a
# result is always attributable to a commit.
# PROVING_GROUND_FIXTURES_FILE: overrides the fixtures file wholesale --
# used only by scripts/tests/test_proving_ground.py to drive this script
# against a throwaway fixture and a stub FACTORYD_BIN, offline.
# PROVING_GROUND_EXTRA_FIXTURES=<file>: appends that file's fixtures (same
# schema; relative paths resolve against its directory) -- for fixtures on
# repos that are not public, kept outside this repo.
# PROVING_GROUND_RESULTS_FILE: overrides the results JSONL path (default
# ~/buildgate/proving-ground/results.jsonl).
#
# Prerequisites (not checked here, same as live-smoke.sh/bar.sh): a
# `factoryd` built from the commit under test (FACTORYD_BIN override), a
# working model route and sandbox runtime (`factoryd doctor` all-green), Docker
# running, and each fixture's source repo present (testdata/fixtures/ for
# this repo's own; a missing repo SKIPs that fixture with a clear message
# rather than failing the run).
#
# Usage: scripts/proving-ground.sh [--list | --results [N] | --validate]
# Exit code: 0 unless at least one run was a false accept (the plan's
# criterion 1) or no fixture was selected; nonzero otherwise. A false
# quarantine, an unclassified outcome, or a SKIP does not by itself fail
# the exit code -- read the printed bar, this is a measurement, not a
# pass/fail gate (same convention as scripts/bar.sh).

set -eu

FACTORYD_BIN="${FACTORYD_BIN:-factoryd}"
REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
LIB="$REPO_ROOT/scripts/proving_ground_lib.py"
FIXTURES_FILE="${PROVING_GROUND_FIXTURES_FILE:-$REPO_ROOT/scripts/proving-ground/fixtures.json}"
RESULTS_FILE="${PROVING_GROUND_RESULTS_FILE:-$HOME/buildgate/proving-ground/results.jsonl}"
KEEP_DIR="$HOME/buildgate/proving-ground"

if [ "${1:-}" = "--validate" ]; then
	python3 "$LIB" validate "$FIXTURES_FILE" --repo-root "$REPO_ROOT"
	exit 0
fi

if [ "${1:-}" = "--list" ]; then
	python3 "$LIB" list "$FIXTURES_FILE" --repo-root "$REPO_ROOT"
	exit 0
fi

if [ "${1:-}" = "--results" ]; then
	n="${2:-20}"
	if [ ! -f "$RESULTS_FILE" ]; then
		echo "proving-ground: no results recorded yet at $RESULTS_FILE" >&2
		exit 1
	fi
	python3 "$LIB" report "$RESULTS_FILE" --recent "$n"
	exit 0
fi

mkdir -p "$KEEP_DIR"
mkdir -p "$(dirname "$RESULTS_FILE")"
# Scratch under $HOME, not /tmp: a Docker backend whose VM only shares
# $HOME accepts a bind mount under /tmp without complaint but the
# container can't see anything written there -- same trap documented in
# live-smoke.sh and bar.sh.
SCRATCH="$(mktemp -d "$KEEP_DIR/run.XXXXXX")"
trap 'rm -rf "$SCRATCH"' EXIT

# Refuse a dirty tree: the recorded sha must be the code that produced
# these results. Same rule as scripts/bar.sh.
GIT_SHA="$(git -C "$REPO_ROOT" rev-parse --short HEAD 2>/dev/null || echo unknown)"
if [ -n "$(git -C "$REPO_ROOT" status --porcelain 2>/dev/null || true)" ]; then
	if [ "${PROVING_GROUND_ALLOW_DIRTY:-}" != 1 ]; then
		echo "proving-ground: working tree is dirty; results must be attributable to a commit. Commit or stash, or set PROVING_GROUND_ALLOW_DIRTY=1 (recorded as ${GIT_SHA}-dirty)." >&2
		exit 3
	fi
	GIT_SHA="${GIT_SHA}-dirty"
fi
FACTORYD_VERSION="$("$FACTORYD_BIN" version 2>/dev/null | head -1 || echo unknown)"

# Model route: credential mode + worker model id for roles.execution,
# read from the effective session config's own routes:/models:/roles:
# block (internal/sessionconfig.DefaultPaths -- $XDG_CONFIG_HOME/
# factoryd/config.yml, ~/.config when unset, else ~/.factory/config.yml;
# routes:/models:/roles: is the only session-config schema). Read
# directly rather than via `factoryd doctor`: doctor has no
# machine-readable output and performs live checks (network, Docker) this
# script has no business triggering just to learn which route is
# configured. "unknown" when no config file is found, or when
# roles.execution isn't configured -- a route override purely by flags on
# FACTORYD_BIN's own invocation (not this script's concern) would then
# read as unknown too; that's the honest answer.
#
# Parsed by proving_ground_lib.py's model-route (PyYAML; "unknown" when
# PyYAML is missing), so any valid YAML style resolves.
session_config() {
	if [ -n "${PROVING_GROUND_CONFIG:-}" ]; then
		echo "$PROVING_GROUND_CONFIG"
		return
	fi
	xdg="${XDG_CONFIG_HOME:-$HOME/.config}"
	cfg="$xdg/factoryd/config.yml"
	[ -f "$cfg" ] || cfg="$HOME/.factory/config.yml"
	echo "$cfg"
}
model_route() {
	cfg="$(session_config)"
	if [ ! -f "$cfg" ]; then
		echo "unknown"
		return
	fi
	python3 "$LIB" model-route "$cfg"
}
execution_harness() {
	cfg="$(session_config)"
	if [ ! -f "$cfg" ]; then
		echo "unknown"
		return
	fi
	python3 "$LIB" harness "$cfg"
}
if [ -n "${PROVING_GROUND_CONFIG:-}" ] && [ ! -f "$PROVING_GROUND_CONFIG" ]; then
	echo "proving-ground: PROVING_GROUND_CONFIG=$PROVING_GROUND_CONFIG is not a file" >&2
	exit 1
fi
MODEL_ROUTE="$(model_route)"
HARNESS="$(execution_harness)"

RUN_PATHS="temporal"

field() { # <label> <name>
	python3 "$LIB" field "$FIXTURES_FILE" --repo-root "$REPO_ROOT" "$1" "$2"
}

selected=0
false_accepts=0
false_quarantines=0
run_records=""

for fx in $(python3 "$LIB" labels "$FIXTURES_FILE" --repo-root "$REPO_ROOT" --only "${PROVING_GROUND_ONLY:-}"); do
	selected=$((selected + 1))
	repo="$(field "$fx" repo)"
	spec="$(field "$fx" spec)"
	expect="$(field "$fx" expect)"
	oracle_dir="$(field "$fx" oracle_dir)"

	if [ ! -d "$repo" ]; then
		echo "SKIP $fx: source repo not found at $repo"
		continue
	fi
	if [ ! -f "$spec" ]; then
		echo "SKIP $fx: ticket spec not found at $spec"
		continue
	fi
	if [ -n "$oracle_dir" ] && [ ! -d "$oracle_dir" ]; then
		echo "SKIP $fx: oracle dir not found at $oracle_dir"
		continue
	fi

	for run_path in $RUN_PATHS; do
		label="$fx"
		ticket_id="${fx}-$(date +%Y%m%d-%H%M%S)"
		clone_dir="$SCRATCH/$label-$run_path"
		data_dir="$SCRATCH/data-$label-$run_path"

		echo "=== $label ($run_path): cloning $repo ==="
		"$REPO_ROOT/scripts/fixture-repo.sh" "$repo" "$clone_dir"
		# Pinned base: the corpus must not drift as the target repos gain the
		# very features its tickets ask for (baseline 2026-09-24: three
		# fixtures quarantined because their feature already existed at HEAD).
		git -C "$clone_dir" checkout --quiet -B proving-ground-base "$(field "$fx" base_ref)"

		echo "=== $label ($run_path): running ticket $ticket_id (expect $expect) ==="
		# -preflight-profile brownfield: these fixture repos are fresh
		# disposable clones with no spec/spec.md, spec/contract.md, or
		# ARCHITECTURE.md the strict default preflight requires -- same
		# reasoning as live-smoke.sh's own use of this flag.
		set -- -ticket "$ticket_id" -spec "$spec" -workspace "$clone_dir" \
			-data-dir "$data_dir" -preflight-profile brownfield -open-pull-request=false
		if [ -n "${PROVING_GROUND_TEMPORAL:-}" ]; then
			set -- "$@" -temporal-address "$PROVING_GROUND_TEMPORAL"
		fi
		if [ -n "${PROVING_GROUND_CONFIG:-}" ]; then
			set -- "$@" -config "$PROVING_GROUND_CONFIG"
		fi
		if [ -n "$oracle_dir" ]; then
			# Per-run copy outside -workspace: the agent can write the
			# workspace, so the oracle must live somewhere it cannot reach.
			oracle_copy="$SCRATCH/oracle-$label-$run_path"
			mkdir -p "$oracle_copy" && cp "$oracle_dir"/* "$oracle_copy"/
			set -- "$@" -reference-oracle-dir "$oracle_copy" -reference-oracle-mount-path .oracle \
				-reference-oracle-command "$(cat "$oracle_copy/RUN_COMMAND.txt")"
		fi

		run_start="$(date +%s)"
		"$FACTORYD_BIN" "$@" || true
		duration_s=$(($(date +%s) - run_start))

		run_json="$(find "$data_dir/runs" -maxdepth 1 -name "${ticket_id}*" -type d 2>/dev/null | head -1)/run.json"
		[ -f "$run_json" ] || run_json="-"

		record="$(python3 "$LIB" record --run-json "$run_json" --date "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
			--git-sha "$GIT_SHA" --factoryd-version "$FACTORYD_VERSION" --model-route "$MODEL_ROUTE" --harness "$HARNESS" \
			--fixture "$label" --path "$run_path" --expect "$expect" --duration-s "$duration_s")"
		echo "$record" >>"$RESULTS_FILE"
		run_records="$run_records
$record"

		category="$(printf '%s' "$record" | python3 -c 'import json,sys; print(json.load(sys.stdin)["category"])')"
		cause="$(printf '%s' "$record" | python3 -c 'import json,sys; print(json.load(sys.stdin)["cause_bucket"])')"
		echo "$label ($run_path): $category (cause=$cause)"

		if [ "$category" = "false_accept" ]; then
			false_accepts=$((false_accepts + 1))
			kept="$KEEP_DIR/failed-$ticket_id"
			mkdir -p "$kept" && cp -R "$data_dir/runs" "$kept/" 2>/dev/null || true
			echo "  ** FALSE ACCEPT -- evidence kept at $kept"
		fi
		case "$category" in
		unexpected_quarantine:*)
			if [ "$cause" = "factoryd_bug_suspect" ]; then
				false_quarantines=$((false_quarantines + 1))
				kept="$KEEP_DIR/failed-$ticket_id"
				mkdir -p "$kept" && cp -R "$data_dir/runs" "$kept/" 2>/dev/null || true
				echo "  ** SUSPECTED FACTORYD-CAUSED FALSE QUARANTINE -- evidence kept at $kept"
			fi
			;;
		esac
	done
done

if [ "$selected" -eq 0 ]; then
	echo "proving-ground: no fixture selected (PROVING_GROUND_ONLY='${PROVING_GROUND_ONLY:-}'); see --list" >&2
	exit 1
fi

echo ""
echo "=== proving-ground summary ($GIT_SHA, $FACTORYD_VERSION, route=$MODEL_ROUTE, harness=$HARNESS) ==="
tmp_records="$SCRATCH/records-this-run.jsonl"
printf '%s\n' "$run_records" | sed '/^$/d' >"$tmp_records"
python3 "$LIB" report "$tmp_records" --recent 1

if [ "$false_accepts" -gt 0 ]; then
	echo "proving-ground: $false_accepts false accept(s) -- exit 1" >&2
	exit 1
fi
