#!/usr/bin/env python3
"""baseline: what the recorded runs say about how builds go today.

It reads every `runs/*/run.json` under the directories it is given and
prints four numbers:

    one-shot acceptance   tickets whose first run was accepted with no
                          override and no rescue, over tickets with a
                          finished first run
    rounds to green       build rounds an accepted run took
    repeated failures     pairs of consecutive failed rounds in one run whose
                          failure was the same one, over all such pairs; and
                          the pairs whose second round changed no file
    what stops runs       the checks that failed on quarantined runs and why
                          halted ones halted (the reason code, else the triage
                          sentence)

The text output gives all four overall and a one-line summary per project;
--json gives all four for each project too. It gates nothing and changes
nothing: no factoryd, no Docker, no model, no network. Standard library only
(Python 3.9+).

    scripts/baseline.py ~/buildgate data
    scripts/baseline.py --json ~/buildgate
    scripts/baseline.py --project todo-kafka-service ~/buildgate
    scripts/baseline.py --exclude-project app ~/buildgate

It counts every run record it finds, and cannot tell a real ticket from a
smoke fixture, a fixture built to fail or a console walk's seeded records:
read the per-project rows, and use --project or --exclude-project to leave
those out.

How each number is counted:

- A run record copied into several data directories (a console walk seeds
  its own copy) is counted once, by run id; the copy updated last wins, and
  of two updated together, the one with a BUILD_REPORT.md beside it.
- A run still in progress is left out and counted as "unfinished".
- A ticket's first run is the one created first; a ticket is a name within
  a project. The runs of a PR-review or conformity round
  (`<request>-001-review2`, `<request>-001-conformity1`, either with `-fix1`)
  belong to the ticket they follow, so they are never a first run.
  A ticket whose first run is unfinished is not in the one-shot rate. A
  ticket submitted again gets a new name and counts as a new ticket.
- A round failed when its record lists blockers (`blockers`, with
  `failure_signature`: the names build_app.py's round feedback uses). A
  round with none listed, or from a run record that predates the field, has
  its blockers rebuilt from what the run kept: the round's outcome fields, the round-end
  line in progress.jsonl (which says whether the round failed and names one
  reason) and BUILD_REPORT.md (model route errors, a stall). The blocker
  names are the build loop's own (agent/pi/scripts/build_app.py,
  round_blockers). The round-end line names one reason only, so a round that
  timed out and changed nothing is rebuilt as timed out, and a failed
  reference oracle is rebuilt only when it was the round's first blocker.
- "The same failure" compares the round's recorded `failure_signature`. A
  record without one gets a signature derived by the build loop's function
  (agent/pi/scripts/round_feedback.py) from the rebuilt blockers, the failing
  output kept in BUILD_REPORT.md and the reviewer's findings. A derived
  signature is close to, not equal to, the one the loop would have recorded
  (the report keeps the end of the output, and a rebuilt blocker list can
  miss one), so recorded and derived pairs are counted separately and never
  compared with each other. A pair with a round that has neither is "not
  comparable", never same or different.
- A round that changed no file carries its own blocker, so its signature
  differs from the round before even when the same command failed the same
  way. Those pairs are counted on their own line: read it beside the
  same-failure share, not instead of it.
"""
from __future__ import annotations

import argparse
import glob
import json
import os
import pathlib
import re
import statistics
import sys
from collections import Counter
from datetime import datetime
from typing import Any, Dict, Iterable, List, Optional, Tuple

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parents[1] / "agent" / "pi" / "scripts"))
import round_feedback  # noqa: E402

TERMINAL_STATES = ("accepted", "quarantined", "halted")
NO_FAILED_CHECK = "(no failed check recorded)"
NO_HALT_REASON = "(no reason recorded)"
HALT_REASON_LIMIT = 80
NO_PROJECT = "(no project recorded)"

SIGNATURE_RECORDED = "recorded"
SIGNATURE_DERIVED = "derived"

REPORT_FILE = "BUILD_REPORT.md"
PROGRESS_FILE = "progress.jsonl"


# ---- reading ----------------------------------------------------------------


def find_run_files(roots: Iterable[str]) -> List[str]:
	"""Every runs/*/run.json at any depth under roots, sorted."""
	found = set()
	for root in roots:
		base = os.path.expanduser(root)
		found.update(glob.glob(os.path.join(base, "**", "runs", "*", "run.json"), recursive=True))
		found.update(glob.glob(os.path.join(base, "runs", "*", "run.json")))
	return sorted(found)


