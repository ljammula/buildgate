#!/usr/bin/env python3
"""Helpers for scripts/bar.sh (`make bar`): the staged-oracle default-on bar.

Pure functions (summarise_run, verdict, render_report) hold the logic so
scripts/tests/test_bar_lib.py can exercise them offline; the CLI at the bottom is
the thin surface bar.sh calls. Standard library only (Python 3.9+).

The four bar criteria ("Default-on bar"):
  1. zero false passes      -- no accepted `-draft-oracles` run whose canary is not
                               TRUSTWORTHY, and no surviving mutant against a
                               committed oracle;
  2. zero false gates       -- no `reference_oracle` gate failure on an
                               implementation the operator judges correct;
  3. acceptance with flag >= without, on the same fixtures;
  4. one recovered in-loop retry -- a run whose build rounds ended fail, then pass.
Each criterion is MET, UNMET (evidence against it) or UNPROVEN (no evidence
either way -- the honest state for criterion 4 until a retry actually fires).
"""
from __future__ import annotations

import argparse
import glob
import hashlib
import json
import os
import sys
from typing import Any, Dict, List, Optional

MET, UNMET, UNPROVEN = "MET", "UNMET", "UNPROVEN"
# Files the operator (or the hook) authors around a drafted oracle; adding
# RUN_COMMAND.txt is not an edit of the drafted tests.
_NOT_ORACLE_CONTENT = {"RUN_COMMAND.txt"}


def _load_json(path: str) -> Any:
    try:
        with open(path) as fh:
            return json.load(fh)
    except (OSError, ValueError):
        return None


def oracle_hash(oracle_dir: str) -> str:
    """sha256 over the drafted oracle's files (name + bytes), RUN_COMMAND.txt excluded."""
    h = hashlib.sha256()
    if not os.path.isdir(oracle_dir):
        return ""
    for name in sorted(os.listdir(oracle_dir)):
        path = os.path.join(oracle_dir, name)
        if name in _NOT_ORACLE_CONTENT or not os.path.isfile(path):
            continue
        h.update(name.encode() + b"\0")
        with open(path, "rb") as fh:
            h.update(fh.read())
        h.update(b"\0")
    return h.hexdigest()


def round_outcomes(progress_path: str) -> List[str]:
    """Outcomes of each build round in a run's progress.jsonl, in order."""
    out: List[str] = []
    try:
        with open(progress_path) as fh:
            for line in fh:
                try:
                    ev = json.loads(line)
                except ValueError:
                    continue
                if ev.get("stage") == "round" and ev.get("event") == "end":
                    out.append(str(ev.get("outcome")))
    except OSError:
        pass
    return out


def summarise_run(run_dir: str, label: str, rnd: int, flag: str, fixture: str,
                  terminal: str, oracle_modified: Optional[bool] = None,
                  hook_used: bool = False, mutants_expected: bool = False,
                  mutants: Optional[Dict[str, str]] = None,
                  minutes: Optional[float] = None) -> Dict[str, Any]:
    """Fold one driven request (run_dir/id + run_dir/data) into a flat record."""
    rec: Dict[str, Any] = {
        "label": label, "round": rnd, "flag": flag, "fixture": fixture,
        "terminal": terminal, "minutes": minutes,
        "request_state": None, "run_state": None, "accepted": False,
        "gates": [], "oracle_gate": None, "canary": None, "rounds": [],
        "oracle_modified": oracle_modified, "hook_used": hook_used,
        "mutants_expected": mutants_expected, "mutants": mutants,
    }
    try:
        rid = open(os.path.join(run_dir, "id")).read().strip()
    except OSError:
        return rec
    req = _load_json(os.path.join(run_dir, "data", "requests", rid, "request.json")) or {}
    rec["request_state"] = req.get("state")
    runs = sorted(glob.glob(os.path.join(run_dir, "data", "runs", rid + "-*", "run.json")))
    if not runs:
        return rec
    run = _load_json(runs[-1]) or {}
    rec["run_state"] = run.get("state")
    # An accepted run halts the request only because -open-pull-request=false; the
    # run record's own state is what "accepted" means (same rule as live-smoke.sh).
    rec["accepted"] = run.get("state") == "accepted"
    rec["gates"] = [[g.get("check"), bool(g.get("passed"))] for g in run.get("gate_results") or []]
    oracle_gates = [g for g in run.get("gate_results") or [] if g.get("check") == "reference_oracle"]
    if oracle_gates:
        rec["oracle_gate"] = bool(oracle_gates[-1].get("passed"))
    rec["canary"] = (run.get("oracle_canary") or {}).get("verdict")
    rec["rounds"] = round_outcomes(os.path.join(os.path.dirname(runs[-1]), "progress.jsonl"))
    return rec


