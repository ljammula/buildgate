"""Offline tests for `make proving-ground` (scripts/proving-ground.sh +
scripts/proving_ground_lib.py): the auto-classifier (unit tests over
synthetic run.json fixtures), the fixtures file's own schema and every
referenced ticket spec's parse-ability, and the shell script's own
result-recording plumbing against a stub FACTORYD_BIN. No Docker, no real
model route, no real fixture repos -- the mirror image of
scripts/tests/test_live_smoke_recording.py and scripts/tests/test_bar_lib.py.

Run: python3 -m unittest discover -s scripts/tests -p 'test_proving_ground.py'
(or `make proving-ground-test`).
"""
import json
import os
import shutil
import subprocess
import sys
import tempfile
import unittest

REPO_ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), "..", ".."))
SCRIPTS = os.path.join(REPO_ROOT, "scripts")
sys.path.insert(0, SCRIPTS)
import proving_ground_lib as pg  # noqa: E402

FIXTURES_FILE = os.path.join(REPO_ROOT, "scripts", "proving-ground", "fixtures.json")
PROVING_GROUND_SH = os.path.join(REPO_ROOT, "scripts", "proving-ground.sh")
STUB_FACTORYD = os.path.join(os.path.dirname(__file__), "fixtures", "stub_factoryd_pg.sh")


# ---- classifier -------------------------------------------------------------

class ClassifyTest(unittest.TestCase):
    def test_accepted_as_expected(self):
        run = {"state": "accepted", "gate_results": []}
        self.assertEqual(("accepted", "accepted_as_expected"), pg.classify(run, "accept"))

    def test_false_accept(self):
        run = {"state": "accepted", "gate_results": []}
        self.assertEqual(("accepted", "false_accept"), pg.classify(run, "quarantine:diff_scope"))

    def test_quarantined_as_expected(self):
        run = {"state": "quarantined", "gate_results": [
            {"check": "canonical_verify", "passed": True},
            {"check": "diff_scope", "passed": False},
        ]}
        self.assertEqual(("quarantined", "quarantined_as_expected"), pg.classify(run, "quarantine:diff_scope"))

    def test_unexpected_quarantine_names_the_actual_failing_gate(self):
        run = {"state": "quarantined", "gate_results": [
            {"check": "canonical_verify", "passed": False},
        ]}
        self.assertEqual(("quarantined", "unexpected_quarantine:canonical_verify"), pg.classify(run, "accept"))

    def test_unexpected_quarantine_wrong_gate_vs_expected(self):
        # expect=quarantine:reference_oracle but the run actually quarantined
        # on required_files_changed -- not the expected gate, so this is
        # unexpected, not quarantined_as_expected.
        run = {"state": "quarantined", "gate_results": [
            {"check": "required_files_changed", "passed": False},
        ]}
        self.assertEqual(("quarantined", "unexpected_quarantine:required_files_changed"),
                         pg.classify(run, "quarantine:reference_oracle"))

    def test_quarantine_with_no_failing_gate_recorded(self):
        run = {"state": "quarantined", "gate_results": []}
        self.assertEqual(("quarantined", "unexpected_quarantine:unknown_gate"), pg.classify(run, "accept"))

    def test_halted_uses_reason_code(self):
        run = {"state": "halted", "halt_reason_code": "relay_ceiling_exceeded"}
        self.assertEqual(("halted", "halted:relay_ceiling_exceeded"), pg.classify(run, "accept"))

    def test_halted_without_reason_code_is_unspecified(self):
        run = {"state": "halted"}
        self.assertEqual(("halted", "halted:unspecified"), pg.classify(run, "accept"))

    def test_no_run_record(self):
        self.assertEqual(("no_run_record", "halted:no_run_record"), pg.classify(None, "accept"))

    def test_non_terminal_state(self):
        run = {"state": "slice_running"}
        self.assertEqual(("slice_running", "halted:non_terminal_state:slice_running"), pg.classify(run, "accept"))


