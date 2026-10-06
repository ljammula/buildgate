#!/bin/sh
# Fixture stand-in for pi-harness-hardening's goal_pilot.py, used only by
# integration tests -- asserts on its own received argv instead of
# silently accepting whatever it's given, same reasoning as
# fake_build_app.sh's own doc comment.
#
# Mimics goal_pilot.py's real --non-interactive boundary at two possible
# halts, selected by whether $pilot_dir/spec/spec.md already contains
# "STATUS: FROZEN" when this fixture starts (the same signal the real
# script's own main() checks):
#
#   - Not yet frozen: step 2 (draft spec.md + spec/tickets/*.md +
#     ARCHITECTURE.md) succeeds and persists to disk, then step 3 (the
#     spec-freeze checkpoint) refuses non-interactively and this exits
#     non-zero -- intakeMain must treat that as this function's expected
#     outcome, not a failure, and judge success by the ticket files
#     actually on disk instead.
#   - Already frozen (a resumed invocation, mimicking a human's own prior
#     edit to spec.md's STATUS line): step 4 (draft spec/contract.md +
#     spec/acceptance/) succeeds and persists to disk, then step 5 (the
#     acceptance-suite checkpoint) refuses non-interactively and this
#     exits non-zero -- intakeMain must treat that the same way, judging
#     success by spec/contract.md actually on disk.
#
# Behavior selected by $FAKE_GOAL_PILOT_MODE:
#   draft (default) - fresh path: writes spec.md, one ticket,
#                      ARCHITECTURE.md, prints the real script's own exact
#                      spec-freeze-checkpoint refusal phrase, then exits 1
#                      (its real exit status). Resumed path: writes
#                      spec/contract.md, prints the real script's own
#                      exact acceptance-suite-checkpoint refusal phrase,
#                      then exits 1.
#   no_tickets       - fresh path only: writes spec.md and the tickets dir
#                       but no ticket file inside it, reaches the same
#                       checkpoint refusal phrase, then exits 1 (used to
#                       test intakeMain's own "drafted no new ticket
#                       files" error path)
#   partial_crash    - fresh path only: writes spec.md and one ticket,
#                       like draft, but exits 1 *without* ever printing
#                       the checkpoint refusal phrase -- simulates a real
#                       crash midway through step 2 (e.g. before
#                       write_architecture_stub), which leaves freshly
#                       changed files on disk despite never actually
#                       reaching the checkpoint. Used to test intakeMain's
#                       own checkpoint-signal check, not just its
#                       file-freshness checks.
#   no_contract      - resumed path only: exits 1 without writing
#                       spec/contract.md and without the acceptance-suite
#                       refusal phrase -- simulates step 4 crashing before
#                       it could write the contract. Used to test
#                       intakeMain's own "did not reach either checkpoint"
#                       error path on a resumed invocation.
#   fail             - either path: exits 1 before writing anything (used
#                       to test intakeMain's own "no spec/tickets
#                       directory" / initial-invocation error path)
set -eu

spec_input=""
pilot_dir=""
non_interactive=""
checkpoint=""
verify_command=""
while [ $# -gt 0 ]; do
	case "$1" in
	--spec-input) spec_input="$2"; shift 2 ;;
	--pilot-dir) pilot_dir="$2"; shift 2 ;;
	--non-interactive) non_interactive="1"; shift 1 ;;
	--checkpoint) checkpoint="$2"; shift 2 ;;
	# Optional, like the real script's own --verify-command: recorded in
	# this fixture's own echoed startup line (below) so a test can assert
	# it was actually threaded through, but not otherwise used by this
	# fixture's fake drafting logic.
	--verify-command) verify_command="$2"; shift 2 ;;
	*) echo "fake_goal_pilot: unrecognized arg $1" >&2; exit 2 ;;
	esac
done

for name_val in "spec_input:$spec_input" "pilot_dir:$pilot_dir" "non_interactive:$non_interactive" "checkpoint:$checkpoint"; do
	name="${name_val%%:*}"
	val="${name_val#*:}"
	if [ -z "$val" ]; then
		echo "fake_goal_pilot: missing required flag for $name" >&2
		exit 2
	fi
done

echo "fake_goal_pilot: spec_input=$spec_input pilot_dir=$pilot_dir non_interactive=$non_interactive checkpoint=$checkpoint verify_command=$verify_command"

if [ "${FAKE_GOAL_PILOT_MODE:-draft}" = "fail" ]; then
	echo "fake_goal_pilot: step 2 FAILED (fixture)" >&2
	exit 1
fi

# --- resumed path: spec.md already FROZEN from a prior invocation ---
# Mirrors the real script's own read_spec_status() EXACTLY (goal_pilot.py:
# SPEC_STATUS_RE = re.compile(r"^STATUS:\s*(\S+)"), matched against line 1
# only) -- not a substring grep anywhere in the file. Found via a real
# Opus review pass, 2026-09-04: a fixture that's more permissive than the
# real script here made TestIntegrationIntakeResumesPastFreezeToDraftContract
# structurally unable to catch the one bug this resume path can actually
# cause -- against the real script, a human's own review edit that adds
# so much as a title line above STATUS, or a case/spacing mismatch, means
# read_spec_status() returns None (not "FROZEN"), so a real re-invocation
# falls into goal_pilot.py's own `if status != "FROZEN":` branch and
# silently re-runs /spec-plan, overwriting the human's reviewed spec --
# the exact failure this fixture is meant to let the test suite catch.
spec_status=""
if [ -f "$pilot_dir/spec/spec.md" ]; then
	spec_status=$(head -n 1 "$pilot_dir/spec/spec.md" | sed -n 's/^STATUS:[[:space:]]*\([^[:space:]]*\).*/\1/p')
