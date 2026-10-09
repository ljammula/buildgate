#!/usr/bin/env python3
"""Zero-human full-stack app builder on top of the pi harness.

Give it a spec file and a workspace directory; it drives `pi -p` through
however many corrective rounds it takes to get real canonical verification
and independent-review evidence passing -- no chat interaction or human
review step.

Why this exists outside the extensions: `quality-gate.ts` and
`cross-model-review.ts` deliberately report settlement evidence without
injecting an in-band corrective turn. This script consumes that evidence
after each `pi -p` invocation, reruns the shared canonical verification
command, and starts a fresh `pi -p --continue` round when any required
signal fails. This is the same shape as
the already-proven `PiHarness.run()` bounded-follow-up fix in
local-model-bench (commits 8531917/dfe4620), generalized from
"context-budget-exceeded" endings to "verification still failing" endings.

Usage:
    python3 build_app.py --workspace /path/to/app --spec spec.md \
        [--max-rounds 3] [--timeout-minutes 45] [--harness pi|pifork] [--sonnet-fallback]

The installed Pi thinking policy is inherited by default. `--review-policy`
defaults to advisory: the whole-diff reviewer is recorded when present, but
only canonical verification blocks completion. factoryd never passes the
option (the reviewer extension is not in the sandbox image, so `required`
would fail every round as "review unavailable"); the intake scripts
(ticket_runner.py, goal_pilot.py) always pass it explicitly. The
`--sonnet-fallback` flag authorizes one billed Sonnet pass only after the local
corrective-round budget is exhausted.

`--review-policy` governs only the legacy whole-diff "reviewer" extension
trace and is independent of `--conformity-policy`, which governs the
separate per-criterion spec-conformity review `--spec-acceptance-criteria`
enables: that review runs once canonical verification passes,
regardless of what `--review-policy` decided about the whole-diff reviewer,
and `--conformity-policy required` (the default whenever criteria are
given) fails the build on any non-clean or unreachable per-criterion
verdict. The two were coupled through `--review-policy` alone until a live
run showed why that was wrong: a sandbox with no whole-diff reviewer
available (`--review-policy required` sees it as an unavailable,
policy-violating verdict) blocked every round outright, so the conformity
review -- gated on a round ever succeeding -- never even got a chance to
run.

Exit code 0 only if canonical verification and the selected review policy
pass by the round budget (or an explicitly authorized Sonnet fallback passes).
A BUILD_REPORT.md is always written to the workspace on any full run,
whether it succeeded or the round budget ran out -- the point of
zero-human is that the report, not a chat transcript, is the record of
what happened.
"""

from __future__ import annotations

import argparse
import hashlib
import json
import os
import re
import shutil
import signal
import subprocess
import sys
import tempfile
import threading
import time
from dataclasses import asdict, dataclass, field
from datetime import datetime, timezone
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import harness_adapters  # noqa: E402
import prompt_templates  # noqa: E402
import round_feedback  # noqa: E402

DEFAULT_ADAPTER = harness_adapters.get("pi")
PI_ROOT = Path(__file__).resolve().parents[1]
REPO_ROOT = PI_ROOT.parent
SONNET_MODEL = "claude-sonnet-5"
THINKING_LEVELS = ("off", "minimal", "low", "medium", "high", "xhigh", "max")
VERIFY_RESOLVER = PI_ROOT / "scripts" / "resolve-verification.ts"
TSX = PI_ROOT / "node_modules" / ".bin" / "tsx"
NON_RETRYABLE_REVIEW_FAILURES = {
	"missing-configuration",
	"invalid-configuration",
	"same-primary",
	"no-task-spec",
	# With --review-base-sha threading the true ticket boundary through to
	# the reviewer (see run_build), an empty diff means literally nothing
	# has changed since the ticket started -- not "this round found nothing
	# new" (that's "unchanged-since-last-review", still retryable). No
	# amount of re-prompting can produce a decisive verdict for zero
	# changes, so fail fast instead of burning the full round budget.
	"empty-diff",
}


def sh(args: list[str], *, cwd: Path | None = None, timeout: float | None = None, env: dict | None = None) -> subprocess.CompletedProcess:
	# stdin is /dev/null: Codex CLI reads stdin whenever it is not a TTY and
	# would wait on an inherited pipe; no child here wants input.
	return subprocess.run(args, cwd=cwd, env=env, text=True, capture_output=True, timeout=timeout, check=False, stdin=subprocess.DEVNULL)


_GIT_SNAPSHOT_RETRIES = 4
_GIT_SNAPSHOT_RETRY_DELAY_SECONDS = 0.25


def _git_snapshot_command(args: list[str], *, cwd: Path, acceptable_codes: set[int]) -> subprocess.CompletedProcess:
	result = sh(args, cwd=cwd)
	for _ in range(_GIT_SNAPSHOT_RETRIES):
		if result.returncode in acceptable_codes:
			return result
		time.sleep(_GIT_SNAPSHOT_RETRY_DELAY_SECONDS)
		result = sh(args, cwd=cwd)
	return result


# Root-only fallback used when the TS resolver can't run at all -- e.g. a
# fresh checkout where `npm install` was never run (pi/node_modules is
# gitignored, tsx is a devDependency, and install.sh doesn't install it).
# Not nested-manifest aware like resolve-verification.ts; good enough to gate
# a corrective round, not a substitute for the real resolver. Found via a
# Codex PR #22 review, 2026-08-20: without this fallback, resolve_verify_command
# silently returned None on every fresh installation, so build_app.py stopped
# after its first round with "no canonical verification command resolvable"
# even against a workspace with a valid Makefile or manifest.
_FALLBACK_VERIFY_CANDIDATES = [
	("Makefile", "verify", "make verify"),
	("Makefile", "test", "make test"),
	("Makefile", "check", "make check"),
	("go.mod", None, "go vet ./... && go test ./..."),
	("package.json", None, "npm test"),
	("pyproject.toml", None, "pytest"),
	("pubspec.yaml", None, "dart test"),
]


def _resolve_verify_command_fallback(workspace: Path) -> str | None:
	makefile = workspace / "Makefile"
	if makefile.exists():
		lines = makefile.read_text(errors="ignore").splitlines()
		targets = {line.split(":", 1)[0].strip() for line in lines if ":" in line and not line.startswith(("\t", " ", "#"))}
		for filename, target, command in _FALLBACK_VERIFY_CANDIDATES:
			if filename != "Makefile":
				continue
			if target in targets:
				return command
	for filename, _target, command in _FALLBACK_VERIFY_CANDIDATES:
		if filename == "Makefile":
			continue
		if (workspace / filename).exists():
			return command
	return None


def resolve_verify_command(workspace: Path) -> str | None:
	"""Resolve through the exact TypeScript implementation used by
	quality-gate when it's available; falls back to a root-only heuristic
	scan when the TS resolver can't even run (tsx missing/erroring), rather
	than silently reporting no command exists. A clean run of the real
	resolver that itself finds nothing is trusted as-is -- that's a more
	accurate answer than the fallback's shallower scan, not a failure to
	paper over."""
	if TSX.exists():
		try:
			result = sh([str(TSX), str(VERIFY_RESOLVER), str(workspace)], cwd=PI_ROOT, timeout=30)
		except (OSError, subprocess.TimeoutExpired):
			result = None
		if result is not None and result.returncode == 0:
			try:
				command = json.loads(result.stdout).get("command")
			except (json.JSONDecodeError, AttributeError):
				command = None
			return command if isinstance(command, str) and command else None
	return _resolve_verify_command_fallback(workspace)


def committed_agents_md_blob(workspace: Path) -> str | None:
	"""Returns the git blob id of the AGENTS.md committed at HEAD, for the
	evidence record, or None when none is committed, HEAD doesn't exist
	yet, or the committed AGENTS.md is a symlink (its blob is only the link
	target text, not an identity of the guidance).

	The text itself is not read: every harness loads the workspace's
	AGENTS.md into its own system prompt (probed 2026-09-29 for Pi, Codex
	CLI and Copilot CLI by capturing their model requests), so pasting it
	into the prompt too sent it twice on every turn. The harness reads the
	working tree, so this blob identifies what the worker saw at round
	start; the round's own later edits are visible in its diff."""
	rev_parse = sh(["git", "rev-parse", "--verify", "-q", "HEAD:AGENTS.md"], cwd=workspace)
	blob_id = rev_parse.stdout.strip()
	if rev_parse.returncode != 0 or not blob_id:
		return None
	ls_tree = sh(["git", "ls-tree", "HEAD", "AGENTS.md"], cwd=workspace)
	mode = ls_tree.stdout.split()[0] if ls_tree.returncode == 0 and ls_tree.stdout.strip() else ""
	if mode == "120000":
		return None
	return blob_id


_SERVICE_ALIAS = re.compile(r"[A-Za-z0-9][A-Za-z0-9_.-]{0,62}")
_SERVICE_PORT = re.compile(r"[0-9]{1,5}")
_SERVICE_ADDRESS = re.compile(r"[0-9]{1,3}(\.[0-9]{1,3}){3}")
_SERVICE_FORWARD = re.compile(r"([0-9]{1,5})=([A-Za-z0-9][A-Za-z0-9_.-]{0,62}:[0-9]{1,5})")


def compose_services_sentence(environ: dict[str, str]) -> str | None:
	"""One fixed-template sentence naming the compose sidecars factoryd
	launched for this attempt, or None when it launched none.

	factoryd sets BG_COMPOSE_SERVICES=up plus BG_SERVICE_<NAME>=<host name or address>
	and BG_SERVICE_<NAME>_PORT=<port> (internal/sandbox.ApplyToWorkerLaunch),
	and BG_COMPOSE_FORWARDS=<port>=<host name>:<port>,... when the compose
	file publishes ports, which bg-forward serves on this worker's localhost.
	Only those host names and ports reach the prompt, each checked against a
	strict pattern: never BG_COMPOSE_SERVICES's disabled reason or any other
	environment value.
	"""
	if environ.get("BG_COMPOSE_SERVICES") != "up":
		return None
	variables = {k: v for k, v in environ.items() if k.startswith("BG_SERVICE_")}
	endpoints = []
	how, plural = "host name", "host names"
	for key in sorted(variables):
		alias = variables[key]
		# BG_SERVICE_DB_PORT is db's port only when BG_SERVICE_DB exists
		# too; otherwise it is the host name of a service named "db-port".
		if key.endswith("_PORT") and key[: -len("_PORT")] in variables and _SERVICE_PORT.fullmatch(alias):
			continue
		if not _SERVICE_ALIAS.fullmatch(alias):
			continue
		port = variables.get(key + "_PORT", "")
		endpoint = f"{alias}:{port}" if _SERVICE_PORT.fullmatch(port) else alias
		if _SERVICE_ADDRESS.fullmatch(alias):
			# A worker with no network of its own is given each service's
			# address, which does not say which service it is: name it from
			# the variable.
			endpoint = f"{key[len('BG_SERVICE_'):].lower()} at {endpoint}"
			how, plural = "address", "addresses"
		endpoints.append(endpoint)
	if not endpoints:
		return None
	forwards = []
	for entry in environ.get("BG_COMPOSE_FORWARDS", "").split(","):
		match = _SERVICE_FORWARD.fullmatch(entry)
		if match:
			forwards.append(f"localhost:{match.group(1)} ({match.group(2)})")
	localhost = (
		"localhost also reaches them at the ports the compose file publishes: " + ", ".join(forwards)
		if forwards else "localhost does not reach them"
	)
	return (
		f"The repository's compose services are running as sidecars for this build, reachable by {how}: "
		+ ", ".join(endpoints)
		+ f" (the BG_SERVICE_<NAME> and BG_SERVICE_<NAME>_PORT environment variables hold the same {plural} and ports; {localhost})."
	)


def redact(output: str, limit: int = 3000) -> str:
	import re

	redacted = re.sub(r"(?i)(authorization:\s*bearer\s+)\S+", r"\1<redacted>", output)
	# JSON-quoted credential fields ("api_key": "...") -- the generic
	# key=value pattern below requires the key itself to be bare word
	# characters immediately followed by [=:], which a JSON key's own
	# closing quote defeats. Found in a round-2 adversarial review: this
	# change is the first to put pi's own stderr -- which can be a
	# JSON error body from a relay/API failure -- into an operator-facing
	# halt reason, so redaction here must cover more than the original
	# hand-picked shapes. Mirrors internal/sanitize.Text's own secretPatterns
	# (Go), extended identically.
	redacted = re.sub(
		r'(?i)"(api_key|access_token|refresh_token|authorization)"(\s*:\s*)"[^"]*"',
		r'"\1"\2"<redacted>"',
		redacted,
	)
	# OpenAI-style secret keys and GitHub tokens: whole-token patterns,
	# checked before the generic key=value pattern so a bare token pasted
	# with no key= prefix at all is still redacted.
	redacted = re.sub(r"sk-[A-Za-z0-9_-]{16,}", "sk-<redacted>", redacted)
	redacted = re.sub(r"gh[pousr]_[A-Za-z0-9]{20,}", "gh_<redacted>", redacted)
	# A JWT (three base64url segments joined by '.', always starting "eyJ"
	# -- the base64 of `{"`) can itself be a bearer credential even where
	# no "Bearer "/"api_key=" prefix precedes it.
	redacted = re.sub(r"eyJ[\w-]+\.[\w-]+\.[\w-]+", "<redacted-jwt>", redacted)
	redacted = re.sub(r"(?i)\b(password|passwd|token|secret|api[_-]?key)\s*[=:]\s*[^\s]+", r"\1=<redacted>", redacted)
	redacted = re.sub(r"://([^\s:/]+):([^\s@]+)@", r"://\1:<redacted>@", redacted)
	return redacted.strip()[-limit:]


def exited_zero_hint(errors: list[str]) -> str:
	"""A bounded, redacted, single-line summary of the agent's model-route errors
	(AgentOutput.route_errors)
	for a drafting script's "agent exited 0 but produced nothing" reason --
	found live in the 2026-09-25 closing walk, where two Copilot models
	failed spec drafting with that bare reason and no recorded cause."""
	if not errors:
		return ""
	# Keep the HEAD: the first error is usually the root cause, and
	# redact(text, limit) keeps the tail. Redact the whole text first so no
	# secret is cut in half before its pattern can match.
	joined = single_line(redact("; ".join(errors), 100_000))
	return re.sub(r"[\x00-\x1f\x7f-\x9f]", "", joined)[:200]


def last_nonblank_line(text: str) -> str:
	"""Returns the last non-blank line of text (stripped), or "" if text has
	none -- used by plan_tickets.py/draft_spec.py to fold a bounded, redacted
	hint of pi's own stderr into their one-line failure reason: before this,
	"agent exited 1" was the whole story even when pi's own stderr already
	named the real cause (an auth failure, a 401, the model route being
	down) -- build_app.sh captures that text but nothing ever printed it.

	str.splitlines() already treats every Unicode line-boundary character
	(\\r, \\r\\n, \\v, \\f, \\x1c-\\x1e, NEL \\x85, U+2028, U+2029) as a
	line break, so none of those can end up embedded mid-line in the
	single line this returns -- but see single_line for why the caller
	still folds the result before printing it (#1/#2, round-2 adversarial
	review of the same plan)."""
	for line in reversed(text.splitlines()):
		stripped = line.strip()
		if stripped:
			return stripped
	return ""


def stderr_hint(stderr: str | bytes | None) -> str:
	"""pi's stderr folded to one bounded, redacted line (its last non-blank
	line), or "" when it has none. Accepts bytes because a
	subprocess.TimeoutExpired carries whatever partial stderr it captured
	undecoded -- draft_spec.py/plan_tickets.py print this on a timeout too, so
	"agent timed out" names the route's last complaint (a 429, an auth
	retry) instead of nothing."""
	if isinstance(stderr, bytes):
		stderr = stderr.decode(errors="replace")
	return single_line(redact(last_nonblank_line(stderr or ""), 200))


def single_line(text: str) -> str:
	"""Folds every run of whitespace or line-breaking characters in text to
	one ordinary space, trimming the ends -- the Python-side sibling of
	internal/sanitize.Line (Go). Found in a round-2 adversarial review;
	used on every single-line operator-facing string this script builds
	from untrusted text (the pi-stderr hint printed on a failure path), a
	defensive belt-and-suspenders on top of last_nonblank_line's own
	str.splitlines()-based line-boundary handling -- e.g. a literal tab or
	a run of ordinary spaces left inside that one line is collapsed too,
	not just the Unicode line-boundary characters splitlines() already
	excludes."""
	import re

	return re.sub(r"\s+", " ", text).strip()


@dataclass(frozen=True)
class ReviewSignal:
	outcome: str
	detail: str = ""


def review_signal(traces: list[dict]) -> ReviewSignal:
	decisive: ReviewSignal | None = None
	startup_reason = ""
	for trace in traces:
		if trace.get("extension") != "reviewer":
			continue
		if trace.get("event") == "startup" and trace.get("outcome") == "blocked":
			startup_reason = str(trace.get("metadata", {}).get("reason") or "reviewer-disabled")
			continue
		if trace.get("event") != "review":
			continue
		outcome = str(trace.get("outcome") or "unavailable")
		metadata = trace.get("metadata") or {}
		if outcome in ("clean", "flagged"):
			decisive = ReviewSignal(outcome, str(metadata.get("findings") or ""))
		elif outcome == "blocked" and metadata.get("reason") in {
			"unchanged-since-last-review", "transient-retry-exhausted",
		} and decisive:
			continue
		else:
			decisive = ReviewSignal("unavailable", str(metadata.get("reason") or outcome))
	return decisive or ReviewSignal("unavailable", startup_reason or "no-review-verdict")


