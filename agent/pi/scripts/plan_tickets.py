#!/usr/bin/env python3
"""Decompose one approved spec into one or more tickets, via a single
headless `pi` invocation.

Companion to draft_spec.py (same shape) and build_app.py (whose
pi_invocation/committed_agents_md_blob/parse_usage helpers this reuses): a single
one-shot pass, no corrective rounds, no independent review. The factory's
own request driver (cmd/factoryd/request_driver.go) is what validates the
result (internal/request.ValidateTicketPlan/ValidatePlanCoverage,
internal/ticketspec, internal/policy.TicketStructureBrownfield) and
decides whether to advance the request or halt it -- "agent output is
evidence, never the oracle", same as draft_spec.py.

Usage:
    python3 plan_tickets.py --spec spec.md --request request.md \
        --workspace /path/to/repo --verify-command "make verify" \
        --out-dir tickets --evidence evidence.json [--timeout-minutes 15] \
        [--feedback-file plan-feedback.md]

The model is instructed to write each ticket to a fixed scratch directory
inside --workspace (see DRAFT_RELATIVE_DIR below), not asked to print
tickets as its chat response -- same reasoning as draft_spec.py's own
DRAFT_RELATIVE_PATH. This script then reads every *.spec.md file back
from that directory, copies each verbatim into --out-dir under its own
filename, and writes --evidence -- both may be, and in the sandboxed case
must be, paths inside --workspace too (see cmd/factoryd/plan_tickets_job.go's
own doc comment for the container-path convention this relies on).

Exit code 0 only when at least one ticket file was produced and copied
into --out-dir. Exit code 2 for a model/route failure (pi itself failed,
timed out, or never wrote a ticket) -- --evidence is still written in
that case, with an empty --out-dir. Any other failure (a bad --spec path,
for instance) raises and exits non-zero via the ordinary Python
traceback, the same convention draft_spec.py uses for its own
argument/setup errors.
"""

from __future__ import annotations

import argparse
import json
import subprocess
import sys
import time
from datetime import datetime, timezone
from pathlib import Path

# build_app.py/draft_spec.py live alongside this script in the same
# embedded harness (internal/harness.Ensure extracts every
# agent/pi/scripts/*.py file together into one directory) -- importing
# them by relative path, not as an installed package, mirrors how every
# script here is invoked: as bare `python3 <path>/plan_tickets.py`, never
# `python3 -m`.
sys.path.insert(0, str(Path(__file__).resolve().parent))
import build_app  # noqa: E402
import harness_adapters  # noqa: E402
import draft_spec  # noqa: E402
import prompt_templates  # noqa: E402

# DRAFT_RELATIVE_DIR is where the model is instructed to write its ticket
# drafts, relative to --workspace (pi's own cwd for the invocation below).
# A directory distinct from draft_spec.py's own .factory-spec-draft/, so a
# request whose planning pass runs against the same workspace a spec draft
# once did never collides with that leftover scratch content.
DRAFT_RELATIVE_DIR = Path(".factory-plan-draft") / "TICKETS_DRAFT"

# EVIDENCE_SCHEMA_VERSION's literal value (1) must always equal
# cmd/factoryd/plan_tickets_job.go's planTicketsEvidenceSchemaVersion
# constant -- see draft_spec.py's own EVIDENCE_SCHEMA_VERSION doc comment
# for why readPlanTicketsOutputs rejects an unrecognised version outright.
EVIDENCE_SCHEMA_VERSION = 1

# Capped context inputs, matching draft_spec.py's own caps for the same
# reason: a handful of repository conventions in the prompt, not a whole
# doc site.
CONTEXT_FILE_MAX_BYTES = draft_spec.CONTEXT_FILE_MAX_BYTES
FILE_INVENTORY_MAX_LINES = draft_spec.FILE_INVENTORY_MAX_LINES

PLAN_INSTRUCTIONS = prompt_templates.load("plan_tickets", ("draft_dir", "verify_command"))
# The plan-rules part of the team design guide; see draft_spec.DESIGN_GUIDE_SECTION.
DESIGN_GUIDE_SECTION = prompt_templates.load("plan_tickets.design_guide", ("guide",))
# The tickets an operator handed over, given back to be revised with the
# feedback; factoryd stages them as one file for --previous-draft-file.
PREVIOUS_DRAFT_SECTION = prompt_templates.load("plan_tickets.previous_draft", ("tickets",))


