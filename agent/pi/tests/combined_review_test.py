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

SCRIPT = Path(__file__).resolve().parents[1] / "scripts" / "combined_review.py"
SPEC = importlib.util.spec_from_file_location("combined_review", SCRIPT)
assert SPEC and SPEC.loader
combined_review = importlib.util.module_from_spec(SPEC)
sys.modules[SPEC.name] = combined_review
SPEC.loader.exec_module(combined_review)

build_app = combined_review.build_app
code_review = combined_review.code_review
conformity_review = combined_review.conformity_review


def init_repo_with_commit(path: Path) -> None:
	run = lambda *args: subprocess.run(["git", *args], cwd=path, check=True, capture_output=True)
	run("init")
	run("config", "user.email", "test@test")
	run("config", "user.name", "test")
	run("config", "commit.gpgsign", "false")
	(path / "seed.txt").write_text("seed\n")
	run("add", "seed.txt")
	run("commit", "-m", "init")


def combined_output(criteria: list[dict], findings: list[dict]) -> str:
	payload = json.dumps({"criteria": criteria, "findings": findings})
	return json.dumps({"type": "message_end", "message": {"role": "assistant", "content": payload}})


class CombinedReviewPromptTests(unittest.TestCase):
	def test_diff_comes_before_the_ticket_spec_and_both_task_rule_sets_are_present(self):
		prompt = combined_review.combined_review_prompt(
			["1. Widget exists"], "Add a widget to the app.", "abc123",
			diff=("stat text", "diff text"),
		)
		diff_index = prompt.index("diff text")
		spec_index = prompt.index("Add a widget to the app.")
		self.assertLess(diff_index, spec_index, "diff block must precede the ticket spec")

		# Task A: every existing conformity rule.
		self.assertIn("1. Widget exists", prompt)
		self.assertIn("cannot run to completion here", prompt)  # command-outcome rule
		self.assertIn("Pure formatting or lint drift", prompt)  # formatting rule
		self.assertIn("using each criterion's exact text", prompt)  # exact-echo instruction

		# Task B: every existing code-review rule.
		self.assertIn("Report only concrete correctness, security, data-loss", prompt)
		self.assertIn("\"high\" is a defect a reviewer would block the merge for", prompt)
		self.assertIn("IMPORTANT rule for command-outcome findings", prompt)

		# One JSON contract covering both tasks.
		self.assertIn('"criteria"', prompt)
		self.assertIn('"findings"', prompt)
		self.assertEqual(prompt.count("Respond with ONLY a single JSON object"), 1)

	def test_falls_back_to_instruction_only_diff_when_diff_is_none(self):
		prompt = combined_review.combined_review_prompt(["1. Widget exists"], "spec text", "abc123", diff=None)
		self.assertIn("Diff the workspace against abc123", prompt)


