from __future__ import annotations

import contextlib
import importlib.util
import io
import json
import shutil
import subprocess
import sys
import tempfile
import time
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

DRAFT_ORACLES_SCRIPT = Path(__file__).resolve().parents[1] / "scripts" / "draft_acceptance_oracles.py"
DRAFT_ORACLES_SPEC = importlib.util.spec_from_file_location("draft_acceptance_oracles", DRAFT_ORACLES_SCRIPT)
assert DRAFT_ORACLES_SPEC and DRAFT_ORACLES_SPEC.loader
draft_acceptance_oracles = importlib.util.module_from_spec(DRAFT_ORACLES_SPEC)
sys.modules[DRAFT_ORACLES_SPEC.name] = draft_acceptance_oracles
DRAFT_ORACLES_SPEC.loader.exec_module(draft_acceptance_oracles)


def init_repo(path: Path) -> None:
	run = lambda *args: subprocess.run(["git", *args], cwd=path, check=True, capture_output=True)
	run("init")
	run("config", "user.email", "test@test")
	run("config", "user.name", "test")
	run("config", "commit.gpgsign", "false")
	(path / "seed.txt").write_text("seed\n")
	run("add", "seed.txt")
	run("commit", "-m", "init")


def scripted_pi(exit_code: int, stdout: str, *, writes: dict[str, str] | None, draft_dir: Path, stderr: str = "", timed_out: bool = False):
	"""run_pi_bounded() side_effect: scripts the actual pi invocation (git
	calls in the script still run for real). `writes` is
	{relative_filename: content}, written to draft_dir just before returning,
	e.g. {"MANIFEST.json": "...", "oracle_001.py": "..."}."""

	def side_effect(command, cwd=None, timeout=None, adapter=None):
		if writes is not None:
			draft_dir.mkdir(parents=True, exist_ok=True)
			for name, content in writes.items():
				(draft_dir / name).write_text(content)
		return draft_acceptance_oracles.PiRun.from_text(exit_code, stdout, stderr, timed_out)

	return side_effect


def scripted_pi_sequence(specs, draft_dir_base: Path, commands: list | None = None):
	"""run_pi_bounded() side_effect for run_draft_oracles' one-pi-call-per-
	criterion design: specs is a list of (exit_code, stdout, writes_or_None,
	stderr, timed_out) tuples, one per criterion IN ORDER (draft_one_criterion
	calls run_pi_bounded exactly once per criterion, strictly sequentially).
	Each call writes into its own draft_dir_base/c{NNN}/, matching
	draft_one_criterion's own per-criterion scratch directory naming."""
	calls = {"n": 0}

	def side_effect(command, cwd=None, timeout=None, adapter=None):
		calls["n"] += 1
		index = calls["n"]
		if commands is not None:
			commands.append(command)
		exit_code, stdout, writes, stderr, timed_out = specs[index - 1]
		if writes is not None:
			target = draft_dir_base / f"c{index:03d}"
			target.mkdir(parents=True, exist_ok=True)
			for name, content in writes.items():
				(target / name).write_text(content)
		return draft_acceptance_oracles.PiRun.from_text(exit_code, stdout, stderr, timed_out)

	return side_effect


class ValidateManifestTests(unittest.TestCase):
	def test_accepts_a_well_formed_manifest_with_a_mix_of_null_and_real_oracles(self):
		with tempfile.TemporaryDirectory() as directory:
			draft_dir = Path(directory)
			(draft_dir / "oracle_001.py").write_text("assert 1 == 1\n")
			manifest = [
				{"criterion": "1. Returns 200.", "oracle_file": "oracle_001.py", "rationale": "derivable value"},
				{"criterion": "2. Code is clean.", "oracle_file": None, "rationale": "judgment call"},
			]
			validated = draft_acceptance_oracles.validate_manifest(
				manifest, ["1. Returns 200.", "2. Code is clean."], draft_dir,
			)
			self.assertEqual(validated, manifest)

	def test_tolerates_criterion_echoed_without_its_leading_number(self):
		with tempfile.TemporaryDirectory() as directory:
			draft_dir = Path(directory)
			manifest = [{"criterion": "Returns 200.", "oracle_file": None, "rationale": "judgment call"}]
			# Must not raise.
			draft_acceptance_oracles.validate_manifest(manifest, ["1. Returns 200."], draft_dir)

	def test_rejects_wrong_entry_count(self):
		with tempfile.TemporaryDirectory() as directory:
			draft_dir = Path(directory)
			manifest = [{"criterion": "1. Returns 200.", "oracle_file": None, "rationale": "x"}]
			with self.assertRaises(ValueError):
				draft_acceptance_oracles.validate_manifest(manifest, ["1. Returns 200.", "2. Also this."], draft_dir)

	def test_rejects_a_mismatched_criterion_text(self):
		with tempfile.TemporaryDirectory() as directory:
			draft_dir = Path(directory)
			manifest = [{"criterion": "1. Something else entirely.", "oracle_file": None, "rationale": "x"}]
			with self.assertRaises(ValueError):
				draft_acceptance_oracles.validate_manifest(manifest, ["1. Returns 200."], draft_dir)

	def test_rejects_an_oracle_file_that_was_never_actually_written(self):
		with tempfile.TemporaryDirectory() as directory:
			draft_dir = Path(directory)
			manifest = [{"criterion": "1. Returns 200.", "oracle_file": "does_not_exist.py", "rationale": "x"}]
			with self.assertRaises(ValueError):
				draft_acceptance_oracles.validate_manifest(manifest, ["1. Returns 200."], draft_dir)

	def test_rejects_a_path_traversal_oracle_file(self):
		# Regression for a real finding (found via review): draft_dir /
		# oracle_file with no containment check lets a crafted or
		# hallucinated oracle_file escape draft_dir entirely -- Path's own
		# "/" operator DISCARDS the left side when the right is absolute,
		# and a relative "../" walks straight out. A file genuinely
		# existing at the traversal target (here, draft_dir's own parent)
		# would previously have passed validation.
		with tempfile.TemporaryDirectory() as directory:
			draft_dir = Path(directory) / "draft"
			draft_dir.mkdir()
			escape_target = Path(directory) / "escaped.txt"
			escape_target.write_text("should never be reachable")
			for hostile in ("../escaped.txt", str(escape_target), "..", "sub/oracle.py"):
				manifest = [{"criterion": "1. Returns 200.", "oracle_file": hostile, "rationale": "x"}]
				with self.assertRaises(ValueError, msg=f"oracle_file={hostile!r} should have been rejected"):
					draft_acceptance_oracles.validate_manifest(manifest, ["1. Returns 200."], draft_dir)

	def test_rejects_a_non_array_manifest(self):
		with tempfile.TemporaryDirectory() as directory:
			draft_dir = Path(directory)
			with self.assertRaises(ValueError):
				draft_acceptance_oracles.validate_manifest({"not": "an array"}, ["1. X."], draft_dir)


class BoundedPiRunTests(unittest.TestCase):
	# A child that streams ~200 MB of thinking_delta events (10 KB each), one
	# line over the per-line cap, and ~20 MB on stderr, then ends with an
	# agent_end carrying usage.
	STREAM = (
		"import sys, json\n"
		"delta = json.dumps({'type': 'message_update', 'assistantMessageEvent': {'type': 'thinking_delta', 'delta': 'x' * 10000}}) + '\\n'\n"
		"out = sys.stdout\n"
		"for _ in range(20000):\n"
		"    out.write(delta)\n"
		"out.write('y' * (%d + 5) + '\\n')\n"
		"out.write(json.dumps({'type': 'agent_end', 'messages': [{'role': 'assistant', 'usage': {'input': 7, 'output': 3}}]}) + '\\n')\n"
		"out.flush()\n"
		"for _ in range(2000):\n"
		"    sys.stderr.write('e' * 10000 + '\\n')\n"
	)

	def test_a_huge_stream_is_digested_in_bounded_memory(self):
		import tracemalloc
		with tempfile.TemporaryDirectory() as directory:
			tracemalloc.start()
			try:
				run = draft_acceptance_oracles.run_pi_bounded(
					[sys.executable, "-c", self.STREAM % draft_acceptance_oracles.MAX_PI_LINE_BYTES], Path(directory), timeout=300,
				)
				_, peak = tracemalloc.get_traced_memory()
			finally:
				tracemalloc.stop()
		self.assertEqual(run.returncode, 0)
		self.assertLess(peak, 40 * 1024 * 1024)
		self.assertEqual(run.digest.events, 20001)
		self.assertEqual(run.digest.oversize_lines, 1)
		self.assertEqual(run.digest.usage(), {"input": 7, "output": 3})
		self.assertLessEqual(len(run.stderr_tail), draft_acceptance_oracles.STDERR_TAIL_BYTES)
		self.assertLess(len(run.digest.summary()), 4000)

	def test_non_json_lines_are_kept_only_as_a_short_tail(self):
		digest = draft_acceptance_oracles.AgentDigest()
		for i in range(100000):
			digest.feed(f"noise {i} " + "n" * 1000)
		self.assertLessEqual(len(digest.plain), draft_acceptance_oracles.DIGEST_PLAIN_LINES)
		self.assertTrue(all(len(p) <= draft_acceptance_oracles.DIGEST_PLAIN_LINE_CHARS for p in digest.plain))
		self.assertIn("noise 99999", digest.summary())

	def test_timeout_kills_the_whole_process_group_including_grandchildren(self):
		import os
		with tempfile.TemporaryDirectory() as directory:
			pid_file = Path(directory) / "grandchild.pid"
			code = (
				"import subprocess, sys, time\n"
				f"g = subprocess.Popen([sys.executable, '-c', 'import time; time.sleep(120)'])\n"
				f"open({str(pid_file)!r}, 'w').write(str(g.pid))\n"
				"time.sleep(120)\n"
			)
			run = draft_acceptance_oracles.run_pi_bounded([sys.executable, "-c", code], Path(directory), timeout=2)
			self.assertTrue(run.timed_out)
			grandchild = int(pid_file.read_text())
			deadline = time.time() + 10
			alive = True
			while alive and time.time() < deadline:
				try:
					os.kill(grandchild, 0)
					time.sleep(0.1)
				except ProcessLookupError:
					alive = False
			if alive:
				os.kill(grandchild, 9)
			self.assertFalse(alive, "the grandchild outlived the drafter's timeout")

	def test_timeout_kills_the_child_and_marks_the_run(self):
		with tempfile.TemporaryDirectory() as directory:
			run = draft_acceptance_oracles.run_pi_bounded(
				[sys.executable, "-c", "import time; print('hi', flush=True); time.sleep(60)"], Path(directory), timeout=1,
			)
		self.assertTrue(run.timed_out)
		self.assertEqual(run.returncode, -1)
		self.assertIn("non-JSON stdout", run.digest.summary())


