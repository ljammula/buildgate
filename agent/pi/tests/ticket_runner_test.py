import contextlib
import importlib.util
import io
import json
import signal
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path
from unittest import mock


SCRIPT = Path(__file__).resolve().parents[1] / "scripts" / "ticket_runner.py"
SPEC = importlib.util.spec_from_file_location("ticket_runner", SCRIPT)
assert SPEC and SPEC.loader
ticket_runner = importlib.util.module_from_spec(SPEC)
sys.modules[SPEC.name] = ticket_runner
SPEC.loader.exec_module(ticket_runner)


class TicketRunnerRetryTests(unittest.TestCase):
	def setUp(self):
		self.ticket = ticket_runner.Ticket(1, "workspace-scaffold", Path("001-workspace-scaffold.md"))

	def next_mode(self, pilot_dir, workspace, commit_sha=None, review_policy="advisory"):
		with mock.patch.object(ticket_runner, "commit_sha_for", return_value=commit_sha):
			return ticket_runner.next_ticket([self.ticket], pilot_dir, workspace, review_policy)[1]

	def test_fresh_ticket_starts_a_build(self):
		with tempfile.TemporaryDirectory() as directory:
			root = Path(directory)
			self.assertEqual(self.next_mode(root, root / "workspace"), "build")

	def test_builder_command_uses_three_internal_rounds(self):
		command = ticket_runner.builder_command(Path("workspace"), self.ticket, "deadbeef")
		self.assertEqual(command[command.index("--max-rounds") + 1], "3")
		self.assertEqual(command[command.index("--review-policy") + 1], "advisory")

	def test_builder_command_accepts_strict_review_for_release_runs(self):
		command = ticket_runner.builder_command(Path("workspace"), self.ticket, "deadbeef", "required")
		self.assertEqual(command[command.index("--review-policy") + 1], "required")

	def test_required_mode_rebuilds_a_passing_advisory_gate(self):
		with tempfile.TemporaryDirectory() as directory:
			root = Path(directory)
			reports = root / "reports" / "ticket-001"
			reports.mkdir(parents=True)
			(reports / "gate.json").write_text(json.dumps({
				"ticket": "001",
				"review_policy": "advisory",
				"passed": True,
				"checks": [],
			}))
			self.assertEqual(
				self.next_mode(root, root / "workspace", "commit-sha", "required"),
				"policy",
			)

	def test_required_mode_accepts_a_passing_required_gate(self):
		with tempfile.TemporaryDirectory() as directory:
			root = Path(directory)
			reports = root / "reports" / "ticket-001"
			reports.mkdir(parents=True)
			(reports / "gate.json").write_text(json.dumps({
				"ticket": "001",
				"review_policy": "required",
				"passed": True,
				"checks": [],
			}))
			self.assertEqual(
				self.next_mode(root, root / "workspace", "commit-sha", "required"),
				None,
			)

	def test_unknown_explicit_gate_policy_is_not_trusted(self):
		gate = {"review_policy": "mystery", "passed": True, "checks": []}
		self.assertFalse(ticket_runner.gate_satisfies_review_policy(gate, "advisory"))

	def test_legacy_gate_without_policy_retains_required_semantics(self):
		gate = {"passed": True, "checks": []}
		self.assertTrue(ticket_runner.gate_satisfies_review_policy(gate, "required"))

	def test_required_mode_rejects_successful_advisory_build_report(self):
		with tempfile.TemporaryDirectory() as directory:
			workspace = Path(directory)
			(workspace / "BUILD_REPORT.md").write_text(
				"Review policy: `advisory`\nOutcome: SUCCEEDED\n"
			)
			ok, detail = ticket_runner.build_report_succeeded(workspace, "required")
		self.assertFalse(ok)
		self.assertIn("weaker than requested", detail)

	def test_required_mode_accepts_successful_required_build_report(self):
		with tempfile.TemporaryDirectory() as directory:
			workspace = Path(directory)
			(workspace / "BUILD_REPORT.md").write_text(
				"Review policy: `required`\nOutcome: SUCCEEDED\n"
			)
			self.assertTrue(ticket_runner.build_report_succeeded(workspace, "required")[0])

	def test_legacy_successful_build_report_retains_required_semantics(self):
		with tempfile.TemporaryDirectory() as directory:
			workspace = Path(directory)
			(workspace / "BUILD_REPORT.md").write_text("Outcome: SUCCEEDED\n")
			self.assertTrue(ticket_runner.build_report_succeeded(workspace, "required")[0])

	def test_builder_command_threads_review_base_sha_to_anchor_the_reviewer(self):
		# Without this, a build_app.py invocation retried against a ticket a
		# prior, interrupted attempt already committed sees an empty diff from
		# its own default "HEAD when this process starts" and can never get a
		# decisive review verdict for work nobody actually reviewed.
		command = ticket_runner.builder_command(Path("workspace"), self.ticket, "deadbeef")
		self.assertEqual(command[command.index("--review-base-sha") + 1], "deadbeef")

	def test_builder_command_omits_review_base_sha_when_none(self):
		# Ticket 1 in a fresh repo has no prior commit to anchor to;
		# build_app.py's own fallback (HEAD-at-process-start, or the
		# unborn-HEAD empty-tree case) covers this.
		command = ticket_runner.builder_command(Path("workspace"), self.ticket, None)
		self.assertNotIn("--review-base-sha", command)

	def test_interrupted_build_retries_instead_of_regating(self):
		with tempfile.TemporaryDirectory() as directory:
			root = Path(directory)
			(root / "reports" / "ticket-001").mkdir(parents=True)
			self.assertEqual(self.next_mode(root, root / "workspace", "commit-sha"), "retry")

	def test_missing_report_gate_retries_only_infrastructure_failure(self):
		with tempfile.TemporaryDirectory() as directory:
			root = Path(directory)
			reports = root / "reports" / "ticket-001"
			reports.mkdir(parents=True)
			(reports / "gate.json").write_text(json.dumps({
				"passed": False,
				"checks": [
					{"name": "make verify", "ok": True},
					{"name": "BUILD_REPORT.md SUCCEEDED", "ok": False},
				],
			}))
			self.assertEqual(self.next_mode(root, root / "workspace", "commit-sha"), "retry")

	def test_builder_timeout_retries_even_when_incomplete_verify_is_red(self):
		with tempfile.TemporaryDirectory() as directory:
			root = Path(directory)
			reports = root / "reports" / "ticket-001"
			reports.mkdir(parents=True)
			(reports / "gate.json").write_text(json.dumps({
				"passed": False,
				"checks": [
					{"name": "build_app.py invocation", "ok": False},
					{"name": "make verify", "ok": False},
					{"name": "BUILD_REPORT.md SUCCEEDED", "ok": False},
				],
			}))
			self.assertTrue(ticket_runner.retryable_build_state(root, root / "workspace", self.ticket))

	def test_stall_report_retries_even_when_builder_wrote_failure_evidence(self):
		with tempfile.TemporaryDirectory() as directory:
			root = Path(directory)
			workspace = root / "workspace"
			workspace.mkdir()
			(workspace / "BUILD_REPORT.md").write_text(
				"Outcome: DID NOT SUCCEED -- stall-timeout while contacting model\n"
			)
			reports = root / "reports" / "ticket-001"
			reports.mkdir(parents=True)
			(reports / "gate.json").write_text(json.dumps({
				"passed": False,
				"checks": [
					{"name": "make verify", "ok": False},
					{"name": "BUILD_REPORT.md SUCCEEDED", "ok": False},
				],
			}))
			self.assertTrue(ticket_runner.retryable_build_state(root, workspace, self.ticket))

	def test_final_timeout_outcome_retries(self):
		with tempfile.TemporaryDirectory() as directory:
			root = Path(directory)
			workspace = root / "workspace"
			workspace.mkdir()
			(workspace / "BUILD_REPORT.md").write_text(
				"Outcome: DID NOT SUCCEED -- local round budget (3) exhausted; "
				"escalation required: pi invocation timed out, canonical verification failed\n"
			)
			reports = root / "reports" / "ticket-001"
			reports.mkdir(parents=True)
			(reports / "gate.json").write_text(json.dumps({
				"passed": False,
				"checks": [
					{"name": "make verify", "ok": False},
					{"name": "BUILD_REPORT.md SUCCEEDED", "ok": False},
				],
			}))
			self.assertTrue(ticket_runner.retryable_build_state(root, workspace, self.ticket))

	def test_final_no_review_outcome_retries(self):
		with tempfile.TemporaryDirectory() as directory:
			root = Path(directory)
			workspace = root / "workspace"
			workspace.mkdir()
			(workspace / "BUILD_REPORT.md").write_text(
				"Outcome: DID NOT SUCCEED -- local round budget (3) exhausted; "
				"escalation required: review unavailable (no-review-verdict)\n"
			)
			reports = root / "reports" / "ticket-001"
			reports.mkdir(parents=True)
			(reports / "gate.json").write_text(json.dumps({
				"passed": False,
				"checks": [
					{"name": "make verify", "ok": True},
					{"name": "BUILD_REPORT.md SUCCEEDED", "ok": False},
				],
			}))
			self.assertTrue(ticket_runner.retryable_build_state(root, workspace, self.ticket))

	def test_build_attempt_numbers_survive_new_runner_processes(self):
		with tempfile.TemporaryDirectory() as directory:
			root = Path(directory)
			reports = root / "reports" / "ticket-001"
			reports.mkdir(parents=True)
			self.assertEqual(ticket_runner.next_build_attempt(root, self.ticket), 2)
			(reports / "build-attempt-02.started.json").write_text("{}")
			(reports / "build-attempt-02.log").write_text("completed")
			self.assertEqual(ticket_runner.next_build_attempt(root, self.ticket), 3)

	def test_interrupted_build_slot_is_resumed_without_consuming_another_slot(self):
		with tempfile.TemporaryDirectory() as directory:
			root = Path(directory)
			reports = root / "reports" / "ticket-001"
			reports.mkdir(parents=True)
			(reports / "build-attempt-03.started.json").write_text("{}")
			self.assertEqual(ticket_runner.next_build_attempt(root, self.ticket), 3)

	def test_interrupted_gate_slot_is_resumed_without_consuming_another_slot(self):
		with tempfile.TemporaryDirectory() as directory:
			root = Path(directory)
			reports = root / "reports" / "ticket-001"
			reports.mkdir(parents=True)
			(reports / "gate-attempt-03.started.json").write_text("{}")
			self.assertEqual(ticket_runner.next_gate_attempt(root, self.ticket), 3)

	def test_three_build_attempts_exhaust_durable_budget(self):
		with tempfile.TemporaryDirectory() as directory:
			root = Path(directory)
			reports = root / "reports" / "ticket-001"
			reports.mkdir(parents=True)
			for attempt in range(1, 4):
				(reports / f"build-attempt-{attempt:02d}.started.json").write_text("{}")
				(reports / f"build-attempt-{attempt:02d}.log").write_text("completed")
			self.assertEqual(ticket_runner.next_build_attempt(root, self.ticket), 4)
			self.assertGreater(ticket_runner.next_build_attempt(root, self.ticket), ticket_runner.MAX_BUILD_ATTEMPTS)

	def test_regates_do_not_consume_build_attempt_budget(self):
		with tempfile.TemporaryDirectory() as directory:
			root = Path(directory)
			reports = root / "reports" / "ticket-001"
			reports.mkdir(parents=True)
			(reports / "build-attempt-01.started.json").write_text("{}")
			(reports / "build-attempt-01.log").write_text("completed")
			for attempt in range(1, 5):
				(reports / f"gate-attempt-{attempt:02d}.json").write_text("{}")
			self.assertEqual(ticket_runner.next_build_attempt(root, self.ticket), 2)
			self.assertEqual(ticket_runner.next_gate_attempt(root, self.ticket), 5)

	def test_fresh_no_commit_run_stops_after_three_durable_builds(self):
		with tempfile.TemporaryDirectory() as directory:
			root = Path(directory)
			with (
				mock.patch.object(sys, "argv", ["ticket_runner.py", "--pilot-dir", str(root)]),
				mock.patch.object(ticket_runner, "discover_tickets", return_value=[self.ticket]),
				mock.patch.object(ticket_runner, "next_ticket", return_value=(self.ticket, "build")),
				mock.patch.object(ticket_runner, "next_build_attempt", return_value=4),
				mock.patch.object(ticket_runner, "append_halt_record"),
				mock.patch.object(ticket_runner, "run_ticket") as run_ticket,
			):
				self.assertEqual(ticket_runner.main(), 1)
			run_ticket.assert_not_called()

	def test_regate_main_path_does_not_request_a_build_attempt(self):
		with tempfile.TemporaryDirectory() as directory:
			root = Path(directory)
			with (
				mock.patch.object(sys, "argv", ["ticket_runner.py", "--pilot-dir", str(root)]),
				mock.patch.object(ticket_runner, "discover_tickets", return_value=[self.ticket]),
				mock.patch.object(ticket_runner, "next_ticket", return_value=(self.ticket, "regate")),
				mock.patch.object(ticket_runner, "next_build_attempt") as next_build,
				mock.patch.object(ticket_runner, "next_gate_attempt", return_value=3),
				mock.patch.object(ticket_runner, "run_ticket", return_value=False) as run_ticket,
			):
				self.assertEqual(ticket_runner.main(), 1)
			next_build.assert_not_called()
			run_ticket.assert_called_once_with(
				root.resolve(),
				root.resolve() / "workspace",
				self.ticket,
				[self.ticket],
				skip_build=True,
				gate_attempt=3,
				build_attempt=None,
				review_policy="advisory",
				verify_command=None,
			)

	def test_policy_upgrade_is_not_blocked_by_prior_build_attempt_budget(self):
		with tempfile.TemporaryDirectory() as directory:
			root = Path(directory)
			with (
				mock.patch.object(
					sys,
					"argv",
					["ticket_runner.py", "--pilot-dir", str(root), "--review-policy", "required"],
				),
				mock.patch.object(ticket_runner, "discover_tickets", return_value=[self.ticket]),
				mock.patch.object(ticket_runner, "next_ticket", return_value=(self.ticket, "policy")),
				mock.patch.object(ticket_runner, "next_build_attempt", return_value=4),
				mock.patch.object(ticket_runner, "next_gate_attempt", return_value=4),
				mock.patch.object(ticket_runner, "retryable_build_state", return_value=False),
				mock.patch.object(ticket_runner, "run_ticket", return_value=False) as run_ticket,
			):
				self.assertEqual(ticket_runner.main(), 1)
			run_ticket.assert_called_once_with(
				root.resolve(),
				root.resolve() / "workspace",
				self.ticket,
				[self.ticket],
				skip_build=False,
				gate_attempt=4,
				build_attempt=4,
				review_policy="required",
				verify_command=None,
			)

	def test_per_attempt_evidence_is_append_only_and_regates_are_separate(self):
		with tempfile.TemporaryDirectory() as directory:
			root = Path(directory)
			workspace = root / "workspace"
			workspace.mkdir()
			report = workspace / "BUILD_REPORT.md"
			report.write_text("Outcome: DID NOT SUCCEED -- first build\n")
			gate = {"ticket": "001", "passed": False, "checks": []}
			ticket_runner.archive_evidence(
				root, self.ticket, gate, gate_attempt=1, build_attempt=1,
			)
			report.write_text("Outcome: DID NOT SUCCEED -- regate\n")
			ticket_runner.archive_evidence(
				root, self.ticket, gate, gate_attempt=2, build_attempt=None,
			)
			reports = root / "reports" / "ticket-001"
			self.assertEqual(
				(reports / "BUILD_REPORT-attempt-01.md").read_text(),
				"Outcome: DID NOT SUCCEED -- first build\n",
			)
			self.assertEqual(
				(reports / "BUILD_REPORT-regate-02.md").read_text(),
				"Outcome: DID NOT SUCCEED -- regate\n",
			)
			with self.assertRaises(FileExistsError):
				ticket_runner.archive_evidence(
					root, self.ticket, gate, gate_attempt=2, build_attempt=None,
				)

	def test_timeout_kills_process_group_even_when_parent_exits_on_term(self):
		process = mock.Mock(pid=4321, returncode=None)
		process.communicate.side_effect = [
			subprocess.TimeoutExpired(["builder"], 1, output="partial", stderr="partial error"),
			("terminated", "terminated error"),
		]
		process.poll.return_value = 0
		with (
			mock.patch.object(ticket_runner.subprocess, "Popen", return_value=process),
			mock.patch.object(ticket_runner.os, "killpg") as killpg,
		):
			result = ticket_runner.invoke_build_app(["builder"], 1)
		self.assertEqual(result, (-1, "terminated", "terminated error", True))
		self.assertEqual(
			killpg.call_args_list,
			[mock.call(4321, signal.SIGTERM), mock.call(4321, signal.SIGKILL)],
		)

	def test_old_transient_marker_does_not_retry_real_final_failure(self):
		with tempfile.TemporaryDirectory() as directory:
			root = Path(directory)
			workspace = root / "workspace"
			workspace.mkdir()
			(workspace / "BUILD_REPORT.md").write_text(
				"Round 1: agent timed out: true\n"
				"Outcome: DID NOT SUCCEED -- canonical verification failed\n"
			)
			reports = root / "reports" / "ticket-001"
			reports.mkdir(parents=True)
			(reports / "gate.json").write_text(json.dumps({
				"passed": False,
				"checks": [
					{"name": "make verify", "ok": False},
					{"name": "BUILD_REPORT.md SUCCEEDED", "ok": False},
				],
			}))
			self.assertFalse(ticket_runner.retryable_build_state(root, workspace, self.ticket))

	def test_model_route_unreachable_is_retried_as_transient_infrastructure(self):
		# build_app.py's own outage short-circuit (round_blockers()) stops a
		# round early with this exact outcome text when every assistant turn
		# errored out -- no implementation work was ever attempted, so this
		# is infrastructure state, not a real gate failure, and should use
		# the bounded build-attempt retry instead of halting for a human
		# (Codex review of PR #28: this marker was previously missing from
		# TRANSIENT_BUILD_MARKERS, so an outage halted the line exactly like
		# a genuine implementation failure).
		with tempfile.TemporaryDirectory() as directory:
			root = Path(directory)
			workspace = root / "workspace"
			workspace.mkdir()
			(workspace / "BUILD_REPORT.md").write_text(
				"Outcome: DID NOT SUCCEED -- model route unreachable "
				"(3/3 assistant turns errored)\n"
			)
			reports = root / "reports" / "ticket-001"
			reports.mkdir(parents=True)
			(reports / "gate.json").write_text(json.dumps({
				"passed": False,
				"checks": [
					{"name": "make verify", "ok": False},
					{"name": "BUILD_REPORT.md SUCCEEDED", "ok": False},
				],
			}))
			self.assertTrue(ticket_runner.retryable_build_state(root, workspace, self.ticket))

	def test_model_route_unreachable_still_stops_on_oracle_or_surface_drift(self):
		# An outage report is only transient infrastructure noise as long as
		# nothing else genuinely went wrong; oracle/verify-surface drift is
		# never treated as retryable regardless of what else the report says.
		with tempfile.TemporaryDirectory() as directory:
			root = Path(directory)
			workspace = root / "workspace"
			workspace.mkdir()
			(workspace / "BUILD_REPORT.md").write_text(
				"Outcome: DID NOT SUCCEED -- model route unreachable "
				"(3/3 assistant turns errored)\n"
			)
			reports = root / "reports" / "ticket-001"
			reports.mkdir(parents=True)
			(reports / "gate.json").write_text(json.dumps({
				"passed": False,
				"checks": [
					{"name": "oracle integrity", "ok": False},
					{"name": "BUILD_REPORT.md SUCCEEDED", "ok": False},
				],
			}))
			self.assertFalse(ticket_runner.retryable_build_state(root, workspace, self.ticket))

	def test_real_gate_failure_remains_regate_only(self):
		with tempfile.TemporaryDirectory() as directory:
			root = Path(directory)
			reports = root / "reports" / "ticket-001"
			reports.mkdir(parents=True)
			(reports / "gate.json").write_text(json.dumps({
				"passed": False,
				"checks": [
					{"name": "make verify", "ok": False},
					{"name": "BUILD_REPORT.md SUCCEEDED", "ok": False},
				],
			}))
			self.assertEqual(self.next_mode(root, root / "workspace", "commit-sha"), "regate")