def round_blockers(
	*,
	verify_passed: bool | None,
	pi_failed: bool,
	pi_timed_out: bool,
	traces: list[dict],
	review_policy: str,
	turn_errors: tuple[int, int] = (0, 0),
	no_changes: bool = False,
	fast_check_ran: bool = False,
	fast_check_passed: bool | None = None,
	oracle_passed: bool | None = None,
	setup_failed: str | None = None,
	autofix_failed: str | None = None,
) -> tuple[list[str], ReviewSignal]:
	blockers: list[str] = []
	if no_changes:
		# See workspace_fingerprint's docstring: verify passing against
		# an untouched workspace is not evidence of anything this round did.
		blockers.append("no changes made to the workspace")
	if pi_timed_out:
		blockers.append("pi invocation timed out")
	elif pi_failed:
		blockers.append("pi invocation failed")
	errored, total = turn_errors
	if total and errored == total:
		# Every assistant turn in this round errored out (e.g. the model
		# route was unreachable) -- pi still exits 0 and verify still
		# legitimately fails, so without this the round is indistinguishable
		# from the model actually trying and failing (observed live:
		# budget-pilot ticket 005, 2026-08-20).
		blockers.append(f"model route unreachable ({errored}/{total} assistant turns errored)")
	if any(trace.get("outcome") == "stall-timeout" or trace.get("stallTimeout") is True for trace in traces):
		blockers.append("stall-timeout")
	if setup_failed is not None:
		# A repository setup command failed, so neither the fast check nor
		# canonical verification ran this round.
		blockers.append(SETUP_FAILED_BLOCKER + setup_failed[:200])
	elif fast_check_ran and fast_check_passed is False:
		# The fast check failed, so canonical verification was never
		# attempted this round (verify_passed is None, its own "not run"
		# value) -- attribute the blocker to the fast check specifically,
		# not to a phantom "canonical verification failed" (Codex review
		# of PR #90: verify_passed being merely "not True" here would
		# otherwise claim a canonical-verification failure that never
		# happened).
		blockers.append("fast check failed")
	elif verify_passed is not True:
		blockers.append("canonical verification failed")
	elif oracle_passed is False:
		# Only reachable when verify_passed is True (run_build's own
		# run_reference_oracle call site is gated on that) -- an
		# additional correctness bar beyond canonical verify, not a
		# substitute for it, so it's checked only once verify itself has
		# already cleared this round.
		blockers.append("reference oracle failed")
	if autofix_failed:
		blockers.append(autofix_failed)
	review = review_signal(traces)
	if review.outcome == "flagged" and review_policy != "advisory":
		blockers.append("reviewer flagged the current diff")
	elif review.outcome != "clean" and review_policy == "required":
		blockers.append(f"review unavailable ({review.detail})")
	return blockers, review


@dataclass
class Round:
	index: int
	agent: str
	command: list[str]
	agent_returncode: int
	agent_timed_out: bool
	usage: dict | None
	traces: list[dict]
	reviewer: ReviewSignal
	verify_command: str | None
	verify_passed: bool | None
	verify_timed_out: bool
	verify_output_tail: str
	duration_s: float
	turn_errors: tuple[int, int] = (0, 0)
	# fast_check_ran/fast_check_passed record whether a --fast-check-command
	# ran this round and, if so, whether it passed. fast_check_passed is
	# None when no fast check ran (unset --fast-check-command), mirroring
	# verify_passed's own None-means-"not resolved" convention.
	fast_check_ran: bool = False
	fast_check_passed: bool | None = None
	# oracle_passed is None when no --reference-oracle-command was given,
	# or when it wasn't run this round because verify itself didn't pass
	# (run_reference_oracle's own call site in run_build gates on
	# verify_passed is True) -- same None-means-"not resolved" convention
	# fast_check_passed already uses.
	#
	# Advisory only -- never authoritative (found in adversarial review):
	# this in-loop check runs inside the SAME writable build-phase container
	# the worker controls for the rest of the round (its own tmpfs $HOME,
	# PATH, any test helpers the oracle command itself invokes) -- a
	# worker could in principle shape its own environment to spoof a
	# pass, the same way it already could for in-loop --verify-command.
	# The separate, one-shot, fresh-container, snapshotted-and-hashed
	# post-build reference_oracle named gate (run_ticket.go,
	# sandboxedForReferenceOracle) remains the sole authoritative check
	# an accepted run's evidence/release decision depends on -- nothing
	# reads oracle_passed for that. Keep it that way: this field is for
	# operator visibility and driving the in-loop retry, not for gating
	# acceptance.
	oracle_command: str | None = None
	oracle_passed: bool | None = None
	oracle_output_tail: str = ""
	# What the next round is told about this one (round_feedback.py): why it
	# did not earn completion, the files its agent turn changed, an id for
	# "this failure" so a repeat is recognised, where the failing command's
	# complete output was saved, and what happened to the agent process when
	# the failure was not a failing command.
	blockers: list[str] = field(default_factory=list)
	changed_files: list[str] = field(default_factory=list)
	failure_signature: str = ""
	failure_log: str = ""
	agent_notes: str = ""
	# run_autofix's record of this round's autofix commands (None when the
	# repository lists none); written as `autofix` in BUILD_EVIDENCE.json.
	autofix: dict | None = None


@dataclass
class BuildResult:
	workspace: Path
	spec_path: Path
	review_policy: str = "required"
	# conformity_policy is the per-criterion spec-conformity review's
	# own policy, separate from review_policy (which governs only the
	# legacy whole-diff "reviewer" extension trace, review_signal's own
	# concern): required fails the build on any non-clean/unavailable
	# criterion verdict, advisory records verdicts without revoking a
	# success canonical verification and the whole-diff review already
	# earned. See run_build's own conformity-review block for why these
	# two policies must never be coupled (found live: review_policy
	# "required" against a sandbox with no whole-diff reviewer available
	# blocked every round before the conformity review ever got a chance
	# to run at all).
	conformity_policy: str = "required"
	rounds: list[Round] = field(default_factory=list)
	succeeded: bool = False
	# What the notes turn did, for BUILD_EVIDENCE.json: never its reply.
	notes_turn: dict = field(default_factory=lambda: {"ran": False, "skipped_reason": "not reached", "duration_s": 0.0})
	stopped_reason: str = ""
	# Recorded so BUILD_EVIDENCE.json/BUILD_REPORT.md show what repository
	# guidance the agent actually saw, not just whether a prompt happened
	# to mention one.
	agents_md_used: bool = False
	agents_md_git_blob: str | None = None
	# review_verdicts is the per-criterion independent-reviewer
	# outcome against --spec-acceptance-criteria, one entry per declared
	# criterion (empty when no criteria file was given). See
	# run_spec_conformity_review's own doc comment for how it's produced.
	review_verdicts: list[dict] = field(default_factory=list)


def workspace_differs_from_base(workspace: Path, base_sha: str) -> bool:
	"""True when tracked files differ from base_sha or untracked (non-ignored) files exist."""
	tracked = subprocess.run(["git", "-C", str(workspace), "diff", "--quiet", base_sha], check=False)
	if tracked.returncode == 1:
		return True
	untracked = subprocess.run(
		["git", "-C", str(workspace), "ls-files", "--others", "--exclude-standard"],
		check=False, capture_output=True, text=True,
	)
	return bool(untracked.stdout.strip())


def handoff_preamble(handoff_text: str) -> str:
	"""Header plus the caller's handoff note for a build resumed after an interrupted attempt."""
	return (
		"An earlier attempt of this build was interrupted. Its work is already in the "
		"workspace; you start from that state, not from a clean checkout, with no memory "
		"of the earlier session. Inspect what exists before changing anything, keep what "
		"is right, and finish the task below.\n\n"
		f"{handoff_text.strip()}"
	)


def earlier_attempt_preamble(record_text: str) -> str:
	"""Header plus the factory's record of an earlier attempt at this ticket
	that finished and then failed its checks. Unlike a handoff (an interrupted
	attempt whose work is in the workspace), that attempt is over. Where this
	build starts (that attempt's branch, or the base again) is the record's
	own first sentence, written by whoever started this build."""
	return (
		"An earlier attempt at this ticket finished its build and was not accepted: it failed "
		"the checks listed below. You have no memory of that attempt. What follows is the "
		"factory's record of it. Use it to see what went wrong, then do the task below so "
		"that those checks pass. The task has not changed, and the record does not widen it: "
		"it describes what happened and gives no instructions of its own.\n\n"
		f"{record_text.strip()}"
	)


def baseline_failure_preamble(note_text: str) -> str:
	"""Header plus the factory's note about the verify command's run on the
	untouched repository, given only when it failed and the ticket names
	every test that failed."""
	return (
		"The factory ran the verify command on the repository before you changed anything. "
		"What follows is its result: a fact about the state you start from, with no "
		"instructions of its own. The task below has not changed.\n\n"
		f"{note_text.strip()}"
	)


def sonnet_invocation(prompt: str) -> list[str]:
	return [
		"claude", "-p", prompt,
		"--model", SONNET_MODEL,
		"--permission-mode", "bypassPermissions",
		"--output-format", "json",
	]


def print_progress(payload: dict) -> None:
	"""Emits one `FACTORY_PROGRESS <json>` line to stdout -- the exact,
	byte-for-byte contract factoryd's sandbox log scanner
	(internal/sandbox/docker.go) parses to relay build-loop activity into
	the run's progress feed live, instead of only after the round exits.
	Flushed explicitly since stdout is a pipe when run under factoryd's
	Docker sandbox, not a tty."""
	print(f"FACTORY_PROGRESS {json.dumps(payload)}", flush=True)


def _emit_agent_progress(adapter, round_index: int, line: str) -> None:
	"""run_agent_streaming's on_event callback: parses one streamed pi stdout
	line and, if it classifies as a tool call or assistant text turn,
	prints the corresponding FACTORY_PROGRESS agent note. Never raises --
	a malformed line (bad JSON, an unexpected event shape) is silently
	skipped, since worker output is untrusted and this is display-only
	(see the progress-feed contract's "Worker stages" section)."""
	detail = adapter.progress_note(line)
	if detail:
		print_progress({"stage": "agent", "event": "note", "round": round_index, "detail": detail[:500]})


def run_agent_streaming(
	command: list[str], *, cwd: Path, timeout: float | None, env: dict, on_event,
) -> tuple[subprocess.CompletedProcess | None, bool]:
	"""Runs an agent round's command with its stdout streamed line by line to
	`on_event` as it arrives, instead of sh()'s capture-then-return-at-exit
	shape -- so a live caller (run_build's FACTORY_PROGRESS agent notes)
	can see activity as the round happens. Returns the same (stdout,
	returncode, timed-out) shape sh() would have: the post-round parser
	(adapter.parse) gets the exact same concatenated stdout text either
	way, unchanged.

	stderr is discarded (redirected away, not merged into stdout): sh()'s
	CompletedProcess.stderr was never read for a pi round either, only
	.stdout, so this preserves that behavior exactly rather than risking
	stray stderr lines interleaved into the JSON-lines stdout the parsers
	above scan.

	Mirrors ticket_runner.py's own invoke_build_app() process-group
	kill-on-timeout pattern (start_new_session=True, SIGTERM then SIGKILL
	after a grace period) rather than inventing a new one."""
	try:
		proc = subprocess.Popen(
			command, cwd=cwd, env=env, text=True, stdin=subprocess.DEVNULL,
			stdout=subprocess.PIPE, stderr=subprocess.DEVNULL,
			start_new_session=True,
		)
	except OSError as exc:
		return subprocess.CompletedProcess(command, 127, "", str(exc)), False

	lines: list[str] = []

	def _reader() -> None:
		for line in proc.stdout:
			lines.append(line)
			try:
				on_event(line.rstrip("\n"))
			except Exception:
				pass

	reader = threading.Thread(target=_reader, daemon=True)
	reader.start()
	try:
		proc.wait(timeout=timeout)
		timed_out = False
	except subprocess.TimeoutExpired:
		timed_out = True
		try:
			os.killpg(proc.pid, signal.SIGTERM)
		except ProcessLookupError:
			pass
		try:
			proc.wait(timeout=10)
		except subprocess.TimeoutExpired:
			try:
				os.killpg(proc.pid, signal.SIGKILL)
			except ProcessLookupError:
				pass
			proc.wait()
	reader.join(timeout=10)
	# Only close the pipe once the reader has let go of it: a grandchild
	# that outlived the process-group kill (its own session) can keep the
	# write end open, leaving the reader blocked in its read, and closing a
	# pipe another thread is blocked reading on stalls here for as long as
	# that grandchild lives. The daemon reader thread is abandoned instead.
	if not reader.is_alive():
		proc.stdout.close()
	if timed_out:
		return None, True
	return subprocess.CompletedProcess(command, proc.returncode, "".join(lines), None), False


def _git_ignores(workspace: Path, name: str) -> bool:
	"""True when git already ignores name under workspace (a .gitignore
	rule, or the repository's info/exclude that factoryd fills in)."""
	probe = name.rstrip("/") + ("/x" if name.endswith("/") else "")
	try:
		return subprocess.run(["git", "-C", str(workspace), "check-ignore", "-q", probe], capture_output=True, check=False).returncode == 0
	except OSError:
		return False

def ensure_git_repo(workspace: Path) -> None:
	# Checking only the command's exit code is not enough: `git rev-parse
	# --show-toplevel` also succeeds -- resolving to an ancestor directory --
	# when `workspace` is merely a subdirectory of an already-git-tracked
	# directory one level up. That's exactly ticket_runner.py's own pilot
	# layout: `workspace/` starts as a plain subdirectory of the pilot dir's
	# control repo, by design (see pi/prompts/spec-plan.md's scaffold step --
	# the control repo's own .gitignore excludes `workspace/` precisely so it
	# can get its own separate repo here). Comparing the resolved toplevel
	# against `workspace` itself is what actually detects "workspace has no
	# repo of its own yet"; a bare exit-code check leaves this always-false
	# for every pilot ticket, and the model then has to work around the
	# missing repo by force-committing past the control repo's .gitignore
	# instead -- which breaks ticket_runner.py's own gate, since the commit's
	# paths end up prefixed with `workspace/` where it expects bare paths.
	toplevel = sh(["git", "rev-parse", "--show-toplevel"], cwd=workspace)
	# `Path("").resolve()` is the *process* cwd, which would read as a match
	# whenever this script happens to run from the workspace itself -- so an
	# empty stdout has to disqualify the match rather than be compared.
	has_own_repo = (
		toplevel.returncode == 0
		and bool(toplevel.stdout.strip())
		and Path(toplevel.stdout.strip()).resolve() == workspace.resolve()
	)
	if not has_own_repo:
		# Announced because --workspace is an arbitrary caller-supplied path:
		# a standalone run pointed at a subdirectory of an existing project
		# should not silently acquire a nested repo with no trace in the log.
		print(f"no git repo of its own in {workspace} -- initializing one")
		sh(["git", "init"], cwd=workspace)
		gitignore = workspace / ".gitignore"
		if not gitignore.exists():
			# __pycache__//*.pyc added 2026-09-17, found live on
			# buildgate's own live-smoke benchmark's first real run:
			# a fresh Python repo with no .gitignore of its own left every
			# round's own compiled bytecode cache untracked-but-dirty after
			# canonical verification ran the test suite, which diff_scope
			# then correctly (if unhelpfully) flagged as a scope violation
			# -- this list was JS/Dart-only until this scaffold's other
			# three entries were added, with nothing for the third
			# language this repo's own engines already build for
			# (agent/pi/tests/*.py).
			gitignore.write_text("node_modules/\ndist/\nbuild/\n.dart_tool/\n__pycache__/\n*.pyc\n")

	# This orchestrator's own bookkeeping -- the session transcript, the
	# report it writes after the build, and the structured evidence file
	# alongside it -- is not app source and must never land in the app's
	# own history. Written before the first pi invocation so the model's
	# own `git add -A`/commit never picks these up; appending even if a
	# project .gitignore already exists, since a fresh scaffold's
	# .gitignore has no reason to know about this script. BUILD_EVIDENCE.json
	# is listed here too even though write_evidence_json() doesn't run
	# until main() returns -- it's a fixed, known filename, so there's no
	# reason to wait.
	# Only what git does not already ignore: factoryd writes these same
	# names into the repository's own info/exclude before this script
	# runs (internal/workspace.ExcludeHarnessArtifacts), so on the
	# factory's own path nothing is appended here and the tracked
	# .gitignore stays exactly as the ticket left it -- found live
	# 2026-09-10, when a repo's first factory pull request carried this
	# append. The .gitignore fallback remains for running this script
	# by hand outside factoryd.
	gitignore = workspace / ".gitignore"
	existing = gitignore.read_text() if gitignore.exists() else ""
	needed = [line for line in (".pi-build-session/", ".pi-build-round-state.json", ".pi-conformity-session/", "BUILD_REPORT.md", "BUILD_EVIDENCE.json", "CONFORMITY_EVIDENCE.json", ".ticket-runner-staged.json") if not _git_ignores(workspace, line)]
	if needed:
		with gitignore.open("a") as handle:
			if existing and not existing.endswith("\n"):
				handle.write("\n")
			handle.write("\n".join(needed) + "\n")


def workspace_fingerprint(workspace: Path) -> tuple | None:
	"""A comparable snapshot of the workspace's current git state: HEAD's
	sha, sorted `git status --porcelain` output (every untracked file
	listed individually, not collapsed into its parent directory), a
	patch of every tracked/staged change, and a content hash per
	untracked file. Two snapshots comparing equal means nothing in the
	workspace changed between them.

	The untracked-file hashes exist because `git status --porcelain`
	alone only reports *that* an untracked path exists, never its
	content -- a round that edits the same still-uncommitted new file a
	prior round already created (the common case before the ticket's
	final commit) would otherwise show an identical status line both
	times and read as a no-op.

	Deliberately scoped to *the moment this is called*, not the ticket's
	overall starting commit: an earlier version of this check compared
	against review_base_sha instead, which a real Codex review of PR #4
	caught as broken two ways -- ticket_runner.py's own stage() call runs
	before build_app.py is invoked at all, so newly staged
	acceptance/contract files already make a same-round no-op look
	"changed"; and any real work a *prior* round in this same build_app.py
	invocation did remains in the diff too, so a later no-op round (or an
	escalated Sonnet pass that itself does nothing) inherits that earlier
	round's credit and reports success for work it didn't do. Comparing
	round-start to round-end instead of ticket-start to round-end catches
	both: it needs no review_base_sha (so ticket 1, whose review base is
	intentionally None, is covered too) and correctly treats each round on
	its own.

	Returns None on any git error -- callers only use this to withhold
	success, never to force failure, so a git hiccup here should not
	itself block a real success.
	"""
	head = _git_snapshot_command(["git", "rev-parse", "HEAD"], cwd=workspace, acceptable_codes={0})
	if head.returncode != 0:
		return None
	status = _git_snapshot_command(
		["git", "status", "--porcelain", "--untracked-files=all"], cwd=workspace, acceptable_codes={0},
	)
	if status.returncode != 0:
		return None
	diff = _git_snapshot_command(["git", "diff", "HEAD"], cwd=workspace, acceptable_codes={0, 1})
	if diff.returncode not in (0, 1):
		return None
	untracked_hashes = []
	for line in status.stdout.splitlines():
		if not line.startswith("?? "):
			continue
		rel_path = line[3:]
		try:
			content = (workspace / rel_path).read_bytes()
		except OSError:
			content = b""
		untracked_hashes.append((rel_path, hashlib.sha256(content).hexdigest()))
	return (
		head.stdout.strip(),
		"\n".join(sorted(status.stdout.splitlines())),
		diff.stdout,
		tuple(sorted(untracked_hashes)),
	)


