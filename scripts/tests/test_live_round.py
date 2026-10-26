"""Offline tests for scripts/live_round.py's own logic: where a fixture
comment lands, when a thread counts as resolved, and the recorded share. No
factoryd, no GitHub."""

import pathlib
import sys
import unittest

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parents[1]))
import live_round  # noqa: E402

DIFF = """diff --git a/main.go b/main.go
--- a/main.go
+++ b/main.go
@@ -304,6 +304,14 @@ func readDurable() {
 }
 
+func handleHealthz(w http.ResponseWriter, r *http.Request) {
+	if r.Method != http.MethodGet {
+		w.WriteHeader(http.StatusMethodNotAllowed)
+		return
+	}
+}
+
 func other() {
-	w.WriteHeader(http.StatusMethodNotAllowed)
+	w.WriteHeader(http.StatusOK)
"""


class AddedLine(unittest.TestCase):
	def test_finds_the_new_file_line_of_the_first_added_match(self):
		self.assertEqual(live_round.added_line(DIFF, "http.StatusMethodNotAllowed"), 308)

	def test_ignores_removed_and_context_lines(self):
		self.assertEqual(live_round.added_line(DIFF, "http.StatusOK"), 314)
		self.assertIsNone(live_round.added_line(DIFF, "func other"))


class ThreadOutcomes(unittest.TestCase):
	def test_resolved_needs_a_reply_naming_a_commit_of_the_pull_request(self):
		comments = [
			{"id": 1, "body": "please fix"},
			{"id": 2, "in_reply_to_id": 1, "body": "Attempted in corrective round 1 of 3: a change was built and not pushed"},
			{"id": 3, "in_reply_to_id": 1, "body": "Addressed in cafef00d."},
			{"id": 4, "body": "another"},
			{"id": 5, "in_reply_to_id": 4, "body": "Addressed in deadbeef."},
		]
		got = live_round.thread_outcomes(comments, [1, 4], ["cafef00d" + "0" * 32])
		self.assertEqual(got, {1: (True, 1), 4: (False, 0)})


class Replays(unittest.TestCase):
	def test_a_reply_naming_a_replayed_commit_resolves_through_its_replay(self):
		replays = live_round.replayed_commits([
			[{"original": "a" * 40, "replayed": "b" * 40, "original_patch_id": "p" * 40, "replayed_patch_id": "p" * 40}],
			[{"original": "b" * 40, "replayed": "c" * 40, "original_patch_id": "p" * 40, "replayed_patch_id": "p" * 40}],
		])
		self.assertEqual(replays["a" * 40], "c" * 40)
		comments = [{"id": 1, "body": "fix"}, {"id": 2, "in_reply_to_id": 1, "body": "Addressed in aaaaaaaa."}]
		self.assertEqual(live_round.thread_outcomes(comments, [1], ["c" * 40], replays), {1: (True, 0)})
		self.assertEqual(live_round.thread_outcomes(comments, [1], ["c" * 40]), {1: (False, 0)})

	def test_a_pair_whose_patch_ids_differ_is_not_a_replay(self):
		replays = live_round.replayed_commits([[{"original": "a" * 40, "replayed": "b" * 40, "original_patch_id": "p" * 40, "replayed_patch_id": "q" * 40}]])
		self.assertEqual(replays, {})


class Stacked(unittest.TestCase):
	def test_finds_ticket_two_stacked_on_ticket_one(self):
		prs = [
			{"url": "u0", "headRefName": "factoryd/other-001-x", "baseRefName": "main"},
			{"url": "u2", "headRefName": "factoryd/a-long-request-bb8755bd3e1e", "baseRefName": "factoryd/a-long-request-798acadf69b6"},
			{"url": "u1", "headRefName": "factoryd/a-long-request-798acadf69b6", "baseRefName": "main"},
		]
		first, second = live_round.stacked_pull_requests(prs, [{"pr_url": "u1"}, {"pr_url": "u2"}])
		self.assertEqual((first["url"], second["url"]), ("u1", "u2"))

	def test_nothing_until_ticket_two_stacks_on_ticket_one(self):
		prs = [
			{"url": "u1", "headRefName": "factoryd/req-1-001-x", "baseRefName": "main"},
			{"url": "u2", "headRefName": "factoryd/req-1-002-x", "baseRefName": "main"},
		]
		self.assertEqual(live_round.stacked_pull_requests(prs, [{"pr_url": "u1"}, {"pr_url": "u2"}]), (None, None))
		self.assertEqual(live_round.stacked_pull_requests(prs, [{"pr_url": "u1"}, {}]), (None, None))
		self.assertEqual(live_round.stacked_pull_requests(prs, []), (None, None))

	def test_restack_rounds_are_the_pushed_accepted_ones(self):
		ticket = {"rounds": [
			{"kind": "restack", "outcome": "accepted", "pushed": True, "run_id": "r1"},
			{"kind": "restack", "outcome": "quarantined", "run_id": "r2"},
			{"outcome": "accepted", "pushed": True, "run_id": "review"},
		]}
		self.assertEqual([r["run_id"] for r in live_round.restack_rounds(ticket)], ["r1"])


