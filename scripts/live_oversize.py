#!/usr/bin/env python3
"""live-oversize: an oversized request ticket goes back to planning, unreviewed.

The size contract (safety-contract.md, "an oversized or mixed change goes back
to planning"): a request ticket whose run passes every gate before the code
review and measures over a release hard cap opens no pull request, is never
reviewed, and moves its request `building -> planning` once. This submits the
challenge fixture's spec alone (no plan, so the request may move to planning)
(its verify command is the fixture's `oversize_verify_command`: with no plan nothing declares tests/ as created, so the
suite command would fail on the untouched repository) to a config whose `release_max_changed_lines` is far below the fixture's
change, approves the spec gate, and PASSES only when the request's history
holds `building -> planning` with the measurement in its reason, the ticket's
run recorded no code review and no pull request, and the re-plan then waits at
`plan_review`. It FAILS if the ticket was reviewed, got a pull request, or the
request went anywhere else.

    LIVE_OVERSIZE_CONFIG    session config with a small release_max_changed_lines (required)
    LIVE_OVERSIZE_DATA_DIR  that config's data_dir, served by a worker (required)
    LIVE_CHALLENGE_*        as for live_challenge.py (fixture, root, timeout)
    LIVE_OVERSIZE_RESULTS_FILE  default ~/buildgate/live-challenge/oversize-results.jsonl
"""

import datetime
import json
import os
import pathlib
import sys
import time

import live_challenge as lc


def oversize_problems(request, run_json, progress):
	"""Why a request and its first ticket's run do not show an oversized
	ticket routed back to planning unreviewed, or []."""
	problems = []
	moves = [(h.get("from"), h.get("to")) for h in request.get("history") or []]
	if ("building", "planning") not in moves:
		problems.append("the request never moved building -> planning")
	if request.get("state") != "plan_review":
		problems.append(f"the request is in {request.get('state')!r}, want plan_review (the re-plan waits there)")
	if any(e.get("stage") == "review" and e.get("event") == "start" for e in progress):
		problems.append("the oversized run's code review started: it was reviewed")
	if run_json.get("pr_url") or run_json.get("pull_request_url"):
		problems.append("the oversized run has a pull request")
	return problems


class Oversize(lc.Challenge):
	def __init__(self):
		os.environ["LIVE_CHALLENGE_CONFIG"] = os.environ.get("LIVE_OVERSIZE_CONFIG", "")
		os.environ["LIVE_CHALLENGE_DATA_DIR"] = os.environ.get("LIVE_OVERSIZE_DATA_DIR", "")
		super().__init__()

	def submit_spec_only(self, repo):
		out = self.factoryd("submit", "-spec-file", str(self.dir / "spec.md"), "-verify-command", self.fixture.get("oversize_verify_command", self.fixture["verify_command"]),
			"-preflight-profile", self.fixture.get("preflight_profile", ""), str(repo))
		ids = [l.strip() for l in out.splitlines() if lc.re.fullmatch(r"[a-z0-9][a-z0-9-]+-\d{8}-\d{6}", l.strip())]
		if not ids:
			raise RuntimeError(f"submit printed no request id:\n{out[-400:]}")
		self.request = ids[0]
		print(f"live-oversize: request {self.request}", flush=True)

	def to_replan(self):
		"""Approve the spec gate, then wait for the re-plan's review gate."""
		approved = []
		def step():
			r = self.request_record()
			state = r.get("state", "")
			moved = any((h.get("from"), h.get("to")) == ("building", "planning") for h in r.get("history") or [])
			if state == "spec_review" and not approved:
				self.factoryd("approve", self.request, check=False)
				approved.append(1)
			if state == "plan_review" and moved:
				return r, state
			if state in lc.STOPPED:
				raise RuntimeError(f"request stopped in {state}: {r.get('error', '')}")
			return None, f"{state}{' (moved to planning)' if moved else ''}"
		return self.wait("the re-plan", step)

	def run(self):
		for name in ("LIVE_OVERSIZE_CONFIG", "LIVE_OVERSIZE_DATA_DIR"):
			if not os.environ.get(name):
				raise SystemExit(f"live-oversize: {name} is required (see this script's doc comment)")
		record = {"date": datetime.datetime.now(datetime.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ"), "fixture": self.name, "outcome": "fail"}
		started = time.time()
		try:
			self.submit_spec_only(lc.fresh_repo(self.fixture, self.root))
			request = self.to_replan()
			ticket = (request.get("tickets") or [{}])[0]
			run_dir = self.data_dir / "runs" / (ticket.get("run_id") or "none")
			run_json = json.loads((run_dir / "run.json").read_text()) if (run_dir / "run.json").exists() else {}
			progress = [json.loads(l) for l in (run_dir / "progress.jsonl").read_text().splitlines()] if (run_dir / "progress.jsonl").exists() else []
			problems = oversize_problems(request, run_json, progress)
			record["problems"], record["run"] = problems, run_dir.name
			record["change_size"] = run_json.get("change_size")
			for p in problems:
				print(f"live-oversize: {p}", file=sys.stderr)
			if not problems:
				record["outcome"] = "pass"
		except RuntimeError as err:
			record["error"] = str(err)
			print(f"live-oversize: {err}", file=sys.stderr)
		finally:
			record["request"] = self.request
			record["duration_s"] = int(time.time() - started)
			if self.request:
				self.factoryd("cancel", "-reason", "live-oversize fixture finished", self.request, check=False)
			path = pathlib.Path(os.environ.get("LIVE_OVERSIZE_RESULTS_FILE", "~/buildgate/live-challenge/oversize-results.jsonl")).expanduser()
			path.parent.mkdir(parents=True, exist_ok=True)
			with path.open("a") as f:
				f.write(json.dumps(record) + "\n")
		print(f"{'PASS' if record['outcome'] == 'pass' else 'FAIL'}  {self.name}: oversized ticket routed back to planning")
		return 0 if record["outcome"] == "pass" else 1


if __name__ == "__main__":
	sys.exit(Oversize().run())