# The most of a failing command's output kept in its log file.
FAILURE_LOG_LIMIT = 2_000_000


def failure_output(raw: str, log_path: Path | None) -> str:
	"""A failing command's output as a corrective prompt shows it: the block
	around its first reported failure plus its end (round_feedback's
	failure_excerpt), not just its last few thousand characters. With
	log_path, the complete redacted output is also saved there for the agent
	to read; a write failure only loses that copy."""
	full = redact(raw, FAILURE_LOG_LIMIT)
	if log_path is not None:
		try:
			log_path.parent.mkdir(parents=True, exist_ok=True)
			log_path.write_text(full + "\n", encoding="utf-8")
		except OSError:
			pass
	return round_feedback.failure_excerpt(full)


def timeout_output(header: str, exc: subprocess.TimeoutExpired, log_path: Path | None) -> str:
	"""failure_output for a command that timed out: header, which says so
	and names the command, stays the first line whatever the excerpt of the
	captured output keeps."""
	captured = (exc.stdout or b"").decode(errors="ignore") if isinstance(exc.stdout, bytes) else (exc.stdout or "")
	return f"{header}\n{failure_output(captured, log_path)}".rstrip()


def _feedback_log(log_dir: Path | None, name: str) -> Path | None:
	return log_dir / name if log_dir is not None else None


FAST_CHECK_LOG, VERIFY_LOG, ORACLE_LOG = "fast-check.log", "verify.log", "oracle.log"
SETUP_LOG = "setup.log"
# Each repository setup command (`.factory.yml` setup:) gets this long.
SETUP_TIMEOUT_SECONDS = 10 * 60
SETUP_FAILED_BLOCKER = "setup command failed: "


# Output kept from one setup or autofix command: its tail, which is where a
# failing tool says why.
OWN_GROUP_OUTPUT_BYTES = 1 << 20


def run_in_own_group(args: list[str], *, cwd: Path, timeout: float, env: dict | None = None, kill_after_exit: bool = True) -> tuple[int | None, str]:
	"""Runs args in its own session, output (stdout and stderr together) in a
	temporary file so a process it leaves behind cannot hold a pipe open.
	Returns (exit code, the last OWN_GROUP_OUTPUT_BYTES of output), the exit
	code None when it timed out. A timeout kills the whole process group.
	With kill_after_exit (autofix), so does a normal exit: a command that
	backgrounded work must not write after the caller has checked the tree.
	Without it (setup), what the command left running in the background
	keeps running, as it does in a verify or gate sandbox. A process that
	detached into its own session is outside the group either way and ends
	with the container. Raises OSError when it cannot start."""
	with tempfile.TemporaryFile() as out:
		proc = subprocess.Popen(args, cwd=cwd, env=env, stdin=subprocess.DEVNULL, stdout=out, stderr=subprocess.STDOUT, start_new_session=True)
		try:
			code: int | None = proc.wait(timeout=timeout)
		except subprocess.TimeoutExpired:
			code = None
		if code is None or kill_after_exit:
			_kill_group(proc)
		size = out.seek(0, os.SEEK_END)
		out.seek(max(size - OWN_GROUP_OUTPUT_BYTES, 0))
		return code, out.read().decode(errors="replace")


def _kill_group(proc: subprocess.Popen) -> None:
	"""Kills proc's process group and waits, briefly, until it is gone."""
	try:
		os.killpg(proc.pid, signal.SIGKILL)
	except (ProcessLookupError, PermissionError):
		pass
	proc.wait()
	deadline = time.monotonic() + 5
	while time.monotonic() < deadline:
		try:
			os.killpg(proc.pid, 0)
		except (ProcessLookupError, PermissionError):
			return
		time.sleep(0.01)


def run_setup(workspace: Path, commands: list[str], log_dir: Path | None) -> tuple[str | None, str]:
	"""Runs the repository's setup commands in order, in the workspace and
	this process's own environment, before the round's checks. Their
	combined output is saved (redacted, bounded) as setup.log in log_dir.
	Returns (None, "") when all passed; else the first failing command and
	the corrective-prompt text for it, and the later commands do not run."""
	if not commands:
		return None, ""
	output = ""
	failed, header = None, ""
	for command in commands:
		output += f"$ {command}\n"
		try:
			code, captured = run_in_own_group(["sh", "-c", command], cwd=workspace, timeout=SETUP_TIMEOUT_SECONDS, kill_after_exit=False)
		except OSError as exc:
			failed, header = command, f"[SETUP] `{command}` failed; the checks were not run this round."
			output += f"{exc}\n"
			break
		output += f"{captured}\n"
		if code is None:
			failed, header = command, f"[SETUP] `{command}` timed out after {SETUP_TIMEOUT_SECONDS // 60} minutes; the checks were not run this round."
			break
		if code != 0:
			failed, header = command, f"[SETUP] `{command}` failed; the checks were not run this round."
			break
	log = _feedback_log(log_dir, SETUP_LOG)
	if failed is None:
		if log is not None:
			try:
				log.parent.mkdir(parents=True, exist_ok=True)
				log.write_text(redact(output, FAILURE_LOG_LIMIT) + "\n", encoding="utf-8")
			except OSError:
				pass
		return None, ""
	return failed, f"{header}\n\n{failure_output(output, log)}"


def run_verification_after_setup(workspace: Path, *, setup_commands: list[str], **kwargs):
	"""run_setup, then run_verification unless setup failed. Returns the
	failed setup command (None when it passed or there was none) and
	run_verification's tuple; a failed setup leaves it at the "not run"
	values with the setup failure as its tail, as a failed fast check does."""
	failed, tail = run_setup(workspace, setup_commands, kwargs.get("log_dir"))
	if failed is not None:
		return failed, (None, None, False, tail, False, None)
	return None, run_verification(workspace, **kwargs)


AUTOFIX_LOG = "autofix.log"
# Each repository autofix command (`.factory.yml` autofix:) gets this long.
AUTOFIX_TIMEOUT_SECONDS = 5 * 60
AUTOFIX_REVERTED_LISTED = 20


AUTOFIX_UNCHECKED_BLOCKER = "autofix ran but its changes could not be checked against the ticket's files"
AUTOFIX_SKIPPED_NOTE = "could not list the round's changed files"


def list_changed_paths(workspace: Path, base_sha: str | None) -> set[str]:
	"""The paths that differ from base_sha (HEAD when unknown) or are
	untracked and not ignored, harness artifacts excluded, or ScopeListingError
	when any part of that listing failed."""
	return _changed_names(workspace, base_sha, strict=True)


def _git_literal(args: list[str], workspace: Path) -> subprocess.CompletedProcess:
	"""A git call whose paths are literal (a file named `:x` is not pathspec magic); bytes out."""
	return subprocess.run(
		["git", *args], cwd=workspace, capture_output=True, check=False, stdin=subprocess.DEVNULL, timeout=60,
		env={**os.environ, "GIT_LITERAL_PATHSPECS": "1"},
	)


def _restore_from_base(workspace: Path, base: str, name: str) -> None:
	"""Puts path `name` back to its content (and file mode) at `base` without
	any git write: bytes from `git show`, so a binary file comes back
	identical; a file the base does not have is deleted, and the directories
	that became empty with it. Everything it needs from git is fetched before
	the worktree is touched; raises OSError (path left as it is) when git
	fails, or when the base entry is a submodule or a directory."""
	path = workspace / name
	try:
		tree = _git_literal(["ls-tree", "-z", base, "--", name], workspace)
	except (subprocess.TimeoutExpired, OSError) as exc:
		raise OSError(f"git ls-tree: {exc}") from exc
	if tree.returncode != 0:
		raise OSError(f"git ls-tree exited {tree.returncode}")
	entry = tree.stdout.split(b"\0")[0].decode(errors="replace")
	mode = entry.split(" ", 1)[0] if entry else ""
	content = b""
	if mode:
		if mode not in ("100644", "100755", "120000"):
			raise OSError(f"base entry has mode {mode}")
		try:
			shown = _git_literal(["show", f"{base}:{name}"], workspace)
		except (subprocess.TimeoutExpired, OSError) as exc:
			raise OSError(f"git show: {exc}") from exc
		if shown.returncode != 0:
			raise OSError(f"git show exited {shown.returncode}")
		content = shown.stdout
	if path.is_dir() and not path.is_symlink():
		raise OSError("path is a directory")
	if path.is_symlink() or path.is_file():
		path.unlink()
	if not mode:
		parent = path.parent
		while parent != workspace and workspace in parent.parents:
			try:
				parent.rmdir()
			except OSError:
				break
			parent = parent.parent
		return
	path.parent.mkdir(parents=True, exist_ok=True)
	if mode == "120000":
		os.symlink(content.decode(errors="surrogateescape"), path)
		return
	path.write_bytes(content)
	path.chmod(0o755 if mode == "100755" else 0o644)


def run_autofix(
	workspace: Path, commands: list[str], base_sha: str | None, log_dir: Path | None, env: dict | None = None,
) -> dict | None:
	"""Runs the repository's autofix commands (`.factory.yml` autofix:) in
	order, in the workspace and the round's environment, after the agent's
	turn and before the round's checks. Advisory: a command that exits
	non-zero or times out is recorded, never a failure of the round (many
	`--fix` tools exit non-zero when they fixed something). Each command runs
	in its own process group and nothing it started is alive when it is done.

	The paths the tree differs from the base in (or has untracked, not
	ignored) before autofix are its scope; a path outside it that differs
	afterwards is autofix's doing alone and is put back to the base's content,
	so a formatter cannot widen the ticket's diff. The listings are strict:
	when the first fails autofix does not run (`skipped`); when the second
	fails nothing is reverted and `scope_check_failed` is set; a path that
	could not be restored is in `revert_failed`. autofix_blocker() turns the
	last two into a failed round. The combined output and any revert are saved
	(redacted, bounded) as autofix.log in log_dir. Returns the round's
	`autofix` evidence record, or None when there are no commands."""
	if not commands:
		return None
	record: dict = {
		"commands": [], "reverted_count": 0, "reverted": [], "revert_failed_count": 0, "revert_failed": [],
		"scope_check_failed": False,
	}

	def save(text: str) -> dict:
		if log_dir is not None:
			try:
				log_dir.mkdir(parents=True, exist_ok=True)
				(log_dir / AUTOFIX_LOG).write_text(redact(text, FAILURE_LOG_LIMIT) + "\n", encoding="utf-8")
			except OSError:
				pass
		return record

	try:
		scope = list_changed_paths(workspace, base_sha)
	except ScopeListingError as exc:
		record["skipped"] = AUTOFIX_SKIPPED_NOTE
		return save(f"autofix skipped: {AUTOFIX_SKIPPED_NOTE} ({exc})")
	output = ""
	for command in commands:
		output += f"$ {command}\n"
		started = time.monotonic()
		exit_code, timed_out = 0, False
		try:
			code, captured = run_in_own_group(["sh", "-c", command], cwd=workspace, timeout=AUTOFIX_TIMEOUT_SECONDS, env=env)
			output += f"{captured}\n"
			if code is None:
				timed_out, exit_code = True, -1
				output += f"[timed out after {AUTOFIX_TIMEOUT_SECONDS // 60} minutes]\n"
			else:
				exit_code = code
		except OSError as exc:
			exit_code = 127
			output += f"{exc}\n"
		record["commands"].append({
			"command": command[:200], "exit_code": exit_code, "timed_out": timed_out,
			"duration_s": round(time.monotonic() - started, 3),
		})
	try:
		touched = sorted(list_changed_paths(workspace, base_sha) - scope)
	except ScopeListingError as exc:
		record["scope_check_failed"] = True
		return save(output + f"could not check autofix's changes against the ticket's files ({exc}); nothing was reverted\n")
	base = base_sha or "HEAD"
	reverted, failed = [], []
	for name in touched:
		try:
			_restore_from_base(workspace, base, name)
			reverted.append(name)
		except OSError as exc:
			failed.append(name)
			output += f"could not revert {name}: {exc}\n"
	if reverted:
		output += f"reverted {len(reverted)} path(s) the ticket had not changed: {', '.join(reverted[:AUTOFIX_REVERTED_LISTED])}\n"
	record.update(
		reverted_count=len(reverted), reverted=reverted[:AUTOFIX_REVERTED_LISTED],
		revert_failed_count=len(failed), revert_failed=failed[:AUTOFIX_REVERTED_LISTED],
	)
	return save(output)


def autofix_blocker(record: dict | None) -> str | None:
	"""The one way autofix fails a round: the tree may hold edits outside the
	ticket's files that nobody could enumerate or put back."""
	if not record:
		return None
	if record.get("scope_check_failed"):
		return AUTOFIX_UNCHECKED_BLOCKER
	if record.get("revert_failed"):
		return "autofix changed files outside the ticket's and could not revert: " + ", ".join(record["revert_failed"][:5])
	return None


def autofix_prompt_note(record: dict | None) -> str:
	"""The line a corrective prompt carries for each autofix command that
	failed or timed out ("" when none did)."""
	if not record:
		return ""
	return "\n".join(
		f"autofix command failed (advisory): `{c['command']}` exit {c['exit_code']}"
		for c in record["commands"] if c["exit_code"] != 0 or c["timed_out"]
	)


def run_verification(
	workspace: Path, *, verify_command_override: str | None = None, fast_check_command: str | None = None,
	log_dir: Path | None = None,
) -> tuple[str | None, bool | None, bool, str, bool, bool | None]:
	# verify_command_override, when set, is a factoryd-resolved ticket's own
	# declared Verify-Command: -- see --verify-command's own doc comment in
	# main() for why resolve_verify_command()'s root-only auto-detection is
	# not trusted to already agree with it. Found live (2026-09-09, a real
	# brownfield monorepo with a top-level `make verify` needing a
	# toolchain -- Flutter/Dart -- the project sandbox image never had):
	# every corrective round's own internal verify silently ran the wrong
	# command and failed on a missing-toolchain error with nothing to do
	# with the agent's actual change, guaranteeing the round loop could
	# never converge regardless of code correctness, while factoryd's own
	# external canonical_verify gate (which *did* already know the ticket's
	# real command) ran the right one -- the two verify commands disagreeing
	# for the entire run went undetected because neither side compared
	# notes with the other.
	#
	# fast_check_command, when set, is a cheap check (format/lint/compile)
	# run first, in the same way and with the same 20-minute timeout/
	# redaction as the full verify command below -- a failure here short-
	# circuits the round without paying for the full command at all.
	#
	# On a fast-check failure/timeout, verify_command/verify_passed/
	# verify_timed_out are left at exactly the same "not run" values as
	# the no-command-resolvable case below (None, None, False) -- NOT the
	# fast check's own command/False -- so a consumer can never mistake
	# "the fast check failed, canonical verification was never attempted"
	# for "canonical verification actually ran and failed" (Codex review
	# of PR #90). The fast check's own outcome travels only through
	# fast_check_ran/fast_check_passed, plus the returned tail, which
	# names the fast check command inline and is clearly labeled so it's
	# still legible to a human or corrective prompt reading it.
	if fast_check_command:
		try:
			completed = sh(["bash", "-o", "pipefail", "-lc", fast_check_command], cwd=workspace, timeout=20 * 60)
			if completed.returncode != 0:
				output = failure_output(f"{completed.stdout}\n{completed.stderr}", _feedback_log(log_dir, FAST_CHECK_LOG))
				tail = f"[FAST CHECK] `{fast_check_command}` failed; the full verify command was not run this round.\n\n{output}"
				return None, None, False, tail, True, False
		except subprocess.TimeoutExpired as exc:
			header = f"[FAST CHECK] `{fast_check_command}` timed out after 20 minutes; the full verify command was not run this round."
			return None, None, False, timeout_output(header, exc, _feedback_log(log_dir, FAST_CHECK_LOG)), True, False

	command = verify_command_override if verify_command_override else resolve_verify_command(workspace)
	fast_check_passed = True if fast_check_command else None
	if not command:
		return None, None, False, "", bool(fast_check_command), fast_check_passed
	try:
		completed = sh(["bash", "-o", "pipefail", "-lc", command], cwd=workspace, timeout=20 * 60)
		combined = f"{completed.stdout}\n{completed.stderr}"
		output = redact(combined) if completed.returncode == 0 else failure_output(combined, _feedback_log(log_dir, VERIFY_LOG))
		return command, completed.returncode == 0, False, output, bool(fast_check_command), fast_check_passed
	except subprocess.TimeoutExpired as exc:
		header = f"verification command timed out after 20 minutes: {command}"
		return command, False, True, timeout_output(header, exc, _feedback_log(log_dir, VERIFY_LOG)), bool(fast_check_command), fast_check_passed


def changed_go_files(workspace: Path, base_sha: str | None) -> list[str]:
	"""Workspace-relative .go files the ticket has touched: tracked files
	differing from base_sha (HEAD when unknown; with base_sha this also
	covers already-committed work) plus untracked, non-ignored files.
	Pre-existing drift in files the agent never touched is out of scope."""
	names: set[str] = set()
	for args in (
		["git", "diff", "--name-only", "--diff-filter=d", "-z", base_sha or "HEAD", "--"],
		["git", "ls-files", "--others", "--exclude-standard", "-z"],
	):
		try:
			done = sh(args, cwd=workspace, timeout=60)
		except (subprocess.TimeoutExpired, OSError):
			continue
		if done.returncode == 0:
			names.update(n for n in done.stdout.split("\0") if n.endswith(".go"))
	# is_symlink() first, not just is_file(): found in review (2026-09-21) --
	# a symlink the agent committed or created (its target need not be under
	# Allowed-Files, or even inside the workspace) would otherwise pass
	# is_file() and gofmt_changed_files would then gofmt -w through it,
	# rewriting a file this ticket never touched after the tree it changed
	# has already been verified.
	return sorted(
		n for n in names
		if not (workspace / n).is_symlink() and (workspace / n).is_file()
	)


