#!/usr/bin/env bash
# test-sharded.sh: `make test`'s runner. Runs `go test -race` over every
# package given on the command line, with the slow packages each split
# across parallel processes (shards).
#
# Why: a package is one test binary whose tests run mostly one after another.
# cmd/factoryd takes ~560 s under -race, because its Docker/Temporal
# integration tests reach t.Setenv through shared fixtures
# (testfixture.NewGitRepo, isolateSessionConfig, isolatedTemporalAddress) and
# Go forbids t.Setenv with t.Parallel; internal/sandbox (~250 s, snapshots of
# large git trees) and internal/workflow (~150 s) are the next two. Each shard
# is its own process with its own environment, so those tests overlap across
# shards. The packages are SHARDED_PACKAGES below; one joins the list only if
# its tests share nothing outside the process (a fixed path, a port).
#
# How:
#   1. deal the shard processes to the packages (see "How many shards")
#   2. per sharded package: build its race test binary once (go test -race
#      -c), then the shard check: every name `-test.list '.*'` prints must
#      match exactly one shard's regex
#   3. start one test Temporal dev server for the whole run (see below)
#   4. run every shard from its binary, concurrently with `go test -race`
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
# How many shards: as many processes as this machine carries. TEST_SHARDS=<n>
# sets it; otherwise it is the larger of
#   - the smaller of cores/3 and memory/6 GiB, between 2 and 8, and
#   - how many 6 GiB jobs fit in the memory free right now
#     (scripts/parallel-jobs.sh), at most cores/2.
# A 16 GiB machine gets 2 either way. cmd/factoryd takes the first two; each
# further process goes to the package whose shards are heaviest by the
# timings file. A package left with one process is not sharded: it runs
# inside the `go test` of the other packages, so the processes of a run are
# never more than that `go test` plus one per shard dealt.
#
# Which test goes where is decided on every run, so there is no shard list to
# edit. scripts/test-timings.txt holds, per package, the measured seconds of
# each test that takes a second or more, and `rest`, the sum of the others.
# Every test a binary lists gets a weight (its seconds, or an even share of
# `rest` when it is not in the file, as a new test is) and they are dealt
# heaviest-first to whichever shard has the least so far. A name in the file
# that no longer exists is ignored.
#
# TEST_SHARDS_RECORD=1 runs the shards with -test.v and rewrites the timings
# file from what it measured, for the packages that ran as shards. Do that
# when the per-shard times printed at the end drift well apart.
# TEST_SHARDS_LOGS=<dir> keeps every job's output there.
#
# Usage: scripts/test-sharded.sh <package>...   (the Makefile passes GO_PACKAGES)
set -euo pipefail

MODULE=buildgate
# The packages that may run as shards, as directories of the module. The
# first always does (two shards or more); another does once the machine has a
# process to spare for it, and until then runs with the other packages.
ALWAYS_SHARDED=cmd/factoryd
SHARDED_PACKAGES="$ALWAYS_SHARDED internal/sandbox internal/workflow"
# What one shard process is counted as when asking how many fit in memory.
SHARD_GIB=6
TIMEOUT=20m

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
timings_file="$repo_root/scripts/test-timings.txt"
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

# Split the command line into the packages that may be sharded and the rest.
candidates=()
other_packages=()
for pkg in "$@"; do
	dir="${pkg#"$MODULE"/}"
	case " $SHARDED_PACKAGES " in
		*" $dir "*) candidates+=("$dir") ;;
		*) other_packages+=("$pkg") ;;
	esac
done

cd "$repo_root"

# shard_total prints how many shard processes this run deals out.
shard_total() {
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
	local cores mem_kib n room
	cores="$(getconf _NPROCESSORS_ONLN 2> /dev/null || echo 4)"
	if [ -r /proc/meminfo ]; then
		mem_kib="$(awk '/^MemTotal:/ { print $2 }' /proc/meminfo)"
	else
		mem_kib="$(($(sysctl -n hw.memsize 2> /dev/null || echo 17179869184) / 1024))"
	fi
	n=$((cores / 3))
	[ "$n" -le $((mem_kib / 1024 / 1024 / SHARD_GIB)) ] || n=$((mem_kib / 1024 / 1024 / SHARD_GIB))
	[ "$n" -ge 2 ] || n=2
	[ "$n" -le 8 ] || n=8
	room=$((cores / 2))
	[ "$room" -ge 1 ] || room=1
	room="$("$repo_root/scripts/parallel-jobs.sh" "$SHARD_GIB" "$room" 2> /dev/null || echo 1)"
	[ "$n" -ge "$room" ] || n="$room"
	echo "$n"
}
total="$(shard_total)"