class FailureReportTests(unittest.TestCase):
	def test_deeply_nested_json_line_does_not_crash_the_summariser(self):
		summary = draft_acceptance_oracles.summarize_pi_output("[" * 100000 + "\n" + json.dumps({"type": "agent_start"}))
		self.assertIn("pi events: 1", summary)

	def test_summary_stays_small_for_huge_deltas_and_texts(self):
		lines = [json.dumps({"type": "message_start", "message": {"role": "assistant"}})]
		lines += [json.dumps({"type": "message_update", "assistantMessageEvent": {"type": "text_delta", "delta": "d" * 100000}}) for _ in range(500)]
		lines.append(json.dumps({"type": "message_end", "message": {"role": "assistant", "stopReason": "error", "errorMessage": "E" * 100000, "content": [{"type": "text", "text": "T" * 100000}]}}))
		lines.append(json.dumps({"type": "message_start", "message": {"role": "assistant"}}))
		lines += [json.dumps({"type": "message_update", "assistantMessageEvent": {"type": "text_delta", "delta": "s" * 100000}}) for _ in range(500)]
		summary = draft_acceptance_oracles.summarize_pi_output("\n".join(lines))
		self.assertLess(len(summary), 4000)
		# The report path bounds it again.
		self.assertLessEqual(len(draft_acceptance_oracles._tail(summary)), draft_acceptance_oracles.FAILURE_TAIL_BYTES)
		self.assertLess(len(draft_acceptance_oracles._tail("q" * 10_000_000)), draft_acceptance_oracles.FAILURE_TAIL_BYTES + 1)

	def test_a_long_run_of_deltas_is_capped_by_count(self):
		lines = [json.dumps({"type": "message_start", "message": {"role": "assistant"}})]
		lines += [json.dumps({"type": "message_update", "assistantMessageEvent": {"type": "text_delta", "delta": f"<{i}>"}}) for i in range(2000)]
		summary = draft_acceptance_oracles.summarize_pi_output("\n".join(lines))
		self.assertIn("<1999>", summary)
		self.assertNotIn("<0>", summary)

	def test_pi_stream_json_is_summarised_not_dumped(self):
		lines = [json.dumps({"type": "agent_start"}), json.dumps({"type": "message_start", "message": {"role": "assistant"}})]
		lines += [json.dumps({"type": "message_update", "assistantMessageEvent": {"type": "thinking_delta", "delta": f"think{i} "}}) for i in range(300)]
		lines += [
			json.dumps({"type": "message_end", "message": {"role": "assistant", "stopReason": "error", "errorMessage": "429 rate limited by upstream", "content": [{"type": "text", "text": "partial answer"}]}}),
			json.dumps({"type": "message_start", "message": {"role": "assistant"}}),
			json.dumps({"type": "message_update", "assistantMessageEvent": {"type": "thinking_delta", "delta": "still deciding the API name"}}),
			"pi: warning: something on stdout",
		]
		summary = draft_acceptance_oracles.summarize_pi_output("\n".join(lines))
		self.assertIn("assistant turns: 1 (1 errored)", summary)
		self.assertIn("429 rate limited by upstream", summary)
		self.assertIn("partial answer", summary)
		self.assertIn("still deciding the API name", summary)
		self.assertIn("pi: warning: something on stdout", summary)
		self.assertNotIn("thinking_delta", summary)
		self.assertNotIn("think150", summary)
		self.assertLess(len(summary), 3000)


	def test_sanitize_shared_vectors_agree_with_the_go_side(self):
		vectors = json.loads((Path(__file__).resolve().parents[3] / "cmd" / "factoryd" / "testdata" / "sanitize_vectors.json").read_text(encoding="utf-8"))
		for vec in vectors:
			got = draft_acceptance_oracles.sanitize_text(vec["in"])
			for absent in vec["absent"]:
				self.assertNotIn(absent, got)
			for present in vec["present"]:
				self.assertIn(present, got)

	def test_report_failure_bounds_and_sanitises_model_controlled_text(self):
		import contextlib
		import io
		with tempfile.TemporaryDirectory() as directory:
			draft_dir = Path(directory)
			for i in range(500):
				(draft_dir / f"f{i:03d}.go").write_text("x")
			result = draft_acceptance_oracles.PiRun.from_text(1, "Authorization: Bearer SECRETVALUE " + "y" * 50000, "\x1b[31mred\x1b[0m " + "z" * 50000)
			stderr = io.StringIO()
			with contextlib.redirect_stderr(stderr):
				draft_acceptance_oracles.report_failure("bad " + "r" * 5000 + " api_key=hunter2", result, draft_dir)
			out = stderr.getvalue()
			self.assertLess(len(out), 12000)
			for bad in ("SECRETVALUE", "hunter2", "\x1b"):
				self.assertNotIn(bad, out)
			self.assertIn("500 entries", out)

	def test_evidence_failure_reason_is_bounded_and_sanitised(self):
		reason = draft_acceptance_oracles._bounded_reason("x" * 5000 + " token=abc123")
		self.assertLessEqual(len(reason), draft_acceptance_oracles.FAILURE_REASON_CHARS)
		self.assertNotIn("abc123", draft_acceptance_oracles._bounded_reason("token=abc123"))


class CriterionEchoTests(unittest.TestCase):
	def test_criteria_that_normalise_alike_are_ambiguous(self):
		with tempfile.TemporaryDirectory() as directory:
			manifest = [{"criterion": "1. Same thing.", "oracle_file": None}, {"criterion": "2. Same thing", "oracle_file": None}]
			with self.assertRaisesRegex(ValueError, "same after normalisation"):
				draft_acceptance_oracles.validate_manifest(manifest, ["1. Same thing.", "2. Same thing"], Path(directory))

	def test_shared_vectors_agree_with_the_go_side(self):
		vectors = json.loads((Path(__file__).resolve().parents[3] / "cmd" / "factoryd" / "testdata" / "criterion_key_vectors.json").read_text(encoding="utf-8"))
		self.assertGreater(len(vectors), 10)
		for vec in vectors:
			with self.subTest(a=vec["a"], b=vec["b"]):
				self.assertEqual(build_app.criterion_key(vec["a"]) == build_app.criterion_key(vec["b"]), vec["match"])


	def _ok(self, echoed, expected, index=0):
		with tempfile.TemporaryDirectory() as directory:
			manifest = [{"criterion": "x", "oracle_file": None}] * index + [{"criterion": echoed, "oracle_file": None}]
			criteria = ["x"] * index + [expected]
			draft_acceptance_oracles.validate_manifest(manifest, criteria, Path(directory))

	def test_number_prefix_trailing_period_and_edge_whitespace_are_ignored(self):
		self._ok("Zero amounts are ignored (e.g. `{}`)  ", "5. Zero amounts are ignored (e.g. `{}`).")
		self._ok("5) Zero amounts.", "5. Zero amounts")

	def test_near_misses_still_fail(self):
		expected = "5. Zero amounts are ignored."
		for echoed in [
			"5. Zero amounts are ignored",  # control: equal after normalisation
		]:
			self._ok(echoed, expected)
		for echoed in [
			"5. Zero amounts are",  # prefix
			"5. Zero amounts are ignored. Also nil is empty.",  # superset
			"5. Zero amounts  are ignored.",  # interior whitespace differs
			"5. zero amounts are ignored.",  # case differs
			"5. Zero amounts are not ignored.",
			"5. Zero amounts are ignored!",  # other punctuation
			"",
		]:
			with self.subTest(echoed=echoed):
				with self.assertRaises(ValueError):
					self._ok(echoed, expected)

	def test_entry_index_is_still_checked(self):
		with tempfile.TemporaryDirectory() as directory:
			manifest = [{"criterion": "2. B.", "oracle_file": None}, {"criterion": "1. A.", "oracle_file": None}]
			with self.assertRaises(ValueError):
				draft_acceptance_oracles.validate_manifest(manifest, ["1. A.", "2. B."], Path(directory))


