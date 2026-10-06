from __future__ import annotations

import importlib.util
import contextlib
import io
import json
import subprocess
import sys
import tempfile
import unittest
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

PLAN_TICKETS_SCRIPT = Path(__file__).resolve().parents[1] / "scripts" / "plan_tickets.py"
PLAN_TICKETS_SPEC = importlib.util.spec_from_file_location("plan_tickets", PLAN_TICKETS_SCRIPT)
assert PLAN_TICKETS_SPEC and PLAN_TICKETS_SPEC.loader
plan_tickets = importlib.util.module_from_spec(PLAN_TICKETS_SPEC)
sys.modules[PLAN_TICKETS_SPEC.name] = plan_tickets
PLAN_TICKETS_SPEC.loader.exec_module(plan_tickets)

VALID_SPEC = """\
# Spec

## Problem

Something is broken.

## Acceptance criteria

1. It works.
2. It doesn't break anything else.
"""

VALID_TICKET = """\
Verify-Command: make verify
Allowed-Files: internal/foo/foo.go, internal/foo/foo_test.go
Required-Changed-Files: internal/foo/foo.go

## Goal

Fix the thing.

## Plan

### Files to touch

- internal/foo/foo.go

### Steps

1. Fix it.

### Tests to add

- internal/foo/foo_test.go

### Acceptance criteria covered

- 1
- 2

## Out of scope

Nothing else.
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
	exit_code: int, stdout: str, *, writes_tickets: dict[str, str] | None, draft_dir: Path, stderr: str = "", commands: list | None = None,
):
	"""build_app.sh() side_effect: routes any git argv to the real
	subprocess, scripts only the actual pi/claude agent invocation --
	mirrors draft_spec_test.py's own scripted_pi."""
	real_sh = build_app.sh

	def side_effect(args, cwd=None, timeout=None, env=None):
		if args and args[0] == "git":
			return real_sh(args, cwd=cwd, timeout=timeout, env=env)
		if commands is not None:
			commands.append(args)
		if writes_tickets is not None:
			draft_dir.mkdir(parents=True, exist_ok=True)
			for name, content in writes_tickets.items():
				(draft_dir / name).write_text(content)
		return subprocess.CompletedProcess(args, exit_code, stdout, stderr)

	return side_effect


