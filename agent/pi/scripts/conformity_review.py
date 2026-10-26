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
import posixpath
import re
import subprocess
import sys
import time
from datetime import datetime, timezone
from pathlib import Path
from typing import Callable

sys.path.insert(0, str(Path(__file__).resolve().parent))
import build_app  # noqa: E402
import harness_adapters  # noqa: E402
import prompt_templates  # noqa: E402

CONFORMITY_EVIDENCE_SCHEMA_VERSION = 1

# A flag that names text a file lacks ("missing" in the JSON contract) is
# checked against the result commit. When the commit holds that text, the
# flagged criteria are judged once more by a fresh turn shown where it is
# (run_recheck). Found on two walks (2026-10-11): every conformity flag was
# false, two of them quarantined correct builds, and two were refutable from
# the files alone (a doc comment said to lack "RFC 7386" held it; a changelog
# link said to be missing was already in the file).
RECHECK = prompt_templates.load("spec_conformity.recheck", ("facts",))
RECHECK_PROMPT_NAME = "review-conformity-recheck"
RECHECK_SESSION_SUBDIR = "recheck"
# build_app.run_spec_conformity_review's session folder: the host copies its
# prompts folder after the launch.
SESSION_DIR = ".pi-conformity-session"
MAX_RESULT_FILE_BYTES = 5_000_000
MAX_QUOTED_LINE_CHARS = 160
MAX_FOUND_LINES = 5
# Text this short matches by chance; text cut to its cap may match a prefix of
# what the criterion requires.
MIN_MISSING_TEXT_CHARS = 4
# The standalone review's whole time: its first turn's 10 minutes
# (build_app.run_review_turn's default) and what a recheck may use of the
# rest, the same 20 minutes combined_review.py's one turn and its retry share.
REVIEW_BUDGET_MINUTES = 20
STANDALONE_TURN_MINUTES = 10
RECHECK_MIN_MINUTES = 5
clock = time.monotonic

_LEADING_NUMBER = re.compile(r"^\s*([0-9]+)[.)]\s")


def result_path(workspace: Path, file: str) -> str | None:
	"""file as a path inside the repository (no leading ./, one pair of
	surrounding backticks dropped, an absolute path under workspace made
	relative), or None when it is outside the workspace, climbs out with ..,
	or holds a backslash, a NUL or a line break."""
	candidate = _unquote(file.strip())
	root = str(workspace).rstrip("/") + "/"
	if candidate.startswith(root):
		candidate = candidate[len(root):]
	if not candidate or any(c in candidate for c in "\x00\n\r\\") or candidate.startswith("/"):
		return None
	normal = posixpath.normpath(candidate)
	if normal in (".", "..") or normal.startswith("../"):
		return None
	return normal


def _unquote(text: str) -> str:
	if len(text) >= 2 and text[0] == text[-1] == "`":
		return text[1:-1]
	return text


def _git(workspace: Path, *args: str) -> bytes | None:
	try:
		completed = subprocess.run(["git", *args], cwd=workspace, capture_output=True, timeout=30, check=False)
	except (subprocess.TimeoutExpired, OSError):
		return None
	return completed.stdout if completed.returncode == 0 else None


def result_file(workspace: Path, path: str) -> str | None:
	"""The result commit's copy of path as text, read from git objects; None
	when the commit holds no file there, when it is over
	MAX_RESULT_FILE_BYTES, or when the worktree's copy is not the same bytes.
	The last covers the instruction files a reviewer sees masked by the base
	commit's version (SC-019), whose result text must not reach a review,
	and any file a review turn rewrote: the recheck reads the worktree, so a
	finding is shown only where the worktree is the result."""
	size = _git(workspace, "cat-file", "-s", f"HEAD:{path}")
	if size is None or not size.strip().isdigit() or int(size) > MAX_RESULT_FILE_BYTES:
		return None
	blob = _git(workspace, "cat-file", "blob", f"HEAD:{path}")
	worktree = workspace / path
	try:
		if blob is None or worktree.is_symlink() or worktree.read_bytes() != blob:
			return None
	except OSError:
		return None
	return blob.decode("utf-8", errors="replace")


def found_in_result(workspace: Path, missing: dict) -> list[int]:
	"""The lines (1-based, at most MAX_FOUND_LINES) at which the result
	commit's copy of missing["file"] (result_file) holds missing["text"]
	exactly; [] when it does not, when the file cannot be read that way, or
	when the text is under MIN_MISSING_TEXT_CHARS or was cut to its cap."""
	path = result_path(workspace, missing["file"])
	text = _unquote(missing["text"].strip())
	if path is None or len(text.strip()) < MIN_MISSING_TEXT_CHARS or len(missing["text"]) >= build_app.MAX_MISSING_TEXT_CHARS:
		return []
	content = result_file(workspace, path)
	if content is None:
		return []
	lines: list[int] = []
	start = 0
	while len(lines) < MAX_FOUND_LINES:
		at = content.find(text, start)
		if at < 0:
			break
		lines.append(content.count("\n", 0, at) + 1)
		start = at + len(text)
	return lines