class RunCombinedReviewTests(unittest.TestCase):
	def _workspace_and_files(self, directory: str, spec_dir: str):
		root = Path(directory)
		init_repo_with_commit(root)
		criteria_path = Path(spec_dir) / "criteria.md"
		criteria_path.write_text("1. Widget exists\n2. Widget is documented\n")
		spec_path = Path(spec_dir) / "ticket.md"
		spec_path.write_text("Add a widget.\n")
		return root, criteria_path, spec_path

	def test_one_turn_yields_both_evidences_and_parsing_matches_the_standalone_scripts(self):
		with tempfile.TemporaryDirectory() as directory, tempfile.TemporaryDirectory() as spec_dir:
			root, criteria_path, spec_path = self._workspace_and_files(directory, spec_dir)
			completed = subprocess.CompletedProcess([], 0, combined_output(
				criteria=[
					{"criterion": "1. Widget exists", "verdict": "clean"},
					{"criterion": "2. Widget is documented", "verdict": "flagged", "detail": "no docs"},
				],
				findings=[
					{"severity": "high", "file": "a.go", "line": 12, "summary": "nil deref",
					 "failure_scenario": "calling Foo(nil) panics"},
				],
			), "")
			git_calls = []

			def side_effect(args, **kwargs):
				if args and args[0] == "git":
					git_calls.append(args[1])
					return subprocess.run(args, capture_output=True, text=True, cwd=kwargs.get("cwd"))
				return completed

			with mock.patch.object(build_app, "sh", side_effect=side_effect):
				conformity, review = combined_review.run_combined_review(
					root, criteria_path=criteria_path, spec_path=spec_path,
					conformity_policy="required", review_policy="required",
				)

			self.assertNotIn("add", git_calls)
			self.assertNotIn("commit", git_calls)

			self.assertFalse(conformity["succeeded"])
			self.assertIn("2. Widget is documented", conformity["stopped_reason"])
			self.assertEqual(conformity["review_verdicts"][0]["verdict"], "clean")
			self.assertEqual(conformity["review_verdicts"][1]["verdict"], "flagged")

			self.assertFalse(review["succeeded"])
			self.assertIn("1 blocking high-severity", review["stopped_reason"])
			self.assertEqual(review["findings"][0]["summary"], "nil deref")

	def test_exit_bits_cover_all_four_pass_fail_combinations(self):
		cases = [
			# (conformity verdict, finding severity, expected exit code)
			("clean", None, 0),
			("flagged", None, 1),
			("clean", "high", 2),
			("flagged", "high", 3),
		]
		for conformity_verdict, severity, expected_exit in cases:
			with self.subTest(conformity_verdict=conformity_verdict, severity=severity):
				with tempfile.TemporaryDirectory() as directory, tempfile.TemporaryDirectory() as spec_dir:
					root, criteria_path, spec_path = self._workspace_and_files(directory, spec_dir)
					findings = []
					if severity is not None:
						findings.append({
							"severity": severity, "file": "a.go", "line": 1, "summary": "bug",
							"failure_scenario": "triggers on nil",
						})
					completed = subprocess.CompletedProcess([], 0, combined_output(
						criteria=[
							{"criterion": "1. Widget exists", "verdict": conformity_verdict},
							{"criterion": "2. Widget is documented", "verdict": "clean"},
						],
						findings=findings,
					), "")
					with mock.patch.object(build_app, "sh", return_value=completed):
						conformity, review = combined_review.run_combined_review(
							root, criteria_path=criteria_path, spec_path=spec_path,
							conformity_policy="required", review_policy="required",
						)
					exit_code = combined_review.COMBINED_EXIT_BASE + ((0 if conformity["succeeded"] else 1) | (0 if review["succeeded"] else 2))
					self.assertEqual(exit_code, combined_review.COMBINED_EXIT_BASE + expected_exit)

	def test_advisory_policies_never_fail_either_review(self):
		with tempfile.TemporaryDirectory() as directory, tempfile.TemporaryDirectory() as spec_dir:
			root, criteria_path, spec_path = self._workspace_and_files(directory, spec_dir)
			completed = subprocess.CompletedProcess([], 0, combined_output(
				criteria=[
					{"criterion": "1. Widget exists", "verdict": "flagged", "detail": "missing"},
					{"criterion": "2. Widget is documented", "verdict": "clean"},
				],
				findings=[{"severity": "high", "file": "a.go", "line": 1, "summary": "bug", "failure_scenario": "x"}],
			), "")
			with mock.patch.object(build_app, "sh", return_value=completed):
				conformity, review = combined_review.run_combined_review(
					root, criteria_path=criteria_path, spec_path=spec_path,
					conformity_policy="advisory", review_policy="advisory",
				)
			self.assertTrue(conformity["succeeded"])
			self.assertTrue(review["succeeded"])

	def test_unavailable_turn_marks_both_reviews_unavailable_with_the_error_recorded(self):
		# Mirrors conformity_review_test.py's/code_review_test.py's own 429
		# incident test -- one relay 429 on the combined turn must leave
		# BOTH evidence files naming the cause, not just one of them.
		with tempfile.TemporaryDirectory() as directory, tempfile.TemporaryDirectory() as spec_dir:
			root, criteria_path, spec_path = self._workspace_and_files(directory, spec_dir)
			error_output = json.dumps({
				"type": "message_end",
				"message": {"role": "assistant", "stopReason": "error", "errorMessage": '429 "token budget exceeded"'},
			})
			completed = subprocess.CompletedProcess([], 0, error_output, "")
			with mock.patch.object(build_app, "sh", return_value=completed):
				conformity, review = combined_review.run_combined_review(
					root, criteria_path=criteria_path, spec_path=spec_path,
					conformity_policy="required", review_policy="required",
				)
			self.assertFalse(conformity["succeeded"])
			self.assertTrue(all(v["verdict"] == "unavailable" for v in conformity["review_verdicts"]))
			self.assertIn("429", conformity["stopped_reason"])

			self.assertFalse(review["succeeded"])
			self.assertFalse(review["available"])
			self.assertIn("429", review["stopped_reason"])
			self.assertEqual(review["findings"], [])


	def test_the_combined_turn_gets_twice_a_standalone_reviews_time_and_a_timeout_names_it(self):
		# One turn does both reviews' work. A turn that runs out of time
		# leaves every criterion unavailable, with the limit in the reason.
		with tempfile.TemporaryDirectory() as directory, tempfile.TemporaryDirectory() as spec_dir:
			root, criteria_path, spec_path = self._workspace_and_files(directory, spec_dir)
			timeouts = []

			def timing_out(command, *, cwd, timeout, env):
				timeouts.append(timeout)
				raise subprocess.TimeoutExpired(command, timeout)

			with mock.patch.object(build_app, "sh", side_effect=timing_out):
				conformity, review = combined_review.run_combined_review(
					root, criteria_path=criteria_path, spec_path=spec_path,
					conformity_policy="required", review_policy="required",
				)
			self.assertEqual(timeouts, [20 * 60])
			self.assertTrue(all(v["verdict"] == "unavailable" for v in conformity["review_verdicts"]))
			self.assertIn("timed out after 20 minutes", conformity["stopped_reason"])
			self.assertFalse(review["available"])
			self.assertIn("timed out after 20 minutes", review["stopped_reason"])