class RunDraftOraclesTests(unittest.TestCase):
	def test_success_copies_oracle_files_and_manifest_and_writes_evidence(self):
		# Two criteria, TWO separate pi invocations: the first drafts
		# an oracle, the second judges its own criterion not oracle-able.
		with tempfile.TemporaryDirectory() as directory:
			workspace = Path(directory) / "repo"
			workspace.mkdir()
			init_repo(workspace)
			criteria_path = Path(directory) / "criteria.md"
			criteria_path.write_text("1. Returns 200.\n2. Code is clean.\n")
			out_dir = Path(directory) / "oracles"
			evidence_path = Path(directory) / "evidence.json"
			draft_dir_base = workspace / draft_acceptance_oracles.DRAFT_RELATIVE_DIR

			manifest1 = [{"criterion": "1. Returns 200.", "oracle_file": "oracle_001.py", "rationale": "derivable value"}]
			manifest2 = [{"criterion": "2. Code is clean.", "oracle_file": None, "rationale": "judgment call"}]
			usage1 = json.dumps({"type": "agent_end", "messages": [{"role": "assistant", "usage": {"input": 10, "output": 5}}]})
			usage2 = json.dumps({"type": "agent_end", "messages": [{"role": "assistant", "usage": {"input": 3, "output": 1}}]})
			specs = [
				(0, usage1, {"oracle_001.py": "assert 1 == 1\n", draft_acceptance_oracles.MANIFEST_FILENAME: json.dumps(manifest1)}, "", False),
				(0, usage2, {draft_acceptance_oracles.MANIFEST_FILENAME: json.dumps(manifest2)}, "", False),
			]
			with mock.patch.object(
				draft_acceptance_oracles, "run_pi_bounded",
				side_effect=scripted_pi_sequence(specs, draft_dir_base),
			):
				exit_code = draft_acceptance_oracles.run_draft_oracles(
					workspace, criteria_path, out_dir, evidence_path, timeout_minutes=2,
				)

			self.assertEqual(exit_code, 0)
			written = json.loads((out_dir / draft_acceptance_oracles.MANIFEST_FILENAME).read_text())
			self.assertEqual([e["criterion"] for e in written], ["1. Returns 200.", "2. Code is clean."])
			self.assertEqual([e["criterion_index"] for e in written], [1, 2])
			self.assertIsNotNone(written[0]["oracle_file"])
			self.assertIsNone(written[1]["oracle_file"])
			# Assembled under a host-chosen, collision-free name -- not
			# necessarily the model's own "oracle_001.py".
			self.assertEqual((out_dir / written[0]["oracle_file"]).read_text(), "assert 1 == 1\n")
			evidence = json.loads(evidence_path.read_text())
			self.assertEqual(evidence["agent_exit_code"], 0)
			self.assertEqual(evidence["criteria_count"], 2)
			self.assertEqual(evidence["oracle_count"], 1)
			self.assertEqual(evidence["usage"], {"input": 13, "output": 6})
			self.assertEqual(evidence["failures"], [])
			self.assertEqual(evidence["salvaged_count"], 0)
			self.assertIsNone(evidence["thinking"])

	def test_run_draft_oracles_builds_max_thinking_argv_and_omits_flag_when_unset(self):
		for thinking in ("max", None):
			with self.subTest(thinking=thinking), tempfile.TemporaryDirectory() as directory:
				workspace = Path(directory) / "repo"
				workspace.mkdir()
				init_repo(workspace)
				criteria_path = Path(directory) / "criteria.md"
				criteria_path.write_text("1. Returns 200.\n")
				out_dir = Path(directory) / "oracles"
				evidence_path = Path(directory) / "evidence.json"
				draft_dir_base = workspace / draft_acceptance_oracles.DRAFT_RELATIVE_DIR
				commands = []
				specs = [(1, "", None, "", False)]
				with mock.patch.object(
					draft_acceptance_oracles, "run_pi_bounded",
					side_effect=scripted_pi_sequence(specs, draft_dir_base, commands),
				):
					draft_acceptance_oracles.run_draft_oracles(
						workspace, criteria_path, out_dir, evidence_path, timeout_minutes=1,
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
			str(DRAFT_ORACLES_SCRIPT), "--criteria", "/tmp/criteria.md", "--workspace", "/tmp/work",
			"--out-dir", "/tmp/oracles", "--evidence", "/tmp/evidence.json",
		]
		levels = build_app.THINKING_LEVELS
		for level in (*levels, None):
			with self.subTest(level=level):
				args = base_argv + ([] if level is None else ["--thinking", level])
				with mock.patch.object(sys, "argv", args), mock.patch.object(draft_acceptance_oracles, "run_draft_oracles", return_value=0) as run_draft_oracles:
					self.assertEqual(draft_acceptance_oracles.main(), 0)
				self.assertEqual(run_draft_oracles.call_args.kwargs["thinking"], level)

		with mock.patch.object(sys, "argv", base_argv + ["--thinking", "unknown"]):
			with contextlib.redirect_stderr(io.StringIO()):
				with self.assertRaises(SystemExit) as raised:
					draft_acceptance_oracles.main()
		self.assertEqual(raised.exception.code, 2)

	def test_one_criterion_failing_does_not_lose_the_others(self):
		# The headline per-criterion-isolation property: of 3 criteria, one
		# times out and one fails outright, but the third is still drafted
		# and installed, and both failures are reported per-criterion in
		# evidence.
		with tempfile.TemporaryDirectory() as directory:
			workspace = Path(directory) / "repo"
			workspace.mkdir()
			init_repo(workspace)
			criteria_path = Path(directory) / "criteria.md"
			criteria_path.write_text("1. Slow one.\n2. Broken one.\n3. Good one.\n")
			out_dir = Path(directory) / "oracles"
			evidence_path = Path(directory) / "evidence.json"
			draft_dir_base = workspace / draft_acceptance_oracles.DRAFT_RELATIVE_DIR

			manifest3 = [{"criterion": "3. Good one.", "oracle_file": "c_test.go", "rationale": "derivable value"}]
			specs = [
				(-1, "", None, "", True),  # criterion 1: timed out
				(1, "", None, "", False),  # criterion 2: pi exited 1
				(0, "", {"c_test.go": "package p\n", draft_acceptance_oracles.MANIFEST_FILENAME: json.dumps(manifest3)}, "", False),
			]
			with mock.patch.object(
				draft_acceptance_oracles, "run_pi_bounded",
				side_effect=scripted_pi_sequence(specs, draft_dir_base),
			):
				exit_code = draft_acceptance_oracles.run_draft_oracles(
					workspace, criteria_path, out_dir, evidence_path, timeout_minutes=3,
				)

			self.assertEqual(exit_code, 0)
			written = json.loads((out_dir / draft_acceptance_oracles.MANIFEST_FILENAME).read_text())
			self.assertEqual(len(written), 3)
			self.assertIsNone(written[0]["oracle_file"])
			self.assertIn("not drafted", written[0]["rationale"])
			self.assertIsNone(written[1]["oracle_file"])
			self.assertIn("not drafted", written[1]["rationale"])
			self.assertIsNotNone(written[2]["oracle_file"])
			evidence = json.loads(evidence_path.read_text())
			self.assertEqual(evidence["status"], "drafted")
			self.assertEqual(evidence["oracle_count"], 1)
			self.assertEqual([f["criterion_index"] for f in evidence["failures"]], [1, 2])
			self.assertIn("wall-clock budget", evidence["failures"][0]["reason"])
			self.assertIn("exited 1", evidence["failures"][1]["reason"])

	def test_pi_failure_on_the_only_criterion_writes_evidence_with_null_oracle_count_and_exits_2(self):
		with tempfile.TemporaryDirectory() as directory:
			workspace = Path(directory) / "repo"
			workspace.mkdir()
			init_repo(workspace)
			criteria_path = Path(directory) / "criteria.md"
			criteria_path.write_text("1. Returns 200.\n")
			out_dir = Path(directory) / "oracles"
			evidence_path = Path(directory) / "evidence.json"
			draft_dir_base = workspace / draft_acceptance_oracles.DRAFT_RELATIVE_DIR

			specs = [(1, "", None, "", False)]
			with mock.patch.object(
				draft_acceptance_oracles, "run_pi_bounded",
				side_effect=scripted_pi_sequence(specs, draft_dir_base),
			):
				exit_code = draft_acceptance_oracles.run_draft_oracles(
					workspace, criteria_path, out_dir, evidence_path, timeout_minutes=1,
				)

			self.assertEqual(exit_code, 2)
			self.assertFalse(out_dir.exists())
			evidence = json.loads(evidence_path.read_text())
			self.assertIsNone(evidence["oracle_count"])
			self.assertEqual(evidence["status"], "failed")
			self.assertEqual(len(evidence["failures"]), 1)

	def test_malformed_manifest_exits_2_even_on_a_clean_pi_exit(self):
		# Regression for the exact failure class validate_manifest exists
		# to catch: a clean pi exit (returncode 0) with a manifest that
		# doesn't actually match the input criteria must not be trusted --
		# same "agent output is evidence, never the oracle" split as every
		# other check in this pipeline.
		with tempfile.TemporaryDirectory() as directory:
			workspace = Path(directory) / "repo"
			workspace.mkdir()
			init_repo(workspace)
			criteria_path = Path(directory) / "criteria.md"
			criteria_path.write_text("1. Returns 200.\n")
			out_dir = Path(directory) / "oracles"
			evidence_path = Path(directory) / "evidence.json"
			draft_dir_base = workspace / draft_acceptance_oracles.DRAFT_RELATIVE_DIR

			specs = [(0, "", {draft_acceptance_oracles.MANIFEST_FILENAME: json.dumps([{"criterion": "wrong text", "oracle_file": None, "rationale": "x"}])}, "", False)]
			with mock.patch.object(
				draft_acceptance_oracles, "run_pi_bounded",
				side_effect=scripted_pi_sequence(specs, draft_dir_base),
			):
				exit_code = draft_acceptance_oracles.run_draft_oracles(
					workspace, criteria_path, out_dir, evidence_path, timeout_minutes=1,
				)

			self.assertEqual(exit_code, 2)
			self.assertFalse(out_dir.exists())

	def test_no_manifest_and_nothing_salvageable_exits_2(self):
		with tempfile.TemporaryDirectory() as directory:
			workspace = Path(directory) / "repo"
			workspace.mkdir()
			init_repo(workspace)
			criteria_path = Path(directory) / "criteria.md"
			criteria_path.write_text("1. Returns 200.\n")
			out_dir = Path(directory) / "oracles"
			evidence_path = Path(directory) / "evidence.json"
			draft_dir_base = workspace / draft_acceptance_oracles.DRAFT_RELATIVE_DIR

			specs = [(0, "", None, "", False)]
			with mock.patch.object(
				draft_acceptance_oracles, "run_pi_bounded",
				side_effect=scripted_pi_sequence(specs, draft_dir_base),
			):
				exit_code = draft_acceptance_oracles.run_draft_oracles(
					workspace, criteria_path, out_dir, evidence_path, timeout_minutes=1,
				)

			self.assertEqual(exit_code, 2)
			self.assertFalse(out_dir.exists())

	def test_no_manifest_written_but_salvageable_content_still_succeeds(self):
		# Salvage from final text: pi exits 0, writes no MANIFEST.json, but
		# its final response text unambiguously names and contains the
		# oracle file.
		with tempfile.TemporaryDirectory() as directory:
			workspace = Path(directory) / "repo"
			workspace.mkdir()
			init_repo(workspace)
			criteria_path = Path(directory) / "criteria.md"
			criteria_path.write_text("1. Returns 200.\n")
			out_dir = Path(directory) / "oracles"
			evidence_path = Path(directory) / "evidence.json"
			draft_dir_base = workspace / draft_acceptance_oracles.DRAFT_RELATIVE_DIR

			manifest_json = json.dumps([{"criterion": "1. Returns 200.", "oracle_file": "oracle_001_test.go", "target_path": "pkg/oracle_001_test.go", "rationale": "derivable value"}])
			final_text = (
				f"Here is my manifest:\n\n{manifest_json}\n\n"
				"And here is oracle_001_test.go:\n\n```go\npackage pkg\n\nfunc TestOracleX(t *testing.T) {}\n```\n"
			)
			message_end = json.dumps({"type": "message_end", "message": {"role": "assistant", "content": [{"type": "text", "text": final_text}]}})
			specs = [(0, message_end, None, "", False)]
			with mock.patch.object(
				draft_acceptance_oracles, "run_pi_bounded",
				side_effect=scripted_pi_sequence(specs, draft_dir_base),
			):
				exit_code = draft_acceptance_oracles.run_draft_oracles(
					workspace, criteria_path, out_dir, evidence_path, timeout_minutes=1,
				)

			self.assertEqual(exit_code, 0)
			written = json.loads((out_dir / draft_acceptance_oracles.MANIFEST_FILENAME).read_text())
			self.assertIsNotNone(written[0]["oracle_file"])
			self.assertIn("salvaged", written[0]["rationale"])
			self.assertEqual((out_dir / written[0]["oracle_file"]).read_text(), "package pkg\n\nfunc TestOracleX(t *testing.T) {}\n")
			evidence = json.loads(evidence_path.read_text())
			self.assertEqual(evidence["salvaged_count"], 1)
			self.assertEqual(evidence["failures"], [])

	def _run_failing(self, criteria_text, specs_factory):
		import contextlib
		import io
		with tempfile.TemporaryDirectory() as directory:
			workspace = Path(directory) / "repo"
			workspace.mkdir()
			init_repo(workspace)
			criteria_path = Path(directory) / "criteria.md"
			criteria_path.write_text(criteria_text)
			evidence_path = Path(directory) / "evidence.json"
			draft_dir_base = workspace / draft_acceptance_oracles.DRAFT_RELATIVE_DIR
			stderr = io.StringIO()
			with mock.patch.object(draft_acceptance_oracles, "run_pi_bounded", side_effect=scripted_pi_sequence(specs_factory(), draft_dir_base)), contextlib.redirect_stderr(stderr):
				exit_code = draft_acceptance_oracles.run_draft_oracles(
					workspace, criteria_path, Path(directory) / "oracles", evidence_path, timeout_minutes=1,
				)
			return exit_code, stderr.getvalue(), json.loads(evidence_path.read_text())

	def test_failed_draft_reports_the_reason_and_pi_output_on_stderr(self):
		manifest = json.dumps([{"criterion": "wrong text", "oracle_file": None, "rationale": "x"}])
		exit_code, stderr, evidence = self._run_failing(
			"1. Returns 200.\n",
			lambda: [(0, "pi said hello", {draft_acceptance_oracles.MANIFEST_FILENAME: manifest}, "", False)],
		)
		self.assertEqual(exit_code, 2)
		self.assertIn("does not match expected", stderr)
		self.assertIn("pi said hello", stderr)
		self.assertIn(draft_acceptance_oracles.MANIFEST_FILENAME, stderr)
		self.assertIn("does not match expected", evidence["failure_reason"])
		self.assertIn("does not match expected", evidence["failures"][0]["reason"])

	def test_pi_timeout_writes_evidence_and_exits_2_instead_of_crashing(self):
		# Found live 2026-09-20: subprocess.TimeoutExpired escaped, so the
		# script exited 1 with no evidence.
		exit_code, stderr, evidence = self._run_failing(
			"1. Returns 200.\n",
			lambda: [(-1, "partial out", None, "partial err", True)],
		)
		self.assertEqual(exit_code, 2)
		self.assertIn("wall-clock budget", stderr)
		self.assertIn("partial out", stderr)
		self.assertIn("partial err", stderr)
		self.assertEqual(evidence["status"], "failed")
		self.assertIn("wall-clock budget", evidence["failure_reason"])

	def test_all_criteria_judged_not_oracle_able_is_still_success(self):
		# A well-formed manifest where every oracle_file is null is a
		# legitimate outcome (a spec whose criteria are all judgment
		# calls), not a failure -- see this script's own doc comment.
		with tempfile.TemporaryDirectory() as directory:
			workspace = Path(directory) / "repo"
			workspace.mkdir()
			init_repo(workspace)
			criteria_path = Path(directory) / "criteria.md"
			criteria_path.write_text("1. Code is clean.\n")
			out_dir = Path(directory) / "oracles"
			evidence_path = Path(directory) / "evidence.json"
			draft_dir_base = workspace / draft_acceptance_oracles.DRAFT_RELATIVE_DIR

			manifest = [{"criterion": "1. Code is clean.", "oracle_file": None, "rationale": "judgment call"}]
			specs = [(0, "", {draft_acceptance_oracles.MANIFEST_FILENAME: json.dumps(manifest)}, "", False)]
			with mock.patch.object(
				draft_acceptance_oracles, "run_pi_bounded",
				side_effect=scripted_pi_sequence(specs, draft_dir_base),
			):
				exit_code = draft_acceptance_oracles.run_draft_oracles(
					workspace, criteria_path, out_dir, evidence_path, timeout_minutes=1,
				)

			self.assertEqual(exit_code, 0)
			evidence = json.loads(evidence_path.read_text())
			self.assertEqual(evidence["oracle_count"], 0)
			self.assertEqual(evidence["status"], "none_eligible")

	def test_criterion_timeout_minutes_flag_overrides_the_split(self):
		# Each criterion's own pi call is timed against
		# --criterion-timeout-minutes when given, not the auto split of
		# --timeout-minutes.
		with tempfile.TemporaryDirectory() as directory:
			workspace = Path(directory) / "repo"
			workspace.mkdir()
			init_repo(workspace)
			criteria_path = Path(directory) / "criteria.md"
			criteria_path.write_text("1. A.\n2. B.\n")
			out_dir = Path(directory) / "oracles"
			evidence_path = Path(directory) / "evidence.json"
			seen_timeouts = []

			def spy(command, cwd, timeout, adapter=None):
				seen_timeouts.append(timeout)
				return draft_acceptance_oracles.PiRun.from_text(1, "", "", False)

			with mock.patch.object(draft_acceptance_oracles, "run_pi_bounded", side_effect=spy):
				draft_acceptance_oracles.run_draft_oracles(
					workspace, criteria_path, out_dir, evidence_path, timeout_minutes=100,
					criterion_timeout_minutes=7,
				)
			self.assertEqual(seen_timeouts, [7 * 60, 7 * 60])

	def test_every_criterion_naming_the_same_local_file_still_assembles_distinct_targets(self):
		# Regression for the live incident (2026-09-24,
		# add-a-get-todos-id-shout-endpoint-that-r-20260924-024340): 6
		# separate invocations, several of them independently naming their
		# local file (and a matching target_path) "oracle_001_test.go" --
		# exactly what happens when every isolated invocation defaults to
		# the DRAFT_INSTRUCTIONS example name. All N must still assemble to
		# N distinct target_paths and install successfully, not fail the
		# whole draft.
		with tempfile.TemporaryDirectory() as directory:
			workspace = Path(directory) / "repo"
			workspace.mkdir()
			init_repo(workspace)
			n = 4
			criteria_path = Path(directory) / "criteria.md"
			criteria_path.write_text("".join(f"{i}. Criterion {i}.\n" for i in range(1, n + 1)))
			out_dir = Path(directory) / "oracles"
			evidence_path = Path(directory) / "evidence.json"
			draft_dir_base = workspace / draft_acceptance_oracles.DRAFT_RELATIVE_DIR

			specs = [
				(0, "", {
					"oracle_001_test.go": f"package p\nfunc TestOracleN{i}(t *testing.T) {{}}\n",
					draft_acceptance_oracles.MANIFEST_FILENAME: json.dumps([{
						"criterion": f"{i}. Criterion {i}.",
						"oracle_file": "oracle_001_test.go",
						"target_path": "oracle_001_test.go",
						"rationale": "r",
					}]),
				}, "", False)
				for i in range(1, n + 1)
			]
			with mock.patch.object(
				draft_acceptance_oracles, "run_pi_bounded",
				side_effect=scripted_pi_sequence(specs, draft_dir_base),
			):
				exit_code = draft_acceptance_oracles.run_draft_oracles(
					workspace, criteria_path, out_dir, evidence_path, timeout_minutes=n,
				)

			self.assertEqual(exit_code, 0)
			written = json.loads((out_dir / draft_acceptance_oracles.MANIFEST_FILENAME).read_text())
			self.assertEqual(len(written), n)
			target_paths = [e["target_path"] for e in written]
			self.assertTrue(all(target_paths), msg=written)
			self.assertEqual(len(set(target_paths)), n, msg=f"expected {n} distinct target_paths, got {target_paths}")
			oracle_files = [e["oracle_file"] for e in written]
			self.assertEqual(len(set(oracle_files)), n)
			for e in written:
				self.assertEqual(e["target_path"], e["oracle_file"])
				self.assertEqual((out_dir / e["oracle_file"]).read_text(), f"package p\nfunc TestOracleN{e['criterion_index']}(t *testing.T) {{}}\n")
			evidence = json.loads(evidence_path.read_text())
			self.assertEqual(evidence["oracle_count"], n)
			self.assertEqual(evidence["failures"], [])

	def test_criteria_sharing_a_real_package_target_basename_still_assemble_distinct_targets(self):
		# Regression for the second live incident (2026-09-24,
		# implement-mergepatch-target-patch-byte-b-20260924-024340): 2 of 9
		# criteria both correctly targeted the SAME real, pre-existing file
		# ("merge_test.go") -- a basename that never matched their own
		# local scratch oracle_file name at all. Must assemble to distinct
		# targets, not fail the whole draft.
		with tempfile.TemporaryDirectory() as directory:
			workspace = Path(directory) / "repo"
			workspace.mkdir()
			init_repo(workspace)
			n = 3
			criteria_path = Path(directory) / "criteria.md"
			criteria_path.write_text("".join(f"{i}. Criterion {i}.\n" for i in range(1, n + 1)))
			out_dir = Path(directory) / "oracles"
			evidence_path = Path(directory) / "evidence.json"
			draft_dir_base = workspace / draft_acceptance_oracles.DRAFT_RELATIVE_DIR

			# Criteria 1 and 3 both (correctly) target the package's real
			# merge_test.go under their own distinct local scratch names;
			# criterion 2 targets a different real file.
			targets = {1: "pkg/merge_test.go", 2: "pkg/other_test.go", 3: "pkg/merge_test.go"}
			specs = [
				(0, "", {
					f"local_{i:03d}_test.go": f"package p\nfunc TestOracleN{i}(t *testing.T) {{}}\n",
					draft_acceptance_oracles.MANIFEST_FILENAME: json.dumps([{
						"criterion": f"{i}. Criterion {i}.",
						"oracle_file": f"local_{i:03d}_test.go",
						"target_path": targets[i],
						"rationale": "r",
					}]),
				}, "", False)
				for i in range(1, n + 1)
			]
			with mock.patch.object(
				draft_acceptance_oracles, "run_pi_bounded",
				side_effect=scripted_pi_sequence(specs, draft_dir_base),
			):
				exit_code = draft_acceptance_oracles.run_draft_oracles(
					workspace, criteria_path, out_dir, evidence_path, timeout_minutes=n,
				)

			self.assertEqual(exit_code, 0)
			written = json.loads((out_dir / draft_acceptance_oracles.MANIFEST_FILENAME).read_text())
			self.assertEqual(len(written), n)
			self.assertTrue(all(e["oracle_file"] for e in written), msg=written)
			self.assertEqual(len({e["oracle_file"] for e in written}), n)
			self.assertEqual(len({e["target_path"] for e in written}), n)
			for e in written:
				self.assertTrue(e["target_path"].startswith("pkg/"), msg=e)
			evidence = json.loads(evidence_path.read_text())
			self.assertEqual(evidence["oracle_count"], n)
			self.assertEqual(evidence["failures"], [])


