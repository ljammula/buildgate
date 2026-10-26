#!/usr/bin/env python3
"""Draft the read-back of a spec the operator handed over, via one headless
harness invocation.

The read-back is what the factory will build, in the model's own words, and
the questions it would otherwise guess at; buildgate shows it at
spec_review beside the spec. The model writes one JSON object to a fixed
path inside --workspace (READ_BACK_RELATIVE_PATH); this script refuses it
structurally (shape, question count, byte sizes) and copies the checked
object to --out. It reads no prose. factoryd checks the same limits again
(request.ValidateReadBack) before recording anything.

Usage:
    python3 read_back.py --spec spec.md --workspace /path/to/repo \
        --out read-back.json --evidence evidence.json [--timeout-minutes 10]

Exit code 0 when a read-back within the limits was written to --out. Exit
code 2 for a model or route failure (the harness failed, timed out, or wrote
nothing). Exit code 3 when the model wrote a read-back this script refuses.
--evidence is written whenever the harness ran, in draft_spec.py's shape.
Every failure prints one "read_back:" line to stderr.
"""

from __future__ import annotations

import argparse
import json
import subprocess
import sys
import time
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import build_app  # noqa: E402
import draft_spec  # noqa: E402 -- write_evidence, neutralise_feedback
import harness_adapters  # noqa: E402
import prompt_templates  # noqa: E402

READ_BACK_RELATIVE_PATH = Path(".factory-read-back") / "READ_BACK.json"

# The limits mirror internal/request/read_back.go's MaxReadBack* constants;
# change both together.
MAX_QUESTIONS = 7
MAX_SUMMARY_BYTES = 1500
MAX_QUESTION_BYTES = 400
# A read-back within the limits is a few KB; a file over this is refused
# before it is parsed.
MAX_FILE_BYTES = 64 * 1024

REFUSED_EXIT_CODE = 3

READ_BACK_INSTRUCTIONS = prompt_templates.load(
	"read_back", ("spec", "draft_path", "max_summary_bytes", "max_questions", "max_question_bytes"),
)


class Refused(ValueError):
	"""The model's read-back is not in the required shape or over a limit."""


def build_prompt(spec_text: str, draft_path: Path) -> str:
	return READ_BACK_INSTRUCTIONS.format(
		spec=draft_spec.neutralise_feedback(spec_text.strip()),
		draft_path=str(draft_path),
		max_summary_bytes=MAX_SUMMARY_BYTES,
		max_questions=MAX_QUESTIONS,
		max_question_bytes=MAX_QUESTION_BYTES,
	)


def _text_within(value, limit: int, what: str) -> str:
	if not isinstance(value, str) or not value.strip():
		raise Refused(f"{what} is missing or empty")
	try:
		encoded = value.encode("utf-8")
	except UnicodeEncodeError as exc:
		# JSON can spell a lone surrogate ("\ud800"), which is no text.
		raise Refused(f"{what} is not valid UTF-8 text") from exc
	if len(encoded) > limit:
		raise Refused(f"{what} is over {limit} bytes")
	return value


def check_read_back(raw: bytes) -> dict:
	"""Returns the read-back as {"summary", "questions": [{"id", "text"}]},
	or raises Refused. Only the shape and the sizes are checked."""
	if len(raw) > MAX_FILE_BYTES:
		raise Refused(f"the read-back file is over {MAX_FILE_BYTES} bytes")
	try:
		data = json.loads(raw.decode("utf-8"))
	except (UnicodeDecodeError, ValueError) as exc:
		raise Refused(f"the read-back is not JSON: {type(exc).__name__}") from exc
	if not isinstance(data, dict) or set(data) != {"summary", "questions"}:
		raise Refused('the read-back must be an object with exactly "summary" and "questions"')
	summary = _text_within(data["summary"], MAX_SUMMARY_BYTES, "the summary")
	questions = data["questions"]
	if not isinstance(questions, list):
		raise Refused('"questions" must be a list')
	if len(questions) > MAX_QUESTIONS:
		raise Refused(f"{len(questions)} questions, at most {MAX_QUESTIONS}")
	checked = []
	for index, question in enumerate(questions, start=1):
		if not isinstance(question, dict) or set(question) != {"id", "text"}:
			raise Refused(f'question {index} must be an object with exactly "id" and "text"')
		if question["id"] != f"Q{index}":
			raise Refused(f"question {index} must have the id Q{index}")
		text = _text_within(question["text"], MAX_QUESTION_BYTES, f"question Q{index}")
		checked.append({"id": question["id"], "text": text})
	return {"summary": summary, "questions": checked}


