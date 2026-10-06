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


class Summary(unittest.TestCase):
	def test_share_counts_comments_over_all_recorded_runs(self):
		total = live_round.summarize([
			{"outcome": "pass", "comments": 1, "resolved": 1},
			{"outcome": "fail", "comments": 2, "resolved": 1},
		])
		self.assertEqual(total, {"runs": 2, "passed": 1, "comments": 3, "resolved": 2})


if __name__ == "__main__":
	unittest.main()