class AssembleManifestTests(unittest.TestCase):
	def _result(self, draft_dir, criterion, oracle_file, target_path, content=None):
		if oracle_file:
			draft_dir.mkdir(parents=True, exist_ok=True)
			(draft_dir / oracle_file).write_text(content if content is not None else "package p\n")
		entry = {"criterion": criterion, "oracle_file": oracle_file, "target_path": target_path, "rationale": "r"}
		return draft_acceptance_oracles.CriterionResult(
			entry=entry, draft_dir=draft_dir,
			result=draft_acceptance_oracles.PiRun.from_text(0, ""),
			usage=None, salvaged=False, failure_reason=None,
		)

	def test_renames_files_and_rewrites_target_path_basenames(self):
		with tempfile.TemporaryDirectory() as directory:
			base = Path(directory)
			results = [
				self._result(base / "c001", "1. A.", "oracle_001_test.go", "pkg/oracle_001_test.go"),
				self._result(base / "c002", "2. B.", "oracle_001_test.go", "oracle_001_test.go"),
			]
			manifest, assembled_dir, failures = draft_acceptance_oracles.assemble_manifest(results, base)
			self.assertEqual(failures, [])
			self.assertEqual(manifest[0]["target_path"], f"pkg/{manifest[0]['oracle_file']}")
			self.assertEqual(manifest[1]["target_path"], manifest[1]["oracle_file"])
			self.assertNotEqual(manifest[0]["oracle_file"], manifest[1]["oracle_file"])
			self.assertTrue((assembled_dir / manifest[0]["oracle_file"]).is_file())
			self.assertTrue((assembled_dir / manifest[1]["oracle_file"]).is_file())

	def test_target_path_basename_not_matching_own_oracle_file_is_rewritten_not_dropped(self):
		# Live shape (implement-mergepatch-target-patch-byte-b-20260924-024340):
		# the model's target_path can legitimately be an existing package
		# file (e.g. "merge_test.go") unrelated to its own local scratch
		# oracle_file name -- that is normal, not a defect, so this must
		# rewrite the basename and keep the criterion, never drop it.
		with tempfile.TemporaryDirectory() as directory:
			base = Path(directory)
			results = [self._result(base / "c001", "1. A.", "oracle_001_test.go", "pkg/merge_test.go")]
			manifest, assembled_dir, failures = draft_acceptance_oracles.assemble_manifest(results, base)
			self.assertEqual(failures, [])
			self.assertIsNotNone(manifest[0]["oracle_file"])
			self.assertEqual(manifest[0]["target_path"], f"pkg/{manifest[0]['oracle_file']}")
			self.assertTrue((assembled_dir / manifest[0]["oracle_file"]).is_file())

	def test_two_criteria_sharing_a_real_target_basename_get_distinct_targets(self):
		# The second live incident's exact shape: two DIFFERENT criteria
		# both correctly targeting the same real, pre-existing file name
		# ("merge_test.go") in the same package.
		with tempfile.TemporaryDirectory() as directory:
			base = Path(directory)
			results = [
				self._result(base / "c001", "1. A.", "a_test.go", "pkg/merge_test.go"),
				self._result(base / "c005", "5. E.", "e_test.go", "pkg/merge_test.go"),
			]
			manifest, _assembled_dir, failures = draft_acceptance_oracles.assemble_manifest(results, base)
			self.assertEqual(failures, [])
			self.assertTrue(all(e["oracle_file"] for e in manifest))
			self.assertEqual(len({e["oracle_file"] for e in manifest}), 2)
			self.assertEqual(len({e["target_path"] for e in manifest}), 2)
			for e in manifest:
				self.assertTrue(e["target_path"].startswith("pkg/"))

	def test_no_target_path_still_assembles(self):
		with tempfile.TemporaryDirectory() as directory:
			base = Path(directory)
			results = [self._result(base / "c001", "1. A.", "oracle_001_test.go", None)]
			manifest, assembled_dir, failures = draft_acceptance_oracles.assemble_manifest(results, base)
			self.assertEqual(failures, [])
			self.assertIsNotNone(manifest[0]["oracle_file"])
			self.assertIsNone(manifest[0]["target_path"])

	def test_null_oracle_file_entry_passes_through_untouched(self):
		with tempfile.TemporaryDirectory() as directory:
			base = Path(directory)
			results = [self._result(base / "c001", "1. A.", None, None)]
			manifest, _assembled_dir, failures = draft_acceptance_oracles.assemble_manifest(results, base)
			self.assertEqual(failures, [])
			self.assertIsNone(manifest[0]["oracle_file"])


