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

SCRIPT = Path(__file__).resolve().parents[1] / "scripts" / "code_review.py"
SPEC = importlib.util.spec_from_file_location("code_review", SCRIPT)
assert SPEC and SPEC.loader
code_review = importlib.util.module_from_spec(SPEC)
sys.modules[SPEC.name] = code_review
SPEC.loader.exec_module(code_review)

build_app = code_review.build_app
harness_adapters = code_review.harness_adapters


def init_repo_with_commit(path: Path) -> None:
	run = lambda *args: subprocess.run(["git", *args], cwd=path, check=True, capture_output=True)
	run("init")
	run("config", "user.email", "test@test")
	run("config", "user.name", "test")
	run("config", "commit.gpgsign", "false")
	(path / "seed.txt").write_text("seed\n")
	run("add", "seed.txt")
	run("commit", "-m", "init")


def review_output(findings: list[dict]) -> str:
	payload = json.dumps({"findings": findings})
	return json.dumps({"type": "message_end", "message": {"role": "assistant", "content": payload}})


class RunCodeReviewTests(unittest.TestCase):
	def test_required_policy_fails_on_high_finding_and_never_touches_git(self):
		with tempfile.TemporaryDirectory() as directory, tempfile.TemporaryDirectory() as spec_dir:
			root = Path(directory)
			init_repo_with_commit(root)
			spec_path = Path(spec_dir) / "ticket.md"
			spec_path.write_text("Add a widget.\n")
			completed = subprocess.CompletedProcess([], 0, review_output([
				{"severity": "high", "file": "a.go", "line": 12, "summary": "nil deref",
				 "failure_scenario": "calling Foo(nil) panics"},
			]), "")
			git_calls = []

			def side_effect(args, **kwargs):
				if args and args[0] == "git":
					git_calls.append(args[1])
					return subprocess.run(args, capture_output=True, text=True, cwd=kwargs.get("cwd"))
				return completed

			with mock.patch.object(build_app, "sh", side_effect=side_effect):
				evidence = code_review.run_code_review(root, spec_path=spec_path, review_policy="required")
			self.assertFalse(evidence["succeeded"])
			self.assertIn("1 blocking high-severity", evidence["stopped_reason"])
			self.assertEqual(evidence["findings"][0]["severity"], "high")
			self.assertNotIn("add", git_calls)
			self.assertNotIn("commit", git_calls)

	def test_required_policy_exit_code_is_1_on_blocking_finding(self):
		with tempfile.TemporaryDirectory() as directory:
			workspace = Path(directory) / "repo"
			workspace.mkdir()
			spec_path = Path(directory) / "ticket.md"
			spec_path.write_text("Add a widget.\n")
			completed = subprocess.CompletedProcess([], 0, review_output([
				{"severity": "high", "summary": "bug"},
			]), "")
			with mock.patch.object(sys, "argv", [
				str(SCRIPT), "--workspace", str(workspace), "--spec", str(spec_path),
			]), mock.patch.object(build_app, "sh", return_value=completed):
				self.assertEqual(code_review.main(), 1)

	def test_advisory_policy_with_high_finding_succeeds(self):
		with tempfile.TemporaryDirectory() as directory, tempfile.TemporaryDirectory() as spec_dir:
			root = Path(directory)
			init_repo_with_commit(root)
			spec_path = Path(spec_dir) / "ticket.md"
			spec_path.write_text("Add a widget.\n")
			completed = subprocess.CompletedProcess([], 0, review_output([
				{"severity": "high", "summary": "bug", "failure_scenario": "x"},
			]), "")
			with mock.patch.object(build_app, "sh", return_value=completed):
				evidence = code_review.run_code_review(
					root, spec_path=spec_path, review_policy="advisory", thinking="max",
				)
			self.assertTrue(evidence["succeeded"])
			self.assertEqual(evidence["thinking"], "max")
			self.assertIn("advisory policy", evidence["stopped_reason"])

	def test_no_findings_succeeds(self):
		with tempfile.TemporaryDirectory() as directory, tempfile.TemporaryDirectory() as spec_dir:
			root = Path(directory)
			init_repo_with_commit(root)
			spec_path = Path(spec_dir) / "ticket.md"
			spec_path.write_text("Add a widget.\n")
			completed = subprocess.CompletedProcess([], 0, review_output([]), "")
			with mock.patch.object(build_app, "sh", return_value=completed):
				evidence = code_review.run_code_review(root, spec_path=spec_path, review_policy="required")
			self.assertTrue(evidence["succeeded"])
			self.assertTrue(evidence["available"])
			self.assertEqual(evidence["findings"], [])
			self.assertIn("no high-severity findings", evidence["stopped_reason"])

	def test_unparseable_output_is_unavailable_and_fails_under_required(self):
		with tempfile.TemporaryDirectory() as directory, tempfile.TemporaryDirectory() as spec_dir:
			root = Path(directory)
			init_repo_with_commit(root)
			spec_path = Path(spec_dir) / "ticket.md"
			spec_path.write_text("Add a widget.\n")
			completed = subprocess.CompletedProcess([], 0, "not json at all", "")
			with mock.patch.object(build_app, "sh", return_value=completed):
				required = code_review.run_code_review(root, spec_path=spec_path, review_policy="required")
				advisory = code_review.run_code_review(root, spec_path=spec_path, review_policy="advisory")
			self.assertFalse(required["available"])
			self.assertFalse(required["succeeded"])
			self.assertFalse(advisory["available"])
			self.assertTrue(advisory["succeeded"])

	def test_relay_429_on_the_final_turn_is_named_in_stopped_reason_and_evidence(self):
		# Live example-app walk, 2026-09-28: the relay's sliding-window limiter
		# answered 429 "token budget exceeded" and the reviewer's final
		# turn carried that as its own errorMessage with no text -- before
		# this fix, CODE_REVIEW_EVIDENCE.json only ever said "no parseable
		# response from the reviewer", with the 429 itself invisible.
		with tempfile.TemporaryDirectory() as directory, tempfile.TemporaryDirectory() as spec_dir:
			root = Path(directory)
			init_repo_with_commit(root)
			spec_path = Path(spec_dir) / "ticket.md"
			spec_path.write_text("Add a widget.\n")
			error_output = json.dumps({
				"type": "message_end",
				"message": {"role": "assistant", "stopReason": "error", "errorMessage": '429 "token budget exceeded"'},
			})
			completed = subprocess.CompletedProcess([], 0, error_output, "")
			with mock.patch.object(build_app, "sh", return_value=completed):
				evidence = code_review.run_code_review(root, spec_path=spec_path, review_policy="required")
			self.assertFalse(evidence["available"])
			self.assertFalse(evidence["succeeded"])
			self.assertIn("429", evidence["stopped_reason"])
			self.assertIn("token budget exceeded", evidence["stopped_reason"])
			self.assertIn("429", evidence["error"])

	def test_writes_a_separate_evidence_file(self):
		with tempfile.TemporaryDirectory() as directory:
			root = Path(directory)
			evidence = {"schema_version": 1, "succeeded": True, "findings": []}
			path = code_review.write_code_review_evidence_json(root, evidence)
			self.assertEqual(path.name, "CODE_REVIEW_EVIDENCE.json")
			self.assertNotEqual(path.name, "BUILD_EVIDENCE.json")
			self.assertNotEqual(path.name, "CONFORMITY_EVIDENCE.json")
			self.assertEqual(json.loads(path.read_text()), evidence)

	def test_run_code_review_puts_thinking_in_the_real_pi_argv(self):
		with tempfile.TemporaryDirectory() as directory:
			workspace = Path(directory) / "repo"
			workspace.mkdir()
			spec_path = Path(directory) / "ticket.md"
			spec_path.write_text("Add a widget.\n")
			for thinking in ("max", None):
				commands = []
				completed_output = review_output([])

				def fake_sh(args, cwd=None, timeout=None, env=None):
					commands.append(args)
					return subprocess.CompletedProcess(args, 0, completed_output, "")

				with self.subTest(thinking=thinking), mock.patch.object(build_app, "sh", side_effect=fake_sh):
					evidence = code_review.run_code_review(workspace, spec_path=spec_path, thinking=thinking)
					self.assertTrue(evidence["succeeded"])
					pi_commands = [c for c in commands if "--print" in c]
					self.assertEqual(len(pi_commands), 1)
					if thinking is None:
						self.assertNotIn("--thinking", pi_commands[0])
					else:
						self.assertEqual(pi_commands[0][pi_commands[0].index("--thinking") + 1], "max")

	def test_main_thinking_option_accepts_every_level_and_rejects_unknown(self):
		base_argv = [str(SCRIPT), "--workspace", "/tmp/work", "--spec", "/tmp/ticket.md"]
		levels = build_app.THINKING_LEVELS
		for level in (*levels, None):
			with self.subTest(level=level):
				args = base_argv + ([] if level is None else ["--thinking", level])
				evidence = {"succeeded": True, "stopped_reason": "ok"}
				with (
					mock.patch.object(sys, "argv", args),
					mock.patch.object(code_review, "run_code_review", return_value=evidence) as run_review,
					mock.patch.object(code_review, "write_code_review_evidence_json", return_value=Path("/tmp/evidence.json")),
				):
					self.assertEqual(code_review.main(), 0)
				self.assertEqual(run_review.call_args.kwargs["thinking"], level)

		with mock.patch.object(sys, "argv", base_argv + ["--thinking", "unknown"]):
			with contextlib.redirect_stderr(io.StringIO()):
				with self.assertRaises(SystemExit) as raised:
					code_review.main()
		self.assertEqual(raised.exception.code, 2)


