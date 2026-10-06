#!/usr/bin/env python3
"""Helpers for scripts/proving-ground.sh (`make proving-ground`): the
standing proving-ground corpus (AGENTS.md's "Live validation" section).

Pure functions (load_fixtures, classify, cause_bucket, summarise, render_*)
hold the logic so scripts/tests/test_proving_ground.py can exercise them
offline, the same split scripts/bar_lib.py uses for `make bar`. Standard
library only (Python 3.9+).

## Auto-classification (plan criterion 3)

`classify(run_json, expect)` reads a run's own durable evidence -- state,
gate_results, halt_reason_code -- and emits exactly one category:

  accepted_as_expected      | quarantined_as_expected
  false_accept               (accepted, but expect was quarantine:<gate>)
  unexpected_quarantine:<gate>
  halted:<reason_code>

`cause_bucket(run_json, category)` then makes a best-effort guess at WHY,
for every category other than the two "as_expected" ones:
factoryd_bug_suspect, ticket_spec, model_quality, target_repo_env, infra,
or unknown. This step is explicitly heuristic -- see CAUSE_RULES's own
doc comment -- and is reported as such; it is not a substitute for a human
reading the run's evidence when it matters.
"""
from __future__ import annotations

import argparse
import glob
import json
import os
import sys
from typing import Any, Dict, List, Optional, Tuple

# ---- fixtures ---------------------------------------------------------------

_REQUIRED_FIXTURE_KEYS = ("label", "repo", "spec", "expect", "base_ref")


def _valid_expect(expect: str) -> bool:
    if expect == "accept":
        return True
    return expect.startswith("quarantine:") and len(expect) > len("quarantine:")


def load_fixtures(path: str, repo_root: str) -> List[Dict[str, Any]]:
    """Load scripts/proving-ground/fixtures.json plus, when
    PROVING_GROUND_EXTRA_FIXTURES names one, an operator's own fixtures file
    kept outside this repo (fixtures on private repos). The extra file's
    relative paths resolve against its own directory.
    """
    fixtures = _load_fixture_file(path, repo_root)
    extra = os.environ.get("PROVING_GROUND_EXTRA_FIXTURES", "")
    if extra:
        fixtures += _load_fixture_file(extra, os.path.dirname(os.path.abspath(extra)))
    labels = [f["label"] for f in fixtures]
    dupes = sorted({l for l in labels if labels.count(l) > 1})
    if dupes:
        raise ValueError("duplicate fixture label(s) across %s and PROVING_GROUND_EXTRA_FIXTURES: %s" % (path, ", ".join(dupes)))
    return fixtures


def _load_fixture_file(path: str, repo_root: str) -> List[Dict[str, Any]]:
    """Load and validate one fixtures file.

    `repo` is expanded (~); `repo`, `spec` and `oracle_dir`, when relative,
    are resolved against repo_root (this repo's own fixture repos, tickets
    and oracles live under testdata/fixtures, data/tickets and data/oracles
    there, not next to the fixtures file).
    """
    with open(path) as fh:
        data = json.load(fh)
    if not isinstance(data, list):
        raise ValueError("%s: top level must be a JSON array" % path)
    labels_seen = set()
    out = []
    for i, f in enumerate(data):
        if not isinstance(f, dict):
            raise ValueError("%s: fixture %d is not an object" % (path, i))
        for key in _REQUIRED_FIXTURE_KEYS:
            if not f.get(key):
                raise ValueError("%s: fixture %d lacks required key %r" % (path, i, key))
        if f["label"] in labels_seen:
            raise ValueError("%s: duplicate fixture label %r" % (path, f["label"]))
        labels_seen.add(f["label"])
        if not _valid_expect(f["expect"]):
            raise ValueError(
                "%s: fixture %r has invalid expect %r (want \"accept\" or \"quarantine:<gate>\")"
                % (path, f["label"], f["expect"])
            )
        g = dict(f)
        g["repo"] = os.path.expanduser(f["repo"])
        for key in ("repo", "spec", "oracle_dir"):
            if g.get(key) and not os.path.isabs(g[key]):
                g[key] = os.path.join(repo_root, g[key])
        out.append(g)
    return out


# ---- classification (plan criterion 3) ---------------------------------------

