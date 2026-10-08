#!/bin/sh
# stage-agent-tests.sh <dest>: copies buildgate's build scripts, their tests
# and the files those tests read into <dest>, laid out as in this repository,
# so the suite can run somewhere this checkout is not: `make
# project-sandbox-image` runs it on a project's own Python inside the image it
# builds (internal/sandbox/Dockerfile.project).
#
# Left out: a test module that imports pytest, which no worker image carries.
# A new test that reads a file outside agent/pi needs that file added here.
set -eu
dest="$1"
root="$(cd "$(dirname "$0")/.." && pwd)"
mkdir -p "$dest/agent/pi" "$dest/cmd/factoryd/testdata" "$dest/internal/meter/testdata"
cp -R "$root/agent/pi/scripts" "$root/agent/pi/tests" "$dest/agent/pi/"
cp "$root/cmd/factoryd/testdata/sanitize_vectors.json" "$root/cmd/factoryd/testdata/criterion_key_vectors.json" "$dest/cmd/factoryd/testdata/"
cp "$root/internal/meter/testdata/chatgpt_codex_function_call.sse" "$dest/internal/meter/testdata/"
find "$dest" -name __pycache__ -type d -prune -exec rm -rf {} +
for test in "$dest"/agent/pi/tests/*.py; do
	if grep -qE '^(import|from) pytest' "$test"; then rm -f "$test"; fi
done