class ParseCodeReviewFindingsTests(unittest.TestCase):
	def test_last_json_object_wins(self):
		text = (
			json.dumps({"findings": [{"severity": "high", "summary": "stale example"}]})
			+ "\n"
			+ json.dumps({"findings": [{"severity": "low", "summary": "real finding"}]})
		)
		available, findings = code_review.parse_code_review_findings(text)
		self.assertTrue(available)
		self.assertEqual(len(findings), 1)
		self.assertEqual(findings[0]["summary"], "real finding")

	def test_prose_around_json_is_ignored(self):
		text = "Here is my review:\n" + json.dumps({"findings": [{"severity": "medium", "summary": "x"}]}) + "\nThanks!"
		available, findings = code_review.parse_code_review_findings(text)
		self.assertTrue(available)
		self.assertEqual(len(findings), 1)

	def test_unknown_severity_defaults_to_medium(self):
		text = json.dumps({"findings": [{"severity": "critical", "summary": "x"}]})
		_, findings = code_review.parse_code_review_findings(text)
		self.assertEqual(findings[0]["severity"], "medium")

	def test_missing_severity_defaults_to_medium(self):
		text = json.dumps({"findings": [{"summary": "x"}]})
		_, findings = code_review.parse_code_review_findings(text)
		self.assertEqual(findings[0]["severity"], "medium")

	def test_bool_line_is_rejected_to_zero(self):
		text = json.dumps({"findings": [{"severity": "low", "summary": "x", "line": True}]})
		_, findings = code_review.parse_code_review_findings(text)
		self.assertEqual(findings[0]["line"], 0)

	def test_negative_line_is_rejected_to_zero(self):
		text = json.dumps({"findings": [{"severity": "low", "summary": "x", "line": -5}]})
		_, findings = code_review.parse_code_review_findings(text)
		self.assertEqual(findings[0]["line"], 0)

	def test_entry_without_summary_is_dropped(self):
		text = json.dumps({"findings": [{"severity": "high", "line": 1}, {"severity": "low", "summary": "kept"}]})
		_, findings = code_review.parse_code_review_findings(text)
		self.assertEqual(len(findings), 1)
		self.assertEqual(findings[0]["summary"], "kept")

	def test_no_findings_key_at_all_is_unavailable(self):
		available, findings = code_review.parse_code_review_findings("no json here")
		self.assertFalse(available)
		self.assertEqual(findings, [])

	def test_empty_findings_list_is_available_and_empty(self):
		text = json.dumps({"findings": []})
		available, findings = code_review.parse_code_review_findings(text)
		self.assertTrue(available)
		self.assertEqual(findings, [])


