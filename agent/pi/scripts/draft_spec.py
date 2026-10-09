#!/usr/bin/env python3
"""Draft a spec for one operator-submitted request, via a single headless
`pi` invocation.

Companion to build_app.py: reuses its pi_invocation/committed_agents_md_blob/
parse_usage helpers (see the imports below) rather than duplicating them,
so the two scripts' prompts, model routing, and usage accounting never
drift apart. Unlike build_app.py this is a single one-shot pass -- no
corrective rounds, no canonical verification, no independent review: the
factory's own request driver (cmd/factoryd/request_driver.go) is what
validates the result (internal/request.ValidateSpecSkeleton) and decides
whether to advance the request or halt it, exactly the same
"agent output is evidence, never the oracle" split build_app.py's own
BUILD_EVIDENCE.json keeps for a build.

Usage:
    python3 draft_spec.py --request request.md --workspace /path/to/repo \
        --out spec.md --evidence evidence.json [--timeout-minutes 10] \
        [--feedback-file spec-feedback.md]

The model is instructed to write its draft to a fixed path inside
--workspace (see DRAFT_RELATIVE_PATH below), not asked to print the spec
as its chat response: matching how build_app.py's own agent edits the
repo directly via its own tool calls rather than having its response text
parsed. This script then reads that file back, copies its content
verbatim to --out, and writes --evidence -- both may be, and in the
sandboxed case must be, paths inside --workspace too, since a sandboxed
worker's only writable mount is the workspace itself (see
cmd/factoryd/spec_draft_job.go's own doc comment for the container-path
convention this relies on).

Exit code 0 only when a non-empty draft was produced and copied to --out.
Exit code 2 for a model/route failure (pi itself failed, timed out, or
never wrote a draft) -- --evidence is still written in that case, with no
--out. Any other failure (a bad --request path, for instance) raises
and exits non-zero via the ordinary Python traceback, the same
convention build_app.py uses for its own argument/setup errors.
"""

from __future__ import annotations

import argparse
import ast
import json
import re
import subprocess
import sys
import time
from datetime import datetime, timezone
from pathlib import Path

# build_app.py lives alongside this script in the same embedded harness
# (internal/harness.Ensure extracts every agent/pi/scripts/*.py file
# together into one directory) -- importing it by relative path, not as
# an installed package, mirrors how both scripts are invoked: as bare
# `python3 <path>/draft_spec.py`, never `python3 -m`.
sys.path.insert(0, str(Path(__file__).resolve().parent))
import build_app  # noqa: E402
import harness_adapters  # noqa: E402
import prompt_templates  # noqa: E402

# DRAFT_RELATIVE_PATH is where the model is instructed to write its draft,
# relative to --workspace (pi's own cwd for the invocation below). Kept
# out of the workspace's tracked tree entirely (a leading-dot directory
# distinct from build_app.py's own .pi-build-session/) so a request whose
# --workspace is a live, shared checkout never risks the model's scratch
# output being mistaken for a tracked file.
DRAFT_RELATIVE_PATH = Path(".factory-spec-draft") / "SPEC_DRAFT.md"

# EVIDENCE_SCHEMA_VERSION's literal value (1) must always equal
# cmd/factoryd/spec_draft_job.go's specDraftEvidenceSchemaVersion constant --
# bump both together, in the same change, when write_evidence's payload
# shape changes. Unlike build_app.py's own BUILD_EVIDENCE.json (best-effort,
# never blocks a run), readSpecDraftOutputs rejects a schema_version it
# doesn't recognise outright: this evidence file is the only source of
# drafting cost/token accounting, so a shape drift must surface as a loud
# failure, not a silently empty SpecEvidence.
EVIDENCE_SCHEMA_VERSION = 1

# Capped context inputs: this is meant to carry a handful of repository
# conventions into the prompt, not a whole doc site.
CONTEXT_FILE_MAX_BYTES = 32 * 1024
FILE_INVENTORY_MAX_LINES = 400

