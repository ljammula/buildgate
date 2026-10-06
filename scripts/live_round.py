#!/usr/bin/env python3
"""live-round: one real PR-review corrective round, end to end.

`make live-smoke` drives the single-ticket run path and opens no pull
request, so nothing in it exercises what happens after a reviewer comments.
This does: it hands a fixture's finished spec and plan to `factoryd submit`,
approves the two review gates, waits for the ticket's pull request, posts
the fixture's review comments on it as a second GitHub account, and then
watches the pull request itself until every comment is answered or the
request stops.

A comment is RESOLVED when its thread holds the factory's "Addressed in
<sha>." reply and <sha> is a commit of the pull request. The run PASSES
when every comment is resolved, which the factory can only do within
max_review_rounds. Each run appends one line to the results file, and
--results prints the share of comments resolved over the recorded runs.

It acts on GitHub for real: a branch, a pull request and review comments on
LIVE_ROUND_REPO's origin. By default it closes the pull request, deletes
the branch and cancels the request when it ends.

    LIVE_ROUND_REPO      local clone of the fixture's repository (required)
    LIVE_ROUND_REVIEWER  gh account that posts the comments (required): logged
                         in with `gh auth login`, write access to the
                         repository, listed in the config's pr_trusted_authors,
                         and not the account factoryd pushes with
    LIVE_ROUND_CONFIG    session config or profile name passed as -config
    LIVE_ROUND_FIXTURE   fixture directory name (default todo-kafka-healthz)
    LIVE_ROUND_TIMEOUT   minutes to wait for each phase (default 90)
    LIVE_ROUND_KEEP=1    leave the pull request and the request as they end
    LIVE_ROUND_RESULTS_FILE  default ~/buildgate/live-round/results.jsonl
    FACTORYD_BIN         default factoryd

Prerequisites are the operator's working setup, as for live-smoke: Docker,
the gateway and meter, a model route, `open_pull_request: true`. A short
`pr_poll_interval` (1m) keeps the wait down.
"""

import datetime
import json
import os
import pathlib
import re
import subprocess
import sys
import time

FIXTURES = pathlib.Path(__file__).resolve().parent / "live-round"
ADDRESSED = re.compile(r"^Addressed in ([0-9a-f]{7,40})\.$")
NOT_PUSHED = re.compile(r"^Attempted in corrective round (\d+) of (\d+)")
STOPPED = {"halted", "quarantined", "cancelled", "done", "resume_review"}


def results_file():
	return pathlib.Path(os.environ.get("LIVE_ROUND_RESULTS_FILE", "~/buildgate/live-round/results.jsonl")).expanduser()


def added_line(diff, anchor):
	"""The new-file line number of the first line a unified diff adds that
	contains anchor, or None. A review comment must sit on a line of the
	pull request's own diff."""
	line = 0
	for row in diff.splitlines():
		hunk = re.match(r"^@@ -\d+(?:,\d+)? \+(\d+)", row)
		if hunk:
			line = int(hunk.group(1))
			continue
		if row.startswith("+++") or row.startswith("---") or row.startswith("\\"):
			continue
		if row.startswith("+"):
			if anchor in row[1:]:
				return line
			line += 1
		elif not row.startswith("-"):
			line += 1
	return None


def thread_outcomes(comments, roots, pr_commits):
	"""For each root comment id: (resolved, rounds_not_pushed). comments is
	GitHub's flat review-comment list (id, in_reply_to_id, body)."""
	outcomes = {}
	for root in roots:
		resolved, not_pushed = False, 0
		for c in comments:
			if c.get("in_reply_to_id") != root:
				continue
			body = (c.get("body") or "").strip()
			addressed = ADDRESSED.match(body)
			if addressed and any(sha.startswith(addressed.group(1)) for sha in pr_commits):
				resolved = True
			elif NOT_PUSHED.match(body):
				not_pushed += 1
		outcomes[root] = (resolved, not_pushed)
	return outcomes


def summarize(records):
	"""Totals over recorded runs: runs, passes, comments, resolved."""
	total = {"runs": 0, "passed": 0, "comments": 0, "resolved": 0}
	for r in records:
		total["runs"] += 1
		total["passed"] += 1 if r.get("outcome") == "pass" else 0
		total["comments"] += r.get("comments", 0)
		total["resolved"] += r.get("resolved", 0)
	return total


