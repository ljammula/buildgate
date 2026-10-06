#!/bin/sh
# stub_factoryd_pg.sh: a fake `factoryd` binary for
# scripts/tests/test_proving_ground.py's RecordingTest -- makes no
# Docker/model/git calls. Understands just enough of the real CLI surface
# for scripts/proving-ground.sh's own result-recording logic to exercise:
# `version`, and a direct-path invocation that writes a caller-chosen
# run.json verbatim where proving-ground.sh expects to find it.
#
# Sibling of scripts/tests/fixtures/stub_factoryd.sh (live-smoke.sh's own
# stub), which always writes a fixed minimal run.json; this one instead
# writes STUB_FACTORYD_PG_RUN_JSON's literal content, so a single test can
# exercise every classify()/cause_bucket() outcome (accepted, quarantined
# on a named gate, halted with a reason code, ...) through the real shell
# script's recording plumbing, not just "accepted"/"halted".
set -eu

if [ "${1:-}" = "version" ]; then
	echo "factoryd stub-test-version"
	exit 0
fi

ticket=""
data_dir=""
while [ $# -gt 0 ]; do
	case "$1" in
	-ticket)
		ticket="$2"
		shift 2
		;;
	-data-dir)
		data_dir="$2"
		shift 2
		;;
	*)
		shift
		;;
	esac
done

if [ -z "$ticket" ] || [ -z "$data_dir" ]; then
	echo "stub_factoryd_pg.sh: -ticket and -data-dir are required" >&2
	exit 1
fi

run_dir="$data_dir/runs/$ticket"
mkdir -p "$run_dir"
printf '%s' "${STUB_FACTORYD_PG_RUN_JSON:-{\"state\": \"accepted\", \"gate_results\": []\}}" >"$run_dir/run.json"
