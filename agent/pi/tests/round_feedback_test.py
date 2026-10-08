import importlib.util
import json
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path
from unittest import mock

SCRIPTS = Path(__file__).resolve().parents[1] / "scripts"
sys.path.insert(0, str(SCRIPTS))

import round_feedback  # noqa: E402

SPEC = importlib.util.spec_from_file_location("build_app_for_feedback", SCRIPTS / "build_app.py")
build_app = importlib.util.module_from_spec(SPEC)
sys.modules[SPEC.name] = build_app
SPEC.loader.exec_module(build_app)


def long_go_test_output() -> str:
	"""A `make test` whose failing assertion is 400 lines above its end."""
	lines = [f"ok  \tpkg/a{i}\t0.{i:03d}s" for i in range(60)]
	lines += ["--- FAIL: TestParseEmpty (0.00s)", "    parse_test.go:41: Parse(\"\") = -1, want 0", "FAIL", "FAIL\tpkg/parse\t0.412s"]
	lines += [f"ok  \tpkg/b{i}\t1.{i:03d}s" for i in range(400)]
	lines += ["make: *** [test] Error 1"]
	return "\n".join(lines)


class ExcerptTests(unittest.TestCase):
	def test_short_output_is_kept_whole(self):
		self.assertEqual(round_feedback.failure_excerpt("  boom\n"), "boom")

	def test_long_output_keeps_the_first_failure_not_only_the_end(self):
		output = long_go_test_output()
		self.assertGreater(len(output), round_feedback.EXCERPT_LIMIT)
		# What a plain tail of the same size as before held: none of it.
		self.assertNotIn("want 0", output[-3000:])
		excerpt = round_feedback.failure_excerpt(output)
		self.assertLessEqual(len(excerpt), round_feedback.EXCERPT_LIMIT)
		self.assertIn("--- FAIL: TestParseEmpty", excerpt)
		self.assertIn('Parse("") = -1, want 0', excerpt)
		self.assertIn("make: *** [test] Error 1", excerpt)
		self.assertIn("earlier line(s) omitted", excerpt)

	def test_output_with_no_failure_line_falls_back_to_its_end(self):
		output = "\n".join(f"line {i}" for i in range(5000))
		excerpt = round_feedback.failure_excerpt(output)
		self.assertTrue(excerpt.startswith("[... earlier output omitted ...]"))
		self.assertTrue(excerpt.endswith("line 4999"))

	def test_failing_names_across_tools(self):
		output = "\n".join([
			"--- FAIL: TestParseEmpty (0.00s)",
			"FAIL\tpkg/parse\t0.412s",
			"FAILED tests/test_add.py::test_negative - AssertionError",
			"FAIL: test_multiply (tests.test_add.AddTests)",
			" ✕ adds two numbers (3 ms)",
			"test parse::empty ... FAILED",
			"make: *** [Makefile:12: test] Error 1",
			"--- FAIL: TestParseEmpty (0.00s)",
		])
		self.assertEqual(round_feedback.failing_names(output), [
			"TestParseEmpty", "pkg/parse", "tests/test_add.py::test_negative",
			"test_multiply (tests.test_add.AddTests)", "adds two numbers", "parse::empty", "test",
		])


