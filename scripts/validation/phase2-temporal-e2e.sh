#!/usr/bin/env bash
# Phase 2, steps 0-1 of doc/designs/validation-plan.md: same throwaway
# scaffold as phase1-direct-e2e.sh, but ticket 001 runs through
# internal/workflow.RunWorkflow on a real local Temporal server instead of
# directly.
#
# Requires: docker-compose.temporal.yml's stack up
# (`docker compose -f docker-compose.temporal.yml up -d`), Docker/colima up,
# the local ai-stack model reachable.
set -euo pipefail
cd "$(dirname "$0")/../.."

TEMPORAL_ADDRESS="${TEMPORAL_ADDRESS:-localhost:7233}"

echo "== precondition: the real Temporal server this script will actually use must be reachable =="
# This is a reachability sanity check on the *configured* TEMPORAL_ADDRESS
# only -- it does not exercise the fail-closed path (see below). An earlier
# version of this script conflated the two: it invoked `doctor` against this
# same expected-to-be-running address and treated that as covering Phase 2
# step 0's fail-closed requirement, but a doctor call that succeeds against
# a reachable server can never observe what happens when Temporal is
# unreachable (found via Codex review, PRs #143-#148 follow-up).
mkdir -p "$HOME/buildgate"
DOCTOR_WORKSPACE="$(mktemp -d "$HOME/buildgate/validation-XXXXXX")"
if go run ./cmd/factoryd doctor -temporal-address "$TEMPORAL_ADDRESS" -workspace "$DOCTOR_WORKSPACE" \
     -sandbox-image "" -registry-proxy-image "" -compose-services=false >/dev/null; then
  echo "Temporal reachable at $TEMPORAL_ADDRESS, good."
else
  echo "Temporal not reachable at $TEMPORAL_ADDRESS -- start docker-compose.temporal.yml first" >&2
  rm -rf "$DOCTOR_WORKSPACE"
  exit 1
fi
rm -rf "$DOCTOR_WORKSPACE"

# Under ~/buildgate, not mktemp's default $TMPDIR: see phase1-direct-e2e.sh's
# own comment on this same line -- colima shares only ~/buildgate read-write.
PILOT_ROOT="$(mktemp -d "$HOME/buildgate/validation-XXXXXX")"
PILOT_DIR="$PILOT_ROOT/validation-target"
mkdir -p "$PILOT_DIR"
echo "== throwaway pilot dir: $PILOT_DIR =="

INTAKE_ARGS=(
  intake
  -spec-input "a single-endpoint HTTP service: GET /health returns {\"status\":\"ok\"}"
  -pilot-dir "$PILOT_DIR"
)

echo "== intake (1st pass) =="
go run ./cmd/factoryd "${INTAKE_ARGS[@]}"

SPEC="$PILOT_DIR/spec/spec.md"
# See phase1-direct-e2e.sh's own comment on this same line (found via
# review): verify line 1 is really the DRAFT placeholder before stomping
# it, so a changed goal_pilot.py output format fails loudly here instead
# of silently overwriting unrelated content with no check at all.
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

echo "== intake (2nd pass) =="
go run ./cmd/factoryd "${INTAKE_ARGS[@]}"

# See phase1-direct-e2e.sh's own comment on this same line: goal_pilot.py's
# /contract-plan does not commit its own output.
( cd "$PILOT_DIR" && git add -A -- . ':!workspace' && git commit -q -m "intake: spec, contract, acceptance suite, tickets" )

TICKET="$(ls "$PILOT_DIR"/spec/tickets/001-*.md)"

echo "== check-project =="
echo "   (a drafted contract may fail program_design_structure -- e.g. missing"
echo "   a Conventions/Error-envelope section before its first Endpoint -- see"
echo "   the plan's own Phase 0-adjacent finding; fix spec/contract.md by hand"
echo "   and re-run this script from here if so, there is no override)"
go run ./cmd/factoryd check-project \
  -project validation-target \
  -spec "$PILOT_DIR/spec/spec.md" \
  -contract "$PILOT_DIR/spec/contract.md" \
  -architecture "$PILOT_DIR/ARCHITECTURE.md" \
  -ticket "$TICKET" \
  -ticket-number 1 \
  -data-dir "$PILOT_DIR/data"

echo "== fail-closed check: run against a guaranteed-unreachable Temporal address =="
# The actual Phase 2 step 0 requirement: a run must halt/HaltConfirmed when
# it cannot reach Temporal at all, never run the ticket anywhere else. 127.0.0.1:1 is a real address nothing
# ever listens on (port 0 is a reserved wildcard, not a live refusal), not
# this machine's own real Temporal server, which per this repo's standing
# operating rules is never stopped by a validation script.
FAILCLOSED_DATA_DIR="$PILOT_DIR/data-failclosed"
go run ./cmd/factoryd \
  -workspace "$PILOT_DIR/workspace-failclosed" \
  -spec "$TICKET" \
  -ticket 001 \
  -ticket-file "$TICKET" \
  -verify-command "true" \
  -data-dir "$FAILCLOSED_DATA_DIR" \
  -temporal-address "127.0.0.1:1" || true
FAILCLOSED_RUN_ID="$(ls -t "$FAILCLOSED_DATA_DIR"/runs | head -1)"
python3 - "$FAILCLOSED_DATA_DIR/runs/$FAILCLOSED_RUN_ID/run.json" <<'PY'
import json, sys
run = json.load(open(sys.argv[1]))
state = run.get("state")
confirmed = run.get("halt_confirmed")
print(f"fail-closed check: state={state!r} halt_confirmed={confirmed!r}")
assert state == "halted" and confirmed, (
    f"expected halted/halt_confirmed=true when Temporal is unreachable, "
    f"got state={state!r} halt_confirmed={confirmed!r} -- possible "
    f"execution outside Temporal, exactly the regression this check exists "
    f"to catch"
)
PY
echo "== fail-closed check passed: the run halted rather than running elsewhere =="

echo "== running ticket 001 through the Temporal path (independent RunWorkflow) =="
go run ./cmd/factoryd \
  -workspace "$PILOT_DIR/workspace" \
  -spec "$TICKET" \
  -ticket 001 \
  -ticket-file "$TICKET" \
  -verify-command "true" \
  -data-dir "$PILOT_DIR/data" \
  -temporal-address "$TEMPORAL_ADDRESS"

RUN_ID="$(ls -t "$PILOT_DIR"/data/runs | head -1)"
echo "== run $RUN_ID =="
go run ./cmd/factoryd status -data-dir "$PILOT_DIR/data" -n 1

python3 - "$PILOT_DIR/data/runs/$RUN_ID/run.json" <<'PY'
import json, sys
run = json.load(open(sys.argv[1]))
state = run.get("state")
print(f"state: {state}")
assert state in ("accepted", "quarantined", "halted"), f"unexpected terminal state {state!r}"
PY

echo "== Phase 2 (steps 0-1) done. Pilot dir left at $PILOT_DIR for inspection; delete it when done. =="
echo "== Continue with the plan's steps 2-6 (repository exclusivity, hard termination, flake check, daemon control plane) by hand. =="
