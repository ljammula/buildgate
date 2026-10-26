#!/usr/bin/env python3
"""live-round: one real PR-review corrective round, end to end.

`make live-smoke` drives the single-ticket run path and opens no pull
request, so nothing in it exercises what happens after a reviewer comments.
This does: it hands a fixture's finished spec and plan to `factoryd submit`,
approves the two review gates, waits for the ticket's pull request, posts
the fixture's review comments on it as a second GitHub account, and then
watches the pull request itself until every comment is answered or the
request stops.

A run that stops before its pull request posts no comment and adds none to
the share; it is still recorded, as a fail. A comment is RESOLVED when its thread holds the factory's "Addressed in
<sha>." reply and <sha> is a commit of the pull request. The run PASSES
when every comment is resolved, which the factory can only do within
max_review_rounds. Each run appends one line to the results file, and
--results prints the share of comments resolved over the recorded runs.

It acts on GitHub for real: a branch, a pull request and review comments on
LIVE_ROUND_REPO's origin, whatever GitHub repository that is (a scratch copy
of the fixture's repository does): every gh call names the origin's
owner/name. By default it closes the pull request, deletes
the branch and cancels the request when it ends.
The pull request is a fixture, never to be merged: once the fixture's change
is on the repository's default branch the build has nothing to add and the
run fails before its pull request.

    LIVE_ROUND_REPO      local clone of the fixture's repository (required)
    LIVE_ROUND_REVIEWER  gh account that posts the comments (required): logged
                         in with `gh auth login`, write access to the
                         repository, listed in the config's pr_trusted_authors,
                         and not the account factoryd pushes with
    LIVE_ROUND_CONFIG    session config or profile name passed as -config
    LIVE_ROUND_FIXTURE   fixture directory name (default todo-kafka-version)
    LIVE_ROUND_TIMEOUT   minutes to wait for each phase (default 90)
    LIVE_ROUND_KEEP=1    leave the pull request and the request as they end
    LIVE_ROUND_RESULTS_FILE  default ~/buildgate/live-round/results.jsonl
    FACTORYD_BIN         default factoryd

Stacked mode, LIVE_ROUND_STACKED=1 (fixture default todo-kafka-stacked):
the handed-over plan has two tickets, so ticket 2's pull request stacks on
ticket 1's branch, and ticket 2 changes the doc-comment line of the handler
ticket 1 adds, one line above where the review comment asks ticket 1's
round to add a line: the restack replays a change beside one made under it.
Before the comments are posted, the reviewer account marks ticket 2's pull
request ready for review. The comments go on ticket 1's pull request; once
its round is pushed, the factory must restack ticket 2. The run passes when
every comment is resolved, ticket 2's pull request head contains ticket 1's
new head, ticket 2's restack run is accepted with every gate it recorded
passed, and ticket 2's pull request was turned back to draft by the factory
before the restack's push. With LIVE_ROUND_STACKED_MERGE=1 the reviewer
account then squash-merges ticket 1's pull request (leaving its branch:
the repository must not delete head branches on merge, or GitHub retargets
ticket 2 itself), and the run also needs ticket 2 restacked onto the default
branch and retargeted to it by the factory (the retarget its request record
holds), not by GitHub.

    LIVE_ROUND_DATA_DIR  the factoryd data dir the requests are in (required
                         in stacked mode: the restack is read from its
                         request and run records)

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
STOPPED = {"halted", "quarantined", "cancelled", "done", "closed", "resume_review"}


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


def origin_slug(url):
	"""owner/name of a GitHub remote URL (https, ssh or scp form), or None."""
	m = re.match(r"^(?:https://(?:[^@/]+@)?github\.com/|ssh://git@github\.com/|git@github\.com:)([^/]+/[^/]+?)(?:\.git)?/?$", url.strip())
	return m.group(1) if m else None


def all_gates_passed(rec):
	"""Whether a run record has gate results and every one of them passed."""
	gates = rec.get("gate_results") or []
	return bool(gates) and all(g.get("passed") is True for g in gates)


def drafted_before_push(events, reviewer):
	"""Whether a pull request's timeline (GitHub's issue timeline events, in
	order) has a convert_to_draft by an account other than reviewer before
	the first head_ref_force_pushed: the factory turned it back to draft
	before it replaced the head."""
	for e in events:
		if e.get("event") == "head_ref_force_pushed":
			return False
		if e.get("event") == "convert_to_draft" and (e.get("actor") or {}).get("login") != reviewer:
			return True
	return False


def factory_retarget(ticket, base):
	"""The factory's recorded retarget of a request ticket onto base
	(request.json's tickets[].retarget), or None."""
	rt = ticket.get("retarget") or {}
	return rt if rt.get("to") == base else None


def replayed_commits(replays):
	"""original sha -> the commit it was last replayed as, from the commit
	pairs of replay runs' records (run.json's replay.commits). A pair counts
	only when its two patch ids are equal: the replay is the same change. A
	commit replayed again is followed to its last replay."""
	moved = {}
	for pairs in replays:
		for p in pairs:
			if p.get("original_patch_id") == p.get("replayed_patch_id"):
				moved[p["original"]] = p["replayed"]
	def last(sha):
		seen = set()
		while sha in moved and sha not in seen:
			seen.add(sha)
			sha = moved[sha]
		return sha
	return {sha: last(sha) for sha in moved}


def thread_outcomes(comments, roots, pr_commits, replays=None):
	"""For each root comment id: (resolved, rounds_not_pushed). comments is
	GitHub's flat review-comment list (id, in_reply_to_id, body). A reply
	naming a commit a restack replayed (replays: replayed_commits) counts
	when the replay is a commit of the pull request."""
	moved = replays or {}
	outcomes = {}
	for root in roots:
		resolved, not_pushed = False, 0
		for c in comments:
			if c.get("in_reply_to_id") != root:
				continue
			body = (c.get("body") or "").strip()
			addressed = ADDRESSED.match(body)
			if addressed:
				named = [o for o in moved if o.startswith(addressed.group(1))]
				candidates = [addressed.group(1)] + [moved[o] for o in named]
				if any(sha.startswith(c) for sha in pr_commits for c in candidates):
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


def stacked_pull_requests(prs, request):
	"""Ticket 1's and ticket 2's open pull requests of request among prs
	(gh pr list --json url,number,headRefName,baseRefName): ticket n's head
	branch names its run, <request>-00n-...; ticket 2's must stack on ticket
	1's branch. (None, None) until both are open."""
	first = next((p for p in prs if f"{request}-001-" in p["headRefName"]), None)
	second = next((p for p in prs if f"{request}-002-" in p["headRefName"]), None)
	if first is None or second is None or second["baseRefName"] != first["headRefName"]:
		return None, None
	return first, second


def restack_rounds(ticket):
	"""A request ticket's restack rounds (request.json's tickets[].rounds of
	kind restack) whose push was confirmed, oldest first."""
	return [r for r in ticket.get("rounds") or [] if r.get("kind") == "restack" and r.get("outcome") == "accepted" and r.get("pushed")]


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
		self.stacked = os.environ.get("LIVE_ROUND_STACKED") == "1"
		self.name = os.environ.get("LIVE_ROUND_FIXTURE", "todo-kafka-stacked" if self.stacked else "todo-kafka-version")
		self.data_dir = os.environ.get("LIVE_ROUND_DATA_DIR", "")
		self.second = ""
		self.timeout = 60 * int(os.environ.get("LIVE_ROUND_TIMEOUT", "90"))
		self.dir = FIXTURES / self.name
		self.fixture = json.loads((self.dir / "fixture.json").read_text())
		self.request = ""
		self.pr = ""
		self.slug = ""

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
		if self.slug:
			# The repository gh acts on is origin's, never one gh infers
			# from another remote.
			env["GH_REPO"] = self.slug
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
				try:
					self.factoryd("approve", self.request)
				except RuntimeError as err:
					# The draft left a choice to the operator: nobody is here,
					# so its own recommendation is the answer. Any other
					# refusal is retried on the next poll, as before.
					if state == "spec_review" and "decisions left for you" in str(err):
						self.factoryd("reject", "-reason", "Take the recommended option for every open decision.", self.request, check=False)
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

	def replays(self):
		"""The commit pairs of this request's replay runs (stacked mode)."""
		if not (self.stacked and self.data_dir and self.request):
			return {}
		pairs = []
		for t in self.request_record().get("tickets") or []:
			for rnd in restack_rounds(t):
				pairs.append((self.run_record(rnd["run_id"]).get("replay") or {}).get("commits") or [])
		return replayed_commits(pairs)

	def watch(self, slug, number, roots):
		def step():
			comments = json.loads(self.gh("api", "--paginate", "--slurp", f"repos/{slug}/pulls/{number}/comments") or "[]")
			comments = [c for page in comments for c in page]
			commits = [c["oid"] for c in self.pr_json("commits")["commits"]]
			outcomes = thread_outcomes(comments, roots, commits, self.replays())
			resolved = sum(1 for ok, _ in outcomes.values() if ok)
			not_pushed = max((n for _, n in outcomes.values()), default=0)
			state = self.state().get("state", "")
			note = f"{resolved}/{len(roots)} comments resolved, {not_pushed} round(s) not pushed, request {state}"
			if resolved == len(roots) or state in STOPPED:
				return (resolved, not_pushed, state), note
			return None, note
		return self.wait("corrective rounds", step)

	# Stacked mode: two tickets, the restack of ticket 2 checked from the
	# factory's own records.

	def record(self, *parts):
		return json.loads((pathlib.Path(self.data_dir).expanduser() / pathlib.Path(*parts)).read_text())

	def request_record(self):
		return self.record("requests", self.request, "request.json")

	def run_record(self, run_id):
		return self.record("runs", run_id, "run.json")

	def to_stacked_pull_requests(self):
		def step():
			state = self.state().get("state", "")
			if state in ("spec_review", "plan_review"):
				self.factoryd("approve", self.request, check=False)
				return None, state
			if state in STOPPED:
				raise RuntimeError(f"request stopped before its pull requests: {state}: {self.state().get('error', '')}")
			prs = json.loads(self.gh("pr", "list", "--state", "open", "--json", "url,number,headRefName,baseRefName"))
			first, second = stacked_pull_requests(prs, self.request)
			if state == "pr_review" and first and second:
				return (first, second), state
			return None, state
		first, second = self.wait("stacked pull requests", step)
		self.pr, self.second = first["url"], second["url"]
		print(f"live-round: ticket 1 {self.pr}, ticket 2 {self.second} (stacked on {first['headRefName']})", flush=True)

	def restacked(self, onto_check, what):
		"""Wait until ticket 2 has a pushed restack whose replay run passes
		onto_check(run); return that run. Its every gate must have passed."""
		def step():
			state = self.state().get("state", "")
			r = self.request_record()
			rounds = restack_rounds(r["tickets"][1])
			for rnd in reversed(rounds):
				rec = self.run_record(rnd["run_id"])
				if rec.get("state") == "accepted" and onto_check(rec):
					if not all_gates_passed(rec):
						raise RuntimeError(f"restack run {rec['id']} was pushed with a gate that did not pass: {rec.get('gate_results')}")
					return rec, f"restack run {rec['id']} accepted"
			if state in STOPPED:
				raise RuntimeError(f"request stopped before ticket 2 was restacked ({what}): {state}: {r.get('error', '')}")
			return None, f"{len(rounds)} restack(s) pushed, request {state}"
		return self.wait(f"ticket 2 restacked {what}", step)

	def contains(self, slug, ancestor, head):
		cmp = json.loads(self.gh("api", f"repos/{slug}/compare/{ancestor}...{head}"))
		return cmp.get("behind_by") == 0

	def ready_second(self):
		"""Mark ticket 2's pull request ready for review as the reviewer, so
		the factory has a ready pull request to turn back to draft."""
		self.gh("pr", "ready", self.second, as_reviewer=True)
		if json.loads(self.gh("pr", "view", self.second, "--json", "isDraft"))["isDraft"]:
			raise RuntimeError(f"ticket 2's pull request {self.second} is still a draft after {self.reviewer} marked it ready")
		print(f"live-round: {self.reviewer} marked ticket 2's pull request ready for review", flush=True)

	def stacked_checks(self, slug, record):
		pr1_head = self.pr_json("headRefOid")["headRefOid"]
		rec = self.restacked(lambda rec: (rec.get("replay") or {}).get("onto_sha") == pr1_head, "onto ticket 1's new head")
		second = json.loads(self.gh("pr", "view", self.second, "--json", "headRefOid,number,isDraft,baseRefName"))
		record["restack_run"] = rec["id"]
		if second["headRefOid"] != rec.get("result_sha") or not self.contains(slug, pr1_head, second["headRefOid"]):
			raise RuntimeError(f"ticket 2's pull request head {second['headRefOid']} is not the restack's {rec.get('result_sha')} on ticket 1's head {pr1_head}")
		events = json.loads(self.gh("api", "--paginate", "--slurp", f"repos/{slug}/issues/{second['number']}/timeline") or "[]")
		if not drafted_before_push([e for page in events for e in page], self.reviewer):
			raise RuntimeError("ticket 2's pull request was ready, and the factory did not turn it back to draft before the restack's push")
		if os.environ.get("LIVE_ROUND_STACKED_MERGE") != "1":
			return
		first = self.pr_json("baseRefName,isDraft")
		if first["isDraft"]:
			# The round's push turned it back to draft, and the reviewer's
			# thread, still open, keeps the factory from marking it ready.
			self.gh("pr", "ready", self.pr, as_reviewer=True)
		self.gh("pr", "merge", self.pr, "--squash", as_reviewer=True)
		base = first["baseRefName"]
		merge = json.loads(self.gh("pr", "view", self.pr, "--json", "mergeCommit"))["mergeCommit"]["oid"]
		print(f"live-round: {self.reviewer} squash-merged ticket 1 into {base} as {merge}", flush=True)
		rec = self.restacked(lambda rec: self.contains(slug, merge, rec.get("result_sha", "")), f"onto {base}")
		def retargeted():
			view = json.loads(self.gh("pr", "view", self.second, "--json", "headRefOid,baseRefName"))
			note = f"ticket 2 on {view['baseRefName']} at {view['headRefOid'][:12]}"
			if view["baseRefName"] != base or view["headRefOid"] != rec.get("result_sha"):
				return None, note
			if factory_retarget(self.request_record()["tickets"][1], base) is None:
				raise RuntimeError(f"ticket 2's pull request is on {base}, but the factory recorded no retarget to it: GitHub moved it (was ticket 1's branch deleted on merge?)")
			return view, note
		self.wait(f"ticket 2 retargeted to {base}", retargeted)
		record["merge_restack_run"] = rec["id"]

	def cleanup(self):
		if os.environ.get("LIVE_ROUND_KEEP") == "1":
			return
		# Cancel first: closing the pull request ends the request as closed,
		# which a cancel then refuses.
		if self.request:
			self.factoryd("cancel", "-reason", "live-round fixture finished", self.request, check=False)
		for pr in (self.second, self.pr):
			if pr:
				self.gh("pr", "close", pr, "--delete-branch", check=False)

	def run(self):
		required = [("LIVE_ROUND_REPO", self.repo), ("LIVE_ROUND_REVIEWER", self.reviewer)]
		if self.stacked:
			required.append(("LIVE_ROUND_DATA_DIR", self.data_dir))
		for name, value in required:
			if not value:
				raise SystemExit(f"live-round: {name} is required (see this script's doc comment)")
		self.slug = origin_slug(sh(["git", "-C", self.repo, "remote", "get-url", "origin"]))
		if not self.slug:
			raise SystemExit(f"live-round: LIVE_ROUND_REPO {self.repo}'s origin is not a GitHub repository")
		record = {"date": datetime.datetime.now(datetime.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ"), "fixture": self.name,
			"factoryd_version": (sh([self.bin, "version"], check=False).splitlines() or ["unknown"])[0], "comments": 0,
			"resolved": 0, "rounds_not_pushed": 0, "outcome": "fail"}
		started = time.time()
		try:
			self.submit()
			if self.stacked:
				self.to_stacked_pull_requests()
				record["stacked_pull_request"] = self.second
				self.ready_second()
			else:
				self.to_pull_request()
			record["pull_request"] = self.pr
			slug, number, roots = self.comment()
			# Counted only once posted: a run that stopped before its pull
			# request put no comment in front of the factory.
			record["comments"] = len(roots)
			record["resolved"], record["rounds_not_pushed"], record["request_state"] = self.watch(slug, number, roots)
			if self.stacked and roots and record["resolved"] == len(roots):
				self.stacked_checks(slug, record)
			if roots and record["resolved"] == len(roots):
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
		print(f"{'PASS' if record['outcome'] == 'pass' else 'FAIL'}  {self.name}: {record['resolved']}/{record['comments']} posted reviewer comments resolved, {record['rounds_not_pushed']} round(s) not pushed first")
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
