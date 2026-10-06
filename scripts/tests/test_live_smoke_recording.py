"""Offline test for scripts/live-smoke.sh's result-recording logic:
one JSON line per fixture run appended to LIVE_SMOKE_RESULTS_FILE.

Drives the real live-smoke.sh against a throwaway git fixture repo and a
stub FACTORYD_BIN (scripts/tests/fixtures/stub_factoryd.sh) -- no Docker, no
real model route, no real fixture repos. Run:
python3 -m unittest discover -s scripts/tests -p 'test_live_smoke_recording.py'
(or `make live-smoke-test`).
"""
import json
import os
import subprocess
import tempfile
import unittest

REPO_ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), "..", ".."))
LIVE_SMOKE = os.path.join(REPO_ROOT, "scripts", "live-smoke.sh")
STUB_FACTORYD = os.path.join(os.path.dirname(__file__), "fixtures", "stub_factoryd.sh")


class LiveSmokeRecordingTest(unittest.TestCase):
    def _run(self, state, no_workflow=False):
        fixture_label = "live-smoke-test-fixture"
        with tempfile.TemporaryDirectory() as tmp:
            # live-smoke.sh records basename(source_repo) as the fixture
            # field (see its own repo_name var, shared with its PASS/FAIL
            # echo lines) -- named after fixture_label here so the recorded
            # row is unambiguous without duplicating that naming choice.
            source_repo = os.path.join(tmp, fixture_label)
            os.makedirs(source_repo)
            subprocess.run(["git", "init", "-q", source_repo], check=True)
            subprocess.run(["git", "-C", source_repo, "config", "user.email", "t@example.com"], check=True)
            subprocess.run(["git", "-C", source_repo, "config", "user.name", "t"], check=True)
            with open(os.path.join(source_repo, "README.md"), "w") as f:
                f.write("x\n")
            subprocess.run(["git", "-C", source_repo, "add", "."], check=True)
            subprocess.run(["git", "-C", source_repo, "commit", "-q", "-m", "init"], check=True)

            spec_file = os.path.join(tmp, "spec.md")
            with open(spec_file, "w") as f:
                f.write("# spec\n")

            results_file = os.path.join(tmp, "results.jsonl")
            env = dict(os.environ)
            env["FACTORYD_BIN"] = STUB_FACTORYD
            env["LIVE_SMOKE_RESULTS_FILE"] = results_file
            env["LIVE_SMOKE_FIXTURES"] = "%s:%s:%s" % (source_repo, spec_file, fixture_label)
            env["STUB_FACTORYD_STATE"] = state
            # The stub records a temporal_workflow_id unless told not to.
            env.pop("LIVE_SMOKE_TEMPORAL", None)
            if no_workflow:
                env["STUB_FACTORYD_NO_WORKFLOW"] = "1"
            env["HOME"] = tmp  # keep scratch/cache writes inside the temp dir

            proc = subprocess.run(
                ["sh", LIVE_SMOKE], env=env, cwd=REPO_ROOT,
                capture_output=True, text=True, timeout=60,
            )
            with open(results_file) as f:
                lines = [json.loads(l) for l in f if l.strip()]
            return proc, lines, fixture_label

    def test_a_run_that_did_not_use_temporal_fails(self):
        proc, lines, _ = self._run("accepted", no_workflow=True)
        self.assertNotEqual(0, proc.returncode, proc.stdout + proc.stderr)
        self.assertIn("did the run reach Temporal?", proc.stdout + proc.stderr)
        self.assertNotIn("path", lines[0])

    def test_accepted_run_is_recorded_and_passes(self):
        proc, lines, fixture_label = self._run("accepted")
        self.assertEqual(0, proc.returncode, proc.stdout + proc.stderr)
        self.assertEqual(1, len(lines), lines)
        row = lines[0]
        self.assertEqual(fixture_label, row["fixture"])
        self.assertNotIn("path", row)
        self.assertEqual("accepted", row["outcome"])
        self.assertEqual("accept", row["expected"])
        self.assertEqual("factoryd stub-test-version", row["factoryd_version"])
        self.assertIsInstance(row["duration_s"], int)
        self.assertGreaterEqual(row["duration_s"], 0)
        self.assertTrue(row["date"])
        self.assertTrue(row["git_sha"])

    def test_unexpected_state_is_recorded_and_fails(self):
        proc, lines, _ = self._run("halted")
        self.assertNotEqual(0, proc.returncode)
        self.assertEqual(1, len(lines), lines)
        self.assertEqual("halted", lines[0]["outcome"])
        self.assertEqual("accept", lines[0]["expected"])

    def test_results_flag_renders_recorded_runs_as_a_table(self):
        proc, lines, fixture_label = self._run("accepted")
        self.assertEqual(0, proc.returncode)
        # _run's results file lives in a TemporaryDirectory already cleaned
        # up by the time it returns, so re-materialize the recorded rows
        # into a fresh file and read them back via --results.
        with tempfile.TemporaryDirectory() as tmp:
            results_file = os.path.join(tmp, "results.jsonl")
            with open(results_file, "w") as f:
                for row in lines:
                    f.write(json.dumps(row) + "\n")
            out = subprocess.run(
                ["sh", LIVE_SMOKE, "--results", "5"],
                env={**os.environ, "LIVE_SMOKE_RESULTS_FILE": results_file},
                cwd=REPO_ROOT, capture_output=True, text=True, timeout=10,
            )
            self.assertEqual(0, out.returncode, out.stdout + out.stderr)
            self.assertIn(fixture_label, out.stdout)
            self.assertIn("outcome", out.stdout)  # header row


if __name__ == "__main__":
    unittest.main()
