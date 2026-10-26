import json
import sys
import tempfile
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "scripts"))
import code_review  # noqa: E402


class CodeReviewEvidenceTests(unittest.TestCase):
	def test_a_verdict_succeeds_whatever_the_findings(self):
		findings = [{"severity": "high", "file": "a.go", "line": 12, "summary": "nil deref", "failure_scenario": "Foo(nil) panics"}]
		evidence = code_review.code_review_evidence(True, findings, "", review_policy="required", thinking=None)
		self.assertTrue(evidence["succeeded"], "a finding never fails the review (SC-022)")
		self.assertIn("1 finding(s), 1 high-severity", evidence["stopped_reason"])

	def test_no_verdict_fails_under_required_and_names_the_error(self):
		evidence = code_review.code_review_evidence(
			False, [], '429 "token budget exceeded"', review_policy="required", thinking="max", diff_fetch_failed=True,
		)
		self.assertFalse(evidence["succeeded"])
		self.assertEqual(evidence["thinking"], "max")
		self.assertIn("token budget exceeded", evidence["stopped_reason"])
		self.assertIn("git diff for the reviewer's prompt failed", evidence["stopped_reason"])

	def test_no_verdict_is_recorded_under_advisory(self):
		evidence = code_review.code_review_evidence(False, [], "", review_policy="advisory", thinking=None)
		self.assertTrue(evidence["succeeded"])
		self.assertFalse(evidence["available"])
		self.assertIn("recorded under advisory policy", evidence["stopped_reason"])

	def test_notes_are_kept_only_when_there_are_any(self):
		self.assertNotIn("notes", code_review.code_review_evidence(True, [], "", review_policy="required", thinking=None))
		evidence = code_review.code_review_evidence(True, [], "", review_policy="required", thinking=None, notes=["n"])
		self.assertEqual(evidence["notes"], ["n"])

	def test_writes_a_separate_evidence_file(self):
		with tempfile.TemporaryDirectory() as directory:
			root = Path(directory)
			evidence = {"schema_version": 1, "succeeded": True, "findings": []}
			path = code_review.write_code_review_evidence_json(root, evidence)
			self.assertEqual(path.name, "CODE_REVIEW_EVIDENCE.json")
			self.assertEqual(json.loads(path.read_text()), evidence)


class ParseCodeReviewFindingsTests(unittest.TestCase):
	def test_last_json_object_wins(self):
		text = (
			json.dumps({"findings": [{"severity": "high", "summary": "stale example"}]})
			+ "\n"
			+ json.dumps({"findings": [{"severity": "low", "summary": "real finding"}]})
		)
		available, findings = code_review.parse_code_review_findings(text)
		self.assertTrue(available)
		self.assertEqual(len(findings), 1)
		self.assertEqual(findings[0]["summary"], "real finding")

	def test_prose_around_json_is_ignored(self):
		text = "Here is my review:\n" + json.dumps({"findings": [{"severity": "medium", "summary": "x"}]}) + "\nThanks!"
		available, findings = code_review.parse_code_review_findings(text)
		self.assertTrue(available)
		self.assertEqual(len(findings), 1)

	def test_unknown_severity_defaults_to_medium(self):
		text = json.dumps({"findings": [{"severity": "critical", "summary": "x"}]})
		_, findings = code_review.parse_code_review_findings(text)
		self.assertEqual(findings[0]["severity"], "medium")

	def test_missing_severity_defaults_to_medium(self):
		text = json.dumps({"findings": [{"summary": "x"}]})
		_, findings = code_review.parse_code_review_findings(text)
		self.assertEqual(findings[0]["severity"], "medium")

	def test_bool_line_is_rejected_to_zero(self):
		text = json.dumps({"findings": [{"severity": "low", "summary": "x", "line": True}]})
		_, findings = code_review.parse_code_review_findings(text)
		self.assertEqual(findings[0]["line"], 0)

	def test_negative_line_is_rejected_to_zero(self):
		text = json.dumps({"findings": [{"severity": "low", "summary": "x", "line": -5}]})
		_, findings = code_review.parse_code_review_findings(text)
		self.assertEqual(findings[0]["line"], 0)

	def test_entry_without_summary_is_dropped(self):
		text = json.dumps({"findings": [{"severity": "high", "line": 1}, {"severity": "low", "summary": "kept"}]})
		_, findings = code_review.parse_code_review_findings(text)
		self.assertEqual(len(findings), 1)
		self.assertEqual(findings[0]["summary"], "kept")

	def test_no_findings_key_at_all_is_unavailable(self):
		available, findings = code_review.parse_code_review_findings("no json here")
		self.assertFalse(available)
		self.assertEqual(findings, [])

	def test_empty_findings_list_is_available_and_empty(self):
		text = json.dumps({"findings": []})
		available, findings = code_review.parse_code_review_findings(text)
		self.assertTrue(available)
		self.assertEqual(findings, [])


if __name__ == "__main__":
	unittest.main()