def _quoted_lines(content: str, numbers: list[int]) -> list[str]:
	"""Each found line with the line before and after it, numbered, one line
	each, cut and quoted as data; a line two hits share is quoted once."""
	lines = content.split("\n")
	shown: list[int] = []
	for number in numbers:
		for n in (number - 1, number, number + 1):
			if 1 <= n <= len(lines) and n not in shown:
				shown.append(n)
	return [
		f"line {n}: {json.dumps(build_app.single_line(lines[n - 1])[:MAX_QUOTED_LINE_CHARS], ensure_ascii=False)}"
		for n in shown
	]


def contradicted_facts(workspace: Path, verdicts: list[dict]) -> dict[int, str]:
	"""For each flagged verdict whose "missing" text the result commit holds,
	keyed by its index: what the factory read, the file's path and the found
	lines with their neighbours, quoted from the commit. Nothing the
	reviewer wrote is in it but the path, which names a file of the commit."""
	facts: dict[int, str] = {}
	for index, verdict in enumerate(verdicts):
		missing = verdict.get("missing") if verdict.get("verdict") == "flagged" else None
		if not missing:
			continue
		numbers = found_in_result(workspace, missing)
		if not numbers:
			continue
		path = result_path(workspace, missing["file"]) or ""
		content = result_file(workspace, path) or ""
		quoted = "\n    ".join(_quoted_lines(content, numbers))
		facts[index] = f"{json.dumps(build_app.single_line(path), ensure_ascii=False)}:\n    {quoted}"
	return facts


def criterion_label(criterion: str) -> str:
	"""The criterion's leading number, or its first 40 characters."""
	match = _LEADING_NUMBER.match(criterion)
	return match.group(1) if match else build_app.single_line(criterion)[:40]


def recheck_prompt(
	criteria: list[str], facts: list[str], review_base_sha: str | None, diff: tuple[str, str] | None,
	instructions_block: str, answers: str = "",
) -> str:
	"""The standalone conformity prompt over the same diff, for the flagged
	criteria alone, with the factory's findings after the diff and the
	read-back answers (build_app.read_spec_answers) after the criteria."""
	lines = "\n".join(f"- Criterion: {criterion}\n  The factory read {fact}" for criterion, fact in zip(criteria, facts))
	block = RECHECK.format(facts=lines).removesuffix("\n")
	return build_app.spec_conformity_prompt(
		criteria, review_base_sha, diff=diff, instructions_block=f"{instructions_block}\n\n{block}", answers=answers,
	)


def apply_recheck(verdicts: list[dict], facts: dict[int, str], answer: str | None, error: str = "") -> list[dict]:
	"""verdicts with each rechecked criterion's verdict replaced by the
	recheck's, and a "recheck" record on it: the factory's finding, the
	earlier detail and "missing", and the outcome. A criterion the recheck
	did not answer (answer None when it was never asked) keeps its earlier
	flag, and the outcome names the recheck turn's error when it had one.
	Nothing the recheck says is checked again."""
	indexes = sorted(facts)
	second = build_app.parse_conformity_verdicts(answer, [verdicts[i]["criterion"] for i in indexes]) if answer is not None else []
	result = [dict(v) for v in verdicts]
	for position, index in enumerate(indexes):
		first = verdicts[index]
		record = {"fact": facts[index], "first_detail": first["detail"], "first_missing": first["missing"]}
		if answer is None:
			result[index] = {**first, "recheck": {**record, "outcome": "not asked: the earlier flag stands"}}
		elif second[position]["verdict"] == "unavailable":
			why = f" ({build_app.single_line(build_app.redact(error, 100_000))[:200]})" if error else ""
			result[index] = {**first, "recheck": {**record, "outcome": f"not answered{why}: the earlier flag stands"}}
		else:
			result[index] = {**second[position], "recheck": {**record, "outcome": f"answered {second[position]['verdict']}"}}
	return result