grep -v '^#' "$timings_file" | grep -v '^$' > "$tmp/timings.txt" || true
bad="$(awk 'NF != 3 || $1 !~ /^[0-9]+(\.[0-9]+)?$/' "$tmp/timings.txt")"
if [ -n "$bad" ]; then
	echo "test-sharded: $timings_file has lines that are not \"<seconds> <package> <TestName>\":" >&2
	echo "$bad" >&2
	exit 1
fi
repeated="$(cut -d' ' -f2,3 "$tmp/timings.txt" | sort | uniq -d)"
if [ -n "$repeated" ]; then
	echo "test-sharded: $timings_file names a test more than once:" >&2
	echo "$repeated" >&2
	exit 1
fi

# Deal the processes to the packages: "<package> <shards>". Each candidate
# starts with one (which means: not sharded) and ALWAYS_SHARDED with two; each
# further process goes to the package whose shards are heaviest by the
# timings file, so a package the file does not time is never split.
awk -v total="$total" -v always="$ALWAYS_SHARDED" -v present="${candidates[*]-}" '
	{ weight[$2] += $1 }
	END {
		count = split(present, pkg, " ")
		left = total
		for (i = 1; i <= count; i++) {
			shards[pkg[i]] = 1
			if (pkg[i] == always) { shards[pkg[i]] = 2; left -= 2 }
		}
		for (; left > 0; left--) {
			best = 0
			for (i = 1; i <= count; i++)
				if (!best || weight[pkg[i]] / shards[pkg[i]] > weight[pkg[best]] / shards[pkg[best]]) best = i
			if (!best || weight[pkg[best]] == 0) break
			shards[pkg[best]]++
		}
		for (i = 1; i <= count; i++) print pkg[i], shards[pkg[i]]
	}' "$tmp/timings.txt" > "$tmp/plan.txt"

# plan_shards <package dir> deals the package's tests to its shards and
# checks the deal. It leaves, under $tmp/<name> (the directory's last
# element): <name>.test (the race test binary, named as go test names it:
# cmd/factoryd's tests rely on that name), all.txt (every test it lists) and
# regex.<n> (the -test.run of shard n).
plan_shards() {
	local dir="$1" count="$2" name shard
	name="$(basename "$dir")"
	mkdir "$tmp/$name"
	go test -race -c -o "$tmp/$name/$name.test" "./$dir" < /dev/null
	# Run from the package directory, as go test does: tests read testdata/
	# and cmd/factoryd's TestMain builds the factoryd binary from ".".
	(cd "$repo_root/$dir" && "$tmp/$name/$name.test" -test.list '.*' < /dev/null) | sort > "$tmp/$name/all.txt"

	# Weigh every listed test ("<seconds> <TestName>"), then deal them
	# heaviest-first: "<shard> <TestName>".
	awk -v dir="$dir" '
		NR == FNR { if ($2 != dir) next; if ($3 == "rest") rest = $1; else timed[$3] = $1; next }
		{ names[++count] = $1; if (!($1 in timed)) untimed++ }
		END {
			share = untimed ? rest / untimed : 0
			for (i = 1; i <= count; i++)
				printf "%.4f %s\n", (names[i] in timed) ? timed[names[i]] : share, names[i]
		}' "$tmp/timings.txt" "$tmp/$name/all.txt" | sort -k1,1 -rn -k2,2 | awk -v n="$count" '
		{
			best = 1
			for (i = 2; i <= n; i++) if (load[i] + 0 < load[best] + 0) best = i
			load[best] += $1
			print best, $2
		}' > "$tmp/$name/shards.txt"
	for shard in $(seq 1 "$count"); do
		awk -v s="$shard" '$1 == s { print $2 }' "$tmp/$name/shards.txt" | anchored_regex > "$tmp/$name/regex.$shard"
	done

	# Collect each shard's matches with the same regexes the shards run with;
	# the result must be the test list itself, with nothing missing or doubled.
	for shard in $(seq 1 "$count"); do
		grep -E -f "$tmp/$name/regex.$shard" "$tmp/$name/all.txt" || true
	done | sort > "$tmp/$name/assigned.txt"
	if ! diff -u "$tmp/$name/all.txt" "$tmp/$name/assigned.txt" > "$tmp/$name/shard-check.diff"; then
		echo "test-sharded: a $dir test is in zero shards (-) or more than one (+):" >&2
		cat "$tmp/$name/shard-check.diff" >&2
		exit 1
	fi
	echo "test-sharded: shard check ok: $(wc -l < "$tmp/$name/all.txt" | tr -d ' ') $dir tests, each in exactly one of $count shards"
}

