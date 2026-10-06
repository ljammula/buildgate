#!/usr/bin/env python3
"""Combined spec-conformity + code review pass: ONE sandboxed launch, ONE
model turn, covering both conformity_review.py's own per-criterion
spec-conformity review and code_review.py's own free-form code review of
the same diff.

Why this exists: measured on a live example-app request (2026-09-28), review
was 50% of buildgate's whole spend ($7.32 of $14.71). The spec-conformity
review and the code review were two separate fresh Pi sessions in two
separate sandboxes over the SAME inlined diff -- each paying Pi's own
system prompt, its own sandbox start, and a prompt sized to that diff
again, with two different prompt openings defeating prefix caching
between them. The operator approved merging them into one launch when
both are enabled (see internal/reviewstep.Plan on the Go side, which
returns this script's step in place of the two standalone ones whenever
both spec-conformity and code review are enabled for a run).

This script asks for both reviews in one prompt (diff first -- a stable
prefix identical in shape to code_review_prompt's/spec_conformity_prompt's
own diff block -- then the ticket spec, then the two tasks, each stated
with the exact same rule text the two standalone reviewers already use;
see build_app.py's own CONFORMITY_* constants and code_review.py's own
SCOPE_RULE/SEVERITY_RULE/COMMAND_OUTCOME_RULE/JSON_CONTRACT) and parses
the one JSON answer with the EXISTING build_app.parse_conformity_verdicts
and code_review.parse_code_review_findings -- both already tolerate extra
keys alongside the one they look for (parse_conformity_verdicts reads
only "criteria", parse_code_review_findings only "findings"), so the
combined {"criteria": [...], "findings": [...]} answer parses with each
unchanged.

Writes BOTH CONFORMITY_EVIDENCE.json and CODE_REVIEW_EVIDENCE.json, each
in exactly conformity_review.py's/code_review.py's own existing schema --
so every Go reader, golden test, and evidence loader on both the direct
and Temporal paths keeps working unchanged; nothing here is aware this
combined evidence came from one launch instead of two, and the Go side
only widens what launched it, not what the evidence looks like.

Usage:
    python3 combined_review.py --workspace /path/to/app --spec ticket.md \\
        --spec-acceptance-criteria criteria.md \\
        [--conformity-policy required] [--review-policy required] \\
        [--review-base-sha <sha>]

Exit code encodes BOTH outcomes as two independent bits (never a single
pass/fail): bit 0 (1) set iff the conformity review did not succeed under
--conformity-policy; bit 1 (2) set iff the code review did not succeed
under --review-policy. 0 means both succeeded; 3 means both failed.
"""
from __future__ import annotations

import argparse
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import build_app  # noqa: E402
import harness_adapters  # noqa: E402
import code_review  # noqa: E402
import conformity_review  # noqa: E402
import prompt_templates  # noqa: E402


# Exit status = COMBINED_EXIT_BASE + bits (bit 0: conformity did not
# succeed; bit 1: code review did not succeed). The offset keeps a crash
# (Python exits 1 on an uncaught exception, e.g. a failed import) from
# decoding as a pass. Must match reviewstep.CombinedExitBase in Go.
COMBINED_EXIT_BASE = 40

# The combined turn does two reviews' work, so it gets twice a standalone
# review's 10 minutes (build_app.run_review_turn's default). Found live
# (todo-kafka-service, 2026-10-04): a 13-criterion spec with a 630-line diff
# was still reading files at 10 minutes, twice, and the ticket was
# quarantined with no verdict at all.
COMBINED_REVIEW_TIMEOUT_MINUTES = 20

DIFF_SELF = prompt_templates.load("combined_review.diff_self", ("base",))
DIFF_INLINE = prompt_templates.load("combined_review.diff_inline", ("diff",))
PROMPT = prompt_templates.load(
	"combined_review",
	(
		"diff_block", "spec_text", "criteria", "conformity_command_outcome_rule", "conformity_formatting_rule",
		"scope_rule", "severity_rule", "command_outcome_rule",
	),
)


