#!/usr/bin/env python3
"""Fixture stand-in for conformity_review.py, used only by the end-to-end
worker test: writes CONFORMITY_EVIDENCE.json with a clean verdict for every
numbered criterion, where the real script writes it.
"""
import argparse
import json
import os
import re


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--workspace", required=True)
    parser.add_argument("--spec-acceptance-criteria", required=True)
    parser.add_argument("--conformity-policy", required=True)
    args, _ = parser.parse_known_args()

    with open(args.spec_acceptance_criteria) as f:
        criteria = [line.strip() for line in f if re.match(r"\d+\. ", line)]
    evidence = {
        "schema_version": 1,
        "conformity_policy": args.conformity_policy,
        "succeeded": True,
        "stopped_reason": "spec conformity review cleared every criterion",
        "error": "",
        "review_verdicts": [{"criterion": c, "verdict": "clean"} for c in criteria],
    }
    with open(os.path.join(args.workspace, "CONFORMITY_EVIDENCE.json"), "w") as f:
        json.dump(evidence, f)


if __name__ == "__main__":
    main()