_TIMESTAMP = re.compile(r"^(\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2})(\.\d+)?(Z|[+-]\d{2}:\d{2})?$")


def instant(value: Any) -> Tuple[int, float, str]:
	"""A sort key for an RFC 3339 timestamp that orders by the moment it
	names, whatever its UTC offset or fraction. A value that is not one
	sorts after every timestamp, by its text."""
	text = str(value or "")
	match = _TIMESTAMP.match(text)
	if match:
		fraction = (match.group(2) or ".0")[:7]
		offset = "+00:00" if match.group(3) in (None, "Z") else match.group(3)
		try:
			return (0, datetime.fromisoformat(match.group(1) + fraction.ljust(7, "0") + offset).timestamp(), "")
		except ValueError:
			pass
	return (1, 0.0, text)


def load_runs(paths: Iterable[str]) -> Tuple[List[Dict[str, Any]], List[str]]:
	"""The run records at paths, one per run id, and the paths that could
	not be read as a run record. Of several copies of a run the one updated
	last is kept; of copies updated together, one with a build report
	beside it. Each record gets a "_path" key naming its file."""
	by_id: Dict[str, Tuple[Any, Dict[str, Any]]] = {}
	unreadable: List[str] = []
	for path in paths:
		try:
			with open(path) as fh:
				record = json.load(fh)
		except (OSError, ValueError, RecursionError):
			unreadable.append(path)
			continue
		if not isinstance(record, dict) or not isinstance(record.get("id"), str) or not record["id"]:
			unreadable.append(path)
			continue
		record["_path"] = path
		updated = instant(record.get("updated_at"))
		# A copy with no readable update time loses to one that has it.
		rank = (updated[0] == 0, updated, os.path.isfile(os.path.join(os.path.dirname(path), REPORT_FILE)))
		kept = by_id.get(record["id"])
		if kept is None or rank > kept[0]:
			by_id[record["id"]] = (rank, record)
	return [by_id[run_id][1] for run_id in sorted(by_id)], unreadable


def read_beside(record: Dict[str, Any], name: str) -> str:
	"""The text of the file called name in the run's own directory, "" when
	the record came from no file or has none."""
	path = record.get("_path")
	if not path:
		return ""
	try:
		with open(os.path.join(os.path.dirname(path), name), errors="replace") as fh:
			return fh.read()
	except OSError:
		return ""


# ---- rounds -----------------------------------------------------------------


def run_rounds(record: Dict[str, Any]) -> List[Dict[str, Any]]:
	evidence = record.get("agent_evidence")
	if not isinstance(evidence, dict):
		return []
	rounds = evidence.get("rounds")
	if not isinstance(rounds, list):
		return []
	return [r for r in rounds if isinstance(r, dict)]


def round_ends(progress: str) -> Dict[int, Tuple[bool, str]]:
	"""(failed, detail) for each round, by round index, from the round-end
	lines the build loop wrote to progress.jsonl. A later line for the same
	round (a resumed build) replaces an earlier one."""
	ends: Dict[int, Tuple[bool, str]] = {}
	for line in progress.splitlines():
		if '"round"' not in line:
			continue
		try:
			event = json.loads(line)
		except (ValueError, RecursionError):
			continue
		if not isinstance(event, dict) or event.get("stage") != "round" or event.get("event") != "end":
			continue
		if isinstance(event.get("round"), int) and event.get("outcome") in ("pass", "fail"):
			ends[event["round"]] = (event["outcome"] == "fail", str(event.get("detail") or ""))
	return ends


_ROUND_HEADING = re.compile(r"^### Round (\d+)\n- agent: ", re.MULTILINE)
_VERDICTS_HEADING = "\n## Independent review verdicts\n"
_BEFORE_OUTPUT = re.compile(r"^- verify passed: .*\n- duration: [\d.]+s$", re.MULTILINE)
_ROUTE_ERRORS = re.compile(r"^- agent turns errored: (\d+)/(\d+) \(model route unreachable\)$", re.MULTILINE)
_STALLED = re.compile(r"^- extension traces: .*=stall-timeout\b", re.MULTILINE)