# A package dealt one process is not sharded: it runs with the other packages.
sharded=()
while read -r dir count; do
	if [ "$count" -lt 2 ]; then
		other_packages+=("$MODULE/$dir")
		continue
	fi
	plan_shards "$dir" "$count"
	sharded+=("$dir $count")
done < "$tmp/plan.txt"

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

# run_shard <package dir> <job name> <cmd>... runs run_timed from the
# package's directory.
run_shard() {
	local dir="$1"
	shift
	(cd "$repo_root/$dir" && run_timed "$@")
}

names=()
for entry in ${sharded[@]+"${sharded[@]}"}; do
	dir="${entry% *}"
	name="$(basename "$dir")"
	for shard in $(seq 1 "${entry#* }"); do
		launch run_shard "$dir" "$name-shard-$shard" "$tmp/$name/$name.test" -test.paniconexit0 ${verbose_flag[@]+"${verbose_flag[@]}"} -test.timeout "$TIMEOUT" -test.run "$(cat "$tmp/$name/regex.$shard")"
		names+=("$name-shard-$shard")
	done
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
if [ -n "${TEST_SHARDS_LOGS:-}" ]; then
	mkdir -p "$TEST_SHARDS_LOGS"
	for name in "${names[@]}"; do
		cp "$tmp/$name.log" "$TEST_SHARDS_LOGS/"
	done
fi

# record_timings rewrites the timings file from the verbose shard logs: the
# lines of every package that ran as shards (another package keeps the lines
# it had), counting only the tests the binary lists: a fixture's own go test
# prints result lines too. A test that paused for t.Parallel shared its shard with the others
# that did, so it counts for a quarter of its elapsed time. Tests of a second
# or more are listed; the rest are summed into `rest`.
record_timings() {
	local entry dir name measured=" "
	for entry in ${sharded[@]+"${sharded[@]}"}; do
		measured="$measured${entry% *} "
	done
	{
		sed -n '/^#/p' "$timings_file"
		for entry in ${sharded[@]+"${sharded[@]}"}; do
			dir="${entry% *}"
			name="$(basename "$dir")"
			cat "$tmp/$name"-shard-*.log | awk -v dir="$dir" -v all="$tmp/$name/all.txt" '
				BEGIN { while ((getline line < all) > 0) listed[line] = 1 }
				/^=== PAUSE / { paused[$3] = 1 }
				/^--- (PASS|FAIL|SKIP): / && ($3 in listed) {
					secs = $4
					gsub(/[()s]/, "", secs)
					elapsed[$3] = secs
				}
				END {
					rest = 0
					for (name in elapsed) {
						cost = (name in paused) ? elapsed[name] / 4 : elapsed[name] + 0
						if (cost >= 1) printf "%d %s %s\n", cost + 0.5, dir, name
						else rest += cost
					}
					printf "%d %s rest\n", rest + 0.5, dir
				}'
		done | sort -k2,2 -k1,1rn -k3,3
		awk -v measured="$measured" 'index(measured, " " $2 " ") == 0' "$tmp/timings.txt"
	} > "$tmp/timings.new"
	mv "$tmp/timings.new" "$timings_file"
	echo "test-sharded: rewrote $timings_file from this run"
}
if [ "$record" = 1 ] && [ "$failed" -eq 0 ]; then
	record_timings
fi
exit "$failed"