class PerCriterionMinutesTests(unittest.TestCase):
	def test_splits_evenly_with_a_floor(self):
		self.assertEqual(draft_acceptance_oracles.per_criterion_minutes(15, 1), 15)
		self.assertEqual(draft_acceptance_oracles.per_criterion_minutes(15, 5), 3)
		# 15 // 7 == 2, floored to the default 3.
		self.assertEqual(draft_acceptance_oracles.per_criterion_minutes(15, 7), 3)
		self.assertEqual(draft_acceptance_oracles.per_criterion_minutes(60, 4), 15)

	def test_zero_criteria_returns_the_total_unchanged(self):
		self.assertEqual(draft_acceptance_oracles.per_criterion_minutes(15, 0), 15)

	def test_custom_floor(self):
		self.assertEqual(draft_acceptance_oracles.per_criterion_minutes(10, 10, floor_minutes=5), 5)


class SalvageFromFinalTextTests(unittest.TestCase):
	def _entry(self, oracle_file="oracle_001_test.go", target_path="pkg/oracle_001_test.go"):
		return {"criterion": "1. A.", "oracle_file": oracle_file, "target_path": target_path, "rationale": "r"}

	def test_labeled_block_with_a_manifest_entry_is_salvaged(self):
		manifest = json.dumps([self._entry()])
		text = f"{manifest}\n\noracle_001_test.go:\n```go\npackage pkg\nfunc TestOracleX(t *testing.T) {{}}\n```\n"
		with tempfile.TemporaryDirectory() as directory:
			draft_dir = Path(directory)
			got = draft_acceptance_oracles.salvage_from_final_text(text, "1. A.", draft_dir)
			self.assertIsNotNone(got)
			self.assertEqual(got[0]["oracle_file"], "oracle_001_test.go")
			self.assertEqual(got[0]["target_path"], "pkg/oracle_001_test.go")
			self.assertIn("salvaged", got[0]["rationale"])
			self.assertEqual((draft_dir / "oracle_001_test.go").read_text(), "package pkg\nfunc TestOracleX(t *testing.T) {}\n")

	def test_single_manifest_and_single_unlabeled_block_is_salvaged_by_elimination(self):
		manifest = json.dumps([self._entry()])
		text = f"{manifest}\n\n```go\npackage pkg\nfunc TestOracleX(t *testing.T) {{}}\n```\n"
		with tempfile.TemporaryDirectory() as directory:
			got = draft_acceptance_oracles.salvage_from_final_text(text, "1. A.", Path(directory))
			self.assertIsNotNone(got)
			self.assertEqual(got[0]["oracle_file"], "oracle_001_test.go")

	def test_no_manifest_at_all_never_salvages(self):
		text = "```go\npackage pkg\nfunc TestOracleX(t *testing.T) {}\n```\n"
		with tempfile.TemporaryDirectory() as directory:
			self.assertIsNone(draft_acceptance_oracles.salvage_from_final_text(text, "1. A.", Path(directory)))

	def test_no_code_block_at_all_never_salvages(self):
		text = json.dumps([self._entry()])
		with tempfile.TemporaryDirectory() as directory:
			self.assertIsNone(draft_acceptance_oracles.salvage_from_final_text(text, "1. A.", Path(directory)))

	def test_multiple_unlabeled_blocks_with_no_pairing_never_salvages(self):
		manifest = json.dumps([self._entry()])
		text = f"{manifest}\n\n```go\npackage a\n```\n\n```go\npackage b\n```\n"
		with tempfile.TemporaryDirectory() as directory:
			self.assertIsNone(draft_acceptance_oracles.salvage_from_final_text(text, "1. A.", Path(directory)))

	def test_disagreeing_manifest_candidates_never_salvage(self):
		text = (
			json.dumps([self._entry(oracle_file="a_test.go")])
			+ "\n"
			+ json.dumps([self._entry(oracle_file="b_test.go")])
			+ "\n```go\npackage pkg\n```\n"
		)
		with tempfile.TemporaryDirectory() as directory:
			self.assertIsNone(draft_acceptance_oracles.salvage_from_final_text(text, "1. A.", Path(directory)))

	def test_null_oracle_file_in_manifest_never_salvages(self):
		text = json.dumps([self._entry(oracle_file=None)]) + "\n```go\npackage pkg\n```\n"
		with tempfile.TemporaryDirectory() as directory:
			self.assertIsNone(draft_acceptance_oracles.salvage_from_final_text(text, "1. A.", Path(directory)))

	def test_unsafe_oracle_file_name_never_salvages(self):
		text = json.dumps([self._entry(oracle_file="../escape.go")]) + "\n```go\npackage pkg\n```\n"
		with tempfile.TemporaryDirectory() as directory:
			self.assertIsNone(draft_acceptance_oracles.salvage_from_final_text(text, "1. A.", Path(directory)))

	def test_empty_code_block_never_salvages(self):
		manifest = json.dumps([self._entry()])
		text = f"{manifest}\n\noracle_001_test.go:\n```go\n   \n```\n"
		with tempfile.TemporaryDirectory() as directory:
			self.assertIsNone(draft_acceptance_oracles.salvage_from_final_text(text, "1. A.", Path(directory)))

	def test_two_labeled_blocks_for_the_same_name_is_ambiguous(self):
		manifest = json.dumps([self._entry()])
		text = (
			f"{manifest}\n\noracle_001_test.go (draft 1):\n```go\npackage pkg\n// v1\n```\n\n"
			"oracle_001_test.go (draft 2):\n```go\npackage pkg\n// v2\n```\n"
		)
		with tempfile.TemporaryDirectory() as directory:
			self.assertIsNone(draft_acceptance_oracles.salvage_from_final_text(text, "1. A.", Path(directory)))