def report_rounds(report: str) -> Dict[int, Dict[str, Any]]:
	"""What BUILD_REPORT.md keeps about each round beyond the run record, by
	round index: "output" (the failing output, the fenced block that closes
	the round's section; "" when it kept none), "route_errors" ((errored,
	total) when every assistant turn errored, else None) and "stalled".

	The report is prose around agent-written text, so this reads its fixed
	lines: a round starts at a "### Round N" line followed by "- agent: ",
	the rounds end at the last "## Independent review verdicts", and the
	output is the fence after the "- verify passed" and "- duration" pair."""
	cut = report.rfind(_VERDICTS_HEADING)
	rounds_part = report if cut == -1 else report[:cut]
	headings = list(_ROUND_HEADING.finditer(rounds_part))
	out: Dict[int, Dict[str, Any]] = {}
	for i, heading in enumerate(headings):
		end = headings[i + 1].start() if i + 1 < len(headings) else len(rounds_part)
		section = rounds_part[heading.start():end]
		before = _BEFORE_OUTPUT.search(section)
		facts, output = section, ""
		if before is not None:
			after = section[before.end():]
			opened = after.find("```\n")
			closed = after.rfind("\n```")
			if opened != -1 and closed > opened:
				# The traces line sits between the duration and the fence.
				facts, output = section[:before.end()] + after[:opened], after[opened + 4:closed]
		errors = _ROUTE_ERRORS.search(facts)
		out[int(heading.group(1))] = {
			"output": output,
			"route_errors": (int(errors.group(1)), int(errors.group(2))) if errors else None,
			"stalled": _STALLED.search(facts) is not None,
		}
	return out


def rebuilt_blockers(rnd: Dict[str, Any], review_policy: str, end: Optional[Tuple[bool, str]], kept: Dict[str, Any]) -> List[str]:
	"""The blockers of a round that recorded none, under the names
	build_app.py's round_blockers gives them. end is the round's (failed,
	detail) from progress.jsonl and kept its report_rounds entry; either may
	be missing. Empty for a round that passed."""
	if end is not None and not end[0]:
		return []
	blockers: List[str] = []
	detail = end[1] if end is not None else ""
	if detail == "no changes made":
		blockers.append(NO_CHANGES)
	if rnd.get("agent_timed_out"):
		blockers.append("pi invocation timed out")
	elif rnd.get("agent_returncode") not in (0, None):
		blockers.append("pi invocation failed")
	errors = kept.get("route_errors")
	if errors:
		blockers.append("model route unreachable (%d/%d assistant turns errored)" % errors)
	if kept.get("stalled"):
		blockers.append("stall-timeout")
	if rnd.get("fast_check_ran") and rnd.get("fast_check_passed") is False:
		blockers.append("fast check failed")
	elif rnd.get("verify_passed") is not True:
		blockers.append("canonical verification failed")
	outcome = rnd.get("reviewer_outcome")
	if outcome == "flagged" and review_policy != "advisory":
		blockers.append("reviewer flagged the current diff")
	elif outcome != "clean" and review_policy == "required":
		blockers.append("review unavailable (%s)" % (rnd.get("reviewer_detail") or ""))
	# The round-end line names the first blocker when it is none of the
	# above cases: the only trace of one this record cannot show otherwise.
	if detail and detail not in ("no changes made", "timed out") and not detail.startswith("verify failed: ") and detail not in blockers:
		blockers.append(detail)
	if not blockers and end is not None:
		blockers.append("round failed")
	return blockers


NO_CHANGES = "no changes made to the workspace"


