#!/bin/sh
# A deterministic build_app.py stand-in for Phase 1.4-1.6 of
# doc/designs/validation-plan.md: gate/evidence-mechanism tests that don't
# need a real model round (the real-model happy path is already proven by
# Phase 1.1-1.3). Makes the ticket's own required change (an ARCHITECTURE.md
# note plus a PROGRESS.md entry) and claims success -- pair with a ticket
# whose own Verify-Command: genuinely fails to exercise the canonical_verify
# gate (SC-006: a claimed success never overrides a real verify failure).
#
# No mode switch: container environment variables never reach a sandboxed
# build script (found live 2026-09-14 -- only a fixed --env allowlist does,
# see containment-matrix.md's Credentials row), so an env-var-selected
# behavior silently never activates. See phase1-gate-scope-violation.sh for
# the diff_scope gate's own separate script, for the same reason.
set -eu
workspace=""
while [ $# -gt 0 ]; do
  case "$1" in
    --workspace) workspace="$2"; shift 2 ;;
    *) shift ;;
  esac
done
[ -n "$workspace" ] || { echo "missing --workspace" >&2; exit 2; }
cd "$workspace"

echo "gate-test $(date -u +%FT%TZ)" >> PROGRESS.md
echo "- Gate test: scripted change, no real app impact." >> ARCHITECTURE.md

# Deliberately left uncommitted, not `git add`+`git commit` here: the
# worker's .git is bind-mounted read-only in the real sandbox (found live
# 2026-09-14 -- `git commit` inside the container failed with "Unable to
# create .../index.lock: Read-only file system", turning this into an
# infrastructure failure instead of the gate test it was meant to be). A
# real agent leaves its diff uncommitted the same way; factoryd's own
# safety-net commit (run host-side, after the container exits) is what
# actually commits it -- see "agent left its diff uncommitted; factoryd
# committed it as a safety net" in a real run's own log output. Note this
# also means only already-tracked-file modifications reliably survive the
# safety net -- a brand-new untracked file does not (see
# phase1-gate-scope-violation.sh's own note).

cat > BUILD_REPORT.md <<'EOF'
# Build report

Status: SUCCEEDED

This is a scripted stand-in report for a Phase 1 gate test -- see
scripts/validation/README.md. Its claim of success is exactly what SC-006
(external oracle only) says must never be trusted on its own.
EOF

exit 0