class NormalizeTargetPathTests(unittest.TestCase):
	def test_accepts_clean_relative_paths(self):
		for ok in ("internal/mood/zz_oracle_test.go", "tests/test_oracle_x.py", "a.go"):
			self.assertEqual(draft_acceptance_oracles.normalize_target_path(ok), ok)

	def test_drops_hostile_or_unclean_values_to_none(self):
		for bad in (
			None, "", 5, "/etc/passwd", "../x.go", "a/../b.go", "a/./b.go", "a//b.go", "a/b/",
			"a\\b.go", ".git/hooks/x", "pkg/.git/x", ".buildgate/oracles.json", ".oracle/x_test.go",
			"pkg/.ORACLE/x", "a\x00b.go", "..",
		):
			self.assertIsNone(draft_acceptance_oracles.normalize_target_path(bad), msg=repr(bad))


class NormalizeManifestTests(unittest.TestCase):
	def _entries(self, draft_dir, sizes):
		manifest = []
		for i, size in enumerate(sizes, start=1):
			name = f"oracle_{i:03d}.py" if size is not None else None
			if name:
				(draft_dir / name).write_bytes(b"x" * size)
			manifest.append({"criterion": f"{i}. C{i}.", "oracle_file": name, "rationale": "r"})
		return manifest

	def test_sets_index_and_supersedes_and_ignores_model_values(self):
		with tempfile.TemporaryDirectory() as directory:
			draft_dir = Path(directory)
			manifest = self._entries(draft_dir, [10, 10])
			manifest[0]["criterion_index"] = 99
			manifest[0]["supersedes"] = ["victim_test.go"]
			manifest[0]["target_path"] = "../evil.go"
			manifest[1]["target_path"] = "pkg/x_test.go"
			out, dropped = draft_acceptance_oracles.normalize_manifest(manifest, draft_dir)
			self.assertEqual(dropped, 0)
			self.assertEqual([e["criterion_index"] for e in out], [1, 2])
			self.assertEqual([e["supersedes"] for e in out], [[], []])
			self.assertIsNone(out[0]["target_path"])
			self.assertEqual(out[1]["target_path"], "pkg/x_test.go")

	def test_per_file_cap_drops_only_the_oversize_file(self):
		with tempfile.TemporaryDirectory() as directory:
			draft_dir = Path(directory)
			manifest = self._entries(draft_dir, [16 * 1024 + 1, 16 * 1024])
			out, dropped = draft_acceptance_oracles.normalize_manifest(manifest, draft_dir)
			self.assertEqual(dropped, 1)
			self.assertIsNone(out[0]["oracle_file"])
			self.assertEqual(out[0]["rationale"], "dropped: file over 16 KiB")
			self.assertEqual(out[1]["oracle_file"], "oracle_002.py")

	def test_ticket_cap_drops_the_rest_in_criterion_order(self):
		with tempfile.TemporaryDirectory() as directory:
			draft_dir = Path(directory)
			# 4 x 16 KiB = 64 KiB fits exactly; the fifth trips the cap and
			# everything after it is dropped even when small.
			manifest = self._entries(draft_dir, [16 * 1024] * 4 + [16 * 1024, 5])
			out, dropped = draft_acceptance_oracles.normalize_manifest(manifest, draft_dir)
			self.assertEqual(dropped, 2)
			self.assertEqual(sum(1 for e in out if e["oracle_file"]), 4)
			for e in out[4:]:
				self.assertIsNone(e["oracle_file"])
				self.assertEqual(e["rationale"], "dropped: over request cap")

	def test_file_count_cap_is_request_level_not_per_ticket(self):
		# The request cap equals the per-ticket cap (15): a draft that fits the
		# request fits one ticket.
		with tempfile.TemporaryDirectory() as directory:
			draft_dir = Path(directory)
			manifest = self._entries(draft_dir, [10] * 10)
			out, dropped = draft_acceptance_oracles.normalize_manifest(manifest, draft_dir)
			self.assertEqual(dropped, 0)
			manifest = self._entries(draft_dir, [10] * (draft_acceptance_oracles.MAX_ORACLE_FILES + 2))
			out, dropped = draft_acceptance_oracles.normalize_manifest(manifest, draft_dir)
			self.assertEqual(dropped, 2)
			self.assertEqual(sum(1 for e in out if e["oracle_file"]), draft_acceptance_oracles.MAX_ORACLE_FILES)

	def test_prompt_names_only_file_shapes_the_canary_recognises(self):
		# Found live 2026-09-20: the prompt's example "oracle_001.go" led the model
		# to draft a file the runtime canary refuses, so the whole draft failed.
		# internal/oraclecanary TestDrafterPromptNamesAreClassified pins the same names.
		with tempfile.TemporaryDirectory() as directory:
			workspace = Path(directory)
			init_repo(workspace)
			prompt, _ = draft_acceptance_oracles.build_prompt(["1. A."], workspace, workspace / ".oracle-draft")
		for shape in ("oracle_NNN_test.go", "TestOracle<Something>"):
			self.assertIn(shape, prompt)
		self.assertIn("ONLY Go oracles are accepted", " ".join(prompt.split()))
		for shape in ("test_oracle_NNN.py", "NNN_oracle_test.py", "NNN.oracle.test.ts", "NNN_test.dart"):
			self.assertNotIn(shape, prompt)
		self.assertNotIn("oracle_001.go", prompt)

	def test_shared_oracle_file_counts_once_toward_the_caps(self):
		# Found live 2026-09-20: one file per criterion (10 files) overran the
		# per-ticket cap. Several criteria may share one file; it counts once.
		with tempfile.TemporaryDirectory() as directory:
			draft_dir = Path(directory)
			(draft_dir / "shared_test.go").write_text("x" * 100)
			manifest = [
				{"criterion": f"{i}. c", "oracle_file": "shared_test.go", "target_path": "a/shared_test.go", "rationale": "r"}
				for i in range(1, 21)
			]
			out, dropped = draft_acceptance_oracles.normalize_manifest(manifest, draft_dir)
			self.assertEqual(dropped, 0)
			self.assertTrue(all(e["oracle_file"] == "shared_test.go" for e in out))

	def test_prompt_groups_only_certain_same_ticket_criteria(self):
		with tempfile.TemporaryDirectory() as directory:
			workspace = Path(directory)
			init_repo(workspace)
			prompt, _ = draft_acceptance_oracles.build_prompt(["1. A."], workspace, workspace / ".oracle-draft")
		flat = " ".join(prompt.split())
		self.assertIn("CERTAINLY be implemented by the same change", flat)
		self.assertIn("When unsure, keep them separate", flat)
		self.assertIn("by default ONE file per criterion", flat)
		self.assertIn("never group across packages", flat)
		self.assertIn(f"at most {draft_acceptance_oracles.MAX_ORACLE_FILES} oracle files in total for the request", flat)
		self.assertIn(f"at most {draft_acceptance_oracles.MAX_ORACLE_FILES} files and 64 KiB per ticket", flat)
		self.assertNotIn("at most 5", flat)

	def test_already_kept_shared_file_is_not_recounted_against_the_total_cap(self):
		# Four 15 KiB files fill 60 of 64 KiB; a later entry re-using the first
		# must stay kept, not trip the total cap as if it were a new file.
		with tempfile.TemporaryDirectory() as directory:
			draft_dir = Path(directory)
			manifest = self._entries(draft_dir, [15 * 1024] * 4)
			manifest.append({"criterion": "5. C5.", "oracle_file": "oracle_001.py", "rationale": "r"})
			out, dropped = draft_acceptance_oracles.normalize_manifest(manifest, draft_dir)
			self.assertEqual(dropped, 0)
			self.assertEqual(out[4]["oracle_file"], "oracle_001.py")

	def test_later_entries_of_a_refused_shared_file_get_the_first_entrys_reason(self):
		with tempfile.TemporaryDirectory() as directory:
			draft_dir = Path(directory)
			(draft_dir / "big_test.go").write_bytes(b"x" * (16 * 1024 + 1))
			manifest = [{"criterion": f"{i}. c", "oracle_file": "big_test.go", "rationale": "r"} for i in (1, 2)]
			out, dropped = draft_acceptance_oracles.normalize_manifest(manifest, draft_dir)
			self.assertEqual(dropped, 2)
			self.assertEqual([e["rationale"] for e in out], ["dropped: file over 16 KiB"] * 2)

	def test_directory_spellings_get_the_file_name_appended(self):
		with tempfile.TemporaryDirectory() as directory:
			base = Path(directory)
			workspace, draft_dir = base / "ws", base / "draft"
			(workspace / "pkg").mkdir(parents=True)
			draft_dir.mkdir()
			(draft_dir / "o_test.go").write_text("x")
			for spelling, want in (
				(".", "o_test.go"), ("./", "o_test.go"), ("./pkg", "pkg/o_test.go"), ("pkg/", "pkg/o_test.go"),
				("./pkg/", "pkg/o_test.go"), ("pkg", "pkg/o_test.go"), ("newdir/", "newdir/o_test.go"),
				("pkg/x_test.go", "pkg/x_test.go"), ("./pkg/x_test.go", None),
				("../", None), ("../pkg/", None), ("/", None), ("/etc/", None), (".git/", None), ("pkg/.oracle/", None),
			):
				manifest = [{"criterion": "1. c", "oracle_file": "o_test.go", "target_path": spelling, "rationale": "r"}]
				out, _ = draft_acceptance_oracles.normalize_manifest(manifest, draft_dir, workspace)
				self.assertEqual(out[0]["target_path"], want, msg=spelling)

	def test_directory_target_path_gets_the_file_name_appended(self):
		# Found live 2026-09-20: the model gave the package DIRECTORY as target_path.
		with tempfile.TemporaryDirectory() as directory:
			base = Path(directory)
			workspace, draft_dir = base / "ws", base / "draft"
			(workspace / "backend" / "internal" / "categorize").mkdir(parents=True)
			draft_dir.mkdir()
			(draft_dir / "oracle_001_test.go").write_text("package categorize\n")
			manifest = [
				{"criterion": "1. A", "oracle_file": "oracle_001_test.go", "target_path": "backend/internal/categorize", "rationale": "r"},
				{"criterion": "2. B", "oracle_file": "oracle_001_test.go", "target_path": "backend/internal/categorize/oracle_001_test.go", "rationale": "r"},
			]
			out, _ = draft_acceptance_oracles.normalize_manifest(manifest, draft_dir, workspace)
			self.assertEqual(out[0]["target_path"], "backend/internal/categorize/oracle_001_test.go")
			self.assertEqual(out[1]["target_path"], "backend/internal/categorize/oracle_001_test.go")
			# Not a directory in the workspace: left alone.
			manifest[0]["target_path"] = "backend/internal/nope"
			out, _ = draft_acceptance_oracles.normalize_manifest(manifest, draft_dir, workspace)
			self.assertEqual(out[0]["target_path"], "backend/internal/nope")

	def test_prompt_states_the_compile_and_spec_example_rules(self):
		# Found live 2026-09-21: a draft that did not compile (a function with no
		# result used as an argument) and one that listed spec-accepted inputs as
		# INVALID. The host checks both after the draft; the prompt says them first.
		with tempfile.TemporaryDirectory() as directory:
			workspace = Path(directory)
			init_repo(workspace)
			prompt, _ = draft_acceptance_oracles.build_prompt(["1. A."], workspace, workspace / ".oracle-draft")
		flat = " ".join(prompt.split())
		self.assertIn("The file must COMPILE", flat)
		self.assertIn("cannot be used as a value or argument", flat)
		self.assertIn("Never contradict the spec's own examples", flat)
		self.assertIn("do not invent extra invalid cases at the boundary of a stated normalisation rule", flat)

	def test_prompt_says_to_test_what_the_criteria_name(self):
		# Found live 2026-09-24: drafted tests called an invented helper, an
		# unrelated existing function, or the HTTP handler.
		with tempfile.TemporaryDirectory() as directory:
			workspace = Path(directory)
			init_repo(workspace)
			prompt, _ = draft_acceptance_oracles.build_prompt(["1. A."], workspace, workspace / ".oracle-draft")
		flat = " ".join(prompt.split())
		self.assertIn("Test the function, method or type the acceptance criteria themselves name", flat)
		self.assertIn("Never call a helper the criteria do not name", flat)

	def test_each_criterion_is_drafted_with_the_others_as_read_only_context(self):
		# Per-criterion drafting used to see only its own criterion, so one
		# that describes behaviour without naming the function gave the model
		# nothing to call.
		criteria = ["1. `MerchantKey(desc string) string` is exported.", "2. Separator runs become one space."]
		prompts = []
		with tempfile.TemporaryDirectory() as directory:
			workspace = Path(directory)
			init_repo(workspace)
			with mock.patch.object(harness_adapters.PiAdapter, "invocation", side_effect=lambda *a, **kw: prompts.append(kw["prompt"]) or ["true"]), \
				mock.patch.object(draft_acceptance_oracles, "run_pi_bounded", return_value=draft_acceptance_oracles.PiRun.from_text(0, "", "", False)):
				draft_acceptance_oracles.draft_one_criterion(2, criteria[1], workspace, 1, None, criteria)
		prompt = prompts[0]
		context, drafted = prompt.split("## Acceptance criteria", 1)
		self.assertIn("(context only)", context)
		self.assertIn(criteria[0], context)
		self.assertNotIn(criteria[1], context)
		self.assertIn(criteria[1], drafted)
		self.assertNotIn(criteria[0], drafted)

	def test_prompt_demands_a_full_file_path_target(self):
		with tempfile.TemporaryDirectory() as directory:
			workspace = Path(directory)
			init_repo(workspace)
			prompt, _ = draft_acceptance_oracles.build_prompt(["1. A."], workspace, workspace / ".oracle-draft")
		self.assertIn("including its file name", prompt)
		self.assertNotIn("is a directory of that package", prompt)