def _fail(message: str, code: int = 2) -> int:
	print(f"read_back: {message}", file=sys.stderr)
	return code


def run_read_back(
	workspace: Path, spec_path: Path, out_path: Path, evidence_path: Path, timeout_minutes: int,
	thinking: str | None = None, adapter=build_app.DEFAULT_ADAPTER,
) -> int:
	prompt = build_prompt(spec_path.read_text(encoding="utf-8"), READ_BACK_RELATIVE_PATH)
	draft_path = workspace / READ_BACK_RELATIVE_PATH
	session_dir = workspace / READ_BACK_RELATIVE_PATH.parent / "session"
	session_dir.mkdir(parents=True, exist_ok=True)
	build_app.saved_prompts.save_prompt(session_dir, "read-back", prompt)
	command = adapter.invocation(workspace, prompt=prompt, session_dir=session_dir, continue_session=False, thinking=thinking)
	agents_md_used = build_app.committed_agents_md_blob(workspace) is not None
	started = time.monotonic()
	try:
		result = build_app.sh(command, cwd=workspace, timeout=timeout_minutes * 60)
	except subprocess.TimeoutExpired as exc:
		partial = exc.stdout.decode(errors="replace") if isinstance(exc.stdout, bytes) else (exc.stdout or "")
		draft_spec.write_evidence(
			evidence_path, usage=adapter.parse(partial).usage, agent_exit_code=-1,
			duration_s=time.monotonic() - started, agents_md_used=agents_md_used, thinking=thinking,
		)
		hint = build_app.agent_exit_hint(adapter.parse(partial), exc.stderr, adapter.stderr_noise)
		return _fail(f"agent timed out after {timeout_minutes} minutes" + (f": {hint}" if hint else ""))
	draft_spec.write_evidence(
		evidence_path, usage=adapter.parse(result.stdout).usage, agent_exit_code=result.returncode,
		duration_s=time.monotonic() - started, agents_md_used=agents_md_used, thinking=thinking,
	)
	if result.returncode != 0:
		hint = build_app.agent_exit_hint(adapter.parse(result.stdout), result.stderr, adapter.stderr_noise)
		return _fail(f"agent exited {result.returncode}" + (f": {hint}" if hint else ""))
	if not draft_path.is_file():
		return _fail("agent exited 0 but wrote no read-back")
	try:
		checked = check_read_back(draft_path.read_bytes()[: MAX_FILE_BYTES + 1])
	except Refused as exc:
		return _fail(f"refused: {exc}", REFUSED_EXIT_CODE)
	out_path.parent.mkdir(parents=True, exist_ok=True)
	out_path.write_text(json.dumps(checked, indent=2) + "\n", encoding="utf-8")
	return 0


def main() -> int:
	parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
	parser.add_argument("--spec", required=True, type=Path, help="the handed-over spec to read back")
	parser.add_argument("--workspace", required=True, type=Path, help="repository checkout the model may read")
	parser.add_argument("--out", required=True, type=Path, help="where the checked read-back JSON is written on success")
	parser.add_argument("--evidence", required=True, type=Path, help="where usage/exit-code/duration evidence JSON is written")
	parser.add_argument("--timeout-minutes", type=int, default=10, help="wall-clock budget for the single harness invocation")
	parser.add_argument("--thinking", choices=build_app.THINKING_LEVELS, default=None)
	parser.add_argument("--harness", choices=sorted(harness_adapters.ADAPTERS), default="pi")
	args = parser.parse_args()
	adapter = harness_adapters.get(args.harness)
	adapter.prepare()
	return run_read_back(
		args.workspace.resolve(), args.spec.resolve(), args.out, args.evidence, args.timeout_minutes,
		thinking=args.thinking, adapter=adapter,
	)


if __name__ == "__main__":
	sys.exit(main())