# FEEDBACK_HEADER/neutralise_feedback/read_feedback_ex mirror
# draft_acceptance_oracles.py's own identically-named pieces -- shared
# here, not duplicated there, since plan_tickets.py already imports this
# module for read_capped_text_file/file_inventory and needs the same
# operator-feedback handling for its own --feedback-file. {kind} names
# which stage's draft the feedback is about ("spec" or "plan"), so the
# prompt reads correctly for either caller.
FEEDBACK_HEADER = prompt_templates.load("draft_feedback", ("kind", "feedback"))

# MAX_FEEDBACK_BYTES mirrors cmd/factoryd/request_driver.go's own
# maxFeedbackBytes (12 KiB) -- the Go side already caps what it writes to
# the feedback file, keeping the newest text; this is a second, independent
# cap on the read side, exactly like draft_acceptance_oracles.py's own.
MAX_FEEDBACK_BYTES = 12 * 1024


def neutralise_feedback(text: str) -> str:
	"""Feedback is operator/model-influenced text placed between delimiter
	lines; break any run of the delimiter characters so a reason can never
	close the section early or forge a new one."""
	return text.replace("<<<", "< < <").replace(">>>", "> > >")


def read_feedback_ex(path: Path | None) -> tuple[str | None, bool]:
	"""Returns (text, truncated). The text is the feedback file's LAST
	MAX_FEEDBACK_BYTES (feedback accumulates oldest-first, so the newest
	operator reason is at the end and must survive), or None when path is
	unset, missing, unreadable, or blank -- the optional flag must never
	turn a missing note into a failed draft. truncated is True when older
	text was cut."""
	if path is None:
		return None, False
	try:
		with open(path, "rb") as handle:
			handle.seek(0, 2)
			size = handle.tell()
			truncated = size > MAX_FEEDBACK_BYTES
			handle.seek(max(0, size - MAX_FEEDBACK_BYTES))
			raw = handle.read(MAX_FEEDBACK_BYTES)
	except OSError:
		return None, False
	text = raw.decode("utf-8", errors="replace")
	return (text if text.strip() else None), truncated


def read_feedback(path: Path | None) -> str | None:
	return read_feedback_ex(path)[0]

SPEC_SKELETON_INSTRUCTIONS = prompt_templates.load("draft_spec", ("draft_path",))
# The spec-decisions part of the team design guide the target repository's
# .factory.yml names; factoryd stages it for --design-guide-file.
DESIGN_GUIDE_SECTION = prompt_templates.load("draft_spec.design_guide", ("guide",))
# The spec an operator handed over, given back to be revised after a
# spec_review rejection; factoryd stages it for --previous-draft-file.
PREVIOUS_DRAFT_SECTION = prompt_templates.load("draft_spec.previous_draft", ("spec",))

# --- Worked-example verification (added after live runs where drafted criteria
# carried wrong arithmetic, e.g. `Eval("10 % 3 ^ 2")` stated as 10 when it is 1;
# they were caught only by the post-build conformity gate). Advisory only: it
# appends a warning to the spec's last section and never blocks or fails the
# draft -- any error degrades to "no warning".
EXAMPLE_CHECK_RELATIVE_PATH = Path(".factory-spec-draft") / "EXAMPLE_CHECK.json"
EXAMPLE_CHECK_MAX_EXAMPLES = 12
EXAMPLE_CHECK_TIMEOUT_S = 180
# The arithmetic recheck is a short, fixed-budget call: pin a low level so
# neither the job's --thinking nor Pi's installed default (either can be
# max) can push it past EXAMPLE_CHECK_TIMEOUT_S and silently skip it.
EXAMPLE_CHECK_THINKING = "low"
# Headroom kept after the check so the spec is still written before the
# job's own deadline.
EXAMPLE_CHECK_MARGIN_S = 30
UNVERIFIED_HEADING = "**Unverified worked examples**"

# `call` <up to 40 non-backtick chars: returns/yields/=/->/...> `result`
EXAMPLE_RE = re.compile(
	r"`(?P<call>[^`\n]{1,200})`[^`\n]{0,40}?"
	r"(?:returns?|returning|yields?|evaluates to|evaluating to|gives|results? in|=|->|\u2192)"
	r"\s*`?(?P<result>-?\d+)`?(?![\w.])"
)
_WRAPPED_RE = re.compile(r"^[A-Za-z_][\w.]*\(\s*([\"'])(?P<expr>.*)\1\s*\)$")
_BARE_EXPR_RE = re.compile(r"^[\d\s+\-*%()/]+$")
MAX_INT = 10**12


