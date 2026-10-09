#!/usr/bin/env python3
"""Standalone AI code-review pass, run as its OWN sandboxed launch,
separate from build_app.py's own round loop and from
conformity_review.py's own per-criterion spec-conformity pass -- see
conformity_review.py's own doc comment for why a one-turn review runs as
a distinct script rather than a mode flag on build_app.py (the same
reasoning applies here: build_app.py's own main() always overwrites
BUILD_EVIDENCE.json/BUILD_REPORT.md, so a review-only pass needs its own
evidence file). This script only ever reads git (a diff/diff --stat to
build the reviewer's own prompt, see build_app.workspace_diff), never writes --
.git is mounted read-only inside the sandbox unconditionally, same as
conformity_review.py.

Before the live example-app walk (2026-09-28), the reviewer was told to run
`git diff` itself and did: ~13 tool-use turns at ~77k input tokens each
against a real ticket, burning the relay's whole 1M-token/hour
sliding-window budget before ever answering, and ending with four 429s
and no findings at all. Fetching the diff here and inlining it in the
prompt (capped at 120,000 characters -- see build_app._DIFF_TRUNCATE_LIMIT) turns
that into a single read instead of an open-ended exploration.

Unlike conformity_review.py, which checks the diff against declared
acceptance criteria, this script asks an independent model turn for a
free-form code review of the diff: concrete correctness, security,
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

SCOPE_RULE = (
	prompt_templates.load_text("code_review.scope_rule")
)

SEVERITY_RULE = (
	prompt_templates.load_text("code_review.severity_rule")
)

COMMAND_OUTCOME_RULE = (
	prompt_templates.load_text("code_review.command_outcome_rule")
)

JSON_CONTRACT = (
	prompt_templates.load_text("code_review.json_contract")
)

# The four constants above are shared with combined_review.py's own
# combined_review_prompt (see build_app.py's own CONFORMITY_*
# constants' doc comment for why: review was 50% of a live example-app
# request's spend, 2026-09-28, split across two separate fresh sandboxed
# sessions over the same diff) -- named here, instead of only inline in
# code_review_prompt's own f-string below, so the combined prompt reuses
# this review's exact existing wording instead of forking a driftable
# copy.

# workspace_diff/_HARNESS_ARTIFACT_PATHSPECS/_DIFF_TRUNCATE_LIMIT used to be
# private copies here; moved to build_app.py (build_app.workspace_diff /
# build_app._HARNESS_ARTIFACT_PATHSPECS / build_app._DIFF_TRUNCATE_LIMIT) so
# conformity_review.py's own spec-conformity review can inline the same
# scoped diff instead of asking its reviewer to `git diff` for itself --
# found live, example-app run 3, 2026-09-28: without it, that review burned the
# relay's whole hourly token budget across 30 tool-exploring turns and ended
# in three straight 429s with no verdicts at all.

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
	rule as parse_conformity_verdicts' own _iter_json_objects use --
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
		parsed.append({
			"severity": severity,
			"file": file_,
			"line": line,
			"summary": summary,
			"failure_scenario": failure_scenario,
		})
	return True, parsed


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
	diff_fetch_failed: bool = False,
) -> dict:
	"""Builds the CODE_REVIEW_EVIDENCE.json shape from an already-parsed
	(available, findings) pair and the underlying turn's own error string
	-- shared by run_code_review below (a standalone, code-review-only
	turn) and combined_review.py's own run_combined_review (one turn
	whose single JSON answer covers both this review and
	conformity_review.py's own spec-conformity review), so the two
	callers can never diverge on how findings turn into a
	succeeded/stopped_reason outcome."""
	blocking = [f for f in findings if f["severity"] == "high"]

	if review_policy == "advisory":
		succeeded = True
		if not available:
			stopped_reason = _unavailable_stopped_reason(error, advisory=True, diff_fetch_failed=diff_fetch_failed)
		elif blocking:
			stopped_reason = f"code review found {len(blocking)} high-severity finding(s), recorded under advisory policy"
		else:
			stopped_reason = f"code review found no high-severity findings ({len(findings)} total)"
	elif not available:
		succeeded = False
		stopped_reason = _unavailable_stopped_reason(error, advisory=False, diff_fetch_failed=diff_fetch_failed)
	elif blocking:
		succeeded = False
		stopped_reason = f"code review found {len(blocking)} blocking high-severity finding(s)"
	else:
		succeeded = True
		stopped_reason = f"code review found no high-severity findings ({len(findings)} total)"

	return {
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
		review_base_sha=review_base_sha, thinking=thinking, adapter=adapter,
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