def gofmt_changed_files(workspace: Path, base_sha: str | None) -> list[str]:
	"""Formatter signal for Go: after canonical verify passed, gofmt -w the
	.go files this ticket changed, and return the ones it rewrote.

	Found live 2026-09-21: `go vet && go test` passed, but the post-build
	spec-conformity reviewer flagged `gofmt -l` drift (a double space) and
	quarantined an otherwise-correct build; nothing in the loop could ever
	see formatting. gofmt is deterministic and whitespace-only, so it is
	applied directly rather than spending a model round on it. Scope is
	only files the ticket already changed (so never outside Allowed-Files),
	and it runs inside the same sandbox process as the build. A strict
	no-op -- never a failure -- when gofmt is absent, the tree is not a git
	repo, no .go file changed, or gofmt errors."""
	if not shutil.which("gofmt"):
		return []
	files = changed_go_files(workspace, base_sha)
	if not files:
		return []
	try:
		listed = sh(["gofmt", "-l", "--", *files], cwd=workspace, timeout=120)
		if listed.returncode != 0 or not listed.stdout.strip():
			return []
		drifted = [f for f in listed.stdout.splitlines() if f.strip()]
		fixed = sh(["gofmt", "-w", "--", *drifted], cwd=workspace, timeout=120)
	except (subprocess.TimeoutExpired, OSError):
		return []
	return drifted if fixed.returncode == 0 else []


def run_reference_oracle(workspace: Path, *, oracle_command: str, log_dir: Path | None = None) -> tuple[bool, str]:
	"""Runs oracle_command (an operator-authored check against a host
	directory already bind-mounted read-only into this container by the
	Go launcher -- see internal/sandbox.LaunchSpec.ReferenceOracleDir's
	own doc comment; this function has no say over what's mounted or
	where, only over invoking whatever command was configured), exactly
	like run_verification's own canonical-verify shell-out. Only called
	once verify_passed is True this round (see run_build's own call
	site) -- the oracle is an additional correctness bar on top of
	canonical verify, not a replacement for it, mirroring the existing
	post-build reference_oracle named gate's own
	`buildRes.ExitCode == 0 && verifyRes.ExitCode == 0` gating
	(run_ticket.go) at the per-round level instead of once at the end.

	Returns (passed, tail) -- tail is the redacted command output,
	included in a failing round's corrective prompt the same way
	verify_tail already is."""
	try:
		completed = sh(["bash", "-o", "pipefail", "-lc", oracle_command], cwd=workspace, timeout=20 * 60)
		combined = f"{completed.stdout}\n{completed.stderr}"
		if completed.returncode == 0:
			return True, redact(combined)
		return False, failure_output(combined, _feedback_log(log_dir, ORACLE_LOG))
	except subprocess.TimeoutExpired as exc:
		header = f"reference-oracle command timed out after 20 minutes: {oracle_command}"
		return False, timeout_output(header, exc, _feedback_log(log_dir, ORACLE_LOG))


def maybe_run_reference_oracle(
	workspace: Path, *, oracle_command: str | None, verify_passed: bool | None, log_dir: Path | None = None,
) -> tuple[str | None, bool | None, str]:
	"""Shared gating for run_reference_oracle's two call sites (the local
	round loop and the Sonnet-fallback escalation pass): only run once
	verify itself has passed -- an additional correctness bar on top of
	canonical verify, not a substitute for it (see run_reference_oracle's
	own doc comment). Returns (None, None, "") -- oracle_passed's own
	"not resolved" convention, the same one fast_check_passed already
	uses -- when no oracle is configured or verify didn't pass this
	round/pass. Pulled out so a future change to this gating condition
	can't land in one call site and not the other (found via review).

	Returns the command actually recorded (None when it didn't run) as
	its first element, not just (passed, tail): both call sites need
	this for their own Round(...)'s oracle_command field, and re-deriving
	it there via `oracle_command if oracle_passed is not None else None`
	in two places is exactly the kind of duplicated gating-condition
	logic this function itself exists to avoid (found via review)."""
	if oracle_command and verify_passed is True:
		extra = {"log_dir": log_dir} if log_dir is not None else {}
		passed, tail = run_reference_oracle(workspace, oracle_command=oracle_command, **extra)
		return oracle_command, passed, tail
	return None, None, ""

CORRECTIVE_INTRO = prompt_templates.load("build_corrective.intro", ("round_index", "max_rounds", "blockers"))
CORRECTIVE_VERIFY = prompt_templates.load("build_corrective.verify", ("verify_command", "verify_tail"))
CORRECTIVE_EXCERPT = prompt_templates.load("build_corrective.excerpt", ("verify_tail",))
CORRECTIVE_ORACLE = prompt_templates.load("build_corrective.oracle", ("oracle_command", "oracle_tail"))
CORRECTIVE_REVIEWER = prompt_templates.load("build_corrective.reviewer", ("detail",))
CORRECTIVE_CLOSING = prompt_templates.load_text("build_corrective.closing")
CORRECTIVE_HISTORY = prompt_templates.load("build_corrective.history", ("history",))
CORRECTIVE_LOG = prompt_templates.load("build_corrective.log", ("failure_log", "failing"))
CORRECTIVE_AGENT = prompt_templates.load("build_corrective.agent", ("agent_notes",))

# The one extra turn a build that ends without passing gets, in its own
# session, to leave notes for whoever attempts the ticket next. The reply is
# written only to <session dir>/HANDOFF_NOTES_FILE: the session folder is
# copied out by the host and removed from the worktree before any later step
# (SC-018), so nothing else here may carry the text.
HANDOFF_NOTES_PROMPT = prompt_templates.load_text("build_handoff_notes")
HANDOFF_NOTES_FILE = "handoff-notes.md"
HANDOFF_NOTES_TIMEOUT_S = 180
HANDOFF_NOTES_MIN_REMAINING_S = 300
# At most this many bytes of UTF-8, cut on a character boundary. The host
# keeps a file of up to 16 KiB (evidence.maxAgentNotesBytes), so a larger one
# was not written by this script.
HANDOFF_NOTES_MAX_BYTES = 12_000
# The launch's timeout in seconds, set by the host for the build launch only.
# The container's wall clock is not consulted: the script measures its own
# elapsed time on the monotonic clock against this budget.
BUILD_TIME_BUDGET_ENV = "FACTORY_BUILD_TIME_BUDGET_SECONDS"
_monotonic = time.monotonic
# Set first thing in main(); run_build falls back to its own entry.
_process_started: float | None = None
CORRECTIVE_STUCK = prompt_templates.load("build_corrective.stuck", ("streak",))
ROUND_CHECKLIST = prompt_templates.load("build_round_checklist", ("verify",))
ESCALATION_PROMPT = prompt_templates.load("build_escalation", ("spec_text", "corrective"))


def corrective_prompt(
	*,
	round_index: int,
	max_rounds: int,
	verify_command: str | None,
	verify_tail: str,
	blockers: list[str],
	reviewer: ReviewSignal,
	review_policy: str,
	oracle_command: str | None = None,
	oracle_tail: str = "",
	history: str = "",
	failure_log: str = "",
	failing: list[str] | None = None,
	agent_notes: str = "",
	streak: int = 0,
) -> str:
	parts = [CORRECTIVE_INTRO.format(round_index=round_index, max_rounds=max_rounds, blockers=", ".join(blockers))]
	if history:
		parts.append(CORRECTIVE_HISTORY.format(history=history))
	if agent_notes:
		parts.append(CORRECTIVE_AGENT.format(agent_notes=agent_notes))
	if verify_command and verify_tail:
		parts.append(CORRECTIVE_VERIFY.format(verify_command=verify_command, verify_tail=verify_tail))
	elif verify_tail:
		# verify_command is None here because canonical verification was
		# never attempted this round (e.g. the fast check failed first) --
		# verify_tail is still shown since it names its own command inline
		# (see run_verification's "[FAST CHECK] `<cmd>`..." tail), just
		# without mislabeling it as the canonical command.
		parts.append(CORRECTIVE_EXCERPT.format(verify_tail=verify_tail))
	# oracle_tail only ever arrives non-empty when the oracle actually ran
	# (run_build only calls run_reference_oracle once verify_passed is
	# True this round), so unlike verify_tail there's no "ran but empty
	# output" ambiguity to disambiguate with a command label.
	if oracle_command and oracle_tail:
		parts.append(CORRECTIVE_ORACLE.format(oracle_command=oracle_command, oracle_tail=oracle_tail))
	if failure_log:
		parts.append(CORRECTIVE_LOG.format(failure_log=failure_log, failing=", ".join(failing or []) or "none recognised"))
	if review_policy != "advisory" and reviewer.outcome == "flagged" and reviewer.detail:
		parts.append(CORRECTIVE_REVIEWER.format(detail=reviewer.detail))
	if streak >= 2:
		parts.append(CORRECTIVE_STUCK.format(streak=streak))
	parts.append(CORRECTIVE_CLOSING)
	return "\n\n".join(part.removesuffix("\n") for part in parts)


# Caps the diff text inlined into a reviewer's prompt (code_review.py's own
# code_review_prompt and spec_conformity_prompt below) -- large enough for a
# real ticket-sized change, small enough to leave headroom in the relay's
# hourly token budget. A diff past this length is truncated by
# format_diff_for_prompt; the stat above it still lists every changed file
# so the reviewer knows what it didn't see in full. Found live (example-app
# walk, 2026-09-28): a code reviewer told to "diff the workspace" itself
# explored with tools for ~13 turns at ~77k input tokens each before the
# relay's own sliding-window limiter cut it off with four straight 429s and
# no findings at all; inlining the diff here turns that into a single read.
_DIFF_TRUNCATE_LIMIT = 120_000

# Harness-written files no reviewer must ever be shown as part of "the
# change" -- each phase's own evidence file, and the per-phase pi session
# directories (".pi-code-review-session", ".pi-conformity-session", the
# build loop's own round sessions -- all match "**/.pi-*-session/**"). Git
# pathspec ":(exclude)" magic requires at least one positive pathspec
# alongside the excludes, hence the leading ".". Shared by
# code_review.py's own code_review_prompt and spec_conformity_prompt below
# (both call workspace_diff), so a reviewer of either kind sees the same
# scoped-down diff.
_HARNESS_ARTIFACT_PATHSPECS = (
	".",
	":(exclude,glob)**/.pi-*-session/**",
	":(exclude)BUILD_EVIDENCE.json",
	":(exclude)BUILD_REPORT.md",
	":(exclude)CONFORMITY_EVIDENCE.json",
	":(exclude)CODE_REVIEW_EVIDENCE.json",
)


def workspace_diff(workspace: Path, review_base_sha: str) -> tuple[str, str] | None:
	"""Returns (stat, diff) for the workspace's current changes against
	review_base_sha, both already scoped away from harness-written
	artifacts via _HARNESS_ARTIFACT_PATHSPECS. Returns None if either `git
	diff` invocation fails or times out -- .git is mounted read-only in
	the sandbox both code_review.py and conformity_review.py run in, so a
	read here normally works, but a base sha the workspace doesn't have (a
	shallow clone) could still fail it; the caller falls back to the
	instruction-only prompt in that case, same as when review_base_sha is
	unknown altogether. Shared by code_review.py's own run_code_review and
	run_spec_conformity_review below -- moved here (from a private copy in
	code_review.py) so both reviews inline the same scoped diff instead of
	only code review having it (found live, example-app run 3, 2026-09-28: the
	conformity reviewer, still told to `git diff` for itself, burned the
	relay's whole hourly token budget across 30 tool-exploring turns and
	ended in three straight 429s with no verdicts at all)."""
	outputs = []
	for extra in (["--stat"], []):
		try:
			# Bytes, decoded with replacement: a diff may carry non-UTF-8
			# bytes (found live, M4 smoke 2026-09-28: a build agent copied
			# .git's zlib objects into the workspace and a strict text-mode
			# decode raised UnicodeDecodeError before any evidence was
			# written).
			completed = subprocess.run(
				["git", "diff", *extra, review_base_sha, "--", *_HARNESS_ARTIFACT_PATHSPECS],
				cwd=workspace, capture_output=True, timeout=60, check=False,
			)
		except (subprocess.TimeoutExpired, OSError):
			return None
		if completed.returncode != 0:
			return None
		outputs.append(completed.stdout.decode("utf-8", errors="replace"))
	return outputs[0], outputs[1]


def format_diff_for_prompt(stat: str, diff_text: str, review_base_sha: str | None, limit: int = _DIFF_TRUNCATE_LIMIT) -> str:
	"""Renders workspace_diff's own (stat, diff_text) pair as the inlined
	"--- diff --stat ---"/"--- diff ---" block both code_review_prompt and
	spec_conformity_prompt use, truncating diff_text at limit characters
	(the stat above it is never truncated, so the reviewer always sees
	every changed file even when the diff body itself is cut off)."""
	truncated = len(diff_text) > limit
	body = diff_text[:limit]
	truncation_note = (
		f"\n[diff truncated here at {limit:,} characters -- for anything past this point, read the "
		"still-changed files listed in the stat above selectively rather than re-running `git diff`]"
		if truncated else ""
	)
	against = review_base_sha or "the commit this session started from"
	return (
		f"--- diff --stat against {against} ---\n{stat}--- end diff --stat ---\n\n"
		f"--- diff against {against} ---\n{body}{truncation_note}\n--- end diff ---"
	)


INSTRUCTIONS_DIFF = prompt_templates.load("review.instructions_diff", ("touched", "diff"))
# The most of an instructions diff a review prompt carries.
MAX_INSTRUCTIONS_DIFF_CHARS = 60_000


def neutralise_instructions_diff(text: str) -> str:
	"""The instructions diff is text the build wrote, placed inside a fenced
	block; break every run of the fence characters (and the <<< >>> section
	delimiters, as draft_spec.neutralise_feedback does) so it can never close
	the fence early or open a new one."""
	while "```" in text:
		text = text.replace("```", "` ` `")
	return text.replace("<<<", "< < <").replace(">>>", "> > >")


# One file's header in the host's instructions.diff: `=== "<path>" (<what>) ===`
# at column 0. The path is Go-quoted. Content lines are prefixed with +, -, a
# space or @ by the host, so a header-shaped line inside a diff is not one.
INSTRUCTIONS_DIFF_HEADER = re.compile(r'^=== ("(?:[^"\\\n]|\\.)*") \(([^\n]*?)\) ===$', re.MULTILINE)


# The most entries, and characters, of a path list in the instructions block.
MAX_INSTRUCTIONS_LIST_ENTRIES = 400
MAX_INSTRUCTIONS_LIST_CHARS = 40_000
MAX_INSTRUCTIONS_LIST_PATH_CHARS = 300


def printable_instructions_line(raw: bytes) -> str:
	"""One line of the host's file as visible text: decoded as UTF-8 with
	replacement, a carriage return shown as the two characters \\r and any
	other C0 control character but tab as its \\xNN form, so a content line
	can never be split into a forged header line."""
	line = raw.decode("utf-8", errors="replace").replace("\r", "\\r")
	return "".join(f"\\x{ord(c):02x}" if ord(c) < 0x20 and c != "\t" else c for c in line)


def instructions_path_list(entries: list[str]) -> list[str]:
	"""Bullet lines for entries (each `- "<quoted path>" (<what>)`), cut to the
	list bounds: a path is cut to MAX_INSTRUCTIONS_LIST_PATH_CHARS, and at most
	MAX_INSTRUCTIONS_LIST_ENTRIES entries and MAX_INSTRUCTIONS_LIST_CHARS
	characters are listed, then one line says how many were left out."""
	lines, used = [], 0
	for quoted, what in entries:
		if len(quoted) > MAX_INSTRUCTIONS_LIST_PATH_CHARS:
			quoted = quoted[:MAX_INSTRUCTIONS_LIST_PATH_CHARS] + "..."
		line = f"- {quoted} ({what})"
		if len(lines) >= MAX_INSTRUCTIONS_LIST_ENTRIES or used + len(line) + 1 > MAX_INSTRUCTIONS_LIST_CHARS:
			lines.append(
				f"... and {len(entries) - len(lines)} more instruction paths not listed here: "
				"this change touches too many instruction files to review; report that as a finding."
			)
			break
		lines.append(line)
		used += len(line) + 1
	return lines


def instructions_diff_block(path: Path | None) -> str:
	"""The text a review prompt appends after the inline diff for the host's
	--instructions-diff file: what the build did to the repository's
	instruction files, which the workspace itself shows as they were before
	it (SC-019). "" when path is unset or the file is empty, so the prompt is
	then byte-identical to one without the flag.

	The file is read as bytes and split on newlines only; control characters
	in a line are made visible (printable_instructions_line). A list of the
	paths the headers name comes first, bounded by instructions_path_list;
	then the diff capped at MAX_INSTRUCTIONS_DIFF_CHARS. Each header owns a
	section, from its line to the next header; a path whose section runs past
	the cap is named after the diff cut as not shown in full, the one the cut
	falls in included, so a build cannot hide an instruction change behind
	filler earlier in the file."""
	if path is None:
		return ""
	text = "\n".join(printable_instructions_line(raw) for raw in path.read_bytes().split(b"\n"))
	if not text.strip():
		return ""
	found = list(INSTRUCTIONS_DIFF_HEADER.finditer(text))
	sections = [(m.group(1), m.group(2), found[i + 1].start() if i + 1 < len(found) else len(text)) for i, m in enumerate(found)]
	not_shown = []
	if len(text) > MAX_INSTRUCTIONS_DIFF_CHARS:
		omitted = len(text) - MAX_INSTRUCTIONS_DIFF_CHARS
		not_shown = [(quoted, what) for quoted, what, end in sections if end > MAX_INSTRUCTIONS_DIFF_CHARS]
		text = text[:MAX_INSTRUCTIONS_DIFF_CHARS] + f"\n[instructions diff truncated here: {omitted:,} more characters not shown]"
	touched = ""
	if sections:
		lines = [f"Instruction paths this change touched ({len(sections)}):"]
		lines += instructions_path_list([(quoted, what) for quoted, what, _ in sections])
		if not_shown:
			lines += ["", "Not shown in full below (the diff was cut):"]
			lines += instructions_path_list(not_shown)
		touched = neutralise_instructions_diff("\n".join(lines)) + "\n\n"
	return "\n\n" + INSTRUCTIONS_DIFF.format(touched=touched, diff=neutralise_instructions_diff(text.rstrip("\n"))).removesuffix("\n")


def read_acceptance_criteria(path: Path) -> list[str]:
	"""Returns one criterion per non-empty, non-comment line of path (the
	approved spec's own numbered acceptance criteria, one per line, e.g.
	"1. Foo does X")."""
	criteria = []
	for line in path.read_text().splitlines():
		line = line.strip()
		if line and not line.startswith("#"):
			criteria.append(line)
	return criteria


CONFORMITY_COMMAND_OUTCOME_RULE = (
	prompt_templates.load_text("spec_conformity.command_outcome_rule")
)

CONFORMITY_FORMATTING_RULE = (
	prompt_templates.load_text("spec_conformity.formatting_rule")
)

