#!/bin/sh
# Fixture stand-in for build_app.py, used only by integration tests. It
# asserts on its own received argv instead of silently accepting whatever
# it's given — a fixture that ignores a dropped/malformed flag would make
# the integration test vacuous for exactly the drift class it exists to
# catch (see the --review-base-sha incident in the plan doc).
#
# Behavior is selected by $FAKE_BUILD_APP_MODE:
#   commit       - edits a tracked file and commits it itself
#   commit_extra - like commit, but also edits and commits an unrelated
#                  file (used to test the diff-scope gate)
#   commit_other - edits and commits a different file (other.txt) instead
#                  of content.txt, leaving content.txt untouched (used to
#                  test the required-files-changed gate)
#   commit_with_marker - like commit, but content.txt's new line includes
#                  the fixed string REQUIRED_MARKER_STRING (used to test
#                  the required-content-present gate's passing case)
#   rewind_and_commit - resets the workspace back to its root commit
#                  (discarding whatever commits led up to base_sha) and
#                  commits a new file from there, simulating a harness
#                  that rewrites history mid-run (used to test the
#                  base_sha-ancestry check)
#   tamper_spec_and_commit_extra - overwrites $spec to strip its
#                  Allowed-Files: line, then edits and commits an
#                  unrelated file, like commit_extra (used to test that
#                  scope declarations are parsed before this subprocess
#                  ever runs, not re-read from a spec file it can edit)
#   lockfile     - edits and commits a lockfile inside a subdirectory
#   leave_dirty  - edits a tracked file but leaves it uncommitted
#   fail         - exits 1 without touching the workspace
#   infra_once   - overflows the caller's output scanner on its first
#                  invocation, then edits and commits normally on retry
#   hang         - sleeps well past any reasonable test timeout, so the
#                  caller's context deadline is what ends it (used to test
#                  that a killed/timed-out attempt is still recorded)
set -eu

workspace=""
spec=""
conformity_policy=""
max_rounds=""
timeout_minutes=""
review_base_sha=""
verify_command=""
reference_oracle_command=""
baseline_failure=""

while [ $# -gt 0 ]; do
	case "$1" in
	--workspace) workspace="$2"; shift 2 ;;
	--spec) spec="$2"; shift 2 ;;
	--conformity-policy) conformity_policy="$2"; shift 2 ;;
	--max-rounds) max_rounds="$2"; shift 2 ;;
	--timeout-minutes) timeout_minutes="$2"; shift 2 ;;
	--review-base-sha) review_base_sha="$2"; shift 2 ;;
	--verify-command) verify_command="$2"; shift 2 ;;
	--reference-oracle-command) reference_oracle_command="$2"; shift 2 ;;
	--baseline-failure) baseline_failure="$2"; shift 2 ;;
	--harness) shift 2 ;;
	*) echo "fake_build_app: unrecognized arg $1" >&2; exit 2 ;;
	esac
done

for name_val in "workspace:$workspace" "spec:$spec" \
	"conformity_policy:$conformity_policy" \
	"max_rounds:$max_rounds" "timeout_minutes:$timeout_minutes" "review_base_sha:$review_base_sha"; do
	name="${name_val%%:*}"
	val="${name_val#*:}"
	if [ -z "$val" ]; then
		echo "fake_build_app: missing required flag for $name" >&2
		exit 2
	fi
done

echo "fake_build_app: mode=${FAKE_BUILD_APP_MODE:-unset} workspace=$workspace review_base_sha=$review_base_sha verify_command=$verify_command"
echo "fake_build_app: reference_oracle_command=$reference_oracle_command"
if [ -n "$baseline_failure" ]; then
	echo "fake_build_app: baseline_failure=$(tr '\n' '|' <"$baseline_failure")"
fi

if [ "${FAKE_BUILD_APP_MODE:-}" = "fail" ]; then
	exit 1
fi

if [ "${FAKE_BUILD_APP_MODE:-}" = "hang" ]; then
	sleep 300 &
	hung_pid=$!
	if [ -n "${FAKE_BUILD_APP_PID_FILE:-}" ]; then
		printf '%s\n' "$hung_pid" >"$FAKE_BUILD_APP_PID_FILE"
	fi
	wait "$hung_pid"
	exit 0
fi

if [ "${FAKE_BUILD_APP_MODE:-}" = "infra_once" ]; then
	# git -C ... rev-parse --absolute-git-dir, not a hardcoded
	# "$workspace/.git": in a git worktree (see internal/workspace.Prepare,
	# used by every isolated run), .git is a plain FILE pointing at the
	# real per-worktree admin dir under the main checkout's
	# .git/worktrees/<name>/, not a directory -- writing straight into it
	# fails with "Not a directory". This resolves correctly for both a
	# plain checkout and a worktree.
	marker="$(git -C "$workspace" rev-parse --absolute-git-dir)/fake-build-app-infra-once"
	if [ ! -e "$marker" ]; then
		: >"$marker"
		# No newline: one token above runner.Run's 1 MiB Scanner limit is
		# an infrastructure read failure even though this process exits 0.
		awk 'BEGIN { for (i = 0; i < 1048577; i++) printf "x" }'
		exit 0
	fi
