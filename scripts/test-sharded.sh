#!/usr/bin/env bash
# test-sharded.sh: `make test`'s runner. Runs `go test -race` over every
# package given on the command line, with cmd/factoryd split across
# parallel processes.
#
# Why: cmd/factoryd takes ~560 s under -race, almost all of it serial,
# because its Docker/Temporal integration tests reach t.Setenv through
# shared fixtures (testfixture.NewGitRepo, isolateSessionConfig,
# isolatedTemporalAddress) and Go forbids t.Setenv with t.Parallel. Each
# shard is its own process with its own environment, so those tests
# overlap across shards.
#
# How:
#   1. build the cmd/factoryd race test binary once (go test -race -c)
#   2. shard check: every name `-test.list '.*'` prints must match exactly
#      one shard's regex, and every name in the shard file must still exist
#   3. start one test Temporal dev server for the whole run (see below)
#   4. run the shards from that binary, concurrently with `go test -race`
#      over every other package
#   5. print each failed shard's (and the other packages') output in full;
#      exit 1 if anything failed
#
# Every end-to-end ticket test runs through that one Temporal dev server,
# exported as FACTORYD_TEST_TEMPORAL_ADDRESS. It is started by
# scripts/temporal-test-server.sh with a fifo as stdin held open by this
# script (fd 9, closed in every job): if this script dies, even by SIGKILL,
# the fifo closes and the server is killed. Without the temporal CLI the
# tests that need it skip. The operator's :7233 is never used.
#
# How many shards: as many as this machine carries. TEST_SHARDS=<n> sets it;
# otherwise it is the smaller of cores/3 and memory/6 GiB, between 2 and 8
# (each shard is a race-detector process; the other packages run beside
# them).
#
# Which test goes where is decided on every run, so there is no shard list to
# edit. scripts/factoryd-test-timings.txt holds the measured seconds of each
# test that takes a second or more, and `rest`, the sum of the others. Every
# test the binary lists gets a weight (its seconds, or an even share of
# `rest` when it is not in the file, as a new test is) and they are dealt
# heaviest-first to whichever shard has the least so far. A name in the file
# that no longer exists is ignored.
#
# TEST_SHARDS_RECORD=1 runs the shards with -test.v and rewrites the timings
# file from what it measured. Do that when the per-shard times printed at the
# end drift well apart.
#
# Usage: scripts/test-sharded.sh <package>...   (the Makefile passes GO_PACKAGES)
set -euo pipefail

FACTORYD_PKG=buildgate/cmd/factoryd
TIMEOUT=20m

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
timings_file="$repo_root/scripts/factoryd-test-timings.txt"
factoryd_dir="$repo_root/cmd/factoryd"
tmp="$(mktemp -d "${TMPDIR:-/tmp}/test-sharded.XXXXXX")"
temporal_pid=""
cleanup() {
	{ exec 9>&-; } 2> /dev/null || true
	if [ -n "$temporal_pid" ]; then
		wait "$temporal_pid" 2> /dev/null || true
	fi
	rm -rf "$tmp"
}
trap cleanup EXIT

# anchored_regex reads names on stdin and prints ^(a|b|c)$.
anchored_regex() {
	printf '^(%s)$' "$(paste -sd '|' -)"
}

other_packages=()
for pkg in "$@"; do
	[ "$pkg" = "$FACTORYD_PKG" ] || other_packages+=("$pkg")
done

cd "$repo_root"
go test -race -c -o "$tmp/factoryd.test" ./cmd/factoryd

# Run from the package directory, as go test does: tests read testdata/ and
# TestMain builds the factoryd binary from ".".
(cd "$factoryd_dir" && "$tmp/factoryd.test" -test.list '.*') | sort > "$tmp/all.txt"

# shard_count prints how many shards this machine should run.
shard_count() {
	if [ -n "${TEST_SHARDS:-}" ]; then
		case "$TEST_SHARDS" in
			'' | *[!0-9]* | 0 | 1)
				echo "test-sharded: TEST_SHARDS must be a number of at least 2, got \"$TEST_SHARDS\"" >&2
				exit 1
				;;
		esac
		echo "$TEST_SHARDS"
		return
	fi
	local cores mem_kib n
	cores="$(getconf _NPROCESSORS_ONLN 2> /dev/null || echo 4)"
	if [ -r /proc/meminfo ]; then
		mem_kib="$(awk '/^MemTotal:/ { print $2 }' /proc/meminfo)"
	else
		mem_kib="$(($(sysctl -n hw.memsize 2> /dev/null || echo 17179869184) / 1024))"
	fi
	n=$((cores / 3))
	[ "$n" -le $((mem_kib / 1024 / 1024 / 6)) ] || n=$((mem_kib / 1024 / 1024 / 6))
	[ "$n" -ge 2 ] || n=2
	[ "$n" -le 8 ] || n=8
	echo "$n"
}
shards="$(shard_count)"