def build_prompt(spec_text: str, request_text: str, workspace: Path, verify_command: str, draft_dir: Path, feedback: str | None = None, design_guide: str | None = None, previous_draft: str | None = None) -> tuple[str, bool]:
	"""Assembles the single prompt passed to pi_invocation. Returns
	(prompt, agents_md_used) -- see draft_spec.py's own build_prompt for
	why agents_md_used is threaded straight into the evidence payload.
	feedback, when given, is a plan_review rejection note the request
	driver wrote via --feedback-file -- see draft_spec.build_prompt's
	identical handling."""
	agents_md_used = build_app.committed_agents_md_blob(workspace) is not None
	sections = [
		f"## Approved spec\n\n{spec_text.strip()}\n",
		f"## Original request\n\n{request_text.strip()}\n",
	]
	architecture = draft_spec.read_capped_text_file(workspace / "ARCHITECTURE.md", CONTEXT_FILE_MAX_BYTES)
	if architecture:
		sections.append(f"## Repository architecture (ARCHITECTURE.md)\n\n{architecture}")
	readme = draft_spec.read_capped_text_file(workspace / "README.md", CONTEXT_FILE_MAX_BYTES)
	if readme:
		sections.append(f"## Repository overview (README.md)\n\n{readme}")
	inventory = draft_spec.file_inventory(workspace, FILE_INVENTORY_MAX_LINES)
	if inventory:
		sections.append(f"## Repository file inventory (git ls-files)\n\n```\n{inventory}\n```")
	if design_guide and design_guide.strip():
		sections.append(DESIGN_GUIDE_SECTION.format(guide=design_guide.strip()).rstrip("\n"))
	if previous_draft and previous_draft.strip():
		sections.append(PREVIOUS_DRAFT_SECTION.format(tickets=previous_draft.strip()).rstrip("\n"))
	if feedback and feedback.strip():
		sections.append(draft_spec.FEEDBACK_HEADER.format(kind="plan", feedback=draft_spec.neutralise_feedback(feedback.strip())))
	sections.append(PLAN_INSTRUCTIONS.format(draft_dir=draft_dir, verify_command=verify_command))
	return "\n\n".join(sections), agents_md_used


def write_evidence(
	evidence_path: Path, *, usage: dict | None, agent_exit_code: int, duration_s: float,
	agents_md_used: bool, thinking: str | None = None,
) -> None:
	payload = {
		"schema_version": EVIDENCE_SCHEMA_VERSION,
		"generated": datetime.now(timezone.utc).isoformat(),
		"usage": usage,
		"agent_exit_code": agent_exit_code,
		"duration_s": duration_s,
		"agents_md_used": agents_md_used,
		"thinking": thinking,
	}
	evidence_path.parent.mkdir(parents=True, exist_ok=True)
	evidence_path.write_text(json.dumps(payload, indent=2) + "\n")


