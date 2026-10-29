"""The code review's shared helpers, imported by combined_review.py (the
only review step the factory launches): the finding parser
(parse_code_review_findings), the advice cap (cap_advice), the rule texts the
review prompt embeds (ADVICE_SCOPE_RULE, ADVICE_SEVERITY_RULE, COMMAND_OUTCOME_RULE),
and the CODE_REVIEW_EVIDENCE.json builder and writer (code_review_evidence,
write_code_review_evidence_json). It has no command line and runs no model
turn.
"""
from __future__ import annotations

import json
import sys
from datetime import datetime, timezone
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import build_app  # noqa: E402
import prompt_templates  # noqa: E402

CODE_REVIEW_EVIDENCE_SCHEMA_VERSION = 1

_VALID_SEVERITIES = ("high", "medium", "low")

ADVICE_SCOPE_RULE = prompt_templates.load_text("code_review.advice_scope_rule")

ADVICE_SEVERITY_RULE = prompt_templates.load_text("code_review.advice_severity_rule")

# Every finding is advice, and the review reports at most this many (the advice scope rule says so; cap_advice enforces it).
MAX_ADVICE_FINDINGS = 5

COMMAND_OUTCOME_RULE = (
	prompt_templates.load_text("code_review.command_outcome_rule")
)

# The rule constants above are shared with combined_review.py's prompt, so it
# reuses one wording instead of a driftable copy.


def parse_code_review_findings(output: str) -> tuple[bool, list[dict]]:
	"""Extracts the reviewer's findings from its final response text.
	Returns (available, findings): available is False when no candidate
	JSON object with a "findings" list was found at all (an unreachable
	or unparseable reviewer), never conflated with a real, empty findings
	list (a clean review). The LAST such candidate wins wholesale, same
	rule as every _iter_json_objects use --
	only the model's actual final answer is ever trusted, not an earlier
	echoed example.

	Each entry must be a dict with a non-empty string "summary" or it is
	dropped entirely (a finding with nothing to say is not a finding).
	"file" defaults to "" when missing/not a string. "line" defaults to 0
	when missing, not an int, or a bool (bool is technically an int
	subclass in Python, but a reviewer emitting `true`/`false` for a line
	number is malformed input, not a line 1/0). "failure_scenario"
	defaults to "" when missing/not a string. "severity" is lowercased;
	anything missing or not one of high/medium/low defaults to "medium"
	-- a reviewer that flags something but garbles its own severity label
	should never have that finding silently disappear."""
	candidate_findings = None
	for candidate in build_app._iter_json_objects(output):
		if not isinstance(candidate, dict):
			continue
		findings = candidate.get("findings")
		if isinstance(findings, list):
			candidate_findings = findings
	if candidate_findings is None:
		return False, []

	parsed: list[dict] = []
	for entry in candidate_findings:
		if not isinstance(entry, dict):
			continue
		summary = entry.get("summary")
		if not isinstance(summary, str) or not summary.strip():
			continue
		file_ = entry.get("file")
		if not isinstance(file_, str):
			file_ = ""
		line = entry.get("line")
		if not isinstance(line, int) or isinstance(line, bool) or line < 0:
			line = 0
		failure_scenario = entry.get("failure_scenario")
		if not isinstance(failure_scenario, str):
			failure_scenario = ""
		severity = entry.get("severity")
		severity = severity.lower() if isinstance(severity, str) else ""
		if severity not in _VALID_SEVERITIES:
			severity = "medium"
		finding = {
			"severity": severity,
			"file": file_,
			"line": line,
			"summary": summary,
			"failure_scenario": failure_scenario,
		}
		parsed.append(finding)
	return True, parsed


def cap_advice(findings: list[dict]) -> list[str]:
	"""Keeps the first MAX_ADVICE_FINDINGS findings, in place, and returns one
	note saying how many were dropped (none when none were). A cap never fails
	a review."""
	dropped = len(findings) - MAX_ADVICE_FINDINGS
	if dropped <= 0:
		return []
	del findings[MAX_ADVICE_FINDINGS:]
	return [f"{dropped} further finding(s) dropped, an advice review carries at most {MAX_ADVICE_FINDINGS}"]


def _unavailable_stopped_reason(error: str, *, advisory: bool, diff_fetch_failed: bool = False) -> str:
	"""Builds the stopped_reason for an unavailable review turn. When the
	turn's own model call errored out (error non-empty -- e.g. the
	relay's sliding-window limiter answering "429 token budget exceeded"),
	names that cause instead of the generic "no parseable response",
	which was the whole story an operator saw on the live example-app walk,
	2026-09-28: the review ran out of relay budget, Pi ended with no
	final text, and CODE_REVIEW_EVIDENCE.json only ever said the response
	was unparseable -- the actual 429 was invisible. Single line, capped
	300 chars (error text is untrusted pi-stderr/relay content, same
	redaction/bounding build_app.exited_zero_hint already applies to it
	before it lands in operator-facing evidence). diff_fetch_failed notes,
	only when the review is unavailable, that the reviewer got the
	pre-fix instruction-only prompt because build_app.workspace_diff's own `git
	diff` failed -- so an operator can tell that case apart from a
	genuinely unreachable model."""
	if error:
		reason = f"code review was unavailable: {error}"
	else:
		reason = "code review was unavailable"
		if not advisory:
			reason += " (no parseable response from the reviewer)"
	if diff_fetch_failed:
		reason += "; git diff for the reviewer's prompt failed, fell back to the instruction-only prompt"
	if advisory:
		reason += "; recorded under advisory policy"
	return reason[:300]


def code_review_evidence(
	available: bool, findings: list[dict], error: str, *, review_policy: str, thinking: str | None,
	diff_fetch_failed: bool = False, notes: list[str] | None = None,
) -> dict:
	"""Builds the CODE_REVIEW_EVIDENCE.json shape from an already-parsed
	(available, findings) pair and the underlying turn's own error string
	used by combined_review.py's run_combined_review, so the outcome
	of findings (succeeded, stopped_reason) has one home. notes are the review's own notes, kept under
	"notes" only when there are any."""
	# A finding never fails the review (SC-022: a finding is advice); a review
	# fails only by giving no verdict.
	high = [f for f in findings if f["severity"] == "high"]
	if not available:
		succeeded = review_policy == "advisory"
		stopped_reason = _unavailable_stopped_reason(
			error, advisory=review_policy == "advisory", diff_fetch_failed=diff_fetch_failed,
		)
	else:
		succeeded = True
		stopped_reason = f"code review gave a verdict: {len(findings)} finding(s), {len(high)} high-severity"
		if review_policy == "advisory":
			stopped_reason += ", recorded under advisory policy"

	evidence = {
		"schema_version": CODE_REVIEW_EVIDENCE_SCHEMA_VERSION,
		"generated": datetime.now(timezone.utc).isoformat(),
		"review_policy": review_policy,
		"thinking": thinking,
		"available": available,
		"succeeded": succeeded,
		"stopped_reason": stopped_reason,
		"error": error,
		"findings": findings,
	}
	if notes:
		evidence["notes"] = notes
	return evidence


def write_code_review_evidence_json(workspace: Path, evidence: dict) -> Path:
	path = workspace / "CODE_REVIEW_EVIDENCE.json"
	path.write_text(json.dumps(evidence, indent=2) + "\n")
	return path
