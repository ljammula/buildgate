#!/usr/bin/env python3
"""live-challenge: one real challenger job, run through a request.

`make live-smoke` drives the single-ticket run path, where no challenger job
exists (SC-022 applies to a request's ticket builds), so it proves nothing
about the challenger. This does: it copies a fixture repository to a fresh
git repository, hands the fixture's finished spec and plan to `factoryd
submit`, approves the two review gates, and waits for the ticket's run to end.
The run PASSES only when it was accepted and its record shows the challenger
job ran: a green or red verdict, or a no-verdict whose cause is not "no
challenger role configured" and whose record holds the job's instruction-file
snapshot (SC-022, SC-019). A run with no challenge record, or a challenge
recorded without a job, FAILS: nothing here accepts "the check did not run".

It makes real model calls and needs the operator's working setup, as
live-smoke does (Docker, the gateway and meter, a model route), plus a worker
draining LIVE_CHALLENGE_DATA_DIR (`factoryd worker -config <config>`) and
`roles.challenger` in the config. No pull request is opened and no GitHub
account is used. Each run appends one line to the results file.

    LIVE_CHALLENGE_CONFIG    session config or profile name passed as -config (required)
    LIVE_CHALLENGE_DATA_DIR  that config's data_dir (required: the records are read from it)
    LIVE_CHALLENGE_FIXTURE   fixture directory name (default math-ops)
    LIVE_CHALLENGE_ROOT      where the fresh repository is made (default
                             ~/buildgate/live-challenge: the Docker VM must see it)
    LIVE_CHALLENGE_TIMEOUT   minutes to wait for each phase (default 60)
    LIVE_CHALLENGE_RESULTS_FILE  default ~/buildgate/live-challenge/results.jsonl
    FACTORYD_BIN             default factoryd
"""

import datetime
import json
import os
import pathlib
import re
import shutil
import subprocess
import sys
import time

REPO_ROOT = pathlib.Path(__file__).resolve().parent.parent
FIXTURES = pathlib.Path(__file__).resolve().parent / "live-challenge"
STOPPED = {"halted", "quarantined", "cancelled", "done", "closed", "resume_review"}
RUN_ENDED = {"accepted", "quarantined", "halted", "failed", "rejected"}
NO_ROLE = "no challenger role configured"


def results_file():
	return pathlib.Path(os.environ.get("LIVE_CHALLENGE_RESULTS_FILE", "~/buildgate/live-challenge/results.jsonl")).expanduser()


def challenge_problems(run):
	"""Why a run's record does not show a challenger job that ran, or [] when
	it does. A verdict of green or red is the job's own; a no-verdict counts
	only with the job's instruction-file snapshot, which exists only once the
	job's step returned, and a cause other than a missing role."""
	c = run.get("challenge")
	if not c:
		return ["the run has no challenge record: no challenger job ran"]
	verdict = c.get("verdict")
	if verdict in ("green", "red"):
		return []
	if verdict != "no_verdict":
		return [f"the challenge record has verdict {verdict!r}"]
	cause = c.get("cause", "")
	if cause == NO_ROLE:
		return ["no challenger role is configured: no challenger job ran"]
	if not c.get("snapshot_sha256"):
		return [f"no-verdict ({cause or 'no cause'}) without the job's snapshot: the job never returned"]
	return []


def sh(args, check=True):
	p = subprocess.run(args, capture_output=True, text=True)
	if check and p.returncode != 0:
		raise RuntimeError(f"{' '.join(args[:3])} ...: exit {p.returncode}: {p.stderr.strip()[-600:]}")
	return p.stdout


def fresh_repo(fixture, root):
	"""A new git repository holding the fixture's source tree, one commit."""
	src = REPO_ROOT / fixture["source"]
	name = f"{fixture['repo']}-{datetime.datetime.now():%Y%m%d-%H%M%S}"
	dest = pathlib.Path(root).expanduser() / name
	shutil.copytree(src, dest)
	git = ["git", "-C", str(dest), "-c", "user.name=live-challenge", "-c", "user.email=live-challenge@localhost"]
	sh(git + ["init", "-q", "-b", "main"])
	sh(git + ["add", "-A"])
	sh(git + ["commit", "-q", "-m", "fixture"])
	return dest