def extract_examples(spec_text: str) -> list[tuple[str, str]]:
	"""Returns up to EXAMPLE_CHECK_MAX_EXAMPLES distinct (call, claimed_result)
	pairs found as `call` ... returns `N` in the spec text."""
	seen: set[tuple[str, str]] = set()
	out: list[tuple[str, str]] = []
	for m in EXAMPLE_RE.finditer(spec_text):
		call, result = m.group("call").strip(), m.group("result")
		if not re.search(r"\d", call) or (call, result) in seen:
			continue
		seen.add((call, result))
		out.append((call, result))
		if len(out) >= EXAMPLE_CHECK_MAX_EXAMPLES:
			break
	return out


def _eval_node(node: ast.AST) -> int:
	if isinstance(node, ast.Constant) and type(node.value) is int:
		return node.value
	if isinstance(node, ast.UnaryOp) and isinstance(node.op, ast.USub):
		return -_eval_node(node.operand)
	if isinstance(node, ast.BinOp):
		left, right = _eval_node(node.left), _eval_node(node.right)
		if isinstance(node.op, ast.Add):
			value = left + right
		elif isinstance(node.op, ast.Sub):
			value = left - right
		elif isinstance(node.op, ast.Mult):
			value = left * right
		elif isinstance(node.op, (ast.Mod, ast.FloorDiv)) and left >= 0 and right > 0:
			# Non-negative operands only: negative-operand semantics differ per language.
			value = left % right if isinstance(node.op, ast.Mod) else left // right
		else:
			raise ValueError("unsupported operator")
		if abs(value) > MAX_INT:
			raise ValueError("too large")
		return value
	raise ValueError("unsupported node")


def safe_int_eval(expr: str) -> int | None:
	"""Evaluates a pure integer expression over + - * % // and parentheses via
	an AST whitelist (never eval/exec). ^ and / are deliberately unsupported:
	their meaning depends on the language the spec describes. None if
	unparseable or unsupported."""
	try:
		if len(expr) > 200 or not _BARE_EXPR_RE.match(expr):
			return None
		return _eval_node(ast.parse(expr.strip(), mode="eval").body)
	except (ValueError, SyntaxError, RecursionError, MemoryError):
		return None


def mechanical_check(examples: list[tuple[str, str]]) -> list[str]:
	"""Warnings for examples whose expression a strict evaluator can compute
	and whose stated result differs."""
	warnings = []
	for call, claimed in examples:
		wrapped = _WRAPPED_RE.match(call)
		expr = wrapped.group("expr") if wrapped else call
		actual = safe_int_eval(expr)
		if actual is not None and str(actual) != claimed:
			warnings.append(f"`{call}` is stated as {claimed}, but plain integer arithmetic gives {actual}")
	return warnings


EXAMPLE_CHECK_INSTRUCTIONS = prompt_templates.load("draft_spec.example_check", ("listing", "out_path"))


def build_example_check_prompt(examples: list[tuple[str, str]], out_path: Path) -> str:
	# The claimed results are withheld on purpose so the recomputation is independent.
	listing = "\n".join(f"{i}. {call}" for i, (call, _) in enumerate(examples, 1))
	return EXAMPLE_CHECK_INSTRUCTIONS.format(listing=listing, out_path=out_path)


def parse_example_check(text: str, examples: list[tuple[str, str]]) -> list[str]:
	try:
		answers = json.loads(text)
	except ValueError:
		return []
	if not isinstance(answers, dict):
		return []
	warnings = []
	for i, (call, claimed) in enumerate(examples, 1):
		answer = str(answers.get(str(i), "")).strip().strip("`")
		if re.fullmatch(r"-?\d+", answer) and answer != claimed:
			warnings.append(f"`{call}` is stated as {claimed}, but an independent recomputation gives {answer}")
	return warnings