def round_signatures(record: Dict[str, Any], report: str, progress: str) -> List[Tuple[int, bool, str, str, bool]]:
	"""(index, failed, signature, signature source, changed nothing) for
	each round of the run. The signature is "" for a round that passed and
	for a failed round with neither a recorded signature nor a section in
	the report."""
	evidence = record.get("agent_evidence") if isinstance(record.get("agent_evidence"), dict) else {}
	policy = str(evidence.get("review_policy") or "")
	kept: Optional[Dict[int, Dict[str, Any]]] = None
	ends: Optional[Dict[int, Tuple[bool, str]]] = None
	out = []
	for position, rnd in enumerate(run_rounds(record), start=1):
		index = rnd.get("index") if isinstance(rnd.get("index"), int) else position
		recorded_blockers = rnd.get("blockers")
		# An empty recorded list is not proof of a pass: a round the build
		# loop restored or built without computing its blockers has one
		# too. Such a round is read like one that recorded none.
		if isinstance(recorded_blockers, list) and recorded_blockers:
			signature = rnd.get("failure_signature")
			usable = isinstance(signature, str) and signature != ""
			out.append((index, True, signature if usable else "", SIGNATURE_RECORDED if usable else "", NO_CHANGES in recorded_blockers))
			continue
		if kept is None:
			kept, ends = report_rounds(report), round_ends(progress)
		blockers = rebuilt_blockers(rnd, policy, ends.get(index), kept.get(index, {}))
		# A round the report has no section for cannot be told from one
		# that failed with no output; one it has is hashed as the loop
		# hashes it, output or none.
		if not blockers or index not in kept:
			out.append((index, bool(blockers), "", "", NO_CHANGES in blockers))
			continue
		output = kept[index]["output"]
		detail = str(rnd.get("reviewer_detail") or "") if rnd.get("reviewer_outcome") == "flagged" else ""
		out.append((index, True, round_feedback.failure_signature(blockers, output, detail), SIGNATURE_DERIVED, NO_CHANGES in blockers))
	return out


# ---- measures ---------------------------------------------------------------

_FOLLOW_UP_RUN = re.compile(r"(?<=-\d{3})-(?:review|conformity)\d+(?:-fix\d+)?$")


def ticket_of(record: Dict[str, Any]) -> Tuple[str, str]:
	"""The ticket a run belongs to, as (project, ticket): its own ticket
	with a PR-review or conformity round's suffix removed. The suffix
	follows the ticket's three-digit index, so a ticket merely named
	"api-review1" keeps its name. A run with no ticket is its own ticket."""
	return (project_of(record), _FOLLOW_UP_RUN.sub("", str(record.get("ticket") or record["id"])))


def first_runs(runs: List[Dict[str, Any]]) -> List[Dict[str, Any]]:
	"""The first run of each ticket: the one created first, the run id
	breaking a tie."""
	by_ticket: Dict[Tuple[str, str], Tuple[Any, Dict[str, Any]]] = {}
	for record in runs:
		key = (instant(record.get("created_at")), record["id"])
		kept = by_ticket.get(ticket_of(record))
		if kept is None or key < kept[0]:
			by_ticket[ticket_of(record)] = (key, record)
	return [by_ticket[t][1] for t in sorted(by_ticket)]


def is_one_shot(record: Dict[str, Any]) -> bool:
	return record.get("state") == "accepted" and not record.get("overrides") and not record.get("rescues")


def failed_checks(record: Dict[str, Any]) -> List[str]:
	"""The checks a quarantined run failed, each once; its reason code, in
	brackets, when it recorded no failed check."""
	names: List[str] = []
	gates = record.get("gate_results")
	for gate in gates if isinstance(gates, list) else []:
		if isinstance(gate, dict) and gate.get("passed") is False:
			name = str(gate.get("check") or "")
			if name and name not in names:
				names.append(name)
	if names:
		return names
	code = str(record.get("halt_reason_code") or "")
	return ["(%s)" % code if code else NO_FAILED_CHECK]


def halt_reason(record: Dict[str, Any]) -> str:
	"""Why a run halted: its reason code, else the first line of its triage
	sentence, which is what an operator was told."""
	code = str(record.get("halt_reason_code") or "")
	if code:
		return code
	triage = str(record.get("triage") or "").strip().splitlines()
	text = triage[0] if triage else ""
	if text.startswith("halted: "):
		text = text[len("halted: "):]
	return text[:HALT_REASON_LIMIT] or NO_HALT_REASON


def count_pairs(signatures: List[Tuple[int, bool, str, str, bool]], pairs: Dict[str, Dict[str, int]]) -> None:
	"""Add one run's consecutive failed-round pairs to pairs."""
	for before, after in zip(signatures, signatures[1:]):
		if not (before[1] and after[1]):
			continue
		pairs["all"]["pairs"] += 1
		if after[4]:
			pairs["all"]["second_changed_nothing"] += 1
		if not before[2] or not after[2] or before[3] != after[3]:
			pairs["all"]["not_comparable"] += 1
			continue
		pairs[before[3]]["same" if before[2] == after[2] else "different"] += 1


