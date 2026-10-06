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
# Shards 1..N-1 are the name lists in scripts/factoryd-test-shards.txt;
# shard N is everything else (-test.skip of every listed name).
#
# Usage: scripts/test-sharded.sh <package>...   (the Makefile passes GO_PACKAGES)
set -euo pipefail

FACTORYD_PKG=buildgate/cmd/factoryd
TIMEOUT=20m

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
shard_file="$repo_root/scripts/factoryd-test-shards.txt"
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

grep -v '^#' "$shard_file" | grep -v '^$' > "$tmp/shards.txt"
explicit_shards="$(cut -d' ' -f1 "$tmp/shards.txt" | sort -un)"
last_shard=$(($(printf '%s\n' "$explicit_shards" | tail -1) + 1))
run_regex=()
for shard in $explicit_shards; do
	run_regex[shard]="$(awk -v s="$shard" '$1 == s { print $2 }' "$tmp/shards.txt" | anchored_regex)"
done
skip_regex="$(cut -d' ' -f2 "$tmp/shards.txt" | anchored_regex)"

repeated="$(cut -d' ' -f2 "$tmp/shards.txt" | sort | uniq -d)"
if [ -n "$repeated" ]; then
	echo "test-sharded: $shard_file names a test more than once:" >&2
	echo "$repeated" >&2
	exit 1
fi
stale="$(cut -d' ' -f2 "$tmp/shards.txt" | sort | comm -23 - "$tmp/all.txt")"
if [ -n "$stale" ]; then
	echo "test-sharded: $shard_file names tests that do not exist:" >&2
	echo "$stale" >&2
	exit 1
fi
# Collect each shard's matches with the same regexes the shards run with;
# the result must be the test list itself, with nothing missing or doubled.
{
	for shard in $explicit_shards; do
		grep -E "${run_regex[shard]}" "$tmp/all.txt" || true
	done
	grep -Ev "$skip_regex" "$tmp/all.txt" || true
} | sort > "$tmp/assigned.txt"
if ! diff -u "$tmp/all.txt" "$tmp/assigned.txt" > "$tmp/shard-check.diff"; then
	echo "test-sharded: a cmd/factoryd test is in zero shards (-) or more than one (+):" >&2
	cat "$tmp/shard-check.diff" >&2
	exit 1
fi
echo "test-sharded: shard check ok: $(wc -l < "$tmp/all.txt" | tr -d ' ') cmd/factoryd tests, each in exactly one of $last_shard shards"

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
# slower, but peak memory is about one race binary's instead of five, for a
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
for shard in $explicit_shards; do
	launch run_factoryd_shard "factoryd-shard-$shard" "$tmp/factoryd.test" -test.paniconexit0 -test.timeout "$TIMEOUT" -test.run "${run_regex[shard]}"
	names+=("factoryd-shard-$shard")
done
launch run_factoryd_shard "factoryd-shard-$last_shard" "$tmp/factoryd.test" -test.paniconexit0 -test.timeout "$TIMEOUT" -test.skip "$skip_regex"
names+=("factoryd-shard-$last_shard")
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
exit "$failed"
