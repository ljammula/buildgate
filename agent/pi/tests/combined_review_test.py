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

APPROVED_SPEC = "# Widget\n\nThe widget is `rounded` to two places.\nIt never panics.\n"


def init_repo_with_commit(path: Path) -> None:
	run = lambda *args: subprocess.run(["git", *args], cwd=path, check=True, capture_output=True)
	run("init")
	run("config", "user.email", "test@test")
	run("config", "user.name", "test")
	run("config", "commit.gpgsign", "false")
	(path / "seed.txt").write_text("seed\n")
	run("add", "seed.txt")
	run("commit", "-m", "init")


def answer(payload) -> subprocess.CompletedProcess:
	text = payload if isinstance(payload, str) else json.dumps(payload)
	return subprocess.CompletedProcess([], 0, json.dumps({"type": "message_end", "message": {"role": "assistant", "content": text}}), "")


def failed_turn(message: str) -> subprocess.CompletedProcess:
	return subprocess.CompletedProcess([], 0, json.dumps({
		"type": "message_end", "message": {"role": "assistant", "stopReason": "error", "errorMessage": message},
	}), "")


FINDINGS = [{"severity": "high", "file": "a.go", "line": 12, "summary": "nil deref", "failure_scenario": "Foo(nil) panics"}]


def repro_finding(n: int, content: str = "package a\n") -> dict:
	return {
		"severity": "high", "file": "a.go", "line": n, "summary": f"bug {n}", "failure_scenario": "x",
		"spec_line": {"number": 4, "text": "It never panics."},
		"reproduction": {"path": f"a/repro{n}_test.go", "content": content},
	}


class CombinedReviewPromptTests(unittest.TestCase):
	def test_diff_comes_before_the_specs_and_the_one_tasks_rules_are_present(self):
		prompt = combined_review.combined_review_prompt(
			"Add a widget to the app.", "abc123", diff=("stat text", "diff text"), approved_spec=APPROVED_SPEC, reproductions=True,
		)
		diff_index = prompt.index("diff text")
		self.assertLess(diff_index, prompt.index("Add a widget to the app."), "diff block must precede the ticket spec")
		self.assertLess(prompt.index("Add a widget to the app."), prompt.index("L1: # Widget"))
		self.assertIn("git's guess at the enclosing function or heading", prompt)
		self.assertIn("Report only concrete correctness, security, data-loss", prompt)
		self.assertIn("IMPORTANT rule for command-outcome findings", prompt)
		self.assertEqual(prompt.count("Respond with ONLY a single JSON object"), 1)
		self.assertIn('"spec_line"', prompt)
		self.assertIn('"reproduction"', prompt)

	def test_there_is_no_conformity_task(self):
		prompt = combined_review.combined_review_prompt("spec", "abc123", diff=("s", "d"), approved_spec=APPROVED_SPEC, reproductions=True)
		for gone in ('"criteria"', "Task A", "acceptance criteri", "Pure formatting or lint drift"):
			self.assertNotIn(gone, prompt)

	def test_the_approved_spec_is_numbered_by_file_line_blank_lines_included(self):
		prompt = combined_review.combined_review_prompt("spec", "abc123", diff=("s", "d"), approved_spec=APPROVED_SPEC, reproductions=True)
		for want in ("L1: # Widget", "L2: ", "L3: The widget is `rounded` to two places.", "L4: It never panics."):
			self.assertIn(want, prompt.split("\n"))
		self.assertNotIn("L5:", prompt)

	def test_crlf_line_ends_are_not_shown(self):
		self.assertEqual(
			combined_review.numbered_spec("a\r\nb\r\n").splitlines()[1:3], ["L1: a", "L2: b"],
		)

	def test_without_an_approved_spec_nothing_is_citable(self):
		prompt = combined_review.combined_review_prompt("spec", "abc123", diff=("s", "d"), reproductions=True)
		self.assertIn("No approved spec is given", prompt)
		self.assertNotIn("L1:", prompt)

	def test_the_reproduction_rule_and_severity_rule_are_in_the_prompt(self):
		prompt = combined_review.combined_review_prompt("spec", "abc123", diff=("s", "d"), approved_spec=APPROVED_SPEC, reproductions=True)
		self.assertIn("blocks the change only through a reproduction", prompt)
		self.assertIn("You write no file in the workspace", prompt)
		self.assertIn("A reproduction at an existing path is advice", prompt)
		self.assertIn("A test that hangs or raises an exception counts as a failure", prompt)
		self.assertIn("A finding without both a spec line and a reproduction is advice", prompt)
		self.assertIn('"high" is a finding that breaks a behaviour the approved spec states', prompt)
		self.assertIn('is "medium" (real, non-blocking) or "low"', prompt)

	def test_falls_back_to_instruction_only_diff_when_diff_is_none(self):
		prompt = combined_review.combined_review_prompt("spec text", "abc123", diff=None)
		self.assertIn("Diff the workspace against abc123", prompt)