class CauseBucketTest(unittest.TestCase):
    def test_as_expected_categories_are_n_a(self):
        self.assertEqual("n/a", pg.cause_bucket({}, "accepted_as_expected"))
        self.assertEqual("n/a", pg.cause_bucket({}, "quarantined_as_expected"))

    def test_no_run_record_is_infra(self):
        self.assertEqual("infra", pg.cause_bucket({}, "halted:no_run_record"))

    def test_relay_ceiling_is_infra(self):
        self.assertEqual("infra", pg.cause_bucket({}, "halted:relay_ceiling_exceeded"))

    def test_docker_halt_error_is_infra(self):
        run = {"halt_error": "docker: Cannot connect to the Docker daemon"}
        self.assertEqual("infra", pg.cause_bucket(run, "halted:unspecified"))

    def test_false_accept_defaults_to_factoryd_bug_suspect(self):
        self.assertEqual("factoryd_bug_suspect", pg.cause_bucket({}, "false_accept"))

    def test_scope_gate_is_ticket_spec(self):
        for gate in ("diff_scope", "required_files_changed", "required_content_present", "tests_added"):
            self.assertEqual("ticket_spec", pg.cause_bucket({}, "unexpected_quarantine:%s" % gate), gate)

    def test_build_quality_gate_is_model_quality(self):
        for gate in ("canonical_verify", "unit_tests", "lint"):
            self.assertEqual("model_quality", pg.cause_bucket({}, "unexpected_quarantine:%s" % gate), gate)

    def test_agent_evidence_failure_is_model_quality(self):
        run = {"agent_evidence": {"succeeded": False}}
        self.assertEqual("model_quality", pg.cause_bucket(run, "halted:unspecified"))

    def test_no_changed_files_is_model_quality(self):
        run = {"changed_files": []}
        self.assertEqual("model_quality", pg.cause_bucket(run, "halted:unspecified"))

    def test_obscure_gate_with_no_signal_is_unknown(self):
        run = {"changed_files": ["a.go"]}
        self.assertEqual("unknown", pg.cause_bucket(run, "unexpected_quarantine:some_future_gate"))

    def test_infra_checked_before_false_accept(self):
        # halted:no_run_record never reaches the false_accept rule (it isn't
        # a false_accept category at all) -- this just pins rule order isn't
        # accidentally reversed for the categories that could plausibly
        # overlap in a future rule change.
        self.assertEqual("infra", pg.cause_bucket({}, "halted:no_run_record"))


class RelayUsageTest(unittest.TestCase):
    def test_sums_across_attempts(self):
        run = {"attempts": [
            {"relay_consumed_input_tokens": 100, "relay_consumed_output_tokens": 20, "relay_consumed_cost_micro_usd": 500},
            {"relay_consumed_input_tokens": 50, "relay_consumed_output_tokens": 10, "relay_consumed_cost_micro_usd": 250},
        ]}
        got = pg.relay_usage(run)
        self.assertEqual(150, got["tokens_in"])
        self.assertEqual(30, got["tokens_out"])
        self.assertAlmostEqual(0.00075, got["cost_usd"])

    def test_empty_when_no_attempts(self):
        got = pg.relay_usage({})
        self.assertEqual({"tokens_in": 0, "tokens_out": 0, "cost_usd": 0.0}, got)


class BarReportTest(unittest.TestCase):
    def test_all_expected_is_a_clean_bar(self):
        records = [
            {"expect": "accept", "category": "accepted_as_expected", "cause_bucket": "n/a"},
            {"expect": "quarantine:diff_scope", "category": "quarantined_as_expected", "cause_bucket": "n/a"},
        ]
        b = pg.bar_report(records)
        self.assertEqual([], b["false_accepts"])
        self.assertEqual([], b["factoryd_false_quarantines"])
        self.assertEqual(1.0, b["one_shot_acceptance_rate"])

    def test_false_accept_is_named(self):
        records = [{"fixture": "f1", "expect": "quarantine:diff_scope", "category": "false_accept", "cause_bucket": "factoryd_bug_suspect"}]
        b = pg.bar_report(records)
        self.assertEqual(["f1"], b["false_accepts"])

    def test_classified_pct_excludes_as_expected(self):
        records = [
            {"expect": "accept", "category": "accepted_as_expected", "cause_bucket": "n/a"},
            {"expect": "accept", "category": "unexpected_quarantine:lint", "cause_bucket": "model_quality"},
            {"expect": "accept", "category": "halted:unspecified", "cause_bucket": "unknown"},
        ]
        b = pg.bar_report(records)
        self.assertAlmostEqual(50.0, b["non_acceptances_classified_pct"])


