#!/usr/bin/env python3
"""The review step: ONE sandboxed launch, ONE model turn, a free-form code
review of the diff. Without --reproductions it is advice on what tests cannot
show (design, security, maintainability, risk), at most
code_review.MAX_ADVICE_FINDINGS findings, no spec and no reproductions. With
--reproductions the approved spec is shown numbered so a finding can cite one
of its lines and carry a reproduction (SC-022).

With --reproductions, a finding blocks a run only through a reproduction the
factory runs itself (SC-022); the reviewer writes no file. Each finding in the answer may then carry:

    "spec_line":    {"number": <int>, "text": "<the spec line, verbatim>"}
    "reproduction": {"path": "<new test file>", "content": "<the file>"}

Both are parsed by code_review.parse_code_review_findings; at most
code_review.MAX_REPRODUCTIONS reproductions of at most
code_review.MAX_REPRODUCTION_BYTES each are kept (a finding past the cap
stays, as advice, with a note in the evidence).

The prompt is built diff first -- a stable prefix, identical in shape to
code_review_prompt's own diff block -- then the ticket spec, the numbered
approved spec, and the rules. An answer with no parseable findings list gets
one further turn (see run_combined_review). A turn that fails gets none: the
host decides from the spend meter's record whether to launch the review
again.

Writes CODE_REVIEW_EVIDENCE.json in code_review.py's schema (the only
evidence file).

Usage:
    python3 combined_review.py --workspace /path/to/app --spec ticket.md \\
        [--reproductions] [--approved-spec spec.md] [--review-policy required] \\
        [--review-base-sha <sha>]

Exit code: COMBINED_EXIT_BASE (40) when the review gave a verdict under
--review-policy, COMBINED_EXIT_BASE + 2 (42) when it did not. Findings never
change it.
"""
from __future__ import annotations

import argparse
import sys
import time
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import build_app  # noqa: E402
import harness_adapters  # noqa: E402
import code_review  # noqa: E402
import prompt_templates  # noqa: E402


# Exit status = COMBINED_EXIT_BASE + NO_VERDICT_BIT when the review gave no
# verdict, COMBINED_EXIT_BASE when it did. The offset keeps a crash (Python
# exits 1 on an uncaught exception, e.g. a failed import) from decoding as a
# pass. Must match reviewstep.CombinedExitBase in Go.
COMBINED_EXIT_BASE = 40
NO_VERDICT_BIT = 2

# Found live (todo-kafka-service, 2026-10-04): a 630-line diff was still
# reading files at 10 minutes, twice, and the ticket was quarantined with no
# verdict at all.
COMBINED_REVIEW_TIMEOUT_MINUTES = 20

# A retry never lengthens the review: the first turn and the retry together
# get the COMBINED_REVIEW_TIMEOUT_MINUTES the one turn always had, so a run's
# own deadline (the build's -timeout-minutes plus five) is reached no sooner
# than before. A retry gets what the first turn left of it, and is skipped
# when under RETRY_MIN_MINUTES are left: less than that has not been enough
# for a review of any real diff.
RETRY_MIN_MINUTES = 5

# The first turn's session folder; the host empties its prompts folder before
# the launch and copies that folder alone afterwards
# (workflow.reviewPromptSession, evidence.RetainPrompts). Before a retry,
# everything else in it (pi's sessions, a codex or copilot home: the first
# turn's transcript and answer) is removed, and the retry runs in a new folder
# below it, saving its prompt beside the first one under its own name.
SESSION_DIR = ".pi-combined-review-session"
RETRY_SESSION_SUBDIR = "retry"
RETRY_PROMPT_NAME = "review-combined-retry"

# The clock the step's budget is measured on; a name of its own so a test can
# replace it without touching time.monotonic for everything else.
clock = time.monotonic

