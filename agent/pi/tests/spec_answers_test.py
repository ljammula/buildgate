"""The read-back answers the operator approved with the spec reach every
reader of the approved spec inside the build: factoryd writes them after the
criteria in the criteria file (requestdriver.AnsweredReadBackBlock), and the
spec-conformity reviewers (in-build, standalone and combined) and the planner
get them labelled as the operator's answers to questions about the spec."""

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
conformity_review = combined_review.conformity_review
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
			self.assertEqual(build_app.read_spec_answers(path), ANSWERS)

	def test_a_criteria_file_without_answers_reads_as_before(self):
		with tempfile.TemporaryDirectory() as directory:
			path = _criteria_path(directory, "1. Foo\n# a comment\n2. Bar\n")
			self.assertEqual(build_app.read_acceptance_criteria(path), ["1. Foo", "2. Bar"])
			self.assertEqual(build_app.read_spec_answers(path), "")
			self.assertEqual(build_app.read_spec_answers(None), "")


class ConformityPromptTests(unittest.TestCase):
	def test_the_answers_follow_the_criteria_labelled_as_the_operators(self):
		prompt = build_app.spec_conformity_prompt(["1. Keys are honoured"], "abc123", answers=ANSWERS)
		criteria_at = prompt.index("1. Keys are honoured")
		label_at = prompt.index("The operator answered these questions about the spec at spec review and approved the answers with it.")
		self.assertLess(criteria_at, label_at)
		self.assertLess(label_at, prompt.index("24 hours."))
		self.assertIn("They are not criteria and get no verdict of their own", prompt)

	def test_no_answers_leaves_the_prompt_unchanged(self):
		self.assertEqual(
			build_app.spec_conformity_prompt(["1. Foo"], "abc123", answers=""),
			build_app.spec_conformity_prompt(["1. Foo"], "abc123"),
		)
		self.assertNotIn("answered these questions", build_app.spec_conformity_prompt(["1. Foo"], "abc123"))

	def test_the_combined_review_gives_task_a_the_answers_after_its_criteria(self):
		prompt = combined_review.combined_review_prompt(["1. Keys are honoured"], "ticket", "abc123", answers=ANSWERS)
		task_a = prompt.index("=== Task A")
		task_b = prompt.index("=== Task B")
		self.assertLess(task_a, prompt.index("24 hours."))
		self.assertLess(prompt.index("24 hours."), task_b)


def _capture_turns(prompts: list[str]):
	def turn(workspace, *, prompt, **kwargs):
		prompts.append(prompt)
		return conformity_review.build_app.ReviewTurn(text="", error="no answer")
	return turn


class ReviewRunTests(unittest.TestCase):
	def test_the_standalone_conformity_review_reads_the_answers_from_the_criteria_file(self):
		prompts: list[str] = []
		with tempfile.TemporaryDirectory() as directory, tempfile.TemporaryDirectory() as spec_dir:
			root = Path(directory)
			_init_repo(root)
			path = _criteria_path(spec_dir)
			# The module conformity_review itself calls: another test file may
			# have loaded its own copy of build_app under the same name.
			with mock.patch.object(conformity_review.build_app, "run_review_turn", side_effect=_capture_turns(prompts)):
				evidence = conformity_review.run_conformity_review(root, criteria_path=path, conformity_policy="required")
		self.assertEqual([v["criterion"] for v in evidence["review_verdicts"]], ["1. Keys are honoured", "2. Expired keys are refused"])
		self.assertTrue(prompts)
		self.assertIn("24 hours.", prompts[0])

	def test_the_combined_review_and_its_conformity_retry_read_the_answers(self):
		prompts: list[str] = []
		with tempfile.TemporaryDirectory() as directory, tempfile.TemporaryDirectory() as spec_dir:
			root = Path(directory)
			_init_repo(root)
			path = _criteria_path(spec_dir)
			spec_path = Path(spec_dir) / "ticket.md"
			spec_path.write_text("Add keys.\n")
			answers = iter([
				build_app.ReviewTurn(text='{"findings": []}', error=""),
				build_app.ReviewTurn(text="", error="no answer"),
			])

			def turn(workspace, *, prompt, **kwargs):
				prompts.append(prompt)
				return next(answers)

			with mock.patch.object(build_app, "run_review_turn", side_effect=turn):
				combined_review.run_combined_review(root, criteria_path=path, spec_path=spec_path)
		self.assertEqual(len(prompts), 2)
		for prompt in prompts:
			self.assertIn("24 hours.", prompt)


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
