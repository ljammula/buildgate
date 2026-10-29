"""Offline tests of live_oversize.py's verdict."""

import json
import pathlib
import sys
import tempfile
import unittest

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parent.parent))
import live_oversize  # noqa: E402

MOVED = {"state": "plan_review", "history": [{"from": "building", "to": "planning"}]}


def run_dir(tmp, name, progress=(), run=None):
	d = pathlib.Path(tmp) / name
	d.mkdir()
	(d / "progress.jsonl").write_text("\n".join(json.dumps(e) for e in progress))
	(d / "run.json").write_text(json.dumps(run or {}))
	return d


class OversizeProblems(unittest.TestCase):
	def test_routed_back_unreviewed_is_clean(self):
		with tempfile.TemporaryDirectory() as tmp:
			d = run_dir(tmp, "r1", [{"stage": "verify", "event": "start"}])
			self.assertEqual(live_oversize.oversize_problems(MOVED, [d]), [])

	def test_no_run_proves_nothing(self):
		self.assertTrue(live_oversize.oversize_problems(MOVED, []))

	def test_a_request_that_never_moved_fails(self):
		req = {"state": "pr_review", "history": [{"from": "building", "to": "pr_review"}]}
		with tempfile.TemporaryDirectory() as tmp:
			self.assertEqual(len(live_oversize.oversize_problems(req, [run_dir(tmp, "r1")])), 2)

	def test_a_reviewed_run_fails(self):
		with tempfile.TemporaryDirectory() as tmp:
			d = run_dir(tmp, "r1", [{"stage": "review", "event": "start"}])
			self.assertTrue(live_oversize.oversize_problems(MOVED, [d]))

	def test_a_pull_request_fails(self):
		with tempfile.TemporaryDirectory() as tmp:
			d = run_dir(tmp, "r1", run={"pr_url": "https://x/pull/1"})
			self.assertTrue(live_oversize.oversize_problems(MOVED, [d]))


if __name__ == "__main__":
	unittest.main()
