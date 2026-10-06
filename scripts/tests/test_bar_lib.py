"""Offline tests for scripts/bar_lib.py: the summariser, criteria verdict and report.

Run: python3 -m unittest discover -s scripts/tests   (or `make bar-test`)
"""
import json
import os
import sys
import tempfile
import unittest

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))
import bar_lib  # noqa: E402


def rec(flag="on", accepted=True, canary="TRUSTWORTHY", gate=True, rounds=None, mutants=None,
        expected=False, modified=False, hook=False, label="x", rnd=1):
    return {"label": label, "round": rnd, "flag": flag, "fixture": "fx", "terminal": "halted",
            "minutes": 5.0, "request_state": "halted", "run_state": "accepted" if accepted else "quarantined",
            "accepted": accepted, "gates": [["reference_oracle", gate]] if flag == "on" else [],
            "oracle_gate": gate if flag == "on" else None, "canary": canary if flag == "on" else None,
            "rounds": rounds or ["pass"], "oracle_modified": modified if flag == "on" else None,
            "hook_used": hook, "mutants_expected": expected, "mutants": mutants}


def status(v, i):
    return v["criteria"][i][1]


class VerdictTest(unittest.TestCase):
    def test_smoke_sample_meets_1_to_3_and_leaves_4_unproven(self):
        recs = [rec("off"), rec("on", mutants={"a": "KILLED", "b": "KILLED"}, expected=True)]
        v = bar_lib.verdict(recs)
        self.assertEqual([bar_lib.MET] * 3, [c[1] for c in v["criteria"][:3]])
        self.assertEqual(bar_lib.UNPROVEN, status(v, 3))
        self.assertTrue(v["overall"].startswith("NOT ADOPTED"))
        self.assertIn("UNPROVEN", v["overall"])

    def test_all_four_met_only_with_recovered_retry(self):
        recs = [rec("off"), rec("on", rounds=["fail", "pass"], mutants={"a": "KILLED"}, expected=True)]
        v = bar_lib.verdict(recs)
        self.assertEqual({bar_lib.MET}, {c[1] for c in v["criteria"]})
        self.assertTrue(v["overall"].startswith("ALL FOUR"))

    def test_surviving_mutant_is_a_false_pass(self):
        v = bar_lib.verdict([rec("off"), rec("on", mutants={"a": "KILLED", "b": "SURVIVED"}, expected=True)])
        self.assertEqual(bar_lib.UNMET, status(v, 0))
        self.assertTrue(v["overall"].startswith("NOT ADOPTED: at least one criterion is UNMET"))

    def test_invalid_mutation_check_is_not_a_pass(self):
        v = bar_lib.verdict([rec("off"), rec("on", mutants={"(baseline)": "INVALID"}, expected=True)])
        self.assertEqual(bar_lib.UNMET, status(v, 0))

    def test_accepted_without_trustworthy_canary_is_a_false_pass(self):
        v = bar_lib.verdict([rec("off"), rec("on", canary="CANARY_NOT_EXECUTED")])
        self.assertEqual(bar_lib.UNMET, status(v, 0))

    def test_missing_mutation_check_leaves_criterion_1_unproven(self):
        v = bar_lib.verdict([rec("off"), rec("on", mutants=None, expected=True)])
        self.assertEqual(bar_lib.UNPROVEN, status(v, 0))

    def test_no_accepted_flag_on_run_is_unproven_not_met(self):
        v = bar_lib.verdict([rec("off"), rec("on", accepted=False, gate=False)])
        self.assertEqual(bar_lib.UNPROVEN, status(v, 0))
        self.assertEqual(bar_lib.UNMET, status(v, 1))  # the gate failed: blocks until root-caused

    def test_no_oracle_gate_reached_is_unproven(self):
        r = rec("on", accepted=False)
        r["oracle_gate"] = None
        r["gates"] = []
        self.assertEqual(bar_lib.UNPROVEN, status(bar_lib.verdict([rec("off"), r]), 1))

    def test_oracle_edited_at_review_is_a_caveat_on_criterion_2(self):
        v = bar_lib.verdict([rec("off"), rec("on", modified=True, mutants={"a": "KILLED"}, expected=True)])
        self.assertEqual(bar_lib.MET, status(v, 1))
        self.assertIn("CAVEAT", v["criteria"][1][2])

    def test_hook_authored_oracle_is_not_a_caveat(self):
        v = bar_lib.verdict([rec("off"), rec("on", modified=True, hook=True)])
        self.assertNotIn("CAVEAT", v["criteria"][1][2])

    def test_acceptance_flag_below_baseline_is_unmet(self):
        v = bar_lib.verdict([rec("off"), rec("off"), rec("on"), rec("on", accepted=False, gate=False)])
        self.assertEqual(bar_lib.UNMET, status(v, 2))

    def test_acceptance_needs_both_flags(self):
        self.assertEqual(bar_lib.UNPROVEN, status(bar_lib.verdict([rec("on")]), 2))

    def test_retry_fired_but_not_recovered_stays_unproven(self):
        v = bar_lib.verdict([rec("off"), rec("on", accepted=False, gate=True, rounds=["fail", "fail"])])
        self.assertEqual(bar_lib.UNPROVEN, status(v, 3))
        self.assertIn("no run recovered", v["criteria"][3][2])

    def test_pass_then_fail_is_not_a_recovery(self):
        self.assertFalse(bar_lib.retry_recovered(rec("on", rounds=["pass", "fail"])))
        self.assertFalse(bar_lib.retry_recovered(rec("on", accepted=False, rounds=["fail", "pass"])))
        self.assertTrue(bar_lib.retry_recovered(rec("on", rounds=["fail", "fail", "pass"])))

    def test_flag_off_retry_does_not_count_for_criterion_4(self):
        off = rec("off", rounds=["fail", "pass"])
        self.assertEqual(bar_lib.UNPROVEN, status(bar_lib.verdict([off, rec("on")]), 3))