def read_results(path):
	if not path.exists():
		return []
	return [json.loads(line) for line in path.read_text().splitlines() if line.strip()]


def print_results(limit):
	records = read_results(results_file())
	for r in records[-limit:]:
		print(f"{r.get('date', '?'):20}  {r.get('fixture', '?'):22}  {r.get('outcome', '?'):5}  resolved {r.get('resolved', 0)}/{r.get('comments', 0)}  rounds not pushed {r.get('rounds_not_pushed', 0)}  {r.get('pull_request', '')}")
	t = summarize(records)
	share = f"{100 * t['resolved'] / t['comments']:.0f}%" if t["comments"] else "n/a"
	print(f"reviewer comments resolved within the cap: {t['resolved']}/{t['comments']} ({share}) over {t['runs']} run(s), {t['passed']} passed")


def sh(args, env=None, check=True, cwd=None):
	p = subprocess.run(args, capture_output=True, text=True, env=env, cwd=cwd)
	if check and p.returncode != 0:
		raise RuntimeError(f"{' '.join(args[:3])} ...: exit {p.returncode}: {p.stderr.strip()[-600:]}")
	return p.stdout


class Round:
	def __init__(self):
		self.bin = os.environ.get("FACTORYD_BIN", "factoryd")
		self.repo = os.environ.get("LIVE_ROUND_REPO", "")
		self.reviewer = os.environ.get("LIVE_ROUND_REVIEWER", "")
		self.config = os.environ.get("LIVE_ROUND_CONFIG", "")
		self.name = os.environ.get("LIVE_ROUND_FIXTURE", "todo-kafka-healthz")
		self.timeout = 60 * int(os.environ.get("LIVE_ROUND_TIMEOUT", "90"))
		self.dir = FIXTURES / self.name
		self.fixture = json.loads((self.dir / "fixture.json").read_text())
		self.request = ""
		self.pr = ""

	def factoryd(self, verb, *args, check=True):
		cmd = [self.bin, verb]
		if self.config:
			cmd += ["-config", self.config]
		return sh(cmd + list(args), check=check)

	def state(self):
		out = json.loads(self.factoryd("status", "-json", "-n", "200"))
		for r in out.get("requests") or []:
			if r.get("id") == self.request:
				return r
		return {}

	def gh(self, *args, as_reviewer=False, check=True):
		env = dict(os.environ)
		if as_reviewer:
			# The reviewer's token goes to gh through its environment only.
			env["GH_TOKEN"] = sh(["gh", "auth", "token", "--user", self.reviewer]).strip()
		return sh(["gh"] + list(args), env=env, check=check, cwd=self.repo)

	def wait(self, what, done):
		deadline = time.time() + self.timeout
		last = None
		while time.time() < deadline:
			value, note = done()
			if note != last:
				print(f"live-round: {datetime.datetime.now():%H:%M:%S} {what}: {note}", flush=True)
				last = note
			if value is not None:
				return value
			time.sleep(20)
		raise RuntimeError(f"timed out after {self.timeout // 60}m waiting for {what} ({last})")

	def submit(self):
		out = self.factoryd(
			"submit", "-spec-file", str(self.dir / "spec.md"), "-plan-dir", str(self.dir / "plan"),
			"-verify-command", self.fixture["verify_command"], "-preflight-profile", self.fixture.get("preflight_profile", ""), self.repo,
		)
		ids = [line.strip() for line in out.splitlines() if re.fullmatch(r"[a-z0-9][a-z0-9-]+-\d{8}-\d{6}", line.strip())]
		if not ids:
			raise RuntimeError(f"submit printed no request id:\n{out[-400:]}")
		self.request = ids[0]
		print(f"live-round: request {self.request}", flush=True)

	def to_pull_request(self):
		def step():
			r = self.state()
			state = r.get("state", "")
			if state in ("spec_review", "plan_review"):
				self.factoryd("approve", self.request, check=False)
			elif state == "pr_review" and r.get("pull_request_url"):
				return r["pull_request_url"], state
			elif state in STOPPED:
				raise RuntimeError(f"request stopped before a pull request: {state}: {r.get('error', '')}")
			return None, state
		self.pr = self.wait("pull request", step)
		print(f"live-round: pull request {self.pr}", flush=True)

	def pr_json(self, fields):
		return json.loads(self.gh("pr", "view", self.pr, "--json", fields))

	def comment(self):
		"""Post each fixture comment on a line the pull request adds; return
		the root comment ids."""
		view = self.pr_json("headRefOid,baseRefName,number")
		slug = re.match(r"https://github\.com/([^/]+/[^/]+)/pull/", self.pr).group(1)
		diff = self.gh("pr", "diff", self.pr)
		roots = []
		for c in self.fixture["comments"]:
			file_diff = "".join(part for part in re.split(r"(?m)^(?=diff --git )", diff) if part.startswith(f"diff --git a/{c['path']} "))
			line = added_line(file_diff, c["anchor"])
			if line is None:
				raise RuntimeError(f"the pull request adds no line in {c['path']} containing {c['anchor']!r}; the fixture comment has nowhere to go")
			out = self.gh(
				"api", f"repos/{slug}/pulls/{view['number']}/comments", "-f", f"body={c['body']}", "-f", f"commit_id={view['headRefOid']}",
				"-f", f"path={c['path']}", "-F", f"line={line}", "-f", "side=RIGHT", as_reviewer=True,
			)
			roots.append(json.loads(out)["id"])
			print(f"live-round: {self.reviewer} commented on {c['path']}:{line}", flush=True)
		return slug, view["number"], roots

	def watch(self, slug, number, roots):
		def step():
			comments = json.loads(self.gh("api", "--paginate", "--slurp", f"repos/{slug}/pulls/{number}/comments") or "[]")
			comments = [c for page in comments for c in page]
			commits = [c["oid"] for c in self.pr_json("commits")["commits"]]
			outcomes = thread_outcomes(comments, roots, commits)
			resolved = sum(1 for ok, _ in outcomes.values() if ok)
			not_pushed = max((n for _, n in outcomes.values()), default=0)
			state = self.state().get("state", "")
			note = f"{resolved}/{len(roots)} comments resolved, {not_pushed} round(s) not pushed, request {state}"
			if resolved == len(roots) or state in STOPPED:
				return (resolved, not_pushed, state), note
			return None, note
		return self.wait("corrective rounds", step)

	def cleanup(self):
		if os.environ.get("LIVE_ROUND_KEEP") == "1":
			return
		if self.pr:
			self.gh("pr", "close", self.pr, "--delete-branch", check=False)
		if self.request:
			self.factoryd("cancel", "-reason", "live-round fixture finished", self.request, check=False)

	def run(self):
		for name, value in (("LIVE_ROUND_REPO", self.repo), ("LIVE_ROUND_REVIEWER", self.reviewer)):
			if not value:
				raise SystemExit(f"live-round: {name} is required (see this script's doc comment)")
		record = {"date": datetime.datetime.now(datetime.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ"), "fixture": self.name,
			"factoryd_version": (sh([self.bin, "version"], check=False).splitlines() or ["unknown"])[0], "comments": len(self.fixture["comments"]),
			"resolved": 0, "rounds_not_pushed": 0, "outcome": "fail"}
		started = time.time()
		try:
			self.submit()
			self.to_pull_request()
			record["pull_request"] = self.pr
			slug, number, roots = self.comment()
			record["resolved"], record["rounds_not_pushed"], record["request_state"] = self.watch(slug, number, roots)
			if record["resolved"] == record["comments"]:
				record["outcome"] = "pass"
		except RuntimeError as err:
			record["error"] = str(err)
			print(f"live-round: {err}", file=sys.stderr)
		finally:
			record["request"] = self.request
			record["duration_s"] = int(time.time() - started)
			self.cleanup()
			path = results_file()
			path.parent.mkdir(parents=True, exist_ok=True)
			with path.open("a") as f:
				f.write(json.dumps(record) + "\n")
		print(f"{'PASS' if record['outcome'] == 'pass' else 'FAIL'}  {self.name}: {record['resolved']}/{record['comments']} reviewer comments resolved, {record['rounds_not_pushed']} round(s) not pushed first")
		return 0 if record["outcome"] == "pass" else 1


def main(argv):
	if argv[:1] == ["--results"]:
		print_results(int(argv[1]) if len(argv) > 1 else 20)
		return 0
	if argv[:1] == ["--list"]:
		for d in sorted(p for p in FIXTURES.iterdir() if (p / "fixture.json").exists()):
			f = json.loads((d / "fixture.json").read_text())
			print(f"{d.name}: repository {f['repo']}, {len(f['comments'])} review comment(s)")
		return 0
	return Round().run()


if __name__ == "__main__":
	sys.exit(main(sys.argv[1:]))
