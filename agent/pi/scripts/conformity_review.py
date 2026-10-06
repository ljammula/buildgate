#!/usr/bin/env python3
"""Standalone spec-conformity review pass, run as its OWN sandboxed
launch, separate from build_app.py's own round loop -- see
run_ticket.go's doc comment on the two-phase design this exists for
(search that file for "conformity review, phase 2").

Why this is a separate script rather than a mode flag on build_app.py:
build_app.py's own main() always overwrites BUILD_EVIDENCE.json/
BUILD_REPORT.md with this invocation's own round data; reusing it for a
conformity-only pass (no rounds at all) would clobber phase 1's already-
recorded evidence with an empty-rounds result the moment phase 2 ran.
This script writes a distinct CONFORMITY_EVIDENCE.json instead, so
phase 1's evidence is never at risk.

Why this never touches git: build_app.py's own PR #175 history (see its
`agent_pi_conformity_review` git log) and a 2026-09-17 live run already
found that a commit attempted from inside factoryd's sandbox always fails --
internal/sandbox/docker.go mounts `.git` read-only there unconditionally,
by deliberate security design. factoryd's own host-side safety-net
commit (run_ticket.go) is expected to have already landed, on the host,
before this script ever launches -- this script only reads the workspace
the reviewer diffs against, never writes to git.

Usage:
    python3 conformity_review.py --workspace /path/to/app \\
        --spec-acceptance-criteria criteria.md \\
        [--conformity-policy required] [--review-base-sha <sha>]

Writes CONFORMITY_EVIDENCE.json into --workspace. Exit code 0 iff the
review passed under the declared --conformity-policy.
"""
from __future__ import annotations

import argparse
import json
import sys
from datetime import datetime, timezone
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import build_app  # noqa: E402
import harness_adapters  # noqa: E402

CONFORMITY_EVIDENCE_SCHEMA_VERSION = 1


def _unavailable_stopped_reason(error: str) -> str:
	"""Builds the stopped_reason override for a review where every
	criterion came back "unavailable" and the underlying ReviewTurn
	carries its own error (e.g. the relay's sliding-window limiter
	answering "429 token budget exceeded") -- mirrors code_review.py's own
	_unavailable_stopped_reason. Before this, CONFORMITY_EVIDENCE.json's
	stopped_reason just listed every criterion as unaccounted for (`spec
	conformity review did not clear all criteria: [...]`), with the actual
	cause nowhere in the evidence -- found live, example-app run 3, 2026-09-28:
	30 tool-exploring turns burned the relay's whole hourly token budget,
	the review ended in three straight 429s with no verdicts at all, and
	an operator saw only "unavailable"/"no-review-verdict" for every
	criterion. Single line, capped 300 chars (error text is untrusted
	pi-stderr/relay content, same redaction/bounding
	code_review._unavailable_stopped_reason already applies before it
	lands in operator-facing evidence)."""
	return build_app.single_line(build_app.redact(f"spec conformity review was unavailable: {error}", 100_000))[:300]


def conformity_evidence(verdicts: list[dict], error: str, *, conformity_policy: str, thinking: str | None) -> dict:
	"""Builds the CONFORMITY_EVIDENCE.json shape from already-parsed
	per-criterion verdicts and the underlying turn's own error string --
	shared by run_conformity_review below (a standalone, conformity-only
	turn) and combined_review.py's own run_combined_review (one turn whose
	single JSON answer covers both this review and code_review.py's own
	code review), so the two callers can never diverge on how a verdict
	list turns into a succeeded/stopped_reason outcome."""
	non_clean = [v["criterion"] for v in verdicts if v["verdict"] != "clean"]
	if non_clean and conformity_policy == "required":
		succeeded = False
		stopped_reason = f"spec conformity review did not clear all criteria: {non_clean}"
	elif non_clean:
		succeeded = True
		stopped_reason = f"spec conformity review flagged criteria under advisory policy: {non_clean}"
	else:
		succeeded = True
		stopped_reason = "spec conformity review cleared every criterion"
	# Only every verdict landing "unavailable" (never a mix of clean/flagged
	# and unavailable) gets the cause named in place of the generic
	# non-clean listing above -- see _unavailable_stopped_reason's own doc
	# comment for the incident this covers.
	if error and verdicts and all(v["verdict"] == "unavailable" for v in verdicts):
		stopped_reason = _unavailable_stopped_reason(error)
	return {
		"schema_version": CONFORMITY_EVIDENCE_SCHEMA_VERSION,
		"generated": datetime.now(timezone.utc).isoformat(),
		"conformity_policy": conformity_policy,
		"thinking": thinking,
		"succeeded": succeeded,
		"stopped_reason": stopped_reason,
		"error": error,
		"review_verdicts": verdicts,
	}


def run_conformity_review(
	workspace: Path, *, criteria_path: Path, conformity_policy: str = "required",
	review_base_sha: str | None = None, thinking: str | None = None, adapter=build_app.DEFAULT_ADAPTER,
) -> dict:
	"""Runs the conformity review and returns a JSON-serializable evidence
	dict (the same shape write_conformity_evidence_json writes to disk),
	so callers/tests can inspect the outcome without round-tripping
	through a file."""
	criteria = build_app.read_acceptance_criteria(criteria_path)
	verdicts, turn_error = build_app.run_spec_conformity_review(
		workspace, criteria=criteria, review_base_sha=review_base_sha,
		thinking=thinking, adapter=adapter,
	)
	error = build_app.single_line(build_app.redact(turn_error, 100_000)) if turn_error else ""
	return conformity_evidence(verdicts, error, conformity_policy=conformity_policy, thinking=thinking)


def write_conformity_evidence_json(workspace: Path, evidence: dict) -> Path:
	path = workspace / "CONFORMITY_EVIDENCE.json"
	path.write_text(json.dumps(evidence, indent=2) + "\n")
	return path


def main() -> int:
	parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
	parser.add_argument("--workspace", required=True, type=Path)
	parser.add_argument("--spec-acceptance-criteria", required=True, type=Path)
	parser.add_argument("--conformity-policy", choices=("required", "advisory"), default="required")
	parser.add_argument(
		"--review-base-sha", default=None,
		help="Commit the independent reviewer should diff against -- pass the same ticket-start boundary build_app.py's own phase 1 used.",
	)
	parser.add_argument(
		"--harness", choices=sorted(harness_adapters.ADAPTERS), default="pi",
		help="Coding agent to drive (see harness_adapters.py).",
	)
	parser.add_argument(
		"--thinking", choices=build_app.THINKING_LEVELS, default=None,
		help="Pi thinking level for this job; omitted inherits Pi's installed setting.",
	)
	args = parser.parse_args()
	adapter = harness_adapters.get(args.harness)
	adapter.prepare()

	evidence = run_conformity_review(
		args.workspace.resolve(), criteria_path=args.spec_acceptance_criteria,
		conformity_policy=args.conformity_policy, review_base_sha=args.review_base_sha,
		thinking=args.thinking, adapter=adapter,
	)
	evidence_path = write_conformity_evidence_json(args.workspace.resolve(), evidence)
	print(f"Evidence written to {evidence_path}")
	print(f"Outcome: {'SUCCEEDED' if evidence['succeeded'] else 'DID NOT SUCCEED'} -- {evidence['stopped_reason']}")
	return 0 if evidence["succeeded"] else 1


if __name__ == "__main__":
	sys.exit(main())
