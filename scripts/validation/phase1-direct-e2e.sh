#!/usr/bin/env bash
# Phase 1, steps 1-3 of doc/designs/validation-plan.md: scaffold a throwaway
# repo through both intake checkpoints, drive ticket 001 to `accepted` (on
# Temporal at the default address, started with Docker when down), then audit
# the evidence it produced.
#
# Requires: factoryd built (`make install` or `go run ./cmd/factoryd`),
# Docker/colima up, the local ai-stack model reachable, `gh auth login` done
# (accepted runs open a draft PR against the throwaway repo's own remote).
set -euo pipefail
cd "$(dirname "$0")/../.."

# Under ~/buildgate, not mktemp's default $TMPDIR (/var/folders/... on
# macOS): colima shares only ~/buildgate read-write into its VM (USAGE.md),
# so a pilot dir anywhere else is invisible to every container factoryd
# launches ("does not appear to share ... into its containers").
mkdir -p "$HOME/buildgate"
PILOT_ROOT="$(mktemp -d "$HOME/buildgate/validation-XXXXXX")"
PILOT_DIR="$PILOT_ROOT/validation-target"
mkdir -p "$PILOT_DIR"
echo "== throwaway pilot dir: $PILOT_DIR =="

INTAKE_ARGS=(
  intake
  -spec-input "a single-endpoint HTTP service: GET /health returns {\"status\":\"ok\"}"
  -pilot-dir "$PILOT_DIR"
)

echo "== intake (1st pass: drafts spec, halts at spec-freeze) =="
go run ./cmd/factoryd "${INTAKE_ARGS[@]}"

SPEC="$PILOT_DIR/spec/spec.md"
echo "== freezing $SPEC by hand =="
# Verify line 1 is actually the DRAFT placeholder before stomping it --
# found via review: a blind `1s/.*/.../ ` always "succeeds" regardless of
# what was there, making the grep below tautological (it just checks for
# the literal string this same command just wrote). Only the STATUS line's
# suffix varies per topic (real content, not a fixed placeholder); the
# leading "STATUS: DRAFT" is what every goal_pilot.py draft actually emits.
head -1 "$SPEC" | grep -q '^STATUS: DRAFT' || {
  echo "spec.md line 1 is not the expected STATUS: DRAFT placeholder (goal_pilot.py's output format may have changed) -- refusing to overwrite it blind:" >&2
  head -1 "$SPEC" >&2
  exit 1
}
sed -i.bak '1s/.*/STATUS: FROZEN/' "$SPEC"  # exact line-1 match required, see goal_pilot.py's own runtime message
rm -f "$SPEC.bak"
grep -q '^STATUS: FROZEN$' "$SPEC" || {
  echo "spec-freeze substitution didn't match; freeze $SPEC by hand and re-run" >&2
  exit 1
}

echo "== intake (2nd pass: drafts contract, halts at acceptance-suite review) =="
go run ./cmd/factoryd "${INTAKE_ARGS[@]}"

# goal_pilot.py's own /contract-plan step does not commit spec/contract.md,
# spec/acceptance/, or ARCHITECTURE.md itself (found live 2026-09-14: only
# the initial scaffold commit existed; everything intake drafted was still
# untracked) -- the run's worktree is cut from the pilot repo's
# current HEAD, so a real run would build against a worktree missing all of
# it. An operator following USAGE.md's own "review, then continue" step
# commits before proceeding; this script does the same.
( cd "$PILOT_DIR" && git add -A -- . ':!workspace' && git commit -q -m "intake: spec, contract, acceptance suite, tickets" )

TICKET="$(ls "$PILOT_DIR"/spec/tickets/001-*.md)"

echo "== check-project =="
echo "   (a drafted contract may fail program_design_structure -- e.g. missing"
echo "   a Conventions/Error-envelope section before its first Endpoint,"
echo "   found live 2026-09-14 -- fix spec/contract.md by hand and re-run"
echo "   from here if so; there is no override, per USAGE.md sec 4)"
go run ./cmd/factoryd check-project \
  -project validation-target \
  -spec "$PILOT_DIR/spec/spec.md" \
  -contract "$PILOT_DIR/spec/contract.md" \
  -architecture "$PILOT_DIR/ARCHITECTURE.md" \
  -ticket "$TICKET" \
  -ticket-number 1 \
  -data-dir "$PILOT_DIR/data"

echo "== running ticket 001 =="
go run ./cmd/factoryd \
  -workspace "$PILOT_DIR/workspace" \
  -spec "$TICKET" \
  -ticket 001 \
  -ticket-file "$TICKET" \
  -verify-command "true" \
  -data-dir "$PILOT_DIR/data"

RUN_ID="$(ls -t "$PILOT_DIR"/data/runs | head -1)"
RUN_JSON="$PILOT_DIR/data/runs/$RUN_ID/run.json"
echo "== run $RUN_ID =="
go run ./cmd/factoryd status -data-dir "$PILOT_DIR/data" -n 1

echo "== evidence audit =="
python3 - "$RUN_JSON" <<'PY'
import json, sys
run = json.load(open(sys.argv[1]))
state = run.get("state")
print(f"state: {state}")
assert state in ("accepted", "quarantined"), f"unexpected terminal state {state!r}"
if state == "accepted":
    assert run.get("changed_files") is not None, "changed_files must never be null"
    digests = [a.get("image_digest") for a in run.get("attempts", []) if a.get("image_digest")]
    assert digests, "no attempt recorded a sandbox image digest"
    print("accepted-run evidence looks structurally sound; see the plan's Phase 1.3 for the full field-by-field audit")
else:
    print(f"quarantined: {run.get('reason')} -- inspect by hand against Phase 1.4's two classes")
PY

echo "== Phase 1 (steps 1-3) done. Pilot dir left at $PILOT_DIR for inspection; delete it when done. =="
echo "== Continue with the plan's steps 4-7 (quarantine, cancellation, ceiling, request pipeline) by hand. =="
