#!/usr/bin/env python3
"""Fixture stand-in for plan_tickets.py, used only by the end-to-end worker
test. It writes one ticket that covers criterion 1 and changes content.txt
(the file build_app.py beside it edits), and the evidence
file, where the real script writes them.
"""
import argparse
import json
import os

TICKET = """Verify-Command: {verify_command}
Allowed-Files: content.txt
Required-Changed-Files: content.txt
Tests-Required: no -- the fixture build adds no test

## Goal

Append a line to content.txt.

## Plan

### Files to touch

- content.txt

### Steps

1. Edit content.txt.

### Tests to add

- none

### Acceptance criteria covered

- 1

## Out of scope

Nothing else.
"""


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--workspace", required=True)
    parser.add_argument("--spec", required=True)
    parser.add_argument("--verify-command", required=True)
    parser.add_argument("--out-dir", required=True)
    parser.add_argument("--evidence", required=True)
    args, _ = parser.parse_known_args()

    os.makedirs(args.out_dir, exist_ok=True)
    with open(os.path.join(args.out_dir, "001.spec.md"), "w") as f:
        f.write(TICKET.format(verify_command=args.verify_command))
    with open(args.evidence, "w") as f:
        json.dump({"schema_version": 1, "usage": {}, "agent_exit_code": 0, "duration_s": 0.1, "agents_md_used": False}, f)


if __name__ == "__main__":
    main()