def classify(run_json: Optional[Dict[str, Any]], expect: str) -> Tuple[str, str]:
    """(outcome, category) for one run, per this module's own doc comment.

    outcome is the run's raw run.json `state` (or "no_run_record" when
    run_json is None -- run.json missing or unparseable). category is
    exactly one of the five forms listed above.
    """
    if run_json is None:
        return "no_run_record", "halted:no_run_record"

    state = run_json.get("state") or "unknown"
    gate_results = run_json.get("gate_results") or []
    failing_gates = [g.get("check") for g in gate_results if g.get("check") and g.get("passed") is False]
    expect_gate = expect.split(":", 1)[1] if expect.startswith("quarantine:") else None

    if state == "accepted":
        return state, "accepted_as_expected" if expect == "accept" else "false_accept"

    if state == "quarantined":
        # "quarantine:a|b": any listed gate counts. An out-of-scope ticket can
        # be refused by diff_scope (the agent edits the forbidden file) or by
        # canonical_verify (the agent declines to), and both are correct
        # (baseline 2026-09-24, direct vs Temporal on the same fixture).
        if expect_gate and set(expect_gate.split("|")) & set(failing_gates):
            return state, "quarantined_as_expected"
        actual = failing_gates[-1] if failing_gates else "unknown_gate"
        return state, "unexpected_quarantine:%s" % actual

    if state == "halted":
        return state, "halted:%s" % (run_json.get("halt_reason_code") or "unspecified")

    # Any other recorded state (ready/slice_running/verifying, or an
    # unrecognized future one) reached this script only because the run
    # never got to a terminal state -- itself worth flagging, not silently
    # treated as any of the above.
    return state, "halted:non_terminal_state:%s" % state


_INFRA_HALT_SUBSTRINGS = (
    "docker", "mount", "relay unreachable", "connection refused", "no such host",
    "context deadline exceeded", "i/o timeout", "sandbox launch", "free-space",
)


def _rule_infra(run_json: Dict[str, Any], category: str) -> bool:
    if category == "halted:no_run_record":
        return True
    if not category.startswith("halted:"):
        return False
    if category == "halted:relay_ceiling_exceeded":
        # A configured budget ceiling was hit -- an operator/infra sizing
        # fact, not a code defect or a bad ticket.
        return True
    halt_error = (run_json.get("halt_error") or "").lower()
    return any(s in halt_error for s in _INFRA_HALT_SUBSTRINGS)


def _rule_false_accept_is_factoryd_bug(run_json: Dict[str, Any], category: str) -> bool:
    # Accepting a run that a deterministic, evidence-based gate should have
    # failed is the default explanation for a false accept: the ticket
    # declared its scope/oracle correctly (that's what "expect
    # quarantine:<gate>" means here), so the gate not catching it points at
    # the gate's own evaluation, not the ticket or the model's code.
    return category == "false_accept"


_SCOPE_GATES = {"diff_scope", "required_files_changed", "required_content_present", "tests_added"}
_BUILD_QUALITY_GATES = {
    "canonical_verify", "unit_tests", "integration_tests", "lint", "security_audit", "full_suite_verify",
}


def _rule_ticket_spec(run_json: Dict[str, Any], category: str) -> bool:
    if not category.startswith("unexpected_quarantine:"):
        return False
    gate = category.split(":", 1)[1]
    return gate in _SCOPE_GATES


def _rule_model_quality(run_json: Dict[str, Any], category: str) -> bool:
    if category.startswith("unexpected_quarantine:"):
        gate = category.split(":", 1)[1]
        if gate in _BUILD_QUALITY_GATES:
            return True
    agent_evidence = run_json.get("agent_evidence") or {}
    if agent_evidence.get("succeeded") is False:
        return True
    changed_files = run_json.get("changed_files")
    if changed_files is not None and len(changed_files) == 0:
        return True
    return False


def _rule_target_repo_env(run_json: Dict[str, Any], category: str) -> bool:
    # Heuristic hook for a base-SHA verify comparison (plan's own wording:
    # "compare with a base-SHA verify if the run records one"). No run.json
    # field records one as of this writing (see AGENTS.md/run.go survey,
    # 2026-09-24) -- this rule is therefore currently unreachable in
    # practice and exists so a future base-verify attempt (any Attempt
    # whose Kind contains "base") is picked up without another classifier
    # change. Left in, not deleted, precisely because it is forward-looking
    # and documented as such -- see CAUSE_RULES's own doc comment.
    if not category.startswith("unexpected_quarantine:"):
        return False
    gate = category.split(":", 1)[1]
    if gate not in ("canonical_verify", "full_suite_verify"):
        return False
    base_attempts = [a for a in (run_json.get("attempts") or []) if "base" in (a.get("kind") or "")]
    return any(a.get("exit_code") not in (0, None) for a in base_attempts)


