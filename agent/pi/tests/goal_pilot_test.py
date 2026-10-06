import importlib.util
import json
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path
from unittest import mock


SCRIPT = Path(__file__).resolve().parents[1] / "scripts" / "goal_pilot.py"
SPEC = importlib.util.spec_from_file_location("goal_pilot", SCRIPT)
assert SPEC and SPEC.loader
goal_pilot = importlib.util.module_from_spec(SPEC)
sys.modules[SPEC.name] = goal_pilot
SPEC.loader.exec_module(goal_pilot)

ticket_runner = goal_pilot.ticket_runner


def make_ticket(number=1, slug="workspace-scaffold"):
	return ticket_runner.Ticket(number, slug, Path(f"{number:03d}-{slug}.md"))


def write_gate(pilot_dir: Path, ticket, checks: list[dict], passed: bool = False):
	report_dir = pilot_dir / "reports" / f"ticket-{ticket.nnn}"
	report_dir.mkdir(parents=True, exist_ok=True)
	(report_dir / "gate.json").write_text(json.dumps({
		"ticket": ticket.nnn, "review_policy": "advisory", "passed": passed, "checks": checks,
	}))


class SpecStatusTests(unittest.TestCase):
	def test_missing_spec_has_no_status(self):
		with tempfile.TemporaryDirectory() as d:
			self.assertIsNone(goal_pilot.read_spec_status(Path(d)))

	def test_reads_draft_status(self):
		with tempfile.TemporaryDirectory() as d:
			pilot_dir = Path(d)
			spec = pilot_dir / "spec"
			spec.mkdir()
			(spec / "spec.md").write_text("STATUS: DRAFT -- pending human review\n\nBody\n")
			self.assertEqual(goal_pilot.read_spec_status(pilot_dir), "DRAFT")

	def test_freeze_spec_rewrites_only_first_line(self):
		with tempfile.TemporaryDirectory() as d:
			pilot_dir = Path(d)
			spec = pilot_dir / "spec"
			spec.mkdir()
			(spec / "spec.md").write_text("STATUS: DRAFT -- pending human review\n\nBody unchanged\n")
			goal_pilot.freeze_spec(pilot_dir)
			text = (spec / "spec.md").read_text()
			self.assertTrue(text.startswith("STATUS: FROZEN -- reviewed "))
			self.assertIn("Body unchanged", text)
			self.assertEqual(goal_pilot.read_spec_status(pilot_dir), "FROZEN")


class CompileCompleteMarkerTests(unittest.TestCase):
	def test_not_complete_when_only_contract_md_exists(self):
		# Codex review of PR #36: a crash mid-/contract-plan (contract.md
		# written, self-check never finished) must not read as step-4-done.
		with tempfile.TemporaryDirectory() as d:
			pilot_dir = Path(d)
			(pilot_dir / "spec").mkdir()
			(pilot_dir / "spec" / "contract.md").write_text("# contract\n")
			self.assertFalse(goal_pilot.is_compile_complete(pilot_dir))

	def test_complete_after_marker_written(self):
		with tempfile.TemporaryDirectory() as d:
			pilot_dir = Path(d)
			(pilot_dir / "spec").mkdir()
			goal_pilot.write_compile_complete_marker(pilot_dir, staged_ticket_count=3)
			self.assertTrue(goal_pilot.is_compile_complete(pilot_dir))
			data = json.loads(goal_pilot.compile_complete_marker(pilot_dir).read_text())
			self.assertEqual(data["staged_ticket_count"], 3)


class ResolveSpecInputTests(unittest.TestCase):
	def test_existing_file_used_as_is(self):
		with tempfile.TemporaryDirectory() as d:
			pilot_dir = Path(d) / "pilot"
			pilot_dir.mkdir()
			existing = Path(d) / "notes.md"
			existing.write_text("rough idea")
			resolved = goal_pilot.resolve_spec_input(str(existing), pilot_dir)
			self.assertEqual(resolved, existing.resolve())

	def test_literal_text_written_to_scratch_file(self):
		with tempfile.TemporaryDirectory() as d:
			pilot_dir = Path(d) / "pilot"
			resolved = goal_pilot.resolve_spec_input("a rough idea, not a path", pilot_dir)
			self.assertTrue(resolved.exists())
			self.assertEqual(resolved.read_text(), "a rough idea, not a path")

	def test_scratch_file_is_a_sibling_not_written_inside_pilot_dir(self):
		# Codex review of PR #37: /spec-plan's own step-0 scaffold check
		# refuses to run against any existing, non-empty directory with no
		# Makefile yet. Writing the scratch file *inside* an unscaffolded
		# pilot_dir would make every fresh run using literal --spec-input
		# text immediately unscaffoldable. It must land outside pilot_dir.
		with tempfile.TemporaryDirectory() as d:
			pilot_dir = Path(d) / "pilot"
			resolved = goal_pilot.resolve_spec_input("a rough idea, not a path", pilot_dir)
			self.assertFalse(resolved.is_relative_to(pilot_dir))
			self.assertFalse(pilot_dir.exists() and any(pilot_dir.iterdir()))

	def test_literal_text_is_not_rewritten_on_a_second_call(self):
		# Resume must keep reusing the same frozen input, not overwrite it
		# with whatever --spec-input happens to be passed on the resume
		# invocation.
		with tempfile.TemporaryDirectory() as d:
			pilot_dir = Path(d) / "pilot"
			goal_pilot.resolve_spec_input("first idea", pilot_dir)
			resolved = goal_pilot.resolve_spec_input("second, different idea", pilot_dir)
			self.assertEqual(resolved.read_text(), "first idea")


class SessionRootTests(unittest.TestCase):
	def test_session_root_is_a_sibling_not_written_inside_pilot_dir(self):
		# Reproduced live: `pi --session-dir` creates its directory as soon as
		# the session starts, before the model's first turn -- i.e. before
		# /spec-plan's own step-0 "is $2 empty" scaffold check ever runs. A
		# session dir placed *inside* a fresh, not-yet-scaffolded pilot_dir
		# makes that check see "exists, non-empty, no Makefile" and correctly
		# refuse to scaffold -- on every single fresh pilot_dir, not just an
		# edge case. Same hazard class as resolve_spec_input()'s scratch file
		# (Codex review of PR #37); the session root must land outside
		# pilot_dir the same way.
		with tempfile.TemporaryDirectory() as d:
			pilot_dir = Path(d) / "pilot"
			root = goal_pilot.goal_pilot_session_root(pilot_dir)
			self.assertFalse(root.is_relative_to(pilot_dir))
			self.assertFalse(pilot_dir.exists() and any(pilot_dir.iterdir()))

	def test_session_root_is_stable_across_calls(self):
		with tempfile.TemporaryDirectory() as d:
			pilot_dir = Path(d) / "pilot"
			first = goal_pilot.goal_pilot_session_root(pilot_dir)
			second = goal_pilot.goal_pilot_session_root(pilot_dir)
			self.assertEqual(first, second)