FIXTURES = Path(__file__).resolve().parent / "fixtures" / "combined_review"


class AdviceReviewTests(unittest.TestCase):
	def test_with_reproductions_the_prompt_is_byte_identical_to_before_the_flag_existed(self):
		inline = combined_review.combined_review_prompt(
			"Add a widget to the app.\n", "abc123", diff=("stat text", "diff text"), instructions_block="\n[instr]",
			approved_spec=APPROVED_SPEC, reproductions=True,
		)
		self.assertEqual(inline, (FIXTURES / "reproductions_inline.txt").read_text())
		bare = combined_review.combined_review_prompt("Add a widget to the app.\n", "abc123", diff=None, reproductions=True)
		self.assertEqual(bare, (FIXTURES / "reproductions_self_no_spec.txt").read_text())

	def test_the_advice_prompt_has_the_advice_rules_and_no_spec_or_reproduction_text(self):
		prompt = combined_review.combined_review_prompt(
			"Add a widget to the app.", "abc123", diff=("stat text", "diff text"), approved_spec=APPROVED_SPEC,
		)
		self.assertIn("what tests cannot show", prompt)
		self.assertIn("at most 5 findings", prompt)
		self.assertIn('"high" is likely real harm', prompt)
		self.assertIn("IMPORTANT rule for command-outcome findings", prompt)
		self.assertIn("diff text", prompt)
		self.assertIn("Add a widget to the app.", prompt)
		for gone in ("L1:", "reproduction", "spec_line", "approved spec", "It never panics."):
			self.assertNotIn(gone, prompt)

	def test_the_advice_prompt_keeps_the_instructions_diff(self):
		prompt = combined_review.combined_review_prompt("s", "abc", diff=("st", "d"), instructions_block="\n[instr]")
		self.assertIn("[instr]", prompt)

	def test_the_parser_drops_a_spec_line_and_reproduction_without_the_flag(self):
		reply = {"findings": [repro_finding(1)]}
		_, findings = code_review.parse_code_review_findings(json.dumps(reply), reproductions=False)
		self.assertEqual(len(findings), 1)
		self.assertNotIn("spec_line", findings[0])
		self.assertNotIn("reproduction", findings[0])
		_, findings = code_review.parse_code_review_findings(json.dumps(reply))
		self.assertIn("reproduction", findings[0])

	def test_an_advice_review_keeps_five_findings_and_notes_the_rest(self):
		findings = [repro_finding(n) for n in range(1, 8)]
		notes = code_review.cap_advice(findings)
		self.assertEqual(len(findings), 5)
		self.assertEqual(len(notes), 1)
		self.assertIn("2 further finding(s) dropped", notes[0])
		self.assertEqual(code_review.cap_advice(findings), [])

	def test_run_without_the_flag_never_reads_the_spec_and_caps_with_a_note(self):
		with tempfile.TemporaryDirectory() as directory, tempfile.TemporaryDirectory() as spec_dir:
			root = Path(directory)
			init_repo_with_commit(root)
			spec_path = Path(spec_dir) / "ticket.md"
			spec_path.write_text("Add a widget.\n")
			findings = [repro_finding(n) for n in range(1, 8)]
			with mock.patch.object(build_app, "sh", return_value=answer({"findings": findings})) as sh:
				review = combined_review.run_combined_review(
					root, spec_path=spec_path, approved_spec_path=Path(spec_dir) / "missing.md",
				)
			self.assertNotIn("reproduction", sh.call_args.args[0][-1])
			self.assertEqual(len(review["findings"]), 5)
			self.assertTrue(all("spec_line" not in f and "reproduction" not in f for f in review["findings"]))
			self.assertEqual(len(review["notes"]), 1)
			self.assertTrue(review["succeeded"])


