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
		self.assertIn("git's guess at the enclosing function or heading", prompt)

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

			ticks = iter([0.0, 20 * 60.0])
			with (
				mock.patch.object(build_app, "sh", side_effect=timing_out),
				mock.patch.object(combined_review, "clock", side_effect=lambda: next(ticks)),
			):
				conformity, review = combined_review.run_combined_review(
					root, criteria_path=criteria_path, spec_path=spec_path,
					conformity_policy="required", review_policy="required",
				)
			# Nothing is left of the review's time, so nothing is asked again.
			self.assertEqual(timeouts, [20 * 60])
			self.assertIn("not asked again", review["retry"])
			self.assertTrue(all(v["verdict"] == "unavailable" for v in conformity["review_verdicts"]))
			self.assertIn("timed out after 20 minutes", conformity["stopped_reason"])
			self.assertFalse(review["available"])
			self.assertIn("timed out after 20 minutes", review["stopped_reason"])


def answer(payload) -> subprocess.CompletedProcess:
	text = payload if isinstance(payload, str) else json.dumps(payload)
	return subprocess.CompletedProcess([], 0, json.dumps({"type": "message_end", "message": {"role": "assistant", "content": text}}), "")


def failed_turn(message: str) -> subprocess.CompletedProcess:
	return subprocess.CompletedProcess([], 0, json.dumps({
		"type": "message_end", "message": {"role": "assistant", "stopReason": "error", "errorMessage": message},
	}), "")


VERDICTS = [
	{"criterion": "1. Widget exists", "verdict": "clean"},
	{"criterion": "2. Widget is documented", "verdict": "flagged", "detail": "no docs"},
]
FINDINGS = [{"severity": "high", "file": "a.go", "line": 12, "summary": "nil deref", "failure_scenario": "Foo(nil) panics"}]