class ClassifyHaltTests(unittest.TestCase):
	def test_unknown_when_no_gate_recorded(self):
		with tempfile.TemporaryDirectory() as d:
			self.assertEqual(goal_pilot.classify_halt(Path(d), make_ticket()), "unknown")

	def test_oracle_integrity_failure_is_canon_drift(self):
		with tempfile.TemporaryDirectory() as d:
			pilot_dir = Path(d)
			ticket = make_ticket(2)
			write_gate(pilot_dir, ticket, [
				{"name": "make verify", "ok": True},
				{"name": "oracle integrity", "ok": False, "detail": ["acceptance/002_handler_test.go"]},
			])
			self.assertEqual(goal_pilot.classify_halt(pilot_dir, ticket), "canon-drift")

	def test_verify_surface_frozen_failure_is_canon_drift(self):
		with tempfile.TemporaryDirectory() as d:
			pilot_dir = Path(d)
			ticket = make_ticket(3)
			write_gate(pilot_dir, ticket, [
				{"name": "verify-surface frozen", "ok": False, "detail": "verify surface changed"},
			])
			self.assertEqual(goal_pilot.classify_halt(pilot_dir, ticket), "canon-drift")

	def test_model_route_unreachable_is_infra(self):
		with tempfile.TemporaryDirectory() as d:
			pilot_dir = Path(d)
			ticket = make_ticket(5)
			write_gate(pilot_dir, ticket, [
				{"name": "BUILD_REPORT.md SUCCEEDED", "ok": False, "detail": "model route unreachable (12/12 assistant turns errored)"},
			])
			self.assertEqual(goal_pilot.classify_halt(pilot_dir, ticket), "infra")

	def test_plain_verification_failure_is_implementation_gap(self):
		with tempfile.TemporaryDirectory() as d:
			pilot_dir = Path(d)
			ticket = make_ticket(4)
			write_gate(pilot_dir, ticket, [
				{"name": "make verify", "ok": False, "detail": "FAIL: TestOnboarding"},
				{"name": "BUILD_REPORT.md SUCCEEDED", "ok": False, "detail": "Outcome: DID NOT SUCCEED"},
			])
			self.assertEqual(goal_pilot.classify_halt(pilot_dir, ticket), "implementation-gap")

	def test_canon_drift_takes_priority_over_infra_markers(self):
		# A ticket can fail both an infra-flavored check and oracle
		# integrity in the same gate; canon-drift must win, since it's the
		# class that must never be auto-applied.
		with tempfile.TemporaryDirectory() as d:
			pilot_dir = Path(d)
			ticket = make_ticket(6)
			write_gate(pilot_dir, ticket, [
				{"name": "oracle integrity", "ok": False, "detail": ["acceptance/006_x_test.go"]},
				{"name": "BUILD_REPORT.md SUCCEEDED", "ok": False, "detail": "stall-timeout"},
			])
			self.assertEqual(goal_pilot.classify_halt(pilot_dir, ticket), "canon-drift")


class InfraHaltRetryTests(unittest.TestCase):
	def test_report_mode_never_retries(self):
		with tempfile.TemporaryDirectory() as d:
			self.assertFalse(goal_pilot.handle_infra_halt(Path(d), "report", attempt_count=0))

	def test_auto_rescue_retries_until_bound(self):
		with tempfile.TemporaryDirectory() as d, mock.patch.object(goal_pilot, "AI_STACK_HOST_FALLBACK", "fallback-host"):
			pilot_dir = Path(d)
			self.assertTrue(goal_pilot.handle_infra_halt(pilot_dir, "auto-rescue", attempt_count=0))
			self.assertTrue(goal_pilot.handle_infra_halt(pilot_dir, "auto-rescue", attempt_count=1))
			self.assertFalse(goal_pilot.handle_infra_halt(pilot_dir, "auto-rescue", attempt_count=goal_pilot.MAX_INFRA_AUTO_RETRIES))

	def test_auto_rescue_sets_ai_stack_host_fallback(self):
		with tempfile.TemporaryDirectory() as d:
			with mock.patch.dict("os.environ", {"AI_STACK_HOST": "example-lan-host.local"}, clear=False), mock.patch.object(goal_pilot, "AI_STACK_HOST_FALLBACK", "fallback-host"):
				goal_pilot.handle_infra_halt(Path(d), "auto-rescue", attempt_count=0)
				import os
				self.assertEqual(os.environ["AI_STACK_HOST"], "fallback-host")

	def test_auto_rescue_without_a_fallback_reports_instead(self):
		"""No deployment-specific hostname is baked in: with nothing in
		AI_STACK_HOST_FALLBACK an infra halt is reported, not retried."""
		with tempfile.TemporaryDirectory() as d, mock.patch.object(goal_pilot, "AI_STACK_HOST_FALLBACK", ""):
			self.assertFalse(goal_pilot.handle_infra_halt(Path(d), "auto-rescue", attempt_count=0))


class ImplementationGapRescueBoundTests(unittest.TestCase):
	def test_report_mode_never_widens(self):
		with tempfile.TemporaryDirectory() as d:
			pilot_dir = Path(d)
			ticket = make_ticket(4)
			result = goal_pilot.handle_implementation_gap_halt(pilot_dir, pilot_dir / "workspace", [ticket], ticket, "advisory", "report")
			self.assertFalse(result)

	def test_second_call_for_same_ticket_does_not_widen_again(self):
		with tempfile.TemporaryDirectory() as d:
			pilot_dir = Path(d)
			ticket = make_ticket(4)
			goal_pilot.append_rescue_record(pilot_dir, {"ticket": ticket.nnn, "class": "implementation-gap", "action": "widened local retry"})
			result = goal_pilot.handle_implementation_gap_halt(pilot_dir, pilot_dir / "workspace", [ticket], ticket, "advisory", "auto-rescue")
			self.assertFalse(result)


def pi_output_with_turn_errors(errored: int, total: int) -> str:
	# Shape matches a real, live `pi --print --mode json` capture -- see
	# harness_adapters.agent_turn_errors' own doc comment (found live, 2026-09-07):
	# message_end is a flat top-level event, never wrapped in
	# {"type": "entry_appended", "entry": ...} the way this fixture (and
	# the function it exercises) wrongly assumed until that fix.
	events = []
	for i in range(total):
		events.append({
			"type": "message_end",
			"message": {"role": "assistant", "stopReason": "error" if i < errored else "end_turn"},
		})
	return "\n".join(json.dumps(e) for e in events)


class RunPiPromptFullyErroredTests(unittest.TestCase):
	"""Regression coverage for Codex review of PR #37: `pi` exits 0 even
	when the model route was unreachable for an entire invocation (every
	assistant turn's stopReason is "error"), and a resumed /contract-plan
	run can leave a stale, already-partial contract.md on disk from a
	previous attempt -- without this check, step4_compile() would accept
	that stale artifact as if a real self-check had just run."""

	def test_ok_false_when_every_turn_errored(self):
		with tempfile.TemporaryDirectory() as d:
			pilot_dir = Path(d)
			with mock.patch.object(
				goal_pilot.ticket_runner, "invoke_build_app",
				return_value=(0, pi_output_with_turn_errors(3, 3), "", False),
			):
				ok, _stdout, diagnostics = goal_pilot.run_pi_prompt(pilot_dir, "/contract-plan .", session_dir=pilot_dir / "s", timeout_s=60)
			self.assertFalse(ok)
			self.assertTrue(diagnostics["fully_errored"])

	def test_ok_true_when_some_turns_succeeded(self):
		with tempfile.TemporaryDirectory() as d:
			pilot_dir = Path(d)
			with mock.patch.object(
				goal_pilot.ticket_runner, "invoke_build_app",
				return_value=(0, pi_output_with_turn_errors(1, 3), "", False),
			):
				ok, _stdout, diagnostics = goal_pilot.run_pi_prompt(pilot_dir, "/contract-plan .", session_dir=pilot_dir / "s", timeout_s=60)
			self.assertTrue(ok)
			self.assertFalse(diagnostics["fully_errored"])

	def test_ok_true_when_no_assistant_turns_recorded_at_all(self):
		# total == 0 must not be treated as "0/0 errored, i.e. fully
		# errored" -- that would misclassify an invocation whose output
		# simply doesn't parse as pi JSONL (e.g. a real, clean success).
		with tempfile.TemporaryDirectory() as d:
			pilot_dir = Path(d)
			with mock.patch.object(
				goal_pilot.ticket_runner, "invoke_build_app",
				return_value=(0, "not pi jsonl output at all", "", False),
			):
				ok, _stdout, diagnostics = goal_pilot.run_pi_prompt(pilot_dir, "/contract-plan .", session_dir=pilot_dir / "s", timeout_s=60)
			self.assertTrue(ok)
			self.assertFalse(diagnostics["fully_errored"])


