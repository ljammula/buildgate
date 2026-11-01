"""Offline tests of scripts/test-json.py over recorded `go test -json` events:
which tests count as passed, and what a failed job prints."""

import json
import os
import subprocess
import tempfile
import unittest

SCRIPT = os.path.join(os.path.dirname(os.path.dirname(os.path.abspath(__file__))), "test-json.py")


def event(action, test=None, output=None, package="p"):
	e = {"Action": action, "Package": package}
	if test:
		e["Test"] = test
	if output is not None:
		e["Output"] = output
	return json.dumps(e)


STREAM = [
	event("start"),
	event("run", "TestPass"),
	event("output", "TestPass", "=== RUN   TestPass\n"),
	event("output", "TestPass", "    a_test.go:3: quiet log\n"),
	event("output", "TestPass", "--- PASS: TestPass (0.00s)\n"),
	event("pass", "TestPass"),
	event("run", "TestSkip"),
	event("output", "TestSkip", "--- SKIP: TestSkip (0.00s)\n"),
	event("skip", "TestSkip"),
	event("run", "TestFail"),
	event("run", "TestFail/sub"),
	event("output", "TestFail/sub", "    a_test.go:5: boom\n"),
	event("pass", "TestFail/ok"),
	event("fail", "TestFail/sub"),
	event("output", "TestFail", "--- FAIL: TestFail (0.00s)\n"),
	event("fail", "TestFail"),
	# A subprocess's own `--- PASS:` text is output, never an event.
	event("run", "TestRunsGoTest"),
	event("output", "TestRunsGoTest", "--- PASS: TestNested (0.00s)\n"),
	event("fail", "TestRunsGoTest"),
	event("run", "TestCutOff"),
	event("output", "TestCutOff", "panic: test timed out\n"),
	"# go command stderr, not an event",
	event("output", None, "FAIL\n"),
	event("fail"),
]


def run(*args):
	return subprocess.run(["python3", SCRIPT, *args], capture_output=True, text=True, timeout=20)


class TestJSONTest(unittest.TestCase):
	def setUp(self):
		self.dir = tempfile.TemporaryDirectory()
		self.path = os.path.join(self.dir.name, "job.json")
		with open(self.path, "w") as f:
			f.write("\n".join(STREAM) + "\n")

	def tearDown(self):
		self.dir.cleanup()

	def test_passed_lists_top_level_passes_only(self):
		out = run("passed", self.path)
		self.assertEqual(0, out.returncode, out.stderr)
		self.assertEqual(["TestPass"], out.stdout.split())

	def test_passed_reads_every_job(self):
		other = os.path.join(self.dir.name, "other.json")
		with open(other, "w") as f:
			f.write(event("pass", "TestElsewhere", package="q") + "\n")
		self.assertEqual(["TestElsewhere", "TestPass"], run("passed", self.path, other).stdout.split())

	def test_a_name_skipped_in_one_package_does_not_count_though_another_passed_it(self):
		other = os.path.join(self.dir.name, "other.json")
		with open(other, "w") as f:
			f.write(event("pass", "TestSkip", package="q") + "\n")
		self.assertEqual(["TestPass"], run("passed", self.path, other).stdout.split())

	def test_failures_keeps_failing_and_unfinished_tests_and_lines_outside_a_test(self):
		out = run("failures", self.path).stdout
		for kept in ("boom", "--- FAIL: TestFail", "TestNested", "panic: test timed out", "# go command stderr", "FAIL\n"):
			self.assertIn(kept, out)
		for dropped in ("quiet log", "--- PASS: TestPass", "--- SKIP"):
			self.assertNotIn(dropped, out)

	def test_text_is_every_line(self):
		out = run("text", self.path).stdout
		for kept in ("quiet log", "--- SKIP", "boom", "# go command stderr"):
			self.assertIn(kept, out)

	def test_an_unknown_verb_is_refused(self):
		self.assertNotEqual(0, run("count", self.path).returncode)


if __name__ == "__main__":
	unittest.main()
