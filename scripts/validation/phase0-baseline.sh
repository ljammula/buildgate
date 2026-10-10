#!/usr/bin/env bash
# Phase 0 of doc/designs/validation-plan.md: the three toolchains, then the
# live-gated Go pass, then factoryd doctor. Run from the repo root.
set -euo pipefail
cd "$(dirname "$0")/../.."

echo "== make verify =="
time make verify

echo "== make console-test =="
time make console-test

echo "== agent/pi pytest =="
python3 -m pytest agent/pi/tests/ -q

echo "== live-gated Go pass (real Docker + Temporal, sequential) =="
: "${DOCKER_SANDBOX_LIVE_ROOT:=$HOME/buildgate/live-root}"
mkdir -p "$DOCKER_SANDBOX_LIVE_ROOT"
# verify-live starts the test Temporal server the live tests dial.
DOCKER_SANDBOX_LIVE_ROOT="$DOCKER_SANDBOX_LIVE_ROOT" make verify-live

echo "== factoryd doctor =="
# Under DOCKER_SANDBOX_LIVE_ROOT, not a plain `mktemp -d` (which defaults to
# macOS's own $TMPDIR): on colima only paths under $HOME are shared into the
# Docker VM, so a $TMPDIR-rooted workspace fails doctor's own mount-
# visibility check for infrastructure reasons unrelated to what this phase
# is actually trying to prove (found live 2026-09-14, see the plan's Phase 0
# findings).
DOCTOR_WORKSPACE="$(mktemp -d "$DOCKER_SANDBOX_LIVE_ROOT/doctor-workspace.XXXXXX")"
trap 'rm -rf "$DOCTOR_WORKSPACE"' EXIT
go run ./cmd/factoryd doctor \
  -temporal-address "$TEMPORAL_ADDRESS" \
  -registry-proxy \
  -workspace "$DOCTOR_WORKSPACE"

echo "== Phase 0 done: fill in the results table in doc/designs/validation-plan.md =="