class CanonDriftRecoveryTests(unittest.TestCase):
	"""Real git, no mocks: the bug guarded here is that
	ticket_runner.run_ticket() already restores the canonical copy over a
	drifted file before returning failure, so reading the workspace file
	directly at halt time always finds it already matching canon."""

	def _init_workspace_with_ticket_commit(self, workspace: Path, rel_path: str, committed_content: str) -> None:
		run = lambda *args: subprocess.run(["git", *args], cwd=workspace, check=True, capture_output=True)
		workspace.mkdir(parents=True, exist_ok=True)
		run("init")
		run("config", "user.email", "test@test")
		run("config", "user.name", "test")
		run("config", "commit.gpgsign", "false")
		run("config", "core.hooksPath", "/dev/null")
		target = workspace / rel_path
		target.parent.mkdir(parents=True, exist_ok=True)
		target.write_text(committed_content)
		run("add", "-A")
		run("commit", "-m", "ticket(002): drifted fix")

	def test_recovers_committed_content_even_after_workspace_was_restored_to_canon(self):
		with tempfile.TemporaryDirectory() as d:
			pilot_dir = Path(d)
			workspace = pilot_dir / "workspace"
			rel = "acceptance/002_handler_test.go"
			self._init_workspace_with_ticket_commit(workspace, rel, "package acceptance // model's fix\n")
			# Simulate run_ticket()'s post-halt restore: the on-disk file no
			# longer matches what the model actually committed.
			(workspace / rel).write_text("package acceptance // canonical original\n")
			ticket = make_ticket(2, "handler")

			recovered = goal_pilot.recover_drifted_content(workspace, ticket, workspace / rel)

			self.assertEqual(recovered, "package acceptance // model's fix\n")

	def test_returns_none_when_ticket_has_no_commit_yet(self):
		with tempfile.TemporaryDirectory() as d:
			pilot_dir = Path(d)
			workspace = pilot_dir / "workspace"
			self._init_workspace_with_ticket_commit(workspace, "acceptance/x_test.go", "content\n")
			ticket = make_ticket(9, "not-committed")

			recovered = goal_pilot.recover_drifted_content(workspace, ticket, workspace / "acceptance/x_test.go")

			self.assertIsNone(recovered)


class RescueLogTests(unittest.TestCase):
	def test_records_round_trip_as_jsonl(self):
		with tempfile.TemporaryDirectory() as d:
			pilot_dir = Path(d)
			goal_pilot.append_rescue_record(pilot_dir, {"ticket": "003", "class": "infra", "action": "retry"})
			goal_pilot.append_rescue_record(pilot_dir, {"ticket": "004", "class": "implementation-gap", "action": "widened"})
			records = goal_pilot.read_rescue_records(pilot_dir)
			self.assertEqual([r["ticket"] for r in records], ["003", "004"])

	def test_missing_log_reads_as_empty(self):
		with tempfile.TemporaryDirectory() as d:
			self.assertEqual(goal_pilot.read_rescue_records(Path(d)), [])


class VerdictTests(unittest.TestCase):
	def test_verdict_reports_no_rescues_cleanly(self):
		with tempfile.TemporaryDirectory() as d:
			pilot_dir = Path(d)
			path = goal_pilot.write_verdict(pilot_dir, [make_ticket()], "advisory", "skip", "auto-rescue")
			text = path.read_text()
			self.assertIn("None -- every ticket gated green", text)
			self.assertIn("--checkpoint=skip --on-halt=auto-rescue", text)

	def test_verdict_states_rescue_counts_by_class(self):
		with tempfile.TemporaryDirectory() as d:
			pilot_dir = Path(d)
			goal_pilot.append_rescue_record(pilot_dir, {"ticket": "003", "class": "infra", "action": "retry"})
			goal_pilot.append_rescue_record(pilot_dir, {"ticket": "004", "class": "implementation-gap", "action": "widened"})
			path = goal_pilot.write_verdict(pilot_dir, [make_ticket()], "advisory", "skip", "auto-rescue")
			text = path.read_text()
			self.assertIn("infra: 1", text)
			self.assertIn("implementation-gap: 1", text)

	def test_verdict_does_not_unconditionally_claim_zero_cloud_usage(self):
		# Regression for a real GitHub Codex App review finding on PR #73:
		# when PI_HARNESS_PROVIDER/PI_HARNESS_MODEL are unset, every pi
		# invocation this run made inherited pi's own configured default
		# route -- which could be a cloud one -- so the verdict must not
		# assert "No cloud model was invoked" as a verified fact.
		with tempfile.TemporaryDirectory() as d:
			pilot_dir = Path(d)
			path = goal_pilot.write_verdict(pilot_dir, [make_ticket()], "advisory", "skip", "auto-rescue")
			text = path.read_text()
			self.assertNotIn("No cloud model was invoked", text)
			self.assertIn("cannot see or verify that from here", text)


class LockContentionTests(unittest.TestCase):
	def test_detects_lock_message(self):
		self.assertTrue(goal_pilot.lock_contention("another ticket_runner.py is already running against ...; refusing to race it"))

	def test_ignores_unrelated_output(self):
		self.assertFalse(goal_pilot.lock_contention("GATE PASSED for ticket 001"))


class Checkpoint5IdempotencyTests(unittest.TestCase):
	def test_skip_mode_writes_ack_marker_and_logs_disclaimer(self):
		with tempfile.TemporaryDirectory() as d:
			pilot_dir = Path(d)
			(pilot_dir / "spec").mkdir()
			self.assertTrue(goal_pilot.step5_checkpoint(pilot_dir, "skip"))
			self.assertTrue(goal_pilot.checkpoint5_ack_marker(pilot_dir).exists())
			log = goal_pilot.execution_log_path(pilot_dir).read_text()
			self.assertIn("never independently (cloud/human) reviewed", log)

	def test_already_acked_run_does_not_reprompt(self):
		with tempfile.TemporaryDirectory() as d:
			pilot_dir = Path(d)
			(pilot_dir / "spec").mkdir()
			goal_pilot.checkpoint5_ack_marker(pilot_dir).write_text("{}")
			# non_interactive=True would normally refuse to proceed -- if this
			# returns True anyway, the already-acked short-circuit fired
			# before any prompt/refusal logic ran.
			self.assertTrue(goal_pilot.step5_checkpoint(pilot_dir, "review", non_interactive=True))


class RunTicketRunnerInvocationTests(unittest.TestCase):
	def test_stop_after_ticket_is_threaded_into_the_command(self):
		with tempfile.TemporaryDirectory() as d:
			pilot_dir = Path(d)
			with mock.patch.object(
				goal_pilot.ticket_runner, "invoke_build_app",
				return_value=(0, "GATE PASSED for ticket 001", "", False),
			) as invoked:
				goal_pilot.run_ticket_runner(pilot_dir, "advisory", stop_after_ticket=1)
			cmd = invoked.call_args[0][0]
			self.assertIn("--stop-after-ticket", cmd)
			self.assertEqual(cmd[cmd.index("--stop-after-ticket") + 1], "1")

	def test_no_stop_flag_when_not_requested(self):
		with tempfile.TemporaryDirectory() as d:
			pilot_dir = Path(d)
			with mock.patch.object(
				goal_pilot.ticket_runner, "invoke_build_app",
				return_value=(0, "all tickets complete.", "", False),
			) as invoked:
				goal_pilot.run_ticket_runner(pilot_dir, "advisory", stop_after_ticket=None)
			cmd = invoked.call_args[0][0]
			self.assertNotIn("--stop-after-ticket", cmd)

	def test_verify_command_is_threaded_into_the_command(self):
		# Real bug found via adversarial review (2026-09-11): goal_pilot.py's
		# own --verify-command override only ever changed the TEXT of a
		# drafted ticket's Verify-Command: key -- ticket_runner.py's actual
		# gate (run_ticket) hardcoded `make verify` + `make verify-full`
		# unconditionally and never read either the ticket's declared value
		# or this override, so the override had zero effect on any run that
		# actually reaches this subprocess.
		with tempfile.TemporaryDirectory() as d:
			pilot_dir = Path(d)
			with mock.patch.object(
				goal_pilot.ticket_runner, "invoke_build_app",
				return_value=(0, "all tickets complete.", "", False),
			) as invoked:
				goal_pilot.run_ticket_runner(pilot_dir, "advisory", stop_after_ticket=None, verify_command="make verify")
			cmd = invoked.call_args[0][0]
			self.assertIn("--verify-command", cmd)
			self.assertEqual(cmd[cmd.index("--verify-command") + 1], "make verify")

	def test_no_verify_command_flag_when_not_given(self):
		with tempfile.TemporaryDirectory() as d:
			pilot_dir = Path(d)
			with mock.patch.object(
				goal_pilot.ticket_runner, "invoke_build_app",
				return_value=(0, "all tickets complete.", "", False),
			) as invoked:
				goal_pilot.run_ticket_runner(pilot_dir, "advisory", stop_after_ticket=None)
			cmd = invoked.call_args[0][0]
			self.assertNotIn("--verify-command", cmd)