def combined_review_prompt(
	criteria: list[str], spec_text: str, review_base_sha: str | None, diff: tuple[str, str] | None = None,
) -> str:
	"""Builds the one prompt covering both tasks. diff, when given, is
	(stat, diff_text) from build_app.workspace_diff, inlined via
	build_app.format_diff_for_prompt exactly as both standalone reviewers
	already do -- see spec_conformity_prompt's/code_review_prompt's own
	doc comments for the live incident (example-app run 3, 2026-09-28) that
	makes inlining the diff, rather than asking the reviewer to fetch it,
	load-bearing here too. The diff comes first, before the ticket spec,
	so it is a stable prefix shared byte-for-byte across every combined-
	review launch for the same commit -- diff first, spec is unique per
	ticket but far smaller, and the tasks/rules text below is a fixed
	suffix identical across every launch of this script."""
	if diff is None:
		diff_block = DIFF_SELF.format(base=review_base_sha or "the commit this session started from")
	else:
		stat, diff_text = diff
		diff_block = DIFF_INLINE.format(diff=build_app.format_diff_for_prompt(stat, diff_text, review_base_sha))
	return PROMPT.format(
		diff_block=diff_block.removesuffix("\n"), spec_text=spec_text, criteria="\n".join(criteria),
		conformity_command_outcome_rule=build_app.CONFORMITY_COMMAND_OUTCOME_RULE,
		conformity_formatting_rule=build_app.CONFORMITY_FORMATTING_RULE,
		scope_rule=code_review.SCOPE_RULE, severity_rule=code_review.SEVERITY_RULE,
		command_outcome_rule=code_review.COMMAND_OUTCOME_RULE,
	).removesuffix("\n")


def run_combined_review(
	workspace: Path, *, criteria_path: Path, spec_path: Path,
	conformity_policy: str = "required", review_policy: str = "required",
	review_base_sha: str | None = None, thinking: str | None = None, adapter=build_app.DEFAULT_ADAPTER,
) -> tuple[dict, dict]:
	"""Runs the one combined turn and returns (conformity_evidence,
	code_review_evidence) -- both JSON-serializable evidence dicts in
	exactly conformity_review.py's/code_review.py's own existing shapes.
	An unavailable turn (a subprocess/relay error, or no parseable JSON
	answer at all) yields both evidences unavailable, each recording the
	turn's own error -- parse_conformity_verdicts defaults every criterion
	to "unavailable" and parse_code_review_findings returns available=False
	when it finds no "criteria"/"findings" list at all, exactly as either
	standalone script already behaves for its own unavailable turn."""
	criteria = build_app.read_acceptance_criteria(criteria_path)
	spec_text = spec_path.read_text()
	diff = build_app.workspace_diff(workspace, review_base_sha) if review_base_sha else None
	diff_fetch_failed = review_base_sha is not None and diff is None
	prompt = combined_review_prompt(criteria, spec_text, review_base_sha, diff=diff)
	turn = build_app.run_review_turn(
		workspace, prompt=prompt, session_dir=workspace / ".pi-combined-review-session",
		review_base_sha=review_base_sha, thinking=thinking, adapter=adapter,
		timeout_minutes=COMBINED_REVIEW_TIMEOUT_MINUTES,
	)
	error = build_app.single_line(build_app.redact(turn.error, 100_000)) if turn.error else ""

	conformity_verdicts = build_app.parse_conformity_verdicts(turn.text, criteria)
	available, findings = code_review.parse_code_review_findings(turn.text)

	conformity = conformity_review.conformity_evidence(
		conformity_verdicts, error, conformity_policy=conformity_policy, thinking=thinking,
	)
	review = code_review.code_review_evidence(
		available, findings, error, review_policy=review_policy, thinking=thinking,
		diff_fetch_failed=diff_fetch_failed,
	)
	return conformity, review


def write_combined_evidence(workspace: Path, conformity: dict, review: dict) -> tuple[Path, Path]:
	conformity_path = conformity_review.write_conformity_evidence_json(workspace, conformity)
	review_path = code_review.write_code_review_evidence_json(workspace, review)
	return conformity_path, review_path


def main() -> int:
	parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
	parser.add_argument("--workspace", required=True, type=Path)
	parser.add_argument("--spec", required=True, type=Path, help="The ticket's approved spec, given to the reviewer as context only (task B).")
	parser.add_argument("--spec-acceptance-criteria", required=True, type=Path)
	parser.add_argument("--conformity-policy", choices=("required", "advisory"), default="required")
	parser.add_argument("--review-policy", choices=("required", "advisory"), default="required")
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

	conformity, review = run_combined_review(
		args.workspace.resolve(), criteria_path=args.spec_acceptance_criteria,
		spec_path=args.spec.resolve(), conformity_policy=args.conformity_policy,
		review_policy=args.review_policy, review_base_sha=args.review_base_sha,
		thinking=args.thinking, adapter=adapter,
	)
	conformity_path, review_path = write_combined_evidence(args.workspace.resolve(), conformity, review)
	print(f"Conformity evidence written to {conformity_path}")
	print(f"Outcome: {'SUCCEEDED' if conformity['succeeded'] else 'DID NOT SUCCEED'} -- {conformity['stopped_reason']}")
	print(f"Code review evidence written to {review_path}")
	print(f"Outcome: {'SUCCEEDED' if review['succeeded'] else 'DID NOT SUCCEED'} -- {review['stopped_reason']}")

	exit_code = COMBINED_EXIT_BASE
	if not conformity["succeeded"]:
		exit_code |= 1
	if not review["succeeded"]:
		exit_code |= 2
	return exit_code


if __name__ == "__main__":
	sys.exit(main())
