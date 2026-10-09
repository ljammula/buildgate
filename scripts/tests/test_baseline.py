"""Offline tests for scripts/baseline.py: which runs are counted, what a
one-shot acceptance is, when two failed rounds are the same failure, and
what is reported as stopping a run. No factoryd, no Docker, no model."""

import contextlib
import io
import json
import pathlib
import sys
import tempfile
import unittest

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parents[1]))
import baseline  # noqa: E402


def rnd(index, verify_passed=True, **extra):
	return {
		"index": index, "agent": "pi", "agent_returncode": 0, "agent_timed_out": False,
		"reviewer_outcome": "unavailable", "reviewer_detail": "", "verify_passed": verify_passed,
		"verify_timed_out": False, "fast_check_ran": False, "fast_check_passed": None, **extra,
	}


def run(run_id, state="accepted", rounds=None, ticket=None, **extra):
	record = {
		"id": run_id, "ticket": ticket or run_id, "project": "app", "state": state,
		"created_at": "2026-10-01T00:00:00Z", "updated_at": "2026-10-01T00:10:00Z",
		"gate_results": [], "overrides": None, "rescues": None,
	}
	if rounds is not None:
		record["agent_evidence"] = {"review_policy": "advisory", "rounds": rounds}
	record.update(extra)
	return record


def report(outputs, facts=None, traces=None):
	"""A BUILD_REPORT.md in build_app.py's layout, with outputs[index] as
	each failed round's kept output, facts[index] as extra "- " lines and
	traces[index] as its extension traces, which follow the duration."""
	lines = ["# Zero-human build report", "", "## Rounds", ""]
	for index, output in outputs.items():
		lines += [f"### Round {index}", "- agent: pi"] + (facts or {}).get(index, []) + [
			"- reviewer outcome: unavailable (no-review-verdict)", "",
			"Reviewer comments:", "```", "no-review-verdict", "```",
			"- verify command: `make test`", "- verify passed: False", "- duration: 3.0s",
		]
		if (traces or {}).get(index):
			lines.append("- extension traces: " + traces[index])
		if output is not None:
			lines += ["", "```", output, "```"]
		lines.append("")
	lines += ["## Independent review verdicts", "", "none", ""]
	return "\n".join(lines)


def progress(ends):
	"""progress.jsonl round-end lines: ends[index] is "" for a pass, else
	the failed round's detail."""
	return "\n".join(
		json.dumps({"source": "worker", "stage": "round", "event": "end", "round": i, "outcome": "fail" if d else "pass", "detail": d})
		for i, d in ends.items()
	)


def pairs_of(record, report_text="", progress_text=""):
	return baseline.measure([record], beside={record["id"]: {"report": report_text, "progress": progress_text}})["failed_round_pairs"]


