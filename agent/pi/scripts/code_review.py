"""The code review's shared helpers, imported by combined_review.py (the
only review step the factory launches): the finding parser
(parse_code_review_findings, with each finding's spec_line and
reproduction), the reproduction caps (MAX_REPRODUCTIONS,
MAX_REPRODUCTION_BYTES, cap_reproductions), the rule texts the review prompt
embeds (SCOPE_RULE, SEVERITY_RULE, REPRODUCTION_RULE, COMMAND_OUTCOME_RULE, and the
advice path's ADVICE_SCOPE_RULE, ADVICE_SEVERITY_RULE),
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

# A review carries at most MAX_REPRODUCTIONS reproductions of at most
# MAX_REPRODUCTION_BYTES each (SC-022); internal/codereview applies the same
# caps (maxReproductions, maxReproductionBytes) to the evidence file it reads.
MAX_REPRODUCTIONS = 3
MAX_REPRODUCTION_BYTES = 32 << 10

SCOPE_RULE = (
	prompt_templates.load_text("code_review.scope_rule")
)

SEVERITY_RULE = (
	prompt_templates.load_text("code_review.severity_rule")
)

REPRODUCTION_RULE = (
	prompt_templates.load_text("code_review.reproduction_rule")
)

ADVICE_SCOPE_RULE = prompt_templates.load_text("code_review.advice_scope_rule")

ADVICE_SEVERITY_RULE = prompt_templates.load_text("code_review.advice_severity_rule")

# Without reproductions every finding is advice, and the review reports at
# most this many (the advice scope rule says so; cap_advice enforces it).
MAX_ADVICE_FINDINGS = 5

COMMAND_OUTCOME_RULE = (
	prompt_templates.load_text("code_review.command_outcome_rule")
)

# The rule constants above are shared with combined_review.py's prompt, so it
# reuses one wording instead of a driftable copy.


def parse_code_review_findings(output: str, *, reproductions: bool = True) -> tuple[bool, list[dict]]:
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
	should never have that finding silently disappear. reproductions False
	(a review of a build that holds none) drops every spec_line and
	reproduction: the answer format never asks for them, and a model that
	sends them anyway cannot make a finding block."""
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
		spec_line = _parse_spec_line(entry.get("spec_line")) if reproductions else None
		if spec_line:
			finding["spec_line"] = spec_line
		reproduction = _parse_reproduction(entry.get("reproduction")) if reproductions else None
		if reproduction:
			finding["reproduction"] = reproduction
		parsed.append(finding)
	return True, parsed


def _parse_spec_line(value) -> dict | None:
	"""The {"number", "text"} a finding cites, or None unless number is an
	int of at least 1 (not a bool) and text a non-empty string. Whether the
	text is the approved spec's line is the host's check (SC-022), not this
	one."""
	if not isinstance(value, dict):
		return None
	number, text = value.get("number"), value.get("text")
	if not isinstance(number, int) or isinstance(number, bool) or number < 1:
		return None
	if not isinstance(text, str) or not text.strip():
		return None
	return {"number": number, "text": text}


def _parse_reproduction(value) -> dict | None:
	"""The {"path", "content"} a finding carries, or None unless both are
	non-empty strings. Its shape (a new test path, a size) is the host's
	check (SC-022); the caps on count and size are cap_reproductions'."""
	if not isinstance(value, dict):
		return None
	path, content = value.get("path"), value.get("content")
	if not isinstance(path, str) or not path.strip() or not isinstance(content, str) or not content:
		return None
	return {"path": path.strip(), "content": content}


def cap_reproductions(findings: list[dict]) -> list[str]:
	"""Drops the reproduction of every finding past the first
	MAX_REPRODUCTIONS, and of any over MAX_REPRODUCTION_BYTES, in place; the
	finding stays, as advice. Returns one note per drop. A cap never fails a
	review."""
	notes: list[str] = []
	kept = 0
	for index, finding in enumerate(findings, start=1):
		reproduction = finding.get("reproduction")
		if not reproduction:
			continue
		size = len(reproduction["content"].encode("utf-8"))
		if size > MAX_REPRODUCTION_BYTES:
			notes.append(f"finding {index}: reproduction dropped, {size} bytes is over the {MAX_REPRODUCTION_BYTES} byte cap")
		elif kept >= MAX_REPRODUCTIONS:
			notes.append(f"finding {index}: reproduction dropped, a review carries at most {MAX_REPRODUCTIONS}")
		else:
			kept += 1
			continue
		del finding["reproduction"]
	return notes


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
	of findings (succeeded, stopped_reason) has one home. notes are the caps' notes (cap_reproductions), kept under
	"notes" only when there are any."""
	# A finding never fails the review (SC-022: it blocks only through a
	# reproduction the factory runs); a review fails only by giving no verdict.
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