fi
if [ "$spec_status" = "FROZEN" ]; then
	if [ "${FAKE_GOAL_PILOT_MODE:-draft}" = "no_contract" ]; then
		echo "fake_goal_pilot: step 4 FAILED (fixture) -- never reached step 5" >&2
		exit 1
	fi
	mkdir -p "$pilot_dir/spec/acceptance"
	# --checkpoint skip on a resume: mirrors the real script's step 5
	# checkpoint_mode == "skip" branch, which has no non-interactive halt
	# at all -- it proceeds straight through step 6 (ticket_runner.py
	# actually building ticket 001) to the always-on step 5b checkpoint
	# instead. intakeMain forces --checkpoint review specifically to
	# avoid ever reaching this; simulated here (a "build" is just a
	# marker file, not a real ticket_runner.py invocation) only so
	# intakeMain's own handling of an explicit -checkpoint skip is
	# actually exercised by a test rather than left untested (found via a
	# real Opus review pass, 2026-09-04, A5).
	if [ "$checkpoint" = "skip" ]; then
		cat >"$pilot_dir/spec/contract.md" <<EOF
# Fixture Contract

Nonce: $$-$(date +%s%N 2>/dev/null || date +%s)
EOF
		: >"$pilot_dir/.fixture-ticket-001-built"
		echo "--non-interactive: refusing to auto-confirm the post-ticket-001 checkpoint. Halting." >&2
		exit 1
	fi
	# A nonce, not static content -- same reasoning as spec.md's nonce
	# below: a real successful re-compile's output genuinely differs run
	# to run, so static content would be indistinguishable from a stale,
	# untouched file.
	cat >"$pilot_dir/spec/contract.md" <<EOF
# Fixture Contract

Nonce: $$-$(date +%s%N 2>/dev/null || date +%s)
EOF
	# The real script's own exact phrase (step5_checkpoint, non-interactive
	# branch, checkpoint_mode == "review") -- intakeMain checks for this
	# specific text, not just a nonzero exit status, so the fixture must
	# match it verbatim.
	echo "--non-interactive: refusing to auto-confirm the acceptance-suite checkpoint. Halting." >&2
	exit 1
fi

# --- fresh path: spec.md not yet frozen (or doesn't exist yet) ---
mkdir -p "$pilot_dir/spec/tickets" "$pilot_dir/spec/acceptance" "$pilot_dir/workspace"
# A nonce, not static content: the real goal_pilot.py drafts spec.md fresh
# via a real model call on every successful invocation, so its content
# genuinely differs run to run (even against the same --spec-input) --
# static fixture content would make every re-run's spec.md byte-identical
# to the last, which is not what a real successful re-run looks like and
# would make intakeMain's own content-based freshness check
# indistinguishable from a truly stale, untouched file.
cat >"$pilot_dir/spec/spec.md" <<EOF
STATUS: DRAFT
# Fixture Spec

Nonce: $$-$(date +%s%N 2>/dev/null || date +%s)
EOF
cat >"$pilot_dir/ARCHITECTURE.md" <<'EOF'
# Fixture Architecture

STATUS: DRAFT
EOF

if [ "${FAKE_GOAL_PILOT_MODE:-draft}" != "no_tickets" ]; then
	# A nonce here too, same reasoning as spec.md's own -- a genuine
	# re-draft (e.g. after a misplaced-freeze re-run of the fresh path)
	# produces different ticket content every time via a real model call;
	# static fixture content would make a second real re-draft
	# indistinguishable from a truly stale, untouched file, tripping
	# intakeReportSpecDraft's ticket-freshness check for the wrong reason.
	cat >"$pilot_dir/spec/tickets/001-fixture-ticket.md" <<EOF
# Ticket: fixture

## Goal

Fixture ticket drafted by fake_goal_pilot.sh. Nonce: $$-$(date +%s%N 2>/dev/null || date +%s)

## Required changes

- content.txt

## Verification

Verify-Command: true
Allowed-Files: content.txt
Required-Changed-Files: content.txt
EOF
fi

if [ "${FAKE_GOAL_PILOT_MODE:-draft}" = "partial_crash" ]; then
	echo "fake_goal_pilot: step 2 crashed before completing (fixture) -- never reached step 3" >&2
	exit 1
fi

# The real script's own exact phrase (step3_freeze_checkpoint,
# non-interactive branch) -- intakeMain checks for this specific text, not
# just a nonzero exit status, so the fixture must match it verbatim.
echo "--non-interactive: refusing to auto-approve the spec freeze. Halting." >&2
exit 1
