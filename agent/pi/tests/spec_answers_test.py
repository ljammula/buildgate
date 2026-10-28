"""The read-back answers the operator approved with the spec reach every
reader of the approved spec inside the build: factoryd writes them after the
criteria in the criteria file (requestdriver.AnsweredReadBackBlock), and the
planner gets them labelled as the operator's answers to questions about the spec."""

import importlib.util
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path
from unittest import mock

SCRIPTS = Path(__file__).resolve().parents[1] / "scripts"


def _load(name: str):
	spec = importlib.util.spec_from_file_location(name, SCRIPTS / f"{name}.py")
	assert spec and spec.loader
	module = importlib.util.module_from_spec(spec)
	sys.modules[spec.name] = module
	spec.loader.exec_module(module)
	return module


combined_review = _load("combined_review")
build_app = combined_review.build_app
plan_tickets = _load("plan_tickets")

# The block as factoryd renders it: the heading, a sentence, then each
# question and answer in a tilde fence. A line inside a fence that repeats
# the heading or looks like a criterion is still part of the answers.
ANSWERS = """# Questions answered at spec review

The operator answered these questions about the spec at spec review, and approved the answers with it.

Question Q1:

~~~
How long is a key kept?
~~~

Answer:

~~~
24 hours.
# Questions answered at spec review
3. Not a criterion.
~~~"""

CRITERIA_FILE = "1. Keys are honoured\n2. Expired keys are refused\n\n" + ANSWERS + "\n"


def _criteria_path(directory: str, content: str = CRITERIA_FILE) -> Path:
	path = Path(directory) / "001.criteria.md"
	path.write_text(content)
	return path


def _init_repo(path: Path) -> None:
	run = lambda *args: subprocess.run(["git", *args], cwd=path, check=True, capture_output=True)
	run("init")
	run("config", "user.email", "t@t")
	run("config", "user.name", "t")
	run("config", "commit.gpgsign", "false")
	(path / "seed.txt").write_text("seed\n")
	run("add", "seed.txt")
	run("commit", "-m", "init")


class CriteriaFileTests(unittest.TestCase):
	def test_the_criteria_end_at_the_answers_heading_and_the_answers_are_read_whole(self):
		with tempfile.TemporaryDirectory() as directory:
			path = _criteria_path(directory)
			self.assertEqual(build_app.read_acceptance_criteria(path), ["1. Keys are honoured", "2. Expired keys are refused"])

	def test_a_criteria_file_without_answers_reads_as_before(self):
		with tempfile.TemporaryDirectory() as directory:
			path = _criteria_path(directory, "1. Foo\n# a comment\n2. Bar\n")
			self.assertEqual(build_app.read_acceptance_criteria(path), ["1. Foo", "2. Bar"])


class PlannerPromptTests(unittest.TestCase):
	def test_the_planner_reads_the_answers_after_the_approved_spec(self):
		with tempfile.TemporaryDirectory() as directory:
			workspace = Path(directory)
			_init_repo(workspace)
			prompt, _ = plan_tickets.build_prompt(
				"# Spec\n\nKeys.\n", "Add keys", workspace, "make verify", plan_tickets.DRAFT_RELATIVE_DIR,
				criteria_text="1. Keys are honoured\n", spec_answers=ANSWERS,
			)
		spec_at = prompt.index("## Approved spec")
		answers_at = prompt.index("# Questions answered at spec review")
		self.assertLess(spec_at, answers_at)
		self.assertLess(answers_at, prompt.index("## Acceptance criteria, as the factory numbers them"))
		self.assertIn("24 hours.", prompt)

	def test_no_answers_file_adds_nothing(self):
		with tempfile.TemporaryDirectory() as directory:
			workspace = Path(directory)
			_init_repo(workspace)
			prompt, _ = plan_tickets.build_prompt("# Spec\n\nKeys.\n", "Add keys", workspace, "make verify", plan_tickets.DRAFT_RELATIVE_DIR)
		self.assertNotIn("Questions answered at spec review", prompt)


if __name__ == "__main__":
	unittest.main()