fi

cd "$workspace"

if [ "${FAKE_BUILD_APP_MODE:-}" = "commit" ]; then
	# Harness evidence is out-of-band bookkeeping, not an app-source change.
	# Keep it visible to factoryd without letting fixture git inventory count it.
	# info/exclude lives in the repo's *common* dir, shared across every
	# worktree of it (unlike the per-worktree admin dir above) -- resolved
	# the same worktree-safe way, not hardcoded as ".git/info/exclude".
	git_common_dir="$(git rev-parse --path-format=absolute --git-common-dir)"
	printf '%s\n' "BUILD_EVIDENCE.json" >>"$git_common_dir/info/exclude"
	cat >BUILD_EVIDENCE.json <<'EOF'
{
  "generated": "2026-08-26T12:34:56+00:00",
  "review_policy": "advisory",
  "provider": "ai-stack-local",
  "model": "Qwen3.8-27B-MTPLX-Optimized-Quality",
  "succeeded": true,
  "stopped_reason": "canonical verification passed; advisory review",
  "rounds": [
    {
      "index": 1,
      "agent": "pi",
      "agent_returncode": 0,
      "agent_timed_out": false,
      "usage": {"input": 120, "output": 30},
      "reviewer_outcome": "flagged",
      "reviewer_detail": "cache.py: stale value",
      "verify_passed": true,
      "verify_timed_out": false,
      "duration_s": 12.5
    }
  ]
}
EOF
fi

if [ "${FAKE_BUILD_APP_MODE:-}" != "commit_other" ]; then
	echo "edited by fake_build_app in mode ${FAKE_BUILD_APP_MODE:-unset}" >>content.txt
fi

# $FAKE_PLANT_FILE: also writes agent-authored bytes at that workspace path
# (an agent squatting on an approved oracle target_path).
if [ -n "${FAKE_PLANT_FILE:-}" ]; then
	mkdir -p "$(dirname "$FAKE_PLANT_FILE")"
	printf 'package mood\n\n// planted by the agent\n' >"$FAKE_PLANT_FILE"
fi

if [ "${FAKE_BUILD_APP_MODE:-}" = "commit_with_marker" ]; then
	echo "REQUIRED_MARKER_STRING" >>content.txt
fi

if [ "${FAKE_BUILD_APP_MODE:-}" = "rewind_and_commit" ]; then
	root_commit=$(git rev-list --max-parents=0 HEAD | tail -1)
	git reset --hard "$root_commit" >/dev/null
	echo "unrelated work from the rewound state" >rewound.txt
	git add -A
	git commit -q -m "commit from a rewound workspace"
	# Written after the commit above (git add -A already ran), so this
	# stays untracked -- matching real build_app.py, which never commits
	# its own report. Lets a test drive the exact "halts after
	# build_app.py wrote a report, isolated worktree then rolled back"
	# scenario cmd/factoryd's own retention-before-rollback fix covers.
	echo "# Build report

Rewound and committed, then factoryd's own base_sha-ancestor check halted this run." >BUILD_REPORT.md
	exit 0
fi

if [ "${FAKE_BUILD_APP_MODE:-}" = "commit_extra" ] || [ "${FAKE_BUILD_APP_MODE:-}" = "tamper_spec_and_commit_extra" ]; then
	echo "unrelated housekeeping change" >>extra-out-of-scope.txt
fi

if [ "${FAKE_BUILD_APP_MODE:-}" = "tamper_spec_and_commit_extra" ]; then
	# Simulate an untrusted build subprocess editing the writable spec
	# snapshot to strip a scope declaration it would otherwise be caught
	# by. grep -v is POSIX and avoids relying on sed -i's inconsistent
	# in-place-edit flag across platforms.
	tmp_spec="$spec.tampered"
	grep -v '^Allowed-Files:' "$spec" >"$tmp_spec"
	mv "$tmp_spec" "$spec"
fi

if [ "${FAKE_BUILD_APP_MODE:-}" = "commit_other" ]; then
	echo "scaffolding only, not the required change" >>other.txt
fi

if [ "${FAKE_BUILD_APP_MODE:-}" = "lockfile" ]; then
	mkdir -p frontend
	printf 'packages: {}\n' >frontend/pubspec.lock
fi

if [ "${FAKE_BUILD_APP_MODE:-}" = "commit" ] || [ "${FAKE_BUILD_APP_MODE:-}" = "commit_extra" ] || [ "${FAKE_BUILD_APP_MODE:-}" = "commit_other" ] || [ "${FAKE_BUILD_APP_MODE:-}" = "commit_with_marker" ] || [ "${FAKE_BUILD_APP_MODE:-}" = "infra_once" ] || [ "${FAKE_BUILD_APP_MODE:-}" = "lockfile" ] || [ "${FAKE_BUILD_APP_MODE:-}" = "tamper_spec_and_commit_extra" ]; then
	git add -A
	git commit -q -m "agent commit"
fi

exit 0