DIFF_SELF = prompt_templates.load("combined_review.diff_self", ("base",))
DIFF_INLINE = prompt_templates.load("combined_review.diff_inline", ("diff",))
PROMPT = prompt_templates.load(
	"combined_review",
	("diff_block", "spec_text", "approved_spec", "scope_rule", "severity_rule", "reproduction_rule", "command_outcome_rule"),
)
ADVICE_PROMPT = prompt_templates.load(
	"combined_review.advice", ("diff_block", "spec_text", "scope_rule", "severity_rule", "command_outcome_rule"),
)

NO_APPROVED_SPEC = (
	"No approved spec is given, so no spec line can be cited: every finding is advice and needs no reproduction."
)


def numbered_spec(text: str) -> str:
	"""The approved spec as the reviewer cites it: every line of the file,
	blank ones too, as `L<n>: <line>` with n counting from 1, so a number is
	the line's number in the file the host checks the citation against."""
	lines = text.split("\n")
	if lines and lines[-1] == "":
		lines.pop()
	body = "\n".join(f"L{n}: {line.rstrip(chr(13))}" for n, line in enumerate(lines, start=1))
	return (
		"--- approved spec, numbered (cite a line by its number and its text) ---\n"
		f"{body}\n"
		"--- end approved spec ---"
	)


def combined_review_prompt(
	spec_text: str, review_base_sha: str | None, diff: tuple[str, str] | None = None,
	instructions_block: str = "", approved_spec: str | None = None, reproductions: bool = False,
) -> str:
	"""Builds the review prompt. diff, when given, is (stat, diff_text) from
	build_app.workspace_diff, inlined via build_app.format_diff_for_prompt as
	code_review_prompt does (see its doc comment for the live incident that
	makes inlining the diff, rather than asking the reviewer to fetch it,
	load-bearing). The diff comes first, before the specs, so it is a stable
	prefix shared byte-for-byte across every launch for the same commit.
	approved_spec, when given, is the approved spec's text, shown numbered
	(numbered_spec); None says no line can be cited. reproductions False builds
	the advice prompt: the design, security, maintainability and risk scope and
	severity rules, no numbered spec (approved_spec is ignored), no
	reproduction rule and an answer format without spec_line or reproduction."""
	if diff is None:
		diff_block = DIFF_SELF.format(base=review_base_sha or "the commit this session started from")
		diff_block = diff_block.removesuffix("\n") + instructions_block
	else:
		stat, diff_text = diff
		diff_block = DIFF_INLINE.format(diff=build_app.format_diff_for_prompt(stat, diff_text, review_base_sha) + instructions_block)
	if not reproductions:
		return ADVICE_PROMPT.format(
			diff_block=diff_block.removesuffix("\n"), spec_text=spec_text,
			scope_rule=code_review.ADVICE_SCOPE_RULE, severity_rule=code_review.ADVICE_SEVERITY_RULE,
			command_outcome_rule=code_review.COMMAND_OUTCOME_RULE,
		).removesuffix("\n")
	return PROMPT.format(
		diff_block=diff_block.removesuffix("\n"), spec_text=spec_text,
		approved_spec=numbered_spec(approved_spec) if approved_spec is not None else NO_APPROVED_SPEC,
		scope_rule=code_review.SCOPE_RULE, severity_rule=code_review.SEVERITY_RULE,
		reproduction_rule=code_review.REPRODUCTION_RULE, command_outcome_rule=code_review.COMMAND_OUTCOME_RULE,
	).removesuffix("\n")


def missing_part(available: bool, error: str) -> str:
	"""Why the first answer calls for a retry, "" when it does not: the
	answer had no parseable findings list. A failed turn (an error) is not
	asked again here. Whether its model was served is the spend meter's
	record, which this script cannot read, and the host decides from it what
	happens next (it launches the review once more when the model served
	nothing)."""
	if available or error:
		return ""
	return "the first answer had no parseable findings list"