class BuildLoopUntilSettledTests(unittest.TestCase):
	"""run_build_loop_until_settled() is shared by both the ticket-1-only
	phase (before the 5b checkpoint) and the remainder-of-the-run phase --
	regression coverage for a real bug caught during implementation review,
	where an earlier version gave up immediately on any ticket-1 halt
	instead of running it through the same class-aware handling every
	other ticket gets."""

	def test_returns_true_immediately_on_a_clean_run(self):
		with tempfile.TemporaryDirectory() as d:
			pilot_dir = Path(d)
			with mock.patch.object(goal_pilot, "run_ticket_runner", return_value=(0, "all tickets complete.")):
				ok = goal_pilot.run_build_loop_until_settled(
					pilot_dir, pilot_dir / "workspace", [make_ticket()], "advisory", "auto-rescue",
					stop_after_ticket=None, non_interactive=False, infra_attempts={},
				)
			self.assertTrue(ok)

	def test_retries_when_handle_halt_says_to_and_eventually_settles(self):
		with tempfile.TemporaryDirectory() as d:
			pilot_dir = Path(d)
			outcomes = iter([(1, "GATE FAILED"), (0, "all tickets complete.")])
			with mock.patch.object(goal_pilot, "run_ticket_runner", side_effect=lambda *a, **k: next(outcomes)), \
				mock.patch.object(goal_pilot, "handle_halt", return_value=True), \
				mock.patch.object(goal_pilot.time, "sleep"):
				ok = goal_pilot.run_build_loop_until_settled(
					pilot_dir, pilot_dir / "workspace", [make_ticket()], "advisory", "auto-rescue",
					stop_after_ticket=1, non_interactive=False, infra_attempts={},
				)
			self.assertTrue(ok)

	def test_stops_and_returns_false_when_handle_halt_says_not_to_retry(self):
		with tempfile.TemporaryDirectory() as d:
			pilot_dir = Path(d)
			with mock.patch.object(goal_pilot, "run_ticket_runner", return_value=(1, "GATE FAILED")), \
				mock.patch.object(goal_pilot, "handle_halt", return_value=False):
				ok = goal_pilot.run_build_loop_until_settled(
					pilot_dir, pilot_dir / "workspace", [make_ticket()], "advisory", "report",
					stop_after_ticket=1, non_interactive=False, infra_attempts={},
				)
			self.assertFalse(ok)

	def test_lock_contention_stops_without_calling_handle_halt(self):
		with tempfile.TemporaryDirectory() as d:
			pilot_dir = Path(d)
			with mock.patch.object(goal_pilot, "run_ticket_runner", return_value=(1, "refusing to race it")), \
				mock.patch.object(goal_pilot, "handle_halt") as halt_handler:
				ok = goal_pilot.run_build_loop_until_settled(
					pilot_dir, pilot_dir / "workspace", [make_ticket()], "advisory", "auto-rescue",
					stop_after_ticket=None, non_interactive=False, infra_attempts={},
				)
			self.assertFalse(ok)
			halt_handler.assert_not_called()

	def test_ticket_one_phase_passes_stop_after_ticket_through(self):
		with tempfile.TemporaryDirectory() as d:
			pilot_dir = Path(d)
			with mock.patch.object(goal_pilot, "run_ticket_runner", return_value=(0, "gate passed")) as runner:
				goal_pilot.run_build_loop_until_settled(
					pilot_dir, pilot_dir / "workspace", [make_ticket()], "advisory", "auto-rescue",
					stop_after_ticket=1, non_interactive=False, infra_attempts={},
				)
			runner.assert_called_once_with(pilot_dir, "advisory", stop_after_ticket=1, verify_command=None)


class MainScaffoldOrderingTests(unittest.TestCase):
	"""Regression coverage for Codex review of PR #37: on a fresh,
	not-yet-scaffolded pilot_dir, goal_pilot.py must not write anything
	into it before /spec-plan's own step-0 scaffold-safety check runs --
	that check refuses any existing, non-empty directory with no Makefile
	yet, so an EXECUTION_LOG.md written first would make every fresh run
	immediately unscaffoldable."""

	def test_nothing_written_into_a_fresh_pilot_dir_before_spec_plan_runs(self):
		with tempfile.TemporaryDirectory() as d:
			pilot_dir = Path(d) / "pilot"  # deliberately does not exist yet

			def fake_step2(pilot_dir_arg, spec_input, verify_command=None):
				# Simulate what /spec-plan's own step 0 does: scaffold the
				# dir for real, including writing a Makefile -- and assert
				# nothing was written into it before this ran.
				self.assertFalse(pilot_dir_arg.exists() and any(pilot_dir_arg.iterdir()))
				pilot_dir_arg.mkdir(parents=True, exist_ok=True)
				(pilot_dir_arg / "Makefile").write_text("run:\n\t@true\n")
				(pilot_dir_arg / "spec").mkdir()
				(pilot_dir_arg / "spec" / "spec.md").write_text("STATUS: DRAFT -- pending review\n")
				return True

			argv = [
				"goal_pilot.py", "--spec-input", "a rough idea",
				"--pilot-dir", str(pilot_dir), "--non-interactive",
			]
			with mock.patch.object(sys, "argv", argv), \
				mock.patch.object(goal_pilot, "step2_draft_spec", side_effect=fake_step2), \
				mock.patch.object(goal_pilot, "step3_freeze_checkpoint", return_value=False):
				goal_pilot.main()

			# step3 was mocked to refuse (return False), so the run halts
			# right after step2 -- but step2 itself must have seen an empty
			# dir, and EXECUTION_LOG.md must exist by now (written once
			# scaffolding was confirmed).
			self.assertTrue((pilot_dir / "EXECUTION_LOG.md").exists())


class WriteArchitectureStubTests(unittest.TestCase):
	"""Coverage for a second, distinct bridging gap found via a real
	end-to-end validation run (2026-08-30): factoryd's own mandatory
	project-bootstrap preflight requires ARCHITECTURE.md to already exist
	and pass policy.ArchitectureStructure before ticket 001 ever runs, but
	`factoryd init` (the tool that normally provides this stub) refuses
	once spec/spec.md already exists -- which /spec-plan's own step 1
	always does, immediately, before this pipeline has any other chance
	to invoke it."""

	def test_writes_stub_with_required_headings_in_order(self):
		with tempfile.TemporaryDirectory() as d:
			pilot_dir = Path(d) / "my-project"
			pilot_dir.mkdir()
			wrote = goal_pilot.write_architecture_stub(pilot_dir)
			self.assertTrue(wrote)
			content = (pilot_dir / "ARCHITECTURE.md").read_text()
			# Order matters to buildgate's own
			# policy.ArchitectureStructure -- pin it, not just presence.
			self.assertLess(content.index("## Repo layout"), content.index("## Verification"))
			self.assertLess(content.index("## Verification"), content.index("## Known deviations"))

	def test_never_overwrites_an_existing_architecture_md(self):
		with tempfile.TemporaryDirectory() as d:
			pilot_dir = Path(d)
			real_content = "# Real Architecture\n\nWritten by ticket 001's own agent.\n"
			(pilot_dir / "ARCHITECTURE.md").write_text(real_content)
			wrote = goal_pilot.write_architecture_stub(pilot_dir)
			self.assertFalse(wrote)
			self.assertEqual((pilot_dir / "ARCHITECTURE.md").read_text(), real_content)

	def test_does_not_follow_a_dangling_architecture_md_symlink(self):
		"""Regression for a real P1 review finding: `Path.exists()` follows
		symlinks and reports False for a *dangling* one, so the old
		`path.exists()` check fell through to `write_text()`, which also
		follows the link -- writing the stub to whatever the link points
		at, outside `pilot_dir`, despite this function's own contract
		never to touch an existing path."""
		with tempfile.TemporaryDirectory() as d:
			outside_dir = Path(d) / "outside"
			outside_dir.mkdir()
			pilot_dir = Path(d) / "pilot"
			pilot_dir.mkdir()
			target = outside_dir / "escaped.md"
			(pilot_dir / "ARCHITECTURE.md").symlink_to(target)  # dangling: target doesn't exist

			wrote = goal_pilot.write_architecture_stub(pilot_dir)

			self.assertFalse(wrote)
			self.assertFalse(target.exists(), "stub must not be written through a dangling symlink")

	def test_propagates_a_real_write_failure_instead_of_reporting_success(self):
		"""Regression for a real P2 review finding: catching every OSError
		from the exclusive-create open() -- not just EEXIST/ELOOP -- meant a
		real failure like a missing parent directory was also swallowed as
		"already there," so a caller like --inject-only would report
		success without the artifact ever having been written, deferring
		the failure to a much later, harder-to-diagnose `factoryd`
		preflight rejection instead."""
		with tempfile.TemporaryDirectory() as d:
			missing_pilot_dir = Path(d) / "does-not-exist"  # never created
			with self.assertRaises(OSError):
				goal_pilot.write_architecture_stub(missing_pilot_dir)


