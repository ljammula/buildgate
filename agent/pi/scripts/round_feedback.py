"""What a failed build round hands the next one.

The rule this module serves: no round ends in failure without the next round
being told what failed and why, whatever kind of failure it was. A round
that continues the same harness session still needs this: the harness trims
a long session, and the failing command ran after the agent's turn, outside
it.

Everything here is a pure function over text and round records; build_app.py
does the I/O (running commands, writing the log file, reading git).
"""

from __future__ import annotations

import hashlib
import re

# A line that says something failed, in the output of the common build and
# test tools (go test, pytest, unittest, jest/vitest, cargo, make, tsc, gcc).
# Deliberately broad: it picks where an excerpt starts and what a signature
# is made of, never a verdict.
FAILURE_MARKERS = re.compile(
	r"(--- FAIL|\bFAIL(?:ED|URE|URES)?\b|\b[Ee]rror\b|\bERROR\b|\bpanic:|Traceback \(most recent call last\)"
	r"|\bAssertionError\b|\bassert(?:ion)? |\bexpected\b|\bwant\b|\bundefined\b|\bcannot \b|No such file"
	r"|\bnot found\b|\bfatal\b|\*\*\* |✗|✘|✕)"
)

# The name of a failing test or target, in the same tools' output.
_FAILING_NAME_PATTERNS = (
	re.compile(r"^\s*--- FAIL: (\S+)"),                      # go test
	re.compile(r"^FAIL\s+(\S+)\s"),                           # go test, package line
	re.compile(r"^FAILED (\S+)"),                             # pytest summary
	re.compile(r"^(?:FAIL|ERROR): (\S+(?: \(\S+\))?)"),      # unittest
	re.compile(r"^\s*(?:✕|✗|✘|×)\s+(.+?)(?:\s+\(\d+ ?m?s\))?$"),  # jest, vitest
	re.compile(r"^test (\S+) \.\.\. FAILED"),                # cargo test
	re.compile(r"^make(?:\[\d+\])?: \*\*\* \[(?:\S+:\d+: )?(\S+?)\]"),  # make target
)

EXCERPT_LIMIT = 6000
MAX_FAILING_NAMES = 20
_CONTEXT_LINES_BEFORE = 5


def failure_excerpt(output: str, limit: int = EXCERPT_LIMIT) -> str:
	"""The part of a failing command's output worth putting in a prompt: the
	whole text when it fits, else the block that starts at the first line
	that reports a failure plus the end of the output. The end alone, which
	is what a plain tail gives, is usually the tool's summary and the make
	error line; the assertion that failed is further up."""
	text = output.strip()
	if len(text) <= limit:
		return text
	lines = text.splitlines()
	first = next((i for i, line in enumerate(lines) if FAILURE_MARKERS.search(line)), None)
	if first is None:
		return "[... earlier output omitted ...]\n" + text[-(limit - 40):]
	start = max(0, first - _CONTEXT_LINES_BEFORE)
	from_first = "\n".join(lines[start:])
	omitted = f"[... {start} earlier line(s) omitted ...]\n" if start else ""
	if len(from_first) + len(omitted) <= limit:
		return omitted + from_first
	tail_budget = limit // 3
	head_budget = limit - tail_budget - len(omitted) - 80
	return (
		omitted + from_first[:head_budget].rstrip()
		+ "\n[... middle omitted; the log file named below has all of it ...]\n"
		+ text[-tail_budget:].lstrip()
	)


def failing_names(output: str) -> list[str]:
	"""The failing tests or targets the output names, in first-seen order,
	at most MAX_FAILING_NAMES. Empty when none is recognised."""
	seen: list[str] = []
	for line in output.splitlines():
		for pattern in _FAILING_NAME_PATTERNS:
			match = pattern.search(line)
			if match and match.group(1) not in seen:
				seen.append(match.group(1))
				break
		if len(seen) >= MAX_FAILING_NAMES:
			break
	return seen


_VOLATILE = re.compile(r"0x[0-9a-fA-F]+|\b\d+(?:\.\d+)?(?:ns|µs|us|ms|s|m|h)?\b")


def failure_signature(blockers: list[str], output: str = "") -> str:
	"""A short id for "this failure": the blockers plus the lines of the
	output that report a failure, with timings, counts and addresses
	blanked. Two rounds with the same signature failed the same way. Empty
	when there is nothing to identify (no blockers)."""
	if not blockers:
		return ""
	lines = [line.strip() for line in output.splitlines() if FAILURE_MARKERS.search(line)][:12]
	normalised = [_VOLATILE.sub("N", line) for line in lines]
	blob = "\n".join(sorted(blockers) + normalised)
	return hashlib.sha256(blob.encode("utf-8", "replace")).hexdigest()[:16]


def same_failure_streak(signatures: list[str]) -> int:
	"""How many rounds in a row, ending with the last, share its signature:
	1 for a failure not seen in the round before, 0 with no signature."""
	if not signatures or not signatures[-1]:
		return 0
	streak = 0
	for signature in reversed(signatures):
		if signature != signatures[-1]:
			break
		streak += 1
	return streak


def history_lines(rounds: list[dict]) -> str:
	"""One line per round so far: what it changed and how it ended. Each
	item has index, changed_files, blockers and signature. A round whose
	signature equals the previous one's is marked, so the reader sees a
	repeated failure without comparing two excerpts."""
	out = []
	previous = ""
	for rnd in rounds:
		files = rnd.get("changed_files") or []
		shown = ", ".join(files[:8]) + (f" and {len(files) - 8} more" if len(files) > 8 else "")
		changed = f"changed {shown}" if files else "changed no files"
		blockers = rnd.get("blockers") or []
		ended = "; ".join(blockers) if blockers else "passed"
		signature = rnd.get("signature") or ""
		repeat = " (the same failure as the round before)" if signature and signature == previous else ""
		out.append(f"- Round {rnd.get('index')}: {changed}; {ended}{repeat}")
		previous = signature
	return "\n".join(out)


def agent_notes(
	*,
	no_changes: bool,
	timed_out: bool,
	timeout_minutes: int,
	returncode: int,
	stderr_tail: str,
	final_text: str,
	route_errors: list[str],
	stalled: bool,
) -> str:
	"""What happened to the agent process itself, for the failures that are
	not a failing command: it changed nothing, ran out of time, stalled,
	exited with an error, or its model route failed. Empty when none of
	those happened."""
	notes = []
	if timed_out:
		notes.append(
			f"Your previous turn was stopped after {timeout_minutes} minutes without finishing. "
			"Work in smaller steps and finish the turn: the harness runs the checks only after your turn ends."
		)
	elif returncode != 0:
		detail = f" Its error output ended with:\n\n{stderr_tail}" if stderr_tail.strip() else ""
		notes.append(f"The agent process exited with status {returncode} before finishing its turn.{detail}")
	if stalled:
		notes.append("Your previous turn stalled: it produced no output for longer than the stall limit and was stopped.")
	if route_errors:
		notes.append("The model route returned errors during your previous turn:\n\n" + "\n".join(f"- {e}" for e in route_errors[:5]))
	if no_changes:
		said = f" Your final message was:\n\n{final_text}" if final_text.strip() else ""
		notes.append(
			"Your previous turn ended without changing any file in the workspace, so there was nothing new to check."
			f"{said}\n\nIf you believe the work is already done, say exactly which existing files satisfy the task; "
			"otherwise make the change."
		)
	return "\n\n".join(notes)