class GateRevisitCountTests(unittest.TestCase):
	def setUp(self):
		self.ticket = ticket_runner.Ticket(4, "onboarding-screen", Path("004-onboarding-screen.md"))

	def test_zero_when_no_reports_dir_exists(self):
		with tempfile.TemporaryDirectory() as directory:
			self.assertEqual(ticket_runner.gate_revisit_count(Path(directory), self.ticket), 0)

	def test_one_real_gate_attempt_is_not_a_revisit(self):
		with tempfile.TemporaryDirectory() as directory:
			reports = Path(directory) / "reports" / "ticket-004"
			reports.mkdir(parents=True)
			(reports / "gate-attempt-01.json").write_text("{}")
			(reports / "gate-attempt-01.started.json").write_text("{}")
			self.assertEqual(ticket_runner.gate_revisit_count(Path(directory), self.ticket), 1)

	def test_started_markers_never_count_as_real_attempts(self):
		# A crashed build-attempt (e.g. the ticket-011 cmux crash) leaves only
		# a .started.json marker behind with no completed .json evidence --
		# that must not be mistaken for a failed-then-revisited gate.
		with tempfile.TemporaryDirectory() as directory:
			reports = Path(directory) / "reports" / "ticket-004"
			reports.mkdir(parents=True)
			(reports / "gate-attempt-01.started.json").write_text("{}")
			self.assertEqual(ticket_runner.gate_revisit_count(Path(directory), self.ticket), 0)

	def test_multiple_real_gate_attempts_count_as_revisits(self):
		with tempfile.TemporaryDirectory() as directory:
			reports = Path(directory) / "reports" / "ticket-004"
			reports.mkdir(parents=True)
			for n in (1, 2, 3):
				(reports / f"gate-attempt-0{n}.json").write_text("{}")
				(reports / f"gate-attempt-0{n}.started.json").write_text("{}")
			self.assertEqual(ticket_runner.gate_revisit_count(Path(directory), self.ticket), 3)