def retry_recovered(rec: Dict[str, Any]) -> bool:
    """True iff a build round failed and a later one passed, and the run was accepted."""
    rounds = rec.get("rounds") or []
    if "fail" not in rounds or not rec.get("accepted"):
        return False
    return "pass" in rounds[rounds.index("fail") + 1:]


def _rate(recs: List[Dict[str, Any]]) -> Optional[float]:
    return sum(1 for r in recs if r["accepted"]) / len(recs) if recs else None


def verdict(records: List[Dict[str, Any]]) -> Dict[str, Any]:
    """Evaluate the four bar criteria. Returns {criteria: [(name,status,why)], overall, text}."""
    on = [r for r in records if r["flag"] == "on"]
    off = [r for r in records if r["flag"] == "off"]
    crit = []

    # 1. zero false passes
    accepted_on = [r for r in on if r["accepted"]]
    bad = [r["label"] for r in accepted_on if r.get("canary") != "TRUSTWORTHY"]
    survived = [r["label"] for r in on if any(v != "KILLED" for v in (r.get("mutants") or {}).values())]
    unchecked = [r["label"] for r in accepted_on if r.get("mutants_expected") and not r.get("mutants")]
    if bad or survived:
        why = []
        if bad:
            why.append("accepted without a TRUSTWORTHY canary: " + ", ".join(bad))
        if survived:
            why.append("surviving/invalid mutants: " + ", ".join(survived))
        crit.append(("Zero false passes", UNMET, "; ".join(why) + " (a false pass blocks outright)"))
    elif not accepted_on:
        crit.append(("Zero false passes", UNPROVEN, "no accepted flag-on run to examine"))
    elif unchecked:
        crit.append(("Zero false passes", UNPROVEN,
                     "canary TRUSTWORTHY but mutation check missing for: " + ", ".join(unchecked)))
    else:
        n_mut = sum(len(r.get("mutants") or {}) for r in accepted_on)
        crit.append(("Zero false passes", MET,
                     "%d accepted flag-on run(s) with a TRUSTWORTHY canary; %d/%d mutants killed"
                     % (len(accepted_on), n_mut, n_mut)))

    # 2. zero false gates
    gated = [r for r in on if r.get("oracle_gate") is not None]
    failed = [r["label"] for r in gated if r["oracle_gate"] is False]
    if failed:
        crit.append(("Zero false gates", UNMET,
                     "reference_oracle gate failed on: %s -- blocks until root-caused; if the implementation "
                     "was in fact wrong this was a true catch, not a false gate" % ", ".join(failed)))
    elif not gated:
        crit.append(("Zero false gates", UNPROVEN, "no flag-on run reached the reference_oracle gate"))
    else:
        edited = [r["label"] for r in gated if r.get("oracle_modified") and not r.get("hook_used")]
        why = "%d flag-on run(s) passed the reference_oracle gate" % len(gated)
        if edited:
            why += ("; CAVEAT: the drafted oracle was edited at oracle_review for %s, so the human step "
                    "(not the drafter) is what held" % ", ".join(edited))
        crit.append(("Zero false gates", MET, why))

    # 3. acceptance rate with flag >= without
    r_on, r_off = _rate(on), _rate(off)
    if r_on is None or r_off is None:
        crit.append(("Acceptance with flag >= without", UNPROVEN, "need runs with the flag both on and off"))
    else:
        txt = "on %d/%d vs off %d/%d" % (sum(r["accepted"] for r in on), len(on),
                                         sum(r["accepted"] for r in off), len(off))
        crit.append(("Acceptance with flag >= without", MET if r_on >= r_off else UNMET, txt))

    # 4. one recovered in-loop retry
    recovered = [r["label"] for r in on if retry_recovered(r)]
    fired = [r["label"] for r in on if "fail" in (r.get("rounds") or [])]
    if recovered:
        crit.append(("One recovered in-loop retry", MET, "fail then pass in: " + ", ".join(recovered)))
    elif fired:
        crit.append(("One recovered in-loop retry", UNPROVEN,
                     "a round failed in %s but no run recovered to accepted" % ", ".join(fired)))
    else:
        crit.append(("One recovered in-loop retry", UNPROVEN,
                     "no flag-on run had a failing build round (every oracle passed first time)"))

    statuses = [c[1] for c in crit]
    if UNMET in statuses:
        overall = "NOT ADOPTED: at least one criterion is UNMET; the flag stays opt-in"
    elif UNPROVEN in statuses:
        overall = "NOT ADOPTED: no criterion is contradicted but at least one is UNPROVEN; the flag stays opt-in"
    else:
        overall = "ALL FOUR CRITERIA MET on this sample; default-on may be proposed (operator decision)"
    return {"criteria": crit, "overall": overall}