# ---- fixtures schema / spec parse-ability ------------------------------------

class FixturesFileTest(unittest.TestCase):
    # The tracked corpus holds only fixtures on this repo's own
    # testdata/fixtures; an operator's fixtures on repos that are not public
    # come from PROVING_GROUND_EXTRA_FIXTURES.
    def test_loads_and_validates(self):
        fixtures = pg.load_fixtures(FIXTURES_FILE, REPO_ROOT)
        self.assertGreaterEqual(len(fixtures), 4)

    def test_every_tracked_fixture_repo_is_in_testdata(self):
        for f in pg.load_fixtures(FIXTURES_FILE, REPO_ROOT):
            self.assertTrue(f["repo"].startswith(os.path.join(REPO_ROOT, "testdata", "fixtures") + os.sep), f)

    def test_extra_fixtures_are_appended_with_paths_from_their_own_dir(self):
        with tempfile.TemporaryDirectory() as tmp:
            os.makedirs(os.path.join(tmp, "tickets"))
            extra = os.path.join(tmp, "extra.json")
            with open(extra, "w") as fh:
                json.dump([{"label": "private-one", "repo": "~/code/private", "spec": "tickets/one.spec.md", "expect": "accept", "base_ref": "abc123"}], fh)
            os.environ["PROVING_GROUND_EXTRA_FIXTURES"] = extra
            try:
                fixtures = pg.load_fixtures(FIXTURES_FILE, REPO_ROOT)
            finally:
                del os.environ["PROVING_GROUND_EXTRA_FIXTURES"]
            last = fixtures[-1]
            self.assertEqual(last["label"], "private-one")
            self.assertEqual(last["spec"], os.path.join(tmp, "tickets", "one.spec.md"))
            self.assertEqual(last["repo"], os.path.expanduser("~/code/private"))

    def test_extra_fixture_label_clashing_with_tracked_one_is_refused(self):
        with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as fh:
            json.dump([{"label": "math-ops-multiply", "repo": "~/code/x", "spec": "s.md", "expect": "accept", "base_ref": "HEAD"}], fh)
            extra = fh.name
        os.environ["PROVING_GROUND_EXTRA_FIXTURES"] = extra
        try:
            with self.assertRaisesRegex(ValueError, "math-ops-multiply"):
                pg.load_fixtures(FIXTURES_FILE, REPO_ROOT)
        finally:
            del os.environ["PROVING_GROUND_EXTRA_FIXTURES"]
            os.unlink(extra)

    def test_at_least_two_quarantine_fixtures(self):
        fixtures = pg.load_fixtures(FIXTURES_FILE, REPO_ROOT)
        quarantine = [f for f in fixtures if f["expect"].startswith("quarantine:")]
        self.assertGreaterEqual(len(quarantine), 2, quarantine)

    def test_reference_oracle_and_diff_scope_both_covered(self):
        fixtures = pg.load_fixtures(FIXTURES_FILE, REPO_ROOT)
        expects = {f["expect"] for f in fixtures}
        self.assertIn("quarantine:reference_oracle", expects)
        self.assertTrue(any(e.startswith("quarantine:") and e != "quarantine:reference_oracle" for e in expects))

    def test_every_spec_file_exists(self):
        for f in pg.load_fixtures(FIXTURES_FILE, REPO_ROOT):
            self.assertTrue(os.path.isfile(f["spec"]), "%s: %s" % (f["label"], f["spec"]))

    def test_every_declared_oracle_dir_has_manifest_and_run_command(self):
        for f in pg.load_fixtures(FIXTURES_FILE, REPO_ROOT):
            if not f.get("oracle_dir"):
                continue
            self.assertTrue(os.path.isfile(os.path.join(f["oracle_dir"], "MANIFEST.json")), f["label"])
            self.assertTrue(os.path.isfile(os.path.join(f["oracle_dir"], "RUN_COMMAND.txt")), f["label"])

    def test_rejects_duplicate_labels(self):
        with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as fh:
            json.dump([
                {"label": "x", "repo": "~/code/a", "spec": "data/tickets/math-ops-multiply.spec.md", "expect": "accept", "base_ref": "HEAD"},
                {"label": "x", "repo": "~/code/b", "spec": "data/tickets/math-ops-multiply.spec.md", "expect": "accept", "base_ref": "HEAD"},
            ], fh)
            path = fh.name
        try:
            with self.assertRaises(ValueError):
                pg.load_fixtures(path, REPO_ROOT)
        finally:
            os.unlink(path)

    def test_rejects_bad_expect(self):
        with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as fh:
            json.dump([{"label": "x", "repo": "~/code/a", "spec": "data/tickets/math-ops-multiply.spec.md", "expect": "quarantine", "base_ref": "HEAD"}], fh)
            path = fh.name
        try:
            with self.assertRaises(ValueError):
                pg.load_fixtures(path, REPO_ROOT)
        finally:
            os.unlink(path)

    def test_rejects_missing_base_ref(self):
        # An unpinned fixture drifts as its repo gains the ticket's own
        # feature (baseline 2026-09-24), so base_ref is required.
        with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as fh:
            json.dump([{"label": "x", "repo": "~/code/a", "spec": "data/tickets/math-ops-multiply.spec.md", "expect": "accept"}], fh)
            path = fh.name
        try:
            with self.assertRaisesRegex(ValueError, "base_ref"):
                pg.load_fixtures(path, REPO_ROOT)
        finally:
            os.unlink(path)

    def test_rejects_missing_required_key(self):
        with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as fh:
            json.dump([{"label": "x", "repo": "~/code/a", "expect": "accept"}], fh)
            path = fh.name
        try:
            with self.assertRaises(ValueError):
                pg.load_fixtures(path, REPO_ROOT)
        finally:
            os.unlink(path)