# Table-driven, evaluated top to bottom, first match wins. Every rule here
# is a HEURISTIC over the run's own evidence, not a proof -- see this
# module's own doc comment and each rule function's comment for which
# signal it leans on and how confident that signal is. `factoryd bug
# suspect` and `infra` lean on the strongest available signals (a
# deterministic gate accepting what it shouldn't; an explicit halt-error
# substring); `ticket_spec` and `model_quality` are the weakest and most
# likely to need a human's eyes on a specific run.
CAUSE_RULES: List[Tuple[str, Any]] = [
    ("infra", _rule_infra),
    ("factoryd_bug_suspect", _rule_false_accept_is_factoryd_bug),
    ("ticket_spec", _rule_ticket_spec),
    ("model_quality", _rule_model_quality),
    ("target_repo_env", _rule_target_repo_env),
]


def cause_bucket(run_json: Dict[str, Any], category: str) -> str:
    """Best-effort cause bucket for a non-"as_expected" category. "n/a" for
    accepted_as_expected/quarantined_as_expected (nothing to explain);
    "unknown" when no rule above fired."""
    if category in ("accepted_as_expected", "quarantined_as_expected"):
        return "n/a"
    for name, predicate in CAUSE_RULES:
        if predicate(run_json, category):
            return name
    return "unknown"


# ---- recording ----------------------------------------------------------------

def relay_usage(run_json: Dict[str, Any]) -> Dict[str, int]:
    """Best-effort sum of an attempt's own recorded relay spend across every
    attempt -- the same fields internal/run.Attempt records per attempt
    (see that struct's own doc comment); partial/absent for an attempt with
    no relay or one whose relay was never cleaned up (RelaySpendPartial)."""
    tokens_in = tokens_out = cost_micro_usd = 0
    for a in run_json.get("attempts") or []:
        tokens_in += int(a.get("relay_consumed_input_tokens") or 0)
        tokens_out += int(a.get("relay_consumed_output_tokens") or 0)
        cost_micro_usd += int(a.get("relay_consumed_cost_micro_usd") or 0)
    return {"tokens_in": tokens_in, "tokens_out": tokens_out, "cost_usd": round(cost_micro_usd / 1e6, 6)}


def build_record(run_json: Optional[Dict[str, Any]], *, date: str, git_sha: str, factoryd_version: str,
                 model_route: str, harness: str, fixture: str, path: str, expect: str,
                 duration_s: int) -> Dict[str, Any]:
    outcome, category = classify(run_json, expect)
    bucket = cause_bucket(run_json or {}, category)
    usage = relay_usage(run_json or {})
    return {
        "date": date, "git_sha": git_sha, "factoryd_version": factoryd_version, "model_route": model_route,
        "harness": harness, "fixture": fixture, "path": path, "expect": expect, "outcome": outcome, "category": category,
        "cause_bucket": bucket, "duration_s": int(duration_s),
        "tokens_in": usage["tokens_in"], "tokens_out": usage["tokens_out"], "cost_usd": usage["cost_usd"],
    }


# ---- reporting ------------------------------------------------------------------

def summary_table(records: List[Dict[str, Any]]) -> str:
    if not records:
        return "(no runs recorded)"
    cols = ["fixture", "path", "expect", "outcome", "category", "cause_bucket", "duration_s"]
    widths = {c: max(len(c), *(len(str(r.get(c, ""))) for r in records)) for c in cols}

    def fmt(r: Dict[str, Any]) -> str:
        return "  ".join(str(r.get(c, "")).ljust(widths[c]) for c in cols)

    lines = [fmt({c: c for c in cols})]
    lines += [fmt(r) for r in records]
    return "\n".join(lines)