class WriteCombinedEvidenceTests(unittest.TestCase):
	def test_writes_both_evidence_files_in_their_existing_schema(self):
		with tempfile.TemporaryDirectory() as directory:
			root = Path(directory)
			conformity = conformity_review.conformity_evidence(
				[{"criterion": "1. Widget exists", "verdict": "clean", "detail": ""}],
				"", conformity_policy="required", thinking=None,
			)
			review = code_review.code_review_evidence(
				True, [], "", review_policy="required", thinking=None,
			)
			conformity_path, review_path = combined_review.write_combined_evidence(root, conformity, review)
			self.assertEqual(conformity_path.name, "CONFORMITY_EVIDENCE.json")
			self.assertEqual(review_path.name, "CODE_REVIEW_EVIDENCE.json")
			self.assertEqual(json.loads(conformity_path.read_text()), conformity)
			self.assertEqual(json.loads(review_path.read_text()), review)
			# Byte-compatible with each standalone script's own schema: the
			# same keys a Go golden test/evidence loader already expects.
			self.assertEqual(conformity["schema_version"], 1)
			self.assertIn("review_verdicts", conformity)
			self.assertEqual(review["schema_version"], 1)
			self.assertIn("findings", review)


class MainTests(unittest.TestCase):
	def test_exit_code_bits_and_thinking_flow_through_main(self):
		with tempfile.TemporaryDirectory() as directory, tempfile.TemporaryDirectory() as spec_dir:
			root = Path(directory)
			init_repo_with_commit(root)
			criteria_path = Path(spec_dir) / "criteria.md"
			criteria_path.write_text("1. Widget exists\n")
			spec_path = Path(spec_dir) / "ticket.md"
			spec_path.write_text("Add a widget.\n")
			argv = [
				str(SCRIPT), "--workspace", str(root), "--spec", str(spec_path),
				"--spec-acceptance-criteria", str(criteria_path), "--thinking", "max",
			]
			completed = subprocess.CompletedProcess([], 0, combined_output(
				criteria=[{"criterion": "1. Widget exists", "verdict": "flagged", "detail": "missing"}],
				findings=[{"severity": "high", "file": "a.go", "line": 1, "summary": "bug", "failure_scenario": "x"}],
			), "")
			with (
				mock.patch.object(sys, "argv", argv),
				mock.patch.object(build_app, "sh", return_value=completed),
			):
				exit_code = combined_review.main()
			self.assertEqual(exit_code, combined_review.COMBINED_EXIT_BASE + 3)
			self.assertEqual(
				json.loads((root / "CONFORMITY_EVIDENCE.json").read_text())["thinking"], "max",
			)
			self.assertEqual(
				json.loads((root / "CODE_REVIEW_EVIDENCE.json").read_text())["thinking"], "max",
			)

	def test_main_thinking_option_rejects_unknown(self):
		base_argv = [
			str(SCRIPT), "--workspace", "/tmp/work", "--spec", "/tmp/ticket.md",
			"--spec-acceptance-criteria", "/tmp/criteria.md",
		]
		with mock.patch.object(sys, "argv", base_argv + ["--thinking", "unknown"]):
			with contextlib.redirect_stderr(io.StringIO()):
				with self.assertRaises(SystemExit) as raised:
					combined_review.main()
		self.assertEqual(raised.exception.code, 2)


if __name__ == "__main__":
	unittest.main()