@unittest.skipUnless(shutil.which("go"), "go toolchain not available")
class FixturesSpecParseTest(unittest.TestCase):
    """Every fixture's ticket spec must parse the same way run_ticket.go's
    own -spec handling parses it -- `factoryd check-ticket <path>` runs the
    exact ticketspec.Parse* calls a real run would (see check_ticket.go's
    own doc comment). Built once via `go build`, not `go run` per file, to
    keep this fast."""

    @classmethod
    def setUpClass(cls):
        cls.tmpdir = tempfile.mkdtemp(prefix="proving-ground-check-ticket-")
        cls.bin = os.path.join(cls.tmpdir, "factoryd")
        subprocess.run(["go", "build", "-o", cls.bin, "./cmd/factoryd"], cwd=REPO_ROOT, check=True,
                       capture_output=True, text=True, timeout=180)

    @classmethod
    def tearDownClass(cls):
        shutil.rmtree(cls.tmpdir, ignore_errors=True)

    def test_every_fixture_spec_passes_check_ticket(self):
        for f in pg.load_fixtures(FIXTURES_FILE, REPO_ROOT):
            proc = subprocess.run([self.bin, "check-ticket", f["spec"]], capture_output=True, text=True, timeout=30)
            self.assertEqual(0, proc.returncode, "%s (%s):\n%s\n%s" % (f["label"], f["spec"], proc.stdout, proc.stderr))


# ---- recording plumbing, via a stub FACTORYD_BIN -----------------------------