class CodeReviewPromptTest(unittest.TestCase):
	def test_prompt_forbids_style_findings_and_running_verify_commands(self):
		prompt = code_review.code_review_prompt("Ticket spec text.", "abc123")
		self.assertIn("Do NOT report style, formatting, naming, lint", prompt)
		self.assertIn("cannot run to completion here", prompt)
		self.assertIn("Do NOT run them", prompt)
		self.assertIn("Ticket spec text.", prompt)
		self.assertIn("abc123", prompt)

	def test_prompt_with_diff_inlines_it_and_forbids_re_fetching(self):
		prompt = code_review.code_review_prompt(
			"Ticket spec text.", "abc123", diff=(" 1 file changed\n", "diff --git a/x.go b/x.go\n+line\n"),
		)
		self.assertIn("do NOT re-run `git diff`", prompt)
		self.assertIn("1 file changed", prompt)
		self.assertIn("diff --git a/x.go b/x.go", prompt)
		# The old "diff the workspace yourself" instruction must be gone --
		# otherwise the reviewer has no reason not to re-fetch it anyway.
		self.assertNotIn("Diff the workspace against", prompt)

	def test_prompt_with_diff_truncates_past_the_limit(self):
		big_diff = "x" * (build_app._DIFF_TRUNCATE_LIMIT + 500)
		prompt = code_review.code_review_prompt("spec", "abc123", diff=("stat\n", big_diff))
		self.assertIn("truncated here at 120,000 characters", prompt)
		self.assertNotIn(big_diff, prompt)
		self.assertIn("x" * build_app._DIFF_TRUNCATE_LIMIT, prompt)