class LoadRuns(unittest.TestCase):
	def write(self, root, data, record, with_report=False):
		d = pathlib.Path(root, data, "runs", record["id"])
		d.mkdir(parents=True)
		(d / "run.json").write_text(json.dumps(record))
		if with_report:
			(d / "BUILD_REPORT.md").write_text("report")
		return d / "run.json"

	def test_a_run_copied_into_two_data_dirs_is_counted_once_and_the_copy_updated_last_wins(self):
		with tempfile.TemporaryDirectory() as root:
			# The newest copy is not the first found, and its timestamp is
			# the later moment only once the offsets are read.
			self.write(root, "a/data", run("r1", state="slice_running", updated_at="2026-10-07T04:00:00Z"))
			self.write(root, "b/deep/data", run("r1", state="accepted", updated_at="2026-10-06T23:30:00-05:00"))
			bad = pathlib.Path(root, "c", "runs", "broken")
			bad.mkdir(parents=True)
			(bad / "run.json").write_text("{not json")
			deep = pathlib.Path(root, "d", "runs", "nested")
			deep.mkdir(parents=True)
			(deep / "run.json").write_text("[" * 100000)
			files = baseline.find_run_files([root])
			runs, unreadable = baseline.load_runs(files)
		self.assertEqual(len(files), 4)
		self.assertEqual([(r["id"], r["state"]) for r in runs], [("r1", "accepted")])
		self.assertEqual(unreadable, [str(bad / "run.json"), str(deep / "run.json")])

	def test_a_copy_with_no_readable_update_time_loses(self):
		with tempfile.TemporaryDirectory() as root:
			self.write(root, "a", run("r1", state="accepted"))
			self.write(root, "b", run("r1", state="slice_running", updated_at=None), with_report=True)
			self.write(root, "c", run("r1", state="slice_running", updated_at="soon"))
			runs, _ = baseline.load_runs(baseline.find_run_files([root]))
		self.assertEqual(runs[0]["state"], "accepted")

	def test_of_two_copies_updated_together_the_one_with_a_report_is_kept(self):
		with tempfile.TemporaryDirectory() as root:
			self.write(root, "a", run("r1"))
			kept = self.write(root, "b", run("r1"), with_report=True)
			self.write(root, "c", run("r1"))
			runs, _ = baseline.load_runs(baseline.find_run_files([root]))
		self.assertEqual(runs[0]["_path"], str(kept))

	def test_a_data_dir_given_directly_is_searched(self):
		with tempfile.TemporaryDirectory() as root:
			self.write(root, ".", run("r1"))
			self.assertEqual(len(baseline.find_run_files([root])), 1)

	def test_timestamps_order_by_the_moment_they_name(self):
		order = sorted(["2026-11-01T01:50:00-05:00", "2026-11-01T01:10:00-06:00", "2026-11-01T06:49:59.123456789Z", "not a time", ""], key=baseline.instant)
		self.assertEqual(order, ["2026-11-01T06:49:59.123456789Z", "2026-11-01T01:50:00-05:00", "2026-11-01T01:10:00-06:00", "", "not a time"])


class OneShot(unittest.TestCase):
	def test_only_a_tickets_first_run_counts_and_a_rescue_or_override_is_not_one_shot(self):
		runs = [
			# Created first, though its id sorts last.
			run("z-a-1", state="quarantined", ticket="a", created_at="2026-10-01T00:00:00Z"),
			run("a-2", state="accepted", ticket="a", created_at="2026-10-01T01:00:00Z"),
			run("b-1", state="accepted", ticket="b"),
			run("c-1", state="accepted", ticket="c", rescues=[{"by": "operator"}]),
			run("d-1", state="accepted", ticket="d", overrides=[{"check": "tests_added"}]),
			# The first run is unfinished: the ticket is not in the rate,
			# whatever its later run did.
			run("e-1", state="slice_running", ticket="e", created_at="2026-10-01T00:00:00Z"),
			run("e-2", state="accepted", ticket="e", created_at="2026-10-01T02:00:00Z"),
		]
		m = baseline.measure(runs, beside={})
		self.assertEqual((m["one_shot"], m["tickets"]), (1, 4))
		self.assertEqual(m["one_shot_rate"], 0.25)
		self.assertEqual((m["runs"], m["finished"], m["unfinished"]), (7, 6, 1))

	def test_the_first_run_is_the_earlier_moment_across_utc_offsets(self):
		runs = [
			run("a-1", state="quarantined", ticket="a", created_at="2026-11-01T01:10:00-06:00"),
			run("a-2", state="accepted", ticket="a", created_at="2026-11-01T01:50:00-05:00"),
		]
		self.assertEqual(baseline.measure(runs, beside={})["one_shot"], 1)

	def test_review_and_conformity_rounds_belong_to_the_ticket_they_follow(self):
		runs = [
			run("t-1", state="accepted", ticket="req-001", created_at="2026-10-01T00:00:00Z"),
			run("t-2", state="quarantined", ticket="req-001-review1", created_at="2026-10-01T01:00:00Z"),
			run("t-3", state="quarantined", ticket="req-001-review2-fix1", created_at="2026-10-01T02:00:00Z"),
			run("t-4", state="quarantined", ticket="req-001-conformity1", created_at="2026-10-01T03:00:00Z"),
			run("t-5", state="accepted", ticket="req-001-corrective1", created_at="2026-10-01T04:00:00Z"),
			run("u-1", state="quarantined", ticket="a-review-of-reviews-001"),
			# Names that only look like a follow-up stay two tickets, and
			# the same name in another project is another ticket.
			run("v-1", state="accepted", ticket="api-review1"),
			run("v-2", state="quarantined", ticket="api-review2"),
			run("w-1", state="quarantined", ticket="req-001", project="other"),
		]
		m = baseline.measure(runs, beside={})
		self.assertEqual((m["one_shot"], m["tickets"]), (2, 5))