class RecordingTest(unittest.TestCase):
    def _run(self, run_json_literal, expect="accept", only=""):
        with tempfile.TemporaryDirectory() as tmp:
            source_repo = os.path.join(tmp, "repo")
            os.makedirs(source_repo)
            subprocess.run(["git", "init", "-q", source_repo], check=True)
            subprocess.run(["git", "-C", source_repo, "config", "user.email", "t@example.com"], check=True)
            subprocess.run(["git", "-C", source_repo, "config", "user.name", "t"], check=True)
            with open(os.path.join(source_repo, "README.md"), "w") as f:
                f.write("x\n")
            subprocess.run(["git", "-C", source_repo, "add", "."], check=True)
            subprocess.run(["git", "-C", source_repo, "commit", "-q", "-m", "init"], check=True)

            spec_file = os.path.join(tmp, "spec.md")
            with open(spec_file, "w") as f:
                f.write("# spec\n")

            fixtures_file = os.path.join(tmp, "fixtures.json")
            with open(fixtures_file, "w") as f:
                json.dump([{"label": "pg-test-fixture", "repo": source_repo, "spec": spec_file, "expect": expect, "base_ref": "HEAD"}], f)

            results_file = os.path.join(tmp, "results.jsonl")
            env = dict(os.environ)
            env["FACTORYD_BIN"] = STUB_FACTORYD
            env["PROVING_GROUND_FIXTURES_FILE"] = fixtures_file
            env["PROVING_GROUND_RESULTS_FILE"] = results_file
            env["STUB_FACTORYD_PG_RUN_JSON"] = json.dumps(run_json_literal)
            env["PROVING_GROUND_ONLY"] = only
            env["HOME"] = tmp  # keep scratch/cache writes inside the temp dir
            # This test drives the real script against REPO_ROOT (for
            # scripts/proving_ground_lib.py) -- irrelevant to whether
            # REPO_ROOT's own working tree happens to be clean while this
            # test runs (e.g. mid-edit on this very file).
            env["PROVING_GROUND_ALLOW_DIRTY"] = "1"

            proc = subprocess.run(["sh", PROVING_GROUND_SH], env=env, cwd=REPO_ROOT,
                                  capture_output=True, text=True, timeout=60)
            if os.path.exists(results_file):
                with open(results_file) as f:
                    lines = [json.loads(l) for l in f if l.strip()]
            else:
                lines = []  # "no fixture selected" never creates the file
            return proc, lines

    def test_accepted_run_is_recorded_as_expected(self):
        proc, lines = self._run({"state": "accepted", "gate_results": []})
        self.assertEqual(0, proc.returncode, proc.stdout + proc.stderr)
        self.assertEqual(1, len(lines), lines)
        row = lines[0]
        self.assertEqual("pg-test-fixture", row["fixture"])
        self.assertEqual("temporal", row["path"])
        self.assertEqual("accepted_as_expected", row["category"])
        self.assertEqual("n/a", row["cause_bucket"])
        self.assertEqual("factoryd stub-test-version", row["factoryd_version"])
        self.assertIsInstance(row["duration_s"], int)
        self.assertTrue(row["git_sha"])
        self.assertIn("tokens_in", row)

    def test_false_accept_fails_the_script(self):
        proc, lines = self._run({"state": "accepted", "gate_results": []}, expect="quarantine:diff_scope")
        self.assertNotEqual(0, proc.returncode)
        self.assertEqual("false_accept", lines[0]["category"])
        self.assertIn("FALSE ACCEPT", proc.stdout)

    def test_quarantined_as_expected_does_not_fail_the_script(self):
        run_json = {"state": "quarantined", "gate_results": [{"check": "diff_scope", "passed": False}]}
        proc, lines = self._run(run_json, expect="quarantine:diff_scope")
        self.assertEqual(0, proc.returncode, proc.stdout + proc.stderr)
        self.assertEqual("quarantined_as_expected", lines[0]["category"])

    def test_only_filter_skips_non_matching_fixtures(self):
        proc, lines = self._run({"state": "accepted", "gate_results": []}, only="no-such-label")
        self.assertNotEqual(0, proc.returncode)  # "no fixture selected"
        self.assertEqual(0, len(lines))


if __name__ == "__main__":
    unittest.main()