CONFORMITY_JSON_CONTRACT = (
	prompt_templates.load_text("spec_conformity.json_contract")
)

# The three constants above are shared with combined_review.py's own
# combined_review_prompt, which folds this same spec-conformity task and
# code_review.py's own free-form code-review task into one prompt/one
# turn (see combined_review.py's own module doc comment for why: review
# was 50% of a live example-app request's spend, 2026-09-28, split across two
# separate fresh sandboxed sessions over the same diff). Keeping the rule
# text here as named constants, instead of only inline inside this
# function's own f-string, is what lets that combined prompt reuse the
# exact wording every existing standalone reviewer already relies on
# instead of forking a second, driftable copy.

CONFORMITY_DIFF_SELF = prompt_templates.load("spec_conformity.diff_self", ("base",))
CONFORMITY_DIFF_INLINE = prompt_templates.load("spec_conformity.diff_inline", ("diff",))
CONFORMITY_PROMPT = prompt_templates.load(
	"spec_conformity", ("diff_instructions", "criteria", "command_outcome_rule", "formatting_rule", "json_contract"),
)


def spec_conformity_prompt(
	criteria: list[str], review_base_sha: str | None, diff: tuple[str, str] | None = None, instructions_block: str = "",
) -> str:
	"""Builds the reviewer's prompt. diff, when given, is (stat, diff_text)
	from workspace_diff: the diff is inlined directly and the reviewer is
	told the diff is complete and not to re-fetch it, instead of being
	asked to `git diff`/`git show` itself -- found live (example-app run 3,
	2026-09-28): asked to diff the workspace itself, the reviewer explored
	with tools for 30 turns at ~33k input tokens each (1,011,032 total)
	until the relay's own sliding-window limiter answered "429 token
	budget exceeded" three times, and the review ended with no verdicts at
	all. diff is None when review_base_sha is unknown, or when the git
	read to build it failed (run_spec_conformity_review's own call site)
	-- the prompt then falls back to asking the reviewer to diff for
	itself, exactly as before this fix."""
	if diff is None:
		diff_instructions = CONFORMITY_DIFF_SELF.format(base=review_base_sha or "the commit this session started from")
		diff_instructions = diff_instructions.removesuffix("\n") + instructions_block
	else:
		stat, diff_text = diff
		diff_instructions = CONFORMITY_DIFF_INLINE.format(diff=format_diff_for_prompt(stat, diff_text, review_base_sha) + instructions_block)
	return CONFORMITY_PROMPT.format(
		diff_instructions=diff_instructions.removesuffix("\n"), criteria="\n".join(criteria),
		command_outcome_rule=CONFORMITY_COMMAND_OUTCOME_RULE, formatting_rule=CONFORMITY_FORMATTING_RULE,
		json_contract=CONFORMITY_JSON_CONTRACT,
	).removesuffix("\n")


_CRITERION_NUMBER_PREFIX = re.compile(r"^[ \t\r\n\f]*[0-9]+[.)][ \t\r\n\f]+")
_CRITERION_EDGE_SPACE = " \t\r\n\f"


def criterion_key(text: str) -> str:
	"""Comparison key for an echoed criterion, shared by every check that
	matches a model's echo of a criterion back to the declared one
	(parse_conformity_verdicts here, validate_manifest in
	draft_acceptance_oracles.py). The number prefix (ASCII digits then a
	mandatory "." or ")" and whitespace: a list marker only, so "1.2.3 is
	supported" keeps its "1.2"), backticks, ASCII edge whitespace and
	trailing ".;:," are the only things not part of its identity -- no
	substring, prefix, interior or fuzzy match. Mirrors criterionKey in
	cmd/factoryd/oracle_draft_job.go; both are pinned by
	cmd/factoryd/testdata/criterion_key_vectors.json. Found live: 2026-09-17,
	a reviewer dropped the leading "N. "; 2026-09-20, a model dropped only a
	final period; 2026-09-24 (codex-route bar run), a model echoed `"all"`
	as "all" -- each time an otherwise-valid response was discarded (a
	whole oracle draft, or a correct build quarantined on "unavailable")."""
	text = text.replace("`", "")
	text = _CRITERION_NUMBER_PREFIX.sub("", text).strip(_CRITERION_EDGE_SPACE)
	return text.rstrip(_CRITERION_EDGE_SPACE + ".;:,")


def _iter_json_objects(output: str):
	r"""Yields every top-level JSON value found in output, in the order
	their opening "{" appears: scan for each "{" and attempt a real parse
	(json.JSONDecoder.raw_decode) there rather than matching with a
	regex. A greedy r"\{.*\}" regex spans from the first "{" in the whole
	response to the last "}", swallowing any earlier JSON-shaped text
	(e.g. an echoed criterion quoting JSON) together with the real
	verdict block that follows -- found live 2026-09-22, see this
	function's own test for the incident. raw_decode finds every
	well-formed object regardless of what surrounds it, and a later
	verdict wins over an earlier one for the same criterion key via
	parse_conformity_verdicts' own left-to-right dict-overwrite order."""
	decoder = json.JSONDecoder()
	i, n = 0, len(output)
	while i < n:
		if output[i] != "{":
			i += 1
			continue
		try:
			obj, end = decoder.raw_decode(output, i)
		except json.JSONDecodeError:
			i += 1
			continue
		yield obj
		i = end


def parse_conformity_verdicts(output: str, criteria: list[str]) -> list[dict]:
	"""Extracts the reviewer's per-criterion verdict from its final
	response text. Returns exactly one {"criterion", "verdict", "detail"}
	entry per declared criterion, in criteria's own order -- a criterion
	the reviewer's own JSON didn't decisively cover (a missing or
	malformed response, or one shorter than the criteria list) defaults to
	"unavailable" rather than being silently dropped or counted clean,
	since an unaccounted-for criterion must never read as having passed.

	Matched by criterion_key on both sides (number prefix, backticks and
	trailing punctuation ignored), not by exact text -- found live
	(2026-09-17 multi-repo validation): the prompt asks the reviewer to
	echo each criterion "verbatim", but a real response routinely drops
	the leading number (e.g. declared as "1. `GET /health` responds
	with HTTP 200..." and echoed back as "`GET /health` responds with
	HTTP 200..."). An exact-string match against the numbered original
	then misses every one of those -- including ones the reviewer
	explicitly marked "clean" -- and this function's own "unaccounted
	criterion defaults to unavailable" rule then fails the build on a
	review that actually passed. Stripped-prefix matching is still a
	1:1 mapping (two criteria differing only in their number would
	collide, but read_acceptance_criteria's caller always numbers them
	uniquely) and preserves an exact match when the reviewer does echo
	the number.

	After key matching, a criterion still unmatched falls back to the
	verdict at its own position in the reviewer's entries list, but only
	when that fallback cannot itself misattribute a verdict: the reviewer
	must have returned exactly as many entries as declared criteria, and
	every criterion that DID match by key must have matched an entry
	sitting at that same index -- i.e. the reviewer answered in order and
	only dropped a leading clause or two, never reordered or skipped an
	entry. An entry already spent on a key match elsewhere never fills a
	second, positional slot. Found live (example-app walk, 2026-09-28): a
	reviewer answered all 6 criteria "clean", in order, but echoed
	criterion 3 without its leading clause ("For both invalid input
	categories, `HandleCreateNote` ..." echoed back starting at
	"`HandleCreateNote` ..." -- criterion_key's own number/backtick/
	punctuation stripping doesn't cover a dropped leading clause), so
	criterion 3 read as unavailable and failed a clean build."""
	# The LAST candidate with a "criteria" list wins wholesale -- entries
	# are not merged key-by-key across candidates. Found via review
	# (2026-09-22): merging would let a criterion entry from an earlier,
	# non-authoritative JSON blob (e.g. the model echoing the expected
	# answer schema as a worked example before its real analysis) survive
	# even when the real, later verdict block never mentions that
	# criterion -- a partial real analysis could inherit a stale "clean"
	# from the example for whatever it didn't cover, silently defeating
	# this function's own "unaccounted criterion defaults to unavailable"
	# invariant. Replacing wholesale means only the model's actual final
	# answer is ever trusted, exactly as intended.
	#
	# Considered and rejected: preferring whichever candidate matches the
	# most declared criteria, instead of simply the last one -- a second
	# review round (2026-09-22) noted "last wins" is itself vulnerable to
	# trailing JSON-shaped prose *after* a real answer overriding it. That
	# scenario is unconfirmed; "most matches wins" would confirmedly
	# regress the incident this fix was written for (an echoed example
	# that fully covers every criterion would then always beat a real but
	# partial analysis). Kept as "last wins" -- fixes the observed
	# failure without reintroducing it -- with the trailing-prose case
	# recorded as a follow-up, not chased further here.
	by_criterion: dict[str, dict] = {}
	winning_entries: list = []
	for candidate in _iter_json_objects(output):
		entries = candidate.get("criteria") if isinstance(candidate, dict) else None
		if not isinstance(entries, list):
			continue
		fresh: dict[str, dict] = {}
		for entry in entries:
			if isinstance(entry, dict) and entry.get("criterion"):
				fresh[criterion_key(str(entry["criterion"]))] = entry
		by_criterion = fresh
		winning_entries = entries

	def verdict_of(entry) -> dict | None:
		if isinstance(entry, dict) and entry.get("verdict") in ("clean", "flagged"):
			return {"verdict": entry["verdict"], "detail": str(entry.get("detail") or "")}
		return None

	# matched_position[criterion index] = index in winning_entries of the
	# entry that satisfied it by key -- used both to check the "answered
	# in order" precondition and to mark that entry as already spent.
	matched_position: dict[int, int] = {}
	used_positions: set[int] = set()
	verdicts: list[dict | None] = [None] * len(criteria)
	for i, criterion in enumerate(criteria):
		entry = by_criterion.get(criterion_key(criterion))
		resolved = verdict_of(entry)
		if resolved is None:
			continue
		verdicts[i] = resolved
		for position, candidate_entry in enumerate(winning_entries):
			if candidate_entry is entry:
				matched_position[i] = position
				used_positions.add(position)
				break

	positional_fallback_eligible = (
		len(winning_entries) == len(criteria)
		and all(position == index for index, position in matched_position.items())
	)
	if positional_fallback_eligible:
		for i in range(len(criteria)):
			if verdicts[i] is not None or i in used_positions:
				continue
			resolved = verdict_of(winning_entries[i])
			if resolved is not None:
				verdicts[i] = resolved

	return [
		{"criterion": criterion, **(verdicts[i] or {"verdict": "unavailable", "detail": "no-review-verdict"})}
		for i, criterion in enumerate(criteria)
	]


@dataclass(frozen=True)
class ReviewTurn:
	"""run_review_turn's own return shape: text is the last completed
	assistant turn's text (possibly ""), error is why the turn produced no
	usable text -- the last assistant turn's own errorMessage when it
	stopped with stopReason == "error" (e.g. a relay 429 on token-budget
	exhaustion), "timed out after N minutes" on a subprocess timeout, or
	the OSError's own text -- else "". A caller with text == "" and
	error == "" genuinely got no assistant turn at all (not a subprocess
	failure and not a model-reported error), which still reads as
	"unavailable" but with nothing further to say. Found live (example-app
	walk, 2026-09-28): a code review that burned the relay's whole
	sliding-window token budget got four 429s and ended with no final
	text; CODE_REVIEW_EVIDENCE.json's stopped_reason then just said "no
	parseable response from the reviewer", with the actual 429 nowhere in
	the evidence an operator could see."""
	text: str
	error: str


def run_review_turn(
	workspace: Path,
	*,
	prompt: str,
	session_dir: Path,
	review_base_sha: str | None,
	thinking: str | None,
	timeout_minutes: int = 10,
	adapter=DEFAULT_ADAPTER,
) -> ReviewTurn:
	"""Runs one bounded, standalone, non-continued model turn (shared by
	run_spec_conformity_review and code_review.py's own run_code_review)
	and returns a ReviewTurn (see its own doc comment) rather than a bare
	string, so a caller can say why the turn produced nothing instead of
	just that it did. Sets AI_REVIEW_BASE_SHA in the subprocess
	environment when review_base_sha is given, so a prompt referencing
	"the diff against $AI_REVIEW_BASE_SHA" resolves the same way for every
	one-turn review launch."""
	command = adapter.invocation(
		workspace, prompt=prompt, session_dir=session_dir,
		continue_session=False, thinking=thinking,
	)
	env = {**os.environ, "AI_STACK_HOST": os.environ.get("AI_STACK_HOST", "127.0.0.1")}
	if review_base_sha:
		env["AI_REVIEW_BASE_SHA"] = review_base_sha
	try:
		completed = sh(command, cwd=workspace, timeout=timeout_minutes * 60, env=env)
		output = completed.stdout if completed else ""
	except subprocess.TimeoutExpired:
		return ReviewTurn(text="", error=f"timed out after {timeout_minutes} minutes")
	except OSError as exc:
		return ReviewTurn(text="", error=str(exc))
	parsed = adapter.parse(output)
	return ReviewTurn(text=parsed.final_text, error=parsed.last_turn_error)


def run_spec_conformity_review(
	workspace: Path,
	*,
	criteria: list[str],
	review_base_sha: str | None,
	thinking: str | None,
	timeout_minutes: int = 10,
	adapter=DEFAULT_ADAPTER,
	instructions_diff: Path | None = None,
) -> tuple[list[dict], str]:
	"""Runs one bounded, standalone pi turn asking it to check the
	workspace's current diff against each declared acceptance criterion
	and return a per-criterion verdict -- separate from, and run after,
	the ordinary corrective-round loop's own whole-diff "reviewer"
	extension trace (review_signal), since that mechanism only ever
	produces one decisive outcome for the whole diff, never a verdict per
	criterion. A subprocess failure or timeout here (no output to parse)
	still returns one "unavailable" entry per criterion via
	parse_conformity_verdicts, never an empty list -- an unreachable
	reviewer must read as "criteria unaccounted for", not "no criteria
	declared".

	When review_base_sha is given, the workspace's diff against it is
	fetched via workspace_diff and inlined into the prompt (see
	spec_conformity_prompt's own doc comment) instead of asking the
	reviewer to fetch it itself -- found live (example-app run 3, 2026-09-28):
	without this, the reviewer burned the relay's whole hourly token
	budget across 30 tool-exploring turns just discovering the diff, and
	the review ended in three straight 429s with no verdicts at all. A
	failed diff fetch (workspace_diff returns None) falls back to the
	instruction-only prompt, same as when review_base_sha is unknown.

	Returns (verdicts, error): error is the underlying ReviewTurn's own
	error (see run_review_turn's ReviewTurn doc comment) -- "" when the
	turn produced usable text, else the model-reported error or subprocess
	failure that explains why every verdict came back "unavailable".
	Callers that only need the verdicts (e.g. this module's own run_build)
	can discard it; conformity_review.py's own run_conformity_review uses
	it to name the cause in CONFORMITY_EVIDENCE.json instead of leaving an
	operator with only "no-review-verdict" per criterion -- the actual
	429 was invisible in that evidence before this fix."""
	diff = workspace_diff(workspace, review_base_sha) if review_base_sha else None
	prompt = spec_conformity_prompt(criteria, review_base_sha, diff=diff, instructions_block=instructions_diff_block(instructions_diff))
	turn = run_review_turn(
		workspace, prompt=prompt, session_dir=workspace / ".pi-conformity-session",
		review_base_sha=review_base_sha, thinking=thinking,
		timeout_minutes=timeout_minutes, adapter=adapter,
	)
	return parse_conformity_verdicts(turn.text, criteria), turn.error


def parse_sonnet_usage(output: str) -> dict | None:
	try:
		payload = json.loads(output)
	except json.JSONDecodeError:
		return None
	usage = payload.get("usage")
	if not isinstance(usage, dict):
		return None
	return {**usage, "total_cost_usd": payload.get("total_cost_usd")}


def build_round_checklist(verify_command: str | None) -> str:
	"""The definition of done appended to the first round's prompt (later
	rounds continue the same session, so it stays in context). Always-on
	here rather than an operator skill: a skill loads only when the model
	chooses to read it, and a live A/B found the equivalent skill was never
	read. Every line restates a check the harness or factoryd already
	enforces, so following it saves rounds rather than adding rules. The first
	line asks only for focused tests: the harness always reruns the full command
	itself (agent output is untrusted), so a full run by the agent too
	duplicated it on every passing round (live A/B, 2026-10-01)."""
	verify = f"`{verify_command}`" if verify_command else "the ticket's `Verify-Command`"
	return ROUND_CHECKLIST.format(verify=verify).removesuffix("\n")


ROUND_STATE_FILE = ".pi-build-round-state.json"
ROUND_STATE_VERSION = 1


class RoundStateError(Exception):
	"""A --resume-from-state file that cannot be trusted to resume from."""


def workspace_head(workspace: Path) -> str | None:
	completed = _git_snapshot_command(["git", "rev-parse", "HEAD"], cwd=workspace, acceptable_codes={0})
	return completed.stdout.strip() if completed.returncode == 0 else None


def write_round_state(
	workspace: Path, *, last_completed_round: int, next_prompt: str, escalation_prompt: str, rounds: list[Round],
	fingerprint: tuple | None = None,
) -> None:
	"""Persist the round loop's resume point at the workspace root. Written
	atomically (temp file in the same directory, then os.replace) so a kill
	mid-write leaves the previous complete state, never a torn file."""
	state = {
		"version": ROUND_STATE_VERSION,
		"last_completed_round": last_completed_round,
		"next_prompt": next_prompt,
		"escalation_prompt": escalation_prompt,
		# Without each round's argv: it holds that round's whole prompt, a
		# resume never reads it back (round_from_state), and this file stays
		# in the workspace after the build, where a later review can read it.
		"rounds": [{**asdict(rnd), "command": []} for rnd in rounds],
		"head": workspace_head(workspace),
		"workspace_fingerprint": hashlib.sha256(repr(fingerprint).encode()).hexdigest() if fingerprint is not None else None,
	}
	target = workspace / ROUND_STATE_FILE
	# The temp file lives in .pi-build-session/, which every exclude list
	# already covers, so a kill between write and replace leaves nothing that
	# reaches the fingerprint, the safety-net commit or the diff-scope check.
	session = workspace / ".pi-build-session"
	session.mkdir(exist_ok=True)
	temp = session / "round-state.json.tmp"
	try:
		temp.write_text(json.dumps(state))
		os.replace(temp, target)
	except BaseException:
		temp.unlink(missing_ok=True)
		raise