class BuildPromptTests(unittest.TestCase):
	def test_includes_spec_request_and_instructions(self):
		with tempfile.TemporaryDirectory() as directory:
			workspace = Path(directory)
			init_repo(workspace)
			prompt, agents_md_used = plan_tickets.build_prompt(
				VALID_SPEC, "Add idempotency keys", workspace, "make verify", plan_tickets.DRAFT_RELATIVE_DIR,
			)
		self.assertIn("Something is broken.", prompt)
		self.assertIn("Add idempotency keys", prompt)
		self.assertIn("Verify-Command: make verify", prompt)
		self.assertIn("### Acceptance criteria covered", prompt)
		self.assertIn("dependency order", prompt.lower())
		self.assertFalse(agents_md_used)

	def test_states_the_tests_rule_and_its_opt_out(self):
		# Live evidence (example-app "reminders_streak MCP tool" and "habit
		# insights", both 2026-09-28): the planner split a ticket that only
		# wires a route, with no test file and no Tests-Required opt-out --
		# a plan writeAndValidateDraftedTickets/policy.TicketTestsAddedFeasible
		# (cmd/factoryd) now catches before plan_review. The prompt must
		# state the rule plainly so the planner stops drafting that shape in
		# the first place, not just get caught after the fact.
		with tempfile.TemporaryDirectory() as directory:
			workspace = Path(directory)
			init_repo(workspace)
			prompt, _ = plan_tickets.build_prompt(
				VALID_SPEC, "Add idempotency keys", workspace, "make verify", plan_tickets.DRAFT_RELATIVE_DIR,
			)
		self.assertIn("Tests-Required: no -- <reason>", prompt)
		self.assertIn("tests_added gate", prompt)
		self.assertIn("Required-Changed-Files", prompt)

	def test_states_the_multi_file_criterion_rule(self):
		# Live evidence (example-app habit-insights request, 2026-09-28): a criterion
		# naming three different tickets' own files was listed as covered
		# by only one of them, and that one ticket could never satisfy the
		# part of the criterion naming the other two tickets' files --
		# cmd/factoryd's writeAndValidateDraftedTickets now catches this
		# before plan_review, but the prompt must state the rule plainly
		# so the planner stops drafting that shape in the first place.
		with tempfile.TemporaryDirectory() as directory:
			workspace = Path(directory)
			init_repo(workspace)
			prompt, _ = plan_tickets.build_prompt(
				VALID_SPEC, "Add idempotency keys", workspace, "make verify", plan_tickets.DRAFT_RELATIVE_DIR,
			)
		self.assertIn("Multi-file criteria rule", prompt)
		self.assertIn("EVERY ticket that owns one of those files", prompt)

	def test_states_the_no_wiring_only_ticket_rule(self):
		with tempfile.TemporaryDirectory() as directory:
			workspace = Path(directory)
			init_repo(workspace)
			prompt, _ = plan_tickets.build_prompt(
				VALID_SPEC, "Add idempotency keys", workspace, "make verify", plan_tickets.DRAFT_RELATIVE_DIR,
			)
		self.assertIn("never split", prompt.lower())
		self.assertIn("wiring", prompt.lower())

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

			prompt, agents_md_used = plan_tickets.build_prompt(VALID_SPEC, "text", workspace, "make verify", plan_tickets.DRAFT_RELATIVE_DIR)
		self.assertNotIn("Use tabs, not spaces.", prompt)
		self.assertTrue(agents_md_used)

	def test_includes_capped_file_inventory(self):
		with tempfile.TemporaryDirectory() as directory:
			workspace = Path(directory)
			init_repo(workspace)
			prompt, _ = plan_tickets.build_prompt(VALID_SPEC, "text", workspace, "make verify", plan_tickets.DRAFT_RELATIVE_DIR)
		self.assertIn("seed.txt", prompt)

	def test_includes_operator_feedback_when_given(self):
		# A plan_review rejection note must reach the redrafting prompt,
		# delimited the same way draft_spec.py's own does.
		with tempfile.TemporaryDirectory() as directory:
			workspace = Path(directory)
			init_repo(workspace)
			prompt, _ = plan_tickets.build_prompt(
				VALID_SPEC, "text", workspace, "make verify", plan_tickets.DRAFT_RELATIVE_DIR,
				feedback="split ticket 1 -- it touches two unrelated packages",
			)
		self.assertIn("<<<BEGIN OPERATOR FEEDBACK>>>", prompt)
		self.assertIn("split ticket 1 -- it touches two unrelated packages", prompt)
		self.assertIn("<<<END OPERATOR FEEDBACK>>>", prompt)

	def test_includes_the_design_guide_before_feedback_and_instructions(self):
		with tempfile.TemporaryDirectory() as directory:
			workspace = Path(directory)
			init_repo(workspace)
			prompt, _ = plan_tickets.build_prompt(
				VALID_SPEC, "text", workspace, "make verify", plan_tickets.DRAFT_RELATIVE_DIR,
				feedback="split it", design_guide="- Layers: handler -> service -> repository.",
			)
		self.assertIn("## Team design guide: plan rules", prompt)
		guide = prompt.index("<<<BEGIN DESIGN GUIDE>>>\n- Layers: handler -> service -> repository.\n<<<END DESIGN GUIDE>>>")
		self.assertLess(guide, prompt.index("<<<BEGIN OPERATOR FEEDBACK>>>"))
		self.assertLess(guide, prompt.index("Decompose the approved spec"))

	def test_omits_the_design_guide_section_when_none_given(self):
		with tempfile.TemporaryDirectory() as directory:
			workspace = Path(directory)
			init_repo(workspace)
			prompt, _ = plan_tickets.build_prompt(VALID_SPEC, "text", workspace, "make verify", plan_tickets.DRAFT_RELATIVE_DIR)
		self.assertNotIn("DESIGN GUIDE", prompt)

	def test_includes_the_handed_over_tickets_to_revise_before_the_feedback(self):
		with tempfile.TemporaryDirectory() as directory:
			workspace = Path(directory)
			init_repo(workspace)
			prompt, _ = plan_tickets.build_prompt(
				VALID_SPEC, "text", workspace, "make verify", plan_tickets.DRAFT_RELATIVE_DIR,
				feedback="name the migration file", previous_draft="=== 001.spec.md ===\nVerify-Command: make verify\n",
			)
		self.assertIn("Do not plan afresh from the spec", " ".join(prompt.split()))
		previous = prompt.index("<<<BEGIN CURRENT TICKETS>>>\n=== 001.spec.md ===\nVerify-Command: make verify\n<<<END CURRENT TICKETS>>>")
		self.assertLess(previous, prompt.index("<<<BEGIN OPERATOR FEEDBACK>>>"))

	def test_omits_the_previous_tickets_section_when_none_given(self):
		with tempfile.TemporaryDirectory() as directory:
			workspace = Path(directory)
			init_repo(workspace)
			prompt, _ = plan_tickets.build_prompt(VALID_SPEC, "text", workspace, "make verify", plan_tickets.DRAFT_RELATIVE_DIR, feedback="x")
		self.assertNotIn("CURRENT TICKETS", prompt)

	def test_omits_operator_feedback_section_when_none_given(self):
		with tempfile.TemporaryDirectory() as directory:
			workspace = Path(directory)
			init_repo(workspace)
			prompt, _ = plan_tickets.build_prompt(VALID_SPEC, "text", workspace, "make verify", plan_tickets.DRAFT_RELATIVE_DIR)
		self.assertNotIn("OPERATOR FEEDBACK", prompt)