class ReportTest(unittest.TestCase):
    def test_report_has_table_criteria_and_verdict(self):
        recs = [rec("off", label="R1-fx-off"), rec("on", label="R1-fx-on", mutants={"m": "SURVIVED"}, expected=True)]
        text = bar_lib.render_report(recs, "abc1234", "2026-09-21", "built from a clean tree", 1, "Path: direct.")
        self.assertIn("# Proving ground: staged-oracle default-on bar, 1 round(s) (2026-09-21)", text)
        self.assertIn("| R1 | fx | on |", text)
        self.assertIn("m=SURVIVED", text)
        self.assertIn("1. **Zero false passes:** UNMET", text)
        self.assertIn("4. **One recovered in-loop retry:** UNPROVEN", text)
        self.assertIn("Verdict: NOT ADOPTED", text)
        self.assertIn("smoke test, not a rate estimate", text)


class SummariseTest(unittest.TestCase):
    def _run_dir(self, run_state="accepted", rounds=("fail", "pass"), canary="TRUSTWORTHY"):
        d = tempfile.mkdtemp()
        self.addCleanup(lambda: __import__("shutil").rmtree(d, ignore_errors=True))
        rid = "req-1"
        open(os.path.join(d, "id"), "w").write(rid + "\n")
        os.makedirs(os.path.join(d, "data", "requests", rid))
        json.dump({"state": "halted"}, open(os.path.join(d, "data", "requests", rid, "request.json"), "w"))
        rd = os.path.join(d, "data", "runs", rid + "-001-20260921")
        os.makedirs(rd)
        json.dump({"state": run_state, "oracle_canary": {"verdict": canary},
                   "gate_results": [{"check": "spec_conformity", "passed": True},
                                    {"check": "reference_oracle", "passed": True}]},
                  open(os.path.join(rd, "run.json"), "w"))
        with open(os.path.join(rd, "progress.jsonl"), "w") as fh:
            fh.write("not json\n")
            fh.write(json.dumps({"stage": "build", "event": "end"}) + "\n")
            for o in rounds:
                fh.write(json.dumps({"stage": "round", "event": "end", "outcome": o}) + "\n")
        return d

    def test_accepted_run_with_retry(self):
        r = bar_lib.summarise_run(self._run_dir(), "R1-fx-on", 1, "on", "fx", "halted")
        self.assertTrue(r["accepted"])
        self.assertEqual(["fail", "pass"], r["rounds"])
        self.assertEqual("TRUSTWORTHY", r["canary"])
        self.assertIs(True, r["oracle_gate"])
        self.assertEqual("halted", r["request_state"])
        self.assertTrue(bar_lib.retry_recovered(r))

    def test_quarantined_run_is_not_accepted(self):
        r = bar_lib.summarise_run(self._run_dir(run_state="quarantined"), "l", 1, "off", "fx", "quarantined")
        self.assertFalse(r["accepted"])

    def test_missing_run_dir_files_yield_an_unaccepted_record(self):
        d = tempfile.mkdtemp()
        self.addCleanup(lambda: __import__("shutil").rmtree(d, ignore_errors=True))
        r = bar_lib.summarise_run(d, "l", 1, "on", "fx", "timeout")
        self.assertFalse(r["accepted"])
        self.assertEqual([], r["rounds"])