def run_plan(
	workspace: Path, spec_path: Path, request_path: Path, verify_command: str, out_dir: Path, evidence_path: Path,
	timeout_minutes: int, feedback_path: Path | None = None, thinking: str | None = None,
	adapter=build_app.DEFAULT_ADAPTER, design_guide_path: Path | None = None, previous_draft_path: Path | None = None,
) -> int:
	spec_text = spec_path.read_text()
	request_text = request_path.read_text()
	draft_dir = workspace / DRAFT_RELATIVE_DIR
	feedback = draft_spec.read_feedback(feedback_path)
	design_guide = design_guide_path.read_text(encoding="utf-8") if design_guide_path else None
	previous_draft = previous_draft_path.read_text(encoding="utf-8") if previous_draft_path else None
	prompt, agents_md_used = build_prompt(
		spec_text, request_text, workspace, verify_command, DRAFT_RELATIVE_DIR, feedback, design_guide, previous_draft,
	)

	session_dir = workspace / ".factory-plan-draft" / "session"
	session_dir.mkdir(parents=True, exist_ok=True)
	command = adapter.invocation(
		workspace, prompt=prompt, session_dir=session_dir,
		continue_session=False, thinking=thinking,
	)

	started = time.monotonic()
	try:
		result = build_app.sh(command, cwd=workspace, timeout=timeout_minutes * 60)
	except subprocess.TimeoutExpired as exc:
		duration_s = time.monotonic() - started
		partial_stdout = exc.stdout
		if isinstance(partial_stdout, bytes):
			partial_stdout = partial_stdout.decode(errors="replace")
		write_evidence(
			evidence_path, usage=adapter.parse(partial_stdout or "").usage, agent_exit_code=-1,
			duration_s=duration_s, agents_md_used=agents_md_used, thinking=thinking,
		)
		hint = build_app.stderr_hint(exc.stderr)
		print(f"plan_tickets: agent timed out after {timeout_minutes} minutes" + (f": {hint}" if hint else ""), file=sys.stderr)
		return 2
	duration_s = time.monotonic() - started
	usage = adapter.parse(result.stdout).usage

	ticket_paths = sorted(draft_dir.glob("*.spec.md")) if draft_dir.is_dir() else []
	write_evidence(
		evidence_path, usage=usage, agent_exit_code=result.returncode,
		duration_s=duration_s, agents_md_used=agents_md_used, thinking=thinking,
	)
	# A live walk hit a 0-byte plan_tickets.log on a real failure --
	# write_evidence's JSON (agent_exit_code/duration/usage) lands in
	# --evidence, a container path the sandbox discards, so the log the
	# halt reason points at was the operator's only lead and said
	# nothing. Every failure path below
	# must print its own one-line reason so the log the driver captures is
	# never empty.
	if result.returncode != 0:
		# #4 (adversarial review of #15's fix): pi's own stderr (auth
		# failure, 401, model route down) is captured by build_app.sh but
		# was never printed, so "agent exited 1" was the whole story even
		# when pi already said why. A bounded, redacted last line, not the
		# raw text, and still one line so the Go side's quotedLogReason
		# (cmd/factoryd/plan_tickets_job.go) can find this exact line by
		# its "plan_tickets:" prefix. single_line, not just
		# last_nonblank_line's own line-boundary handling, per #1/#2
		# (round-2 adversarial review of this same fix): this printed
		# line is itself untrusted-text-derived and single-line
		# operator-facing.
		hint = build_app.stderr_hint(result.stderr)
		if hint:
			print(f"plan_tickets: agent exited {result.returncode}: {hint}", file=sys.stderr)
		else:
			print(f"plan_tickets: agent exited {result.returncode}", file=sys.stderr)
		return 2
	if not ticket_paths:
		hint = build_app.exited_zero_hint(adapter.parse(result.stdout).route_errors)
		if hint:
			print(f"plan_tickets: agent exited 0 but drafted no *.spec.md ticket files; model route error: {hint}", file=sys.stderr)
		else:
			print("plan_tickets: agent exited 0 but drafted no *.spec.md ticket files", file=sys.stderr)
		return 2

	out_dir.mkdir(parents=True, exist_ok=True)
	for ticket_path in ticket_paths:
		text = ticket_path.read_text()
		if not text.strip():
			continue
		(out_dir / ticket_path.name).write_text(text)
	if not any(out_dir.glob("*.spec.md")):
		print(f"plan_tickets: agent drafted {len(ticket_paths)} ticket file(s) but every one was empty", file=sys.stderr)
		return 2
	return 0


def main() -> int:
	parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
	parser.add_argument("--spec", required=True, type=Path, help="path to the approved spec.md")
	parser.add_argument("--request", required=True, type=Path, help="path to the verbatim request text (request.md)")
	parser.add_argument("--workspace", required=True, type=Path, help="repository checkout the plan is drafted against, read from at HEAD")
	parser.add_argument("--verify-command", required=True, help="the canonical Verify-Command every drafted ticket must declare verbatim")
	parser.add_argument("--out-dir", required=True, type=Path, help="directory the drafted tickets (NNN.spec.md) are copied into on success")
	parser.add_argument("--evidence", required=True, type=Path, help="where usage/exit-code/duration evidence JSON is always written, success or failure")
	parser.add_argument("--timeout-minutes", type=int, default=15, help="wall-clock budget for the single pi invocation")
	parser.add_argument(
		"--thinking", choices=build_app.THINKING_LEVELS, default=None,
		help="Pi thinking level for this job; omitted inherits Pi's installed setting.",
	)
	parser.add_argument(
		"--harness", choices=sorted(harness_adapters.ADAPTERS), default="pi",
		help="Coding agent to drive (see harness_adapters.py).",
	)
	parser.add_argument("--feedback-file", type=Path, default=None, help="optional operator feedback from a plan_review rejection (the request driver writes plan-feedback.md from Request.Rejections), included in the prompt as guidance only")
	parser.add_argument("--previous-draft-file", type=Path, default=None, help="optional tickets the operator handed over, in one file, to be revised with the feedback instead of planned afresh; staged by factoryd")
	parser.add_argument("--design-guide-file", type=Path, default=None, help="optional plan-rules part of the team design guide, staged by factoryd")
	args = parser.parse_args()

	adapter = harness_adapters.get(args.harness)
	adapter.prepare()
	workspace = args.workspace.resolve()
	return run_plan(
		workspace, args.spec.resolve(), args.request.resolve(), args.verify_command, args.out_dir, args.evidence,
		args.timeout_minutes, args.feedback_file, thinking=args.thinking, adapter=adapter,
		design_guide_path=args.design_guide_file, previous_draft_path=args.previous_draft_file,
	)


if __name__ == "__main__":
	sys.exit(main())