def model_check(
	workspace: Path, examples: list[tuple[str, str]], adapter=build_app.DEFAULT_ADAPTER,
) -> tuple[list[str], dict | None]:
	"""One bounded extra pi call (same route as the draft, no repo access
	needed). Returns (warnings, usage); any failure -> ([], None)."""
	if not examples:
		return [], None
	check_path = workspace / EXAMPLE_CHECK_RELATIVE_PATH
	try:
		check_path.unlink(missing_ok=True)
		session_dir = workspace / ".factory-spec-draft" / "check-session"
		session_dir.mkdir(parents=True, exist_ok=True)
		check_prompt = build_example_check_prompt(examples, EXAMPLE_CHECK_RELATIVE_PATH)
		build_app.saved_prompts.save_prompt(session_dir, "draft-spec-example-check", check_prompt)
		command = adapter.invocation(
			workspace, prompt=check_prompt,
			session_dir=session_dir, continue_session=False, thinking=EXAMPLE_CHECK_THINKING,
		)
		result = build_app.sh(command, cwd=workspace, timeout=EXAMPLE_CHECK_TIMEOUT_S)
		if result.returncode != 0 or not check_path.is_file():
			return [], None
		return parse_example_check(check_path.read_text(), examples), adapter.parse(result.stdout).usage
	except Exception:  # advisory pass must never fail the draft
		return [], None


def append_unverified_block(spec_text: str, warnings: list[str]) -> str:
	"""Appends the warning block at the end of the spec, i.e. inside the last
	required section (`## Open questions`), so the skeleton stays valid and the
	reviewer sees it at spec_review."""
	if not warnings:
		return spec_text
	lines = "\n".join(f"- {w}" for w in warnings)
	return (
		spec_text.rstrip("\n") + f"\n\n{UNVERIFIED_HEADING} (automatic check, advisory): "
		"these worked examples disagree with an independent recomputation. "
		"Verify or fix them before approving; they are not confirmed wrong.\n\n" + lines + "\n"
	)


def check_worked_examples(
	workspace: Path, spec_text: str, time_left_s: float | None = None, adapter=build_app.DEFAULT_ADAPTER,
) -> tuple[str, dict]:
	"""Returns (possibly annotated spec, evidence dict). Never raises.

	time_left_s is what remains of the job's own budget; when it can't fit
	the model recheck plus EXAMPLE_CHECK_MARGIN_S, the recheck is skipped
	(recorded as model_check_skipped) so an advisory check never costs a
	finished spec its deadline."""
	info: dict = {"examples": 0, "warnings": 0, "model_usage": None}
	try:
		examples = extract_examples(spec_text)
		info["examples"] = len(examples)
		warnings = mechanical_check(examples)
		flagged = {w.split("`")[1] for w in warnings}
		to_check = [e for e in examples if e[0] not in flagged]
		if to_check and time_left_s is not None and time_left_s < EXAMPLE_CHECK_TIMEOUT_S + EXAMPLE_CHECK_MARGIN_S:
			info["model_check_skipped"] = f"only {int(time_left_s)}s of the job budget left"
			model_warnings, usage = [], None
		else:
			model_warnings, usage = model_check(workspace, to_check, adapter)
		info["model_usage"] = usage
		warnings += model_warnings
		info["warnings"] = len(warnings)
		return append_unverified_block(spec_text, warnings), info
	except Exception:
		return spec_text, info


def read_capped_text_file(path: Path, max_bytes: int) -> str | None:
	"""Reads path (relative to nothing in particular -- callers pass an
	already-workspace-joined path), capped at max_bytes, or None if it
	doesn't exist or isn't a regular file. ARCHITECTURE.md/README.md are
	supplementary prompt context; AGENTS.md is not read here because the
	harness already loads it into its own system prompt (see
	build_app.committed_agents_md_blob)."""
	if not path.is_file():
		return None
	data = path.read_bytes()[: max_bytes + 1]
	truncated = len(data) > max_bytes
	text = data[:max_bytes].decode(errors="ignore") if truncated else data.decode(errors="ignore")
	if truncated:
		text += f"\n\n[... {path.name} truncated at {max_bytes // 1024}KB ...]\n"
	return text


