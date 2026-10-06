#!/usr/bin/env python3
"""Fixture stand-in for draft_spec.py, used only by the end-to-end worker
test. It writes a spec with the required headings and one acceptance
criterion, and the evidence file, where the real script writes them.
"""
import argparse
import json
import os

SPEC = """# Spec

## Problem

content.txt needs one more line.

## Scope

Append a line to content.txt.

## Non-goals

Nothing else changes.

## Affected services and packages

- content.txt

## Acceptance criteria

1. content.txt has one more line than before.

## Risks

None.

## Open questions

None.
"""


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--workspace", required=True)
    parser.add_argument("--request", required=True)
    parser.add_argument("--out", required=True)
    parser.add_argument("--evidence", required=True)
    args, _ = parser.parse_known_args()

    os.makedirs(os.path.dirname(args.out), exist_ok=True)
    with open(args.out, "w") as f:
        f.write(SPEC)
    with open(args.evidence, "w") as f:
        json.dump({"schema_version": 1, "usage": {}, "agent_exit_code": 0, "duration_s": 0.1, "agents_md_used": False}, f)


if __name__ == "__main__":
    main()