def measure(runs: List[Dict[str, Any]], beside: Optional[Dict[str, Dict[str, str]]] = None) -> Dict[str, Any]:
	"""The baseline over runs (one record per run id). beside maps a run id
	to {"report": ..., "progress": ...}, the text of its BUILD_REPORT.md
	and progress.jsonl; a run absent from it has them read from its own
	directory."""
	beside = beside or {}
	states = Counter(str(r.get("state") or "") for r in runs)
	finished = [r for r in runs if r.get("state") in TERMINAL_STATES]

	firsts = [r for r in first_runs(runs) if r.get("state") in TERMINAL_STATES]
	one_shot = sum(1 for r in firsts if is_one_shot(r))

	rounds_to_green = sorted(len(run_rounds(r)) for r in finished if r.get("state") == "accepted" and run_rounds(r))

	pairs = {"all": {"pairs": 0, "not_comparable": 0, "second_changed_nothing": 0}, SIGNATURE_RECORDED: {"same": 0, "different": 0}, SIGNATURE_DERIVED: {"same": 0, "different": 0}}
	multi_round = 0
	for record in finished:
		if len(run_rounds(record)) < 2:
			continue
		multi_round += 1
		texts = beside.get(record["id"])
		if texts is None:
			texts = {"report": read_beside(record, REPORT_FILE), "progress": read_beside(record, PROGRESS_FILE)}
		count_pairs(round_signatures(record, texts.get("report", ""), texts.get("progress", "")), pairs)

	quarantine_checks: Counter = Counter()
	for record in finished:
		if record.get("state") == "quarantined":
			quarantine_checks.update(failed_checks(record))
	halt_reasons = Counter(halt_reason(r) for r in finished if r.get("state") == "halted")

	recorded, derived = pairs[SIGNATURE_RECORDED], pairs[SIGNATURE_DERIVED]
	return {
		"runs": len(runs),
		"finished": len(finished),
		"unfinished": len(runs) - len(finished),
		"accepted": states.get("accepted", 0),
		"quarantined": states.get("quarantined", 0),
		"halted": states.get("halted", 0),
		"tickets": len(firsts),
		"one_shot": one_shot,
		"one_shot_rate": ratio(one_shot, len(firsts)),
		"rounds_to_green": {
			"runs": len(rounds_to_green),
			"mean": round(statistics.mean(rounds_to_green), 2) if rounds_to_green else None,
			"median": statistics.median(rounds_to_green) if rounds_to_green else None,
			"max": rounds_to_green[-1] if rounds_to_green else None,
			"first_round": sum(1 for n in rounds_to_green if n == 1),
			"histogram": {str(n): c for n, c in sorted(Counter(rounds_to_green).items())},
		},
		"multi_round_runs": multi_round,
		"failed_round_pairs": {
			"pairs": pairs["all"]["pairs"],
			"not_comparable": pairs["all"]["not_comparable"],
			"second_changed_nothing": pairs["all"]["second_changed_nothing"],
			"recorded": {**recorded, "same_rate": ratio(recorded["same"], recorded["same"] + recorded["different"])},
			"derived": {**derived, "same_rate": ratio(derived["same"], derived["same"] + derived["different"])},
		},
		"quarantine_checks": dict(quarantine_checks.most_common()),
		"halt_reasons": dict(halt_reasons.most_common()),
	}


def ratio(part: int, whole: int) -> Optional[float]:
	return round(part / whole, 4) if whole else None


def project_of(record: Dict[str, Any]) -> str:
	return str(record.get("project") or NO_PROJECT)


def baseline(runs: List[Dict[str, Any]], beside: Optional[Dict[str, Dict[str, str]]] = None) -> Dict[str, Any]:
	"""measure() over all runs and over each project's runs."""
	by_project: Dict[str, List[Dict[str, Any]]] = {}
	for record in runs:
		by_project.setdefault(project_of(record), []).append(record)
	return {
		"overall": measure(runs, beside),
		"projects": {name: measure(by_project[name], beside) for name in sorted(by_project)},
	}


# ---- rendering --------------------------------------------------------------


def share(part: int, whole: int) -> str:
	if not whole:
		return "none to count"
	return "%d of %d (%d%%)" % (part, whole, round(100 * part / whole))