class ArchitectureBootstrapOnResumeTests(unittest.TestCase):
	"""Regression for a real P2 review finding: a pilot already FROZEN on
	entry -- a resume, or one whose spec/tickets were produced by an
	earlier version of this script or a standalone /spec-plan invocation
	-- skips step2_draft_spec() entirely, so write_architecture_stub()
	(called only from inside step2_draft_spec() at the time) never ran,
	leaving the later mandatory `factoryd` preflight to fail on a missing
	ARCHITECTURE.md that nothing in the resumed run would otherwise
	create."""

	def test_bootstraps_architecture_md_on_a_frozen_resume_that_lacks_it(self):
		with tempfile.TemporaryDirectory() as d:
			pilot_dir = Path(d)
			(pilot_dir / "Makefile").write_text("run:\n\t@true\n")
			(pilot_dir / "spec").mkdir()
			(pilot_dir / "spec" / "spec.md").write_text("STATUS: FROZEN -- reviewed 2026-08-01T00:00:00+00:00\n")
			self.assertFalse((pilot_dir / "ARCHITECTURE.md").exists())

			argv = [
				"goal_pilot.py", "--spec-input", "irrelevant, spec already frozen",
				"--pilot-dir", str(pilot_dir), "--non-interactive",
			]
			with mock.patch.object(sys, "argv", argv), \
				mock.patch.object(goal_pilot, "step4_compile", return_value=False):
				exit_code = goal_pilot.main()

			# step4_compile was mocked to refuse (return False), so the run
			# halts right after the bootstrap -- but ARCHITECTURE.md must
			# already exist by then, not only on a fresh /spec-plan path.
			self.assertEqual(exit_code, 1)
			self.assertTrue((pilot_dir / "ARCHITECTURE.md").exists())


class InjectOnlyCLITests(unittest.TestCase):
	"""Regression coverage for a real P1 review finding: a /spec-plan
	invocation run standalone (the entry point README.md documents,
	outside goal_pilot.py's own orchestration) had no way to reach
	inject_ticketspec_keys() at all before this flag existed."""

	def test_inject_only_runs_injector_and_exits_without_requiring_other_args(self):
		with tempfile.TemporaryDirectory() as d:
			pilot_dir = Path(d)
			tickets_dir = pilot_dir / "spec" / "tickets"
			tickets_dir.mkdir(parents=True)
			ticket_path = tickets_dir / "002-x.md"
			ticket_path.write_text(
				"## Required changes\n\n1. Edit `isprime.py`.\n\n"
				"## Verification\n\n`make verify` must pass.\n\n## Commit\n\nx\n"
			)

			argv = ["goal_pilot.py", "--inject-only", str(pilot_dir)]
			with mock.patch.object(sys, "argv", argv):
				exit_code = goal_pilot.main()

			self.assertEqual(exit_code, 0)
			self.assertIn("Allowed-Files: isprime.py", ticket_path.read_text())

	def test_inject_only_also_bootstraps_architecture_stub(self):
		"""Regression for a real P2 review finding: a standalone /spec-plan
		invocation reaches this flag but not step2_draft_spec()/main()'s own
		frozen-resume bootstrap, so write_architecture_stub() being called
		only from those two places left standalone runs still missing
		ARCHITECTURE.md, and the later mandatory `factoryd` preflight would
		still fail for exactly the audience --inject-only exists for."""
		with tempfile.TemporaryDirectory() as d:
			pilot_dir = Path(d)
			(pilot_dir / "spec" / "tickets").mkdir(parents=True)

			argv = ["goal_pilot.py", "--inject-only", str(pilot_dir)]
			with mock.patch.object(sys, "argv", argv):
				exit_code = goal_pilot.main()

			self.assertEqual(exit_code, 0)
			self.assertTrue((pilot_dir / "ARCHITECTURE.md").exists())

	def test_missing_required_args_without_inject_only_errors(self):
		argv = ["goal_pilot.py"]
		with mock.patch.object(sys, "argv", argv):
			with self.assertRaises(SystemExit):
				goal_pilot.main()


TICKET_002_TEMPLATE = """This is an existing repo. Read `ARCHITECTURE.md`, `PROGRESS.md`, and `spec/contract.md` before changing anything, and preserve all existing functionality and passing tests -- this is an extension, not a rewrite.

## Goal

Harden the CLI against invalid input.

## Required changes

1. Extend `isprime.py`'s argument handling so every invalid invocation is a usage error.
2. Add unit tests in `test_isprime.py` for the new parsing behavior.

Update `ARCHITECTURE.md` and append a `PROGRESS.md` entry before finishing.

## Verification

- `make verify` must pass
- Confirm `make verify-full` still passes.

## Commit

Commit once both pass. Commit message must be exactly: `ticket(002): input-validation`
"""


