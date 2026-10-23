#!/usr/bin/env python3
"""Draft a repository's root AGENTS.md through one sandboxed model job.

The factory supplies the operator-facing instructions in a staged input file.
The model may inspect the repository and run its own commands inside the
sandbox, but it writes only the fixed scratch output below. The Go caller
copies that output to the repository root after the job exits successfully.
"""

from __future__ import annotations

import argparse
import subprocess
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import build_app  # noqa: E402
import harness_adapters  # noqa: E402
import prompt_templates  # noqa: E402

DRAFT_RELATIVE_PATH = Path(".factory-agents-draft") / "AGENTS.md"
CONTEXT_MAX_BYTES = 16 * 1024
INVENTORY_MAX_LINES = 300

DRAFT_INSTRUCTIONS = prompt_templates.load("draft_agents", ("instructions", "draft_path", "context"))


def read_capped(path: Path) -> str | None:
	if not path.is_file():
		return None
	with path.open("rb") as handle:
		data = handle.read(CONTEXT_MAX_BYTES + 1)
	truncated = len(data) > CONTEXT_MAX_BYTES
	text = data[:CONTEXT_MAX_BYTES].decode(errors="replace")
	if truncated:
		text += f"\n\n[... {path.name} truncated at {CONTEXT_MAX_BYTES // 1024}KB ...]"
	return text


def repository_context(workspace: Path) -> str:
	sections: list[str] = []
	for name in ("README.md", "CONTRIBUTING.md", "Makefile", "go.mod", "package.json", "pyproject.toml"):
		text = read_capped(workspace / name)
		if text:
			sections.append(f"### {name}\n\n{text}")
	result = build_app.sh(["git", "ls-files"], cwd=workspace)
	if result.returncode == 0:
		lines = result.stdout.splitlines()
		shown = lines[:INVENTORY_MAX_LINES]
		inventory = "\n".join(shown)
		if len(lines) > INVENTORY_MAX_LINES:
			inventory += f"\n... ({len(lines) - INVENTORY_MAX_LINES} more files not shown)"
		sections.append("### Tracked file inventory\n\n```\n" + inventory + "\n```")
	else:
		paths = []
		for path in sorted(workspace.rglob("*")):
			relative = path.relative_to(workspace)
			if ".git" in relative.parts or ".factory-agents-draft" in relative.parts or not path.is_file():
				continue
			paths.append(relative.as_posix())
			if len(paths) >= INVENTORY_MAX_LINES:
				break
		sections.append("### Repository file inventory\n\n```\n" + "\n".join(paths) + "\n```")
	return "\n\n".join(sections) or "No repository context was available; inspect the workspace directly."


def build_prompt(instructions: str, workspace: Path, draft_path: Path) -> str:
	return DRAFT_INSTRUCTIONS.format(
		instructions=instructions.strip(),
		draft_path=str(draft_path),
		context=repository_context(workspace),
	)


def run_draft(
	workspace: Path,
	instructions_path: Path,
	out_path: Path,
	timeout_minutes: int,
	thinking: str | None = None,
	adapter=build_app.DEFAULT_ADAPTER,
) -> int:
	instructions = instructions_path.read_text(encoding="utf-8")
	draft_path = workspace / DRAFT_RELATIVE_PATH
	prompt = build_prompt(instructions, workspace, DRAFT_RELATIVE_PATH)
	session_dir = workspace / ".factory-agents-draft" / "session"
	session_dir.mkdir(parents=True, exist_ok=True)
	build_app.saved_prompts.save_prompt(session_dir, "draft-agents", prompt)
	command = adapter.invocation(
		workspace,
		prompt=prompt,
		session_dir=session_dir,
		continue_session=False,
		thinking=thinking,
	)
	try:
		result = build_app.sh(command, cwd=workspace, timeout=timeout_minutes * 60)
	except subprocess.TimeoutExpired as exc:
		partial_stdout = exc.stdout
		if isinstance(partial_stdout, bytes):
			partial_stdout = partial_stdout.decode(errors="replace")
		hint = build_app.agent_exit_hint(adapter.parse(partial_stdout or ""), exc.stderr, adapter.stderr_noise)
		message = f"draft_agents: agent timed out after {timeout_minutes} minutes"
		if hint:
			message += f": {hint}"
		print(message, file=sys.stderr)
		return 2
	if result.returncode != 0:
		hint = build_app.agent_exit_hint(adapter.parse(result.stdout), result.stderr, adapter.stderr_noise)
		message = f"draft_agents: agent exited {result.returncode}"
		if hint:
			message += f": {hint}"
		print(message, file=sys.stderr)
		return 2
	if not draft_path.is_file() or not draft_path.read_text(encoding="utf-8").strip():
		print("draft_agents: agent exited 0 but wrote no AGENTS.md draft", file=sys.stderr)
		return 2
	if out_path != draft_path:
		out_path.parent.mkdir(parents=True, exist_ok=True)
		out_path.write_text(draft_path.read_text(encoding="utf-8"), encoding="utf-8")
	return 0


def main() -> int:
	parser = argparse.ArgumentParser(description=__doc__)
	parser.add_argument("--workspace", required=True, type=Path)
	parser.add_argument("--instructions", required=True, type=Path)
	parser.add_argument("--out", required=True, type=Path)
	parser.add_argument("--timeout-minutes", type=int, default=10)
	parser.add_argument("--thinking", choices=build_app.THINKING_LEVELS, default=None)
	parser.add_argument("--harness", choices=sorted(harness_adapters.ADAPTERS), default="pi")
	args = parser.parse_args()
	adapter = harness_adapters.get(args.harness)
	adapter.prepare()
	return run_draft(
		args.workspace.resolve(), args.instructions.resolve(), args.out,
		args.timeout_minutes, thinking=args.thinking, adapter=adapter,
	)


if __name__ == "__main__":
	sys.exit(main())