class PythonEcosystemTests(unittest.TestCase):
	"""--ecosystem python: separate drafting instructions, index-derived
	test_oracle_NNN.py file names, and an end-to-end run_draft_oracles pass
	that never sends Go instructions."""

	def test_oracle_filename_for_is_ecosystem_specific(self):
		self.assertEqual(draft_acceptance_oracles.oracle_filename_for(1), "oracle_001_test.go")
		self.assertEqual(draft_acceptance_oracles.oracle_filename_for(1, "go"), "oracle_001_test.go")
		self.assertEqual(draft_acceptance_oracles.oracle_filename_for(1, "python"), "test_oracle_001.py")
		self.assertEqual(draft_acceptance_oracles.oracle_filename_for(12, "python"), "test_oracle_012.py")

	def test_build_prompt_sends_python_instructions_not_go(self):
		with tempfile.TemporaryDirectory() as directory:
			workspace = Path(directory)
			init_repo(workspace)
			prompt, _ = draft_acceptance_oracles.build_prompt(["1. A."], workspace, workspace / ".oracle-draft", ecosystem="python")
		self.assertIn("test_oracle_NNN.py", prompt)
		self.assertIn("NO pytest, NO unittest, NO test classes", prompt)
		self.assertNotIn("oracle_NNN_test.go", prompt)
		self.assertNotIn("TestOracle<Something>", prompt)

	def test_build_prompt_defaults_to_go_instructions(self):
		with tempfile.TemporaryDirectory() as directory:
			workspace = Path(directory)
			init_repo(workspace)
			prompt, _ = draft_acceptance_oracles.build_prompt(["1. A."], workspace, workspace / ".oracle-draft")
		self.assertIn("oracle_NNN_test.go", prompt)
		self.assertNotIn("test_oracle_NNN.py", prompt)

	def test_criterion_index_addendum_names_the_python_default_filename(self):
		with tempfile.TemporaryDirectory() as directory:
			workspace = Path(directory)
			init_repo(workspace)
			prompts = []
			with mock.patch.object(harness_adapters.PiAdapter, "invocation", side_effect=lambda *a, **kw: prompts.append(kw["prompt"]) or ["true"]), \
				mock.patch.object(draft_acceptance_oracles, "run_pi_bounded", return_value=draft_acceptance_oracles.PiRun.from_text(0, "", "", False)):
				draft_acceptance_oracles.draft_one_criterion(2, "2. B.", workspace, 1, None, ["1. A.", "2. B."], ecosystem="python")
		prompt = prompts[0]
		self.assertIn("Name your oracle file exactly test_oracle_002.py", prompt)
		self.assertIn("not test_oracle_001.py unless", prompt)

	def test_assemble_manifest_renames_to_python_shape(self):
		criterion = draft_acceptance_oracles.CriterionResult(
			entry={"criterion": "1. A.", "oracle_file": "whatever.py", "target_path": "tests/whatever.py", "rationale": "r"},
			draft_dir=None, result=None, usage=None, salvaged=False, failure_reason=None,
		)
		with tempfile.TemporaryDirectory() as directory:
			workspace = Path(directory)
			draft_dir = workspace / draft_acceptance_oracles.DRAFT_RELATIVE_DIR / "c001"
			draft_dir.mkdir(parents=True)
			(draft_dir / "whatever.py").write_text("def test_oracle_x():\n    assert True\n")
			criterion.draft_dir = draft_dir
			manifest, assembled_dir, failures = draft_acceptance_oracles.assemble_manifest([criterion], workspace, ecosystem="python")
			self.assertTrue((assembled_dir / "test_oracle_001.py").is_file())
		self.assertEqual(failures, [])
		self.assertEqual(manifest[0]["oracle_file"], "test_oracle_001.py")
		self.assertEqual(manifest[0]["target_path"], "tests/test_oracle_001.py")

	def test_run_draft_oracles_end_to_end_with_python_ecosystem(self):
		with tempfile.TemporaryDirectory() as directory:
			workspace = Path(directory) / "repo"
			workspace.mkdir()
			init_repo(workspace)
			(workspace / "add.py").write_text("def multiply_numbers(a, b):\n    return a * b\n")
			criteria_path = Path(directory) / "criteria.md"
			criteria_path.write_text("1. multiply_numbers(3, 4) == 12.\n2. Code is clean.\n")
			out_dir = Path(directory) / "oracles"
			evidence_path = Path(directory) / "evidence.json"
			draft_dir_base = workspace / draft_acceptance_oracles.DRAFT_RELATIVE_DIR

			manifest1 = [{"criterion": "1. multiply_numbers(3, 4) == 12.", "oracle_file": "test_oracle_001.py", "target_path": "tests/test_oracle_001.py", "rationale": "derivable value"}]
			manifest2 = [{"criterion": "2. Code is clean.", "oracle_file": None, "rationale": "judgment call"}]
			specs = [
				(0, "", {"test_oracle_001.py": "from add import multiply_numbers\n\n\ndef test_oracle_x():\n    assert multiply_numbers(3, 4) == 12\n", draft_acceptance_oracles.MANIFEST_FILENAME: json.dumps(manifest1)}, "", False),
				(0, "", {draft_acceptance_oracles.MANIFEST_FILENAME: json.dumps(manifest2)}, "", False),
			]
			prompts = []
			original_pi_invocation = harness_adapters.PiAdapter.invocation

			def capture_pi_invocation(self, *a, **kw):
				prompts.append(kw["prompt"])
				return original_pi_invocation(self, *a, **kw)

			with mock.patch.object(harness_adapters.PiAdapter, "invocation", autospec=True, side_effect=capture_pi_invocation), \
				mock.patch.object(draft_acceptance_oracles, "run_pi_bounded", side_effect=scripted_pi_sequence(specs, draft_dir_base)):
				exit_code = draft_acceptance_oracles.run_draft_oracles(
					workspace, criteria_path, out_dir, evidence_path, timeout_minutes=2, ecosystem="python",
				)

			self.assertEqual(exit_code, 0)
			written = json.loads((out_dir / draft_acceptance_oracles.MANIFEST_FILENAME).read_text())
			self.assertIsNotNone(written[0]["oracle_file"])
			self.assertTrue(written[0]["oracle_file"].startswith("test_oracle_"))
			self.assertIsNone(written[1]["oracle_file"])
			# Every prompt sent to the model was the Python variant, never Go's.
			for prompt in prompts:
				self.assertIn("NO pytest, NO unittest", prompt)
				self.assertNotIn("oracle_NNN_test.go", prompt)