class ExtractRequiredChangePathsTests(unittest.TestCase):
	def test_finds_real_paths_including_the_mandatory_state_files(self):
		# ARCHITECTURE.md/PROGRESS.md are included, not skipped: a real P1
		# found via review -- they are NOT in buildgate's own
		# harnessByproducts exemption list (only .gitignore/BUILD_REPORT.md/
		# BUILD_EVIDENCE.json/.pi-build-session/ are), and every ticket
		# after 001 edits both via this mandatory closing instruction. An
		# earlier version of this exclusion list wrongly treated them as
		# already-exempted and stripped them from every generated ticket's
		# own Allowed-Files, which would have quarantined every one of them
		# on diff_scope in practice.
		paths = goal_pilot.extract_required_change_paths(TICKET_002_TEMPLATE)
		self.assertEqual(paths, ["isprime.py", "test_isprime.py", "ARCHITECTURE.md", "PROGRESS.md"])

	def test_ignores_prose_and_commit_template(self):
		content = (
			"## Required changes\n\n"
			"1. Run `make verify` first (not a real path).\n"
			"2. Edit `internal/prime/prime.go`.\n\n"
			"## Commit\n\n"
			"Commit message must be exactly: `ticket(002): x`\n"
		)
		self.assertEqual(goal_pilot.extract_required_change_paths(content), ["internal/prime/prime.go"])

	def test_ignores_spec_contract_reference_citations(self):
		# Real bug found in a live end-to-end /spec-plan run (2026-08-30):
		# a ticket's own prose routinely cites `spec/contract.md` as the
		# source of an expected error message or exit code -- a reference,
		# not a file this ticket edits -- and the earlier version of this
		# function had no exclusion for it.
		content = (
			"## Required changes\n\n"
			"1. Edit `isprime.py` so it matches the error text `spec/contract.md` specifies.\n"
		)
		self.assertEqual(goal_pilot.extract_required_change_paths(content), ["isprime.py"])

	def test_ignores_numeric_examples_that_look_like_extensions(self):
		# Real bug found in the same live run: a non-integer CLI-argument
		# example like `3.5` matched the earlier, looser "any 1-6 alnum
		# chars after a dot" extension check and was extracted as if it
		# were a file.
		content = (
			"## Required changes\n\n"
			"1. Edit `isprime.py` to reject non-integer input such as `3.5` or `abc`.\n"
		)
		self.assertEqual(goal_pilot.extract_required_change_paths(content), ["isprime.py"])

	def test_ignores_qualified_identifiers_that_look_like_extensions(self):
		# Real bug found via a live brownfield /spec-plan run against
		# example-app, 2026-09-10: a ticket's own "## Required
		# changes" prose backtick-quotes a qualified Go identifier it is
		# adding alongside a real file -- e.g. "add the sentinel errors
		# `domain.ErrNoteTitleTooLong` and `domain.ErrNoteBodyTooLong`" --
		# and the old case-insensitive FILE_EXTENSION_RE treated
		# "ErrNoteTitleTooLong" as a plausible extension, extracting the
		# whole identifier as a required-changed path. That path can never
		# appear in a real diff, so required_files_changed would
		# permanently quarantine an otherwise-correct ticket.
		content = (
			"## Required changes\n\n"
			"1. Add the sentinel errors `domain.ErrNoteTitleTooLong` and "
			"`domain.ErrNoteBodyTooLong`.\n"
			"2. Edit `internal/note/validation.go`.\n"
		)
		self.assertEqual(
			goal_pilot.extract_required_change_paths(content),
			["internal/note/validation.go"],
		)

	def test_recognizes_extensionless_conventional_filenames(self):
		content = (
			"## Required changes\n\n"
			"1. Add a `Makefile` target.\n"
			"2. Update `Dockerfile` to install the new dependency.\n"
		)
		self.assertEqual(goal_pilot.extract_required_change_paths(content), ["Makefile", "Dockerfile"])

	def test_recognizes_screaming_case_extensionless_files_not_on_any_fixed_list(self):
		# Real finding via review: a fixed filename allowlist can never be
		# exhaustive (Bazel's WORKSPACE/BUILD, git's CODEOWNERS, LICENSE,
		# none of which this repo's own list happened to name). The shape
		# rule (SCREAMING_CASE) recognizes them without enumerating them.
		content = (
			"## Required changes\n\n"
			"1. Add a `WORKSPACE` file.\n"
			"2. Add a `BUILD` file alongside it.\n"
			"3. Add `CODEOWNERS` for this directory.\n"
			"4. Add a `LICENSE` file.\n"
			"5. Reject a bare CLI-argument example like `abc` or `OK` (too short to count).\n"
		)
		self.assertEqual(
			goal_pilot.extract_required_change_paths(content),
			["WORKSPACE", "BUILD", "CODEOWNERS", "LICENSE"],
		)

	def test_recognizes_readme_when_explicitly_named(self):
		# Real finding via review: README.md was unconditionally excluded
		# on the same wrong assumption as ARCHITECTURE.md/PROGRESS.md --
		# but unlike those two, it's not universally edited, so a ticket
		# that explicitly names it as a real required change had it
		# silently stripped from scope for no reason at all.
		content = "## Required changes\n\n1. Update `README.md` to document the new flag.\n"
		self.assertEqual(goal_pilot.extract_required_change_paths(content), ["README.md"])

	def test_recognizes_long_and_unusual_extensions(self):
		content = "## Required changes\n\n1. Add `schema.graphql`.\n"
		self.assertEqual(goal_pilot.extract_required_change_paths(content), ["schema.graphql"])

	def test_ignores_routes_urls_and_traversal(self):
		# Real bug found via review: a bare "/" in candidate" check
		# classified a route, URL, or directory reference as a file this
		# ticket edits, even though none of them can ever match a real
		# workspace-relative changed-file path -- required_files_changed
		# can then never be satisfied, quarantining an otherwise-correct
		# run forever.
		content = (
			"## Required changes\n\n"
			"1. Expose a new `/health` endpoint.\n"
			"2. See `docs/` for the existing convention.\n"
			"3. Do not touch `../outside.py`.\n"
			"4. Follow the schema at `https://example.com/schema`.\n"
			"5. Edit `internal/prime/prime.go`.\n"
		)
		self.assertEqual(goal_pilot.extract_required_change_paths(content), ["internal/prime/prime.go"])

	def test_no_required_changes_section_is_empty(self):
		self.assertEqual(goal_pilot.extract_required_change_paths("## Goal\n\nx\n"), [])


