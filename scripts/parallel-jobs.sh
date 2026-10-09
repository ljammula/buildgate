#!/bin/sh
# parallel-jobs.sh <GiB per job> <max>: how many memory-heavy jobs this machine
# can run at once right now, between 1 and <max>.
#
# It is the memory that is free now, or the machine's total less 8 GiB for
# everything else if that is smaller, divided by <GiB per job>. A 16 GiB Mac
# therefore gets 1 for any job of 6 GiB or more, which is the default every
# caller is written for; a larger machine gets more without being told.
# `parallel-jobs.sh --free` prints the free GiB it would use.
#
# Callers let an environment variable of their own override the answer
# (LIVE_SMOKE_JOBS, CHECKS_PARALLEL): that is how one machine is tuned.
set -eu

total_kib() {
	if [ -r /proc/meminfo ]; then
		awk '/^MemTotal:/ { print $2 }' /proc/meminfo
	else
		echo "$(($(sysctl -n hw.memsize 2>/dev/null || echo 17179869184) / 1024))"
	fi
}

# free_kib: MemAvailable on Linux; on macOS the share memory_pressure reports
# free, of the total. Unknown reads as a quarter of the total.
free_kib() {
	total="$1"
	if [ -r /proc/meminfo ]; then
		awk '/^MemAvailable:/ { print $2; found = 1 } END { if (!found) exit 1 }' /proc/meminfo && return
	fi
	pct="$(memory_pressure 2>/dev/null | awk '/free percentage/ { gsub("%", "", $NF); print $NF }' | tail -1)"
	case "$pct" in '' | *[!0-9]*) pct=25 ;; esac
	echo "$((total * pct / 100))"
}

total="$(total_kib)"
free="$(free_kib "$total")"
usable=$((total - 8 * 1024 * 1024))
[ "$free" -le "$usable" ] && usable="$free"
[ "$usable" -ge 0 ] || usable=0
usable_gib=$((usable / 1024 / 1024))

if [ "${1:-}" = "--free" ]; then
	echo "$usable_gib"
	exit 0
fi
per_job="${1:?usage: parallel-jobs.sh <GiB per job> <max> | --free}"
max="${2:?usage: parallel-jobs.sh <GiB per job> <max> | --free}"
case "$per_job$max" in *[!0-9]* | '') echo "parallel-jobs: both arguments must be positive integers" >&2; exit 2 ;; esac
[ "$per_job" -gt 0 ] && [ "$max" -gt 0 ] || { echo "parallel-jobs: both arguments must be positive integers" >&2; exit 2; }
n=$((usable_gib / per_job))
[ "$n" -ge 1 ] || n=1
[ "$n" -le "$max" ] || n="$max"
echo "$n"