ROUND_STATE_MAX_PROMPT_BYTES = 200_000
ROUND_STATE_MAX_TEXT = 20_000


def _typed(item: dict, key: str, kinds: tuple, default, *, cap: int | None = None):
	"""A round-record field, or `default` when absent. A present field of the
	wrong type refuses the whole file (bool is never accepted as an int)."""
	if key not in item:
		return default
	value = item[key]
	if isinstance(value, bool) and bool not in kinds:
		raise ValueError(f"{key} has the wrong type")
	if not isinstance(value, kinds):
		raise ValueError(f"{key} has the wrong type")
	if cap is not None and isinstance(value, str):
		value = value[:cap]
	return value


def round_from_state(item: dict, position: int) -> Round:
	"""Rebuild a Round from an explicit typed subset of its recorded fields.
	Command and traces are not restored (the reports and evidence never need
	an earlier round's raw argv or trace events, and a forged trace would
	otherwise become a review verdict); every other field is type-checked."""
	if not isinstance(item, dict):
		raise ValueError("round record is not an object")
	if _typed(item, "index", (int,), position) != position:
		raise ValueError(f"round record {position} has index {item['index']!r}")
	reviewer = _typed(item, "reviewer", (dict,), {})
	turn_errors = _typed(item, "turn_errors", (list,), [0, 0])
	if len(turn_errors) != 2 or any(isinstance(n, bool) or not isinstance(n, int) for n in turn_errors):
		raise ValueError("turn_errors is not two integers")
	text = ROUND_STATE_MAX_TEXT
	return Round(
		index=position,
		agent=_typed(item, "agent", (str,), "", cap=200),
		command=[],
		agent_returncode=_typed(item, "agent_returncode", (int,), 0),
		agent_timed_out=_typed(item, "agent_timed_out", (bool,), False),
		usage=_typed(item, "usage", (dict, type(None)), None),
		traces=[],
		turn_errors=(turn_errors[0], turn_errors[1]),
		reviewer=ReviewSignal(
			outcome=_typed(reviewer, "outcome", (str,), "unavailable", cap=200),
			detail=_typed(reviewer, "detail", (str,), "", cap=text),
		),
		verify_command=_typed(item, "verify_command", (str, type(None)), None, cap=text),
		verify_passed=_typed(item, "verify_passed", (bool, type(None)), None),
		verify_timed_out=_typed(item, "verify_timed_out", (bool,), False),
		verify_output_tail=_typed(item, "verify_output_tail", (str,), "", cap=text),
		duration_s=float(_typed(item, "duration_s", (int, float), 0.0)),
		fast_check_ran=_typed(item, "fast_check_ran", (bool,), False),
		fast_check_passed=_typed(item, "fast_check_passed", (bool, type(None)), None),
		oracle_command=_typed(item, "oracle_command", (str, type(None)), None, cap=text),
		oracle_passed=_typed(item, "oracle_passed", (bool, type(None)), None),
		oracle_output_tail=_typed(item, "oracle_output_tail", (str,), "", cap=text),
		blockers=_typed_strings(item, "blockers"),
		changed_files=_typed_strings(item, "changed_files"),
		failure_signature=_typed(item, "failure_signature", (str,), "", cap=64),
		failure_log=_typed(item, "failure_log", (str,), "", cap=400),
		agent_notes=_typed(item, "agent_notes", (str,), "", cap=text),
		autofix=_typed(item, "autofix", (dict, type(None)), None),
	)


def _typed_strings(item: dict, key: str, *, max_items: int = 200, cap: int = 400) -> list[str]:
	"""A round-record field that is a list of strings, or [] when absent.
	Anything else refuses the whole file, as _typed does."""
	value = _typed(item, key, (list,), [])
	if any(not isinstance(entry, str) for entry in value):
		raise ValueError(f"{key} is not a list of strings")
	return [entry[:cap] for entry in value[:max_items]]


def load_round_state(path: Path, max_rounds: int) -> dict:
	"""Read and validate a round-state file. Control flow uses only the round
	number, next_prompt (size-capped) and the type-checked earlier round
	records; the stored escalation_prompt, head and fingerprint are never
	read back."""
	try:
		state = json.loads(path.read_text())
	except (OSError, ValueError) as exc:
		raise RoundStateError(f"cannot read round state {path}: {exc}") from exc
	if not isinstance(state, dict) or state.get("version") != ROUND_STATE_VERSION:
		raise RoundStateError(f"round state {path}: unsupported version (want {ROUND_STATE_VERSION})")
	last = state.get("last_completed_round")
	if isinstance(last, bool) or not isinstance(last, int) or not 0 <= last < max_rounds:
		raise RoundStateError(f"round state {path}: last_completed_round {last!r} is not an integer in [0, {max_rounds})")
	next_prompt = state.get("next_prompt")
	if not isinstance(next_prompt, str) or not next_prompt:
		raise RoundStateError(f"round state {path}: next_prompt is missing")
	if len(next_prompt.encode()) > ROUND_STATE_MAX_PROMPT_BYTES:
		raise RoundStateError(f"round state {path}: next_prompt exceeds {ROUND_STATE_MAX_PROMPT_BYTES} bytes")
	records = state.get("rounds", [])
	if not isinstance(records, list):
		raise RoundStateError(f"round state {path}: rounds is not a list")
	try:
		rounds = [round_from_state(item, position) for position, item in enumerate(records, start=1)]
	except (TypeError, ValueError, AttributeError) as exc:
		raise RoundStateError(f"round state {path}: malformed rounds: {exc}") from exc
	if len(rounds) != last:
		raise RoundStateError(f"round state {path}: {len(rounds)} round records for last_completed_round {last}")
	return {"last_completed_round": last, "next_prompt": next_prompt, "rounds": rounds}


def build_escalation_prompt(spec_text: str, corrective: str) -> str:
	return ESCALATION_PROMPT.format(spec_text=spec_text, corrective=corrective).removesuffix("\n")


# A third round in a row with the same failure signature ends the loop: a
# fourth would spend its budget on a change that has already not worked
# twice since the diagnose-first prompt.
STUCK_STOP_STREAK = 3
_FEEDBACK_HASH_MAX_BYTES = 5_000_000


class ScopeListingError(Exception):
	"""A git listing of the changed paths failed, timed out or held a name that
	is not valid UTF-8, so the set it gave would be short."""


def _changed_names(workspace: Path, base_sha: str | None, strict: bool) -> set[str]:
	names: set[str] = set()
	for args in (
		["git", "diff", "--name-only", "-z", base_sha or "HEAD", "--", *_HARNESS_ARTIFACT_PATHSPECS],
		["git", "ls-files", "--others", "--exclude-standard", "-z", "--", *_HARNESS_ARTIFACT_PATHSPECS],
	):
		try:
			done = sh(args, cwd=workspace, timeout=60)
		except (subprocess.TimeoutExpired, OSError, ValueError) as exc:
			# ValueError: a file name git printed that is not valid UTF-8.
			if strict:
				raise ScopeListingError(f"{args[1]}: {exc!r}") from exc
			continue
		if done.returncode == 0:
			names.update(name for name in done.stdout.split("\0") if name)
		elif strict:
			raise ScopeListingError(f"{args[1]} exited {done.returncode}")
	return names


def changed_file_hashes(workspace: Path, base_sha: str | None) -> dict[str, str]:
	"""A content hash per file that differs from base_sha (HEAD when
	unknown) or is untracked, harness artifacts excluded. Two of these, taken
	before and after an agent turn, say which files that turn changed
	(round_changed_files): the "what was already tried" half of what the next
	round is told. Best effort: a git failure gives {}."""
	names = _changed_names(workspace, base_sha, strict=False)
	hashes: dict[str, str] = {}
	for name in names:
		path = workspace / name
		try:
			if path.is_symlink():
				hashes[name] = "link:" + os.readlink(path)
			elif not path.is_file():
				hashes[name] = "absent"
			elif path.stat().st_size > _FEEDBACK_HASH_MAX_BYTES:
				hashes[name] = f"size:{path.stat().st_size}"
			else:
				hashes[name] = hashlib.sha256(path.read_bytes()).hexdigest()
		except OSError:
			hashes[name] = "unreadable"
	return hashes


def round_changed_files(before: dict[str, str], after: dict[str, str]) -> list[str]:
	"""The files whose state differs between two changed_file_hashes."""
	return sorted(name for name in before.keys() | after.keys() if before.get(name) != after.get(name))


def round_failure_log(log_dir: Path, workspace: Path, setup_failed: bool = False) -> str:
	"""The workspace-relative path of the log this round's failing command
	left in log_dir, in the order the checks run, or "" when none did.
	setup.log is a failing command's log only when setup_failed: a passing
	setup leaves one too."""
	for name in (SETUP_LOG, FAST_CHECK_LOG, VERIFY_LOG, ORACLE_LOG):
		if (log_dir / name).is_file() and not (name == SETUP_LOG and not setup_failed):
			return (log_dir / name).relative_to(workspace).as_posix()
	return ""


def missing_log_note(workspace: Path, rounds: list[Round]) -> str:
	"""What to add to a recorded corrective prompt when the log file it
	names is gone: a resumed build starts without the earlier attempt's
	session folder, which held it. "" when the file is there or none was
	named."""
	log = rounds[-1].failure_log if rounds else ""
	if not log or (workspace / log).is_file():
		return ""
	return f"\n\nThe saved output `{log}` named above no longer exists (this build was resumed). Rerun the failing command to see its output."


def round_history(rounds: list[Round]) -> str:
	return round_feedback.history_lines([
		{"index": r.index, "changed_files": r.changed_files, "blockers": r.blockers, "signature": r.failure_signature}
		for r in rounds
	])


def notes_turn_skip_reason(
	*, result: "BuildResult", last_turn: dict | None, sonnet_fallback: bool, criteria_given: bool,
	adapter, session_dir: Path, elapsed: float,
) -> str:
	"""Why the notes turn does not run, or "" when it does. It runs only for a
	build that never passed, whose last agent turn finished cleanly in a
	session that can be continued, with room left in the launch's time budget."""
	if result.succeeded:
		return "build passed"
	if last_turn is None:
		return "no round ran in this process"
	if sonnet_fallback:
		return "sonnet fallback enabled"
	if criteria_given:
		return "spec acceptance criteria given"
	if last_turn["timed_out"]:
		return "last turn timed out"
	if last_turn["returncode"] != 0:
		return "last turn exited non-zero"
	if last_turn["errored"]:
		return "last turn errored"
	if last_turn["all_errored"] or result.stopped_reason.startswith("model route unreachable"):
		return "model route unreachable"
	if not adapter.can_continue_session(session_dir):
		return "session cannot be continued"
	try:
		budget = int(os.environ.get(BUILD_TIME_BUDGET_ENV, ""))
	except ValueError:
		return "no build time budget"
	if budget - elapsed < HANDOFF_NOTES_MIN_REMAINING_S:
		return f"under {HANDOFF_NOTES_MIN_REMAINING_S} s of the build time budget left"
	return ""


def cut_utf8(text: str, limit: int) -> str:
	"""text cut to at most limit bytes of UTF-8, on a character boundary."""
	return text.encode("utf-8", errors="replace")[:limit].decode("utf-8", errors="ignore")


def run_notes_turn(workspace: Path, session_dir: Path, adapter, env: dict, thinking: str | None) -> dict:
	"""Asks the build agent, once, in its own session, for notes. Returns the
	record for BUILD_EVIDENCE.json (no text from the reply). The reply goes,
	redacted and cut, to session_dir/HANDOFF_NOTES_FILE and nowhere else: not
	printed, not in the progress feed (the event callback is a no-op), not on
	a round. The notes are an aid: whatever goes wrong inside the turn is
	recorded by its exception class name alone (a message may hold the
	reply), the partial file is removed, and the build goes on."""
	record: dict = {"ran": True, "skipped_reason": "", "duration_s": 0.0}
	started = _monotonic()
	notes_path = session_dir / HANDOFF_NOTES_FILE
	try:
		return _notes_turn(workspace, notes_path, session_dir, adapter, env, thinking, record, started)
	except Exception as exc:
		try:
			notes_path.unlink(missing_ok=True)
		except OSError:
			pass
		record.update(ran=False, skipped_reason=f"notes turn failed: {type(exc).__name__}", duration_s=_monotonic() - started)
		record.pop("usage", None)
		return record


def _notes_turn(workspace: Path, notes_path: Path, session_dir: Path, adapter, env: dict, thinking: str | None, record: dict, started: float) -> dict:
	command = adapter.invocation(
		workspace, prompt=HANDOFF_NOTES_PROMPT, session_dir=session_dir,
		continue_session=True, thinking=thinking, load_repo_skills=False,
	)
	fingerprint_before = workspace_fingerprint(workspace)
	completed, timed_out = run_agent_streaming(
		command, cwd=workspace, timeout=HANDOFF_NOTES_TIMEOUT_S, env=env, on_event=lambda line: None,
	)
	fingerprint_after = workspace_fingerprint(workspace)
	record["duration_s"] = _monotonic() - started
	parsed = adapter.parse(completed.stdout if completed else "")
	record["usage"] = parsed.usage
	if fingerprint_before != fingerprint_after:
		record["changed_files_during_notes"] = True
	text = parsed.final_text.strip()
	if timed_out or completed is None or completed.returncode != 0 or parsed.last_turn_error or not text:
		return record
	notes_path.unlink(missing_ok=True)
	body = cut_utf8(redact(text, 1_000_000), HANDOFF_NOTES_MAX_BYTES)
	with open(notes_path, "w", encoding="utf-8", errors="replace") as handle:
		handle.write(body + "\n")
	return record