def bar_report(records: List[Dict[str, Any]]) -> Dict[str, Any]:
    """The plan's Phase 4 bar: false accepts, factoryd-caused false
    quarantines, % of non-acceptances classified non-"unknown", and the
    one-shot acceptance rate over expect=accept fixtures."""
    false_accepts = [r for r in records if r["category"] == "false_accept"]
    factoryd_false_quarantines = [
        r for r in records
        if r["category"].startswith("unexpected_quarantine:") and r["cause_bucket"] == "factoryd_bug_suspect"
    ]
    non_acceptances = [r for r in records if r["category"] not in ("accepted_as_expected", "quarantined_as_expected")]
    classified = [r for r in non_acceptances if r["cause_bucket"] not in ("unknown", "n/a")]
    classified_pct = 100.0 * len(classified) / len(non_acceptances) if non_acceptances else None
    accept_fixtures = [r for r in records if r["expect"] == "accept"]
    accepted = [r for r in accept_fixtures if r["category"] == "accepted_as_expected"]
    one_shot_rate = len(accepted) / len(accept_fixtures) if accept_fixtures else None
    return {
        "false_accepts": [r["fixture"] for r in false_accepts],
        "factoryd_false_quarantines": [r["fixture"] for r in factoryd_false_quarantines],
        "non_acceptances_classified_pct": classified_pct,
        "one_shot_acceptance_rate": one_shot_rate,
        "n_accept_fixtures": len(accept_fixtures),
        "n_accepted": len(accepted),
    }


def render_bar_report(records: List[Dict[str, Any]]) -> str:
    b = bar_report(records)
    lines = ["", "## Proving-ground bar", ""]
    lines.append("1. Zero false accepts: %s%s" % (
        "MET" if not b["false_accepts"] else "UNMET (%s)" % ", ".join(b["false_accepts"]),
        "" if records else " (no runs)"))
    lines.append("2. Zero factoryd-caused false quarantines: %s" % (
        "MET" if not b["factoryd_false_quarantines"] else "UNMET (%s)" % ", ".join(b["factoryd_false_quarantines"])))
    pct = b["non_acceptances_classified_pct"]
    lines.append("3. Non-acceptances classified into a non-unknown bucket: %s" % (
        "n/a (no non-acceptances)" if pct is None else "%.0f%%" % pct))
    rate = b["one_shot_acceptance_rate"]
    lines.append("4. One-shot acceptance rate (expect=accept fixtures): %s" % (
        "n/a (no expect=accept fixtures run)" if rate is None else "%d/%d (%.0f%%)" % (
            b["n_accepted"], b["n_accept_fixtures"], 100.0 * rate)))
    return "\n".join(lines)


def group_by_date_sha(records: List[Dict[str, Any]]) -> Dict[Tuple[str, str, str], List[Dict[str, Any]]]:
    """Groups by date, commit and execution harness, so runs of the same
    corpus under different harnesses get separate bar reports."""
    groups: Dict[Tuple[str, str, str], List[Dict[str, Any]]] = {}
    for r in records:
        # Rows recorded before the harness field existed are "unknown", not
        # "pi": some of them ran under another harness.
        key = (r.get("date", "")[:10], r.get("git_sha", ""), r.get("harness", "unknown"))
        groups.setdefault(key, []).append(r)
    return groups


def render_recent(records: List[Dict[str, Any]], n: int) -> str:
    groups = group_by_date_sha(records)
    keys = sorted(groups, reverse=True)[:n]
    out = []
    for date, sha, harness in keys:
        group = groups[(date, sha, harness)]
        out.append("=== %s @ %s, harness %s (%d runs) ===" % (date, sha, harness, len(group)))
        out.append(summary_table(group))
        out.append(render_bar_report(group))
        out.append("")
    return "\n".join(out) if out else "(no runs recorded)"


# ---- model route --------------------------------------------------------------

def model_route(config_path: str) -> str:
    """"<credential_mode>/<model id>" for roles.execution in a session config
    (routes:/models:/roles:), or "unknown" when it can't be resolved: no
    file, no PyYAML, or a role, model or route that isn't there. Mirrors
    internal/sessionconfig's defaults: credential_mode empty means static,
    and a model's id on a route is route_ids[route] when set, else id."""
    try:
        import yaml  # PyYAML: optional, only this subcommand needs it
        with open(config_path, encoding="utf-8") as fh:
            cfg = yaml.safe_load(fh) or {}
        model_name = cfg["roles"]["execution"]["model"]
        model = cfg["models"][model_name]
        route_name = model["routes"][0]
        route = cfg["routes"][route_name]
        model_id = (model.get("route_ids") or {}).get(route_name) or model["id"]
        mode = route.get("credential_mode") or "static"
    except Exception:
        return "unknown"
    return "%s/%s" % (mode, model_id)