class ExpectAlternativesTest(unittest.TestCase):
    def test_any_listed_gate_counts_as_expected(self):
        run = {"state": "quarantined", "gate_results": [
            {"check": "canonical_verify", "passed": False},
            {"check": "diff_scope", "passed": True},
            {"check": "tests_added", "passed": False},
        ]}
        self.assertEqual(("quarantined", "quarantined_as_expected"),
                         pg.classify(run, "quarantine:diff_scope|canonical_verify"))
        self.assertEqual(("quarantined", "unexpected_quarantine:tests_added"),
                         pg.classify(run, "quarantine:diff_scope"))


# ---- model_route() ----------------------------------------------------------
#
# proving-ground.sh's own model_route() parses the effective session
# config's routes:/models:/roles: block with a small indentation-based awk
# state machine, not a real YAML parser -- these tests prove it resolves
# the same route/model regardless of the config's own indentation width
# (found via review: an earlier version hardcoded a 2-space-per-level
# assumption that never matched quickstartWriteConfig's own yaml.v3
# Marshal output, which defaults to 4 spaces per level, so a
# `factoryd quickstart`-authored config never resolved a route here at
# all). Extracts and sources model_route() alone (not the whole script,
# which expects a real FACTORYD_BIN and git repo to run end to end) by
# locating its exact "model_route() {" ... closing "}" span in the
# script's own source text.

def _shell_function_source(name):
    with open(PROVING_GROUND_SH, encoding="utf-8") as f:
        lines = f.readlines()
    start = next(i for i, line in enumerate(lines) if line.startswith(name + "() {"))
    end = next(i for i in range(start + 1, len(lines)) if lines[i].rstrip("\n") == "}")
    return "".join(lines[start:end + 1])


def _model_route_function_source():
    # model_route() finds its file through session_config().
    return _shell_function_source("session_config") + _shell_function_source("model_route")


def _model_route(config_yaml):
    """Runs model_route() alone against a config.yml holding config_yaml,
    under a throwaway XDG_CONFIG_HOME, and returns its stdout (stripped)."""
    with tempfile.TemporaryDirectory() as home:
        config_dir = os.path.join(home, "factoryd")
        os.makedirs(config_dir)
        with open(os.path.join(config_dir, "config.yml"), "w", encoding="utf-8") as f:
            f.write(config_yaml)
        script = 'LIB="$LIB"\n' + _model_route_function_source() + "\nmodel_route\n"
        env = dict(os.environ, XDG_CONFIG_HOME=home,
                   LIB=os.path.join(REPO_ROOT, "scripts", "proving_ground_lib.py"))
        proc = subprocess.run(["sh", "-c", script], env=env, capture_output=True, text=True, check=True)
        return proc.stdout.strip()