class RunCombinedReviewTests(unittest.TestCase):
	def _files(self, directory: str, spec_dir: str):
		root = Path(directory)
		init_repo_with_commit(root)
		spec_path = Path(spec_dir) / "ticket.md"
		spec_path.write_text("Add a widget.\n")
		approved = Path(spec_dir) / "spec.md"
		approved.write_text(APPROVED_SPEC)
		return root, spec_path, approved

	def test_one_turn_yields_the_findings_with_citations_and_reproductions(self):
		with tempfile.TemporaryDirectory() as directory, tempfile.TemporaryDirectory() as spec_dir:
			root, spec_path, approved = self._files(directory, spec_dir)
			git_calls = []

			def side_effect(args, **kwargs):
				if args and args[0] == "git":
					git_calls.append(args[1])
					return subprocess.run(args, capture_output=True, text=True, cwd=kwargs.get("cwd"))
				return answer({"findings": [repro_finding(1), FINDINGS[0]]})

			with mock.patch.object(build_app, "sh", side_effect=side_effect):
				review = combined_review.run_combined_review(
					root, spec_path=spec_path, approved_spec_path=approved, review_policy="required", reproductions=True,
				)
			self.assertNotIn("add", git_calls)
			self.assertNotIn("commit", git_calls)
			self.assertTrue(review["succeeded"], "a finding never fails the review")
			self.assertIn("2 finding(s), 2 high-severity", review["stopped_reason"])
			self.assertEqual(review["findings"][0]["spec_line"], {"number": 4, "text": "It never panics."})
			self.assertEqual(review["findings"][0]["reproduction"], {"path": "a/repro1_test.go", "content": "package a\n"})
			self.assertNotIn("spec_line", review["findings"][1])
			self.assertNotIn("reproduction", review["findings"][1])
			self.assertNotIn("notes", review)

	def test_the_approved_spec_reaches_the_prompt_numbered(self):
		with tempfile.TemporaryDirectory() as directory, tempfile.TemporaryDirectory() as spec_dir:
			root, spec_path, approved = self._files(directory, spec_dir)
			with mock.patch.object(build_app, "sh", return_value=answer({"findings": []})) as sh:
				combined_review.run_combined_review(root, spec_path=spec_path, approved_spec_path=approved, reproductions=True)
			self.assertIn("L4: It never panics.", sh.call_args.args[0][-1])

	def test_a_high_finding_does_not_fail_a_required_review_and_a_missing_list_does(self):
		with tempfile.TemporaryDirectory() as directory, tempfile.TemporaryDirectory() as spec_dir:
			root, spec_path, _ = self._files(directory, spec_dir)
			with mock.patch.object(build_app, "sh", return_value=answer({"findings": FINDINGS})):
				review = combined_review.run_combined_review(root, spec_path=spec_path, review_policy="required")
			self.assertTrue(review["succeeded"])

	def test_advisory_policy_never_fails_the_review(self):
		with tempfile.TemporaryDirectory() as directory, tempfile.TemporaryDirectory() as spec_dir:
			root, spec_path, _ = self._files(directory, spec_dir)
			with mock.patch.object(build_app, "sh", return_value=failed_turn("boom")):
				review = combined_review.run_combined_review(root, spec_path=spec_path, review_policy="advisory")
			self.assertTrue(review["succeeded"])
			self.assertFalse(review["available"])

	def test_unavailable_turn_records_the_error(self):
		with tempfile.TemporaryDirectory() as directory, tempfile.TemporaryDirectory() as spec_dir:
			root, spec_path, _ = self._files(directory, spec_dir)
			with mock.patch.object(build_app, "sh", return_value=failed_turn('429 "token budget exceeded"')):
				review = combined_review.run_combined_review(root, spec_path=spec_path, review_policy="required")
			self.assertFalse(review["succeeded"])
			self.assertFalse(review["available"])
			self.assertIn("429", review["stopped_reason"])
			self.assertEqual(review["findings"], [])

	def test_at_most_three_reproductions_are_kept_and_the_rest_become_advice_with_a_note(self):
		with tempfile.TemporaryDirectory() as directory, tempfile.TemporaryDirectory() as spec_dir:
			root, spec_path, _ = self._files(directory, spec_dir)
			findings = [repro_finding(n) for n in range(1, 6)]
			with mock.patch.object(build_app, "sh", return_value=answer({"findings": findings})):
				review = combined_review.run_combined_review(root, spec_path=spec_path, reproductions=True)
			self.assertTrue(review["succeeded"], "a cap never fails the review")
			self.assertEqual(len(review["findings"]), 5)
			self.assertEqual(["reproduction" in f for f in review["findings"]], [True, True, True, False, False])
			self.assertTrue(all("spec_line" in f for f in review["findings"]))
			self.assertEqual(len(review["notes"]), 2)
			self.assertIn("reproduction dropped", review["notes"][0])
			self.assertIn("at most 3", review["notes"][0])

	def test_a_reproduction_over_32_kib_is_dropped_with_a_note_and_one_at_the_cap_is_kept(self):
		with tempfile.TemporaryDirectory() as directory, tempfile.TemporaryDirectory() as spec_dir:
			root, spec_path, _ = self._files(directory, spec_dir)
			at_cap = "a" * code_review.MAX_REPRODUCTION_BYTES
			findings = [repro_finding(1, at_cap), repro_finding(2, at_cap + "a"), repro_finding(3, "é" * 20_000)]
			with mock.patch.object(build_app, "sh", return_value=answer({"findings": findings})):
				review = combined_review.run_combined_review(root, spec_path=spec_path, reproductions=True)
			self.assertEqual(["reproduction" in f for f in review["findings"]], [True, False, False])
			self.assertEqual(len(review["notes"]), 2, "bytes are counted, not characters")
			self.assertIn("over the 32768 byte cap", review["notes"][0])
			self.assertTrue(review["succeeded"])

	def test_the_cap_does_not_count_a_dropped_oversize_file_against_the_three(self):
		findings = [repro_finding(1, "a" * (code_review.MAX_REPRODUCTION_BYTES + 1)), repro_finding(2), repro_finding(3), repro_finding(4)]
		notes = code_review.cap_reproductions(findings)
		self.assertEqual(["reproduction" in f for f in findings], [False, True, True, True])
		self.assertEqual(len(notes), 1)

	def test_a_timeout_names_the_limit_and_is_not_asked_again(self):
		with tempfile.TemporaryDirectory() as directory, tempfile.TemporaryDirectory() as spec_dir:
			root, spec_path, _ = self._files(directory, spec_dir)
			timeouts = []

			def timing_out(command, *, cwd, timeout, env):
				timeouts.append(timeout)
				raise subprocess.TimeoutExpired(command, timeout)

			ticks = iter([0.0, 20 * 60.0])
			with (
				mock.patch.object(build_app, "sh", side_effect=timing_out),
				mock.patch.object(combined_review, "clock", side_effect=lambda: next(ticks)),
			):
				review = combined_review.run_combined_review(root, spec_path=spec_path, reproductions=True)
			self.assertEqual(timeouts, [20 * 60])
			self.assertNotIn("retry", review)
			self.assertFalse(review["available"])
			self.assertIn("timed out after 20 minutes", review["stopped_reason"])


