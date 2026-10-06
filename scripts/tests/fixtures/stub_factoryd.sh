#!/bin/sh
# stub_factoryd.sh: a fake `factoryd` binary for
# scripts/tests/test_live_smoke_recording.py -- makes no Docker/model/git
# calls. Understands just enough of the real CLI surface for
# scripts/live-smoke.sh's own result-recording logic to exercise: `version`,
# and a run invocation that writes a minimal run.json (with a Temporal
# workflow id, unless STUB_FACTORYD_NO_WORKFLOW=1) where live-smoke.sh expects
# to find it.
#
# The run's outcome is controlled by STUB_FACTORYD_STATE (default
# "accepted"), read from the environment so the test can also exercise a
# non-accepted outcome without a second copy of this script.
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
	echo "stub_factoryd.sh: -ticket and -data-dir are required" >&2
	exit 1
fi

run_dir="$data_dir/runs/$ticket"
mkdir -p "$run_dir"
state="${STUB_FACTORYD_STATE:-accepted}"
workflow='"temporal_workflow_id": "stub-workflow", '
if [ "${STUB_FACTORYD_NO_WORKFLOW:-}" = 1 ]; then
	workflow=""
fi
cat >"$run_dir/run.json" <<EOF
{$workflow"state": "$state"}
EOF
