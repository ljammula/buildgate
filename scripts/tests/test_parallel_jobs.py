"""Offline tests of scripts/parallel-jobs.sh: its bounds and its refusals.
The count itself depends on the machine, so only what holds on any machine
is asserted."""

import os
import subprocess
import unittest

SCRIPT = os.path.join(os.path.dirname(os.path.dirname(os.path.abspath(__file__))), "parallel-jobs.sh")


def run(*args):
	return subprocess.run(["sh", SCRIPT, *args], capture_output=True, text=True, timeout=20)


class ParallelJobsTest(unittest.TestCase):
	def test_the_count_is_between_one_and_the_maximum(self):
		for per_job, most in ((1, 1), (6, 4), (1, 3)):
			out = run(str(per_job), str(most))
			self.assertEqual(0, out.returncode, out.stderr)
			self.assertTrue(1 <= int(out.stdout) <= most, out.stdout)

	def test_a_job_larger_than_any_machine_still_gets_one(self):
		out = run("1000000", "8")
		self.assertEqual("1", out.stdout.strip(), out.stderr)

	def test_free_prints_a_number(self):
		out = run("--free")
		self.assertEqual(0, out.returncode, out.stderr)
		self.assertGreaterEqual(int(out.stdout), 0)

	def test_bad_arguments_are_refused(self):
		for args in ((), ("6",), ("six", "4"), ("0", "4"), ("6", "0")):
			self.assertNotEqual(0, run(*args).returncode, args)


if __name__ == "__main__":
	unittest.main()
