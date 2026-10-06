from __future__ import annotations

import ast
import importlib.util
import io
import json
import subprocess
import sys
import tempfile
import unittest
import contextlib
from pathlib import Path
from unittest import mock

BUILD_APP_SCRIPT = Path(__file__).resolve().parents[1] / "scripts" / "build_app.py"
BUILD_APP_SPEC = importlib.util.spec_from_file_location("build_app", BUILD_APP_SCRIPT)
assert BUILD_APP_SPEC and BUILD_APP_SPEC.loader
build_app = importlib.util.module_from_spec(BUILD_APP_SPEC)
sys.modules[BUILD_APP_SPEC.name] = build_app
BUILD_APP_SPEC.loader.exec_module(build_app)
harness_adapters = sys.modules["harness_adapters"]

DRAFT_SPEC_SCRIPT = Path(__file__).resolve().parents[1] / "scripts" / "draft_spec.py"
DRAFT_SPEC_SPEC = importlib.util.spec_from_file_location("draft_spec", DRAFT_SPEC_SCRIPT)
assert DRAFT_SPEC_SPEC and DRAFT_SPEC_SPEC.loader
draft_spec = importlib.util.module_from_spec(DRAFT_SPEC_SPEC)
sys.modules[DRAFT_SPEC_SPEC.name] = draft_spec
DRAFT_SPEC_SPEC.loader.exec_module(draft_spec)

VALID_SPEC = """\
# Spec

## Problem

Something is broken.

## Scope

Just this service.

## Non-goals

Not that.

## Affected services and packages

internal/foo

## Acceptance criteria

1. It works.

## Risks

None.

## Open questions

None.
"""


def init_repo(path: Path) -> None:
	run = lambda *args: subprocess.run(["git", *args], cwd=path, check=True, capture_output=True)
	run("init")
	run("config", "user.email", "test@test")
	run("config", "user.name", "test")
	run("config", "commit.gpgsign", "false")
	(path / "seed.txt").write_text("seed\n")
	run("add", "seed.txt")
	run("commit", "-m", "init")


def scripted_pi(
	exit_code: int, stdout: str, *, writes_draft: str | None, draft_path: Path, stderr: str = "", commands: list | None = None,
):
	"""build_app.sh() side_effect: routes any git argv to the real
	subprocess (committed_agents_md_blob and file_inventory both call sh() for git
	commands), and only scripts the actual pi/claude agent invocation --
	mirrors build_app_test.py's own real_git_and_scripted_agent."""
	real_sh = build_app.sh

	def side_effect(args, cwd=None, timeout=None, env=None):
		if args and args[0] == "git":
			return real_sh(args, cwd=cwd, timeout=timeout, env=env)
		if commands is not None:
			commands.append(args)
		if writes_draft is not None:
			draft_path.parent.mkdir(parents=True, exist_ok=True)
			draft_path.write_text(writes_draft)
		return subprocess.CompletedProcess(args, exit_code, stdout, stderr)

	return side_effect


