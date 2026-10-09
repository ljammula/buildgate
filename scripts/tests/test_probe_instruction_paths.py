"""Offline tests for scripts/probe_instruction_paths.py's own logic: reading
the Go table, which table entry covers a path, classifying an observation and
building the fixture. No Docker, no model."""

import pathlib
import sys
import unittest

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parents[1]))
import probe_instruction_paths as probe  # noqa: E402

GO = '''
var reviewInstructionDirs = []string{".agents/skills", ".pi", ".github/hooks"}

// comment
var reviewInstructionFiles = []string{".github/copilot-instructions.md", ".mcp.json"}

var reviewInstructionBaseNames = []string{"AGENTS.md", "CLAUDE.md"}
'''
TABLE = probe.parse_table(GO)


class ParseTable(unittest.TestCase):
	def test_reads_the_three_lists(self):
		self.assertEqual(TABLE["dirs"], [".agents/skills", ".pi", ".github/hooks"])
		self.assertEqual(TABLE["files"], [".github/copilot-instructions.md", ".mcp.json"])
		self.assertEqual(TABLE["names"], ["AGENTS.md", "CLAUDE.md"])

	def test_a_missing_list_is_an_error(self):
		with self.assertRaises(SystemExit):
			probe.parse_table('var reviewInstructionDirs = []string{"a"}')

	def test_reads_the_real_go_file(self):
		table = probe.parse_table(probe.GO_TABLE.read_text())
		self.assertIn("AGENTS.md", table["names"])
		self.assertIn(".mcp.json", table["files"])
		self.assertTrue(table["dirs"])


class CoveredBy(unittest.TestCase):
	def test_a_dir_covers_what_is_below_it(self):
		self.assertEqual(probe.covered_by(TABLE, ".pi/extensions/x.ts"), ".pi")

	def test_a_file_covers_only_itself(self):
		self.assertEqual(probe.covered_by(TABLE, ".mcp.json"), ".mcp.json")
		self.assertIsNone(probe.covered_by(TABLE, ".github/mcp.json"))

	def test_fixed_paths_match_in_any_directory(self):
		self.assertEqual(probe.covered_by(TABLE, "pkg/.github/copilot-instructions.md"), ".github/copilot-instructions.md")
		self.assertEqual(probe.covered_by(TABLE, "pkg/.pi/SYSTEM.md"), ".pi")
		self.assertEqual(probe.covered_by(TABLE, "a/b/c/.github/hooks/probe.json"), ".github/hooks")
		self.assertEqual(probe.covered_by(TABLE, "a/b/c/.mcp.json"), ".mcp.json")
		self.assertIsNone(probe.covered_by(TABLE, "pkg/.github/mcp.json"))
		self.assertIsNone(probe.covered_by(TABLE, "pkg/.github/workflows/ci.yml"))
		self.assertIsNone(probe.covered_by(TABLE, "pkg/github/hooks/probe.json"))

	def test_the_first_covering_entry_in_table_order_is_named(self):
		self.assertEqual(probe.covered_by(TABLE, "a/.pi/x/.github/hooks/h.json"), ".pi")
		self.assertEqual(probe.covered_by(TABLE, "a/.github/hooks/.pi/x"), ".pi")
		real = probe.parse_table(probe.GO_TABLE.read_text())
		self.assertEqual(probe.covered_by(real, "pkg/.claude/skills/probe/SKILL.md"), ".claude/skills")
		self.assertEqual(probe.covered_by(real, "pkg/.claude/settings.json"), ".claude")

	def test_a_saved_result_is_judged_against_the_table_as_it_is_now(self):
		result = {"candidates": [{"path": "pkg/.github/copilot-instructions.md", "table_path": None}, {"path": ".github/mcp.json", "table_path": "stale"}]}
		probe.recover_candidates(result, TABLE)
		self.assertEqual([c["table_path"] for c in result["candidates"]], [".github/copilot-instructions.md", None])

	def test_a_base_name_matches_in_any_directory(self):
		self.assertEqual(probe.covered_by(TABLE, "pkg/deep/AGENTS.md"), "AGENTS.md")

	def test_a_sibling_prefix_is_not_covered(self):
		self.assertIsNone(probe.covered_by(TABLE, ".pix/a"))


class Candidates(unittest.TestCase):
	def setUp(self):
		self.cands = probe.build_candidates(TABLE)
		self.by_path = {c["path"]: c for c in self.cands}

	def test_every_table_entry_has_a_marker(self):
		covered = {c["table_path"] for c in self.cands}
		for entry in TABLE["dirs"] + TABLE["files"] + TABLE["names"]:
			self.assertIn(entry, covered)

	def test_tokens_are_unique_and_in_the_content_of_text_markers(self):
		slugs = [c["slug"] for c in self.cands]
		self.assertEqual(len(slugs), len(set(slugs)))
		self.assertIn(probe.token("AGENTS-md"), self.by_path["AGENTS.md"]["content"])
		self.assertIn(probe.token("agents-skills-probe-SKILL-md"), self.by_path[".agents/skills/probe/SKILL.md"]["content"])

	def test_an_executed_marker_creates_its_own_file(self):
		self.assertIn("/probe-out/executed-mcp-json", self.by_path[".mcp.json"]["content"])

	def test_candidates_outside_the_table_are_planted(self):
		self.assertIsNone(self.by_path[".github/mcp.json"]["table_path"])
		self.assertIsNone(self.by_path[".cursorrules"]["table_path"])