class RoundsToGreen(unittest.TestCase):
	def test_counts_accepted_runs_with_recorded_rounds_only(self):
		runs = [
			run("one", rounds=[rnd(1)]),
			run("three", rounds=[rnd(1, False), rnd(2, False), rnd(3)]),
			run("no-evidence"),
			run("quarantined", state="quarantined", rounds=[rnd(1, False)]),
		]
		green = baseline.measure(runs, beside={})["rounds_to_green"]
		self.assertEqual((green["runs"], green["mean"], green["max"], green["first_round"]), (2, 2, 3, 1))
		self.assertEqual(green["histogram"], {"1": 1, "3": 1})


class RebuiltBlockers(unittest.TestCase):
	def test_the_build_loops_own_names_from_the_outcome_fields(self):
		cases = [
			(rnd(1), "advisory", []),
			(rnd(1, False), "advisory", ["canonical verification failed"]),
			(rnd(1, None), "advisory", ["canonical verification failed"]),
			(rnd(1, agent_timed_out=True), "advisory", ["pi invocation timed out"]),
			(rnd(1, False, agent_returncode=2), "advisory", ["pi invocation failed", "canonical verification failed"]),
			(rnd(1, None, fast_check_ran=True, fast_check_passed=False), "advisory", ["fast check failed"]),
			(rnd(1, reviewer_outcome="flagged"), "degraded", ["reviewer flagged the current diff"]),
			(rnd(1, reviewer_outcome="flagged"), "advisory", []),
			(rnd(1, reviewer_outcome="unavailable", reviewer_detail="no-review-verdict"), "required", ["review unavailable (no-review-verdict)"]),
			(rnd(1, reviewer_outcome="clean"), "required", []),
		]
		for record, policy, want in cases:
			self.assertEqual(baseline.rebuilt_blockers(record, policy, None, {}), want, (record, policy))

	def test_the_round_end_line_and_the_report_add_what_the_fields_cannot_show(self):
		passed_fields = rnd(1)
		self.assertEqual(baseline.rebuilt_blockers(passed_fields, "advisory", (True, "no changes made"), {}), [baseline.NO_CHANGES])
		self.assertEqual(baseline.rebuilt_blockers(passed_fields, "advisory", (True, "reference oracle failed"), {}), ["reference oracle failed"])
		self.assertEqual(baseline.rebuilt_blockers(passed_fields, "advisory", (True, ""), {}), ["round failed"])
		# The loop said the round passed: its fields are not second-guessed.
		self.assertEqual(baseline.rebuilt_blockers(rnd(1, False), "advisory", (False, ""), {}), [])
		kept = {"route_errors": (4, 4), "stalled": True}
		self.assertEqual(
			baseline.rebuilt_blockers(rnd(1, False), "advisory", (True, "verify failed: make test"), kept),
			["model route unreachable (4/4 assistant turns errored)", "stall-timeout", "canonical verification failed"],
		)

	def test_round_ends_keeps_the_last_line_for_a_round(self):
		text = progress({1: "verify failed: make test", 2: "no changes made"}) + "\nnot json \"round\"\n" + progress({2: ""})
		self.assertEqual(baseline.round_ends(text), {1: (True, "verify failed: make test"), 2: (False, "")})


