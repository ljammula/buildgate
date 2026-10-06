#!/usr/bin/env python3
"""Deterministic stand-in for agent/pi/scripts/draft_spec.py, for
Phase 1.7 of doc/designs/validation-plan.md: exercises the request
pipeline's spec_drafting -> spec_review state machine without a real
model round. Writes a spec.md satisfying internal/request's
ValidateSpecSkeleton (the fixed heading skeleton, in order, with
non-blank content under "## Acceptance criteria") and an evidence.json
matching draftSpecEvidencePayload's own field names.

Argv mirrors draftSpecArgs exactly: --workspace --request --out
--evidence --timeout-minutes (see cmd/factoryd/spec_draft_job.go).
"""
import argparse
import json
import os
import sys
from datetime import datetime, timezone

p = argparse.ArgumentParser()
p.add_argument("--workspace", required=True)
p.add_argument("--request", required=True)
p.add_argument("--out", required=True)
p.add_argument("--evidence", required=True)
p.add_argument("--timeout-minutes", type=int, default=0)
args = p.parse_args()

request_text = ""
try:
    with open(args.request) as f:
        request_text = f.read().strip()
except OSError as e:
    print(f"warning: could not read request text: {e}", file=sys.stderr)

spec = f"""# Spec

## Problem
{request_text or "Phase 1.7 gate test: exercise the request pipeline's state machine."}

## Scope
A single scripted change, no real app impact -- this spec exists to
drive submitted -> spec_drafting -> spec_review, not to describe real
work.

## Non-goals
Real feature work. This is a validation-plan gate test.

## Affected services and packages
None -- scripted change only.

## Acceptance criteria
1. The request pipeline reaches spec_review with this spec attached.

## Risks
None; this is a gate test with no production impact.

## Open questions
None.
"""

os.makedirs(os.path.dirname(args.out), exist_ok=True)
os.makedirs(os.path.dirname(args.evidence), exist_ok=True)
with open(args.out, "w") as f:
    f.write(spec)

evidence = {
    "generated": datetime.now(timezone.utc).isoformat(),
    "usage": {"input": 0, "output": 0},
    "agent_exit_code": 0,
    "duration_s": 0.1,
    "agents_md_used": False,
}
with open(args.evidence, "w") as f:
    json.dump(evidence, f)

print("phase1-mock-draft-spec: wrote", args.out)
