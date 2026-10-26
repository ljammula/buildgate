#!/usr/bin/env python3
"""The code review's finding parser and evidence writer, shared with
combined_review.py (the review step the factory launches), plus a
standalone CLI for a code-review-only launch. A one-turn review runs as a
distinct script rather than a mode flag on build_app.py: build_app.py's own
main() always overwrites BUILD_EVIDENCE.json/BUILD_REPORT.md, so a
review-only pass needs its own evidence file. The CLI only ever reads git
(a diff/diff --stat to build the reviewer's own prompt, see
build_app.workspace_diff), never writes -- .git is mounted read-only inside
the sandbox unconditionally.

Before the live example-app walk (2026-09-28), the reviewer was told to run
`git diff` itself and did: ~13 tool-use turns at ~77k input tokens each
against a real ticket, burning the relay's whole 1M-token/hour
sliding-window budget before ever answering, and ending with four 429s
and no findings at all. Fetching the diff here and inlining it in the
prompt (capped at 120,000 characters -- see build_app._DIFF_TRUNCATE_LIMIT) turns
that into a single read instead of an open-ended exploration.

This script asks an independent model turn for a free-form code review of
the diff: concrete correctness, security,
data-loss, or concurrency defects, each with a failure scenario. It is
not a style/lint/formatting pass -- build_app.py's own round loop already
formats changed Go files, and a reviewer flagging style drift here would
just be false-positive noise on top of that.

Usage:
    python3 code_review.py --workspace /path/to/app --spec ticket.md \\
        [--review-policy required] [--review-base-sha <sha>]

Writes CODE_REVIEW_EVIDENCE.json into --workspace. Exit code 0 iff the
review succeeded under the declared --review-policy.
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

COMMAND_OUTCOME_RULE = (
	prompt_templates.load_text("code_review.command_outcome_rule")
)

JSON_CONTRACT = (
	prompt_templates.load_text("code_review.json_contract")
)

# The rule constants above are shared with combined_review.py's own prompt,
# so it reuses this review's exact wording instead of a driftable copy.

DIFF_SELF = prompt_templates.load("code_review.diff_self", ("base",))
DIFF_INLINE = prompt_templates.load("code_review.diff_inline", ("diff",))
PROMPT = prompt_templates.load(
	"code_review",
	("diff_instructions", "spec_text", "scope_rule", "severity_rule", "command_outcome_rule", "json_contract"),
)


def code_review_prompt(
	spec_text: str, review_base_sha: str | None, diff: tuple[str, str] | None = None, instructions_block: str = "",
) -> str:
	"""Builds the reviewer's prompt. diff, when given, is (stat, diff_text)
	from build_app.workspace_diff: the diff is inlined directly and the reviewer is
	told not to re-fetch it, instead of being asked to run `git diff`
	itself -- found live (example-app walk, 2026-09-28): asked to "diff the
	workspace" itself, the reviewer explored with tools for ~13 turns at
	~77k input tokens each before the relay's own sliding-window limiter
	cut it off with four straight 429s and no findings at all. diff is
	None when review_base_sha is unknown, or when the git read to build it
	failed (run_code_review's own call site) -- the prompt then falls back
	to asking the reviewer to diff for itself, exactly as before this
	fix."""
	if diff is None:
		diff_instructions = DIFF_SELF.format(base=review_base_sha or "the commit this session started from")
		diff_instructions = diff_instructions.removesuffix("\n") + instructions_block
	else:
		stat, diff_text = diff
		diff_instructions = DIFF_INLINE.format(diff=build_app.format_diff_for_prompt(stat, diff_text, review_base_sha) + instructions_block)
	return PROMPT.format(
		diff_instructions=diff_instructions.removesuffix("\n"), spec_text=spec_text, scope_rule=SCOPE_RULE,
		severity_rule=SEVERITY_RULE, command_outcome_rule=COMMAND_OUTCOME_RULE, json_contract=JSON_CONTRACT,
	).removesuffix("\n")


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
		spec_line = _parse_spec_line(entry.get("spec_line"))
		if spec_line:
			finding["spec_line"] = spec_line
		reproduction = _parse_reproduction(entry.get("reproduction"))
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
	-- shared by run_code_review below (the standalone CLI) and
	combined_review.py's own run_combined_review, so the two callers can
	never diverge on how findings turn into a succeeded/stopped_reason
	outcome. notes are the caps' notes (cap_reproductions), kept under
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


def run_code_review(
	workspace: Path, *, spec_path: Path, review_policy: str = "required",
	review_base_sha: str | None = None, thinking: str | None = None, adapter=build_app.DEFAULT_ADAPTER,
	instructions_diff: Path | None = None,
) -> dict:
	"""Runs the code review and returns a JSON-serializable evidence dict
	(the same shape write_code_review_evidence_json writes to disk), so
	callers/tests can inspect the outcome without round-tripping through
	a file."""
	spec_text = spec_path.read_text()
	diff = build_app.workspace_diff(workspace, review_base_sha) if review_base_sha else None
	diff_fetch_failed = review_base_sha is not None and diff is None
	prompt = code_review_prompt(
		spec_text, review_base_sha, diff=diff, instructions_block=build_app.instructions_diff_block(instructions_diff),
	)
	turn = build_app.run_review_turn(
		workspace, prompt=prompt, session_dir=workspace / ".pi-code-review-session",
		review_base_sha=review_base_sha, thinking=thinking, adapter=adapter, prompt_name="review-code",
	)
	error = build_app.single_line(build_app.redact(turn.error, 100_000)) if turn.error else ""
	available, findings = parse_code_review_findings(turn.text)
	return code_review_evidence(
		available, findings, error, review_policy=review_policy, thinking=thinking,
		diff_fetch_failed=diff_fetch_failed,
	)


def crashed_review_evidence(review_policy: str, thinking: str | None, exc: BaseException) -> dict:
	"""Evidence for a review that crashed before producing a result: the
	same shape run_code_review returns, unavailable, with the exception
	named in error/stopped_reason, so the gate and the run record say why
	instead of the script dying with no evidence file at all."""
	error = build_app.single_line(build_app.redact(f"{type(exc).__name__}: {exc}", 100_000))[:300]
	return {
		"schema_version": CODE_REVIEW_EVIDENCE_SCHEMA_VERSION,
		"generated": datetime.now(timezone.utc).isoformat(),
		"review_policy": review_policy,
		"thinking": thinking,
		"available": False,
		"succeeded": review_policy == "advisory",
		"stopped_reason": f"code review crashed before a result: {error}",
		"error": error,
		"findings": [],
	}


def write_code_review_evidence_json(workspace: Path, evidence: dict) -> Path:
	path = workspace / "CODE_REVIEW_EVIDENCE.json"
	path.write_text(json.dumps(evidence, indent=2) + "\n")
	return path


def main() -> int:
	parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
	parser.add_argument("--workspace", required=True, type=Path)
	parser.add_argument("--spec", required=True, type=Path, help="The ticket's approved spec, given to the reviewer as context only.")
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

	try:
		adapter.prepare()
		evidence = run_code_review(
			args.workspace.resolve(), spec_path=args.spec.resolve(),
			review_policy=args.review_policy, review_base_sha=args.review_base_sha,
			thinking=args.thinking, adapter=adapter, instructions_diff=args.instructions_diff,
		)
	except Exception as exc:  # noqa: BLE001 -- any crash must still leave evidence naming it
		evidence = crashed_review_evidence(args.review_policy, args.thinking, exc)
	evidence_path = write_code_review_evidence_json(args.workspace.resolve(), evidence)
	print(f"Evidence written to {evidence_path}")
	print(f"Outcome: {'SUCCEEDED' if evidence['succeeded'] else 'DID NOT SUCCEED'} -- {evidence['stopped_reason']}")
	return 0 if evidence["succeeded"] else 1


if __name__ == "__main__":
	sys.exit(main())