class BuildPromptTests(unittest.TestCase):
	def test_includes_request_text_and_skeleton_instructions(self):
		with tempfile.TemporaryDirectory() as directory:
			workspace = Path(directory)
			init_repo(workspace)
			prompt, agents_md_used = draft_spec.build_prompt(
				"Add idempotency keys to POST /refunds", workspace, draft_spec.DRAFT_RELATIVE_PATH,
			)
		self.assertIn("Add idempotency keys to POST /refunds", prompt)
		self.assertIn("## Acceptance criteria", prompt)
		self.assertIn("not a plan", prompt.lower())
		self.assertFalse(agents_md_used)

	def test_forbids_criteria_about_the_tests_themselves(self):
		# Found live 2026-09-20 (step-10 bar runs): drafted criteria like "the suite
		# passes, including new coverage of criteria 2-9" were judged by the conformity
		# reviewer, which quarantined an otherwise accepted run over test-file gofmt.
		with tempfile.TemporaryDirectory() as directory:
			workspace = Path(directory)
			init_repo(workspace)
			prompt, _ = draft_spec.build_prompt("x", workspace, draft_spec.DRAFT_RELATIVE_PATH)
		flat = " ".join(prompt.split())
		self.assertIn("when the deliverable is product behaviour, do NOT include a criterion that is merely a proxy about the tests", flat)
		self.assertIn("When the request itself asks for tests or tooling", flat)
		self.assertIn('never "a test exists"', flat)
		self.assertIn("for a documentation deliverable -- the content the document must contain", flat)
		self.assertIn('"a test exists for X"', flat)
		self.assertIn("Behavioural criteria that name cases are fine and wanted", flat)

	def test_records_committed_agents_md_without_pasting_it(self):
		# The harness loads AGENTS.md into its own system prompt; a copy in
		# this prompt would reach the model twice.
		with tempfile.TemporaryDirectory() as directory:
			workspace = Path(directory)
			init_repo(workspace)
			run = lambda *args: subprocess.run(["git", *args], cwd=workspace, check=True, capture_output=True)
			(workspace / "AGENTS.md").write_text("Use tabs, not spaces.")
			run("add", "AGENTS.md")
			run("commit", "-m", "add AGENTS.md")

			prompt, agents_md_used = draft_spec.build_prompt("text", workspace, draft_spec.DRAFT_RELATIVE_PATH)
		self.assertNotIn("Use tabs, not spaces.", prompt)
		self.assertTrue(agents_md_used)

	def test_includes_architecture_and_readme_when_present(self):
		with tempfile.TemporaryDirectory() as directory:
			workspace = Path(directory)
			init_repo(workspace)
			(workspace / "ARCHITECTURE.md").write_text("Service A talks to Service B.")
			(workspace / "README.md").write_text("This repo does X.")

			prompt, _ = draft_spec.build_prompt("text", workspace, draft_spec.DRAFT_RELATIVE_PATH)
		self.assertIn("Service A talks to Service B.", prompt)
		self.assertIn("This repo does X.", prompt)

	def test_includes_capped_file_inventory(self):
		with tempfile.TemporaryDirectory() as directory:
			workspace = Path(directory)
			init_repo(workspace)
			prompt, _ = draft_spec.build_prompt("text", workspace, draft_spec.DRAFT_RELATIVE_PATH)
		self.assertIn("seed.txt", prompt)

	def test_includes_operator_feedback_when_given(self):
		# A spec_review rejection note must actually reach the redrafting
		# prompt, delimited so the model can tell it apart from the
		# request text itself.
		with tempfile.TemporaryDirectory() as directory:
			workspace = Path(directory)
			init_repo(workspace)
			prompt, _ = draft_spec.build_prompt(
				"text", workspace, draft_spec.DRAFT_RELATIVE_PATH,
				feedback="require TypeError; name files test_sub.py/test_div.py",
			)
		self.assertIn("<<<BEGIN OPERATOR FEEDBACK>>>", prompt)
		self.assertIn("require TypeError; name files test_sub.py/test_div.py", prompt)
		self.assertIn("<<<END OPERATOR FEEDBACK>>>", prompt)

	def test_includes_the_design_guide_before_feedback_and_instructions(self):
		with tempfile.TemporaryDirectory() as directory:
			workspace = Path(directory)
			init_repo(workspace)
			prompt, _ = draft_spec.build_prompt(
				"text", workspace, draft_spec.DRAFT_RELATIVE_PATH,
				feedback="shorter", design_guide="1. Delivery. Does the caller need the result?\n",
			)
		self.assertIn("## Team design guide: spec decisions", prompt)
		guide = prompt.index("<<<BEGIN DESIGN GUIDE>>>\n1. Delivery. Does the caller need the result?\n<<<END DESIGN GUIDE>>>")
		self.assertLess(guide, prompt.index("<<<BEGIN OPERATOR FEEDBACK>>>"))
		self.assertLess(guide, prompt.index("# Spec"))

	def test_omits_the_design_guide_section_when_none_given(self):
		with tempfile.TemporaryDirectory() as directory:
			workspace = Path(directory)
			init_repo(workspace)
			prompt, _ = draft_spec.build_prompt("text", workspace, draft_spec.DRAFT_RELATIVE_PATH, design_guide="  \n")
		self.assertNotIn("DESIGN GUIDE", prompt)
		self.assertNotIn("Team design guide", prompt)

	def test_includes_the_handed_over_spec_to_revise_before_the_feedback(self):
		with tempfile.TemporaryDirectory() as directory:
			workspace = Path(directory)
			init_repo(workspace)
			prompt, _ = draft_spec.build_prompt(
				"text", workspace, draft_spec.DRAFT_RELATIVE_PATH,
				feedback="state the retention period", previous_draft="# Spec\n\n## Problem\n\nRefunds double-process.\n",
			)
		self.assertIn("Do not redraft it from the request", " ".join(prompt.split()))
		previous = prompt.index("<<<BEGIN CURRENT SPEC>>>\n# Spec\n\n## Problem\n\nRefunds double-process.\n<<<END CURRENT SPEC>>>")
		self.assertLess(previous, prompt.index("<<<BEGIN OPERATOR FEEDBACK>>>"))

	def test_omits_the_previous_draft_section_when_none_given(self):
		with tempfile.TemporaryDirectory() as directory:
			workspace = Path(directory)
			init_repo(workspace)
			prompt, _ = draft_spec.build_prompt("text", workspace, draft_spec.DRAFT_RELATIVE_PATH, feedback="shorter")
		self.assertNotIn("CURRENT SPEC", prompt)

	def test_omits_operator_feedback_section_when_none_given(self):
		with tempfile.TemporaryDirectory() as directory:
			workspace = Path(directory)
			init_repo(workspace)
			prompt, _ = draft_spec.build_prompt("text", workspace, draft_spec.DRAFT_RELATIVE_PATH)
		self.assertNotIn("OPERATOR FEEDBACK", prompt)

	def test_instructs_carrying_over_named_files_and_functions_verbatim(self):
		# Onboarding P4: the approved spec is the only input later oracle
		# drafting sees, so a spec that abstracts "sub.py's
		# subtract_numbers" into "a subtraction operation" leaves later,
		# independently-drafted oracles to guess the module name themselves
		# (live defect, 2026-09-24: four criteria guessed four different
		# modules for the same two functions).
		with tempfile.TemporaryDirectory() as directory:
			workspace = Path(directory)
			init_repo(workspace)
			prompt, _ = draft_spec.build_prompt("text", workspace, draft_spec.DRAFT_RELATIVE_PATH)
		flat = " ".join(prompt.split())
		self.assertIn("carry that name over verbatim here rather than abstracting it", flat)
		self.assertIn("sub.py", flat)
		self.assertIn("subtract_numbers", flat)
		# ...and in each criterion, since oracles are drafted from the
		# criteria alone (live, 2026-09-24: Scope named sub.py, the
		# criteria did not, and the drafted oracles invented modules).
		self.assertIn("put it IN each criterion that exercises it", flat)
		self.assertIn("`subtract_numbers(a, b)` in `sub.py`", flat)
		self.assertIn("carry over verbatim any file, module, or function/API name the request itself states", flat)

	def test_instructs_private_ambiguity_triage_and_bounded_decisions(self):
		with tempfile.TemporaryDirectory() as directory:
			workspace = Path(directory)
			init_repo(workspace)
			prompt, _ = draft_spec.build_prompt("x", workspace, draft_spec.DRAFT_RELATIVE_PATH)
		flat = " ".join(prompt.split())
		self.assertIn("private ambiguity and quality pass", flat)
		self.assertIn("scope and actors", flat)
		self.assertIn("data identity and lifecycle", flat)
		self.assertIn("external dependencies and failure modes", flat)
		self.assertIn("Use a reasonable, repository-supported default for minor gaps", flat)
		self.assertIn("[NEEDS DECISION]", flat)
		self.assertIn("at most three", flat)
		self.assertIn("recommended option", flat)
		self.assertIn("why the decision matters", flat)
		self.assertIn("Never create a blocking decision for style", flat)

	def test_instructs_silent_quality_review_and_feedback_integration(self):
		with tempfile.TemporaryDirectory() as directory:
			workspace = Path(directory)
			init_repo(workspace)
			prompt, _ = draft_spec.build_prompt(
				"x", workspace, draft_spec.DRAFT_RELATIVE_PATH,
				feedback="Use optimistic locking for concurrent edits.",
			)
		flat = " ".join(prompt.split())
		self.assertIn("primary success flow and relevant negative, error, and boundary behaviour", flat)
		self.assertIn("Record consequential assumptions and dependencies under Risks", flat)
		self.assertIn("integrate the answer into every affected section", flat)
		self.assertIn("Do not emit a `[NEEDS DECISION]` marker for that resolved issue", flat)
		self.assertIn("keep the draft focused on the original request and feedback", flat)
		self.assertIn("privately check the finished draft", flat)
		self.assertIn("Do not write that review or a checklist into the spec", flat)