class Classify(unittest.TestCase):
	CANDS = [{"slug": "a", "table_path": "A"}, {"slug": "b", "table_path": "B"}, {"slug": "c", "table_path": None}]

	def test_a_token_in_a_request_wins_over_a_side_effect(self):
		out = probe.classify(self.CANDS, "x " + probe.token("a") + " y", {"a", "b"})
		self.assertEqual(out, {"a": "request", "b": "executed", "c": "not_observed"})

	def test_table_observation_lists_turns_per_table_path(self):
		per_kind = {"review": {"a": "request", "b": "not_observed", "c": "request"}, "build": {"a": "request", "b": "executed", "c": "not_observed"}}
		self.assertEqual(probe.table_observation(self.CANDS, per_kind), {"A": ["build", "review"], "B": ["build"]})

	def test_uncovered_observed_reports_non_table_paths_only(self):
		cands = [{"slug": "a", "path": "a", "table_path": "A"}, {"slug": "c", "path": "c/x", "table_path": None}]
		per_kind = {"review": {"a": "request", "c": "executed"}}
		self.assertEqual(probe.uncovered_observed(cands, per_kind), [{"path": "c/x", "turns": ["review"], "how": ["executed"]}])


class Variants(unittest.TestCase):
	def test_each_shadowed_file_is_judged_where_nothing_shadows_it(self):
		for path in ("AGENTS.override.md", "CLAUDE.md", "pkg/CLAUDE.md"):
			judges = [v for v in probe.VARIANTS if probe.variant_judges(v, path) and not probe.variant_drops(v, path)]
			self.assertEqual(len(judges), 1, path)

	def test_an_ordinary_path_is_judged_in_main_only(self):
		judges = [v for v in probe.VARIANTS if probe.variant_judges(v, "GEMINI.md")]
		self.assertEqual(judges, ["main"])


class Fixture(unittest.TestCase):
	def test_a_probed_harness_gets_observed_paths_and_a_documented_one_keeps_its_entry(self):
		cands = [
			{"slug": "agents", "path": "AGENTS.md", "table_path": "AGENTS.md", "kind": "text"},
			{"slug": "pi-x", "path": ".pi/x", "table_path": ".pi", "kind": "text"},
			{"slug": "hooks", "path": ".github/hooks/p.json", "table_path": ".github/hooks", "kind": "exec"},
			{"slug": "other", "path": ".cursorrules", "table_path": None, "kind": "text"},
		]
		no = "not_observed"
		turns = {
			"review": {"outcomes": {"agents": "request", "pi-x": no, "hooks": no, "other": no}},
			"build": {"outcomes": {"agents": "request", "pi-x": no, "hooks": no, "other": "request"}},
			"control": {"outcomes": {"agents": "request", "pi-x": "request", "hooks": "executed", "other": no}},
		}
		result = {"candidates": cands, "harnesses": {"pi": {"version": "0.9.1", "turns": turns}, "pifork": {"version": None, "turns": {}}}}
		old = {n: {"version": "1.2.3", "source": "documented", "paths": ["AGENTS.md"]} for n in probe.HARNESSES}
		fixture = probe.fixture_from_result(result, TABLE, old)
		self.assertEqual(fixture["pi"]["source"], "probed")
		self.assertEqual(fixture["pi"]["version"], "0.9.1")
		self.assertEqual(fixture["pi"]["paths"], ["AGENTS.md"])
		self.assertEqual(fixture["pi"]["trust_gated"], [".pi", ".github/hooks"])
		self.assertEqual(fixture["pi"]["uncovered"], [{"path": ".cursorrules", "turns": ["build"], "how": ["request"]}])
		self.assertEqual(fixture["pifork"], old["pifork"])
		self.assertEqual(fixture["codex"], old["codex"])
		self.assertNotIn("AGENTS.md", fixture["unprobed"])
		self.assertIn(".mcp.json", fixture["unprobed"])

	def test_versions_are_read_from_the_images_own_text(self):
		self.assertEqual(probe.parse_version("codex-cli 0.154.0"), "0.154.0")
		self.assertEqual(probe.parse_version("GitHub Copilot CLI 1.0.88."), "1.0.88")
		self.assertIsNone(probe.parse_version(None))


class Replies(unittest.TestCase):
	def test_a_read_tool_is_chosen_with_the_path_argument(self):
		body = {"tools": [{"type": "function", "function": {"name": "read", "parameters": {"properties": {"path": {"type": "string"}}}}}]}
		self.assertEqual(probe.pick_tool_call(body), ("read", {"path": probe.LAZY_FILE}))

	def test_no_known_tool_gives_none(self):
		self.assertIsNone(probe.pick_tool_call({"tools": []}))


if __name__ == "__main__":
	unittest.main()