def render_report(records: List[Dict[str, Any]], sha: str, date: str, binary_note: str,
                  rounds: int, path_note: str) -> str:
    v = verdict(records)
    lines = [
        "# Proving ground: staged-oracle default-on bar, %d round(s) (%s)" % (rounds, date),
        "",
        "Generated by `make bar` (scripts/bar.sh). %d round(s) is a smoke test, not a rate estimate." % rounds,
        "",
        "Binary: buildgate `%s` (%s). %s" % (sha, binary_note, path_note),
        "",
        "| Round | Fixture | Flag | Outcome | Gates / canary | Rounds | Mutants | Oracle edited |",
        "|---|---|---|---|---|---|---|---|",
    ]
    for r in records:
        outcome = "accepted" if r["accepted"] else "%s (run=%s, request=%s)" % (
            r["terminal"], r.get("run_state"), r.get("request_state"))
        if r.get("minutes") is not None:
            outcome += ", ~%.1f min" % r["minutes"]
        gates = ", ".join("%s%s" % (c, "" if ok else " FAILED") for c, ok in r["gates"]) or "-"
        if r["flag"] == "on":
            gates += "; canary=%s" % (r.get("canary") or "-")
        muts = r.get("mutants")
        if muts:
            mtxt = "%d/%d killed" % (sum(1 for x in muts.values() if x == "KILLED"), len(muts))
            bad = [k for k, x in muts.items() if x != "KILLED"]
            if bad:
                mtxt += " (" + ", ".join("%s=%s" % (k, muts[k]) for k in bad) + ")"
        else:
            mtxt = "not run" if r["flag"] == "on" else "-"
        edited = "-"
        if r["flag"] == "on":
            edited = "hook" if r.get("hook_used") else {True: "yes", False: "no", None: "?"}[r.get("oracle_modified")]
        lines.append("| R%d | %s | %s | %s | %s | %s | %s | %s |" % (
            r["round"], r["fixture"], r["flag"], outcome, gates, "/".join(r["rounds"]) or "-", mtxt, edited))
    lines += ["", "## Bar criteria", ""]
    for i, (name, status, why) in enumerate(v["criteria"], 1):
        lines.append("%d. **%s:** %s. %s" % (i, name, status, why))
    lines += ["", "Verdict: " + v["overall"] + ".", ""]
    return "\n".join(lines)


# ---- fixtures ---------------------------------------------------------------

def load_fixtures(path: str) -> List[Dict[str, Any]]:
    """scripts/bar/fixtures.json plus, when BAR_EXTRA_FIXTURES names one, an
    operator's own fixtures file kept outside this repo (fixtures on private
    repos); each file's relative paths resolve against its own directory."""
    fixtures = _load_fixture_file(path)
    extra = os.environ.get("BAR_EXTRA_FIXTURES", "")
    if extra:
        fixtures += _load_fixture_file(extra)
    labels = [f["label"] for f in fixtures]
    dupes = sorted({l for l in labels if labels.count(l) > 1})
    if dupes:
        raise ValueError("duplicate fixture label(s) across %s and BAR_EXTRA_FIXTURES: %s" % (path, ", ".join(dupes)))
    return fixtures


def _load_fixture_file(path: str) -> List[Dict[str, Any]]:
    with open(path) as fh:
        data = json.load(fh)
    base = os.path.dirname(os.path.abspath(path))
    out = []
    for f in data:
        for key in ("label", "repo", "request", "verify"):
            if not f.get(key):
                raise ValueError("fixture %r lacks %r" % (f.get("label"), key))
        g = dict(f)
        g["repo"] = os.path.expanduser(f["repo"])
        for key in ("request", "mutants", "oracle_hook"):
            if f.get(key):
                g[key] = os.path.join(base, os.path.expanduser(f[key]))  # absolute paths win
        out.append(g)
    return out