class Challenge:
	def __init__(self):
		self.bin = os.environ.get("FACTORYD_BIN", "factoryd")
		self.config = os.environ.get("LIVE_CHALLENGE_CONFIG", "")
		self.data_dir = pathlib.Path(os.environ.get("LIVE_CHALLENGE_DATA_DIR", "")).expanduser()
		self.root = os.environ.get("LIVE_CHALLENGE_ROOT", "~/buildgate/live-challenge")
		self.timeout = 60 * int(os.environ.get("LIVE_CHALLENGE_TIMEOUT", "60"))
		self.name = os.environ.get("LIVE_CHALLENGE_FIXTURE", "math-ops")
		self.dir = FIXTURES / self.name
		self.fixture = json.loads((self.dir / "fixture.json").read_text())
		self.request = ""

	def factoryd(self, verb, *args, check=True):
		return sh([self.bin, verb, "-config", self.config] + list(args), check=check)

	def request_record(self):
		return json.loads((self.data_dir / "requests" / self.request / "request.json").read_text())

	def wait(self, what, done):
		deadline = time.time() + self.timeout
		last = None
		while time.time() < deadline:
			value, note = done()
			if note != last:
				print(f"live-challenge: {datetime.datetime.now():%H:%M:%S} {what}: {note}", flush=True)
				last = note
			if value is not None:
				return value
			time.sleep(15)
		raise RuntimeError(f"timed out after {self.timeout // 60}m waiting for {what} ({last})")

	def submit(self, repo):
		out = self.factoryd(
			"submit", "-spec-file", str(self.dir / "spec.md"), "-plan-dir", str(self.dir / "plan"),
			"-verify-command", self.fixture["verify_command"], "-preflight-profile", self.fixture.get("preflight_profile", ""), str(repo),
		)
		ids = [line.strip() for line in out.splitlines() if re.fullmatch(r"[a-z0-9][a-z0-9-]+-\d{8}-\d{6}", line.strip())]
		if not ids:
			raise RuntimeError(f"submit printed no request id:\n{out[-400:]}")
		self.request = ids[0]
		print(f"live-challenge: request {self.request}", flush=True)

	def to_ended_run(self):
		"""Approve the two gates, then wait for the ticket's run to end."""
		def step():
			r = self.request_record()
			state = r.get("state", "")
			if state in ("spec_review", "plan_review"):
				self.factoryd("approve", self.request, check=False)
				return None, state
			tickets = r.get("tickets") or []
			run_id = tickets[0].get("run_id", "") if tickets else ""
			if run_id:
				path = self.data_dir / "runs" / run_id / "run.json"
				if path.exists():
					run = json.loads(path.read_text())
					if run.get("state") in RUN_ENDED:
						return run, f"run {run_id} {run['state']}"
			if state in STOPPED:
				raise RuntimeError(f"request stopped before its ticket's run ended: {state}: {r.get('error', '')}")
			return None, f"{state}, run {run_id or 'not started'}"
		return self.wait("the ticket's run", step)

	def run(self):
		for name, value in (("LIVE_CHALLENGE_CONFIG", self.config), ("LIVE_CHALLENGE_DATA_DIR", str(self.data_dir) if os.environ.get("LIVE_CHALLENGE_DATA_DIR") else "")):
			if not value:
				raise SystemExit(f"live-challenge: {name} is required (see this script's doc comment)")
		record = {"date": datetime.datetime.now(datetime.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ"), "fixture": self.name,
			"factoryd_version": (sh([self.bin, "version"], check=False).splitlines() or ["unknown"])[0], "outcome": "fail"}
		started = time.time()
		try:
			self.submit(fresh_repo(self.fixture, self.root))
			run = self.to_ended_run()
			record["run"] = run.get("id") or run.get("run_id", "")
			record["run_state"] = run.get("state", "")
			c = run.get("challenge") or {}
			record["challenge_verdict"], record["challenge_cause"] = c.get("verdict", ""), c.get("cause", "")
			problems = challenge_problems(run)
			if run.get("state") != "accepted":
				problems.append(f"run state {run.get('state')!r}, want accepted")
			record["problems"] = problems
			for p in problems:
				print(f"live-challenge: {p}", file=sys.stderr)
			if not problems:
				record["outcome"] = "pass"
		except RuntimeError as err:
			record["error"] = str(err)
			print(f"live-challenge: {err}", file=sys.stderr)
		finally:
			record["request"] = self.request
			record["duration_s"] = int(time.time() - started)
			if self.request:
				self.factoryd("cancel", "-reason", "live-challenge fixture finished", self.request, check=False)
			path = results_file()
			path.parent.mkdir(parents=True, exist_ok=True)
			with path.open("a") as f:
				f.write(json.dumps(record) + "\n")
		print(f"{'PASS' if record['outcome'] == 'pass' else 'FAIL'}  {self.name}: challenge {record.get('challenge_verdict') or 'absent'}")
		return 0 if record["outcome"] == "pass" else 1


def main(argv):
	if argv[:1] == ["--results"]:
		path = results_file()
		for line in (path.read_text().splitlines() if path.exists() else [])[-20:]:
			r = json.loads(line)
			print(r["date"], r["fixture"], r["outcome"], r.get("challenge_verdict", ""), r.get("request", ""))
		return 0
	return Challenge().run()


if __name__ == "__main__":
	sys.exit(main(sys.argv[1:]))