class PriorImportsTests(unittest.TestCase):
	"""Each criterion is drafted in its own isolated pi invocation, so
	without sharing imports across them, two criteria naming the same
	function could each guess a different module (live defect, 2026-09-24:
	4 separately-drafted criteria produced 4 different guessed modules for
	the same two functions, quarantined on diff_scope)."""

	def test_extract_python_imports_reads_from_and_plain_imports(self):
		with tempfile.TemporaryDirectory() as directory:
			path = Path(directory) / "test_oracle_001.py"
			path.write_text(
				"from sub import subtract_numbers\n"
				"import math\n\n\n"
				"def test_oracle_x():\n"
				"    assert subtract_numbers(5, 2) == 3\n"
			)
			lines = draft_acceptance_oracles.extract_python_imports(path)
		self.assertIn("from sub import subtract_numbers", lines)
		self.assertIn("import math", lines)

	def test_extract_python_imports_returns_empty_for_unparseable_file(self):
		with tempfile.TemporaryDirectory() as directory:
			path = Path(directory) / "broken.py"
			path.write_text("def bad(:\n")
			self.assertEqual(draft_acceptance_oracles.extract_python_imports(path), [])

	def test_build_prompt_includes_prior_imports_section(self):
		with tempfile.TemporaryDirectory() as directory:
			workspace = Path(directory)
			init_repo(workspace)
			prompt, _ = draft_acceptance_oracles.build_prompt(
				["1. A."], workspace, workspace / ".oracle-draft",
				prior_imports=["from sub import subtract_numbers"], ecosystem="python",
			)
		self.assertIn("Imports already used by oracles drafted earlier", prompt)
		self.assertIn("from sub import subtract_numbers", prompt)

	def test_run_draft_oracles_python_forwards_earlier_imports_to_later_criteria(self):
		# Criterion 1 drafts subtract_numbers from "sub"; criterion 2's own
		# prompt must be told to reuse that module, not guess "add" or
		# "divide" for a same-named or related function.
		with tempfile.TemporaryDirectory() as directory:
			workspace = Path(directory) / "repo"
			workspace.mkdir()
			init_repo(workspace)
			criteria_path = Path(directory) / "criteria.md"
			criteria_path.write_text("1. subtract_numbers(5, 2) == 3.\n2. divide_numbers(6, 2) == 3.\n")
			out_dir = Path(directory) / "oracles"
			evidence_path = Path(directory) / "evidence.json"
			draft_dir_base = workspace / draft_acceptance_oracles.DRAFT_RELATIVE_DIR

			manifest1 = [{"criterion": "1. subtract_numbers(5, 2) == 3.", "oracle_file": "test_oracle_001.py", "target_path": "tests/test_oracle_001.py", "rationale": "derivable value"}]
			manifest2 = [{"criterion": "2. divide_numbers(6, 2) == 3.", "oracle_file": "test_oracle_002.py", "target_path": "tests/test_oracle_002.py", "rationale": "derivable value"}]
			specs = [
				(0, "", {"test_oracle_001.py": "from sub import subtract_numbers\n\n\ndef test_oracle_x():\n    assert subtract_numbers(5, 2) == 3\n", draft_acceptance_oracles.MANIFEST_FILENAME: json.dumps(manifest1)}, "", False),
				(0, "", {"test_oracle_002.py": "from div import divide_numbers\n\n\ndef test_oracle_y():\n    assert divide_numbers(6, 2) == 3\n", draft_acceptance_oracles.MANIFEST_FILENAME: json.dumps(manifest2)}, "", False),
			]
			prompts = []
			original_pi_invocation = harness_adapters.PiAdapter.invocation

			def capture_pi_invocation(self, *a, **kw):
				prompts.append(kw["prompt"])
				return original_pi_invocation(self, *a, **kw)

			with mock.patch.object(harness_adapters.PiAdapter, "invocation", autospec=True, side_effect=capture_pi_invocation), \
				mock.patch.object(draft_acceptance_oracles, "run_pi_bounded", side_effect=scripted_pi_sequence(specs, draft_dir_base)):
				exit_code = draft_acceptance_oracles.run_draft_oracles(
					workspace, criteria_path, out_dir, evidence_path, timeout_minutes=2, ecosystem="python",
				)

			self.assertEqual(exit_code, 0)
			self.assertEqual(len(prompts), 2)
			# The first criterion's prompt has nothing to reuse yet.
			self.assertNotIn("Imports already used", prompts[0])
			# The second criterion's prompt is told to reuse "sub", the
			# module the first criterion's own drafted oracle actually used.
			self.assertIn("Imports already used", prompts[1])
			self.assertIn("from sub import subtract_numbers", prompts[1])

	def test_run_draft_oracles_go_ecosystem_never_accumulates_imports(self):
		# Go oracles don't need this (their package path is explicit in
		# target_path); confirm the Go path never even looks at import
		# lines, so a Go draft's prompt is unaffected by this change.
		with tempfile.TemporaryDirectory() as directory:
			workspace = Path(directory) / "repo"
			workspace.mkdir()
			init_repo(workspace)
			criteria_path = Path(directory) / "criteria.md"
			criteria_path.write_text("1. First.\n2. Second.\n")
			out_dir = Path(directory) / "oracles"
			evidence_path = Path(directory) / "evidence.json"
			draft_dir_base = workspace / draft_acceptance_oracles.DRAFT_RELATIVE_DIR

			manifest1 = [{"criterion": "1. First.", "oracle_file": "oracle_001_test.go", "rationale": "derivable value"}]
			manifest2 = [{"criterion": "2. Second.", "oracle_file": None, "rationale": "judgment call"}]
			specs = [
				(0, "", {"oracle_001_test.go": "package p\nimport \"testing\"\nfunc TestOracleX(t *testing.T) {}\n", draft_acceptance_oracles.MANIFEST_FILENAME: json.dumps(manifest1)}, "", False),
				(0, "", {draft_acceptance_oracles.MANIFEST_FILENAME: json.dumps(manifest2)}, "", False),
			]
			prompts = []
			original_pi_invocation = harness_adapters.PiAdapter.invocation

			def capture_pi_invocation(self, *a, **kw):
				prompts.append(kw["prompt"])
				return original_pi_invocation(self, *a, **kw)

			with mock.patch.object(harness_adapters.PiAdapter, "invocation", autospec=True, side_effect=capture_pi_invocation), \
				mock.patch.object(draft_acceptance_oracles, "run_pi_bounded", side_effect=scripted_pi_sequence(specs, draft_dir_base)):
				draft_acceptance_oracles.run_draft_oracles(
					workspace, criteria_path, out_dir, evidence_path, timeout_minutes=2,
				)

			for prompt in prompts:
				self.assertNotIn("Imports already used", prompt)
