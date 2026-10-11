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

An answer that lacks one of the two parts gets one further turn (see
run_combined_review): the missing review alone, or the combined turn again
when the answer had neither. A turn that fails gets none: the host decides
from the spend meter's record whether to launch the review again.

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
import os
import shutil
import sys
import time
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

# A standalone review turn's time (build_app.run_review_turn's default): what
# a retry that asks for one review alone gets.
STANDALONE_REVIEW_TIMEOUT_MINUTES = 10

# A retry never lengthens the review: the first turn and the retry together
# get the COMBINED_REVIEW_TIMEOUT_MINUTES the one turn always had, so a run's
# own deadline (the build's -timeout-minutes plus five) is reached no sooner
# than before. A retry gets what the first turn left of it, never more than
# its own turn's timeout, and is skipped when under RETRY_MIN_MINUTES are
# left: less than that has not been enough for a review of any real diff.
RETRY_MIN_MINUTES = 5

# The first turn's session folder; the host empties its prompts folder before
# the launch and copies that folder alone afterwards
# (workflow.reviewPromptSession, evidence.RetainPrompts). Before a retry,
# everything else in it (pi's sessions, a codex or copilot home: the first
# turn's transcript and answer) is removed, and the retry runs in a new folder
# below it, saving its prompt beside the first one under its own name.
SESSION_DIR = ".pi-combined-review-session"
RETRY_SESSION_SUBDIR = "retry"

# What a retry asks for, by the part of the first answer that was missing.
RETRY_COMBINED, RETRY_CONFORMITY, RETRY_CODE_REVIEW = "combined", "conformity", "code_review"
RETRY_PROMPT_NAMES = {
	RETRY_COMBINED: "review-combined-retry",
	RETRY_CONFORMITY: "review-conformity-retry",
	RETRY_CODE_REVIEW: "review-code-retry",
}
RETRY_ASKS = {
	RETRY_COMBINED: "both reviews",
	RETRY_CONFORMITY: "the spec conformity review alone",
	RETRY_CODE_REVIEW: "the code review alone",
}

# The clock the step's budget is measured on; a name of its own so a test can
# replace it without touching time.monotonic for everything else.
clock = time.monotonic

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
	instructions_block: str = "",
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
		diff_block = diff_block.removesuffix("\n") + instructions_block
	else:
		stat, diff_text = diff
		diff_block = DIFF_INLINE.format(diff=build_app.format_diff_for_prompt(stat, diff_text, review_base_sha) + instructions_block)
	return PROMPT.format(
		diff_block=diff_block.removesuffix("\n"), spec_text=spec_text, criteria="\n".join(criteria),
		conformity_command_outcome_rule=build_app.CONFORMITY_COMMAND_OUTCOME_RULE,
		conformity_formatting_rule=build_app.CONFORMITY_FORMATTING_RULE,
		scope_rule=code_review.SCOPE_RULE, severity_rule=code_review.SEVERITY_RULE,
		command_outcome_rule=code_review.COMMAND_OUTCOME_RULE,
	).removesuffix("\n")


def conformity_unavailable(verdicts: list[dict]) -> bool:
	"""True when the answer held a verdict for no criterion at all (what
	conformity_evidence reports as an unavailable review). One real verdict
	makes the half usable, the rest staying unavailable as before."""
	return bool(verdicts) and all(v["verdict"] == "unavailable" for v in verdicts)


def missing_part(verdicts: list[dict], available: bool, error: str) -> tuple[str, str]:
	"""Returns (which retry the first answer calls for, why), ("", "") when
	both halves are usable, and when the turn failed: an error and neither
	half. A failed turn is not asked again here. Whether its model was served
	is the spend meter's record, which this script cannot read, and the host
	decides from it what happens next (it launches the review once more when
	the model served nothing)."""
	no_conformity = conformity_unavailable(verdicts)
	if no_conformity and not available:
		if error:
			return "", ""
		return RETRY_COMBINED, "the first answer had neither a verdict for any criterion nor a findings list"
	if no_conformity:
		return RETRY_CONFORMITY, "the combined answer had no verdict for any criterion"
	if not available:
		return RETRY_CODE_REVIEW, "the combined answer had no parseable findings list"
	return "", ""


