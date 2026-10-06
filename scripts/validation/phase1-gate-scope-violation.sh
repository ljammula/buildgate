#!/bin/sh
# scope_violation half of the Phase 1 gate tests, split from
# phase1-gate-build-app.sh because container environment variables never
# reach the sandboxed build script (found live 2026-09-14: an --env
# allowlist, not the host's full environment, is what a worker container
# gets -- see containment-matrix.md's Credentials row), so a single
# env-var-switched script can't actually select its own behavior from
# -build-app-script's own invocation.
#
# Modifies an already-tracked file outside the ticket's declared
# Allowed-Files -- not a brand-new untracked one: found live the same day
# that factoryd's own safety-net commit (which stages this script's
# uncommitted changes after the container exits, since .git is read-only
# inside it -- see phase1-gate-build-app.sh's own comment) only reliably
# picks up modifications to files git already tracks, not new untracked
# ones sitting in the worktree. A ticket whose own required change touches
# an already-tracked file (e.g. an existing Makefile) is what actually
# needs -- and this script needs Makefile to pre-exist in the workspace it
# runs against.
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
echo "gate-test $(date -u +%FT%TZ): scope_violation" >> PROGRESS.md
echo "- Gate test (scope_violation): scripted change, no real app impact." >> ARCHITECTURE.md
echo "# gate-test: touched outside Allowed-Files" >> Makefile
cat > BUILD_REPORT.md <<'EOF'
# Build report

Status: SUCCEEDED
EOF
exit 0