class CombinedReviewRetryTests(unittest.TestCase):
	"""An answer that lacks a part is asked for once more: the missing review
	alone, or the combined turn again when nothing was usable."""

	def _run(self, answers, *, thinking="high", clock=None, base=True):
		"""Runs the review with the model turns answered in order by answers
		(a CompletedProcess, or an exception to raise). Returns (conformity,
		review, turns, root): turns holds, per model call, its prompt, session
		folder, timeout, thinking level and review base."""
		directory = tempfile.TemporaryDirectory()
		spec_dir = tempfile.TemporaryDirectory()
		self.addCleanup(directory.cleanup)
		self.addCleanup(spec_dir.cleanup)
		root = Path(directory.name)
		init_repo_with_commit(root)
		base_sha = subprocess.run(["git", "rev-parse", "HEAD"], cwd=root, check=True, capture_output=True, text=True).stdout.strip()
		(root / "seed.txt").write_text("seed\nchanged by the build\n")
		criteria_path = Path(spec_dir.name) / "criteria.md"
		criteria_path.write_text("1. Widget exists\n2. Widget is documented\n")
		spec_path = Path(spec_dir.name) / "ticket.md"
		spec_path.write_text("Add a widget.\n")
		self.base_sha = base_sha if base else None
		self.diff = build_app.workspace_diff(root, base_sha) if base else None
		queue = list(answers)
		turns = []

		def fake_sh(command, *, cwd, timeout, env):
			session = Path(command[command.index("--session-dir") + 1])
			turns.append({
				"prompt": command[-1], "session": session, "timeout": timeout,
				"thinking": command[command.index("--thinking") + 1],
				"base": env.get("AI_REVIEW_BASE_SHA"),
				"session_was_empty": not session.exists() or not [p for p in session.iterdir() if p.name != "prompts"],
				"first_session": sorted(p.name for p in (root / ".pi-combined-review-session").iterdir()) if (root / ".pi-combined-review-session").is_dir() else [],
				"evidence": {
					name: json.loads((root / name).read_text())
					for name in ("CONFORMITY_EVIDENCE.json", "CODE_REVIEW_EVIDENCE.json") if (root / name).exists()
				},
			})
			# What a harness leaves behind: session state the next turn must not find.
			session.mkdir(parents=True, exist_ok=True)
			(session / "state.jsonl").write_text("turn %d\n" % len(turns))
			(session / "codex-home").mkdir(exist_ok=True)
			(session / "codex-home" / "transcript.jsonl").write_text("the answer of turn %d\n" % len(turns))
			# A reviewer that edits the workspace must not change the retry's diff.
			(root / "seed.txt").write_text("seed\nrewritten by reviewer turn %d\n" % len(turns))
			result = queue.pop(0)
			if isinstance(result, BaseException):
				raise result
			return result

		patches = [mock.patch.object(build_app, "sh", side_effect=fake_sh)]
		if clock is not None:
			patches.append(mock.patch.object(combined_review, "clock", side_effect=clock))
		with contextlib.ExitStack() as stack, contextlib.redirect_stdout(io.StringIO()):
			for patch in patches:
				stack.enter_context(patch)
			conformity, review = combined_review.run_combined_review(
				root, criteria_path=criteria_path, spec_path=spec_path,
				conformity_policy="required", review_policy="required",
				review_base_sha=base_sha if base else None, thinking=thinking,
			)
		self.assertEqual(queue, [], "every prepared answer is one model call")
		return conformity, review, turns, root

	def _saved_prompts(self, root: Path) -> dict[str, str]:
		folder = root / ".pi-combined-review-session" / "prompts"
		return {p.name: p.read_text() for p in sorted(folder.iterdir())}

	def test_a_whole_first_answer_makes_exactly_one_model_call_and_records_no_retry(self):
		conformity, review, turns, root = self._run([answer({"criteria": VERDICTS, "findings": FINDINGS})])
		self.assertEqual(len(turns), 1)
		self.assertNotIn("retry", conformity)
		self.assertNotIn("retry", review)
		self.assertEqual(list(self._saved_prompts(root)), ["review-combined.md"])
		self.assertFalse((root / ".pi-combined-review-session" / "retry").exists())

	def test_a_missing_findings_list_is_asked_for_alone_and_the_verdicts_are_kept(self):
		conformity, review, turns, root = self._run([
			answer({"criteria": VERDICTS}),
			answer({"findings": FINDINGS}),
		])
		self.assertEqual(len(turns), 2)
		# The conformity half is the first answer's, untouched.
		self.assertEqual([v["verdict"] for v in conformity["review_verdicts"]], ["clean", "flagged"])
		self.assertFalse(conformity["succeeded"])
		self.assertIn("2. Widget is documented", conformity["stopped_reason"])
		self.assertEqual(conformity["error"], "")
		# The code review half is the retry's.
		self.assertTrue(review["available"])
		self.assertEqual(review["findings"][0]["summary"], "nil deref")
		self.assertIn("1 blocking high-severity", review["stopped_reason"])
		# The retry is the standalone code review's own prompt over the first turn's diff.
		prompts = self._saved_prompts(root)
		self.assertEqual(list(prompts), ["review-code-retry.md", "review-combined.md"])
		self.assertEqual(turns[1]["prompt"], prompts["review-code-retry.md"])
		self.assertEqual(turns[1]["prompt"], code_review.code_review_prompt("Add a widget.\n", self.base_sha, diff=self.diff))
		self.assertIn("changed by the build", turns[1]["prompt"])
		self.assertNotIn("rewritten by reviewer", turns[1]["prompt"])
		self.assertNotIn("1. Widget exists", turns[1]["prompt"], "the code review prompt carries no criteria")
		self.assertEqual(turns[1]["timeout"], 10 * 60)
		for evidence in (conformity, review):
			self.assertIn("asked once more for the code review alone", evidence["retry"])
			self.assertIn("no parseable findings list", evidence["retry"])

	def test_missing_verdicts_are_asked_for_alone_and_the_findings_are_kept(self):
		conformity, review, turns, root = self._run([
			answer({"findings": FINDINGS}),
			answer({"criteria": VERDICTS}),
		])
		self.assertEqual(len(turns), 2)
		self.assertEqual(review["findings"][0]["summary"], "nil deref")
		self.assertTrue(review["available"])
		self.assertEqual([v["verdict"] for v in conformity["review_verdicts"]], ["clean", "flagged"])
		self.assertIn("2. Widget is documented", conformity["stopped_reason"])
		prompts = self._saved_prompts(root)
		self.assertEqual(list(prompts), ["review-combined.md", "review-conformity-retry.md"])
		self.assertEqual(turns[1]["prompt"], prompts["review-conformity-retry.md"])
		self.assertEqual(turns[1]["prompt"], build_app.spec_conformity_prompt(
			["1. Widget exists", "2. Widget is documented"], self.base_sha, diff=self.diff,
		))
		self.assertIn("1. Widget exists", turns[1]["prompt"])
		self.assertIn("changed by the build", turns[1]["prompt"])
		self.assertNotIn("rewritten by reviewer", turns[1]["prompt"])
		self.assertNotIn('"findings"', turns[1]["prompt"], "the conformity prompt asks for no findings")
		self.assertEqual(turns[1]["timeout"], 10 * 60)
		for evidence in (conformity, review):
			self.assertIn("asked once more for the spec conformity review alone", evidence["retry"])
			self.assertIn("no verdict for any criterion", evidence["retry"])

	def test_a_failed_turn_runs_the_combined_turn_once_more(self):
		ticks = iter([0.0, 2 * 60.0])
		conformity, review, turns, root = self._run([
			failed_turn("upstream connection reset"),
			answer({"criteria": VERDICTS, "findings": []}),
		], clock=lambda: next(ticks))
		self.assertEqual(len(turns), 2)
		self.assertEqual(turns[1]["prompt"], turns[0]["prompt"], "the same question, word for word")
		# The two turns share the one turn's 20 minutes: 2 used, 18 left.
		self.assertEqual(turns[1]["timeout"], 18 * 60)
		self.assertEqual([v["verdict"] for v in conformity["review_verdicts"]], ["clean", "flagged"])
		self.assertTrue(review["available"])
		self.assertTrue(review["succeeded"])
		self.assertEqual(conformity["error"], "")
		self.assertEqual(review["error"], "")
		self.assertEqual(list(self._saved_prompts(root)), ["review-combined-retry.md", "review-combined.md"])
		for evidence in (conformity, review):
			self.assertIn("asked once more for both reviews", evidence["retry"])
			self.assertIn("upstream connection reset", evidence["retry"])

	def test_an_answer_with_no_json_at_all_runs_the_combined_turn_once_more(self):
		conformity, review, turns, _ = self._run([
			answer("Everything looks fine to me."),
			answer({"criteria": VERDICTS, "findings": FINDINGS}),
		])
		self.assertEqual(len(turns), 2)
		self.assertEqual(turns[1]["prompt"], turns[0]["prompt"])
		self.assertTrue(review["available"])
		self.assertIn("neither a verdict for any criterion nor a findings list", review["retry"])

	def test_a_second_failure_stays_unavailable_with_the_retrys_error_and_no_third_call(self):
		conformity, review, turns, _ = self._run([
			failed_turn("first failure"),
			failed_turn("second failure"),
		])
		self.assertEqual(len(turns), 2)
		self.assertFalse(conformity["succeeded"])
		self.assertTrue(all(v["verdict"] == "unavailable" for v in conformity["review_verdicts"]))
		self.assertEqual(conformity["error"], "second failure")
		self.assertEqual(conformity["stopped_reason"], "spec conformity review was unavailable: second failure")
		self.assertFalse(review["available"])
		self.assertFalse(review["succeeded"])
		self.assertEqual(review["error"], "second failure")
		self.assertEqual(review["stopped_reason"], "code review was unavailable: second failure")
		for evidence in (conformity, review):
			self.assertIn("first failure", evidence["retry"])

	def test_a_half_retry_that_fails_leaves_that_half_unavailable_and_the_other_as_it_was(self):
		conformity, review, turns, _ = self._run([
			answer({"criteria": VERDICTS}),
			subprocess.TimeoutExpired("pi", 600),
		])
		self.assertEqual(len(turns), 2)
		self.assertEqual([v["verdict"] for v in conformity["review_verdicts"]], ["clean", "flagged"])
		self.assertEqual(conformity["error"], "")
		self.assertFalse(review["available"])
		self.assertEqual(review["stopped_reason"], "code review was unavailable: timed out after 10 minutes")

	def test_a_combined_retry_that_answers_half_is_not_retried_again(self):
		conformity, review, turns, _ = self._run([
			failed_turn("first failure"),
			answer({"criteria": VERDICTS}),
		])
		self.assertEqual(len(turns), 2)
		self.assertEqual([v["verdict"] for v in conformity["review_verdicts"]], ["clean", "flagged"])
		self.assertFalse(review["available"])
		self.assertIn("no parseable response", review["stopped_reason"])

	def test_the_retry_has_its_own_empty_session_folder_and_the_first_turns_settings(self):
		for first in (answer({"criteria": VERDICTS}), answer({"findings": FINDINGS}), failed_turn("boom")):
			with self.subTest(first=first.stdout[:60]):
				_, _, turns, root = self._run([first, answer({"criteria": VERDICTS, "findings": FINDINGS})], thinking="max")
				first_turn, retry = turns
				self.assertEqual(first_turn["session"], root / ".pi-combined-review-session")
				self.assertEqual(retry["session"], root / ".pi-combined-review-session" / "retry")
				self.assertTrue(retry["session_was_empty"])
				self.assertEqual(retry["thinking"], first_turn["thinking"])
				self.assertEqual(retry["thinking"], "max")
				self.assertEqual(retry["base"], first_turn["base"])
				self.assertEqual(retry["base"], self.base_sha)
				# The retry is told nothing of the first turn: its prompt is
				# one a first launch would send, built from the same inputs.
				self.assertIn(retry["prompt"], (
					first_turn["prompt"],
					code_review.code_review_prompt("Add a widget.\n", self.base_sha, diff=self.diff),
					build_app.spec_conformity_prompt(["1. Widget exists", "2. Widget is documented"], self.base_sha, diff=self.diff),
				))

	def test_what_an_earlier_launch_left_in_the_retry_folder_is_removed_first(self):
		for plant in ("folder", "link"):
			with self.subTest(plant=plant), tempfile.TemporaryDirectory() as outside:
				seen = {}
				real = combined_review.fresh_retry_session_dir

				def planting(workspace):
					retry = workspace / ".pi-combined-review-session" / "retry"
					if plant == "folder":
						retry.mkdir(parents=True)
						(retry / "old-session.jsonl").write_text("an earlier conversation\n")
					else:
						(Path(outside) / "keep.txt").write_text("not the workspace's\n")
						retry.symlink_to(outside, target_is_directory=True)
					seen["path"] = real(workspace)
					return seen["path"]

				with mock.patch.object(combined_review, "fresh_retry_session_dir", side_effect=planting):
					_, _, turns, _ = self._run([failed_turn("boom"), answer({"criteria": VERDICTS, "findings": []})])
				self.assertTrue(turns[1]["session_was_empty"])
				self.assertFalse(seen["path"].is_symlink())
				self.assertEqual((Path(outside) / "keep.txt").exists(), plant == "link")

	def test_a_retry_that_does_not_fit_the_reviews_time_is_skipped_and_the_reason_recorded(self):
		# The clock is read at the start and once when the retry is sized:
		# 16 of the review's 20 minutes are gone by then.
		ticks = iter([0.0, 16 * 60.0])
		conformity, review, turns, root = self._run([failed_turn("boom")], clock=lambda: next(ticks))
		self.assertEqual(len(turns), 1)
		self.assertEqual(conformity["error"], "boom")
		self.assertEqual(review["stopped_reason"], "code review was unavailable: boom")
		for evidence in (conformity, review):
			self.assertTrue(evidence["retry"].startswith("not asked again (the first turn failed: boom)"), evidence["retry"])
			self.assertIn("under 5 of the review's 20 minutes", evidence["retry"])
		self.assertEqual(list(self._saved_prompts(root)), ["review-combined.md"])

	def test_the_two_turns_together_never_get_more_than_the_one_turns_time(self):
		for used_minutes, own in ((13.5, "half"), (13.5, "whole"), (0.1, "half"), (0.1, "whole"), (14.9, "whole")):
			with self.subTest(used_minutes=used_minutes, own=own):
				ticks = iter([0.0, used_minutes * 60.0])
				first = answer({"criteria": VERDICTS}) if own == "half" else failed_turn("boom")
				_, _, turns, _ = self._run([first, answer({"criteria": VERDICTS, "findings": []})], clock=lambda: next(ticks))
				self.assertEqual(turns[0]["timeout"], 20 * 60)
				self.assertLessEqual(used_minutes * 60 + turns[1]["timeout"], 20 * 60)
				self.assertGreaterEqual(turns[1]["timeout"], 5 * 60)
				self.assertLessEqual(turns[1]["timeout"], (10 if own == "half" else 20) * 60)

	def test_a_turn_the_spend_meter_stopped_is_not_asked_again(self):
		deny = '403 {"middleware":"meter","reason_code":"%s","detail":"x"}'
		errors = [deny % "budget_exceeded", deny % "ceiling_exceeded", deny % "rate_limited"]
		for error in errors:
			with self.subTest(error=error):
				conformity, review, turns, root = self._run([failed_turn(error)])
				self.assertEqual(len(turns), 1)
				self.assertFalse(review["available"])
				self.assertTrue(all(v["verdict"] == "unavailable" for v in conformity["review_verdicts"]))
				self.assertEqual(review["error"], conformity["error"])
				self.assertIn("reason_code", review["error"])
				for evidence in (conformity, review):
					self.assertIn("not asked again", evidence["retry"])
					self.assertIn("the spend meter stopped the first turn", evidence["retry"])
				self.assertEqual(list(self._saved_prompts(root)), ["review-combined.md"])

	def test_a_providers_own_rate_limit_is_asked_again_once(self):
		# A provider's 429 is not the spend meter's denial: the turn gets the
		# one retry any failed turn gets, in what is left of the same budget.
		errors = [c["error"] for c in self._stop_causes() if "provider's own" in c["name"]]
		self.assertGreaterEqual(len(errors), 5)
		for error in errors:
			with self.subTest(error=error):
				ticks = iter([0.0, 60.0])
				conformity, review, turns, root = self._run(
					[failed_turn(error), answer({"criteria": VERDICTS, "findings": []})], clock=lambda: next(ticks),
				)
				self.assertEqual(len(turns), 2)
				self.assertTrue(review["available"])
				self.assertLessEqual(60 + turns[1]["timeout"], 20 * 60)
				for evidence in (conformity, review):
					self.assertNotIn("not asked again", evidence["retry"])
					self.assertNotIn("spend meter", evidence["retry"])
				self.assertEqual(sorted(self._saved_prompts(root)), ["review-combined-retry.md", "review-combined.md"])

	def test_a_turn_the_providers_usage_limit_refused_is_not_asked_again(self):
		# The codex CLI's message as captured live (2026-10-11), when the
		# turn was asked again and refused again.
		error = next(c["error"] for c in self._stop_causes() if c["stopped_by"] == "provider_usage_limit")
		conformity, review, turns, root = self._run([failed_turn(error)])
		self.assertEqual(len(turns), 1)
		self.assertFalse(review["available"])
		self.assertTrue(all(v["verdict"] == "unavailable" for v in conformity["review_verdicts"]))
		self.assertEqual(review["error"], error)
		for evidence in (conformity, review):
			self.assertIn("not asked again", evidence["retry"])
			self.assertIn("the model provider's usage limit was reached", evidence["retry"])
			self.assertNotIn("spend meter", evidence["retry"])
		self.assertEqual(list(self._saved_prompts(root)), ["review-combined.md"])

	@staticmethod
	def _stop_causes() -> list[dict]:
		return json.loads((Path(__file__).parent / "fixtures" / "review_stop_causes.json").read_text())

	def test_stop_causes_are_named_as_the_host_names_them(self):
		# internal/codereview's TestStopCausesFixture reads the same file.
		cases = self._stop_causes()
		self.assertTrue(cases)
		for case in cases:
			with self.subTest(case=case["name"]):
				self.assertEqual(combined_review.stop_cause(case["error"]), case["stopped_by"])
				if case["stopped_by"] == "provider_usage_limit":
					self.assertEqual(build_app.provider_limit_resets_at(case["error"]), case["resets_at"])
					self.assertEqual(build_app.model_stop(["connection reset", case["error"]]), {"code": "provider_usage_limit", "resets_at": case["resets_at"]})
				elif not case["stopped_by"]:
					self.assertIsNone(build_app.model_stop([case["error"]]))

	def test_a_denial_code_outside_the_meters_body_is_an_ordinary_failure(self):
		self.assertEqual(combined_review.meter_stop('{"reason_code":"budget_exceeded"}'), "")
		self.assertEqual(combined_review.meter_stop('{"middleware":"meter","reason_code":"policy_changed"}'), "")
		self.assertEqual(combined_review.meter_stop('{"middleware":"meter","reason_code":"rate_limited"}'), "rate_limited")
		self.assertEqual(combined_review.meter_stop("connection reset"), "")

	def test_the_first_turns_evidence_is_on_disk_before_the_retry_starts(self):
		# A retry killed from outside leaves what the first turn alone gave.
		cases = [
			(answer({"criteria": VERDICTS}), ["clean", "flagged"], False, ""),
			(answer({"findings": FINDINGS}), ["unavailable", "unavailable"], True, ""),
			(failed_turn("boom"), ["unavailable", "unavailable"], False, "boom"),
		]
		for first, verdicts, available, error in cases:
			with self.subTest(first=first.stdout[:60]):
				_, _, turns, _ = self._run([first, answer({"criteria": VERDICTS, "findings": FINDINGS})])
				self.assertEqual(turns[0]["evidence"], {})
				on_disk = turns[1]["evidence"]
				self.assertEqual([v["verdict"] for v in on_disk["CONFORMITY_EVIDENCE.json"]["review_verdicts"]], verdicts)
				self.assertEqual(on_disk["CODE_REVIEW_EVIDENCE.json"]["available"], available)
				self.assertEqual(on_disk["CODE_REVIEW_EVIDENCE.json"]["error"], error)
				for evidence in on_disk.values():
					self.assertNotIn("retry", evidence)

	def test_the_retry_cannot_read_the_first_turns_transcript(self):
		for first in (answer({"criteria": VERDICTS}), answer({"findings": FINDINGS}), failed_turn("boom")):
			with self.subTest(first=first.stdout[:60]):
				_, _, turns, root = self._run([first, answer({"criteria": VERDICTS, "findings": FINDINGS})])
				# All the retry finds in the first turn's folder: both saved
				# prompts and its own empty folder.
				self.assertEqual(turns[1]["first_session"], ["prompts", "retry"])
				self.assertEqual(len(self._saved_prompts(root)), 2)
				left = [p for p in root.rglob("*") if ".git" not in p.parts and p.is_file() and "turn 1" in p.read_text(errors="replace")]
				self.assertEqual(left, [])

	def test_a_session_folder_that_is_a_link_is_never_followed(self):
		with tempfile.TemporaryDirectory() as workspace, tempfile.TemporaryDirectory() as outside:
			root = Path(workspace)
			(Path(outside) / "keep.txt").write_text("not the workspace's\n")
			(Path(outside) / "retry").mkdir()
			(Path(outside) / "retry" / "keep.txt").write_text("not the workspace's either\n")
			(root / ".pi-combined-review-session").symlink_to(outside, target_is_directory=True)
			retry = combined_review.fresh_retry_session_dir(root)
			self.assertEqual(retry, root / ".pi-combined-review-session" / "retry")
			self.assertFalse((root / ".pi-combined-review-session").is_symlink())
			self.assertEqual(list(retry.iterdir()), [])
			self.assertEqual(retry.resolve(), (root / ".pi-combined-review-session" / "retry").resolve())
			self.assertTrue((Path(outside) / "keep.txt").exists())
			self.assertTrue((Path(outside) / "retry" / "keep.txt").exists())

	def test_links_inside_the_session_folder_are_removed_by_name(self):
		with tempfile.TemporaryDirectory() as workspace, tempfile.TemporaryDirectory() as outside:
			root = Path(workspace)
			session = root / ".pi-combined-review-session"
			(session / "prompts").mkdir(parents=True)
			(session / "prompts" / "review-combined.md").write_text("the first prompt\n")
			(Path(outside) / "keep.txt").write_text("not the workspace's\n")
			(session / "codex-home").symlink_to(outside, target_is_directory=True)
			(session / "sessions").mkdir()
			(session / "sessions" / "inner").symlink_to(outside, target_is_directory=True)
			retry = combined_review.fresh_retry_session_dir(root)
			self.assertEqual(sorted(p.name for p in session.iterdir()), ["prompts", "retry"])
			self.assertEqual(list(retry.iterdir()), [])
			self.assertEqual((session / "prompts" / "review-combined.md").read_text(), "the first prompt\n")
			self.assertTrue((Path(outside) / "keep.txt").exists())

	def test_a_session_folder_that_cannot_be_emptied_asks_nothing_again(self):
		with mock.patch.object(combined_review, "fresh_retry_session_dir", return_value=None):
			conformity, review, turns, _ = self._run([failed_turn("boom")])
		self.assertEqual(len(turns), 1)
		self.assertEqual(review["error"], "boom")
		self.assertIn("could not be emptied", conformity["retry"])
		self.assertIn("could not be emptied", review["retry"])


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