def render(result: Dict[str, Any], files: int, unreadable: List[str]) -> str:
	overall = result["overall"]
	green = overall["rounds_to_green"]
	pairs = overall["failed_round_pairs"]
	lines = [
		"Baseline over %d run record file(s): %d run(s) after counting each run id once, %d finished, %d unfinished"
		% (files, overall["runs"], overall["finished"], overall["unfinished"]),
		"",
		"| Measure | Value |",
		"|---|---|",
		"| Finished runs | %d accepted, %d quarantined, %d halted |"
		% (overall["accepted"], overall["quarantined"], overall["halted"]),
		"| One-shot acceptance (first run of a ticket accepted, no override or rescue) | %s |"
		% share(overall["one_shot"], overall["tickets"]),
		"| Rounds to green (accepted runs with recorded rounds) | %s |" % render_rounds(green),
		"| Finished runs with more than one round | %s |" % share(overall["multi_round_runs"], overall["finished"]),
		"| Pairs of consecutive failed rounds | %d, %d of them not comparable |" % (pairs["pairs"], pairs["not_comparable"]),
		"| Pairs whose second round changed no file | %s |" % share(pairs["second_changed_nothing"], pairs["pairs"]),
		"| Same failure twice running, by the signature the build recorded | %s |" % pair_share(pairs["recorded"]),
		"| Same failure twice running, by a signature derived from the build report | %s |" % pair_share(pairs["derived"]),
	]
	lines += ["", "| Check that failed on a quarantined run | Runs |", "|---|---|"]
	lines += ["| %s | %d |" % (name, count) for name, count in overall["quarantine_checks"].items()] or ["| (none) | 0 |"]
	lines += ["", "| Reason a run halted | Runs |", "|---|---|"]
	lines += ["| %s | %d |" % (name, count) for name, count in overall["halt_reasons"].items()] or ["| (none) | 0 |"]
	lines += [
		"",
		"| Project | Runs | Accepted | Quarantined | Halted | One-shot | Mean rounds to green | Same failure (recorded) | Same failure (derived) |",
		"|---|---|---|---|---|---|---|---|---|",
	]
	for name, m in result["projects"].items():
		p = m["failed_round_pairs"]
		lines.append(
			"| %s | %d | %d | %d | %d | %s | %s | %s | %s |"
			% (
				name, m["runs"], m["accepted"], m["quarantined"], m["halted"],
				share(m["one_shot"], m["tickets"]),
				"-" if m["rounds_to_green"]["mean"] is None else m["rounds_to_green"]["mean"],
				pair_share(p["recorded"]), pair_share(p["derived"]),
			)
		)
	if unreadable:
		lines += ["", "Not read as a run record (%d):" % len(unreadable)] + ["  " + p for p in unreadable]
	return "\n".join(lines) + "\n"


def render_rounds(green: Dict[str, Any]) -> str:
	if not green["runs"]:
		return "none to count"
	return "mean %s, median %s, max %s over %d run(s); %s in the first round" % (
		green["mean"], green["median"], green["max"], green["runs"], share(green["first_round"], green["runs"]),
	)


def pair_share(kind: Dict[str, Any]) -> str:
	return share(kind["same"], kind["same"] + kind["different"])


def main(argv: Optional[List[str]] = None) -> int:
	parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
	parser.add_argument("roots", nargs="+", help="directories to search for runs/*/run.json, at any depth")
	parser.add_argument("--json", action="store_true", help="print the numbers as JSON")
	parser.add_argument("--project", action="append", default=[], help="count only this project's runs (repeatable)")
	parser.add_argument("--exclude-project", action="append", default=[], help="leave this project's runs out (repeatable)")
	args = parser.parse_args(argv)

	roots = [root for root in args.roots if os.path.isdir(os.path.expanduser(root))]
	for root in args.roots:
		if root not in roots:
			print("baseline: %s is not a directory; skipped" % root, file=sys.stderr)
	if not roots:
		print("baseline: none of the given directories exists", file=sys.stderr)
		return 2

	files = find_run_files(roots)
	runs, unreadable = load_runs(files)
	if args.project:
		runs = [r for r in runs if project_of(r) in args.project]
	runs = [r for r in runs if project_of(r) not in args.exclude_project]
	if not runs:
		print("baseline: no run record found under %s" % ", ".join(roots), file=sys.stderr)
	result = baseline(runs)
	if args.json:
		json.dump({"files": len(files), "unreadable": unreadable, **result}, sys.stdout, indent=2)
		sys.stdout.write("\n")
	else:
		sys.stdout.write(render(result, len(files), unreadable))
	return 0


if __name__ == "__main__":
	sys.exit(main())
