import contextlib
import importlib.util
import io
import json
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path
from unittest import mock

SCRIPT = Path(__file__).resolve().parents[1] / "scripts" / "challenge.py"
SPEC = importlib.util.spec_from_file_location("challenge", SCRIPT)
assert SPEC and SPEC.loader
challenge = importlib.util.module_from_spec(SPEC)
sys.modules[SPEC.name] = challenge
SPEC.loader.exec_module(challenge)
build_app = challenge.build_app

APPROVED = "# Widget\n\n## Acceptance criteria\n\n1. The widget is rounded to two places.\n2. It never panics.\n"


def answer(text: str) -> subprocess.CompletedProcess:
	return subprocess.CompletedProcess([], 0, json.dumps({"type": "message_end", "message": {"role": "assistant", "content": text}}), "")


def failed(message: str) -> subprocess.CompletedProcess:
	return subprocess.CompletedProcess([], 0, json.dumps({"type": "message_end", "message": {"role": "assistant", "stopReason": "error", "errorMessage": message}}), "")


class ChallengePromptTests(unittest.TestCase):
	def test_the_prompt_carries_the_spec_the_paths_and_the_header_rule_and_nothing_else(self):
		prompt = challenge.challenge_prompt(APPROVED, ["internal/widget/widget.go", "docs/widget.md"])
		for want in ("1. The widget is rounded to two places.", "- internal/widget/widget.go", "at most 5 new test files", "buildgate-challenge: AC-2", "buildgate-challenge: answer-1", "Do not modify any existing file"):
			self.assertIn(want, prompt)
		for gone in ("handoff", "finding", "earlier attempt", "session"):
			self.assertNotIn(gone, prompt.lower().replace("test files", ""))

	def test_a_long_path_list_is_cut_with_a_count(self):
		paths = [f"p{n}.go" for n in range(challenge.MAX_CHANGED_PATHS + 7)]
		prompt = challenge.challenge_prompt(APPROVED, paths)
		self.assertIn("... and 7 more", prompt)
		self.assertNotIn(f"p{challenge.MAX_CHANGED_PATHS}.go", prompt)


class RunChallengeTests(unittest.TestCase):
	def _run(self, completed, **kwargs):
		with tempfile.TemporaryDirectory() as directory, tempfile.TemporaryDirectory() as inputs:
			root = Path(directory)
			spec = Path(inputs) / "spec.md"
			spec.write_text(APPROVED)
			paths = Path(inputs) / "paths.txt"
			paths.write_text("a.go\nb.go\n")
			with mock.patch.object(build_app, "sh", return_value=completed) as sh:
				code, line = challenge.run_challenge(root, spec_path=spec, changed_paths_file=paths, **kwargs)
			return code, line, sh

	def test_a_turn_that_answered_exits_zero_and_sends_the_one_prompt(self):
		code, line, sh = self._run(answer("left 2 files"))
		self.assertEqual(code, 0)
		self.assertEqual(line, "left 2 files")
		self.assertEqual(sh.call_count, 1)
		self.assertIn("- a.go", sh.call_args.args[0][-1])

	def test_a_failed_turn_is_no_turn(self):
		code, line, _ = self._run(failed("429 token budget exceeded"))
		self.assertEqual(code, challenge.CHALLENGE_NO_TURN)
		self.assertIn("429", line)

	def test_a_failed_setup_command_ends_the_job_before_any_model_call(self):
		with tempfile.TemporaryDirectory() as directory, tempfile.TemporaryDirectory() as inputs:
			spec = Path(inputs) / "spec.md"
			spec.write_text(APPROVED)
			with mock.patch.object(build_app, "run_setup", return_value=("make deps", "boom")), mock.patch.object(build_app, "sh") as sh:
				code, line = challenge.run_challenge(Path(directory), spec_path=spec, setup_commands=["make deps"])
			self.assertEqual(code, build_app.SETUP_FAILED_EXIT_CODE)
			self.assertIn("make deps", line)
			sh.assert_not_called()

	def test_setup_commands_run_before_the_turn(self):
		with tempfile.TemporaryDirectory() as directory, tempfile.TemporaryDirectory() as inputs:
			spec = Path(inputs) / "spec.md"
			spec.write_text(APPROVED)
			with mock.patch.object(build_app, "run_setup", return_value=(None, "")) as setup, mock.patch.object(build_app, "sh", return_value=answer("ok")):
				code, _ = challenge.run_challenge(Path(directory), spec_path=spec, setup_commands=["a", "b"])
			self.assertEqual(code, 0)
			self.assertEqual(setup.call_args.args[1], ["a", "b"])

	def test_main_exit_code_is_the_runs(self):
		with tempfile.TemporaryDirectory() as directory, tempfile.TemporaryDirectory() as inputs:
			spec = Path(inputs) / "spec.md"
			spec.write_text(APPROVED)
			argv = [str(SCRIPT), "--workspace", directory, "--spec", str(spec)]
			with mock.patch.object(sys, "argv", argv), mock.patch.object(build_app, "sh", return_value=answer("ok")), contextlib.redirect_stdout(io.StringIO()):
				self.assertEqual(challenge.main(), 0)


if __name__ == "__main__":
	unittest.main()