class Origin(unittest.TestCase):
	def test_reads_owner_and_name_from_any_github_remote_form(self):
		for url in ("https://github.com/acme/todo-scratch.git", "https://github.com/acme/todo-scratch", "git@github.com:acme/todo-scratch.git", "ssh://git@github.com/acme/todo-scratch.git\n"):
			self.assertEqual(live_round.origin_slug(url), "acme/todo-scratch", url)

	def test_a_remote_that_is_not_github_has_none(self):
		self.assertIsNone(live_round.origin_slug("https://gitlab.com/acme/todo.git"))
		self.assertIsNone(live_round.origin_slug("/srv/git/todo.git"))


class StackedChecks(unittest.TestCase):
	def test_every_gate_must_have_passed(self):
		self.assertTrue(live_round.all_gates_passed({"gate_results": [{"check": "verify", "passed": True}, {"check": "code_review", "passed": True}]}))
		self.assertFalse(live_round.all_gates_passed({"gate_results": [{"check": "verify", "passed": True}, {"check": "code_review", "passed": False}]}))
		self.assertFalse(live_round.all_gates_passed({"gate_results": [{"check": "verify"}]}))
		self.assertFalse(live_round.all_gates_passed({"gate_results": []}))

	def test_the_factory_drafts_before_the_head_is_replaced(self):
		events = [
			{"event": "ready_for_review", "actor": {"login": "reviewer"}},
			{"event": "convert_to_draft", "actor": {"login": "factory"}},
			{"event": "head_ref_force_pushed", "actor": {"login": "factory"}},
		]
		self.assertTrue(live_round.drafted_before_push(events, "reviewer"))

	def test_a_draft_after_the_push_or_by_the_reviewer_does_not_count(self):
		late = [
			{"event": "ready_for_review", "actor": {"login": "reviewer"}},
			{"event": "head_ref_force_pushed", "actor": {"login": "factory"}},
			{"event": "convert_to_draft", "actor": {"login": "factory"}},
		]
		self.assertFalse(live_round.drafted_before_push(late, "reviewer"))
		by_reviewer = [
			{"event": "convert_to_draft", "actor": {"login": "reviewer"}},
			{"event": "head_ref_force_pushed", "actor": {"login": "factory"}},
		]
		self.assertFalse(live_round.drafted_before_push(by_reviewer, "reviewer"))

	def test_only_a_recorded_retarget_onto_the_base_is_the_factorys(self):
		self.assertIsNotNone(live_round.factory_retarget({"retarget": {"from": "factoryd/req-1-001-x", "to": "main", "at": "t"}}, "main"))
		self.assertIsNone(live_round.factory_retarget({}, "main"))
		self.assertIsNone(live_round.factory_retarget({"retarget": {"to": "develop"}}, "main"))


class StackedFixture(unittest.TestCase):
	def test_ticket_two_edits_the_line_above_where_the_comment_asks_for_one(self):
		d = live_round.FIXTURES / "todo-kafka-stacked"
		fixture = __import__("json").loads((d / "fixture.json").read_text())
		self.assertEqual([c["anchor"] for c in fixture["comments"]], ["func handleVersion"])
		self.assertIn("directly under its `func` line", fixture["comments"][0]["body"])
		second = (d / "plan" / "002.spec.md").read_text()
		self.assertIn("Allowed-Files: README.md, main.go", second)
		# Only the README row is required: ticket 1 sees the whole spec and
		# may write the doc-comment line itself.
		self.assertIn("Required-Changed-Files: README.md\n", second)
		self.assertIn("directly above `func handleVersion`", second)


class Summary(unittest.TestCase):
	def test_share_counts_comments_over_all_recorded_runs(self):
		total = live_round.summarize([
			{"outcome": "pass", "comments": 1, "resolved": 1},
			{"outcome": "fail", "comments": 2, "resolved": 1},
		])
		self.assertEqual(total, {"runs": 2, "passed": 1, "comments": 3, "resolved": 2})


if __name__ == "__main__":
	unittest.main()