class SameFailure(unittest.TestCase):
	def test_recorded_signatures_are_compared_without_a_report(self):
		rounds = [
			rnd(1, False, blockers=["canonical verification failed"], failure_signature="aaaa"),
			rnd(2, False, blockers=["canonical verification failed"], failure_signature="aaaa"),
			rnd(3, False, blockers=["canonical verification failed"], failure_signature="bbbb"),
			rnd(4, True, blockers=[], failure_signature=""),
			# Recorded blockers decide, not the outcome fields.
			rnd(5, True, blockers=[baseline.NO_CHANGES], failure_signature="cccc"),
			rnd(6, True, blockers=[baseline.NO_CHANGES], failure_signature="cccc"),
		]
		pairs = pairs_of(run("r", state="quarantined", rounds=rounds))
		self.assertEqual((pairs["pairs"], pairs["not_comparable"], pairs["second_changed_nothing"]), (3, 0, 1))
		self.assertEqual(pairs["recorded"], {"same": 2, "different": 1, "same_rate": 0.6667})
		self.assertEqual(pairs["derived"], {"same": 0, "different": 0, "same_rate": None})

	def test_an_empty_recorded_blocker_list_does_not_pass_a_round_whose_fields_failed(self):
		rounds = [
			rnd(1, False, blockers=["canonical verification failed"], failure_signature="aaaa"),
			rnd(2, False, blockers=[], agent_timed_out=True),
			rnd(3, True, blockers=[]),
		]
		record = run("r", state="quarantined", rounds=rounds)
		signatures = baseline.round_signatures(record, "", "")
		self.assertEqual([s[1] for s in signatures], [True, True, False])
		pairs = pairs_of(record)
		self.assertEqual((pairs["pairs"], pairs["not_comparable"]), (1, 1))

	def test_a_signature_is_derived_from_the_report_and_ignores_durations(self):
		rounds = [rnd(1, False), rnd(2, False), rnd(3, False)]
		text = report({
			1: "--- FAIL: TestSum (0.03s)\n    sum_test.go:9: got 3, want 9\nFAIL",
			2: "--- FAIL: TestSum (0.41s)\n    sum_test.go:9: got 3, want 9\nFAIL",
			3: "--- FAIL: TestSum (0.02s)\n    sum_test.go:9: got 8, want 9\nFAIL",
		})
		pairs = pairs_of(run("r", state="quarantined", rounds=rounds), text)
		self.assertEqual(pairs["derived"], {"same": 1, "different": 1, "same_rate": 0.5})
		self.assertEqual(pairs["recorded"]["same"] + pairs["recorded"]["different"], 0)

	def test_a_derived_signature_is_the_one_the_build_loop_computes(self):
		output = "--- FAIL: TestSum (0.03s)\n    sum_test.go:9: got 3, want 9\nFAIL"
		record = run("r", state="quarantined", rounds=[rnd(1, False)])
		got = baseline.round_signatures(record, report({1: output}), "")[0]
		want = baseline.round_feedback.failure_signature(["canonical verification failed"], output, "")
		self.assertEqual(got, (1, True, want, baseline.SIGNATURE_DERIVED, False))

	def test_a_round_that_changed_nothing_is_a_different_failure_and_is_counted(self):
		rounds = [rnd(1, False), rnd(2, False), rnd(3, False)]
		text = report({1: "FAIL x", 2: "FAIL x", 3: "FAIL x"})
		ends = progress({1: "verify failed: make test", 2: "no changes made", 3: "no changes made"})
		pairs = pairs_of(run("r", state="quarantined", rounds=rounds), text, ends)
		self.assertEqual(pairs["derived"], {"same": 1, "different": 1, "same_rate": 0.5})
		self.assertEqual(pairs["second_changed_nothing"], 2)

	def test_the_reviewers_findings_are_part_of_a_derived_signature(self):
		rounds = [
			rnd(1, False, reviewer_outcome="flagged", reviewer_detail="bug A"),
			rnd(2, False, reviewer_outcome="flagged", reviewer_detail="bug B"),
		]
		pairs = pairs_of(run("r", state="quarantined", rounds=rounds), report({1: "FAIL x", 2: "FAIL x"}))
		self.assertEqual(pairs["derived"], {"same": 0, "different": 1, "same_rate": 0.0})

	def test_a_failed_round_with_no_output_is_compared_by_its_blockers_and_findings(self):
		# The report has the round, with nothing after its duration: a
		# review that flagged the diff while verification passed.
		def flagged(index, detail):
			return rnd(index, reviewer_outcome="flagged", reviewer_detail=detail)
		record = run("r", state="quarantined", rounds=[flagged(1, "bug A"), flagged(2, "bug A"), flagged(3, "bug B")])
		record["agent_evidence"]["review_policy"] = "required"
		pairs = pairs_of(record, report({1: None, 2: None, 3: None}))
		self.assertEqual((pairs["pairs"], pairs["not_comparable"]), (2, 0))
		self.assertEqual(pairs["derived"], {"same": 1, "different": 1, "same_rate": 0.5})

	def test_a_pair_with_no_signature_or_of_two_sources_is_not_comparable_and_a_passed_round_breaks_a_pair(self):
		quarantined = run("q", state="quarantined", rounds=[rnd(1, False), rnd(2, False)])
		mixed = run("m", state="quarantined", rounds=[rnd(1, False), rnd(2, False, blockers=["canonical verification failed"], failure_signature="aaaa")])
		recovered = run("ok", rounds=[rnd(1, False), rnd(2), rnd(3, False), rnd(4)])
		m = baseline.measure([quarantined, mixed, recovered], beside={
			"q": {"report": report({1: "FAIL x"})},
			"m": {"report": report({1: "FAIL x"})},
			"ok": {},
		})
		pairs = m["failed_round_pairs"]
		self.assertEqual((pairs["pairs"], pairs["not_comparable"]), (2, 2))
		self.assertIsNone(pairs["derived"]["same_rate"])
		self.assertEqual(m["multi_round_runs"], 3)

	def test_the_report_and_progress_beside_the_run_record_are_read(self):
		with tempfile.TemporaryDirectory() as root:
			d = pathlib.Path(root, "runs", "r")
			d.mkdir(parents=True)
			(d / "run.json").write_text(json.dumps(run("r", state="quarantined", rounds=[rnd(1, False), rnd(2, False)])))
			(d / "BUILD_REPORT.md").write_text(report({1: "error: boom", 2: "error: boom"}))
			(d / "progress.jsonl").write_text(progress({1: "verify failed: x", 2: "no changes made"}))
			runs, _ = baseline.load_runs(baseline.find_run_files([root]))
			pairs = baseline.measure(runs)["failed_round_pairs"]
		self.assertEqual((pairs["pairs"], pairs["derived"]["different"], pairs["second_changed_nothing"]), (1, 1, 1))