class ReadCappedTextFileTests(unittest.TestCase):
	def test_returns_none_when_absent(self):
		with tempfile.TemporaryDirectory() as directory:
			self.assertIsNone(draft_spec.read_capped_text_file(Path(directory) / "MISSING.md", 100))

	def test_truncates_at_the_byte_cap(self):
		with tempfile.TemporaryDirectory() as directory:
			path = Path(directory) / "BIG.md"
			path.write_text("x" * 200)
			text = draft_spec.read_capped_text_file(path, 100)
		self.assertLessEqual(len(text), 100 + len("\n\n[... BIG.md truncated at 0KB ...]\n"))
		self.assertIn("truncated", text)


class RunDraftTests(unittest.TestCase):
	def test_success_writes_out_and_evidence(self):
		with tempfile.TemporaryDirectory() as directory:
			workspace = Path(directory) / "repo"
			workspace.mkdir()
			init_repo(workspace)
			request_path = Path(directory) / "request.md"
			request_path.write_text("Add a feature")
			out_path = Path(directory) / "spec.md"
			evidence_path = Path(directory) / "evidence.json"
			draft_path = workspace / draft_spec.DRAFT_RELATIVE_PATH

			usage_output = json.dumps({
				"type": "agent_end",
				"messages": [{"role": "assistant", "usage": {"input": 10, "output": 5}}],
			})
			with mock.patch.object(
				build_app, "sh",
				side_effect=scripted_pi(0, usage_output, writes_draft=VALID_SPEC, draft_path=draft_path),
			):
				exit_code = draft_spec.run_draft(workspace, request_path, out_path, evidence_path, timeout_minutes=1)

			self.assertEqual(exit_code, 0)
			self.assertEqual(out_path.read_text(), VALID_SPEC)
			evidence = json.loads(evidence_path.read_text())
			self.assertEqual(evidence["agent_exit_code"], 0)
			self.assertEqual(evidence["usage"], {"input": 10, "output": 5})
			self.assertFalse(evidence["agents_md_used"])
			self.assertIsNone(evidence["thinking"])
			self.assertIn("duration_s", evidence)

	def test_run_draft_builds_max_thinking_argv_and_omits_flag_when_unset(self):
		for thinking in ("max", None):
			with self.subTest(thinking=thinking), tempfile.TemporaryDirectory() as directory:
				workspace = Path(directory) / "repo"
				workspace.mkdir()
				init_repo(workspace)
				request_path = Path(directory) / "request.md"
				request_path.write_text("Add a feature")
				out_path = Path(directory) / "spec.md"
				evidence_path = Path(directory) / "evidence.json"
				commands = []
				with mock.patch.object(
					build_app, "sh",
					side_effect=scripted_pi(
						0, "", writes_draft=VALID_SPEC,
						draft_path=workspace / draft_spec.DRAFT_RELATIVE_PATH, commands=commands,
					),
				):
					draft_spec.run_draft(
						workspace, request_path, out_path, evidence_path, timeout_minutes=1,
						thinking=thinking,
					)
				self.assertEqual(json.loads(evidence_path.read_text())["thinking"], thinking)
			self.assertEqual(len(commands), 1)
			if thinking is None:
				self.assertNotIn("--thinking", commands[0])
			else:
				self.assertEqual(commands[0][commands[0].index("--thinking") + 1], "max")

	def test_main_thinking_option_accepts_every_level_and_rejects_unknown(self):
		base_argv = [
			str(DRAFT_SPEC_SCRIPT), "--request", "/tmp/request.md", "--workspace", "/tmp/work",
			"--out", "/tmp/spec.md", "--evidence", "/tmp/evidence.json",
		]
		levels = build_app.THINKING_LEVELS
		for level in (*levels, None):
			with self.subTest(level=level):
				args = base_argv + ([] if level is None else ["--thinking", level])
				with mock.patch.object(sys, "argv", args), mock.patch.object(draft_spec, "run_draft", return_value=0) as run_draft:
					self.assertEqual(draft_spec.main(), 0)
				self.assertEqual(run_draft.call_args.kwargs["thinking"], level)

		with mock.patch.object(sys, "argv", base_argv + ["--thinking", "unknown"]):
			with contextlib.redirect_stderr(io.StringIO()):
				with self.assertRaises(SystemExit) as raised:
					draft_spec.main()
		self.assertEqual(raised.exception.code, 2)

	def test_feedback_file_reaches_the_prompt(self):
		# run_draft's own --feedback-file plumbing, end to end from a
		# file on disk to the prompt build_prompt hands to pi_invocation.
		with tempfile.TemporaryDirectory() as directory:
			workspace = Path(directory) / "repo"
			workspace.mkdir()
			init_repo(workspace)
			request_path = Path(directory) / "request.md"
			request_path.write_text("Add a feature")
			out_path = Path(directory) / "spec.md"
			evidence_path = Path(directory) / "evidence.json"
			draft_path = workspace / draft_spec.DRAFT_RELATIVE_PATH
			feedback_path = Path(directory) / "spec-feedback.md"
			feedback_path.write_text("## Rejected by alice\n\nname the test files explicitly")

			with mock.patch.object(harness_adapters.PiAdapter, "invocation", autospec=True, wraps=harness_adapters.PiAdapter.invocation) as spy_pi_invocation:
				with mock.patch.object(
					build_app, "sh",
					side_effect=scripted_pi(0, "", writes_draft=VALID_SPEC, draft_path=draft_path),
				):
					exit_code = draft_spec.run_draft(
						workspace, request_path, out_path, evidence_path, timeout_minutes=1,
						feedback_path=feedback_path,
					)

			self.assertEqual(exit_code, 0)
			spy_pi_invocation.assert_called_once()
			self.assertIn("name the test files explicitly", spy_pi_invocation.call_args.kwargs["prompt"])
			self.assertIn("<<<BEGIN OPERATOR FEEDBACK>>>", spy_pi_invocation.call_args.kwargs["prompt"])

	def test_pi_failure_writes_evidence_but_not_out_and_exits_2(self):
		with tempfile.TemporaryDirectory() as directory:
			workspace = Path(directory) / "repo"
			workspace.mkdir()
			init_repo(workspace)
			request_path = Path(directory) / "request.md"
			request_path.write_text("Add a feature")
			out_path = Path(directory) / "spec.md"
			evidence_path = Path(directory) / "evidence.json"
			draft_path = workspace / draft_spec.DRAFT_RELATIVE_PATH

			import contextlib
			import io
			stderr = io.StringIO()
			with contextlib.redirect_stderr(stderr):
				with mock.patch.object(
					build_app, "sh",
					side_effect=scripted_pi(1, "", writes_draft=None, draft_path=draft_path),
				):
					exit_code = draft_spec.run_draft(workspace, request_path, out_path, evidence_path, timeout_minutes=1)

			self.assertEqual(exit_code, 2)
			self.assertFalse(out_path.exists())
			evidence = json.loads(evidence_path.read_text())
			self.assertEqual(evidence["agent_exit_code"], 1)
			# Same fix as plan_tickets.py -- a failed pass must never leave an
			# empty log.
			self.assertIn("agent exited 1", stderr.getvalue())

	def test_pi_failure_includes_a_redacted_hint_from_pi_stderr(self):
		# Found in adversarial review of that fix: same fix as
		# plan_tickets.py -- pi's own stderr is the only place an auth
		# failure/401/route-down reason lives, and build_app.sh captures
		# it but nothing ever printed it.
		with tempfile.TemporaryDirectory() as directory:
			workspace = Path(directory) / "repo"
			workspace.mkdir()
			init_repo(workspace)
			request_path = Path(directory) / "request.md"
			request_path.write_text("Add a feature")
			out_path = Path(directory) / "spec.md"
			evidence_path = Path(directory) / "evidence.json"
			draft_path = workspace / draft_spec.DRAFT_RELATIVE_PATH
			pi_stderr = (
				"Traceback (most recent call last):\n"
				"  File \"pi.py\", line 1, in <module>\n"
				"RuntimeError: 401 Unauthorized: Authorization: Bearer sk-abcdefgh12345678\n"
			)

			import contextlib
			import io
			stderr = io.StringIO()
			with contextlib.redirect_stderr(stderr):
				with mock.patch.object(
					build_app, "sh",
					side_effect=scripted_pi(1, "", writes_draft=None, draft_path=draft_path, stderr=pi_stderr),
				):
					exit_code = draft_spec.run_draft(workspace, request_path, out_path, evidence_path, timeout_minutes=1)

			self.assertEqual(exit_code, 2)
			printed = stderr.getvalue()
			self.assertIn("draft_spec: agent exited 1:", printed)
			self.assertIn("401 Unauthorized", printed)
			self.assertNotIn("sk-abcdefgh12345678", printed)
			self.assertNotIn("Traceback", printed)
			reason_line = next(line for line in printed.splitlines() if line.startswith("draft_spec: agent exited 1"))
			self.assertNotIn("\n", reason_line)

	def test_no_draft_written_exits_2_even_on_a_clean_pi_exit(self):
		with tempfile.TemporaryDirectory() as directory:
			workspace = Path(directory) / "repo"
			workspace.mkdir()
			init_repo(workspace)
			request_path = Path(directory) / "request.md"
			request_path.write_text("Add a feature")
			out_path = Path(directory) / "spec.md"
			evidence_path = Path(directory) / "evidence.json"
			draft_path = workspace / draft_spec.DRAFT_RELATIVE_PATH

			import contextlib
			import io
			stderr = io.StringIO()
			with contextlib.redirect_stderr(stderr):
				with mock.patch.object(
					build_app, "sh",
					side_effect=scripted_pi(0, "", writes_draft=None, draft_path=draft_path),
				):
					exit_code = draft_spec.run_draft(workspace, request_path, out_path, evidence_path, timeout_minutes=1)

			self.assertEqual(exit_code, 2)
			self.assertFalse(out_path.exists())
			self.assertIn("agent exited 0 but wrote no spec draft", stderr.getvalue())

	def test_no_draft_written_exits_2_even_on_a_clean_pi_exit_names_the_model_route_error(self):
		STDOUT = '{"type":"message_end","message":{"stopReason":"error","errorMessage":"400 model gpt-5.6-luna is not supported via /chat/completions (token ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZ123456)"}}'
		with tempfile.TemporaryDirectory() as directory:
			workspace = Path(directory) / "repo"
			workspace.mkdir()
			init_repo(workspace)
			request_path = Path(directory) / "request.md"
			request_path.write_text("Add a feature")
			out_path = Path(directory) / "spec.md"
			evidence_path = Path(directory) / "evidence.json"
			draft_path = workspace / draft_spec.DRAFT_RELATIVE_PATH

			import contextlib
			import io
			stderr = io.StringIO()
			with contextlib.redirect_stderr(stderr):
				with mock.patch.object(
					build_app, "sh",
					side_effect=scripted_pi(0, STDOUT, writes_draft=None, draft_path=draft_path),
				):
					exit_code = draft_spec.run_draft(workspace, request_path, out_path, evidence_path, timeout_minutes=1)

			self.assertEqual(exit_code, 2)
			self.assertFalse(out_path.exists())
			self.assertIn("agent exited 0 but wrote no spec draft", stderr.getvalue())
			self.assertIn("wrote no spec draft; model route error: 400 model gpt-5.6-luna is not supported via /chat/completions", stderr.getvalue())
			self.assertNotIn("ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZ123456", stderr.getvalue())
			self.assertEqual(len(stderr.getvalue().strip().splitlines()), 1)

	def test_pi_timeout_writes_evidence_and_exits_2_instead_of_crashing(self):
		with tempfile.TemporaryDirectory() as directory:
			workspace = Path(directory) / "repo"
			workspace.mkdir()
			init_repo(workspace)
			request_path = Path(directory) / "request.md"
			request_path.write_text("Add a feature")
			out_path = Path(directory) / "spec.md"
			evidence_path = Path(directory) / "evidence.json"
			real_sh = build_app.sh

			def timeout(args, cwd=None, timeout=None, env=None):
				if args and args[0] == "git":
					return real_sh(args, cwd=cwd, timeout=timeout, env=env)
				raise subprocess.TimeoutExpired(args, timeout or 0, stderr=b"retrying\nupstream 429: rate limited\n")

			stderr = io.StringIO()
			with contextlib.redirect_stderr(stderr), mock.patch.object(build_app, "sh", side_effect=timeout):
				exit_code = draft_spec.run_draft(
					workspace, request_path, out_path, evidence_path, timeout_minutes=2,
				)

			self.assertEqual(exit_code, 2)
			self.assertFalse(out_path.exists())
			self.assertIn("draft_spec: agent timed out after 2 minutes: upstream 429: rate limited", stderr.getvalue())
			self.assertNotIn("Traceback", stderr.getvalue())
			evidence = json.loads(evidence_path.read_text())
			self.assertEqual(evidence["agent_exit_code"], -1)
			self.assertIsNone(evidence["thinking"])