class HaltRecordExistsTests(unittest.TestCase):
	def setUp(self):
		self.ticket = ticket_runner.Ticket(12, "persistence-and-hardening", Path("012-x.md"))

	def test_false_when_no_progress_md_exists(self):
		with tempfile.TemporaryDirectory() as directory:
			self.assertFalse(ticket_runner.halt_record_exists(Path(directory), self.ticket))

	def test_true_when_a_halt_record_for_this_ticket_is_present(self):
		with tempfile.TemporaryDirectory() as directory:
			workspace = Path(directory)
			(workspace / "PROGRESS.md").write_text(
				"\n\n## [runner-written] HALT at ticket 012 (2026-08-21T03:56:21+00:00)\n\n"
				"ticket_runner.py stopped the line here. Gate failures:\n- verify-surface frozen\n"
			)
			self.assertTrue(ticket_runner.halt_record_exists(workspace, self.ticket))

	def test_false_for_a_different_ticket_s_halt_record(self):
		with tempfile.TemporaryDirectory() as directory:
			workspace = Path(directory)
			(workspace / "PROGRESS.md").write_text(
				"\n\n## [runner-written] HALT at ticket 010 (2026-08-21T03:01:42+00:00)\n\n..."
			)
			self.assertFalse(ticket_runner.halt_record_exists(workspace, self.ticket))


