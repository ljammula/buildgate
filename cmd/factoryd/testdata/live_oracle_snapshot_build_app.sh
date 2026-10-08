#!/bin/sh
# Build-phase probe for TestLiveDockerBuildPhaseMountsTheOracleSnapshot. It runs
# INSIDE the real sandbox container as the build script, so what it observes is
# what an agent would: the oracle mount at $workspace/.oracle. It reports back
# through files at the root of the run's worktree, which the host reads before it acks.
#   1. signals .live-ready, waits for the host to edit the SOURCE oracle
#      directory and write .live-go;
#   2. copies what .oracle holds now to .live-seen;
#   3. tries to write into .oracle and records the outcome in .live-write;
#   4. waits for the host's .live-ack before exiting;
#   (The workspace's .git is read-only in the container, so the probe commits
#   nothing; the run quarantines as "no changes", which the test ignores.)
set -eu
workspace=""
while [ $# -gt 0 ]; do
	case "$1" in
	--workspace) workspace="$2"; shift 2 ;;
	--spec|--review-policy|--conformity-policy|--max-rounds|--timeout-minutes|--review-base-sha|--verify-command|--reference-oracle-command) shift 2 ;;
	*) shift ;;
	esac
done
cd "$workspace"
: >.live-ready
i=0
while [ ! -e .live-go ]; do
	i=$((i + 1))
	if [ "$i" -gt 600 ]; then echo "live probe: host never signalled" >&2; exit 1; fi
	sleep 0.2
done
cat .oracle/x_oracle_test.go >.live-seen
# .live-write appears with its content already in it: the host reads it as soon
# as it exists.
if (echo tamper >>.oracle/x_oracle_test.go) 2>/dev/null; then echo writable >.live-write.tmp; else echo blocked >.live-write.tmp; fi
mv .live-write.tmp .live-write
# The worktree is removed when the run ends: wait for the host to read the two
# answers above (bounded, so a host that died does not hold the build).
i=0
while [ ! -e .live-ack ] && [ "$i" -lt 150 ]; do i=$((i + 1)); sleep 0.2; done