def recheck_timeout_minutes(started: float, own: int = STANDALONE_TURN_MINUTES, budget: int = REVIEW_BUDGET_MINUTES) -> int:
	"""A recheck's timeout: its own turn's, cut to the whole minutes left of
	budget since started; 0 when under RECHECK_MIN_MINUTES are left."""
	left = int((budget * 60 - (clock() - started)) // 60)
	return min(own, left) if left >= RECHECK_MIN_MINUTES else 0


def run_recheck(
	workspace: Path, verdicts: list[dict], *, review_base_sha: str | None, diff: tuple[str, str] | None,
	instructions_block: str, thinking: str | None, adapter, timeout_minutes: int,
	prompt_session: Path, fresh_session: Callable[[], Path | None], conformity_policy: str = "required",
	answers: str = "",
) -> tuple[list[dict], str]:
	"""Checks every flag's "missing" against the result commit and, when the
	commit holds the text, asks one fresh turn about those criteria alone
	(recheck_prompt), shown the lines of the commit that hold the text and
	nothing the earlier answer said. Returns (verdicts, note): note is one line for the evidence's
	"recheck" key saying what was asked again and why, or why it was not;
	"" when no flag was contradicted, and then nothing is asked.
	prompt_session is the folder the host copies prompts from; fresh_session
	makes the turn's own empty folder (None when it cannot). Under the
	advisory policy a flag fails nothing, so nothing is asked."""
	if conformity_policy != "required":
		return verdicts, ""
	facts = contradicted_facts(workspace, verdicts)
	if not facts:
		return verdicts, ""
	numbers = ", ".join(criterion_label(verdicts[i]["criterion"]) for i in sorted(facts))
	why = f"the result commit holds the text an answer called missing (criteria {numbers})"
	session = fresh_session() if timeout_minutes else None
	if session is None:
		skipped = (
			f"under {RECHECK_MIN_MINUTES} minutes of the review's time were left" if not timeout_minutes
			else "the review's session folder could not be emptied"
		)
		return apply_recheck(verdicts, facts, None), f"not asked again ({why}): {skipped}"[:400]
	indexes = sorted(facts)
	prompt = recheck_prompt(
		[verdicts[i]["criterion"] for i in indexes], [facts[i] for i in indexes],
		review_base_sha, diff, instructions_block, answers,
	)
	build_app.saved_prompts.save_prompt(prompt_session, RECHECK_PROMPT_NAME, prompt)
	build_app.print_progress({"stage": "agent", "event": "note", "detail": f"review: asking once more about criteria {numbers}"})
	turn = build_app.run_review_turn(
		workspace, prompt=prompt, session_dir=session, review_base_sha=review_base_sha,
		thinking=thinking, adapter=adapter, timeout_minutes=timeout_minutes,
	)
	return apply_recheck(verdicts, facts, turn.text, turn.error), f"asked once more about criteria {numbers}, in a new session: {why}"[:400]


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
	instructions_diff: Path | None = None,
) -> dict:
	"""Runs the conformity review and returns a JSON-serializable evidence
	dict (the same shape write_conformity_evidence_json writes to disk),
	so callers/tests can inspect the outcome without round-tripping
	through a file."""
	started = clock()
	criteria = build_app.read_acceptance_criteria(criteria_path)
	answers = build_app.read_spec_answers(criteria_path)
	diff = build_app.workspace_diff(workspace, review_base_sha) if review_base_sha else None
	instructions_block = build_app.instructions_diff_block(instructions_diff)
	verdicts, turn_error = build_app.run_spec_conformity_turn(
		workspace, criteria=criteria, review_base_sha=review_base_sha, diff=diff,
		instructions_block=instructions_block, thinking=thinking,
		timeout_minutes=STANDALONE_TURN_MINUTES, adapter=adapter, answers=answers,
	)
	error = build_app.single_line(build_app.redact(turn_error, 100_000)) if turn_error else ""
	session = workspace / SESSION_DIR
	verdicts, note = run_recheck(
		workspace, verdicts, review_base_sha=review_base_sha, diff=diff, instructions_block=instructions_block,
		thinking=thinking, adapter=adapter, timeout_minutes=recheck_timeout_minutes(started),
		prompt_session=session, fresh_session=lambda: build_app.fresh_session_subdir(session, RECHECK_SESSION_SUBDIR),
		conformity_policy=conformity_policy, answers=answers,
	)
	evidence = conformity_evidence(verdicts, error, conformity_policy=conformity_policy, thinking=thinking)
	if note:
		evidence["recheck"] = note
	return evidence


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
	parser.add_argument(
		"--instructions-diff", type=Path, default=None,
		help="Host file holding what the build did to the repository's instruction files; shown to the reviewer as data.",
	)
	args = parser.parse_args()
	adapter = harness_adapters.get(args.harness)
	adapter.prepare()

	evidence = run_conformity_review(
		args.workspace.resolve(), criteria_path=args.spec_acceptance_criteria,
		conformity_policy=args.conformity_policy, review_base_sha=args.review_base_sha,
		thinking=args.thinking, adapter=adapter, instructions_diff=args.instructions_diff,
	)
	evidence_path = write_conformity_evidence_json(args.workspace.resolve(), evidence)
	if evidence.get("recheck"):
		print(f"Recheck: {evidence['recheck']}")
	print(f"Evidence written to {evidence_path}")
	print(f"Outcome: {'SUCCEEDED' if evidence['succeeded'] else 'DID NOT SUCCEED'} -- {evidence['stopped_reason']}")
	return 0 if evidence["succeeded"] else 1


if __name__ == "__main__":
	sys.exit(main())