class SignatureTests(unittest.TestCase):
	def test_same_failure_with_different_timings_has_one_signature(self):
		first = "--- FAIL: TestParseEmpty (0.00s)\n    parse_test.go:41: got -1, want 0\nFAIL\tpkg/parse\t0.412s"
		again = "--- FAIL: TestParseEmpty (0.03s)\n    parse_test.go:41: got -1, want 0\nFAIL\tpkg/parse\t1.907s"
		other = "--- FAIL: TestParseSpace (0.00s)\n    parse_test.go:55: got 2, want 1\nFAIL\tpkg/parse\t0.412s"
		blockers = ["canonical verification failed"]
		self.assertEqual(round_feedback.failure_signature(blockers, first), round_feedback.failure_signature(blockers, again))
		self.assertNotEqual(round_feedback.failure_signature(blockers, first), round_feedback.failure_signature(blockers, other))
		self.assertNotEqual(round_feedback.failure_signature(blockers, first), round_feedback.failure_signature(["fast check failed"], first))
		self.assertEqual(round_feedback.failure_signature([], first), "")

	def test_streak_counts_only_the_trailing_repeat(self):
		self.assertEqual(round_feedback.same_failure_streak([]), 0)
		self.assertEqual(round_feedback.same_failure_streak(["a", ""]), 0)
		self.assertEqual(round_feedback.same_failure_streak(["a"]), 1)
		self.assertEqual(round_feedback.same_failure_streak(["a", "b"]), 1)
		self.assertEqual(round_feedback.same_failure_streak(["b", "a", "a"]), 2)
		self.assertEqual(round_feedback.same_failure_streak(["a", "a", "a"]), 3)

	def test_history_marks_a_repeat(self):
		text = round_feedback.history_lines([
			{"index": 1, "changed_files": ["a.go", "a_test.go"], "blockers": ["canonical verification failed"], "signature": "s1"},
			{"index": 2, "changed_files": [], "blockers": ["no changes made to the workspace", "canonical verification failed"], "signature": "s2"},
			{"index": 3, "changed_files": ["a.go"], "blockers": ["no changes made to the workspace", "canonical verification failed"], "signature": "s2"},
		])
		self.assertEqual(text.splitlines(), [
			"- Round 1: changed a.go, a_test.go; canonical verification failed",
			"- Round 2: changed no files; no changes made to the workspace; canonical verification failed",
			"- Round 3: changed a.go; no changes made to the workspace; canonical verification failed (the same failure as the round before)",
		])


class AgentNotesTests(unittest.TestCase):
	def notes(self, **overrides):
		args = dict(no_changes=False, timed_out=False, timeout_minutes=30, returncode=0, stderr_tail="", final_text="", route_errors=[], stalled=False)
		args.update(overrides)
		return round_feedback.agent_notes(**args)

	def test_nothing_to_say_when_the_agent_ran_normally(self):
		self.assertEqual(self.notes(), "")

	def test_every_agent_failure_kind_says_what_happened(self):
		self.assertIn("stopped after 30 minutes", self.notes(timed_out=True))
		crashed = self.notes(returncode=2, stderr_tail="unknown flag --frobnicate")
		self.assertIn("exited with status 2", crashed)
		self.assertIn("unknown flag --frobnicate", crashed)
		self.assertIn("stalled", self.notes(stalled=True))
		self.assertIn("- 429 rate limited", self.notes(route_errors=["429 rate limited"]))
		idle = self.notes(no_changes=True, final_text="The function already exists.")
		self.assertIn("without changing any file", idle)
		self.assertIn("The function already exists.", idle)


def agent_stdout(text: str = "done") -> str:
	return json.dumps({"type": "message_end", "message": {"role": "assistant", "content": text}})


