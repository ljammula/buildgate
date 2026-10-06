#!/usr/bin/env python3
"""Deterministic build_app.py stand-in for queue-run's own building step
(Phase 1.7), which -- unlike factoryd <run>'s own -build-app-interpreter
flag -- always invokes its build script with "python3" and has no CLI
override for that. See scripts/validation/phase1-gate-build-app.sh's own
comment for why files are left uncommitted (the worker's .git is
read-only; factoryd's own safety-net commit picks up modifications to
already-tracked files)."""
import argparse
import os

p = argparse.ArgumentParser()
p.add_argument("--workspace", required=True)
args, _ = p.parse_known_args()

os.chdir(args.workspace)
with open("PROGRESS.md", "a") as f:
    f.write("gate-test (queue-run building step)\n")
with open("ARCHITECTURE.md", "a") as f:
    f.write("- Gate test: scripted change via queue-run, no real app impact.\n")
with open("BUILD_REPORT.md", "w") as f:
    f.write("# Build report\n\nStatus: SUCCEEDED\n")