class InjectTicketspecKeysTests(unittest.TestCase):
	def test_injects_keys_into_ticket_with_real_paths(self):
		with tempfile.TemporaryDirectory() as d:
			pilot_dir = Path(d)
			tickets_dir = pilot_dir / "spec" / "tickets"
			tickets_dir.mkdir(parents=True)
			ticket_path = tickets_dir / "002-input-validation.md"
			ticket_path.write_text(TICKET_002_TEMPLATE)

			modified = goal_pilot.inject_ticketspec_keys(pilot_dir)

			self.assertEqual(modified, [ticket_path])
			updated = ticket_path.read_text()
			# make verify && make verify-full, not make verify alone --
			# ticket_runner.py's own gate requires both, and the ticket's
			# own prose already promises both.
			self.assertIn("Verify-Command: make verify && make verify-full", updated)
			self.assertIn("Allowed-Files: isprime.py, test_isprime.py, ARCHITECTURE.md, PROGRESS.md", updated)
			self.assertIn("Required-Changed-Files: isprime.py, test_isprime.py, ARCHITECTURE.md, PROGRESS.md", updated)
			# The declaration must land inside ## Verification, before ##
			# Commit -- not after it, which would silently become part of
			# the commit-message section instead.
			self.assertLess(updated.index("Verify-Command:"), updated.index("## Commit"))

	def test_verify_command_override_replaces_the_hardcoded_default(self):
		# Real bug found via a live brownfield /spec-plan run against
		# example-app, 2026-09-10: the hardcoded "make verify &&
		# make verify-full" default assumes the target repo's own
		# Makefile defines both targets -- true only for a repo this
		# pipeline scaffolds from scratch. An existing repo being
		# onboarded has no reason to define verify-full at all, and
		# declaring a command that can never pass permanently quarantines
		# an otherwise-correct ticket.
		with tempfile.TemporaryDirectory() as d:
			pilot_dir = Path(d)
			tickets_dir = pilot_dir / "spec" / "tickets"
			tickets_dir.mkdir(parents=True)
			ticket_path = tickets_dir / "002-input-validation.md"
			ticket_path.write_text(TICKET_002_TEMPLATE)

			modified = goal_pilot.inject_ticketspec_keys(pilot_dir, verify_command="make verify")

			self.assertEqual(modified, [ticket_path])
			updated = ticket_path.read_text()
			# The injected Verify-Command: key uses the override verbatim
			# (not appending && make verify-full itself); the ticket's own
			# pre-existing "## Verification" prose still mentions
			# verify-full, since that's the template's prose, not this
			# injected key -- only the injected line is asserted here.
			self.assertIn("Verify-Command: make verify\n", updated)
			self.assertNotIn("Verify-Command: make verify &&", updated)

	def test_verify_command_override_replaces_an_already_declared_stale_line(self):
		# Real gap found via adversarial review of the fix above
		# (2026-09-11): a ticket that already carries the unsatisfiable
		# hardcoded default (e.g. from a run before --verify-command was
		# passed) used to be silently skipped -- this function only
		# injects a key that's MISSING, and Verify-Command: was already
		# present. That made `--inject-only <dir> --verify-command ...`,
		# the documented remediation for exactly this ticket, a no-op on
		# the one ticket it exists to fix.
		with tempfile.TemporaryDirectory() as d:
			pilot_dir = Path(d)
			tickets_dir = pilot_dir / "spec" / "tickets"
			tickets_dir.mkdir(parents=True)
			ticket_path = tickets_dir / "002-input-validation.md"
			# Already carries the stale hardcoded default, as if injected
			# by an earlier run with no override.
			ticket_path.write_text(TICKET_002_TEMPLATE)
			goal_pilot.inject_ticketspec_keys(pilot_dir)  # first pass: injects the stale hardcoded default
			self.assertIn("Verify-Command: make verify && make verify-full", ticket_path.read_text())

			modified = goal_pilot.inject_ticketspec_keys(pilot_dir, verify_command="make verify")

			self.assertEqual(modified, [ticket_path])
			updated = ticket_path.read_text()
			self.assertIn("Verify-Command: make verify\n", updated)
			self.assertNotIn("Verify-Command: make verify && make verify-full", updated)
			# Exactly one Verify-Command: line, not two -- the stale one was
			# replaced in place, not left alongside a second, fresh one.
			self.assertEqual(updated.count("Verify-Command:"), 1)
			# Allowed-Files:/Required-Changed-Files: (unaffected by the
			# override) must survive untouched, not be dropped as a side
			# effect of rewriting Verify-Command:.
			self.assertIn("Allowed-Files: isprime.py, test_isprime.py, ARCHITECTURE.md, PROGRESS.md", updated)

	def test_verify_command_override_is_a_no_op_when_already_matching(self):
		with tempfile.TemporaryDirectory() as d:
			pilot_dir = Path(d)
			tickets_dir = pilot_dir / "spec" / "tickets"
			tickets_dir.mkdir(parents=True)
			ticket_path = tickets_dir / "002-input-validation.md"
			ticket_path.write_text(TICKET_002_TEMPLATE)
			goal_pilot.inject_ticketspec_keys(pilot_dir, verify_command="make verify")
			first_pass = ticket_path.read_text()

			modified = goal_pilot.inject_ticketspec_keys(pilot_dir, verify_command="make verify")

			self.assertEqual(modified, [])
			self.assertEqual(ticket_path.read_text(), first_pass)

	def test_skips_ticket_001_walking_skeleton_exemption(self):
		with tempfile.TemporaryDirectory() as d:
			pilot_dir = Path(d)
			tickets_dir = pilot_dir / "spec" / "tickets"
			tickets_dir.mkdir(parents=True)
			ticket_path = tickets_dir / "001-walking-skeleton.md"
			ticket_path.write_text(TICKET_002_TEMPLATE.replace("ticket(002)", "ticket(001)"))

			modified = goal_pilot.inject_ticketspec_keys(pilot_dir)

			self.assertEqual(modified, [])
			self.assertNotIn("Verify-Command:", ticket_path.read_text())

	def test_skips_ticket_already_declaring_all_three_keys(self):
		with tempfile.TemporaryDirectory() as d:
			pilot_dir = Path(d)
			tickets_dir = pilot_dir / "spec" / "tickets"
			tickets_dir.mkdir(parents=True)
			ticket_path = tickets_dir / "002-already-declared.md"
			ticket_path.write_text(
				TICKET_002_TEMPLATE
				+ "\nVerify-Command: make verify-full\n"
				+ "Allowed-Files: isprime.py\n"
				+ "Required-Changed-Files: isprime.py\n"
			)

			modified = goal_pilot.inject_ticketspec_keys(pilot_dir)

			# Not modified, and the hand-declared Verify-Command is not
			# overwritten with the fixed default -- an existing declaration
			# (hand-edited, or from a prior run of this function) always
			# wins, once all three keys are already present.
			self.assertEqual(modified, [])
			self.assertIn("Verify-Command: make verify-full", ticket_path.read_text())

	def test_fills_only_missing_keys_when_partially_declared(self):
		# Real bug found via review: a ticket declaring only
		# Verify-Command: by hand used to be treated as "already fully
		# declared" and skipped entirely, silently leaving Allowed-Files:/
		# Required-Changed-Files: undeclared.
		with tempfile.TemporaryDirectory() as d:
			pilot_dir = Path(d)
			tickets_dir = pilot_dir / "spec" / "tickets"
			tickets_dir.mkdir(parents=True)
			ticket_path = tickets_dir / "002-partially-declared.md"
			ticket_path.write_text(TICKET_002_TEMPLATE + "\nVerify-Command: make verify-full\n")

			modified = goal_pilot.inject_ticketspec_keys(pilot_dir)

			self.assertEqual(modified, [ticket_path])
			updated = ticket_path.read_text()
			# The hand-declared Verify-Command survives untouched...
			self.assertIn("Verify-Command: make verify-full", updated)
			self.assertNotIn("Verify-Command: make verify &&", updated)
			# ...but the two missing keys are now filled in.
			self.assertIn("Allowed-Files: isprime.py, test_isprime.py, ARCHITECTURE.md, PROGRESS.md", updated)
			self.assertIn("Required-Changed-Files: isprime.py, test_isprime.py, ARCHITECTURE.md, PROGRESS.md", updated)

	def test_still_injects_verify_command_with_no_extractable_path_at_all(self):
		# Verify-Command: doesn't depend on knowing any file path, so it's
		# injected even for a ticket this vague -- only Allowed-Files:/
		# Required-Changed-Files: (which do need real paths to be
		# meaningful) are skipped.
		with tempfile.TemporaryDirectory() as d:
			pilot_dir = Path(d)
			tickets_dir = pilot_dir / "spec" / "tickets"
			tickets_dir.mkdir(parents=True)
			ticket_path = tickets_dir / "002-vague.md"
			content = (
				"## Required changes\n\n1. Improve error handling generally.\n\n"
				"## Verification\n\n`make verify` must pass.\n\n## Commit\n\nx\n"
			)
			ticket_path.write_text(content)

			modified = goal_pilot.inject_ticketspec_keys(pilot_dir)

			self.assertEqual(modified, [ticket_path])
			updated = ticket_path.read_text()
			self.assertIn("Verify-Command: make verify && make verify-full", updated)
			self.assertNotIn("Allowed-Files:", updated)
			self.assertNotIn("Required-Changed-Files:", updated)

	def test_skips_allowed_files_but_still_injects_verify_command_with_only_boilerplate_paths(self):
		# Real bug found live, twice: calc-app tickets 002-003
		# (2026-09-05) and notes-demo tickets 002-005 (2026-09-06) each
		# described their real changes by package/endpoint/directory name
		# ("Implement the list-notes endpoint per spec/contract.md") rather
		# than a literal backtick-quoted file path -- so the only paths
		# extract_required_change_paths found were the two the mandatory
		# closing instruction always contributes. `if not paths: continue`
		# never fires in that case (paths is ["ARCHITECTURE.md",
		# "PROGRESS.md"], not empty), so Allowed-Files/Required-Changed-Files
		# got injected as just those two -- which then quarantines any
		# correct implementation the moment it touches real source files,
		# exactly the failure the "nothing concrete to declare" skip exists
		# to prevent. Both projects needed hand correction after the fact
		# (calc-app commit c1ca6b4, notes-demo ticket 001's spec-freeze
		# commit) before this fix.
		#
		# Verify-Command: is unaffected by any of this -- it doesn't depend
		# on knowing a file path at all (found via Codex review of this
		# same fix's first version, which wrongly skipped it too): omitting
		# it lets a direct `factoryd` run skip canonical_verify entirely.
		with tempfile.TemporaryDirectory() as d:
			pilot_dir = Path(d)
			tickets_dir = pilot_dir / "spec" / "tickets"
			tickets_dir.mkdir(parents=True)
			ticket_path = tickets_dir / "002-list-notes.md"
			content = (
				"## Required changes\n\n"
				"1. Implement the list-notes endpoint per `spec/contract.md`.\n"
				"2. Extend the frontend to render the list.\n\n"
				"Update `ARCHITECTURE.md` and append a `PROGRESS.md` entry before finishing.\n\n"
				"## Verification\n\n`make verify` must pass.\n\n## Commit\n\nx\n"
			)
			ticket_path.write_text(content)

			modified = goal_pilot.inject_ticketspec_keys(pilot_dir)

			self.assertEqual(modified, [ticket_path])
			updated = ticket_path.read_text()
			self.assertIn("Verify-Command: make verify && make verify-full", updated)
			self.assertNotIn("Allowed-Files:", updated)
			self.assertNotIn("Required-Changed-Files:", updated)

	def test_already_declared_verify_command_is_left_alone_with_only_boilerplate_paths(self):
		# The other half of the same fix: if Verify-Command: is already
		# present (hand-edited, or injected by a prior run), a ticket with
		# only the boilerplate paths has nothing left to inject at all.
		with tempfile.TemporaryDirectory() as d:
			pilot_dir = Path(d)
			tickets_dir = pilot_dir / "spec" / "tickets"
			tickets_dir.mkdir(parents=True)
			ticket_path = tickets_dir / "002-list-notes.md"
			content = (
				"## Required changes\n\n"
				"1. Implement the list-notes endpoint per `spec/contract.md`.\n\n"
				"Update `ARCHITECTURE.md` and append a `PROGRESS.md` entry before finishing.\n\n"
				"## Verification\n\n`make verify` must pass.\n\n"
				"Verify-Command: make verify && make verify-full\n\n"
				"## Commit\n\nx\n"
			)
			ticket_path.write_text(content)

			modified = goal_pilot.inject_ticketspec_keys(pilot_dir)

			self.assertEqual(modified, [])
			self.assertNotIn("Allowed-Files:", ticket_path.read_text())

	def test_still_injects_when_real_paths_accompany_the_mandatory_closing_ones(self):
		# The fix above must not regress the ordinary case: a ticket that
		# does name a real path alongside the mandatory two still gets the
		# full set injected, ARCHITECTURE.md/PROGRESS.md included.
		with tempfile.TemporaryDirectory() as d:
			pilot_dir = Path(d)
			tickets_dir = pilot_dir / "spec" / "tickets"
			tickets_dir.mkdir(parents=True)
			ticket_path = tickets_dir / "002-input-validation.md"
			ticket_path.write_text(TICKET_002_TEMPLATE)

			modified = goal_pilot.inject_ticketspec_keys(pilot_dir)

			self.assertEqual(modified, [ticket_path])
			self.assertIn(
				"Allowed-Files: isprime.py, test_isprime.py, ARCHITECTURE.md, PROGRESS.md",
				ticket_path.read_text(),
			)

	def test_no_tickets_dir_returns_empty(self):
		with tempfile.TemporaryDirectory() as d:
			self.assertEqual(goal_pilot.inject_ticketspec_keys(Path(d)), [])


