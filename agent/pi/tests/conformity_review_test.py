import contextlib
import io
import importlib.util
import json
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path
from unittest import mock

SCRIPT = Path(__file__).resolve().parents[1] / "scripts" / "conformity_review.py"
SPEC = importlib.util.spec_from_file_location("conformity_review", SCRIPT)
assert SPEC and SPEC.loader
conformity_review = importlib.util.module_from_spec(SPEC)
sys.modules[SPEC.name] = conformity_review
SPEC.loader.exec_module(conformity_review)

build_app = conformity_review.build_app


def init_repo_with_commit(path: Path) -> None:
	run = lambda *args: subprocess.run(["git", *args], cwd=path, check=True, capture_output=True)
	run("init")
	run("config", "user.email", "test@test")
	run("config", "user.name", "test")
	run("config", "commit.gpgsign", "false")
	(path / "seed.txt").write_text("seed\n")
	run("add", "seed.txt")
	run("commit", "-m", "init")


def conformity_output(verdicts: list[dict]) -> str:
	payload = json.dumps({"criteria": verdicts})
	return json.dumps({"type": "message_end", "message": {"role": "assistant", "content": payload}})


class RunConformityReviewTests(unittest.TestCase):
	def test_required_policy_fails_on_flagged_criterion_and_never_touches_git(self):
		with tempfile.TemporaryDirectory() as directory, tempfile.TemporaryDirectory() as spec_dir:
			root = Path(directory)
			init_repo_with_commit(root)
			criteria_path = Path(spec_dir) / "criteria.md"
			criteria_path.write_text("1. Widget exists\n2. Widget is documented\n")
			completed = subprocess.CompletedProcess([], 0, conformity_output([
				{"criterion": "1. Widget exists", "verdict": "clean"},
				{"criterion": "2. Widget is documented", "verdict": "flagged", "detail": "no docs"},
			]), "")
			git_calls = []

			def side_effect(args, **kwargs):
				if args and args[0] == "git":
					git_calls.append(args[1])
					return subprocess.run(args, capture_output=True, text=True, cwd=kwargs.get("cwd"))
				return completed

			with mock.patch.object(build_app, "sh", side_effect=side_effect):
				evidence = conformity_review.run_conformity_review(
					root, criteria_path=criteria_path, conformity_policy="required",
				)
			self.assertFalse(evidence["succeeded"])
			self.assertIn("2. Widget is documented", evidence["stopped_reason"])
			self.assertEqual(evidence["review_verdicts"][0]["verdict"], "clean")
			self.assertEqual(evidence["review_verdicts"][1]["verdict"], "flagged")
			self.assertNotIn("add", git_calls)
			self.assertNotIn("commit", git_calls)

	def test_advisory_policy_records_but_does_not_fail(self):
		with tempfile.TemporaryDirectory() as directory, tempfile.TemporaryDirectory() as spec_dir:
			root = Path(directory)
			init_repo_with_commit(root)
			criteria_path = Path(spec_dir) / "criteria.md"
			criteria_path.write_text("1. Widget exists\n")
			completed = subprocess.CompletedProcess([], 0, conformity_output([
				{"criterion": "1. Widget exists", "verdict": "flagged", "detail": "still missing"},
			]), "")
			with mock.patch.object(build_app, "sh", return_value=completed):
				evidence = conformity_review.run_conformity_review(
					root, criteria_path=criteria_path, conformity_policy="advisory", thinking="max",
				)
			self.assertTrue(evidence["succeeded"])
			self.assertEqual(evidence["review_verdicts"][0]["verdict"], "flagged")
			self.assertEqual(evidence["thinking"], "max")

	def test_relay_429_on_the_final_turn_is_named_in_stopped_reason_and_evidence(self):
		# Live example-app run 3, 2026-09-28: the conformity reviewer, asked to
		# diff the workspace itself, burned the relay's whole hourly token
		# budget across 30 tool-exploring turns (1,011,032 input tokens)
		# until the sliding-window limiter answered 429 "token budget
		# exceeded" three times in a row; the reviewer ended with no final
		# text and every criterion read "unavailable"/"no-review-verdict"
		# with the actual 429 nowhere in CONFORMITY_EVIDENCE.json. Same
		# fix/test shape as code_review_test.py's own
		# test_relay_429_on_the_final_turn_is_named_in_stopped_reason_and_evidence.
		with tempfile.TemporaryDirectory() as directory, tempfile.TemporaryDirectory() as spec_dir:
			root = Path(directory)
			init_repo_with_commit(root)
			criteria_path = Path(spec_dir) / "criteria.md"
			criteria_path.write_text("1. Widget exists\n2. Widget is documented\n")
			error_output = json.dumps({
				"type": "message_end",
				"message": {"role": "assistant", "stopReason": "error", "errorMessage": '429 "token budget exceeded"'},
			})
			completed = subprocess.CompletedProcess([], 0, error_output, "")
			with mock.patch.object(build_app, "sh", return_value=completed):
				evidence = conformity_review.run_conformity_review(
					root, criteria_path=criteria_path, conformity_policy="required",
				)
			self.assertFalse(evidence["succeeded"])
			self.assertTrue(all(v["verdict"] == "unavailable" for v in evidence["review_verdicts"]))
			self.assertIn("429", evidence["stopped_reason"])
			self.assertIn("token budget exceeded", evidence["stopped_reason"])
			self.assertIn("429", evidence["error"])

	def test_all_clean_succeeds(self):
		with tempfile.TemporaryDirectory() as directory, tempfile.TemporaryDirectory() as spec_dir:
			root = Path(directory)
			init_repo_with_commit(root)
			criteria_path = Path(spec_dir) / "criteria.md"
			criteria_path.write_text("1. Widget exists\n")
			completed = subprocess.CompletedProcess([], 0, conformity_output([
				{"criterion": "1. Widget exists", "verdict": "clean"},
			]), "")
			with mock.patch.object(build_app, "sh", return_value=completed):
				evidence = conformity_review.run_conformity_review(
					root, criteria_path=criteria_path, conformity_policy="required",
				)
			self.assertTrue(evidence["succeeded"])
			self.assertEqual(evidence["stopped_reason"], "spec conformity review cleared every criterion")
			self.assertIsNone(evidence["thinking"])
			self.assertEqual(evidence["error"], "")

	def test_run_conformity_review_puts_thinking_in_the_real_pi_argv(self):
		# Goes through conformity_review.run_conformity_review and the real
		# build_app.run_spec_conformity_review/pi_invocation; only build_app.sh
		# is faked, so dropping thinking at either layer fails this test.
		with tempfile.TemporaryDirectory() as directory:
			workspace = Path(directory) / "repo"
			workspace.mkdir()
			criteria_path = Path(directory) / "criteria.md"
			criteria_path.write_text("1. Widget exists\n")
			for thinking in ("max", None):
				commands = []
				completed_output = conformity_output([{"criterion": "1. Widget exists", "verdict": "clean"}])

				def fake_sh(args, cwd=None, timeout=None, env=None):
					commands.append(args)
					return subprocess.CompletedProcess(args, 0, completed_output, "")

				with self.subTest(thinking=thinking), mock.patch.object(build_app, "sh", side_effect=fake_sh):
					evidence = conformity_review.run_conformity_review(
						workspace, criteria_path=criteria_path, thinking=thinking,
					)
					self.assertTrue(evidence["succeeded"])
					pi_commands = [c for c in commands if "--print" in c]
					self.assertEqual(len(pi_commands), 1)
					if thinking is None:
						self.assertNotIn("--thinking", pi_commands[0])
					else:
						self.assertEqual(pi_commands[0][pi_commands[0].index("--thinking") + 1], "max")

	def test_main_thinking_option_accepts_every_level_and_rejects_unknown(self):
		base_argv = [
			str(SCRIPT), "--workspace", "/tmp/work", "--spec-acceptance-criteria", "/tmp/criteria.md",
		]
		levels = build_app.THINKING_LEVELS
		for level in (*levels, None):
			with self.subTest(level=level):
				args = base_argv + ([] if level is None else ["--thinking", level])
				evidence = {"succeeded": True, "stopped_reason": "ok"}
				with (
					mock.patch.object(sys, "argv", args),
					mock.patch.object(conformity_review, "run_conformity_review", return_value=evidence) as run_review,
					mock.patch.object(conformity_review, "write_conformity_evidence_json", return_value=Path("/tmp/evidence.json")),
				):
					self.assertEqual(conformity_review.main(), 0)
				self.assertEqual(run_review.call_args.kwargs["thinking"], level)

		with mock.patch.object(sys, "argv", base_argv + ["--thinking", "unknown"]):
			with contextlib.redirect_stderr(io.StringIO()):
				with self.assertRaises(SystemExit) as raised:
					conformity_review.main()
		self.assertEqual(raised.exception.code, 2)