def run_build(
	workspace: Path,
	spec_path: Path,
	*,
	max_rounds: int,
	timeout_minutes: int,
	thinking: str | None = None,
	review_policy: str = "required",
	conformity_policy: str = "required",
	sonnet_fallback: bool = False,
	review_base_sha: str | None = None,
	verify_command_override: str | None = None,
	fast_check_command: str | None = None,
	setup_commands: list[str] | None = None,
	autofix_commands: list[str] | None = None,
	spec_acceptance_criteria: Path | None = None,
	oracle_command: str | None = None,
	handoff: Path | None = None,
	resume_from_state: Path | None = None,
	earlier_attempt: Path | None = None,
	baseline_failure: Path | None = None,
	adapter=DEFAULT_ADAPTER,
	started_monotonic: float | None = None,
) -> BuildResult:
	if started_monotonic is None:
		started_monotonic = _process_started if _process_started is not None else _monotonic()
	resume = load_round_state(resume_from_state, max_rounds) if resume_from_state is not None else None
	workspace.mkdir(parents=True, exist_ok=True)
	ensure_git_repo(workspace)
	session_dir = workspace / ".pi-build-session"
	spec_text = spec_path.read_text()
	# A file from an earlier run of this script is not this build's notes.
	try:
		(session_dir / HANDOFF_NOTES_FILE).unlink(missing_ok=True)
	except OSError:
		pass
	last_turn: dict | None = None

	agents_md_git_blob = committed_agents_md_blob(workspace)
	result = BuildResult(
		workspace=workspace, spec_path=spec_path, review_policy=review_policy,
		conformity_policy=conformity_policy,
		agents_md_used=agents_md_git_blob is not None, agents_md_git_blob=agents_md_git_blob,
	)
	env = {**os.environ, "AI_STACK_HOST": os.environ.get("AI_STACK_HOST", "127.0.0.1")}
	# Anchors the independent reviewer's diff scope to the ticket's true
	# starting commit (the caller's job to know -- ticket_runner.py passes
	# its own prior-ticket-boundary sha) instead of cross-model-review.ts's
	# own default of "HEAD when this OS process happened to start". Without
	# this, a build_app.py invocation retried against a ticket a prior,
	# interrupted process already finished and committed sees an empty diff
	# (nothing changed since *this* process's start) and can never get a
	# decisive review verdict for work that was, in fact, never reviewed by
	# anyone -- see PR #25 review discussion. Omitted for standalone
	# build_app.py usage with no known ticket boundary; the reviewer falls
	# back to its own HEAD-at-process-start default.
	if review_base_sha:
		env["AI_REVIEW_BASE_SHA"] = review_base_sha
	prompt = f"{spec_text}\n\n---\n\n{build_round_checklist(verify_command_override or resolve_verify_command(workspace))}"
	services_sentence = compose_services_sentence(dict(os.environ))
	if services_sentence:
		# Printed too, so the build log shows what the worker was told.
		print(f"compose services: {services_sentence}", file=sys.stderr)
		prompt = f"{services_sentence}\n\n---\n\n{prompt}"
	# The record of an earlier attempt opens the task as this build sees it,
	# so it is in base_prompt. It is never written to the round-state file
	# (stored(), below): that file outlives the build in the workspace, and
	# the record is for the build alone. A resume is given --earlier-attempt
	# again and puts it back.
	# The baseline note (--baseline-failure) is handled the same way, ahead
	# of the record: it describes the state the earlier attempt started from
	# too.
	record_block = ""
	if baseline_failure is not None:
		record_block += f"{baseline_failure_preamble(baseline_failure.read_text())}\n\n---\n\n"
	if earlier_attempt is not None:
		record_block += f"{earlier_attempt_preamble(earlier_attempt.read_text())}\n\n---\n\n"
	prompt = record_block + prompt
	base_prompt = prompt

	def stored(text: str) -> str:
		return text.replace(record_block, "", 1) if record_block else text

	if handoff is not None:
		# Round 1 only: later rounds' prompts are corrective_prompt()s that
		# continue the session this round-1 prompt opened.
		prompt = f"{handoff_preamble(handoff.read_text())}\n\n---\n\n{prompt}"
	escalation_prompt = ""
	first_round = 1
	if resume is not None:
		first_round = resume["last_completed_round"] + 1
		result.rounds.extend(resume["rounds"])
		prompt = resume["next_prompt"]
		if resume["last_completed_round"] == 0:
			# The recorded round-1 prompt already holds the spec (and the
			# handoff preamble, when the earlier attempt had one).
			if handoff is not None:
				preamble = handoff_preamble(handoff.read_text())
				if not prompt.startswith(preamble):
					prompt = f"{preamble}\n\n---\n\n{prompt}"
			if record_block and record_block not in prompt:
				prompt = record_block + prompt
		else:
			# The recorded prompt is a corrective one, written for a session
			# that already held the spec; the fresh session needs the task
			# first. The escalation prompt is rebuilt, never read from the file.
			escalation_prompt = build_escalation_prompt(spec_text, prompt)
			prompt = prompt + missing_log_note(workspace, result.rounds)
			prompt = "\n\n".join([
				base_prompt,
				"---",
				"Earlier rounds of this build already changed the workspace in a session that is no longer available. Inspect the workspace, then address this feedback from the last round:",
				prompt,
			])
	# What the state file records as the next prompt: never the composite
	# resume prompt above, which would embed the spec twice on a later resume.
	state_prompt = resume["next_prompt"] if resume is not None else prompt

	def persist(last_completed_round: int, next_prompt: str, fingerprint: tuple | None = None) -> None:
		write_round_state(
			workspace, last_completed_round=last_completed_round, next_prompt=stored(next_prompt),
			escalation_prompt=stored(escalation_prompt), rounds=result.rounds, fingerprint=fingerprint,
		)

	if resume is None:
		persist(0, prompt)

	# The repository's setup commands run once before the first agent turn, so
	# the agent works in a prepared tree; the same helper (timeout, captured
	# output) reruns them before each round's checks. A failure ends the build
	# here, without a model call, as a build that did not pass.
	started_setup_failed, _ = run_setup(workspace, setup_commands or [], session_dir / "feedback" / "setup")
	if started_setup_failed is not None:
		result.stopped_reason = SETUP_FAILED_BLOCKER + started_setup_failed[:200]
		return result

	for round_index in range(first_round, max_rounds + 1):
		continue_session = round_index > first_round
		command = adapter.invocation(
			workspace, prompt=prompt, session_dir=session_dir,
			continue_session=continue_session,
			thinking=thinking,
			load_repo_skills=True,
		)
		print_progress({"stage": "round", "event": "start", "round": round_index, "max_rounds": max_rounds})
		# Captured immediately before the round's own agent invocation --
		# not the ticket's overall starting commit -- so the no-changes
		# check below only ever credits (or blames) *this* round. See
		# workspace_fingerprint's own docstring for why comparing against
		# the ticket boundary instead was wrong.
		fingerprint_before = workspace_fingerprint(workspace)
		hashes_before = changed_file_hashes(workspace, review_base_sha)
		feedback_dir = session_dir / "feedback" / f"round-{round_index}"
		# An earlier invocation's log for this round number must not be
		# reported as this round's.
		shutil.rmtree(feedback_dir, ignore_errors=True)
		started = time.monotonic()
		completed, timed_out = run_agent_streaming(
			command, cwd=workspace, timeout=timeout_minutes * 60, env=env,
			on_event=lambda line: _emit_agent_progress(adapter, round_index, line),
		)
		duration = time.monotonic() - started
		stdout = completed.stdout if completed else ""
		# Snapshotted here, before run_verification() below -- verify/build
		# steps can leave untracked build artifacts in their wake (compiled
		# binaries, __pycache__, etc.) that would otherwise read as "the
		# round changed something" even when the agent itself touched
		# nothing.
		fingerprint_after = workspace_fingerprint(workspace)
		hashes_after = changed_file_hashes(workspace, review_base_sha)
		changed_files = round_changed_files(hashes_before, hashes_after)
		# Taken here, before verification and gofmt below leave their own untracked
		# artifacts, so only the agent's (or an interrupted attempt's) work counts.
		differs_from_base = bool(handoff is not None and round_index == 1 and review_base_sha and workspace_differs_from_base(workspace, review_base_sha))

		# Autofix runs after the agent-change snapshots above (its edits are
		# never the agent's work) and before this round's checks, so they
		# judge the fixed tree.
		autofix_record = run_autofix(workspace, autofix_commands or [], review_base_sha, feedback_dir, env)

		setup_failed, (verify_command, verify_passed, verify_timed_out, verify_tail, fast_check_ran, fast_check_passed) = run_verification_after_setup(
			workspace, setup_commands=setup_commands or [], verify_command_override=verify_command_override,
			fast_check_command=fast_check_command, log_dir=feedback_dir,
		)
		if verify_passed is True:
			reformatted = gofmt_changed_files(workspace, review_base_sha)
			if reformatted:
				print(f"gofmt reformatted {len(reformatted)} changed file(s): {', '.join(reformatted)}", file=sys.stderr)
		recorded_oracle_command, oracle_passed, oracle_tail = maybe_run_reference_oracle(
			workspace, oracle_command=oracle_command, verify_passed=verify_passed, log_dir=feedback_dir,
		)

		agent_returncode = completed.returncode if completed else -1
		# A nonzero pi exit means the CLI itself crashed, was invoked wrong, or
		# otherwise didn't complete a real agent turn -- verify_passed alone
		# can't be trusted as evidence of *this round's* work in that case,
		# since it just reruns whatever verification command already exists in
		# the workspace and would happily report "passed" against a tree pi
		# never touched. Success requires both: pi actually ran to completion
		# (returncode 0) and the real verification command passed.
		pi_failed = timed_out or agent_returncode != 0
		parsed = adapter.parse(stdout)
		traces = parsed.traces
		turn_errors = parsed.turn_errors
		last_turn = {
			"timed_out": timed_out, "returncode": agent_returncode, "errored": bool(parsed.last_turn_error),
			"all_errored": bool(turn_errors[1] and turn_errors[0] == turn_errors[1]),
		}
		if turn_errors[0]:
			error_messages = parsed.route_errors
			if error_messages:
				print(f"Model route error: {redact('; '.join(error_messages), 1000)}", file=sys.stderr)
		# None on either side means the fingerprint itself couldn't be
		# trusted (a git hiccup) -- treated as "no confirmed change" rather
		# than guessing either way, which only ever costs an extra round,
		# never a false failure.
		no_changes = fingerprint_before is None or fingerprint_after is None or fingerprint_before == fingerprint_after
		if no_changes and differs_from_base:
			# A resumed build whose interrupted attempt already did the work:
			# round 1 legitimately changes nothing more, and verify passing
			# on that work is evidence of the work, not of an untouched tree.
			no_changes = False
		blockers, reviewer = round_blockers(
			verify_passed=verify_passed,
			pi_failed=pi_failed,
			pi_timed_out=timed_out,
			traces=traces,
			review_policy=review_policy,
			turn_errors=turn_errors,
			no_changes=no_changes,
			fast_check_ran=fast_check_ran,
			fast_check_passed=fast_check_passed,
			oracle_passed=oracle_passed,
			setup_failed=setup_failed,
			autofix_failed=autofix_blocker(autofix_record),
		)
		if timed_out:
			round_end_detail = "timed out"
		elif no_changes:
			round_end_detail = "no changes made"
		elif blockers and verify_passed is not True and verify_command:
			round_end_detail = f"verify failed: {verify_command}"
		elif blockers:
			round_end_detail = blockers[0]
		else:
			round_end_detail = ""
		print_progress({
			"stage": "round", "event": "end", "round": round_index, "max_rounds": max_rounds,
			"outcome": "fail" if blockers else "pass", "detail": round_end_detail[:500],
		})

		rnd = Round(
			index=round_index,
			agent=adapter.name,
			command=command,
			agent_returncode=agent_returncode,
			agent_timed_out=timed_out,
			usage=parsed.usage,
			traces=traces,
			turn_errors=turn_errors,
			reviewer=reviewer,
			verify_command=verify_command,
			verify_passed=verify_passed,
			verify_timed_out=verify_timed_out,
			verify_output_tail=verify_tail,
			duration_s=duration,
			fast_check_ran=fast_check_ran,
			fast_check_passed=fast_check_passed,
			oracle_command=recorded_oracle_command,
			oracle_passed=oracle_passed,
			oracle_output_tail=oracle_tail,
			blockers=list(blockers),
			changed_files=changed_files,
			failure_signature=round_feedback.failure_signature(
				blockers, "\n".join(part for part in (verify_tail if verify_passed is not True else "", oracle_tail if oracle_passed is False else "") if part),
				reviewer.detail if reviewer.outcome == "flagged" else "",
			),
			failure_log=round_failure_log(feedback_dir, workspace, setup_failed is not None) if blockers else "",
			autofix=autofix_record,
			agent_notes=round_feedback.agent_notes(
				no_changes=no_changes, timed_out=timed_out, timeout_minutes=timeout_minutes,
				returncode=agent_returncode, stderr_tail=redact(parsed.last_turn_error, 1500),
				final_text=redact(parsed.final_text, 1500), route_errors=[redact(e, 400) for e in parsed.route_errors],
				stalled="stall-timeout" in blockers,
			) if blockers else "",
		)
		result.rounds.append(rnd)
		streak = round_feedback.same_failure_streak([r.failure_signature for r in result.rounds])

		errored, total = turn_errors
		if total and errored == total:
			# The model route was unreachable for every assistant turn this
			# round -- no code was ever produced for the agent to act on, so
			# further rounds against the same dead route would just repeat
			# this outcome and burn the rest of the round budget for nothing
			# (observed live: budget-pilot ticket 005 burned all 3 rounds this
			# way before the outage was noticed). Stop immediately instead of
			# looping to max_rounds; ticket_runner.py's own build-attempt
			# retry is the layer that should recover once the route is back
			# -- checked (and left with no escalation_prompt) before the
			# verify_command-is-None branch below, since a full route
			# outage produces that exact symptom (nothing got a chance to
			# create anything) and must not be spent as a billed Sonnet
			# pass that can only repeat the same outage (found via Codex
			# review of PR #5).
			result.stopped_reason = f"model route unreachable ({errored}/{total} assistant turns errored)"
			persist(round_index, state_prompt, fingerprint_after)
			break
		# Deliberately no special early-break for verify_command is None
		# (a brand-new ticket 1 with nothing created yet, e.g.): unlike the
		# route-outage case just above, this is not unrecoverable, and this
		# module's own documented contract is that --sonnet-fallback only
		# runs "after the local corrective-round budget is exhausted" --
		# an unconditional break here after round 1 would spend a billed
		# Sonnet pass before that budget was used, contradicting it (found
		# via Codex review of PR #5). round_blockers already adds
		# "canonical verification failed" whenever verify_passed is not
		# True (None here, same as any other unresolvable/failing check),
		# so this falls through to the same corrective-prompt/max_rounds
		# flow as every other verify failure, and a Sonnet pass still gets
		# escalation_prompt (built fresh every round below) once that
		# budget really is exhausted.
		if not blockers:
			result.succeeded = True
			if reviewer.outcome == "clean":
				result.stopped_reason = "canonical verification passed and independent review was clean"
			elif review_policy == "advisory":
				result.stopped_reason = f"canonical verification passed; advisory review ({reviewer.outcome}: {reviewer.detail or 'no detail'})"
			else:
				result.stopped_reason = f"canonical verification passed; degraded review ({reviewer.detail})"
			persist(round_index, state_prompt, fingerprint_after)
			break

		prompt = corrective_prompt(
			round_index=round_index,
			max_rounds=max_rounds,
			verify_command=verify_command,
			verify_tail=verify_tail,
			blockers=blockers,
			reviewer=reviewer,
			review_policy=review_policy,
			oracle_command=oracle_command,
			oracle_tail=oracle_tail,
			history=round_history(result.rounds),
			failure_log=rnd.failure_log,
			failing=round_feedback.failing_names("\n".join((verify_tail, oracle_tail))) if rnd.failure_log else None,
			agent_notes=rnd.agent_notes,
			streak=streak,
		)
		if autofix_prompt_note(autofix_record):
			prompt += "\n\n" + autofix_prompt_note(autofix_record)
		escalation_prompt = build_escalation_prompt(spec_text, prompt)
		state_prompt = prompt
		persist(round_index, prompt, fingerprint_after)
		if review_policy == "required" and reviewer.outcome == "unavailable" and reviewer.detail in NON_RETRYABLE_REVIEW_FAILURES:
			result.stopped_reason = f"review unavailable; escalation required: {reviewer.detail}"
			break
		if round_index == max_rounds:
			result.stopped_reason = f"local round budget ({max_rounds}) exhausted; escalation required: {', '.join(blockers)}"
			break
		if streak >= STUCK_STOP_STREAK:
			result.stopped_reason = f"no progress: the same failure {streak} rounds in a row; escalation required: {', '.join(blockers)}"
			break

	try:
		skipped = notes_turn_skip_reason(
			result=result, last_turn=last_turn, sonnet_fallback=sonnet_fallback,
			criteria_given=spec_acceptance_criteria is not None, adapter=adapter, session_dir=session_dir,
			elapsed=_monotonic() - started_monotonic,
		)
	except Exception as exc:
		skipped = f"notes turn failed: {type(exc).__name__}"
	if skipped:
		result.notes_turn = {"ran": False, "skipped_reason": skipped, "duration_s": 0.0}
	else:
		result.notes_turn = run_notes_turn(workspace, session_dir, adapter, env, thinking)

	# Deliberately does not also require resolve_verify_command(workspace)
	# to already succeed here: that would make this unreachable in exactly
	# the "nothing exists yet" case above, where creating the verify
	# surface is itself part of what the Sonnet pass is being asked to do.
	# run_verification() a few lines down re-resolves fresh against
	# whatever this round actually produces, so a still-unresolvable
	# command after the Sonnet pass simply fails verify_passed normally.
	if not result.succeeded and sonnet_fallback and escalation_prompt:
		command = sonnet_invocation(escalation_prompt)
		# Same round-scoped comparison as the local rounds above: baseline
		# is the workspace as the sonnet round finds it (i.e. after
		# whatever the local rounds already did or didn't do), not the
		# ticket's overall starting commit.
		fingerprint_before = workspace_fingerprint(workspace)
		started = time.monotonic()
		try:
			completed = sh(command, cwd=workspace, timeout=timeout_minutes * 60, env=env)
			timed_out = False
		except subprocess.TimeoutExpired:
			completed = None
			timed_out = True
		except OSError as exc:
			completed = subprocess.CompletedProcess(command, 127, "", str(exc))
			timed_out = False
		duration = time.monotonic() - started
		fingerprint_after = workspace_fingerprint(workspace)
		sonnet_autofix = None
		if autofix_commands:
			sonnet_autofix = run_autofix(
				workspace, autofix_commands, review_base_sha, session_dir / "feedback" / f"round-{len(result.rounds) + 1}", env,
			)
		sonnet_setup_failed, (verify_command, verify_passed, verify_timed_out, verify_tail, fast_check_ran, fast_check_passed) = run_verification_after_setup(
			workspace, setup_commands=setup_commands or [], verify_command_override=verify_command_override,
			fast_check_command=fast_check_command,
		)
		if verify_passed is True:
			gofmt_changed_files(workspace, review_base_sha)
		# Same gating (maybe_run_reference_oracle) as the local round loop
		# above -- a billed Sonnet pass must clear the oracle too, not
		# just the local rounds it's here to rescue.
		sonnet_recorded_oracle_command, sonnet_oracle_passed, sonnet_oracle_tail = maybe_run_reference_oracle(
			workspace, oracle_command=oracle_command, verify_passed=verify_passed,
		)
		returncode = completed.returncode if completed else -1
		result.rounds.append(Round(
			index=len(result.rounds) + 1,
			agent=SONNET_MODEL,
			command=command,
			agent_returncode=returncode,
			agent_timed_out=timed_out,
			usage=parse_sonnet_usage(completed.stdout if completed else ""),
			traces=[],
			reviewer=ReviewSignal("sonnet-fallback"),
			verify_command=verify_command,
			verify_passed=verify_passed,
			verify_timed_out=verify_timed_out,
			verify_output_tail=verify_tail,
			duration_s=duration,
			fast_check_ran=fast_check_ran,
			fast_check_passed=fast_check_passed,
			oracle_command=sonnet_recorded_oracle_command,
			oracle_passed=sonnet_oracle_passed,
			oracle_output_tail=sonnet_oracle_tail,
			autofix=sonnet_autofix,
		))
		sonnet_no_changes = (
			fingerprint_before is None or fingerprint_after is None or fingerprint_before == fingerprint_after
		)
		if not timed_out and returncode == 0 and verify_passed is True and sonnet_oracle_passed is not False and not sonnet_no_changes and not autofix_blocker(sonnet_autofix):
			result.succeeded = True
			result.stopped_reason = "Sonnet fallback passed canonical verification"
		elif autofix_blocker(sonnet_autofix):
			result.stopped_reason = f"Sonnet fallback: {autofix_blocker(sonnet_autofix)}"
		elif sonnet_setup_failed is not None:
			result.stopped_reason = f"Sonnet fallback's {SETUP_FAILED_BLOCKER}{sonnet_setup_failed[:200]}"
		elif sonnet_no_changes:
			result.stopped_reason = "Sonnet fallback made no changes to the workspace"
		elif fast_check_ran and fast_check_passed is False:
			# Same attribution fix as round_blockers above: canonical
			# verification was never attempted this pass, so don't claim
			# it failed.
			result.stopped_reason = "Sonnet fallback's fast check failed"
		elif sonnet_oracle_passed is False:
			result.stopped_reason = "Sonnet fallback did not pass the reference-oracle check"
		else:
			result.stopped_reason = "Sonnet fallback did not pass canonical verification"

	# Per-criterion spec-conformity review: only meaningful once
	# canonical verification itself already succeeded above -- checking
	# conformity of a build that never even passed verification would
	# just repeat "canonical verification failed" through a different,
	# more expensive mechanism. Runs exactly once, against the workspace's
	# final state, not per round: unlike the whole-diff "reviewer"
	# extension (which re-runs every round to catch regressions early),
	# per-criterion conformity is a final acceptance check.
	#
	# A criterion checking whether the diff is committed (e.g. a commit
	# subject-line convention) can only ever be evaluated meaningfully
	# against an ALREADY-committed workspace -- this function's own round
	# loop relies on the agent's own `git add -A`/commit, which doesn't
	# always happen, and this process has no way to commit on the
	# agent's behalf itself when invoked from factoryd's sandboxed path:
	# `internal/sandbox/docker.go` mounts `.git` read-only inside that
	# sandbox unconditionally, by deliberate security design (a
	# real 2026-09-04 incident: an untrusted agent that could write
	# `.git/config` could plant a filter/hook the HOST's later `git
	# commit` would then execute with real credentials) -- so a commit
	# attempted from inside this process, in that environment, cannot
	# ever succeed (found live, 2026-09-17: `git add -A failed: ...
	# Read-only file system`). factoryd's own two-phase design handles
	# this instead: the caller omits --spec-acceptance-criteria from
	# this invocation entirely when it wants the conformity review to
	# run only after its own host-side safety-net commit has landed (see
	# agent/pi/scripts/conformity_review.py, run separately, after that
	# commit, in the sandboxed path). This function's own criteria
	# handling below is unchanged for a standalone/non-sandboxed caller
	# with a genuinely writable .git, where running both in one pass is
	# still correct.
	if spec_acceptance_criteria is not None and result.succeeded:
		criteria = read_acceptance_criteria(spec_acceptance_criteria)
		result.review_verdicts, _conformity_review_error = run_spec_conformity_review(
			workspace, criteria=criteria, review_base_sha=review_base_sha,
			thinking=thinking, adapter=adapter,
		)
		non_clean = [v["criterion"] for v in result.review_verdicts if v["verdict"] != "clean"]
		if non_clean and conformity_policy == "required":
			# Mirrors round_blockers' own required-policy handling of a
			# non-clean whole-diff review, but against conformity_policy,
			# not review_policy: the two are deliberately independent (see
			# BuildResult.conformity_policy's own doc comment). Under
			# "required", anything short of "clean" (including
			# "unavailable"/"no-review-verdict") must fail rather than
			# silently pass. "advisory" records the verdicts (above)
			# without revoking the success already earned by canonical
			# verification and whole-diff review.
			result.succeeded = False
			result.stopped_reason = f"spec conformity review did not clear all criteria: {non_clean}"

	return result