class ReportRounds(unittest.TestCase):
	def test_output_that_looks_like_the_reports_own_lines_is_kept_whole(self):
		tricky = "before\n```\ninner\n```\n### Round 2\n\n## Independent review verdicts\n\n- duration: 9.9s\nafter"
		got = baseline.report_rounds(report({1: tricky, 2: "second"}))
		self.assertEqual({i: r["output"] for i, r in got.items()}, {1: tricky, 2: "second"})

	def test_route_errors_and_a_stall_are_read_from_the_rounds_own_lines(self):
		facts = {1: ["- agent turns errored: 3/3 (model route unreachable)"]}
		text = report({1: "- agent turns errored: 9/9 (model route unreachable)", 2: None, 3: "- extension traces: x=stall-timeout"}, facts, traces={1: "guard:turn_end=stall-timeout", 2: "guard:turn_end=stall-timeout"})
		got = baseline.report_rounds(text)
		self.assertEqual(got[1], {"output": "- agent turns errored: 9/9 (model route unreachable)", "route_errors": (3, 3), "stalled": True})
		self.assertEqual(got[2], {"output": "", "route_errors": None, "stalled": True})
		self.assertEqual(got[3], {"output": "- extension traces: x=stall-timeout", "route_errors": None, "stalled": False})


class WhatStopsRuns(unittest.TestCase):
	def test_failed_checks_and_halt_reasons(self):
		runs = [
			run("q1", state="quarantined", gate_results=[
				{"check": "canonical_verify", "passed": True}, {"check": "code_review", "passed": False},
				{"check": "tests_added", "passed": False}, {"check": "code_review", "passed": False},
			]),
			run("q2", state="quarantined", gate_results=[{"check": "code_review", "passed": False}]),
			run("q3", state="quarantined", halt_reason_code="verify_failed"),
			run("q4", state="quarantined", gate_results=5),
			run("h1", state="halted", halt_reason_code="relay_ceiling_exceeded", triage="halted: spend ceiling"),
			run("h2", state="halted", triage="halted: sandbox attempts exhausted\nmore"),
			run("h3", state="halted"),
			run("a1", gate_results=[{"check": "lint", "passed": False}]),
		]
		m = baseline.measure(runs, beside={})
		self.assertEqual(m["quarantine_checks"], {"code_review": 2, "tests_added": 1, "(verify_failed)": 1, baseline.NO_FAILED_CHECK: 1})
		self.assertEqual(m["halt_reasons"], {"relay_ceiling_exceeded": 1, "sandbox attempts exhausted": 1, baseline.NO_HALT_REASON: 1})