class CheckAcceptanceSuitePathsTests(unittest.TestCase):
	def test_no_acceptance_dir_returns_no_warnings(self):
		with tempfile.TemporaryDirectory() as d:
			self.assertEqual(goal_pilot.check_acceptance_suite_paths(Path(d)), [])

	def test_flags_the_exact_notes_app_regression(self):
		# The real bug (notes-demo, 2026-09-06): every one of 5 acceptance
		# slices computed "where the app lives" as a fixed __file__-relative
		# parent-directory climb, which has no single correct answer across
		# this pipeline's different build flows -- see the function's own
		# docstring. Confirmed this was NOT actually about the literal word
		# "workspace" (a code-review pass showed a correct-looking
		# `os.getcwd()`-free repo-root guess is equally wrong in the flow
		# where it isn't): the check now flags the climb itself.
		with tempfile.TemporaryDirectory() as d:
			pilot_dir = Path(d)
			slice_dir = pilot_dir / "spec" / "acceptance" / "001"
			slice_dir.mkdir(parents=True)
			(slice_dir / "test_001_create.py").write_text(
				"import os\n"
				"WORKSPACE = os.path.abspath(\n"
				'    os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "..", "..", "workspace")\n'
				")\n"
			)
			warnings = goal_pilot.check_acceptance_suite_paths(pilot_dir)
			self.assertEqual(len(warnings), 1)
			self.assertIn("test_001_create.py", warnings[0])
			self.assertIn("os.getcwd()", warnings[0])

	def test_flags_a_fixed_repo_root_guess_too_not_just_workspace(self):
		# The check's own first version only flagged a literal "workspace"
		# segment, on the theory that the pilot dir's own root was always
		# the correct answer instead -- a real code-review pass showed
		# that's equally wrong in the flow where ticket_runner.py's own
		# `make run` builds directly inside workspace/ for real. Neither
		# fixed guess is correct in every flow; only os.getcwd() is.
		with tempfile.TemporaryDirectory() as d:
			pilot_dir = Path(d)
			slice_dir = pilot_dir / "spec" / "acceptance" / "001"
			slice_dir.mkdir(parents=True)
			(slice_dir / "test_001_create.py").write_text(
				"import os\n"
				"WORKSPACE = os.path.abspath(\n"
				'    os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "..", "..")\n'
				")\n"
			)
			self.assertEqual(len(goal_pilot.check_acceptance_suite_paths(pilot_dir)), 1)

	def test_reports_every_slice_independently(self):
		with tempfile.TemporaryDirectory() as d:
			pilot_dir = Path(d)
			bad = 'WORKSPACE = os.path.join(os.path.dirname(__file__), "..", "..", "..", "workspace")\n'
			for n in ("001", "002"):
				slice_dir = pilot_dir / "spec" / "acceptance" / n
				slice_dir.mkdir(parents=True)
				(slice_dir / f"test_{n}.py").write_text(bad)
			warnings = goal_pilot.check_acceptance_suite_paths(pilot_dir)
			self.assertEqual(len(warnings), 2)

	def test_flags_pathlib_division_and_parent_chains(self):
		with tempfile.TemporaryDirectory() as d:
			pilot_dir = Path(d)
			slice_dir = pilot_dir / "spec" / "acceptance" / "001"
			slice_dir.mkdir(parents=True)
			(slice_dir / "test_001.py").write_text(
				'WORKSPACE = Path(__file__).resolve().parent.parent.parent / "workspace"\n'
			)
			self.assertEqual(len(goal_pilot.check_acceptance_suite_paths(pilot_dir)), 1)

	def test_flags_joinpath_call(self):
		with tempfile.TemporaryDirectory() as d:
			pilot_dir = Path(d)
			slice_dir = pilot_dir / "spec" / "acceptance" / "001"
			slice_dir.mkdir(parents=True)
			(slice_dir / "test_001.py").write_text(
				'WORKSPACE = Path(__file__).parent.parent.parent.joinpath("workspace")\n'
			)
			self.assertEqual(len(goal_pilot.check_acceptance_suite_paths(pilot_dir)), 1)

	def test_flags_regardless_of_join_being_imported_bare(self):
		# The climb-count is what matters, not which name a join-style
		# function is called through -- unlike the check's own prior
		# version, nothing here needs to specifically recognize
		# `os.path.join` vs. a bare `join` from `from os.path import
		# join`: the ".." string constants are counted either way.
		with tempfile.TemporaryDirectory() as d:
			pilot_dir = Path(d)
			slice_dir = pilot_dir / "spec" / "acceptance" / "001"
			slice_dir.mkdir(parents=True)
			(slice_dir / "test_001.py").write_text(
				"from os.path import dirname, abspath, join\n"
				'WORKSPACE = join(dirname(abspath(__file__)), "..", "..", "..", "workspace")\n'
			)
			self.assertEqual(len(goal_pilot.check_acceptance_suite_paths(pilot_dir)), 1)

	def test_does_not_flag_a_single_level_sibling_lookup(self):
		# Finding a file next to the slice itself (one level, no farther)
		# is a normal, unrelated use of __file__ -- not the app-root
		# computation this check exists to catch.
		with tempfile.TemporaryDirectory() as d:
			pilot_dir = Path(d)
			slice_dir = pilot_dir / "spec" / "acceptance" / "001"
			slice_dir.mkdir(parents=True)
			(slice_dir / "test_001.py").write_text(
				'FIXTURE = os.path.join(os.path.dirname(__file__), "fixture.json")\n'
			)
			self.assertEqual(goal_pilot.check_acceptance_suite_paths(pilot_dir), [])

	def test_does_not_flag_the_recommended_os_getcwd_pattern(self):
		with tempfile.TemporaryDirectory() as d:
			pilot_dir = Path(d)
			slice_dir = pilot_dir / "spec" / "acceptance" / "001"
			slice_dir.mkdir(parents=True)
			(slice_dir / "test_001.py").write_text(
				"import os\n"
				"APP_ROOT = os.getcwd()\n"
			)
			self.assertEqual(goal_pilot.check_acceptance_suite_paths(pilot_dir), [])

	def test_does_not_flag_unrelated_code_with_no_file_dunder(self):
		with tempfile.TemporaryDirectory() as d:
			pilot_dir = Path(d)
			slice_dir = pilot_dir / "spec" / "acceptance" / "001"
			slice_dir.mkdir(parents=True)
			(slice_dir / "test_001.py").write_text(
				'CONFIG = os.path.join(BASE_DIR, "config.json")\n'
				'IGNORE_DIRS = ("node_modules", "workspace")\n'
			)
			self.assertEqual(goal_pilot.check_acceptance_suite_paths(pilot_dir), [])

	def test_syntax_error_returns_no_warnings_rather_than_raising(self):
		with tempfile.TemporaryDirectory() as d:
			pilot_dir = Path(d)
			slice_dir = pilot_dir / "spec" / "acceptance" / "001"
			slice_dir.mkdir(parents=True)
			(slice_dir / "test_001.py").write_text("def broken(:\n")
			self.assertEqual(goal_pilot.check_acceptance_suite_paths(pilot_dir), [])


if __name__ == "__main__":
	unittest.main()