class OracleHashTest(unittest.TestCase):
    def test_hash_ignores_run_command_but_not_content(self):
        d = tempfile.mkdtemp()
        self.addCleanup(lambda: __import__("shutil").rmtree(d, ignore_errors=True))
        open(os.path.join(d, "a_test.go"), "w").write("one")
        h1 = bar_lib.oracle_hash(d)
        open(os.path.join(d, "RUN_COMMAND.txt"), "w").write("go test")
        self.assertEqual(h1, bar_lib.oracle_hash(d))
        open(os.path.join(d, "a_test.go"), "w").write("two")
        self.assertNotEqual(h1, bar_lib.oracle_hash(d))
        self.assertEqual("", bar_lib.oracle_hash(os.path.join(d, "missing")))


class FixturesAndMutantsTest(unittest.TestCase):
    def test_shipped_fixtures_are_well_formed_and_resolve(self):
        path = os.path.join(os.path.dirname(__file__), "..", "bar", "fixtures.json")
        fixtures = bar_lib.load_fixtures(path)
        # Tracked fixtures build only testdata/fixtures; BAR_EXTRA_FIXTURES
        # adds an operator's own on repos that are not public.
        self.assertGreaterEqual(len(fixtures), 1)
        for f in fixtures:
            self.assertTrue(os.path.isfile(f["request"]), f["request"])
            self.assertTrue(os.path.isfile(f["mutants"]), f["mutants"])
            self.assertTrue(bar_lib.load_mutants(f["mutants"]))
            self.assertTrue(f["impl"])

    def test_fixture_missing_required_field_is_rejected(self):
        d = tempfile.mkdtemp()
        self.addCleanup(lambda: __import__("shutil").rmtree(d, ignore_errors=True))
        p = os.path.join(d, "f.json")
        json.dump([{"label": "a", "repo": "~/r", "request": "r.md"}], open(p, "w"))
        with self.assertRaises(ValueError):
            bar_lib.load_fixtures(p)

    def test_apply_mutant(self):
        d = tempfile.mkdtemp()
        self.addCleanup(lambda: __import__("shutil").rmtree(d, ignore_errors=True))
        m = os.path.join(d, "m.py")
        open(m, "w").write('MUTS=[("flip","a == b","a != b"),("gone","zzz","y")]\n')
        src, dst = os.path.join(d, "s.go"), os.path.join(d, "d.go")
        open(src, "w").write("if a == b { a == b }")
        self.assertEqual("flip", bar_lib.apply_mutant(src, dst, m, 0))
        self.assertEqual("if a != b { a == b }", open(dst).read())  # first occurrence only
        with self.assertRaises(ValueError):
            bar_lib.apply_mutant(src, dst, m, 1)


if __name__ == "__main__":
    unittest.main()


class ExtraFixturesTest(unittest.TestCase):
    def test_extra_fixtures_are_appended_with_paths_from_their_own_dir(self):
        tracked = os.path.join(os.path.dirname(__file__), "..", "bar", "fixtures.json")
        with tempfile.TemporaryDirectory() as tmp:
            extra = os.path.join(tmp, "fixtures.json")
            with open(extra, "w") as fh:
                json.dump([{"label": "private-one", "repo": "~/code/private", "request": "req.md", "verify": "make test", "mutants": "m.py"}], fh)
            os.environ["BAR_EXTRA_FIXTURES"] = extra
            try:
                fixtures = bar_lib.load_fixtures(tracked)
            finally:
                del os.environ["BAR_EXTRA_FIXTURES"]
        last = fixtures[-1]
        self.assertEqual(last["label"], "private-one")
        self.assertEqual(last["request"], os.path.join(tmp, "req.md"))
        self.assertEqual(last["mutants"], os.path.join(tmp, "m.py"))
        self.assertEqual(last["repo"], os.path.expanduser("~/code/private"))
