#!/bin/sh
# A slow build_app.py stand-in for Phase 1.5's cancellation tests: sleeps
# well past any reasonable interrupt window so the caller's own SIGINT/
# SIGKILL is what ends it, not this script finishing on its own.
set -eu
echo "hanging..."
sleep 600
