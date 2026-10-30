"""Offline tests of live_challenge.py's own verdict: it must FAIL when no
challenger job ran, however the record looks otherwise."""

import json
import os
import pathlib
import subprocess
import sys
import tempfile
import unittest

SCRIPTS = pathlib.Path(__file__).resolve().parent.parent
sys.path.insert(0, str(SCRIPTS))
import live_challenge  # noqa: E402


class ChallengeProblems(unittest.TestCase):
	def test_no_record_means_no_job_ran(self):
		self.assertTrue(live_challenge.challenge_problems({"state": "accepted"}))

	def test_a_missing_role_is_not_a_job(self):
		run = {"challenge": {"verdict": "no_verdict", "cause": live_challenge.NO_ROLE}}
		self.assertTrue(live_challenge.challenge_problems(run))

	def test_a_no_verdict_without_the_jobs_snapshot_is_not_a_job(self):
		run = {"challenge": {"verdict": "no_verdict", "cause": "the challenger job's step failed: x"}}
		self.assertTrue(live_challenge.challenge_problems(run))

	def test_a_no_verdict_with_the_snapshot_is_a_job(self):
		run = {"challenge": {"verdict": "no_verdict", "cause": "no held challenge file gave a green or a red verdict", "snapshot_sha256": "ab" * 32}}
		self.assertEqual(live_challenge.challenge_problems(run), [])

	def test_green_and_red_are_jobs(self):
		for verdict in ("green", "red"):
			self.assertEqual(live_challenge.challenge_problems({"challenge": {"verdict": verdict}}), [])

	def test_an_unknown_verdict_fails(self):
		self.assertTrue(live_challenge.challenge_problems({"challenge": {"verdict": "maybe"}}))


class FailsWhenNoChallengerRan(unittest.TestCase):
	"""The whole script against a stub factoryd whose accepted run has no
	challenge record: exit status 1 and a recorded fail."""

	def test_exit_status_is_nonzero(self):
		with tempfile.TemporaryDirectory() as tmp:
			tmp = pathlib.Path(tmp)
			data = tmp / "data"
			rid = "stub-20260101-000000"
			(data / "requests" / rid).mkdir(parents=True)
			(data / "requests" / rid / "request.json").write_text(json.dumps({"state": "pr_review", "tickets": [{"run_id": "run1"}]}))
			(data / "runs" / "run1").mkdir(parents=True)
			(data / "runs" / "run1" / "run.json").write_text(json.dumps({"id": "run1", "state": "accepted"}))
			stub = tmp / "factoryd"
			stub.write_text(f"#!/bin/sh\ncase \"$1\" in version) echo 'factoryd version stub';; submit) echo {rid};; esac\n")
			stub.chmod(0o755)
			results = tmp / "results.jsonl"
			env = dict(os.environ, FACTORYD_BIN=str(stub), LIVE_CHALLENGE_CONFIG="stub", LIVE_CHALLENGE_DATA_DIR=str(data),
				LIVE_CHALLENGE_ROOT=str(tmp / "repos"), LIVE_CHALLENGE_RESULTS_FILE=str(results))
			(tmp / "repos").mkdir()
			p = subprocess.run([sys.executable, str(SCRIPTS / "live_challenge.py")], env=env, capture_output=True, text=True)
			self.assertEqual(p.returncode, 1, p.stdout + p.stderr)
			row = json.loads(results.read_text().splitlines()[-1])
			self.assertEqual(row["outcome"], "fail")
			self.assertIn("no challenger job ran", " ".join(row["problems"]))

	def test_a_run_with_a_verdict_passes(self):
		with tempfile.TemporaryDirectory() as tmp:
			tmp = pathlib.Path(tmp)
			data = tmp / "data"
			rid = "stub-20260101-000001"
			(data / "requests" / rid).mkdir(parents=True)
			(data / "requests" / rid / "request.json").write_text(json.dumps({"state": "pr_review", "tickets": [{"run_id": "run1"}]}))
			(data / "runs" / "run1").mkdir(parents=True)
			(data / "runs" / "run1" / "run.json").write_text(json.dumps({"id": "run1", "state": "accepted", "challenge": {"verdict": "green"}}))
			stub = tmp / "factoryd"
			stub.write_text(f"#!/bin/sh\ncase \"$1\" in version) echo 'factoryd version stub';; submit) echo {rid};; esac\n")
			stub.chmod(0o755)
			env = dict(os.environ, FACTORYD_BIN=str(stub), LIVE_CHALLENGE_CONFIG="stub", LIVE_CHALLENGE_DATA_DIR=str(data),
				LIVE_CHALLENGE_ROOT=str(tmp / "repos"), LIVE_CHALLENGE_RESULTS_FILE=str(tmp / "r.jsonl"))
			(tmp / "repos").mkdir()
			p = subprocess.run([sys.executable, str(SCRIPTS / "live_challenge.py")], env=env, capture_output=True, text=True)
			self.assertEqual(p.returncode, 0, p.stdout + p.stderr)


if __name__ == "__main__":
	unittest.main()