class PrintStatusRescueAccountingTests(unittest.TestCase):
	def setUp(self):
		self.tickets = [
			ticket_runner.Ticket(1, "a", Path("001-a.md")),
			ticket_runner.Ticket(2, "b", Path("002-b.md")),
		]

	def run_status(self, pilot_dir, done_commits, halted_tickets):
		with (
			mock.patch.object(ticket_runner, "committed_ticket_numbers", return_value=done_commits),
			mock.patch.object(ticket_runner, "ticket_done", return_value=True),
			mock.patch.object(ticket_runner, "next_ticket", return_value=(None, "build")),
			mock.patch.object(
				ticket_runner, "halt_record_exists",
				side_effect=lambda _workspace, t: t.number in halted_tickets,
			),
		):
			buf = io.StringIO()
			with contextlib.redirect_stdout(buf):
				ticket_runner.print_status(pilot_dir, self.tickets, pilot_dir / "workspace")
			return buf.getvalue()

	def test_tagged_rescue_is_counted_and_labeled(self):
		with tempfile.TemporaryDirectory() as directory:
			out = self.run_status(
				Path(directory),
				done_commits={1: "ticket(001): a", 2: "ticket(002): b [rescued]"},
				halted_tickets=set(),
			)
		self.assertIn("rescued: 1 -- 1 tagged, 0 untagged halts", out)
		self.assertIn("[x] 002-b [rescued]", out)
		self.assertNotIn("[x] 001-a [rescued]", out)

	def test_untagged_halt_is_still_counted_as_a_rescue(self):
		# This is the accuracy fix: a ticket that genuinely halted (a real,
		# non-recoverable gate failure, e.g. an infra fix or a human
		# forgetting the commit-message tag) no longer disappears from the
		# rescue count just because nobody remembered to write "[rescued]"
		# in the commit subject.
		with tempfile.TemporaryDirectory() as directory:
			out = self.run_status(
				Path(directory),
				done_commits={1: "ticket(001): a", 2: "ticket(002): b"},
				halted_tickets={2},
			)
		self.assertIn("rescued: 1 -- 0 tagged, 1 untagged halts", out)
		self.assertIn("[x] 002-b [rescued: untagged]", out)

	def test_clean_tickets_report_zero_rescued(self):
		with tempfile.TemporaryDirectory() as directory:
			out = self.run_status(
				Path(directory),
				done_commits={1: "ticket(001): a", 2: "ticket(002): b"},
				halted_tickets=set(),
			)
		self.assertIn("rescued: 0 -- 0 tagged, 0 untagged halts", out)

	def test_automatic_retry_with_multiple_real_gate_attempts_is_not_a_false_rescue(self):
		# Regression test for the Codex P2 on PR #29: a ticket that passes
		# purely through build_app.py's bounded automatic retry (e.g. a
		# transient route outage archives a real, failing gate-attempt-*.json
		# before the retry succeeds) must NOT be reported as rescued just
		# because more than one real gate-attempt exists -- only a genuine
		# non-recoverable halt (halt_record_exists()) counts.
		with tempfile.TemporaryDirectory() as directory:
			pilot_dir = Path(directory)
			reports = pilot_dir / "reports" / "ticket-002"
			reports.mkdir(parents=True)
			(reports / "gate-attempt-01.json").write_text("{}")
			(reports / "gate-attempt-02.json").write_text("{}")
			self.assertEqual(ticket_runner.gate_revisit_count(pilot_dir, self.tickets[1]), 2)
			out = self.run_status(
				pilot_dir,
				done_commits={1: "ticket(001): a", 2: "ticket(002): b"},
				halted_tickets=set(),
			)
		self.assertIn("rescued: 0 -- 0 tagged, 0 untagged halts", out)
		self.assertNotIn("[rescued", out)