class ModelRouteTest(unittest.TestCase):
    def test_resolves_with_2_space_indentation(self):
        got = _model_route(
            "routes:\n"
            "  codex:\n"
            "    credential_mode: chatgpt-codex\n"
            "models:\n"
            "  luna:\n"
            "    id: gpt-5.6-luna\n"
            "    routes:\n"
            "      - codex\n"
            "roles:\n"
            "  execution:\n"
            "    model: luna\n"
        )
        self.assertEqual("chatgpt-codex/gpt-5.6-luna", got)

    def test_resolves_with_4_space_indentation_yaml_v3_default(self):
        # This is exactly the shape gopkg.in/yaml.v3's default Marshal
        # produces (quickstartWriteConfig's own encoder) -- the case the
        # earlier, 2-space-hardcoded parser never resolved.
        got = _model_route(
            "routes:\n"
            "    litellm:\n"
            "        credential_mode: static\n"
            "        upstream: https://litellm.example.invalid\n"
            "models:\n"
            "    luna:\n"
            "        id: gpt-5.6-luna\n"
            "        routes:\n"
            "            - litellm\n"
            "roles:\n"
            "    execution:\n"
            "        model: luna\n"
        )
        self.assertEqual("static/gpt-5.6-luna", got)

    def test_unknown_when_config_has_no_route(self):
        got = _model_route('sandbox_cpus: "4"\n')
        self.assertEqual("unknown", got)

    def test_list_at_same_indent_as_its_key(self):
        got = _model_route(
            "routes:\n"
            "  codex:\n"
            "    credential_mode: chatgpt-codex\n"
            "models:\n"
            "  luna:\n"
            "    id: gpt-5.6-luna\n"
            "    routes:\n"
            "    - codex\n"
            "roles:\n"
            "  execution:\n"
            "    model: luna\n"
        )
        self.assertEqual("chatgpt-codex/gpt-5.6-luna", got)

    def test_flow_list_uses_first_route(self):
        got = _model_route(
            "routes:\n"
            "  codex: { credential_mode: chatgpt-codex }\n"
            "  copilot: { credential_mode: github-copilot }\n"
            "models:\n"
            "  luna: { id: gpt-5.6-luna, routes: [copilot, codex] }\n"
            "roles:\n"
            "  execution: { model: luna }\n"
        )
        self.assertEqual("github-copilot/gpt-5.6-luna", got)

    def test_model_key_with_colon(self):
        got = _model_route(
            "routes:\n"
            "  local: { upstream: http://127.0.0.1:11434 }\n"
            "models:\n"
            '  "qwen3:32b": { id: "qwen3:32b", routes: [local] }\n'
            "roles:\n"
            '  execution: { model: "qwen3:32b" }\n'
        )
        self.assertEqual("static/qwen3:32b", got)

    def test_route_ids_override_model_id(self):
        got = _model_route(
            "routes:\n"
            "  copilot: { credential_mode: github-copilot }\n"
            "models:\n"
            "  luna: { id: gpt-5.6-luna, routes: [copilot], route_ids: { copilot: gpt-5.6-luna-copilot } }\n"
            "roles:\n"
            "  execution: { model: luna }\n"
        )
        self.assertEqual("github-copilot/gpt-5.6-luna-copilot", got)

    def test_unresolved_route_is_unknown_not_static(self):
        got = _model_route(
            "models:\n"
            "  luna: { id: gpt-5.6-luna, routes: [missing] }\n"
            "roles:\n"
            "  execution: { model: luna }\n"
        )
        self.assertEqual("unknown", got)


class HarnessRecordTest(unittest.TestCase):
    """Each result row records the execution harness, and reports group by
    it, so one corpus measured under several roles.<role>.harness settings
    yields one bar report per harness."""

    def _config(self, text):
        f = tempfile.NamedTemporaryFile("w", suffix=".yml", delete=False)
        f.write(text)
        f.close()
        self.addCleanup(os.unlink, f.name)
        return f.name

    def test_execution_harness_defaults_to_pi(self):
        path = self._config("roles:\n  execution:\n    model: luna\n")
        self.assertEqual("pi", pg.execution_harness(path))

    def test_execution_harness_reads_the_role(self):
        path = self._config("roles:\n    execution:\n        model: luna\n        harness: ' Codex '\n")
        self.assertEqual("codex", pg.execution_harness(path))

    def test_missing_config_is_unknown(self):
        self.assertEqual("unknown", pg.execution_harness("/nonexistent/config.yml"))

    def test_reports_group_by_harness(self):
        common = dict(date="2026-09-30T00:00:00Z", git_sha="abc", factoryd_version="v",
                      model_route="chatgpt-codex/gpt-5.6-luna", fixture="math-ops-multiply",
                      path="direct", expect="accept", duration_s=1)
        records = [pg.build_record(None, harness=h, **common) for h in ("pi", "codex")]
        self.assertEqual(["codex", "pi"], sorted(r["harness"] for r in records))
        report = pg.render_recent(records, 5)
        self.assertIn("harness pi (1 runs)", report)
        self.assertIn("harness codex (1 runs)", report)

    def test_rows_without_a_harness_group_as_unknown(self):
        row = pg.build_record(None, harness="pi", date="2026-09-30T00:00:00Z", git_sha="abc",
                              factoryd_version="v", model_route="r", fixture="f", path="direct",
                              expect="accept", duration_s=1)
        del row["harness"]
        self.assertIn("harness unknown (1 runs)", pg.render_recent([row], 5))