class WriteConformityEvidenceJSONTests(unittest.TestCase):
	def test_writes_a_separate_file_from_build_evidence(self):
		with tempfile.TemporaryDirectory() as directory:
			root = Path(directory)
			evidence = {"schema_version": 1, "succeeded": True, "review_verdicts": []}
			path = conformity_review.write_conformity_evidence_json(root, evidence)
			self.assertEqual(path.name, "CONFORMITY_EVIDENCE.json")
			self.assertNotEqual(path.name, "BUILD_EVIDENCE.json")
			self.assertEqual(json.loads(path.read_text()), evidence)


class SpecConformityPromptTest(unittest.TestCase):
	# Found live (2026-09-19, example-app mood tickets): a criterion like "make
	# verify passes" was flagged unverifiable because the review sandbox has
	# no network/module cache, quarantining runs whose canonical gate passed.
	def test_prompt_tells_reviewer_not_to_flag_unrunnable_verify_commands(self):
		prompt = build_app.spec_conformity_prompt(["1. `make verify` passes."], "abc123")
		self.assertIn("cannot run to completion here", prompt)
		self.assertIn("answer \"clean\" unless the diff itself shows a concrete defect", prompt)
		self.assertIn("1. `make verify` passes.", prompt)
		self.assertIn("abc123", prompt)


if __name__ == "__main__":
	unittest.main()
