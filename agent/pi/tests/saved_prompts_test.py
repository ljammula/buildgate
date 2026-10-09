import contextlib
import io
import os
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path
from unittest import mock

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "scripts"))
import build_app  # noqa: E402
import saved_prompts  # noqa: E402


def read_saved(session: Path) -> dict:
	return {f.name: f.read_text() for f in (session / "prompts").glob("*.md")}


class SavePromptTests(unittest.TestCase):
	def test_writes_the_text_exactly_with_mode_0600(self):
		with tempfile.TemporaryDirectory() as d:
			text = "line one\né tail  \n"
			path = saved_prompts.save_prompt(Path(d) / "s", "build-round-1", text)
			self.assertEqual(path, Path(d) / "s" / "prompts" / "build-round-1.md")
			self.assertEqual(path.read_text(encoding="utf-8"), text)
			self.assertEqual(path.stat().st_mode & 0o777, 0o600)

	def test_a_second_save_of_a_name_is_numbered_not_overwritten(self):
		with tempfile.TemporaryDirectory() as d:
			session = Path(d)
			saved_prompts.save_prompt(session, "build-round-1", "first")
			saved_prompts.save_prompt(session, "build-round-1", "second")
			self.assertEqual(read_saved(session), {"build-round-1.md": "first", "build-round-1-2.md": "second"})

	def test_an_oversize_prompt_is_cut_with_a_final_line_saying_how_much(self):
		with tempfile.TemporaryDirectory() as d:
			path = saved_prompts.save_prompt(Path(d), "big", "a" * (saved_prompts.MAX_PROMPT_BYTES + 10))
			data = path.read_bytes()
			self.assertTrue(data.endswith(b"[saved prompt cut: 10 bytes not saved]\n"))
			self.assertLess(len(data), saved_prompts.MAX_PROMPT_BYTES + 100)

	def test_unencodable_text_is_replaced_never_raised(self):
		with tempfile.TemporaryDirectory() as d:
			path = saved_prompts.save_prompt(Path(d), "odd", "a\ud800b")
			self.assertEqual(path.read_text(encoding="utf-8"), "a?b")

	def test_the_fifty_first_prompt_is_not_saved(self):
		with tempfile.TemporaryDirectory() as d:
			out = io.StringIO()
			with contextlib.redirect_stdout(out):
				for i in range(51):
					saved_prompts.save_prompt(Path(d), f"p-{i}", "x")
			self.assertEqual(len(read_saved(Path(d))), saved_prompts.MAX_PROMPT_FILES)
			self.assertIn("prompt not saved", out.getvalue())

	def test_a_bad_name_or_a_link_saves_nothing_and_notes_only_the_class_name(self):
		with tempfile.TemporaryDirectory() as d:
			session = Path(d) / "s"
			session.mkdir()
			target = Path(d) / "elsewhere"
			target.mkdir()
			(session / "prompts").symlink_to(target)
			out = io.StringIO()
			with contextlib.redirect_stdout(out):
				self.assertIsNone(saved_prompts.save_prompt(session, "ok", "SECRET-TEXT"))
				self.assertIsNone(saved_prompts.save_prompt(Path(d), "../bad", "SECRET-TEXT"))
			self.assertEqual(list(target.iterdir()), [])
			self.assertNotIn("SECRET-TEXT", out.getvalue())
			self.assertIn("OSError", out.getvalue())
			self.assertIn("ValueError", out.getvalue())


class ReviewTurnSavesPromptTests(unittest.TestCase):
	def test_the_saved_prompt_is_the_one_the_adapter_received(self):
		completed = subprocess.CompletedProcess([], 0, "", "")
		with tempfile.TemporaryDirectory() as d, \
				mock.patch.object(build_app, "sh", return_value=completed), \
				mock.patch.object(build_app.DEFAULT_ADAPTER.__class__, "invocation", autospec=True, return_value=["pi"]) as spy:
			session = Path(d) / ".s"
			build_app.run_review_turn(
				Path(d), prompt="PROMPT\nBODY", session_dir=session, review_base_sha=None, thinking=None,
				prompt_name="review-code",
			)
			self.assertEqual(spy.call_args.kwargs["prompt"], "PROMPT\nBODY")
			self.assertEqual(read_saved(session), {"review-code.md": "PROMPT\nBODY"})

	def test_a_failing_save_does_not_fail_the_turn(self):
		completed = subprocess.CompletedProcess([], 0, "", "")
		with tempfile.TemporaryDirectory() as d, mock.patch.object(build_app, "sh", return_value=completed) as sh:
			blocker = Path(d) / ".s"
			blocker.write_text("a file where the session folder should be")
			turn = build_app.run_review_turn(
				Path(d), prompt="p", session_dir=blocker, review_base_sha=None, thinking=None, prompt_name="review-code",
			)
			sh.assert_called_once()
			self.assertEqual(turn.error, "")


class ReviewScriptsSavePromptTests(unittest.TestCase):
	"""Each review script saves what its adapter received, in its own session folder."""

	def _spy(self):
		return mock.patch.object(build_app.DEFAULT_ADAPTER.__class__, "invocation", autospec=True, return_value=["pi"])

	def _init(self, root: Path) -> None:
		for args in (["init"], ["config", "user.email", "t@t"], ["config", "user.name", "t"], ["config", "commit.gpgsign", "false"]):
			subprocess.run(["git", *args], cwd=root, check=True, capture_output=True)
		(root / "seed.txt").write_text("seed\n")
		subprocess.run(["git", "add", "."], cwd=root, check=True, capture_output=True)
		subprocess.run(["git", "commit", "-m", "init"], cwd=root, check=True, capture_output=True)

	def _check(self, run, session_name, saved_name):
		import code_review, combined_review, conformity_review  # noqa: F401
		with tempfile.TemporaryDirectory() as d, tempfile.TemporaryDirectory() as inputs:
			root = Path(d)
			self._init(root)
			spec = Path(inputs) / "spec.md"
			spec.write_text("Add a widget.\n")
			criteria = Path(inputs) / "criteria.md"
			criteria.write_text("1. Widget exists\n")
			completed = subprocess.CompletedProcess([], 0, "", "")
			with self._spy() as spy, mock.patch.object(build_app, "sh", return_value=completed):
				run(root, spec, criteria)
			sent = spy.call_args.kwargs["prompt"]
			self.assertEqual(read_saved(root / session_name), {saved_name + ".md": sent})

	def test_code_review(self):
		import code_review
		self._check(lambda root, spec, criteria: code_review.run_code_review(root, spec_path=spec), ".pi-code-review-session", "review-code")

	def test_conformity_review(self):
		import conformity_review
		self._check(lambda root, spec, criteria: conformity_review.run_conformity_review(root, criteria_path=criteria, conformity_policy="required"), ".pi-conformity-session", "review-conformity")

	def test_combined_review(self):
		import combined_review
		self._check(
			lambda root, spec, criteria: combined_review.run_combined_review(root, criteria_path=criteria, spec_path=spec, conformity_policy="required", review_policy="required"),
			".pi-combined-review-session", "review-combined",
		)


if __name__ == "__main__":
	unittest.main()