class Rendering(unittest.TestCase):
	def test_per_project_rows_and_empty_data(self):
		runs = [run("a", project="one"), run("b", state="quarantined", project="two", gate_results=[{"check": "lint", "passed": False}])]
		result = baseline.baseline(runs, beside={})
		self.assertEqual(sorted(result["projects"]), ["one", "two"])
		text = baseline.render(result, 2, [])
		self.assertIn("| One-shot acceptance (first run of a ticket accepted, no override or rescue) | 1 of 2 (50%) |", text)
		self.assertIn("| lint | 1 |", text)
		self.assertIn("| two | 1 | 0 | 1 | 0 | 0 of 1 (0%) | - | none to count | none to count |", text)
		empty = baseline.render(baseline.baseline([], beside={}), 0, [])
		self.assertIn("none to count", empty)

	def run_main(self, argv):
		out, err = io.StringIO(), io.StringIO()
		with contextlib.redirect_stdout(out), contextlib.redirect_stderr(err):
			code = baseline.main(argv)
		return code, out.getvalue(), err.getvalue()

	def test_main_prints_json_for_the_projects_asked_for(self):
		with tempfile.TemporaryDirectory() as root:
			for run_id, project in (("a", "one"), ("b", "two"), ("c", "three")):
				d = pathlib.Path(root, "data", "runs", run_id)
				d.mkdir(parents=True)
				(d / "run.json").write_text(json.dumps(run(run_id, project=project)))
			code, out, _ = self.run_main(["--json", "--project", "two", root])
			got = json.loads(out)
			self.assertEqual((code, got["files"], got["overall"]["runs"], list(got["projects"])), (0, 3, 1, ["two"]))
			_, out, _ = self.run_main(["--json", "--exclude-project", "two", root])
			self.assertEqual(list(json.loads(out)["projects"]), ["one", "three"])

	def test_main_says_when_a_directory_is_missing_or_holds_no_run(self):
		with tempfile.TemporaryDirectory() as root:
			code, _, err = self.run_main([root, root + "/missing"])
			self.assertEqual(code, 0)
			self.assertIn("missing is not a directory", err)
			self.assertIn("no run record found", err)
			code, out, err = self.run_main([root + "/missing"])
			self.assertEqual((code, out), (2, ""))


if __name__ == "__main__":
	unittest.main()