def execution_harness(config_path: str) -> str:
    """roles.execution.harness in a session config ("pi" when the role sets
    none, matching internal/harness's default), or "unknown" when the file
    can't be read: no file or no PyYAML."""
    try:
        import yaml  # PyYAML: optional, only this subcommand needs it
        with open(config_path, encoding="utf-8") as fh:
            cfg = yaml.safe_load(fh) or {}
    except Exception:
        return "unknown"
    role = ((cfg.get("roles") or {}).get("execution") or {})
    # Normalised the way internal/harness looks names up (trim, lower-case).
    return (role.get("harness") or "").strip().lower() or "pi"


# ---- CLI ------------------------------------------------------------------------

def main(argv: Optional[List[str]] = None) -> int:
    ap = argparse.ArgumentParser(prog="proving_ground_lib.py")
    sub = ap.add_subparsers(dest="cmd", required=True)

    p = sub.add_parser("labels"); p.add_argument("fixtures"); p.add_argument("--repo-root", required=True)
    p.add_argument("--only", default="")
    p = sub.add_parser("field"); p.add_argument("fixtures"); p.add_argument("--repo-root", required=True)
    p.add_argument("label"); p.add_argument("name")
    p = sub.add_parser("list"); p.add_argument("fixtures"); p.add_argument("--repo-root", required=True)
    p = sub.add_parser("validate"); p.add_argument("fixtures"); p.add_argument("--repo-root", required=True)
    p = sub.add_parser("classify")
    p.add_argument("--run-json", required=True, help="path to a run.json, or '-' for none (no_run_record)")
    p.add_argument("--expect", required=True)
    p = sub.add_parser("record")
    p.add_argument("--run-json", required=True)
    p.add_argument("--date", required=True); p.add_argument("--git-sha", required=True)
    p.add_argument("--factoryd-version", required=True); p.add_argument("--model-route", required=True)
    p.add_argument("--harness", required=True)
    p.add_argument("--fixture", required=True); p.add_argument("--path", required=True)
    p.add_argument("--expect", required=True); p.add_argument("--duration-s", type=int, required=True)
    p = sub.add_parser("report"); p.add_argument("records"); p.add_argument("--recent", type=int, default=20)
    p = sub.add_parser("model-route"); p.add_argument("config")
    p = sub.add_parser("harness"); p.add_argument("config")
    a = ap.parse_args(argv)

    if a.cmd == "labels":
        for f in load_fixtures(a.fixtures, a.repo_root):
            if a.only in f["label"]:
                print(f["label"])
    elif a.cmd == "field":
        f = next(x for x in load_fixtures(a.fixtures, a.repo_root) if x["label"] == a.label)
        print(f.get(a.name) or "")
    elif a.cmd == "list":
        for f in load_fixtures(a.fixtures, a.repo_root):
            print("%-32s repo=%s expect=%s oracle=%s" % (
                f["label"], f["repo"], f["expect"], os.path.basename(f.get("oracle_dir") or "") or "-"))
    elif a.cmd == "validate":
        fixtures = load_fixtures(a.fixtures, a.repo_root)
        print("%d fixture(s) parsed ok" % len(fixtures))
    elif a.cmd == "classify":
        run_json = None if a.run_json == "-" else json.load(open(a.run_json))
        outcome, category = classify(run_json, a.expect)
        bucket = cause_bucket(run_json or {}, category)
        print(json.dumps({"outcome": outcome, "category": category, "cause_bucket": bucket}))
    elif a.cmd == "record":
        run_json = None if a.run_json == "-" else json.load(open(a.run_json))
        rec = build_record(run_json, date=a.date, git_sha=a.git_sha, factoryd_version=a.factoryd_version,
                           model_route=a.model_route, harness=a.harness, fixture=a.fixture, path=a.path, expect=a.expect,
                           duration_s=a.duration_s)
        print(json.dumps(rec))
    elif a.cmd == "report":
        with open(a.records) as fh:
            recs = [json.loads(line) for line in fh if line.strip()]
        print(render_recent(recs, a.recent))
    elif a.cmd == "model-route":
        print(model_route(a.config))
    elif a.cmd == "harness":
        print(execution_harness(a.config))
    return 0


if __name__ == "__main__":
    sys.exit(main())