def file_inventory(workspace: Path, max_lines: int) -> str | None:
	"""Returns `git ls-files` output, capped at max_lines, or None if the
	command failed outright (e.g. workspace isn't a git repo)."""
	result = build_app.sh(["git", "ls-files"], cwd=workspace)
	if result.returncode != 0:
		return None
	lines = result.stdout.splitlines()
	truncated = len(lines) > max_lines
	shown = lines[:max_lines]
	text = "\n".join(shown)
	if truncated:
		text += f"\n... ({len(lines) - max_lines} more files not shown)"
	return text


def build_prompt(
	request_text: str, workspace: Path, draft_path: Path, feedback: str | None = None, design_guide: str | None = None,
	previous_draft: str | None = None,
) -> tuple[str, bool]:
	"""Assembles the single prompt passed to pi_invocation. Returns
	(prompt, agents_md_used) -- agents_md_used is threaded straight into
	the evidence payload, the same field build_app.py's own
	BUILD_EVIDENCE.json carries for the identical reason (see
	write_evidence_json there). feedback, when given, is a spec_review
	rejection note the request driver wrote via --feedback-file -- placed
	last, right before the drafting instructions, so it's the thing the
	model reads immediately before being told what to write."""
	agents_md_used = build_app.committed_agents_md_blob(workspace) is not None
	sections = [f"## Request\n\n{request_text.strip()}\n"]
	architecture = read_capped_text_file(workspace / "ARCHITECTURE.md", CONTEXT_FILE_MAX_BYTES)
	if architecture:
		sections.append(f"## Repository architecture (ARCHITECTURE.md)\n\n{architecture}")
	readme = read_capped_text_file(workspace / "README.md", CONTEXT_FILE_MAX_BYTES)
	if readme:
		sections.append(f"## Repository overview (README.md)\n\n{readme}")
	inventory = file_inventory(workspace, FILE_INVENTORY_MAX_LINES)
	if inventory:
		sections.append(f"## Repository file inventory (git ls-files)\n\n```\n{inventory}\n```")
	if design_guide and design_guide.strip():
		sections.append(DESIGN_GUIDE_SECTION.format(guide=design_guide.strip()).rstrip("\n"))
	if previous_draft and previous_draft.strip():
		sections.append(PREVIOUS_DRAFT_SECTION.format(spec=previous_draft.strip()).rstrip("\n"))
	if feedback and feedback.strip():
		sections.append(FEEDBACK_HEADER.format(kind="spec", feedback=neutralise_feedback(feedback.strip())))
	sections.append(SPEC_SKELETON_INSTRUCTIONS.format(draft_path=draft_path))
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


