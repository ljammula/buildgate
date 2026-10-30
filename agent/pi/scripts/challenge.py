#!/usr/bin/env python3
"""The challenger job: ONE sandboxed launch, ONE agentic model turn that may
read the repository, write new test files and run them, on a scratch copy of
the result commit (SC-022). The approved spec (with the operator's read-back
answers) and the list of changed paths are the only inputs: no build session,
handoff, agent notes, review finding or build prompt reaches it.

Nothing this script reports is a verdict. The host reads the new files from
the scratch worktree with git, holds a copy of each that passes its shape
rules, and runs them itself.

Usage:
    python3 challenge.py --workspace <dir> --spec <approved spec> \\
        [--changed-paths <file>] [--harness pi|pifork|codex|copilot] [--thinking low|...]

Exit 0 when the turn ran, CHALLENGE_NO_TURN (42) when it produced nothing.
"""
from __future__ import annotations

import argparse
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import build_app  # noqa: E402
import harness_adapters  # noqa: E402
import prompt_templates  # noqa: E402

CHALLENGE_NO_TURN = 42
MAX_FILES = 5
MAX_CHANGED_PATHS = 200
CHALLENGE_TIMEOUT_MINUTES = 25
SESSION_DIR = ".pi-challenge-session"

PROMPT = prompt_templates.load("challenge", ("spec_text", "changed_paths", "max_files"))


def challenge_prompt(spec_text: str, changed_paths: list[str]) -> str:
	"""The challenger's prompt: the approved spec, the changed paths (at
	most MAX_CHANGED_PATHS, one per line), and the file cap."""
	shown = changed_paths[:MAX_CHANGED_PATHS]
	listing = "\n".join(f"- {p}" for p in shown) or "(none listed)"
	if len(changed_paths) > len(shown):
		listing += f"\n... and {len(changed_paths) - len(shown)} more"
	return PROMPT.format(spec_text=spec_text.rstrip("\n"), changed_paths=listing, max_files=MAX_FILES).removesuffix("\n")


def run_challenge(
	workspace: Path, *, spec_path: Path, changed_paths_file: Path | None = None, thinking: str | None = None,
	adapter=build_app.DEFAULT_ADAPTER, setup_commands: list[str] | None = None,
) -> tuple[int, str]:
	"""Runs the repository's setup commands, then the one turn, and returns
	(exit code, one line). A failed setup command ends the job with
	build_app.SETUP_FAILED_EXIT_CODE before any model call. The workspace is not
	read afterwards here: the host does that."""
	if setup_commands:
		failed, tail = build_app.run_setup(workspace, setup_commands, None)
		if failed:
			return build_app.SETUP_FAILED_EXIT_CODE, build_app.single_line(build_app.redact(build_app.SETUP_FAILED_BLOCKER + failed, 300))
	changed = changed_paths_file.read_text().splitlines() if changed_paths_file else []
	prompt = challenge_prompt(spec_path.read_text(), [p for p in changed if p.strip()])
	turn = build_app.run_review_turn(
		workspace, prompt=prompt, session_dir=workspace / SESSION_DIR, review_base_sha=None, thinking=thinking,
		adapter=adapter, timeout_minutes=CHALLENGE_TIMEOUT_MINUTES, prompt_name="challenge",
	)
	if turn.error:
		return CHALLENGE_NO_TURN, "challenger turn failed: " + build_app.single_line(build_app.redact(turn.error, 300))
	return 0, build_app.single_line(build_app.redact(turn.text, 300)) or "challenger finished"


def main() -> int:
	parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
	parser.add_argument("--workspace", required=True, type=Path)
	parser.add_argument("--spec", required=True, type=Path, help="The approved spec, with the read-back answers appended.")
	parser.add_argument("--changed-paths", type=Path, default=None, help="File with one changed path per line.")
	parser.add_argument("--setup-command", action="append", default=[], help="Repository setup command, run in order before the turn (the build's own flag).")
	parser.add_argument("--harness", choices=sorted(harness_adapters.ADAPTERS), default="pi", help="Coding agent to drive (see harness_adapters.py).")
	parser.add_argument("--thinking", choices=build_app.THINKING_LEVELS, default=None, help="Thinking level for this job.")
	args = parser.parse_args()
	adapter = harness_adapters.get(args.harness)
	adapter.prepare()
	code, line = run_challenge(
		args.workspace.resolve(), spec_path=args.spec.resolve(),
		changed_paths_file=args.changed_paths.resolve() if args.changed_paths else None, thinking=args.thinking, adapter=adapter, setup_commands=args.setup_command,
	)
	print(line)
	return code


if __name__ == "__main__":
	sys.exit(main())