grep -v '^#' "$timings_file" | grep -v '^$' > "$tmp/timings.txt" || true
bad="$(awk 'NF != 2 || $1 !~ /^[0-9]+(\.[0-9]+)?$/' "$tmp/timings.txt")"
if [ -n "$bad" ]; then
	echo "test-sharded: $timings_file has lines that are not \"<seconds> <TestName>\":" >&2
	echo "$bad" >&2
	exit 1
fi
repeated="$(cut -d' ' -f2 "$tmp/timings.txt" | sort | uniq -d)"
if [ -n "$repeated" ]; then
	echo "test-sharded: $timings_file names a test more than once:" >&2
	echo "$repeated" >&2
	exit 1
fi

# Weigh every listed test ("<seconds> <TestName>"), then deal them
# heaviest-first: "<shard> <TestName>".
awk '
	NR == FNR { if ($2 == "rest") rest = $1; else timed[$2] = $1; next }
	{ names[++count] = $1; if (!($1 in timed)) untimed++ }
	END {
		share = untimed ? rest / untimed : 0
		for (i = 1; i <= count; i++)
			printf "%.4f %s\n", (names[i] in timed) ? timed[names[i]] : share, names[i]
	}' "$tmp/timings.txt" "$tmp/all.txt" | sort -k1,1 -rn -k2,2 | awk -v n="$shards" '
	{
		best = 1
		for (i = 2; i <= n; i++) if (load[i] + 0 < load[best] + 0) best = i
		load[best] += $1
		print best, $2
	}' > "$tmp/shards.txt"
run_regex=()
for shard in $(seq 1 "$shards"); do
	run_regex[shard]="$(awk -v s="$shard" '$1 == s { print $2 }' "$tmp/shards.txt" | anchored_regex)"
done

# Collect each shard's matches with the same regexes the shards run with;
# the result must be the test list itself, with nothing missing or doubled.
for shard in $(seq 1 "$shards"); do
	grep -E "${run_regex[shard]}" "$tmp/all.txt" || true
done | sort > "$tmp/assigned.txt"
if ! diff -u "$tmp/all.txt" "$tmp/assigned.txt" > "$tmp/shard-check.diff"; then
	echo "test-sharded: a cmd/factoryd test is in zero shards (-) or more than one (+):" >&2
	cat "$tmp/shard-check.diff" >&2
	exit 1
fi
echo "test-sharded: shard check ok: $(wc -l < "$tmp/all.txt" | tr -d ' ') cmd/factoryd tests, each in exactly one of $shards shards"

# TEST_SHARDS_RECORD=1: verbose output, so each test's time can be read back.
record="${TEST_SHARDS_RECORD:-0}"
verbose_flag=()
if [ "$record" = 1 ]; then
	verbose_flag=(-test.v)
fi

# start_test_temporal launches the shared test Temporal server and exports
# its address; it returns quietly (tests skip) when the temporal CLI is
# missing and fails the run when the server never becomes healthy.
start_test_temporal() {
	command -v temporal > /dev/null || {
		echo "test-sharded: no temporal CLI; Temporal-backed tests will skip"
		return 0
	}
	local port address start
	port="$(python3 -c 'import socket; s = socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1])')"
	address="127.0.0.1:$port"
	mkfifo "$tmp/temporal.lifeline"
	sh "$repo_root/scripts/temporal-test-server.sh" factoryd-test-temporal "$port" "$tmp/temporal.log" < "$tmp/temporal.lifeline" &
	temporal_pid=$!
	exec 9> "$tmp/temporal.lifeline"
	start="$(date +%s)"
	until temporal operator cluster health --address "$address" > /dev/null 2>&1; do
		if [ $(($(date +%s) - start)) -gt 30 ]; then
			echo "test-sharded: test Temporal server at $address never became healthy:" >&2
			cat "$tmp/temporal.log" >&2
			exit 1
		fi
		sleep 0.2
	done
	export FACTORYD_TEST_TEMPORAL_ADDRESS="$address"
	echo "test-sharded: test Temporal server at $address"
}
start_test_temporal

