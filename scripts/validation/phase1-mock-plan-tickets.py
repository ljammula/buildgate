#!/usr/bin/env python3
"""Deterministic stand-in for agent/pi/scripts/plan_tickets.py, for
Phase 1.7 of doc/designs/validation-plan.md: exercises the request
pipeline's planning -> plan_review state machine without a real model
round. Writes one ticket file satisfying internal/request's
ValidateTicketPlan (fixed heading skeleton) and internal/policy's
TicketStructureBrownfield (Verify-Command:/Allowed-Files:/
Required-Changed-Files: header lines), claiming spec acceptance
criterion 1 (the only one phase1-mock-draft-spec.py's spec declares) so
ValidatePlanCoverage is satisfied too. Also writes an evidence.json
matching planTicketsEvidencePayload's own field names.

Argv mirrors planTicketsArgs exactly: --workspace --spec --request
--verify-command --out-dir --evidence --timeout-minutes (see
cmd/factoryd/plan_tickets_job.go).
"""
import argparse
import json
import os
import sys
from datetime import datetime, timezone

p = argparse.ArgumentParser()
p.add_argument("--workspace", required=True)
p.add_argument("--spec", required=True)
p.add_argument("--request", required=True)
p.add_argument("--verify-command", required=True)
p.add_argument("--out-dir", required=True)
p.add_argument("--evidence", required=True)
p.add_argument("--timeout-minutes", type=int, default=0)
args = p.parse_args()

os.makedirs(args.out_dir, exist_ok=True)
os.makedirs(os.path.dirname(args.evidence), exist_ok=True)

ticket = f"""This is an existing repo.

# Ticket 001: gate test

Verify-Command: {args.verify_command}
Allowed-Files: ARCHITECTURE.md, PROGRESS.md
Required-Changed-Files: PROGRESS.md
Tests-Required: no -- gate test, not app code

## Goal
exercise the request pipeline's planning/plan_review state machine.

## Plan

### Files to touch
ARCHITECTURE.md, PROGRESS.md

### Steps
Append a PROGRESS.md entry and an ARCHITECTURE.md note.

### Tests to add
None -- scripted gate test, not app code.

### Acceptance criteria covered
- 1

## Out of scope
Any real feature work.
"""

with open(os.path.join(args.out_dir, "001.spec.md"), "w") as f:
    f.write(ticket)

evidence = {
    "generated": datetime.now(timezone.utc).isoformat(),
    "usage": {"input": 0, "output": 0},
    "agent_exit_code": 0,
    "duration_s": 0.1,
    "agents_md_used": False,
}
with open(args.evidence, "w") as f:
    json.dump(evidence, f)

print("phase1-mock-plan-tickets: wrote 1 ticket under", args.out_dir, file=sys.stderr)
