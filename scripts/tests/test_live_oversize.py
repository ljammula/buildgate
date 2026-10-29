"""Offline tests of live_oversize.py's verdict."""

import pathlib
import sys
import unittest

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parent.parent))
import live_oversize  # noqa: E402

MOVED = {"state": "plan_review", "history": [{"from": "building", "to": "planning"}]}


class OversizeProblems(unittest.TestCase):
	def test_routed_back_unreviewed_is_clean(self):
		self.assertEqual(live_oversize.oversize_problems(MOVED, {}, [{"stage": "verify", "event": "start"}]), [])

	def test_a_request_that_never_moved_fails(self):
		req = {"state": "pr_review", "history": [{"from": "building", "to": "pr_review"}]}
		self.assertEqual(len(live_oversize.oversize_problems(req, {}, [])), 2)

	def test_a_reviewed_run_fails(self):
		self.assertTrue(live_oversize.oversize_problems(MOVED, {}, [{"stage": "review", "event": "start"}]))

	def test_a_pull_request_fails(self):
		self.assertTrue(live_oversize.oversize_problems(MOVED, {"pr_url": "https://x/pull/1"}, []))


if __name__ == "__main__":
	unittest.main()