# run_timed <name> <cmd>...: runs cmd with its output in $tmp/<name>.log and
# records "<exit status> <wall seconds>" in $tmp/<name>.result.
run_timed() {
	local name="$1" start rc
	shift
	start="$(date +%s)"
	set +e
	"$@" 9>&- > "$tmp/$name.log" 2>&1
	rc=$?
	set -e
	echo "$rc $(($(date +%s) - start))" > "$tmp/$name.result"
}

# TEST_SHARDS_SEQUENTIAL=1 runs the shards and then the other packages one
# after another (the other packages with go test -p 2) instead of all at once:
# slower, but peak memory is about one race binary's instead of one per shard, for a
# machine whose memory is mostly held by the Docker VM.
sequential="${TEST_SHARDS_SEQUENTIAL:-0}"
other_flags=()
if [ "$sequential" = 1 ]; then
	other_flags=(-p 2)
fi

# launch runs one job: in the background (pid recorded in job_pids), or in
# the foreground with TEST_SHARDS_SEQUENTIAL=1.
job_pids=()
launch() {
	if [ "$sequential" = 1 ]; then
		"$@"
	else
		"$@" &
		job_pids+=("$!")
	fi
}

# run_factoryd_shard runs run_timed from cmd/factoryd's directory.
run_factoryd_shard() {
	(cd "$factoryd_dir" && run_timed "$@")
}

names=()
for shard in $(seq 1 "$shards"); do
	launch run_factoryd_shard "factoryd-shard-$shard" "$tmp/factoryd.test" -test.paniconexit0 ${verbose_flag[@]+"${verbose_flag[@]}"} -test.timeout "$TIMEOUT" -test.run "${run_regex[shard]}"
	names+=("factoryd-shard-$shard")
done
if [ "${#other_packages[@]}" -gt 0 ]; then
	launch run_timed other-packages go test -race ${other_flags[@]+"${other_flags[@]}"} -timeout "$TIMEOUT" "${other_packages[@]}"
	names+=(other-packages)
fi
# Only the test jobs: a bare `wait` would also wait for the test Temporal
# server, which lives until this script exits.
if [ "${#job_pids[@]}" -gt 0 ]; then
	wait "${job_pids[@]}"
fi

failed=0
for name in "${names[@]}"; do
	read -r rc seconds < "$tmp/$name.result"
	if [ "$rc" -eq 0 ]; then
		echo "test-sharded: $name ok (${seconds}s)"
	else
		failed=1
		echo "test-sharded: $name FAILED (exit $rc, ${seconds}s); full output:"
		cat "$tmp/$name.log"
		echo "test-sharded: end of $name output"
	fi
done
# record_timings rewrites the timings file from the verbose shard logs. A
# test that paused for t.Parallel shared its shard with the others that did,
# so it counts for a quarter of its elapsed time. Tests of a second or more
# are listed; the rest are summed into `rest`.
record_timings() {
	local logs=()
	for name in "${names[@]}"; do
		case "$name" in factoryd-shard-*) logs+=("$tmp/$name.log") ;; esac
	done
	{
		sed -n '/^#/p' "$timings_file"
		cat "${logs[@]}" | awk '
			/^=== PAUSE / { paused[$3] = 1 }
			/^--- (PASS|FAIL|SKIP): / {
				secs = $4
				gsub(/[()s]/, "", secs)
				elapsed[$3] = secs
			}
			END {
				rest = 0
				for (name in elapsed) {
					cost = (name in paused) ? elapsed[name] / 4 : elapsed[name] + 0
					if (cost >= 1) printf "%d %s\n", cost + 0.5, name
					else rest += cost
				}
				printf "%d rest\n", rest + 0.5
			}' | sort -k1,1 -rn -k2,2
	} > "$tmp/timings.new"
	mv "$tmp/timings.new" "$timings_file"
	echo "test-sharded: rewrote $timings_file from this run"
}
if [ "$record" = 1 ] && [ "$failed" -eq 0 ]; then
	record_timings
fi
exit "$failed"