def retry_timeout_minutes(started: float) -> int:
	"""The retry's timeout: the whole minutes the first turn left of
	COMBINED_REVIEW_TIMEOUT_MINUTES; 0 when under RETRY_MIN_MINUTES are left."""
	left = int((COMBINED_REVIEW_TIMEOUT_MINUTES * 60 - (clock() - started)) // 60)
	return min(COMBINED_REVIEW_TIMEOUT_MINUTES, left) if left >= RETRY_MIN_MINUTES else 0


def fresh_retry_session_dir(workspace: Path, subdir: str = RETRY_SESSION_SUBDIR) -> Path | None:
	"""Returns the retry's session folder, new and empty, after removing what
	the first turn (or an earlier launch, or the build) left in the review's
	session folder (build_app.fresh_session_subdir). None when the folder
	could not be emptied; the caller then asks nothing."""
	return build_app.fresh_session_subdir(workspace / SESSION_DIR, subdir)


def _evidence(
	available: bool, findings: list[dict], error: str, retry: str, *, review_policy: str, thinking: str | None,
	diff_fetch_failed: bool, reproductions: bool,
) -> dict:
	notes = code_review.cap_reproductions(findings) if reproductions else code_review.cap_advice(findings)
	evidence = code_review.code_review_evidence(
		available, findings, error, review_policy=review_policy, thinking=thinking,
		diff_fetch_failed=diff_fetch_failed, notes=notes,
	)
	if retry:
		evidence["retry"] = retry
	return evidence


def run_combined_review(
	workspace: Path, *, spec_path: Path, approved_spec_path: Path | None = None,
	review_policy: str = "required", review_base_sha: str | None = None, thinking: str | None = None,
	adapter=build_app.DEFAULT_ADAPTER, instructions_diff: Path | None = None, reproductions: bool = False,
) -> dict:
	"""Runs the review and returns the CODE_REVIEW_EVIDENCE.json dict.
	reproductions is --reproductions: the build holds the reproductions the
	findings carry. Without it the review is advice (combined_review_prompt),
	the approved spec is not read, a finding's spec_line and reproduction are
	dropped by the parser, and at most code_review.MAX_ADVICE_FINDINGS are kept.

	When the turn answered and the answer has no parseable findings list, the
	same prompt is asked once more, in one further turn at most
	(missing_part). Found live (2026-10-10): an answer with no parseable JSON
	left the review unavailable, which quarantines a run with no corrective
	build, over a formatting slip. The retry is the same question over the
	same inputs: the diff and the instruction-file block fetched for the
	first turn (never fetched again, so a file the first reviewer touched
	cannot change it), the same base, thinking level and adapter, a session
	folder of its own (fresh_retry_session_dir) and not a word about the
	first turn, whose transcript is removed before it starts. The two turns
	together get the COMBINED_REVIEW_TIMEOUT_MINUTES the one turn had
	(retry_timeout_minutes), and the first turn's evidence is written to the
	workspace before the retry starts, so a retry killed from outside leaves
	what the first turn alone would have.

	A review still without findings after the retry is unavailable, recording
	the retry's own error. The evidence then carries one more key, "retry":
	one line saying what was asked again and why, or why it was not. The key
	is absent when the first answer was whole and when the first turn
	failed."""
	started = clock()
	spec_text = spec_path.read_text()
	approved_spec = approved_spec_path.read_text() if reproductions and approved_spec_path is not None else None
	diff = build_app.workspace_diff(workspace, review_base_sha) if review_base_sha else None
	diff_fetch_failed = review_base_sha is not None and diff is None
	instructions_block = build_app.instructions_diff_block(instructions_diff)
	prompt = combined_review_prompt(
		spec_text, review_base_sha, diff=diff, instructions_block=instructions_block, approved_spec=approved_spec,
		reproductions=reproductions,
	)
	turn = build_app.run_review_turn(
		workspace, prompt=prompt, session_dir=workspace / SESSION_DIR,
		review_base_sha=review_base_sha, thinking=thinking, adapter=adapter,
		timeout_minutes=COMBINED_REVIEW_TIMEOUT_MINUTES, prompt_name="review-combined",
	)
	error = build_app.single_line(build_app.redact(turn.error, 100_000)) if turn.error else ""
	available, findings = code_review.parse_code_review_findings(turn.text, reproductions=reproductions)
	options = dict(review_policy=review_policy, thinking=thinking, diff_fetch_failed=diff_fetch_failed, reproductions=reproductions)

	why = missing_part(available, error)
	if not why:
		return _evidence(available, findings, error, "", **options)
	retry = ""
	timeout_minutes = retry_timeout_minutes(started)
	retry_session = None
	if not timeout_minutes:
		retry = f"under {RETRY_MIN_MINUTES} of the review's {COMBINED_REVIEW_TIMEOUT_MINUTES} minutes were left"
	else:
		# What the first turn alone amounts to is on disk before the retry
		# starts: a retry killed from outside leaves that, not nothing.
		write_combined_evidence(workspace, _evidence(available, findings, error, "", **options))
		retry_session = fresh_retry_session_dir(workspace)
		if not retry_session:
			retry = "the first turn's session folder could not be emptied"
	if retry:
		return _evidence(available, findings, error, f"not asked again ({why}): {retry}"[:400], **options)

	# Saved beside the first turn's prompt, in the one folder the host copies
	# for this step, not in the retry's own session folder.
	build_app.saved_prompts.save_prompt(workspace / SESSION_DIR, RETRY_PROMPT_NAME, prompt)
	build_app.print_progress({"stage": "agent", "event": "note", "detail": "review: asking once more"})
	second = build_app.run_review_turn(
		workspace, prompt=prompt, session_dir=retry_session,
		review_base_sha=review_base_sha, thinking=thinking, adapter=adapter, timeout_minutes=timeout_minutes,
	)
	second_error = build_app.single_line(build_app.redact(second.error, 100_000)) if second.error else ""
	available, findings = code_review.parse_code_review_findings(second.text, reproductions=reproductions)
	return _evidence(
		available, findings, second_error, f"asked once more, in a new session: {why}"[:400], **options,
	)


def write_combined_evidence(workspace: Path, review: dict) -> Path:
	return code_review.write_code_review_evidence_json(workspace, review)


def main() -> int:
	parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
	parser.add_argument("--workspace", required=True, type=Path)
	parser.add_argument("--spec", required=True, type=Path, help="The ticket's spec, given to the reviewer as context only.")
	parser.add_argument(
		"--approved-spec", type=Path, default=None,
		help="The approved spec a finding may cite, shown to the reviewer numbered as L<n>: <line>. Omitted: no line can be cited.",
	)
	parser.add_argument(
		"--reproductions", action="store_true",
		help="The build holds the reproductions a finding carries: ask for spec lines and reproductions (SC-022). Omitted: an advice review.",
	)
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
	parser.add_argument(
		"--instructions-diff", type=Path, default=None,
		help="Host file holding what the build did to the repository's instruction files; shown to the reviewer as data.",
	)
	args = parser.parse_args()
	adapter = harness_adapters.get(args.harness)
	adapter.prepare()

	review = run_combined_review(
		args.workspace.resolve(), spec_path=args.spec.resolve(),
		approved_spec_path=args.approved_spec.resolve() if args.approved_spec else None,
		review_policy=args.review_policy, review_base_sha=args.review_base_sha,
		thinking=args.thinking, adapter=adapter, instructions_diff=args.instructions_diff,
		reproductions=args.reproductions,
	)
	review_path = write_combined_evidence(args.workspace.resolve(), review)
	if review.get("retry"):
		print(f"Retry: {review['retry']}")
	print(f"Code review evidence written to {review_path}")
	print(f"Outcome: {'SUCCEEDED' if review['succeeded'] else 'DID NOT SUCCEED'} -- {review['stopped_reason']}")
	return COMBINED_EXIT_BASE if review["succeeded"] else COMBINED_EXIT_BASE + NO_VERDICT_BIT


if __name__ == "__main__":
	sys.exit(main())