class StagedPairsTests(unittest.TestCase):
	def test_same_basename_across_slices_does_not_collide(self):
		with tempfile.TemporaryDirectory() as directory:
			pilot_dir = Path(directory)
			workspace = pilot_dir / "workspace"
			workspace.mkdir(parents=True)
			slice_1 = pilot_dir / "spec" / "acceptance" / "001"
			slice_2 = pilot_dir / "spec" / "acceptance" / "002"
			slice_1.mkdir(parents=True)
			slice_2.mkdir(parents=True)
			(slice_1 / "handler_test.go").write_text("package acceptance // 001\n")
			(slice_2 / "handler_test.go").write_text("package acceptance // 002\n")

			pairs = ticket_runner.staged_pairs(pilot_dir, workspace, upto=2)
			staged_paths = sorted(str(staged.relative_to(workspace)) for _, staged in pairs)

			self.assertEqual(
				staged_paths,
				["acceptance/001_handler_test.go", "acceptance/002_handler_test.go"],
			)

	def test_go_mod_and_sum_keep_their_literal_name(self):
		with tempfile.TemporaryDirectory() as directory:
			pilot_dir = Path(directory)
			workspace = pilot_dir / "workspace"
			workspace.mkdir(parents=True)
			slice_1 = pilot_dir / "spec" / "acceptance" / "001"
			slice_1.mkdir(parents=True)
			(slice_1 / "go.mod").write_text("module app/acceptance\n")
			(slice_1 / "go.sum").write_text("")

			pairs = ticket_runner.staged_pairs(pilot_dir, workspace, upto=1)
			staged_names = sorted(staged.name for _, staged in pairs)

			self.assertEqual(staged_names, ["go.mod", "go.sum"])

	def test_stage_removes_a_stale_pre_prefix_staged_file(self):
		with tempfile.TemporaryDirectory() as directory:
			pilot_dir = Path(directory)
			workspace = pilot_dir / "workspace"
			workspace.mkdir(parents=True)
			slice_1 = pilot_dir / "spec" / "acceptance" / "001"
			slice_1.mkdir(parents=True)
			(slice_1 / "handler_test.go").write_text("package acceptance // canon\n")
			(pilot_dir / "spec" / "contract.md").write_text("# contract\n")
			# Simulate a workspace staged by the pre-fix ticket_runner.py, which
			# wrote the bare basename with no ticket-number prefix.
			stale = workspace / "acceptance" / "handler_test.go"
			stale.parent.mkdir(parents=True)
			stale.write_text("package acceptance // stale, pre-prefix copy\n")

			ticket_runner.stage(pilot_dir, workspace, upto=1)

			self.assertFalse(stale.exists())
			self.assertEqual(
				(workspace / "acceptance" / "001_handler_test.go").read_text(),
				"package acceptance // canon\n",
			)

	def test_stage_never_deletes_an_agent_authored_product_test(self):
		# Regression for a real GitHub Codex App review finding on PR #73:
		# stage() used to sweep every existing file of a staged extension
		# in its destination directories, treating the whole directory as
		# runner-owned. A normal product test the agent legitimately wrote
		# alongside the staged oracle -- e.g. a Flutter pilot's own
		# app/test/dashboard_screen_test.dart from an earlier ticket --
		# has no entry in this function's own staged-files manifest and
		# must survive every later stage() call untouched.
		with tempfile.TemporaryDirectory() as directory:
			pilot_dir = Path(directory)
			workspace = pilot_dir / "workspace"
			workspace.mkdir(parents=True)
			slice_1 = pilot_dir / "spec" / "acceptance" / "001"
			slice_1.mkdir(parents=True)
			(slice_1 / "handler_test.dart").write_text("// oracle 001\n")
			(pilot_dir / "spec" / "contract.md").write_text("# contract\n")

			ticket_runner.stage(pilot_dir, workspace, upto=1)

			# The agent, building ticket 001, adds its own product test --
			# not a staged oracle -- directly into the same destination
			# directory stage() writes oracles into.
			product_test = workspace / "app" / "test" / "dashboard_screen_test.dart"
			product_test.write_text("// agent-authored product test\n")

			# A later ticket's stage() call (ticket 001's oracle still
			# active, upto unchanged) must not remove it.
			ticket_runner.stage(pilot_dir, workspace, upto=1)

			self.assertEqual(product_test.read_text(), "// agent-authored product test\n")

	def test_stage_writes_both_colliding_basenames_to_disk(self):
		with tempfile.TemporaryDirectory() as directory:
			pilot_dir = Path(directory)
			workspace = pilot_dir / "workspace"
			workspace.mkdir(parents=True)
			slice_1 = pilot_dir / "spec" / "acceptance" / "001"
			slice_2 = pilot_dir / "spec" / "acceptance" / "002"
			slice_1.mkdir(parents=True)
			slice_2.mkdir(parents=True)
			(slice_1 / "handler_test.go").write_text("package acceptance // 001\n")
			(slice_2 / "handler_test.go").write_text("package acceptance // 002\n")
			(pilot_dir / "spec" / "contract.md").write_text("# contract\n")

			ticket_runner.stage(pilot_dir, workspace, upto=2)

			self.assertEqual(
				(workspace / "acceptance" / "001_handler_test.go").read_text(),
				"package acceptance // 001\n",
			)
			self.assertEqual(
				(workspace / "acceptance" / "002_handler_test.go").read_text(),
				"package acceptance // 002\n",
			)

	def test_python_slices_are_staged_and_ticket_prefixed(self):
		# Regression found via Codex review of PR #8: STAGED_EXTENSIONS
		# only ever listed Go/Dart extensions, so a Python pilot's
		# acceptance suite -- .py slices existed as a real, documented
		# /contract-plan convention well before this test -- was silently
		# never staged into the workspace at all under this script's own
		# gate, and so could neither test the implementation as part of a
		# ticket_runner.py-driven build nor be protected as frozen oracle
		# content the way Go/Dart slices already are.
		with tempfile.TemporaryDirectory() as directory:
			pilot_dir = Path(directory)
			workspace = pilot_dir / "workspace"
			workspace.mkdir(parents=True)
			slice_1 = pilot_dir / "spec" / "acceptance" / "001"
			slice_2 = pilot_dir / "spec" / "acceptance" / "002"
			slice_1.mkdir(parents=True)
			slice_2.mkdir(parents=True)
			(slice_1 / "test_001_create.py").write_text("# 001\n")
			(slice_2 / "test_002_list.py").write_text("# 002\n")

			pairs = ticket_runner.staged_pairs(pilot_dir, workspace, upto=2)
			staged_paths = sorted(str(staged.relative_to(workspace)) for _, staged in pairs)

			self.assertEqual(
				staged_paths,
				["acceptance/001_test_001_create.py", "acceptance/002_test_002_list.py"],
			)

	def test_helpers_py_keeps_its_literal_name_like_go_mod(self):
		# `from helpers import ...` in every later slice depends on the
		# staged file being named exactly `helpers.py` -- the usual
		# `NNN_` prefix would silently break every one of those imports.
		with tempfile.TemporaryDirectory() as directory:
			pilot_dir = Path(directory)
			workspace = pilot_dir / "workspace"
			workspace.mkdir(parents=True)
			slice_1 = pilot_dir / "spec" / "acceptance" / "001"
			slice_1.mkdir(parents=True)
			(slice_1 / "helpers.py").write_text("def api(): ...\n")
			(slice_1 / "test_001.py").write_text("from helpers import api\n")

			pairs = ticket_runner.staged_pairs(pilot_dir, workspace, upto=1)
			staged_paths = sorted(str(staged.relative_to(workspace)) for _, staged in pairs)

			self.assertEqual(
				staged_paths,
				["acceptance/001_test_001.py", "acceptance/helpers.py"],
			)