class RunCodeReviewDiffFetchTests(unittest.TestCase):
	"""_workspace_diff/_DIFF_TRUNCATE_LIMIT used to be private copies here;
	moved to build_app.workspace_diff/build_app._DIFF_TRUNCATE_LIMIT (see
	build_app_test.py's own WorkspaceDiffTest) so conformity_review.py can
	share them. This class keeps only the run_code_review-level coverage
	of a failed diff fetch."""

	def test_run_code_review_notes_diff_fetch_failure_when_unavailable(self):
		with tempfile.TemporaryDirectory() as directory, tempfile.TemporaryDirectory() as spec_dir:
			root = Path(directory)
			init_repo_with_commit(root)
			spec_path = Path(spec_dir) / "ticket.md"
			spec_path.write_text("Add a widget.\n")
			completed = subprocess.CompletedProcess([], 0, "not json at all", "")

			def side_effect(args, **kwargs):
				if args and args[0] == "git":
					return subprocess.run(args, capture_output=True, text=True, cwd=kwargs.get("cwd"))
				return completed

			with mock.patch.object(build_app, "sh", side_effect=side_effect):
				evidence = code_review.run_code_review(
					root, spec_path=spec_path, review_policy="required", review_base_sha="0" * 40,
				)
			self.assertFalse(evidence["available"])
			self.assertIn("git diff for the reviewer's prompt failed", evidence["stopped_reason"])


if __name__ == "__main__":
	unittest.main()


class CrashedReviewEvidenceTest(unittest.TestCase):
	def test_main_writes_unavailable_evidence_naming_the_crash(self):
		with tempfile.TemporaryDirectory() as directory, tempfile.TemporaryDirectory() as spec_dir:
			root = Path(directory)
			spec_path = Path(spec_dir) / "ticket.md"
			spec_path.write_text("spec\n")

			def boom(*_args, **_kwargs):
				raise UnicodeDecodeError("utf-8", b"\x9d", 0, 1, "invalid start byte")

			argv = ["code_review.py", "--workspace", str(root), "--spec", str(spec_path), "--review-policy", "required"]
			with mock.patch.object(code_review, "run_code_review", boom), mock.patch.object(sys, "argv", argv):
				exit_code = code_review.main()

			evidence = json.loads((root / "CODE_REVIEW_EVIDENCE.json").read_text())
			self.assertEqual(exit_code, 1)
			self.assertFalse(evidence["available"])
			self.assertFalse(evidence["succeeded"])
			self.assertIn("UnicodeDecodeError", evidence["error"])
			self.assertIn("crashed", evidence["stopped_reason"])

	def test_harness_prepare_crash_still_writes_evidence(self):
		# pifork's prepare() reads and writes state files; its failure
		# (a malformed permission config, an unwritable agent dir) must
		# land in the crash evidence like any other review crash.
		with tempfile.TemporaryDirectory() as directory, tempfile.TemporaryDirectory() as spec_dir:
			root = Path(directory)
			spec_path = Path(spec_dir) / "ticket.md"
			spec_path.write_text("spec\n")

			def broken_prepare(_self):
				raise OSError("agent dir not writable")

			argv = ["code_review.py", "--workspace", str(root), "--spec", str(spec_path), "--review-policy", "required", "--harness", "pifork"]
			with mock.patch.object(harness_adapters.PiforkAdapter, "prepare", broken_prepare), mock.patch.object(sys, "argv", argv):
				exit_code = code_review.main()

			evidence = json.loads((root / "CODE_REVIEW_EVIDENCE.json").read_text())
			self.assertEqual(exit_code, 1)
			self.assertFalse(evidence["available"])
			self.assertIn("agent dir not writable", evidence["error"])

	def test_advisory_crash_still_succeeds(self):
		evidence = code_review.crashed_review_evidence("advisory", None, RuntimeError("x"))
		self.assertTrue(evidence["succeeded"])
		self.assertFalse(evidence["available"])