def retry_timeout_minutes(part: str, started: float) -> int:
	"""The retry's timeout: its turn's own, cut to the whole minutes the first
	turn left of COMBINED_REVIEW_TIMEOUT_MINUTES; 0 when under
	RETRY_MIN_MINUTES are left."""
	own = COMBINED_REVIEW_TIMEOUT_MINUTES if part == RETRY_COMBINED else STANDALONE_REVIEW_TIMEOUT_MINUTES
	left = int((COMBINED_REVIEW_TIMEOUT_MINUTES * 60 - (clock() - started)) // 60)
	return min(own, left) if left >= RETRY_MIN_MINUTES else 0


def _remove(path: str, is_real_dir: bool) -> None:
	"""Removes path without following a link: a real directory with its
	contents, anything else by its name."""
	if is_real_dir:
		shutil.rmtree(path, ignore_errors=True)
		return
	try:
		os.unlink(path)
	except OSError:
		pass


def fresh_retry_session_dir(workspace: Path) -> Path | None:
	"""Returns the retry's session folder, new and empty, after removing what
	the first turn (or an earlier launch, or the build) left in the review's
	session folder: everything in it but the saved prompts, so the retry can
	read neither the first turn's transcript nor its answer. No link is
	followed: a session folder that is a link (or not a directory) is removed
	by name and made again, and so is each entry in it. None when something
	other than the prompts is still there afterwards; the caller then asks
	nothing."""
	session = workspace / SESSION_DIR
	try:
		if session.is_symlink() or (session.exists() and not session.is_dir()):
			session.unlink()
		session.mkdir(exist_ok=True)
		with os.scandir(session) as entries:
			for entry in entries:
				if entry.name != build_app.saved_prompts.PROMPTS_DIR:
					_remove(entry.path, entry.is_dir(follow_symlinks=False))
		left = [name for name in os.listdir(session) if name != build_app.saved_prompts.PROMPTS_DIR]
		if left or session.is_symlink():
			return None
		retry = session / RETRY_SESSION_SUBDIR
		retry.mkdir()
		return retry
	except OSError:
		return None


def _evidences(
	verdicts: list[dict], conformity_error: str, available: bool, findings: list[dict], review_error: str, *,
	conformity_policy: str, review_policy: str, thinking: str | None, diff_fetch_failed: bool,
) -> tuple[dict, dict]:
	conformity = conformity_review.conformity_evidence(
		verdicts, conformity_error, conformity_policy=conformity_policy, thinking=thinking,
	)
	review = code_review.code_review_evidence(
		available, findings, review_error, review_policy=review_policy, thinking=thinking,
		diff_fetch_failed=diff_fetch_failed,
	)
	return conformity, review


def run_combined_review(
	workspace: Path, *, criteria_path: Path, spec_path: Path,
	conformity_policy: str = "required", review_policy: str = "required",
	review_base_sha: str | None = None, thinking: str | None = None, adapter=build_app.DEFAULT_ADAPTER,
	instructions_diff: Path | None = None,
) -> tuple[dict, dict]:
	"""Runs the one combined turn and returns (conformity_evidence,
	code_review_evidence) -- both JSON-serializable evidence dicts in
	exactly conformity_review.py's/code_review.py's own existing shapes.

	When the turn answered and the answer lacks a part, that part is asked
	for once more, in one further turn at most (missing_part): the missing
	review alone, with the standalone reviewer's own prompt
	(code_review.code_review_prompt, build_app.spec_conformity_prompt), the
	other review's result kept as parsed; or the combined prompt again when
	neither part parsed. A turn that failed (an error and neither part) is
	not asked again: both reviews are unavailable with its error. Found live (2026-10-10): an answer with no parseable JSON left
	both reviews unavailable, which quarantines a run with no corrective
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

	A part still missing after the retry is unavailable exactly as a
	standalone review's unavailable turn is, recording the retry's own
	error -- parse_conformity_verdicts defaults every criterion to
	"unavailable" and parse_code_review_findings returns available=False
	when it finds no "criteria"/"findings" list at all. Both evidences then
	carry one more key, "retry": one line saying what was asked again and
	why, or why it was not. The key is absent when the first answer was whole
	and when the first turn failed; no Go reader decodes it."""
	started = clock()
	criteria = build_app.read_acceptance_criteria(criteria_path)
	spec_text = spec_path.read_text()
	diff = build_app.workspace_diff(workspace, review_base_sha) if review_base_sha else None
	diff_fetch_failed = review_base_sha is not None and diff is None
	instructions_block = build_app.instructions_diff_block(instructions_diff)
	prompt = combined_review_prompt(
		criteria, spec_text, review_base_sha, diff=diff, instructions_block=instructions_block,
	)
	turn = build_app.run_review_turn(
		workspace, prompt=prompt, session_dir=workspace / SESSION_DIR,
		review_base_sha=review_base_sha, thinking=thinking, adapter=adapter,
		timeout_minutes=COMBINED_REVIEW_TIMEOUT_MINUTES, prompt_name="review-combined",
	)
	error = build_app.single_line(build_app.redact(turn.error, 100_000)) if turn.error else ""

	conformity_verdicts = build_app.parse_conformity_verdicts(turn.text, criteria)
	available, findings = code_review.parse_code_review_findings(turn.text)

	policies = dict(
		conformity_policy=conformity_policy, review_policy=review_policy, thinking=thinking,
		diff_fetch_failed=diff_fetch_failed,
	)
	conformity, review = _evidences(conformity_verdicts, error, available, findings, error, **policies)
	part, why = missing_part(conformity_verdicts, available, error)
	if not part:
		return conformity, review

	timeout_minutes = retry_timeout_minutes(part, started)
	retry_session = None
	if not timeout_minutes:
		skipped = (
			f"under {RETRY_MIN_MINUTES} of the review's {COMBINED_REVIEW_TIMEOUT_MINUTES} minutes were left"
		)
	else:
		# What the first turn alone amounts to is on disk before the retry
		# starts: a retry killed from outside leaves that, not nothing.
		write_combined_evidence(workspace, conformity, review)
		retry_session = fresh_retry_session_dir(workspace)
		skipped = "" if retry_session else "the first turn's session folder could not be emptied"
	if skipped:
		conformity["retry"] = review["retry"] = f"not asked again ({why}): {skipped}"[:400]
		return conformity, review

	if part == RETRY_CONFORMITY:
		retry_prompt = build_app.spec_conformity_prompt(
			criteria, review_base_sha, diff=diff, instructions_block=instructions_block,
		)
	elif part == RETRY_CODE_REVIEW:
		retry_prompt = code_review.code_review_prompt(
			spec_text, review_base_sha, diff=diff, instructions_block=instructions_block,
		)
	else:
		retry_prompt = prompt
	# Saved beside the first turn's prompt, in the one folder the host copies
	# for this step, not in the retry's own session folder.
	build_app.saved_prompts.save_prompt(workspace / SESSION_DIR, RETRY_PROMPT_NAMES[part], retry_prompt)
	build_app.print_progress({"stage": "agent", "event": "note", "detail": f"review: asking once more for {RETRY_ASKS[part]}"})
	second = build_app.run_review_turn(
		workspace, prompt=retry_prompt, session_dir=retry_session,
		review_base_sha=review_base_sha, thinking=thinking, adapter=adapter,
		timeout_minutes=timeout_minutes,
	)
	second_error = build_app.single_line(build_app.redact(second.error, 100_000)) if second.error else ""
	conformity_error = review_error = error
	if part != RETRY_CODE_REVIEW:
		conformity_verdicts = build_app.parse_conformity_verdicts(second.text, criteria)
		conformity_error = second_error
	if part != RETRY_CONFORMITY:
		available, findings = code_review.parse_code_review_findings(second.text)
		review_error = second_error
	conformity, review = _evidences(conformity_verdicts, conformity_error, available, findings, review_error, **policies)
	conformity["retry"] = review["retry"] = f"asked once more for {RETRY_ASKS[part]}, in a new session: {why}"[:400]
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
	parser.add_argument(
		"--instructions-diff", type=Path, default=None,
		help="Host file holding what the build did to the repository's instruction files; shown to the reviewer as data.",
	)
	args = parser.parse_args()
	adapter = harness_adapters.get(args.harness)
	adapter.prepare()

	conformity, review = run_combined_review(
		args.workspace.resolve(), criteria_path=args.spec_acceptance_criteria,
		spec_path=args.spec.resolve(), conformity_policy=args.conformity_policy,
		review_policy=args.review_policy, review_base_sha=args.review_base_sha,
		thinking=args.thinking, adapter=adapter, instructions_diff=args.instructions_diff,
	)
	conformity_path, review_path = write_combined_evidence(args.workspace.resolve(), conformity, review)
	if conformity.get("retry"):
		print(f"Retry: {conformity['retry']}")
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