class RunMakeTests(unittest.TestCase):
	def test_timeout_with_bytes_output_does_not_raise(self):
		# Regression for a real GitHub Codex App review finding on PR #73:
		# subprocess.TimeoutExpired.stdout/.stderr can be bytes even though
		# sh() passes text=True -- a real CPython quirk (the partial output
		# buffered before a timeout bypasses the normal text-decoding step).
		# Concatenating that bytes value with a str via "+" used to raise
		# TypeError here, crashing the runner instead of returning the
		# failed-gate result this function promises on every other failure
		# path.
		with tempfile.TemporaryDirectory() as d:
			workspace = Path(d)
			exc = subprocess.TimeoutExpired(cmd=["make", "verify"], timeout=5, output=b"partial stdout", stderr=b"partial stderr")
			with mock.patch.object(ticket_runner, "sh", side_effect=exc):
				ok, detail = ticket_runner.run_make(workspace, "verify", timeout=5)

		self.assertFalse(ok)
		self.assertIn("timed out after 5s", detail)
		self.assertIn("partial stdout", detail)
		self.assertIn("partial stderr", detail)

	def test_timeout_with_none_output_does_not_raise(self):
		with tempfile.TemporaryDirectory() as d:
			workspace = Path(d)
			exc = subprocess.TimeoutExpired(cmd=["make", "verify"], timeout=5)
			with mock.patch.object(ticket_runner, "sh", side_effect=exc):
				ok, detail = ticket_runner.run_make(workspace, "verify", timeout=5)

		self.assertFalse(ok)
		self.assertIn("timed out after 5s", detail)


class RunShellCommandTests(unittest.TestCase):
	def test_runs_the_command_via_sh_dash_c(self):
		with tempfile.TemporaryDirectory() as d:
			workspace = Path(d)
			(workspace / "marker.txt").write_text("")
			ok, detail = ticket_runner.run_shell_command(
				workspace, "echo hi && test -f marker.txt", timeout=5
			)
		self.assertTrue(ok, detail)
		self.assertIn("hi", detail)

	def test_reports_failure_without_raising(self):
		with tempfile.TemporaryDirectory() as d:
			workspace = Path(d)
			ok, detail = ticket_runner.run_shell_command(workspace, "exit 1", timeout=5)
		self.assertFalse(ok)

	def test_timeout_with_bytes_output_does_not_raise(self):
		# Same real CPython quirk RunMakeTests guards for run_make: a
		# timeout's own captured output can be bytes even under text=True.
		with tempfile.TemporaryDirectory() as d:
			workspace = Path(d)
			exc = subprocess.TimeoutExpired(cmd=["sh", "-c", "sleep 5"], timeout=5, output=b"partial stdout", stderr=b"partial stderr")
			with mock.patch.object(ticket_runner, "sh", side_effect=exc):
				ok, detail = ticket_runner.run_shell_command(workspace, "sleep 5", timeout=5)

		self.assertFalse(ok)
		self.assertIn("timed out after 5s", detail)
		self.assertIn("partial stdout", detail)
		self.assertIn("partial stderr", detail)