def run_draft(
	workspace: Path, request_path: Path, out_path: Path, evidence_path: Path, timeout_minutes: int,
	feedback_path: Path | None = None, thinking: str | None = None, adapter=build_app.DEFAULT_ADAPTER,
	design_guide_path: Path | None = None, previous_draft_path: Path | None = None,
) -> int:
	request_text = request_path.read_text()
	draft_path = workspace / DRAFT_RELATIVE_PATH
	feedback = read_feedback(feedback_path)
	design_guide = design_guide_path.read_text(encoding="utf-8") if design_guide_path else None
	previous_draft = previous_draft_path.read_text(encoding="utf-8") if previous_draft_path else None
	prompt, agents_md_used = build_prompt(request_text, workspace, DRAFT_RELATIVE_PATH, feedback, design_guide, previous_draft)

	session_dir = workspace / ".factory-spec-draft" / "session"
	session_dir.mkdir(parents=True, exist_ok=True)
	build_app.saved_prompts.save_prompt(session_dir, "draft-spec", prompt)
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
		print(f"draft_spec: agent timed out after {timeout_minutes} minutes" + (f": {hint}" if hint else ""), file=sys.stderr)
		return 2
	duration_s = time.monotonic() - started
	usage = adapter.parse(result.stdout).usage

	draft_text = draft_path.read_text() if draft_path.is_file() else ""
	write_evidence(
		evidence_path, usage=usage, agent_exit_code=result.returncode,
		duration_s=duration_s, agents_md_used=agents_md_used, thinking=thinking,
	)
	# plan_tickets.py's identical pattern left a 0-byte log on a real
	# failure -- write_evidence's JSON lands in --evidence, a container
	# path the sandbox discards, so the log the halt reason points at was
	# the operator's only lead and said nothing. Print the reason so the
	# log is never empty.
	if result.returncode != 0:
		# Found in adversarial review of that fix: pi's own stderr (auth
		# failure, 401, model route down) is captured by build_app.sh but
		# was never printed, so "agent exited 1" was the whole story even
		# when pi already said why. A bounded, redacted last line, not the
		# raw text, and still one line so the Go side's quotedLogReason
		# (cmd/factoryd/spec_draft_job.go) can find this exact line by its
		# "draft_spec:" prefix. single_line, not just last_nonblank_line's
		# own line-boundary handling, per #1/#2 (round-2 adversarial
		# review of this same fix): this printed line is itself
		# untrusted-text-derived and single-line operator-facing.
		hint = build_app.stderr_hint(result.stderr)
		if hint:
			print(f"draft_spec: agent exited {result.returncode}: {hint}", file=sys.stderr)
		else:
			print(f"draft_spec: agent exited {result.returncode}", file=sys.stderr)
		return 2
	if not draft_text.strip():
		hint = build_app.exited_zero_hint(adapter.parse(result.stdout).route_errors)
		if hint:
			print(f"draft_spec: agent exited 0 but wrote no spec draft; model route error: {hint}", file=sys.stderr)
		else:
			print("draft_spec: agent exited 0 but wrote no spec draft", file=sys.stderr)
		return 2
	# The recheck runs at EXAMPLE_CHECK_THINKING, never the job's level, and
	# only if it fits in what is left of this job's budget.
	time_left_s = timeout_minutes * 60 - (time.monotonic() - started)
	draft_text, example_check = check_worked_examples(workspace, draft_text, time_left_s=time_left_s, adapter=adapter)
	try:
		payload = json.loads(evidence_path.read_text())
		payload["example_check"] = example_check
		evidence_path.write_text(json.dumps(payload, indent=2) + "\n")
	except (OSError, ValueError):
		pass
	out_path.parent.mkdir(parents=True, exist_ok=True)
	out_path.write_text(draft_text)
	return 0


def main() -> int:
	parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
	parser.add_argument("--request", required=True, type=Path, help="path to the verbatim request text (request.md)")
	parser.add_argument("--workspace", required=True, type=Path, help="repository checkout the spec is drafted against, read from at HEAD -- see this script's own doc comment for why it is not actually write-protected at the mount level")
	parser.add_argument("--out", required=True, type=Path, help="where the drafted spec.md is written on success")
	parser.add_argument("--evidence", required=True, type=Path, help="where usage/exit-code/duration evidence JSON is always written, success or failure")
	parser.add_argument("--timeout-minutes", type=int, default=10, help="wall-clock budget for the single pi invocation")
	parser.add_argument(
		"--thinking", choices=build_app.THINKING_LEVELS, default=None,
		help="Pi thinking level for this job; omitted inherits Pi's installed setting.",
	)
	parser.add_argument(
		"--harness", choices=sorted(harness_adapters.ADAPTERS), default="pi",
		help="Coding agent to drive (see harness_adapters.py).",
	)
	parser.add_argument("--feedback-file", type=Path, default=None, help="optional operator feedback from a spec_review rejection (the request driver writes spec-feedback.md from Request.Rejections), included in the prompt as guidance only")
	parser.add_argument("--previous-draft-file", type=Path, default=None, help="optional spec the operator handed over, to be revised with the feedback instead of drafted afresh; staged by factoryd")
	parser.add_argument("--design-guide-file", type=Path, default=None, help="optional spec-decisions part of the team design guide, staged by factoryd")
	args = parser.parse_args()

	adapter = harness_adapters.get(args.harness)
	adapter.prepare()
	workspace = args.workspace.resolve()
	return run_draft(
		workspace, args.request.resolve(), args.out, args.evidence, args.timeout_minutes,
		args.feedback_file, thinking=args.thinking, adapter=adapter, design_guide_path=args.design_guide_file,
		previous_draft_path=args.previous_draft_file,
	)


if __name__ == "__main__":
	sys.exit(main())