def load_mutants(path: str) -> List[Any]:
    ns: Dict[str, Any] = {}
    with open(path) as fh:
        exec(compile(fh.read(), path, "exec"), ns)  # noqa: S102 -- operator-authored, in-repo data file
    return list(ns["MUTS"])


def apply_mutant(src: str, dst: str, mutants_file: str, index: int) -> str:
    name, old, new = load_mutants(mutants_file)[index]
    with open(src) as fh:
        text = fh.read()
    if old not in text:
        raise ValueError("mutant pattern not found: " + name)
    with open(dst, "w") as fh:
        fh.write(text.replace(old, new, 1))
    return name


# ---- CLI --------------------------------------------------------------------

def main(argv: Optional[List[str]] = None) -> int:
    ap = argparse.ArgumentParser(prog="bar_lib.py")
    sub = ap.add_subparsers(dest="cmd", required=True)
    p = sub.add_parser("labels"); p.add_argument("fixtures"); p.add_argument("--only", default="")
    p = sub.add_parser("field"); p.add_argument("fixtures"); p.add_argument("label"); p.add_argument("name")
    p = sub.add_parser("list"); p.add_argument("fixtures")
    p = sub.add_parser("oracle-hash"); p.add_argument("dir")
    p = sub.add_parser("mutants-count"); p.add_argument("file")
    p = sub.add_parser("mutant-apply")
    p.add_argument("src"); p.add_argument("dst"); p.add_argument("file"); p.add_argument("index", type=int)
    p = sub.add_parser("summarise")
    p.add_argument("--dir", required=True); p.add_argument("--label", required=True)
    p.add_argument("--round", type=int, required=True); p.add_argument("--flag", required=True)
    p.add_argument("--fixture", required=True); p.add_argument("--terminal", required=True)
    p.add_argument("--oracle-modified", choices=["0", "1", ""], default="")
    p.add_argument("--hook-used", action="store_true"); p.add_argument("--mutants-expected", action="store_true")
    p.add_argument("--mutants-json", default=""); p.add_argument("--minutes", type=float)
    p = sub.add_parser("report")
    p.add_argument("records"); p.add_argument("--sha", required=True); p.add_argument("--date", required=True)
    p.add_argument("--rounds", type=int, required=True); p.add_argument("--binary-note", default="")
    p.add_argument("--path-note", default=""); p.add_argument("--out", required=True)
    a = ap.parse_args(argv)

    if a.cmd == "labels":
        for f in load_fixtures(a.fixtures):
            if a.only in f["label"]:
                print(f["label"])
    elif a.cmd == "field":
        f = next(x for x in load_fixtures(a.fixtures) if x["label"] == a.label)
        print(f.get(a.name) or "")
    elif a.cmd == "list":
        for f in load_fixtures(a.fixtures):
            print("%-24s repo=%s mutants=%s hook=%s" % (
                f["label"], f["repo"], os.path.basename(f.get("mutants") or "") or "-",
                os.path.basename(f.get("oracle_hook") or "") or "-"))
    elif a.cmd == "oracle-hash":
        print(oracle_hash(a.dir))
    elif a.cmd == "mutants-count":
        print(len(load_mutants(a.file)))
    elif a.cmd == "mutant-apply":
        try:
            print(apply_mutant(a.src, a.dst, a.file, a.index))
        except ValueError as e:
            print(e, file=sys.stderr)
            return 1
    elif a.cmd == "summarise":
        mutants = _load_json(a.mutants_json) if a.mutants_json else None
        modified = None if a.oracle_modified == "" else a.oracle_modified == "1"
        print(json.dumps(summarise_run(a.dir, a.label, a.round, a.flag, a.fixture, a.terminal, modified,
                                       a.hook_used, a.mutants_expected, mutants, a.minutes)))
    elif a.cmd == "report":
        with open(a.records) as fh:
            recs = [json.loads(line) for line in fh if line.strip()]
        text = render_report(recs, a.sha, a.date, a.binary_note, a.rounds, a.path_note)
        with open(a.out, "w") as fh:
            fh.write(text)
        v = verdict(recs)
        for name, status, why in v["criteria"]:
            print("%-8s %s: %s" % (status, name, why))
        print("Verdict: " + v["overall"])
        return 1 if any(c[1] == UNMET for c in v["criteria"]) else 0
    return 0


if __name__ == "__main__":
    sys.exit(main())