class RunTicketVerifyCommandOverrideTests(unittest.TestCase):
	"""run_ticket()'s own gate hardcoded `make verify` + `make verify-full`
	unconditionally, never reading a ticket's declared Verify-Command: or
	goal_pilot.py's --verify-command override at all -- found via
	adversarial review, 2026-09-11, of the fix that added the override one
	layer up (inject_ticketspec_keys/verify_command_for_ticket): that fix
	changed what TEXT a ticket declares, but this gate never read it,
	so a brownfield repo with no verify-full target still failed here
	unconditionally regardless of the override."""

	def _ticket(self, pilot_dir: Path) -> ticket_runner.Ticket:
		tickets_dir = pilot_dir / "spec" / "tickets"
		tickets_dir.mkdir(parents=True)
		ticket_path = tickets_dir / "001-x.md"
		ticket_path.write_text(
			"# Ticket 1: x\n\n## Goal\n\nx\n\n## Required changes\n\nx\n\n"
			"## Verification\n\nx\n\n## Commit\n\nticket(001): x\n"
		)
		return ticket_runner.Ticket(1, "x", ticket_path)

	def test_verify_command_override_runs_once_via_shell_not_make(self):
		with tempfile.TemporaryDirectory() as d:
			pilot_dir = Path(d)
			workspace = pilot_dir / "workspace"
			workspace.mkdir()
			ticket = self._ticket(pilot_dir)
			with mock.patch.object(ticket_runner, "stage"), \
				mock.patch.object(ticket_runner, "prior_boundary_sha", return_value=None), \
				mock.patch.object(ticket_runner, "oracle_drift", return_value=""), \
				mock.patch.object(ticket_runner, "build_report_succeeded", return_value=(True, "Outcome: SUCCEEDED")), \
				mock.patch.object(ticket_runner, "commit_and_state_files_ok", return_value=(True, "ok")), \
				mock.patch.object(ticket_runner, "write_once"), \
				mock.patch.object(ticket_runner, "archive_evidence"), \
				mock.patch.object(ticket_runner, "save_verify_baseline"), \
				mock.patch.object(ticket_runner, "run_make") as make_mock, \
				mock.patch.object(ticket_runner, "run_shell_command", return_value=(True, "ok")) as shell_mock:
				ticket_runner.run_ticket(
					pilot_dir, workspace, ticket, [ticket],
					skip_build=True, gate_attempt=1, build_attempt=None,
					review_policy="advisory", verify_command="cd backend && go test ./...",
				)
			make_mock.assert_not_called()
			shell_mock.assert_called_once()
			self.assertEqual(shell_mock.call_args[0][1], "cd backend && go test ./...")

	def test_no_override_still_runs_make_verify_and_verify_full(self):
		with tempfile.TemporaryDirectory() as d:
			pilot_dir = Path(d)
			workspace = pilot_dir / "workspace"
			workspace.mkdir()
			ticket = self._ticket(pilot_dir)
			with mock.patch.object(ticket_runner, "stage"), \
				mock.patch.object(ticket_runner, "prior_boundary_sha", return_value=None), \
				mock.patch.object(ticket_runner, "oracle_drift", return_value=""), \
				mock.patch.object(ticket_runner, "build_report_succeeded", return_value=(True, "Outcome: SUCCEEDED")), \
				mock.patch.object(ticket_runner, "commit_and_state_files_ok", return_value=(True, "ok")), \
				mock.patch.object(ticket_runner, "write_once"), \
				mock.patch.object(ticket_runner, "archive_evidence"), \
				mock.patch.object(ticket_runner, "save_verify_baseline"), \
				mock.patch.object(ticket_runner, "run_make", return_value=(True, "ok")) as make_mock, \
				mock.patch.object(ticket_runner, "run_shell_command") as shell_mock:
				ticket_runner.run_ticket(
					pilot_dir, workspace, ticket, [ticket],
					skip_build=True, gate_attempt=1, build_attempt=None,
					review_policy="advisory",
				)
			shell_mock.assert_not_called()
			self.assertEqual(
				[c.args[1] for c in make_mock.call_args_list],
				["verify", "verify-full"],
			)


class PriorBoundaryShaTests(unittest.TestCase):
	"""Real git, no mocks: the bug guarded here is git's own traversal into
	an ancestor repo when the workspace has no repo of its own yet."""

	def _init(self, path):
		run = lambda *args: subprocess.run(["git", *args], cwd=path, check=True, capture_output=True)
		run("init")
		run("config", "user.email", "test@test")
		run("config", "user.name", "test")
		run("config", "commit.gpgsign", "false")
		run("config", "core.hooksPath", "/dev/null")
		(path / "seed.txt").write_text("seed\n")
		run("add", "seed.txt")
		run("commit", "-m", "chore: scaffold pilot dir")
		head = subprocess.run(["git", "rev-parse", "HEAD"], cwd=path, text=True, capture_output=True)
		return head.stdout.strip()

	def test_returns_none_before_the_workspace_has_a_repo_of_its_own(self):
		"""Ticket 1 of a fresh pilot. Returning the control repo's root
		commit here hands build_app.py a --review-base-sha that will not
		exist in the workspace repo ensure_git_repo() is about to create."""
		with tempfile.TemporaryDirectory() as directory:
			pilot_dir = Path(directory)
			control_root = self._init(pilot_dir)
			workspace = pilot_dir / "workspace"
			workspace.mkdir()
			ticket = ticket_runner.Ticket(1, "scaffold", Path("001-scaffold.md"))

			sha = ticket_runner.prior_boundary_sha(workspace, [ticket], ticket)

			self.assertIsNone(sha)
			self.assertNotEqual(sha, control_root)

	def test_first_ticket_stays_none_even_after_its_own_commit_exists(self):
		"""Regression for a real bug found live 2026-08-21 (second
		calculator-pilot run, post-067c666): once the workspace has its own
		repo AND ticket 1's commit already exists (the regate path -- a
		second run_ticket() call for the same ticket), the old code fell
		through to `git rev-list --max-parents=0 HEAD`, which in a
		fresh single-commit workspace *is* ticket 1's own commit -- an
		empty self-diff that made commit_and_state_files_ok() falsely
		report state files untouched even when they're in the commit.
		Ticket 1 (index 0 in the ordered list) has no prior state to diff
		against on any call, not just the first."""
		with tempfile.TemporaryDirectory() as directory:
			pilot_dir = Path(directory)
			self._init(pilot_dir)
			workspace = pilot_dir / "workspace"
			workspace.mkdir()
			self._init(workspace)  # workspace now has its own repo AND a commit
			ticket = ticket_runner.Ticket(1, "scaffold", Path("001-scaffold.md"))

			sha = ticket_runner.prior_boundary_sha(workspace, [ticket], ticket)

			self.assertIsNone(sha)

	def test_second_ticket_falls_back_to_root_commit_if_first_ticket_never_committed(self):
		"""Unlike ticket 1, a later ticket with no immediately-prior commit
		still uses the root-commit fallback -- this defensive case (an
		earlier ticket somehow never committed) is unaffected by the
		idx == 0 fix above."""
		with tempfile.TemporaryDirectory() as directory:
			pilot_dir = Path(directory)
			self._init(pilot_dir)
			workspace = pilot_dir / "workspace"
			workspace.mkdir()
			workspace_root = self._init(workspace)
			t1 = ticket_runner.Ticket(1, "scaffold", Path("001-scaffold.md"))
			t2 = ticket_runner.Ticket(2, "second", Path("002-second.md"))

			sha = ticket_runner.prior_boundary_sha(workspace, [t1, t2], t2)

			self.assertEqual(sha, workspace_root)