class CombinedReviewRetryTests(unittest.TestCase):
	"""An answer with no parseable findings list is asked for once more, with
	the same prompt. A turn that failed is not asked again."""

	def _run(self, answers, *, thinking="high", clock=None, review_policy="required", reproductions=True):
		"""Runs the review with the model turns answered in order by answers
		(a CompletedProcess, or an exception to raise). Returns (review,
		turns, root): turns holds, per model call, its prompt, session
		folder, timeout, thinking level and review base."""
		directory = tempfile.TemporaryDirectory()
		spec_dir = tempfile.TemporaryDirectory()
		self.addCleanup(directory.cleanup)
		self.addCleanup(spec_dir.cleanup)
		root = Path(directory.name)
		init_repo_with_commit(root)
		base_sha = subprocess.run(["git", "rev-parse", "HEAD"], cwd=root, check=True, capture_output=True, text=True).stdout.strip()
		(root / "seed.txt").write_text("seed\nchanged by the build\n")
		spec_path = Path(spec_dir.name) / "ticket.md"
		spec_path.write_text("Add a widget.\n")
		self.base_sha = base_sha
		queue = list(answers)
		turns = []

		def fake_sh(command, *, cwd, timeout, env):
			session = Path(command[command.index("--session-dir") + 1])
			first = root / ".pi-combined-review-session"
			turns.append({
				"prompt": command[-1], "session": session, "timeout": timeout,
				"thinking": command[command.index("--thinking") + 1],
				"base": env.get("AI_REVIEW_BASE_SHA"),
				"session_was_empty": not session.exists() or not [p for p in session.iterdir() if p.name != "prompts"],
				"first_session": sorted(p.name for p in first.iterdir()) if first.is_dir() else [],
				"evidence": json.loads((root / "CODE_REVIEW_EVIDENCE.json").read_text()) if (root / "CODE_REVIEW_EVIDENCE.json").exists() else None,
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
			review = combined_review.run_combined_review(
				root, spec_path=spec_path, review_policy=review_policy, review_base_sha=base_sha, thinking=thinking,
				reproductions=reproductions,
			)
		self.assertEqual(queue, [], "every prepared answer is one model call")
		return review, turns, root

	def _saved_prompts(self, root: Path) -> dict[str, str]:
		folder = root / ".pi-combined-review-session" / "prompts"
		return {p.name: p.read_text() for p in sorted(folder.iterdir())}

	def test_a_whole_first_answer_makes_exactly_one_model_call_and_records_no_retry(self):
		review, turns, root = self._run([answer({"findings": FINDINGS})])
		self.assertEqual(len(turns), 1)
		self.assertNotIn("retry", review)
		self.assertEqual(list(self._saved_prompts(root)), ["review-combined.md"])
		self.assertIn("account for every relevant return value and branch", self._saved_prompts(root)["review-combined.md"])
		self.assertFalse((root / ".pi-combined-review-session" / "retry").exists())

	def test_an_empty_findings_list_is_a_whole_answer(self):
		review, turns, _ = self._run([answer({"findings": []})])
		self.assertEqual(len(turns), 1)
		self.assertTrue(review["available"])
		self.assertNotIn("retry", review)

	def test_a_failed_turn_is_not_asked_again(self):
		deny = '403 {"middleware":"meter","reason_code":"%s","detail":"x"}'
		errors = [
			"upstream connection reset",
			'429 "rate limit exceeded"',
			deny % "budget_exceeded", deny % "ceiling_exceeded", deny % "rate_limited",
			"You've hit your usage limit. Upgrade to Pro (https://chatgpt.com/explore/pro) or try again at 3:11 AM.",
		]
		for error in errors:
			with self.subTest(error=error):
				review, turns, root = self._run([failed_turn(error)])
				self.assertEqual(len(turns), 1)
				self.assertFalse(review["available"])
				self.assertEqual(review["error"], error)
				self.assertEqual(review["stopped_reason"], "code review was unavailable: " + error)
				self.assertNotIn("retry", review)
				self.assertEqual(list(self._saved_prompts(root)), ["review-combined.md"])
				self.assertFalse((root / ".pi-combined-review-session" / "retry").exists())

	def test_an_answer_with_no_json_at_all_runs_the_same_prompt_once_more(self):
		review, turns, root = self._run([answer("Everything looks fine to me."), answer({"findings": FINDINGS})])
		self.assertEqual(len(turns), 2)
		self.assertEqual(turns[1]["prompt"], turns[0]["prompt"], "the same question, word for word")
		self.assertIn("changed by the build", turns[1]["prompt"])
		self.assertNotIn("rewritten by reviewer", turns[1]["prompt"])
		self.assertTrue(review["available"])
		self.assertEqual(review["findings"][0]["summary"], "nil deref")
		self.assertIn("asked once more, in a new session", review["retry"])
		self.assertIn("no parseable findings list", review["retry"])
		self.assertEqual(list(self._saved_prompts(root)), ["review-combined-retry.md", "review-combined.md"])

	def test_a_retry_shares_the_one_turns_time(self):
		ticks = iter([0.0, 2 * 60.0])
		review, turns, _ = self._run(
			[answer("Everything looks fine to me."), answer({"findings": []})], clock=lambda: next(ticks),
		)
		self.assertEqual(turns[1]["timeout"], 18 * 60)
		self.assertTrue(review["succeeded"])

	def test_a_retry_that_fails_stays_unavailable_with_the_retrys_error_and_no_third_call(self):
		review, turns, _ = self._run([answer("Everything looks fine to me."), failed_turn("second failure")])
		self.assertEqual(len(turns), 2)
		self.assertFalse(review["available"])
		self.assertFalse(review["succeeded"])
		self.assertEqual(review["error"], "second failure")
		self.assertEqual(review["stopped_reason"], "code review was unavailable: second failure")
		self.assertIn("asked once more", review["retry"])

	def test_the_retry_has_its_own_empty_session_folder_and_the_first_turns_settings(self):
		_, turns, root = self._run([answer("Everything looks fine to me."), answer({"findings": FINDINGS})], thinking="max")
		first_turn, retry = turns
		self.assertEqual(first_turn["session"], root / ".pi-combined-review-session")
		self.assertEqual(retry["session"], root / ".pi-combined-review-session" / "retry")
		self.assertTrue(retry["session_was_empty"])
		self.assertEqual(retry["thinking"], "max")
		self.assertEqual(retry["base"], first_turn["base"])
		self.assertEqual(retry["base"], self.base_sha)

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
					_, turns, _ = self._run([answer("Everything looks fine to me."), answer({"findings": []})])
				self.assertTrue(turns[1]["session_was_empty"])
				self.assertFalse(seen["path"].is_symlink())
				self.assertEqual((Path(outside) / "keep.txt").exists(), plant == "link")

	def test_a_retry_that_does_not_fit_the_reviews_time_is_skipped_and_the_reason_recorded(self):
		ticks = iter([0.0, 16 * 60.0])
		review, turns, root = self._run([answer("Everything looks fine to me.")], clock=lambda: next(ticks))
		self.assertEqual(len(turns), 1)
		self.assertFalse(review["available"])
		self.assertTrue(review["retry"].startswith("not asked again (the first answer had no parseable findings list)"), review["retry"])
		self.assertIn("under 5 of the review's 20 minutes", review["retry"])
		self.assertEqual(list(self._saved_prompts(root)), ["review-combined.md"])

	def test_the_two_turns_together_never_get_more_than_the_one_turns_time(self):
		for used_minutes in (13.5, 0.1, 14.9):
			with self.subTest(used_minutes=used_minutes):
				ticks = iter([0.0, used_minutes * 60.0])
				_, turns, _ = self._run(
					[answer("Everything looks fine to me."), answer({"findings": []})], clock=lambda: next(ticks),
				)
				self.assertEqual(turns[0]["timeout"], 20 * 60)
				self.assertLessEqual(used_minutes * 60 + turns[1]["timeout"], 20 * 60)
				self.assertGreaterEqual(turns[1]["timeout"], 5 * 60)

	def test_the_first_turns_evidence_is_on_disk_before_the_retry_starts(self):
		_, turns, _ = self._run([answer("Everything looks fine to me."), answer({"findings": FINDINGS})])
		self.assertIsNone(turns[0]["evidence"])
		on_disk = turns[1]["evidence"]
		self.assertFalse(on_disk["available"])
		self.assertNotIn("retry", on_disk)

	def test_the_retry_cannot_read_the_first_turns_transcript(self):
		_, turns, root = self._run([answer("Everything looks fine to me."), answer({"findings": FINDINGS})])
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
			review, turns, _ = self._run([answer("Everything looks fine to me.")])
		self.assertEqual(len(turns), 1)
		self.assertFalse(review["available"])
		self.assertIn("could not be emptied", review["retry"])


class WriteCombinedEvidenceTests(unittest.TestCase):
	def test_writes_only_the_code_review_evidence_file(self):
		with tempfile.TemporaryDirectory() as directory:
			root = Path(directory)
			review = code_review.code_review_evidence(True, [], "", review_policy="required", thinking=None)
			path = combined_review.write_combined_evidence(root, review)
			self.assertEqual(path.name, "CODE_REVIEW_EVIDENCE.json")
			self.assertEqual(json.loads(path.read_text()), review)
			self.assertEqual(review["schema_version"], 1)
			self.assertIn("findings", review)
			self.assertEqual(sorted(p.name for p in root.iterdir()), ["CODE_REVIEW_EVIDENCE.json"])


class MainTests(unittest.TestCase):
	def _main(self, payload, *extra):
		directory = tempfile.TemporaryDirectory()
		spec_dir = tempfile.TemporaryDirectory()
		self.addCleanup(directory.cleanup)
		self.addCleanup(spec_dir.cleanup)
		root = Path(directory.name)
		init_repo_with_commit(root)
		spec_path = Path(spec_dir.name) / "ticket.md"
		spec_path.write_text("Add a widget.\n")
		approved = Path(spec_dir.name) / "spec.md"
		approved.write_text(APPROVED_SPEC)
		argv = [str(SCRIPT), "--workspace", str(root), "--spec", str(spec_path), "--approved-spec", str(approved), *extra]
		with (
			mock.patch.object(sys, "argv", argv),
			mock.patch.object(build_app, "sh", return_value=payload),
			contextlib.redirect_stdout(io.StringIO()),
		):
			return combined_review.main(), root

	def test_a_verdict_exits_40_whatever_the_findings(self):
		for findings in ([], FINDINGS, [repro_finding(1)]):
			with self.subTest(findings=len(findings)):
				exit_code, root = self._main(answer({"findings": findings}), "--thinking", "max")
				self.assertEqual(exit_code, 40)
				self.assertEqual(exit_code, combined_review.COMBINED_EXIT_BASE)
				evidence = json.loads((root / "CODE_REVIEW_EVIDENCE.json").read_text())
				self.assertEqual(evidence["thinking"], "max")
				self.assertFalse((root / "CONFORMITY_EVIDENCE.json").exists())

	def test_no_verdict_exits_42(self):
		exit_code, root = self._main(failed_turn("boom"))
		self.assertEqual(exit_code, 42)
		self.assertEqual(exit_code, combined_review.COMBINED_EXIT_BASE + 2)
		self.assertFalse(json.loads((root / "CODE_REVIEW_EVIDENCE.json").read_text())["available"])

	def test_no_verdict_under_advisory_policy_exits_40(self):
		exit_code, _ = self._main(failed_turn("boom"), "--review-policy", "advisory")
		self.assertEqual(exit_code, 40)

	def test_bit_zero_is_never_set(self):
		for payload in (answer({"findings": FINDINGS}), failed_turn("x"), answer("no json")):
			exit_code, _ = self._main(payload)
			self.assertEqual((exit_code - combined_review.COMBINED_EXIT_BASE) & 1, 0)

	def test_the_conformity_flags_are_gone(self):
		for flag in ("--conformity-policy", "--spec-acceptance-criteria"):
			argv = [str(SCRIPT), "--workspace", "/tmp/work", "--spec", "/tmp/ticket.md", flag, "x"]
			with mock.patch.object(sys, "argv", argv), contextlib.redirect_stderr(io.StringIO()):
				with self.assertRaises(SystemExit) as raised:
					combined_review.main()
			self.assertEqual(raised.exception.code, 2)

	def test_main_thinking_option_rejects_unknown(self):
		base_argv = [str(SCRIPT), "--workspace", "/tmp/work", "--spec", "/tmp/ticket.md"]
		with mock.patch.object(sys, "argv", base_argv + ["--thinking", "unknown"]):
			with contextlib.redirect_stderr(io.StringIO()):
				with self.assertRaises(SystemExit) as raised:
					combined_review.main()
		self.assertEqual(raised.exception.code, 2)


class ParseSpecLineAndReproductionTests(unittest.TestCase):
	def _parse(self, finding):
		return code_review.parse_code_review_findings(json.dumps({"findings": [{"severity": "high", "summary": "s", **finding}]}))[1][0]

	def test_well_formed_values_are_kept(self):
		f = self._parse({"spec_line": {"number": 3, "text": "t"}, "reproduction": {"path": " x_test.go ", "content": "c"}})
		self.assertEqual(f["spec_line"], {"number": 3, "text": "t"})
		self.assertEqual(f["reproduction"], {"path": "x_test.go", "content": "c"})

	def test_malformed_values_are_dropped_and_the_finding_stays(self):
		for finding in (
			{"spec_line": {"number": 0, "text": "t"}},
			{"spec_line": {"number": True, "text": "t"}},
			{"spec_line": {"number": "3", "text": "t"}},
			{"spec_line": {"number": 3, "text": " "}},
			{"spec_line": "L3"},
			{"reproduction": {"path": "", "content": "c"}},
			{"reproduction": {"path": "a_test.go", "content": ""}},
			{"reproduction": {"path": 1, "content": "c"}},
			{"reproduction": ["a_test.go"]},
		):
			with self.subTest(finding=finding):
				f = self._parse(finding)
				self.assertEqual(f["summary"], "s")
				self.assertNotIn("spec_line", f)
				self.assertNotIn("reproduction", f)


if __name__ == "__main__":
	unittest.main()