class ExampleCheckTests(unittest.TestCase):
	def test_module_level_function_names_are_unique(self):
		tree = ast.parse(DRAFT_SPEC_SCRIPT.read_text())
		names = [node.name for node in tree.body if isinstance(node, ast.FunctionDef)]
		self.assertEqual(len(names), len(set(names)))

	def test_extracts_call_result_pairs(self):
		text = 'Eval("10 % 3 ^ 2") returns `10`. Also `Eval("2 + 3")` returns 5 and `f(x)` is fine.'
		text = text.replace('Eval("10 % 3 ^ 2")', '`Eval("10 % 3 ^ 2")`')
		self.assertEqual(draft_spec.extract_examples(text), [('Eval("10 % 3 ^ 2")', "10"), ('Eval("2 + 3")', "5")])

	def test_safe_eval(self):
		self.assertEqual(draft_spec.safe_int_eval("2 + 3 * (4 - 1)"), 11)
		self.assertEqual(draft_spec.safe_int_eval("10 % 3"), 1)
		for bad in ["__import__('os')", "2 ^ 3", "2 ** 9", "1 / 2", "-3 % 2", "10 % 0", "a + 1", "9*9*9*9*9*9*9*9*9*9*9*9*9*9", ""]:
			self.assertIsNone(draft_spec.safe_int_eval(bad), bad)

	def test_mechanical_flags_only_wrong_computable(self):
		ex = [('Eval("2 + 3")', "6"), ('Eval("2 + 3")', "5"), ('Eval("10 % 3 ^ 2")', "10"), ("2 * 4", "9")]
		w = draft_spec.mechanical_check(ex)
		self.assertEqual(len(w), 2)
		self.assertIn("gives 5", w[0])
		self.assertIn("gives 8", w[1])

	def test_mechanical_checks_floor_division(self):
		# main ran the copy without a `//` skip; the dedupe must keep that.
		w = draft_spec.mechanical_check([('Eval("7 // 2")', "4"), ('Eval("7 // 2")', "3")])
		self.assertEqual(len(w), 1)
		self.assertIn("gives 3", w[0])

	def test_model_recheck_skipped_when_job_budget_is_short(self):
		spec = VALID_SPEC.replace("1. It works.", '1. `g(4)` returns `7`.')
		with tempfile.TemporaryDirectory() as directory, mock.patch.object(draft_spec, "model_check") as model_check:
			_, info = draft_spec.check_worked_examples(Path(directory), spec, time_left_s=60)
		model_check.assert_not_called()
		self.assertEqual(info["model_check_skipped"], "only 60s of the job budget left")
		with tempfile.TemporaryDirectory() as directory, mock.patch.object(draft_spec, "model_check", return_value=([], None)) as model_check:
			_, info = draft_spec.check_worked_examples(Path(directory), spec, time_left_s=600)
		model_check.assert_called_once()
		self.assertNotIn("model_check_skipped", info)

	def test_parse_example_check(self):
		ex = [("a(1)", "10"), ("b(2)", "4"), ("c(3)", "7")]
		w = draft_spec.parse_example_check('{"1": "1", "2": "4", "3": "ambiguous"}', ex)
		self.assertEqual(len(w), 1)
		self.assertIn("gives 1", w[0])
		self.assertEqual(draft_spec.parse_example_check("not json", ex), [])
		self.assertEqual(draft_spec.parse_example_check("[1]", ex), [])

	def test_append_block_keeps_skeleton_and_noop_without_warnings(self):
		self.assertEqual(draft_spec.append_unverified_block(VALID_SPEC, []), VALID_SPEC)
		out = draft_spec.append_unverified_block(VALID_SPEC, ["`x` is stated as 1"])
		self.assertIn(draft_spec.UNVERIFIED_HEADING, out)
		self.assertTrue(out.index("## Open questions") < out.index(draft_spec.UNVERIFIED_HEADING))

	def test_check_never_raises_when_model_call_fails(self):
		spec = VALID_SPEC.replace("1. It works.", '1. `Eval("2 + 3")` returns `6`; `g(4)` returns `7`.')
		with tempfile.TemporaryDirectory() as directory:
			with mock.patch.object(build_app, "sh", side_effect=RuntimeError("boom")):
				out, info = draft_spec.check_worked_examples(Path(directory), spec)
		self.assertEqual(info["examples"], 2)
		self.assertEqual(info["warnings"], 1)
		self.assertIn("gives 5", out)

	def test_run_draft_annotates_spec_via_model_check(self):
		spec = VALID_SPEC.replace("1. It works.", "1. `g(4)` returns `7`.")
		with tempfile.TemporaryDirectory() as directory:
			workspace = Path(directory) / "repo"
			workspace.mkdir()
			init_repo(workspace)
			request_path = Path(directory) / "request.md"
			request_path.write_text("x")
			out_path = Path(directory) / "spec.md"
			evidence_path = Path(directory) / "evidence.json"
			draft_path = workspace / draft_spec.DRAFT_RELATIVE_PATH
			check_path = workspace / draft_spec.EXAMPLE_CHECK_RELATIVE_PATH
			real_sh = build_app.sh
			calls = []

			def fake(args, cwd=None, timeout=None, env=None):
				if args and args[0] == "git":
					return real_sh(args, cwd=cwd, timeout=timeout, env=env)
				calls.append(args)
				if len(calls) == 1:
					draft_path.parent.mkdir(parents=True, exist_ok=True)
					draft_path.write_text(spec)
				else:
					check_path.write_text('{"1": "8"}')
				stdout = "" if len(calls) == 1 else json.dumps({
					"type": "agent_end",
					"messages": [{"role": "assistant", "usage": {"input": 3, "output": 1}}],
				})
				return subprocess.CompletedProcess(args, 0, stdout, "")

			with mock.patch.object(build_app, "sh", side_effect=fake):
				code = draft_spec.run_draft(
					workspace, request_path, out_path, evidence_path, timeout_minutes=10, thinking="max",
				)
			self.assertEqual(code, 0)
			self.assertEqual(len(calls), 2)
			self.assertIn("--thinking", calls[0])
			self.assertEqual(calls[0][calls[0].index("--thinking") + 1], "max")
			# The recheck is pinned low, never the job's max.
			self.assertEqual(calls[1][calls[1].index("--thinking") + 1], draft_spec.EXAMPLE_CHECK_THINKING)
			self.assertEqual(draft_spec.EXAMPLE_CHECK_THINKING, "low")
			written = out_path.read_text()
			self.assertIn("gives 8", written)
			self.assertEqual(written.count(draft_spec.UNVERIFIED_HEADING), 1)
			evidence = json.loads(evidence_path.read_text())
			self.assertEqual(evidence["thinking"], "max")
			self.assertEqual(evidence["example_check"]["warnings"], 1)
			self.assertEqual(evidence["example_check"]["model_usage"], {"input": 3, "output": 1})


if __name__ == "__main__":
	unittest.main()
