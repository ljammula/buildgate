#!/bin/sh
# checks.sh: the three test suites a change may need (`make verify`,
# `make agent-pi-test`, `make console-test`), and with CHECKS_LIVE=1 also
# `make live-smoke`, as one command with one verdict.
#
# One after another by default, which is what a 16 GiB Mac carries: verify
# alone runs several race-detector processes. All at once when this machine
# has 32 GiB or more free right now (scripts/parallel-jobs.sh --free).
# CHECKS_PARALLEL=1 or 0 overrides the measurement. Each suite's output goes
# to a file under CHECKS_LOG_DIR (default a fresh temporary directory, named
# at the end); a suite is judged by its exit status.
set -u
repo_root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$repo_root"
logs="${CHECKS_LOG_DIR:-$(mktemp -d "${TMPDIR:-/tmp}/buildgate-checks.XXXXXX")}"
mkdir -p "$logs"

suites="verify agent-pi-test"
command -v npm >/dev/null 2>&1 && suites="$suites console-test" || echo "checks: npm not installed, console-test skipped"
[ "${CHECKS_LIVE:-0}" = 1 ] && suites="$suites live-smoke"

parallel="${CHECKS_PARALLEL:-}"
if [ -z "$parallel" ]; then
	free="$(scripts/parallel-jobs.sh --free 2>/dev/null || echo 0)"
	if [ "$free" -ge 32 ]; then parallel=1; else parallel=0; fi
	echo "checks: ${free} GiB free: running the suites $([ "$parallel" = 1 ] && echo "at once" || echo "one after another") (CHECKS_PARALLEL=0|1 overrides)"
fi

run() {
	start="$(date +%s)"
	status=0
	${MAKE:-make} "$1" >"$logs/$1.log" 2>&1 || status=$?
	echo "$status $(($(date +%s) - start))" >"$logs/$1.status"
}

for s in $suites; do
	if [ "$parallel" = 1 ]; then run "$s" & else run "$s"; fi
done
wait

failed=0
for s in $suites; do
	read -r status seconds <"$logs/$s.status" 2>/dev/null || { status=1; seconds=0; }
	if [ "$status" = 0 ]; then
		echo "ok    $s (${seconds}s)"
	else
		echo "FAIL  $s (exit $status, ${seconds}s): $logs/$s.log"
		grep -E -- '^--- FAIL|^FAIL' "$logs/$s.log" | head -40
		failed=1
	fi
done
echo "checks: logs in $logs"
exit "$failed"