class AmendCanonTests(unittest.TestCase):
	def _pilot(self, directory):
		pilot_dir = Path(directory)
		workspace = pilot_dir / "workspace"
		workspace.mkdir(parents=True)
		return pilot_dir, workspace

	def test_rejects_a_target_outside_the_workspace(self):
		with tempfile.TemporaryDirectory() as directory:
			pilot_dir, workspace = self._pilot(directory)
			outside = Path(directory) / "elsewhere.sh"
			outside.write_text("echo hi\n")
			rc = ticket_runner.amend_canon(pilot_dir, workspace, [], outside, "because", True)
			self.assertEqual(rc, 1)

	def test_rejects_a_file_that_is_not_a_recognized_frozen_artifact(self):
		with tempfile.TemporaryDirectory() as directory:
			pilot_dir, workspace = self._pilot(directory)
			target = workspace / "backend" / "main.go"
			target.parent.mkdir(parents=True)
			target.write_text("package main\n")
			rc = ticket_runner.amend_canon(pilot_dir, workspace, [], target, "because", True)
			self.assertEqual(rc, 1)

	def test_requires_a_reason(self):
		with tempfile.TemporaryDirectory() as directory:
			pilot_dir, workspace = self._pilot(directory)
			baseline_dir = pilot_dir / "reports" / "ticket-001" / "verify-baseline"
			baseline_dir.mkdir(parents=True)
			(baseline_dir / "scripts__verify-full.sh").write_text("old\n")
			ticket_runner.verify_baseline_json(pilot_dir).write_text(json.dumps({"scripts/verify-full.sh": "x"}))
			target = workspace / "scripts" / "verify-full.sh"
			target.parent.mkdir(parents=True)
			target.write_text("new\n")
			rc = ticket_runner.amend_canon(pilot_dir, workspace, [], target, "", True)
			self.assertEqual(rc, 1)

	def test_amends_a_verify_surface_file_and_updates_the_baseline_hash(self):
		with tempfile.TemporaryDirectory() as directory:
			pilot_dir, workspace = self._pilot(directory)
			baseline_dir = pilot_dir / "reports" / "ticket-001" / "verify-baseline"
			baseline_dir.mkdir(parents=True)
			(baseline_dir / "scripts__verify-full.sh").write_text("old content\n")
			ticket_runner.verify_baseline_json(pilot_dir).write_text(
				json.dumps({"scripts/verify-full.sh": ticket_runner.sha256_of(baseline_dir / "scripts__verify-full.sh")})
			)
			target = workspace / "scripts" / "verify-full.sh"
			target.parent.mkdir(parents=True)
			target.write_text("new content, fixed the restart hang\n")

			rc = ticket_runner.amend_canon(pilot_dir, workspace, [], target, "fixed a real WaitDelay hang", True)

			self.assertEqual(rc, 0)
			self.assertEqual(
				(baseline_dir / "scripts__verify-full.sh").read_text(),
				"new content, fixed the restart hang\n",
			)
			ok, detail = ticket_runner.check_verify_surface_frozen(pilot_dir, workspace)
			self.assertTrue(ok, detail)
			log = (pilot_dir / ticket_runner.AMEND_CANON_LOG).read_text()
			self.assertIn("scripts/verify-full.sh", log)
			self.assertIn("fixed a real WaitDelay hang", log)

	def test_amends_a_staged_acceptance_oracle(self):
		with tempfile.TemporaryDirectory() as directory:
			pilot_dir, workspace = self._pilot(directory)
			canon_dir = pilot_dir / "spec" / "acceptance" / "005"
			canon_dir.mkdir(parents=True)
			canon_file = canon_dir / "widget_test.dart"
			canon_file.write_text("testWidgets('renders $X.YY', (t) async {})\n")
			staged = workspace / "app" / "test" / "005_widget_test.dart"
			staged.parent.mkdir(parents=True)
			staged.write_text("testWidgets(r'renders $X.YY', (t) async {})\n")
			tickets = [ticket_runner.Ticket(5, "dashboard", Path("005-dashboard.md"))]

			rc = ticket_runner.amend_canon(pilot_dir, workspace, tickets, staged, "canon had a real Dart interpolation bug", True)

			self.assertEqual(rc, 0)
			self.assertEqual(canon_file.read_text(), staged.read_text())
			drift = ticket_runner.oracle_drift(pilot_dir, workspace, 5)
			self.assertEqual(drift, [])

	def test_declining_the_confirmation_leaves_canon_unchanged(self):
		with tempfile.TemporaryDirectory() as directory:
			pilot_dir, workspace = self._pilot(directory)
			baseline_dir = pilot_dir / "reports" / "ticket-001" / "verify-baseline"
			baseline_dir.mkdir(parents=True)
			(baseline_dir / "scripts__verify-full.sh").write_text("old\n")
			ticket_runner.verify_baseline_json(pilot_dir).write_text(json.dumps({"scripts/verify-full.sh": "x"}))
			target = workspace / "scripts" / "verify-full.sh"
			target.parent.mkdir(parents=True)
			target.write_text("new\n")
			with mock.patch("builtins.input", return_value="n"):
				rc = ticket_runner.amend_canon(pilot_dir, workspace, [], target, "because", False)
			self.assertEqual(rc, 1)
			self.assertEqual((baseline_dir / "scripts__verify-full.sh").read_text(), "old\n")

	def test_noop_when_workspace_already_matches_canon(self):
		with tempfile.TemporaryDirectory() as directory:
			pilot_dir, workspace = self._pilot(directory)
			baseline_dir = pilot_dir / "reports" / "ticket-001" / "verify-baseline"
			baseline_dir.mkdir(parents=True)
			(baseline_dir / "scripts__verify-full.sh").write_text("same\n")
			ticket_runner.verify_baseline_json(pilot_dir).write_text(json.dumps({"scripts/verify-full.sh": "x"}))
			target = workspace / "scripts" / "verify-full.sh"
			target.parent.mkdir(parents=True)
			target.write_text("same\n")
			rc = ticket_runner.amend_canon(pilot_dir, workspace, [], target, "because", True)
			self.assertEqual(rc, 0)
			log_path = pilot_dir / ticket_runner.AMEND_CANON_LOG
			self.assertFalse(log_path.exists())


if __name__ == "__main__":
	unittest.main()