def review_verdicts(rounds: list[Round]) -> list[dict]:
	verdicts = []
	for rnd in rounds:
		for trace in rnd.traces:
			if trace.get("event") == "review" and trace.get("outcome") in ("clean", "flagged"):
				verdicts.append({"round": rnd.index, **trace})
	return verdicts


def write_report(result: BuildResult) -> Path:
	report_path = result.workspace / "BUILD_REPORT.md"
	lines = [
		"# Zero-human build report",
		"",
		f"Generated: {datetime.now(timezone.utc).isoformat()}",
		f"Spec: `{result.spec_path}`",
		f"Review policy: `{result.review_policy}`",
		f"Conformity policy: `{result.conformity_policy}`",
		f"AGENTS.md guidance used: {result.agents_md_used}"
		+ (f" (git blob {result.agents_md_git_blob})" if result.agents_md_used else ""),
		f"Outcome: {'SUCCEEDED' if result.succeeded else 'DID NOT SUCCEED'} -- {result.stopped_reason}",
		f"Rounds run: {len(result.rounds)}",
		"",
		"## Rounds",
		"",
	]
	for rnd in result.rounds:
		lines.append(f"### Round {rnd.index}")
		lines.append(f"- agent: {rnd.agent}")
		lines.append(f"- agent exit code: {rnd.agent_returncode}")
		lines.append(f"- agent timed out: {rnd.agent_timed_out}")
		errored, total = rnd.turn_errors
		if errored and errored == total:
			lines.append(f"- agent turns errored: {errored}/{total} (model route unreachable)")
		if rnd.usage:
			lines.append(f"- agent usage: {json.dumps(rnd.usage)}")
		lines.append(f"- reviewer outcome: {rnd.reviewer.outcome}{f' ({rnd.reviewer.detail})' if rnd.reviewer.detail else ''}")
		if rnd.reviewer.detail:
			lines.append("")
			lines.append("Reviewer comments:")
			lines.append("```")
			lines.append(rnd.reviewer.detail)
			lines.append("```")
		if rnd.fast_check_ran:
			lines.append(f"- fast check passed: {rnd.fast_check_passed}")
		if rnd.fast_check_ran and rnd.fast_check_passed is False:
			# Canonical verification was never attempted this round -- say
			# so explicitly rather than printing "(none resolved)", which
			# would read as "no command could be found" (Codex review of
			# PR #90).
			lines.append("- verify command: (not run -- fast check failed)")
		else:
			lines.append(f"- verify command: `{rnd.verify_command or '(none resolved)'}`")
		verify_status = "timed out" if rnd.verify_timed_out else str(rnd.verify_passed)
		lines.append(f"- verify passed: {verify_status}")
		lines.append(f"- duration: {rnd.duration_s:.1f}s")
		trace_summary = [f"{t.get('extension')}:{t.get('event')}={t.get('outcome')}" for t in rnd.traces]
		if trace_summary:
			lines.append(f"- extension traces: {', '.join(trace_summary)}")
		if (rnd.verify_passed is False or rnd.fast_check_passed is False) and rnd.verify_output_tail:
			lines.append("")
			lines.append("```")
			lines.append(rnd.verify_output_tail)
			lines.append("```")
		lines.append("")

	verdicts = review_verdicts(result.rounds)
	lines.append("## Independent review verdicts")
	lines.append("")
	if not verdicts:
		if result.review_policy == "advisory":
			lines.append(
				"No decisive clean/flagged verdict was recorded. Advisory policy "
				"allows success based on canonical verification; the unavailable "
				"reason appears in the round summary above."
			)
		elif result.review_policy == "degraded":
			lines.append(
				"No decisive clean/flagged verdict was recorded. Degraded policy "
				"allows labeled success when review is unavailable; the reason "
				"appears in the round summary above."
			)
		else:
			lines.append(
				"No decisive clean/flagged verdict was recorded. Required policy "
				"prevents local success in this state; the unavailable reason "
				"appears in the round summary above."
			)
	else:
		for verdict in verdicts:
			lines.append(f"- round {verdict['round']}: **{verdict['outcome']}** ({verdict.get('metadata', {}).get('trigger', 'unknown trigger')})")
	lines.append("")

	report_path.write_text("\n".join(lines))
	return report_path


def write_evidence_json(result: BuildResult) -> Path:
	"""Structured counterpart to write_report's own prose: the same
	per-round data (usage, reviewer verdict/detail, verify outcome,
	timing), shaped to match buildgate's own run.AgentEvidence/
	AgentEvidenceRound Go structs (internal/run/run.go) field-for-field
	so cmd/factoryd's loadAgentEvidence can json.Unmarshal this file
	directly, no translation layer on that side.

	Closes a real, currently-open gap (CLAIMS.md's Remaining gaps,
	`buildgate`): BUILD_EVIDENCE.json was never actually written by
	any version of this script on record, so run.AgentEvidence has been
	nil on every real accepted run to date -- loadAgentEvidence's own
	warning ("could not read BUILD_EVIDENCE.json") fires on every single
	run rather than only a build_app.py version genuinely too old to emit
	one, which is the only case that warning's own doc comment describes
	as expected.

	provider/model reflect PI_HARNESS_PROVIDER/PI_HARNESS_MODEL -- null exactly when those env
	vars are unset, matching AgentEvidence's own doc comment ("preserve
	JSON null when build_app.py inherited its configured defaults instead
	of explicitly pinning an identity"). A relay route that pins a worker
	model via those env vars (found live: PI_HARNESS_MODEL set but this
	evidence still reporting null) must show up here, not just per-round
	via Round.agent (rnd.agent below).

	schema_version's literal value (2) must always equal
	buildgate's own run.AgentEvidenceSchemaVersion constant
	(internal/run/run.go) -- bump both together, in the same change, when
	this payload's shape changes. loadAgentEvidence warns (never fails a
	run on it -- this evidence is best-effort by design) on a mismatch,
	so a future shape drift on either side of this two-repo, unversioned-
	file contract surfaces as a visible warning on the very next real run
	instead of silently misparsing or dropping fields, the way two real
	bugs already did before this field existed.
	"""
	SCHEMA_VERSION = 2
	evidence_path = result.workspace / "BUILD_EVIDENCE.json"
	payload = {
		"schema_version": SCHEMA_VERSION,
		"generated": datetime.now(timezone.utc).isoformat(),
		"review_policy": result.review_policy,
		# Additive, same convention as agents_md_used/review_verdicts
		# above: the per-criterion conformity review's own policy,
		# independent of review_policy -- see BuildResult.conformity_policy's
		# own doc comment.
		"conformity_policy": result.conformity_policy,
		"provider": os.environ.get("PI_HARNESS_PROVIDER"),
		"model": os.environ.get("PI_HARNESS_MODEL"),
		# Additive fields, tolerated by run.AgentEvidence's json.Unmarshal --
		# whether a committed AGENTS.md guided this build and its
		# content-addressed git blob id, so the evidence package shows what
		# repository guidance the agent actually saw.
		"agents_md_used": result.agents_md_used,
		"agents_md_git_blob": result.agents_md_git_blob,
		# review_verdicts is the per-criterion independent-reviewer
		# outcome (empty when no --spec-acceptance-criteria was given) --
		# additive, same convention as agents_md_used/agents_md_git_blob
		# above: tolerated by run.AgentEvidence's json.Unmarshal without a
		# schema_version bump, since an older factoryd simply doesn't read
		# this field.
		"review_verdicts": result.review_verdicts,
		"succeeded": result.succeeded,
		"stopped_reason": result.stopped_reason,
		# Additive: whether the one notes turn ran, and its usage. Never the
		# notes themselves (SC-018).
		"notes_turn": result.notes_turn,
		"rounds": [
			{
				"index": rnd.index,
				"agent": rnd.agent,
				"agent_returncode": rnd.agent_returncode,
				"agent_timed_out": rnd.agent_timed_out,
				"usage": rnd.usage,
				"reviewer_outcome": rnd.reviewer.outcome,
				"reviewer_detail": rnd.reviewer.detail,
				"verify_passed": rnd.verify_passed,
				"verify_timed_out": rnd.verify_timed_out,
				"duration_s": rnd.duration_s,
				# Additive fields, tolerated by run.AgentEvidenceRound's
				# json.Unmarshal -- whether a --fast-check-command ran this
				# round and, if so, whether it passed (null when none ran).
				"fast_check_ran": rnd.fast_check_ran,
				"fast_check_passed": rnd.fast_check_passed,
				# Additive, same convention as fast_check_ran/fast_check_passed
				# above: whether an in-loop --reference-oracle-command ran
				# this round and, if so, whether it passed (both null when
				# none was configured, or when it wasn't reached because
				# verify itself didn't pass this round -- see
				# run_reference_oracle's own doc comment).
				"oracle_command": rnd.oracle_command,
				"oracle_passed": rnd.oracle_passed,
				"oracle_output_tail": rnd.oracle_output_tail,
				# Additive, same convention: what the next round was told
				# about this one (see Round's own fields). blockers is []
				# for a round that passed, so a reader tells "passed" from
				# "written by a build_app.py that did not record it".
				"blockers": rnd.blockers,
				"changed_files": rnd.changed_files,
				"failure_signature": rnd.failure_signature,
				"failure_log": rnd.failure_log,
				"agent_notes": rnd.agent_notes,
				# Additive, present only for a round that ran autofix commands.
				**({"autofix": rnd.autofix} if rnd.autofix is not None else {}),
			}
			for rnd in result.rounds
		],
	}
	evidence_path.write_text(json.dumps(payload, indent=2) + "\n")
	return evidence_path


def main() -> int:
	global _process_started
	_process_started = _monotonic()
	parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
	parser.add_argument("--workspace", required=True, type=Path)
	parser.add_argument("--spec", required=True, type=Path)
	parser.add_argument("--max-rounds", type=int, default=3)
	parser.add_argument(
		"--thinking",
		choices=THINKING_LEVELS,
		default=None,
		help="Pi thinking level for this job; omitted inherits Pi's installed setting.",
	)
	parser.add_argument(
		"--review-policy",
		choices=("required", "degraded", "advisory"),
		default="advisory",
		help="Review policy: required blocks on unavailable/flagged review; degraded permits unavailable review; advisory records review but only canonical verification blocks success.",
	)
	parser.add_argument(
		"--conformity-policy",
		choices=("required", "advisory"),
		default="required",
		help="Policy for the separate per-criterion spec-conformity review --spec-acceptance-criteria "
		"enables, independent of --review-policy: required fails the build on any "
		"non-clean or unreachable per-criterion verdict; advisory records verdicts without "
		"failing the build. Meaningless (never consulted) when --spec-acceptance-criteria is "
		"not given, since no conformity review runs at all.",
	)
	parser.add_argument(
		"--sonnet-fallback",
		action="store_true",
		help="After local rounds are exhausted, authorize one billed claude-sonnet-5 corrective pass.",
	)
	parser.add_argument(
		"--review-base-sha",
		default=None,
		help="Commit the independent reviewer should diff against for the whole invocation, "
		"instead of HEAD when this process starts -- pass the true ticket-start boundary "
		"(e.g. ticket_runner.py's prior-ticket commit) so a retried invocation against "
		"already-committed work still gets reviewed rather than seeing an empty diff.",
	)
	parser.add_argument(
		"--harness", choices=sorted(harness_adapters.ADAPTERS), default="pi",
		help="Coding agent to drive (see harness_adapters.py).",
	)
	parser.add_argument("--timeout-minutes", type=int, default=45)
	parser.add_argument(
		"--verify-command",
		default=None,
		help="Override resolve_verify_command()'s own root-only auto-detection for every "
		"corrective round's internal verify -- pass the same command a caller's external "
		"canonical-verify gate already resolved (e.g. a ticket's own declared "
		"Verify-Command:) so the two never silently disagree. Found live (2026-09-09): "
		"without this, a monorepo whose root-level auto-detected command needs a toolchain "
		"the sandbox doesn't have (e.g. a Go+Flutter repo's own `make verify` needing Dart) "
		"fails every round on a missing-toolchain error unrelated to the agent's actual "
		"change, regardless of correctness, while an external gate using the right command "
		"never sees the mismatch. Omitted (the default) preserves the prior auto-detected "
		"behavior exactly.",
	)
	parser.add_argument(
		"--fast-check-command",
		default=None,
		help="Cheap check (format/lint/compile) run before --verify-command each round; a "
		"failure short-circuits the round without running the full verify command at all. "
		"Omitted (the default) preserves prior behavior exactly -- only --verify-command runs.",
	)
	parser.add_argument(
		"--setup-command",
		action="append",
		default=None,
		help="A repository setup command (`.factory.yml` setup:), repeatable. Run in order "
		"in the workspace once before the first agent turn (a failure ends the build without "
		"one) and again before each round's fast check and verify; one that fails fails the "
		"round and the checks are skipped. Omitted (the default) runs nothing extra.",
	)
	parser.add_argument(
		"--autofix-command",
		action="append",
		default=None,
		help="A repository autofix command (`.factory.yml` autofix:), repeatable. Run in order "
		"inside each round, after the agent's turn and before the round's checks. Advisory: "
		"a failure is recorded, never fails the round. It may change only files the ticket "
		"already changed; any other change is reverted. Omitted (the default) runs nothing extra.",
	)
	parser.add_argument(
		"--spec-acceptance-criteria",
		type=Path,
		default=None,
		help="Path to a file holding the approved spec's numbered acceptance criteria for "
		"this ticket, one per line. When given, an independent reviewer checks the final "
		"workspace against each criterion and returns a per-criterion verdict, recorded in "
		"BUILD_EVIDENCE.json's review_verdicts. Under --conformity-policy required (the "
		"default whenever this flag is given), any verdict short of \"clean\" (including an "
		"unreachable reviewer) fails the build. Omitted (the default) runs no conformity "
		"review at all.",
	)
	parser.add_argument(
		"--reference-oracle-command",
		default=None,
		help="Operator-authored check (same trust model as factoryd's own -reference-oracle-command: "
		"never ticket/agent-authored) run every round once --verify-command has already passed, "
		"against a host directory the Go launcher has already bind-mounted read-only into this "
		"container -- an additional correctness bar beyond canonical verify, not a substitute for "
		"it. A failing round gets the same targeted-retry treatment (corrective_prompt, "
		"continue_session) a canonical-verify failure already gets, instead of only being caught "
		"by the separate, one-shot post-build reference_oracle named gate. Omitted (the default) "
		"runs no in-loop oracle check at all -- prior behavior exactly.",
	)
	parser.add_argument(
		"--handoff",
		type=Path,
		default=None,
		help="Path to a short note about an earlier, interrupted attempt of this build whose "
		"work is already in the workspace. Its content is prepended to round 1's prompt only. "
		"Omitted (the default) leaves the prompt unchanged.",
	)
	parser.add_argument(
		"--earlier-attempt",
		type=Path,
		default=None,
		help="Path to the factory's record of an earlier attempt at this ticket that finished "
		"and failed its checks (what each round changed, which checks failed and what was "
		"found). Its content is put before the task in round 1's prompt. Omitted (the default) "
		"leaves the prompt unchanged.",
	)
	parser.add_argument(
		"--baseline-failure",
		type=Path,
		default=None,
		help="Path to the factory's note about the verify command's run on the untouched "
		"repository: the tests that failed there, all named by the ticket. Its content is put "
		"before the task in round 1's prompt. Omitted (the default) leaves the prompt unchanged.",
	)
	parser.add_argument(
		"--resume-from-state",
		type=Path,
		default=None,
		help="Path to a .pi-build-round-state.json written by an earlier run of this script. "
		"Continues at the round after the last completed one, in a fresh harness session, with "
		"the recorded next prompt. Refused (exit 2) when the file is unreadable, its version "
		"is not 1, or last_completed_round is not in [0, --max-rounds).",
	)
	args = parser.parse_args()
	if args.earlier_attempt is not None and args.spec_acceptance_criteria is not None:
		# The in-build conformity review runs while this build's session, and
		# the record in it, are in the workspace: a review must never run
		# beside the record of an earlier attempt.
		parser.error("--earlier-attempt cannot be combined with --spec-acceptance-criteria")
	adapter = harness_adapters.get(args.harness)
	adapter.prepare()

	try:
		result = run_build(
			args.workspace.resolve(), args.spec.resolve(),
			max_rounds=args.max_rounds,
			timeout_minutes=args.timeout_minutes,
			thinking=args.thinking, review_policy=args.review_policy, conformity_policy=args.conformity_policy,
			sonnet_fallback=args.sonnet_fallback,
			review_base_sha=args.review_base_sha,
			verify_command_override=args.verify_command,
			fast_check_command=args.fast_check_command,
			setup_commands=args.setup_command,
			autofix_commands=args.autofix_command,
			spec_acceptance_criteria=args.spec_acceptance_criteria,
			oracle_command=args.reference_oracle_command,
			handoff=args.handoff,
			resume_from_state=args.resume_from_state,
			earlier_attempt=args.earlier_attempt,
			baseline_failure=args.baseline_failure,
			adapter=adapter,
		)
	except RoundStateError as exc:
		print(f"build_app: {exc}", file=sys.stderr)
		return 2
	report_path = write_report(result)
	print(f"Report written to {report_path}")
	evidence_path = write_evidence_json(result)
	print(f"Evidence written to {evidence_path}")
	print(f"Outcome: {'SUCCEEDED' if result.succeeded else 'DID NOT SUCCEED'} -- {result.stopped_reason}")
	return 0 if result.succeeded else 1


if __name__ == "__main__":
	sys.exit(main())