class RunPlanTests(unittest.TestCase):
	def test_success_writes_tickets_and_evidence(self):
		with tempfile.TemporaryDirectory() as directory:
			workspace = Path(directory) / "repo"
			workspace.mkdir()
			init_repo(workspace)
			spec_path = Path(directory) / "spec.md"
			spec_path.write_text(VALID_SPEC)
			request_path = Path(directory) / "request.md"
			request_path.write_text("Add a feature")
			out_dir = Path(directory) / "tickets"
			evidence_path = Path(directory) / "evidence.json"
			draft_dir = workspace / plan_tickets.DRAFT_RELATIVE_DIR

			usage_output = json.dumps({
				"type": "agent_end",
				"messages": [{"role": "assistant", "usage": {"input": 10, "output": 5}}],
			})
			tickets = {"001.spec.md": VALID_TICKET, "002.spec.md": VALID_TICKET}
			with mock.patch.object(
				build_app, "sh",
				side_effect=scripted_pi(0, usage_output, writes_tickets=tickets, draft_dir=draft_dir),
			):
				exit_code = plan_tickets.run_plan(workspace, spec_path, request_path, "make verify", out_dir, evidence_path, timeout_minutes=1)

			self.assertEqual(exit_code, 0)
			self.assertEqual((out_dir / "001.spec.md").read_text(), VALID_TICKET)
			self.assertEqual((out_dir / "002.spec.md").read_text(), VALID_TICKET)
			evidence = json.loads(evidence_path.read_text())
			self.assertEqual(evidence["agent_exit_code"], 0)
			self.assertEqual(evidence["usage"], {"input": 10, "output": 5})
			self.assertFalse(evidence["agents_md_used"])
			self.assertIsNone(evidence["thinking"])
			self.assertIn("duration_s", evidence)

	def test_run_plan_builds_max_thinking_argv_and_omits_flag_when_unset(self):
		for thinking in ("max", None):
			with self.subTest(thinking=thinking), tempfile.TemporaryDirectory() as directory:
				workspace = Path(directory) / "repo"
				workspace.mkdir()
				init_repo(workspace)
				spec_path = Path(directory) / "spec.md"
				spec_path.write_text(VALID_SPEC)
				request_path = Path(directory) / "request.md"
				request_path.write_text("Add a feature")
				out_dir = Path(directory) / "tickets"
				evidence_path = Path(directory) / "evidence.json"
				commands = []
				with mock.patch.object(
					build_app, "sh",
					side_effect=scripted_pi(
						0, "", writes_tickets={"001.spec.md": VALID_TICKET},
						draft_dir=workspace / plan_tickets.DRAFT_RELATIVE_DIR, commands=commands,
					),
				):
					plan_tickets.run_plan(
						workspace, spec_path, request_path, "make verify", out_dir, evidence_path,
						timeout_minutes=1, thinking=thinking,
					)
				self.assertEqual(json.loads(evidence_path.read_text())["thinking"], thinking)
			self.assertEqual(len(commands), 1)
			if thinking is None:
				self.assertNotIn("--thinking", commands[0])
			else:
				self.assertEqual(commands[0][commands[0].index("--thinking") + 1], "max")

	def test_main_thinking_option_accepts_every_level_and_rejects_unknown(self):
		base_argv = [
			str(PLAN_TICKETS_SCRIPT), "--spec", "/tmp/spec.md", "--request", "/tmp/request.md",
			"--workspace", "/tmp/work", "--verify-command", "make verify",
			"--out-dir", "/tmp/tickets", "--evidence", "/tmp/evidence.json",
		]
		levels = build_app.THINKING_LEVELS
		for level in (*levels, None):
			with self.subTest(level=level):
				args = base_argv + ([] if level is None else ["--thinking", level])
				with mock.patch.object(sys, "argv", args), mock.patch.object(plan_tickets, "run_plan", return_value=0) as run_plan:
					self.assertEqual(plan_tickets.main(), 0)
				self.assertEqual(run_plan.call_args.kwargs["thinking"], level)

		with mock.patch.object(sys, "argv", base_argv + ["--thinking", "unknown"]):
			with contextlib.redirect_stderr(io.StringIO()):
				with self.assertRaises(SystemExit) as raised:
					plan_tickets.main()
		self.assertEqual(raised.exception.code, 2)

	def test_feedback_file_reaches_the_prompt(self):
		# run_plan's own --feedback-file plumbing, end to end.
		with tempfile.TemporaryDirectory() as directory:
			workspace = Path(directory) / "repo"
			workspace.mkdir()
			init_repo(workspace)
			spec_path = Path(directory) / "spec.md"
			spec_path.write_text(VALID_SPEC)
			request_path = Path(directory) / "request.md"
			request_path.write_text("Add a feature")
			out_dir = Path(directory) / "tickets"
			evidence_path = Path(directory) / "evidence.json"
			draft_dir = workspace / plan_tickets.DRAFT_RELATIVE_DIR
			feedback_path = Path(directory) / "plan-feedback.md"
			feedback_path.write_text("## Rejected by alice\n\nsplit ticket 1")

			tickets = {"001.spec.md": VALID_TICKET}
			with mock.patch.object(harness_adapters.PiAdapter, "invocation", autospec=True, wraps=harness_adapters.PiAdapter.invocation) as spy_pi_invocation:
				with mock.patch.object(
					build_app, "sh",
					side_effect=scripted_pi(0, "", writes_tickets=tickets, draft_dir=draft_dir),
				):
					exit_code = plan_tickets.run_plan(
						workspace, spec_path, request_path, "make verify", out_dir, evidence_path, timeout_minutes=1,
						feedback_path=feedback_path,
					)

			self.assertEqual(exit_code, 0)
			spy_pi_invocation.assert_called_once()
			self.assertIn("split ticket 1", spy_pi_invocation.call_args.kwargs["prompt"])
			self.assertIn("<<<BEGIN OPERATOR FEEDBACK>>>", spy_pi_invocation.call_args.kwargs["prompt"])

	def test_pi_failure_writes_evidence_but_no_tickets_and_exits_2(self):
		with tempfile.TemporaryDirectory() as directory:
			workspace = Path(directory) / "repo"
			workspace.mkdir()
			init_repo(workspace)
			spec_path = Path(directory) / "spec.md"
			spec_path.write_text(VALID_SPEC)
			request_path = Path(directory) / "request.md"
			request_path.write_text("Add a feature")
			out_dir = Path(directory) / "tickets"
			evidence_path = Path(directory) / "evidence.json"
			draft_dir = workspace / plan_tickets.DRAFT_RELATIVE_DIR

			import contextlib
			import io
			stderr = io.StringIO()
			with contextlib.redirect_stderr(stderr):
				with mock.patch.object(
					build_app, "sh",
					side_effect=scripted_pi(1, "", writes_tickets=None, draft_dir=draft_dir),
				):
					exit_code = plan_tickets.run_plan(workspace, spec_path, request_path, "make verify", out_dir, evidence_path, timeout_minutes=1)

			self.assertEqual(exit_code, 2)
			self.assertFalse(out_dir.exists())
			evidence = json.loads(evidence_path.read_text())
			self.assertEqual(evidence["agent_exit_code"], 1)
			# A live walk hit a 0-byte plan_tickets.log on this exact
			# failure path -- the script must print a reason so the log the
			# halt message points at is never empty.
			self.assertIn("agent exited 1", stderr.getvalue())

	def test_pi_timeout_writes_evidence_and_exits_2_instead_of_crashing(self):
		with tempfile.TemporaryDirectory() as directory:
			workspace = Path(directory) / "repo"
			workspace.mkdir()
			init_repo(workspace)
			spec_path = Path(directory) / "spec.md"
			spec_path.write_text(VALID_SPEC)
			request_path = Path(directory) / "request.md"
			request_path.write_text("Add a feature")
			out_dir = Path(directory) / "tickets"
			evidence_path = Path(directory) / "evidence.json"
			real_sh = build_app.sh

			def timeout(args, cwd=None, timeout=None, env=None):
				if args and args[0] == "git":
					return real_sh(args, cwd=cwd, timeout=timeout, env=env)
				raise subprocess.TimeoutExpired(args, timeout or 0, stderr=b"retrying\nupstream 429: rate limited\n")

			stderr = io.StringIO()
			with contextlib.redirect_stderr(stderr), mock.patch.object(build_app, "sh", side_effect=timeout):
				exit_code = plan_tickets.run_plan(
					workspace, spec_path, request_path, "make verify", out_dir, evidence_path, timeout_minutes=3,
				)

			self.assertEqual(exit_code, 2)
			self.assertFalse(out_dir.exists())
			self.assertIn("plan_tickets: agent timed out after 3 minutes: upstream 429: rate limited", stderr.getvalue())
			self.assertNotIn("Traceback", stderr.getvalue())
			evidence = json.loads(evidence_path.read_text())
			self.assertEqual(evidence["agent_exit_code"], -1)
			self.assertIsNone(evidence["thinking"])

	def test_pi_failure_includes_a_redacted_hint_from_pi_stderr(self):
		# Found in adversarial review of that fix: pi's own stderr
		# (build_app.sh captures it but nothing ever printed it) is the
		# only place an auth failure/401/route-down reason lives -- the
		# script must fold a bounded, redacted last line of it into its
		# one-line reason. Redaction (build_app.redact) must still apply,
		# and the printed line must stay a single line, not the raw
		# multi-line traceback.
		with tempfile.TemporaryDirectory() as directory:
			workspace = Path(directory) / "repo"
			workspace.mkdir()
			init_repo(workspace)
			spec_path = Path(directory) / "spec.md"
			spec_path.write_text(VALID_SPEC)
			request_path = Path(directory) / "request.md"
			request_path.write_text("Add a feature")
			out_dir = Path(directory) / "tickets"
			evidence_path = Path(directory) / "evidence.json"
			draft_dir = workspace / plan_tickets.DRAFT_RELATIVE_DIR
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
					side_effect=scripted_pi(1, "", writes_tickets=None, draft_dir=draft_dir, stderr=pi_stderr),
				):
					exit_code = plan_tickets.run_plan(workspace, spec_path, request_path, "make verify", out_dir, evidence_path, timeout_minutes=1)

			self.assertEqual(exit_code, 2)
			printed = stderr.getvalue()
			self.assertIn("plan_tickets: agent exited 1:", printed)
			self.assertIn("401 Unauthorized", printed)
			self.assertNotIn("sk-abcdefgh12345678", printed)
			self.assertNotIn("Traceback", printed)
			# The printed reason (excluding the surrounding test's own
			# newline) must be a single line.
			reason_line = next(line for line in printed.splitlines() if line.startswith("plan_tickets: agent exited 1"))
			self.assertNotIn("\n", reason_line)

	def test_no_tickets_written_exits_2_even_on_a_clean_pi_exit(self):
		with tempfile.TemporaryDirectory() as directory:
			workspace = Path(directory) / "repo"
			workspace.mkdir()
			init_repo(workspace)
			spec_path = Path(directory) / "spec.md"
			spec_path.write_text(VALID_SPEC)
			request_path = Path(directory) / "request.md"
			request_path.write_text("Add a feature")
			out_dir = Path(directory) / "tickets"
			evidence_path = Path(directory) / "evidence.json"
			draft_dir = workspace / plan_tickets.DRAFT_RELATIVE_DIR

			import contextlib
			import io
			stderr = io.StringIO()
			with contextlib.redirect_stderr(stderr):
				with mock.patch.object(
					build_app, "sh",
					side_effect=scripted_pi(0, "", writes_tickets=None, draft_dir=draft_dir),
				):
					exit_code = plan_tickets.run_plan(workspace, spec_path, request_path, "make verify", out_dir, evidence_path, timeout_minutes=1)

			self.assertEqual(exit_code, 2)
			self.assertIn("drafted no *.spec.md ticket files", stderr.getvalue())

	def test_no_tickets_written_exits_2_even_on_a_clean_pi_exit_names_the_model_route_error(self):
		STDOUT = '{"type":"message_end","message":{"stopReason":"error","errorMessage":"400 model gpt-5.6-luna is not supported via /chat/completions (token ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZ123456)"}}'
		with tempfile.TemporaryDirectory() as directory:
			workspace = Path(directory) / "repo"
			workspace.mkdir()
			init_repo(workspace)
			spec_path = Path(directory) / "spec.md"
			spec_path.write_text(VALID_SPEC)
			request_path = Path(directory) / "request.md"
			request_path.write_text("Add a feature")
			out_dir = Path(directory) / "tickets"
			evidence_path = Path(directory) / "evidence.json"
			draft_dir = workspace / plan_tickets.DRAFT_RELATIVE_DIR

			import contextlib
			import io
			stderr = io.StringIO()
			with contextlib.redirect_stderr(stderr):
				with mock.patch.object(
					build_app, "sh",
					side_effect=scripted_pi(0, STDOUT, writes_tickets=None, draft_dir=draft_dir),
				):
					exit_code = plan_tickets.run_plan(workspace, spec_path, request_path, "make verify", out_dir, evidence_path, timeout_minutes=1)

			self.assertEqual(exit_code, 2)
			self.assertIn("drafted no *.spec.md ticket files", stderr.getvalue())
			self.assertIn("drafted no *.spec.md ticket files; model route error: 400 model gpt-5.6-luna is not supported via /chat/completions", stderr.getvalue())
			self.assertNotIn("ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZ123456", stderr.getvalue())
			self.assertEqual(len(stderr.getvalue().strip().splitlines()), 1)

	def test_all_empty_tickets_exits_2_and_prints_a_reason(self):
		with tempfile.TemporaryDirectory() as directory:
			workspace = Path(directory) / "repo"
			workspace.mkdir()
			init_repo(workspace)
			spec_path = Path(directory) / "spec.md"
			spec_path.write_text(VALID_SPEC)
			request_path = Path(directory) / "request.md"
			request_path.write_text("Add a feature")
			out_dir = Path(directory) / "tickets"
			evidence_path = Path(directory) / "evidence.json"
			draft_dir = workspace / plan_tickets.DRAFT_RELATIVE_DIR

			import contextlib
			import io
			stderr = io.StringIO()
			with contextlib.redirect_stderr(stderr):
				with mock.patch.object(
					build_app, "sh",
					side_effect=scripted_pi(0, "", writes_tickets={"001.spec.md": "   \n"}, draft_dir=draft_dir),
				):
					exit_code = plan_tickets.run_plan(workspace, spec_path, request_path, "make verify", out_dir, evidence_path, timeout_minutes=1)

			self.assertEqual(exit_code, 2)
			self.assertIn("every one was empty", stderr.getvalue())

	def test_blank_ticket_file_is_skipped(self):
		with tempfile.TemporaryDirectory() as directory:
			workspace = Path(directory) / "repo"
			workspace.mkdir()
			init_repo(workspace)
			spec_path = Path(directory) / "spec.md"
			spec_path.write_text(VALID_SPEC)
			request_path = Path(directory) / "request.md"
			request_path.write_text("Add a feature")
			out_dir = Path(directory) / "tickets"
			evidence_path = Path(directory) / "evidence.json"
			draft_dir = workspace / plan_tickets.DRAFT_RELATIVE_DIR

			tickets = {"001.spec.md": VALID_TICKET, "002.spec.md": "   \n"}
			with mock.patch.object(
				build_app, "sh",
				side_effect=scripted_pi(0, "", writes_tickets=tickets, draft_dir=draft_dir),
			):
				exit_code = plan_tickets.run_plan(workspace, spec_path, request_path, "make verify", out_dir, evidence_path, timeout_minutes=1)

			self.assertEqual(exit_code, 0)
			self.assertTrue((out_dir / "001.spec.md").exists())
			self.assertFalse((out_dir / "002.spec.md").exists())


if __name__ == "__main__":
	unittest.main()