class BuildLoopFeedbackTests(unittest.TestCase):
	"""run_build with a real failing verify command and scripted agent turns:
	what each later round's prompt holds."""

	def run_failing_build(self, root: Path, *, rounds: int, max_rounds: int, write_each_round: bool = True):
		subprocess.run(["git", "init", "-q", str(root)], check=True)
		subprocess.run(["git", "-C", str(root), "-c", "user.email=t@example.com", "-c", "user.name=t", "commit", "-q", "--allow-empty", "-m", "base"], check=True)
		(root / "noisy.txt").write_text(long_go_test_output() + "\n")
		subprocess.run(["git", "-C", str(root), "add", "noisy.txt"], check=True)
		subprocess.run(["git", "-C", str(root), "-c", "user.email=t@example.com", "-c", "user.name=t", "commit", "-q", "-m", "fixture"], check=True)
		spec = root.parent / (root.name + "-spec.md")
		spec.write_text("Fix the parser")
		prompts = []
		turn = {"n": 0}

		def stream(command, *, cwd=None, timeout=None, env=None, on_event=None):
			prompts.append(str(command[-1]))
			turn["n"] += 1
			if write_each_round:
				(root / "parse.go").write_text(f"package parse // attempt {turn['n']}\n")
			return subprocess.CompletedProcess(command, 0, agent_stdout(), ""), False

		with (
			mock.patch.object(build_app, "ensure_git_repo"),
			mock.patch.object(build_app, "run_agent_streaming", side_effect=stream),
		):
			result = build_app.run_build(
				root, spec, max_rounds=max_rounds, timeout_minutes=1, review_policy="off",
				verify_command_override="cat noisy.txt; exit 1",
			)
		self.assertEqual(len(prompts), rounds)
		return result, prompts

	def test_the_second_round_is_told_the_first_failure_and_where_the_whole_output_is(self):
		with tempfile.TemporaryDirectory() as directory:
			root = Path(directory) / "ws"
			root.mkdir()
			result, prompts = self.run_failing_build(root, rounds=2, max_rounds=2)
			second = prompts[1]
			self.assertIn("--- FAIL: TestParseEmpty", second)
			self.assertIn('Parse("") = -1, want 0', second)
			self.assertIn("- Round 1: changed parse.go; canonical verification failed", second)
			log = ".pi-build-session/feedback/round-1/verify.log"
			self.assertIn(f"`{log}`", second)
			self.assertIn("Failing tests or targets it names: TestParseEmpty, pkg/parse", second)
			self.assertEqual((root / log).read_text().strip(), long_go_test_output())
			self.assertNotIn("rounds in a row", second)
			self.assertFalse(result.succeeded)
			self.assertEqual(result.rounds[0].changed_files, ["parse.go"])
			self.assertEqual(result.rounds[0].failure_log, log)
			# The log is a harness artifact: it never counts as the agent's work.
			self.assertNotIn(log, build_app.changed_file_hashes(root, None))

	def test_a_repeated_failure_switches_to_diagnosis_then_stops(self):
		with tempfile.TemporaryDirectory() as directory:
			root = Path(directory) / "ws"
			root.mkdir()
			result, prompts = self.run_failing_build(root, rounds=3, max_rounds=6)
			self.assertNotIn("rounds in a row", prompts[1])
			third = prompts[2]
			self.assertIn("This is the same failure 2 rounds in a row", third)
			self.assertIn("(the same failure as the round before)", third)
			self.assertIn("no progress: the same failure 3 rounds in a row", result.stopped_reason)
			self.assertEqual(len(result.rounds), 3)

	def test_a_round_that_changed_nothing_is_told_so_with_its_own_last_message(self):
		with tempfile.TemporaryDirectory() as directory:
			root = Path(directory) / "ws"
			root.mkdir()
			_, prompts = self.run_failing_build(root, rounds=2, max_rounds=2, write_each_round=False)
			second = prompts[1]
			self.assertIn("- Round 1: changed no files; no changes made to the workspace", second)
			self.assertIn("Your previous turn ended without changing any file", second)
			self.assertIn("Your final message was:\n\ndone", second)

	def test_round_records_survive_a_resume(self):
		with tempfile.TemporaryDirectory() as directory:
			root = Path(directory) / "ws"
			root.mkdir()
			result, _ = self.run_failing_build(root, rounds=2, max_rounds=2)
			state = json.loads((root / build_app.ROUND_STATE_FILE).read_text())
			restored = build_app.round_from_state(state["rounds"][0], 1)
			self.assertEqual(restored.blockers, result.rounds[0].blockers)
			self.assertEqual(restored.changed_files, ["parse.go"])
			self.assertEqual(restored.failure_signature, result.rounds[0].failure_signature)
			with self.assertRaises(ValueError):
				build_app.round_from_state({**state["rounds"][0], "changed_files": ["ok", 7]}, 1)


if __name__ == "__main__":
	unittest.main()
