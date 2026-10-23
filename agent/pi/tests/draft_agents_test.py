"""Tests for the sandboxed AGENTS.md drafting harness."""

from __future__ import annotations

import importlib.util
import subprocess
from pathlib import Path
from types import SimpleNamespace


SCRIPTS = Path(__file__).resolve().parents[1] / "scripts"


def _load(name: str):
	spec = importlib.util.spec_from_file_location(name, SCRIPTS / f"{name}.py")
	module = importlib.util.module_from_spec(spec)
	assert spec.loader is not None
	spec.loader.exec_module(module)
	return module


draft_agents = _load("draft_agents")


def test_build_prompt_keeps_factory_instructions_and_repository_context(tmp_path: Path) -> None:
	(tmp_path / "README.md").write_text("# Example\n\nRun `make test`.\n", encoding="utf-8")
	(tmp_path / "CONTRIBUTING.md").write_text("Use the formatter.\n", encoding="utf-8")
	prompt = draft_agents.build_prompt("Write AGENTS.md exactly from evidence.", tmp_path, draft_agents.DRAFT_RELATIVE_PATH)
	assert "Write AGENTS.md exactly from evidence." in prompt
	assert "# Example" in prompt
	assert "Use the formatter." in prompt
	assert str(draft_agents.DRAFT_RELATIVE_PATH) in prompt
	assert "repository workspace containing" in prompt


def test_run_draft_writes_only_the_model_output(tmp_path: Path, monkeypatch) -> None:
	workspace = tmp_path / "repo"
	workspace.mkdir()
	instructions = tmp_path / "instructions.txt"
	instructions.write_text("Use only commands that work.\n", encoding="utf-8")
	out = tmp_path / "out" / "AGENTS.md"
	calls: list[list[str]] = []

	class Adapter:
		stderr_noise = ()

		def invocation(self, workspace, **kwargs):
			return ["fake-agent", kwargs["prompt"]]

		def parse(self, _text):
			return SimpleNamespace()

	monkeypatch.setattr(draft_agents.build_app, "sh", lambda command, **kwargs: _run_fake_agent(command, workspace, calls))
	adapter = Adapter()
	assert draft_agents.run_draft(workspace, instructions, out, 1, adapter=adapter) == 0
	assert out.read_text(encoding="utf-8") == "# AGENTS.md\n"
	assert any("Use only commands that work." in command[-1] for command in calls if command[0] == "fake-agent")


def _run_fake_agent(command: list[str], workspace: Path, calls: list[list[str]]) -> subprocess.CompletedProcess:
	calls.append(command)
	if command[0] == "fake-agent":
		draft = workspace / draft_agents.DRAFT_RELATIVE_PATH
		draft.parent.mkdir(parents=True, exist_ok=True)
		draft.write_text("# AGENTS.md\n", encoding="utf-8")
	return subprocess.CompletedProcess(command, 0, "", "")
