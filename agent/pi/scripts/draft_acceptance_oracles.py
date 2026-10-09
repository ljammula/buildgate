#!/usr/bin/env python3
"""Draft deterministic acceptance-test oracles for an approved spec's
acceptance criteria: ONE headless `pi` invocation PER criterion, each with
its own bounded timeout, assembled into one MANIFEST.json host-side after
every invocation finishes or times out. Before this, a single pi call
drafted every criterion in one pass sharing one wall-clock budget -- a
2026-09-22 live validation run found 7/7 attempts across two repos failed
to produce anything reviewable, mostly full 15-minute timeouts, because
one slow criterion (of ~7) burned the whole budget and lost every other
criterion's draft with it. Per-criterion invocation means a slow or hung
criterion only costs its own share of the budget: every other criterion
still gets drafted, reviewed, and reported on its own. See
per_criterion_minutes for the timeout split and
draft_one_criterion/assemble_manifest for the per-invocation and
host-side-assembly mechanics; see salvage_from_final_text for the "model
composed the content but never called the write tool" failure mode the
same run also found (attempt 3, json-merge-patch-rfc7396).

"Intake drafts the tests, wiring stays manual" -- this script only
classifies and drafts; it does not wire its output into a ticket's build.
An operator runs this manually, reviews the drafted
oracle(s) the same way they'd review any other reviewer-facing artifact,
and -- today -- manually passes the resulting file(s) to
`factoryd <run>`'s existing `-reference-oracle-dir`/
`-reference-oracle-mount-path`/`-reference-oracle-command`/
`-reference-oracle-in-loop-retry` flags themselves.

Companion to draft_spec.py/plan_tickets.py (same shape): a single
one-shot pi pass, no corrective rounds, no independent review -- "agent
output is evidence, never the oracle" applies doubly here, since this
script's whole output is itself a set of proposed oracles that must be
human-reviewed before they're trusted for anything. Reuses
build_app.py's read_acceptance_criteria so this script parses the exact
same criteria-file shape build_app.py's own --spec-acceptance-criteria
already does, instead of an independent Python re-implementation of
internal/request.SpecAcceptanceCriteria's markdown parsing that could
drift from it.

Usage:
    python3 draft_acceptance_oracles.py --criteria criteria.md \\
        --workspace /path/to/repo --out-dir oracles --evidence evidence.json \\
        [--timeout-minutes 15] [--feedback feedback.md]

--feedback (optional) is a text file of operator feedback on a previous
draft (the request driver writes oracle-feedback.md from oracle_review
rejections). Its text is appended to the prompt under a delimited
"Operator feedback on the previous draft" section as guidance only. A
missing or empty file is the same as no flag.

--criteria is the same flat, one-criterion-per-line file
build_app.py's own --spec-acceptance-criteria takes (e.g. what
cmd/factoryd/request_driver.go's writeTicketCriteriaFile already
produces per ticket) -- not spec.md itself, so this script never needs
its own copy of the markdown-section parser that already lives in Go.

The model is instructed to write a MANIFEST.json plus zero or more
oracle test files into a fixed scratch directory inside --workspace
(see DRAFT_RELATIVE_DIR below), not asked to print anything as its chat
response -- same reasoning as draft_spec.py's own DRAFT_RELATIVE_PATH.
This script then reads the manifest back, validates it names exactly
one entry per input criterion (order-preserving) with each named
oracle_file actually present, copies everything into --out-dir, and
writes --evidence.

Exit code 0 whenever AT LEAST ONE criterion was either drafted or
legitimately judged not oracle-able by its own pi invocation (a manifest
where every entry has oracle_file: null is still well-formed and still
exit 0 -- a legitimate, expected outcome for a spec whose criteria are
all inherently judgment calls). A criterion whose own pi invocation timed
out, exited non-zero, or wrote a manifest this script rejects (not valid
JSON, wrong entry count, mismatched criterion text, a referenced
oracle_file that doesn't exist) is recorded as an individual "not
drafted: <reason>" entry -- see draft_one_criterion -- and does not by
itself fail the run: the other criteria's drafts are still installed.
Exit code 2 only when NO criterion could be drafted or judged (every one
failed for a genuine model/route reason, as opposed to a legitimate
null) -- --evidence is still written in that case, with a per-criterion
"failures" list. Any other failure (a bad --criteria path, for instance)
raises and exits non-zero via the ordinary Python traceback, the same
convention every sibling script in this file uses for its own
argument/setup errors.

Manifest contract. Every MANIFEST.json entry this script WRITES carries,
beyond criterion/oracle_file/rationale: criterion_index (1-based position
in the criteria list this script was given; always set here, never trusted from the
model), target_path (repo-relative path the oracle would be committed at
in the target repo, or null -- the model proposes it from the file
inventory, and any value that is not a clean relative path, or that
touches .git/.buildgate/.oracle, is dropped to null), and supersedes
(always [] here; only an operator writes it). Older manifests lacking the
new fields stay valid for every reader.

Caps: each oracle file <= 16 KiB, all drafted content <= 64 KiB and at most 15 oracle files per request (request.MaxTicketOracleFiles = 15 and the 64 KiB per-ticket byte cap are enforced again at the materializer).
Over the cap, criterion order is kept and the remaining entries get
oracle_file null with rationale "dropped: over request cap" (or
"dropped: file over 16 KiB" for a single oversize file); dropped files
are not copied. The evidence JSON reports dropped_count.

Evidence `status`: "drafted" (>=1 oracle file written), "none_eligible"
(every criterion null, no drafts, and no criterion's own pi invocation
failed -- every null is a legitimate judgment call), "over_cap" (the cap
dropped >=1 entry AND >=1 oracle file was still written), "failed" (exit
2: no criterion drafted AND at least one criterion's own invocation
genuinely failed rather than judging null). A run in which every entry
ended up null, including ones the cap dropped, is "none_eligible"
(dropped_count still reports the dropped entries), so the caller keys on
"was anything written" and never creates a MANIFEST-only directory. For
none_eligible this script writes NO oracle files, only MANIFEST.json into
--out-dir; the Go caller reads the status and decides not to create the
oracle directory at all (a MANIFEST-only directory would halt the build
on a missing RUN_COMMAND.txt). Evidence also carries `failures`: one
{"criterion_index", "reason"} entry per criterion whose own invocation did
not produce a usable result (timeout, non-zero exit, rejected manifest,
or an unsalvageable missing manifest) -- present regardless of the run's
overall status, so a partially-successful draft ("drafted" overall, with
2 of 7 criteria failed) still reports exactly which criteria those were
and why, and `salvaged_count`: how many entries came from
salvage_from_final_text rather than a written MANIFEST.json.

--criterion-timeout-minutes overrides the per-criterion wall-clock budget
directly, bypassing the --timeout-minutes split below (cmd/factoryd's own
oracle-drafting job always passes this explicitly, computed by its own
mirrored split; a human running this script by hand, or a test, gets the
auto-split default instead). cmd/factoryd's own container this script
runs inside is sandboxed via runSandboxWithRetries and given a deadline
sized to outlive this script's own total per-criterion budget by a fixed
margin plus slack (requestJobContainerDeadline), not to agree with it
exactly, so this script's own wall-clock timeout fires -- and this
process gets to run its own timeout handling -- before Docker kills the
container. See per_criterion_minutes for the split/floor rule.
"""

from __future__ import annotations

import argparse
import ast
import json
import os
import posixpath
import re
import shutil
import signal
import subprocess
import sys
import threading
import time
import unicodedata
from datetime import datetime, timezone
from pathlib import Path

# build_app.py lives alongside this script in the same embedded harness
# (internal/harness.Ensure extracts every agent/pi/scripts/*.py file
# together into one directory) -- importing it by relative path, not as
# an installed package, mirrors how every script here is invoked: as
# bare `python3 <path>/draft_acceptance_oracles.py`, never `python3 -m`.
sys.path.insert(0, str(Path(__file__).resolve().parent))
import build_app  # noqa: E402
import harness_adapters  # noqa: E402
import draft_spec  # noqa: E402 -- reuses file_inventory, see build_prompt's own call site
import prompt_templates  # noqa: E402

# DRAFT_RELATIVE_DIR is where the model is instructed to write
# MANIFEST.json and any oracle files, relative to --workspace (pi's own
# cwd for the invocation below). Distinct from draft_spec.py's/
# plan_tickets.py's own scratch directories, so a request whose earlier
# drafting passes ran against the same workspace never collides with
# this one's leftover scratch content.
DRAFT_RELATIVE_DIR = Path(".factory-oracle-draft") / "ORACLES_DRAFT"

# EVIDENCE_SCHEMA_VERSION's literal value (1) must always equal
# cmd/factoryd/oracle_draft_job.go's oracleDraftEvidenceSchemaVersion
# constant -- see draft_spec.py's own EVIDENCE_SCHEMA_VERSION doc comment
# for why readDraftEvidence rejects an unrecognised version outright.
EVIDENCE_SCHEMA_VERSION = 1

MANIFEST_FILENAME = "MANIFEST.json"

MAX_ORACLE_FILE_BYTES = 16 * 1024
MAX_TOTAL_ORACLE_BYTES = 64 * 1024
# Request-level bound (the drafter runs before tickets exist). Equal to
# request.MaxTicketOracleFiles, so any draft that fits here fits one ticket
# (the per-ticket byte cap equals MAX_TOTAL_ORACLE_BYTES); the materializer
# still enforces both per ticket.
MAX_ORACLE_FILES = 15
MAX_FEEDBACK_BYTES = 16 * 1024
FORBIDDEN_TARGET_COMPONENTS = frozenset({".git", ".buildgate", ".oracle"})

QUALIFICATION_INSTRUCTIONS = prompt_templates.load("draft_acceptance_oracles.qualification")

GO_DRAFT_INSTRUCTIONS = QUALIFICATION_INSTRUCTIONS + prompt_templates.load("draft_acceptance_oracles.go", ("draft_dir", "max_files", "manifest_name"))

PYTHON_DRAFT_INSTRUCTIONS = QUALIFICATION_INSTRUCTIONS + prompt_templates.load("draft_acceptance_oracles.python", ("draft_dir", "max_files", "manifest_name"))


FEEDBACK_HEADER = prompt_templates.load("draft_acceptance_oracles.feedback", ("draft_dir", "feedback"))


PRIOR_IMPORTS_HEADER = prompt_templates.load("draft_acceptance_oracles.prior_imports", ("imports",))


def extract_python_imports(path: Path) -> list[str]:
	"""Returns every top-level `from X import Y`/`import X` line a drafted
	Python oracle file uses, rendered back in that exact form (so it can be
	handed to a later criterion's prompt as a literal line to reuse).
	Parse-only via ast.parse -- this never executes the file, the same
	non-execution guarantee internal/oraclecanary.CheckPythonOracle gets on
	the host side from its own `python3 -I` + ast inspection. Returns []
	for a file that doesn't parse (a malformed earlier draft is reported
	elsewhere; this function's only job is to read what compiles)."""
	try:
		tree = ast.parse(path.read_text(encoding="utf-8"), filename=str(path))
	except (OSError, SyntaxError, UnicodeDecodeError):
		return []
	lines: list[str] = []
	for node in ast.walk(tree):
		if isinstance(node, ast.ImportFrom) and node.module:
			names = ", ".join(alias.name for alias in node.names)
			lines.append(f"from {node.module} import {names}")
		elif isinstance(node, ast.Import):
			names = ", ".join(alias.name for alias in node.names)
			lines.append(f"import {names}")
	return lines


def read_feedback_ex(path: Path | None) -> tuple[str | None, bool]:
	"""Returns (text, truncated). The text is the feedback file's LAST
	MAX_FEEDBACK_BYTES (feedback accumulates oldest-first, so the newest
	operator reason is at the end and must survive), or None when path is
	unset, missing, unreadable, or blank -- the optional flag must never
	turn a missing note into a failed draft. truncated is True when older
	text was cut."""
	if path is None:
		return None, False
	try:
		with open(path, "rb") as handle:
			handle.seek(0, 2)
			size = handle.tell()
			truncated = size > MAX_FEEDBACK_BYTES
			handle.seek(max(0, size - MAX_FEEDBACK_BYTES))
			raw = handle.read(MAX_FEEDBACK_BYTES)
	except OSError:
		return None, False
	text = raw.decode("utf-8", errors="replace")
	return (text if text.strip() else None), truncated


def read_feedback(path: Path | None) -> str | None:
	return read_feedback_ex(path)[0]


def neutralise_feedback(text: str) -> str:
	"""Feedback is operator/model-influenced text placed between delimiter
	lines; break any run of the delimiter characters so a reason can never
	close the section early or forge a new one."""
	return text.replace("<<<", "< < <").replace(">>>", "> > >")


DRAFT_INSTRUCTIONS_BY_ECOSYSTEM = {"go": GO_DRAFT_INSTRUCTIONS, "python": PYTHON_DRAFT_INSTRUCTIONS}


def build_prompt(
	criteria: list[str], workspace: Path, draft_dir: Path, feedback: str | None = None,
	context_criteria: list[str] | None = None, ecosystem: str = "go",
	prior_imports: list[str] | None = None,
) -> tuple[str, bool]:
	"""Assembles the single prompt passed to pi_invocation. Returns
	(prompt, agents_md_used) -- same convention every sibling script in
	this file uses. Deliberately includes the target repo's own file
	inventory (reused from draft_spec.py) so the model can actually look
	at existing tests/conventions before choosing a test shape (AGENTS.md
	comes from the harness's own system prompt), but NOT the full
	spec/request text -- this pass only needs the criteria themselves plus enough repo
	context to write a real test, not the surrounding narrative.
	context_criteria, when given, are the request's other criteria, shown
	read-only before the ones to draft: per-criterion drafting otherwise
	sees a criterion like "every separator run becomes one space, so
	`"Joe's #5"` returns `"joe s"`" with nothing naming the function under
	test, and the model guessed one (see DRAFT_INSTRUCTIONS_BY_ECOSYSTEM).
	ecosystem picks which language's own drafting instructions are sent --
	cmd/factoryd's own oracle-drafting job classifies the workspace once
	(classifyDraftEcosystem) and always passes its answer explicitly via
	--ecosystem, so this script never independently re-derives it.
	prior_imports, when given, are the exact `from X import Y`/`import X`
	lines oracles drafted EARLIER in this same request already used (see
	extract_python_imports and its caller in run_draft_oracles): each
	criterion is drafted in its own isolated pi invocation that cannot see
	another's output, so without this, two criteria naming the same
	function could each guess a different module for it (live defect,
	2026-09-24: see this module's own doc comment)."""
	agents_md_used = build_app.committed_agents_md_blob(workspace) is not None
	numbered = "\n".join(criteria)
	sections = []
	if context_criteria:
		others = "\n".join(context_criteria)
		sections.append(
			"## The request's other acceptance criteria (context only)\n\n"
			"Read these only to learn what the request adds and what it is "
			"called. Do NOT draft them and do NOT give them manifest entries; "
			"each is drafted separately.\n\n"
			f"{others}\n"
		)
	sections.append(f"## Acceptance criteria\n\n{numbered}\n")
	# Reused directly from draft_spec.py (found via review: an earlier
	# version copy-pasted this instead of importing it, on the mistaken
	# assumption draft_spec wasn't reusably on sys.path -- it already is,
	# the same way build_app already is, both by the sys.path.insert
	# above) -- no independent copy of the same git-ls-files-capping
	# logic to drift from the original.
	inventory = draft_spec.file_inventory(workspace, draft_spec.FILE_INVENTORY_MAX_LINES)
	if inventory:
		sections.append(f"## Repository file inventory (git ls-files)\n\n```\n{inventory}\n```")
	if feedback and feedback.strip():
		sections.append(FEEDBACK_HEADER.format(draft_dir=draft_dir, feedback=neutralise_feedback(feedback.strip())))
	if prior_imports:
		sections.append(PRIOR_IMPORTS_HEADER.format(imports="\n".join(f"- {line}" for line in prior_imports)))
	instructions = DRAFT_INSTRUCTIONS_BY_ECOSYSTEM[ecosystem]
	sections.append(instructions.format(draft_dir=draft_dir, manifest_name=MANIFEST_FILENAME, max_files=MAX_ORACLE_FILES))
	return "\n\n".join(sections), agents_md_used


def write_evidence(
	evidence_path: Path, *, usage: dict | None, agent_exit_code: int, duration_s: float,
	agents_md_used: bool, criteria_count: int, oracle_count: int | None,
	status: str, dropped_count: int = 0, feedback_truncated: bool = False,
	failure_reason: str | None = None, failures: list[dict] | None = None,
	salvaged_count: int = 0, thinking: str | None = None,
) -> None:
	payload = {
		"schema_version": EVIDENCE_SCHEMA_VERSION,
		"generated": datetime.now(timezone.utc).isoformat(),
		"usage": usage,
		"agent_exit_code": agent_exit_code,
		"duration_s": duration_s,
		"agents_md_used": agents_md_used,
		"thinking": thinking,
		"criteria_count": criteria_count,
		# None when the manifest itself was missing/malformed -- distinct
		# from 0 (a well-formed manifest that legitimately drafted no
		# oracles at all), the same None-means-"not resolved" convention
		# build_app.py's own evidence fields already use.
		"oracle_count": oracle_count,
		"status": status,
		"dropped_count": dropped_count,
		"feedback_truncated": feedback_truncated,
		"failure_reason": _bounded_reason(failure_reason),
		# One {"criterion_index", "reason"} entry per criterion whose OWN
		# pi invocation did not produce a usable result -- present
		# regardless of overall status, so a "drafted" run that still lost
		# 2 of 7 criteria reports exactly which ones and why.
		"failures": [
			{"criterion_index": f["criterion_index"], "reason": _bounded_reason(f["reason"])}
			for f in (failures or [])
		],
		# How many entries came from salvage_from_final_text rather
		# than a MANIFEST.json the model actually wrote.
		"salvaged_count": salvaged_count,
	}
	evidence_path.parent.mkdir(parents=True, exist_ok=True)
	evidence_path.write_text(json.dumps(payload, indent=2) + "\n", encoding="utf-8")


FAILURE_TAIL_BYTES = 4096
FAILURE_REASON_CHARS = 1000
FAILURE_MAX_NAMES = 20
FAILURE_NAME_CHARS = 100


_ANSI = re.compile(r"\x1b\[[0-9;?]*[ -/]*[@-~]|\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)")
_SECRETS = [
	(re.compile(r"(authorization\s*[:=])[^\n]*", re.I), r"\1 [redacted]"),
	(re.compile(r"\bbearer\s+[A-Za-z0-9._~+/=-]+", re.I), "Bearer [redacted]"),
	(re.compile(r"\b([A-Za-z0-9_-]{0,40}(?:api[_-]?key|token|secret|password|passwd)[A-Za-z0-9_-]{0,40})(\s*[:=]\s*)(\"[^\"\n]*\"|'[^'\n]*'|[^\s,;]+)", re.I), r"\1\2[redacted]"),
	(re.compile(r"\bsk-[A-Za-z0-9_-]{8,}"), "sk-[redacted]"),
]


def _bidi_or_zero_width(ch: str) -> bool:
	o = ord(ch)
	return o in (0xAD, 0xFEFF) or 0x200B <= o <= 0x200F or 0x202A <= o <= 0x202E or 0x2060 <= o <= 0x2064 or 0x2066 <= o <= 0x2069


def sanitize_text(text: str) -> str:
	"""Drops ANSI sequences, control characters other than newline/tab/CR and
	bidi/zero-width characters, and redacts obvious credentials. Mirrors
	sanitizeLogText in cmd/factoryd/oracle_draft_job.go (shared vectors in
	cmd/factoryd/testdata/sanitize_vectors.json)."""
	text = _ANSI.sub("", text)
	text = "".join(ch for ch in text if ch in "\n\t\r" or not (unicodedata.category(ch) == "Cc" or _bidi_or_zero_width(ch)))
	for pattern, repl in _SECRETS:
		text = pattern.sub(repl, text)
	return text


def _tail(text: str) -> str:
	return sanitize_text(text[-4 * FAILURE_TAIL_BYTES:])[-FAILURE_TAIL_BYTES:]


MAX_PI_LINE_BYTES = 8 * 1024 * 1024
STDERR_TAIL_BYTES = 16 * 1024
DIGEST_TEXT_CHARS = 2000
DIGEST_PLAIN_LINES = 10
DIGEST_PLAIN_LINE_CHARS = 300
# Bound for AgentDigest.last_full_text (final-text salvage): a single criterion's
# own oracle file is capped at MAX_ORACLE_FILE_BYTES, so 3x that leaves
# generous room for one file's content plus surrounding prose/manifest
# JSON in the model's final response, without letting an adversarial or
# runaway model respose grow this process's own memory unbounded.
FULL_TEXT_MAX_CHARS = 3 * MAX_ORACLE_FILE_BYTES


class AgentDigest:
	"""Incremental, memory-bounded digest of the agent's `--mode json` stdout: event and
	turn counters, summed token usage, the last assistant turn's error/text,
	the tail of what the model was last streaming, and the last few non-JSON
	lines. Nothing here grows with the size of the stream."""

	def __init__(self, adapter=build_app.DEFAULT_ADAPTER) -> None:
		self.adapter = adapter
		self.events = 0
		self.last_type: object = None
		self.turns = 0
		self.errored = 0
		self.oversize_lines = 0
		self.last_error: str | None = None
		self.last_text: str | None = None
		# Untruncated (up to FULL_TEXT_MAX_CHARS) text of the LAST completed
		# assistant turn -- mirrors harness_adapters.final_assistant_text's own
		# "last completed assistant turn" semantics, but computed
		# incrementally here (never holding the whole stdout stream) and
		# bounded, so salvage_from_final_text has enough of the model's own
		# response to find a whole oracle file in, not just last_text's
		# short tail preview.
		self.last_full_text: str | None = None
		self.streamed = ""
		self.plain: list[str] = []
		self._usage: dict = {}

	def feed(self, line: str) -> None:
		line = line.strip()
		if not line:
			return
		try:
			event = json.loads(line)
		except (json.JSONDecodeError, RecursionError):
			self._add_plain(line)
			return
		if not isinstance(event, dict):
			self._add_plain(line)
			return
		self.events += 1
		self.last_type = event.get("type")
		# The harness's own adapter reads its event shapes; this only folds
		# the neutral facts it reports into bounded state.
		facts = self.adapter.line_event(line)
		if not facts:
			return
		if facts.get("stream_reset"):
			self.streamed = ""
		delta = facts.get("stream_delta")
		if isinstance(delta, str):
			self.streamed = (self.streamed + delta[-DIGEST_TEXT_CHARS:])[-DIGEST_TEXT_CHARS:]
		if facts.get("turn"):
			self.turns += 1
		if facts.get("failed"):
			self.errored += 1
		error = facts.get("error")
		if isinstance(error, str) and error:
			self.last_error = error[:DIGEST_TEXT_CHARS]
		text = facts.get("text")
		if isinstance(text, str):
			self.last_text = text[-DIGEST_TEXT_CHARS:] or None
			self.last_full_text = text[:FULL_TEXT_MAX_CHARS] if text else None
		for key, value in (facts.get("usage") or {}).items():
			self._usage[key] = self._usage.get(key, 0) + value

	def _add_plain(self, line: str) -> None:
		self.plain.append(line[:DIGEST_PLAIN_LINE_CHARS])
		del self.plain[:-DIGEST_PLAIN_LINES]

	def usage(self) -> dict | None:
		return dict(self._usage) or None

	def summary(self) -> str:
		lines = [f"{self.adapter.name} events: {self.events}; last event: {self.last_type}; assistant turns: {self.turns} ({self.errored} errored)"]
		if self.oversize_lines:
			lines.append(f"{self.oversize_lines} output line(s) over {MAX_PI_LINE_BYTES} bytes were skipped")
		if self.last_error:
			lines.append(f"last assistant error: {self.last_error[:800]}")
		if self.last_text:
			lines.append(f"last assistant text: {self.last_text[-800:]}")
		if self.streamed:
			lines.append(f"model was last streaming (tail): {self.streamed[-800:]}")
		if self.plain:
			lines.append("non-JSON stdout (tail): " + " | ".join(self.plain)[-800:])
		return "\n".join(lines)


def summarize_pi_output(stdout: str) -> str:
	"""Digest of an in-memory pi stdout (tests, small inputs)."""
	digest = AgentDigest()
	for line in stdout.splitlines():
		digest.feed(line)
	return digest.summary()


class PiRun:
	"""Outcome of one bounded pi invocation."""

	def __init__(self, returncode: int, digest: AgentDigest, stderr_tail: str = "", timed_out: bool = False) -> None:
		self.returncode = returncode
		self.digest = digest
		self.stderr_tail = stderr_tail
		self.timed_out = timed_out

	@classmethod
	def from_text(
		cls, returncode: int, stdout: str, stderr: str = "", timed_out: bool = False, adapter=build_app.DEFAULT_ADAPTER,
	) -> "PiRun":
		digest = AgentDigest(adapter)
		for line in stdout.splitlines():
			digest.feed(line)
		return cls(returncode, digest, stderr[-STDERR_TAIL_BYTES:], timed_out)


def _drain_stdout(stream, digest: AgentDigest) -> None:
	skipping = False
	while True:
		chunk = stream.readline(MAX_PI_LINE_BYTES + 1)
		if not chunk:
			return
		complete = chunk.endswith(b"\n")
		if skipping:
			skipping = not complete
			continue
		if not complete and len(chunk) > MAX_PI_LINE_BYTES:
			digest.oversize_lines += 1
			skipping = True
			continue
		digest.feed(chunk.decode("utf-8", errors="replace"))


def _drain_stderr(stream, tail: bytearray) -> None:
	while True:
		chunk = stream.read(65536)
		if not chunk:
			return
		tail += chunk
		del tail[:-STDERR_TAIL_BYTES]


PROCESS_GROUP_GRACE_SECONDS = 5.0


def _signal_group(proc: subprocess.Popen, sig: int) -> None:
	try:
		os.killpg(proc.pid, sig)
	except (ProcessLookupError, PermissionError):
		pass


def _terminate_group(proc: subprocess.Popen) -> None:
	"""SIGTERM the whole process group, then SIGKILL it after a grace period,
	so grandchildren of pi cannot outlive the drafter."""
	_signal_group(proc, signal.SIGTERM)
	try:
		proc.wait(timeout=PROCESS_GROUP_GRACE_SECONDS)
	except subprocess.TimeoutExpired:
		pass
	_signal_group(proc, signal.SIGKILL)


def run_pi_bounded(command: list[str], cwd: Path, timeout: float, adapter=build_app.DEFAULT_ADAPTER) -> PiRun:
	"""Runs pi reading its output incrementally, so a model that streams
	hundreds of MB cannot exhaust this process's memory: stdout goes through a
	AgentDigest (per-line cap MAX_PI_LINE_BYTES) and only the last
	STDERR_TAIL_BYTES of stderr are kept. On timeout the process is killed and
	the run is marked timed_out."""
	# Its own session/process group, so a timeout can take down pi's whole tree.
	proc = subprocess.Popen(command, cwd=cwd, stdin=subprocess.DEVNULL, stdout=subprocess.PIPE, stderr=subprocess.PIPE, start_new_session=True)
	digest = AgentDigest(adapter)
	stderr_tail = bytearray()
	threads = [
		threading.Thread(target=_drain_stdout, args=(proc.stdout, digest), daemon=True),
		threading.Thread(target=_drain_stderr, args=(proc.stderr, stderr_tail), daemon=True),
	]
	for t in threads:
		t.start()
	timed_out = False
	try:
		returncode = proc.wait(timeout=timeout)
	except subprocess.TimeoutExpired:
		timed_out = True
		_terminate_group(proc)
		returncode = proc.wait()
	for t in threads:
		t.join(timeout=10)
	return PiRun(-1 if timed_out else returncode, digest, stderr_tail.decode("utf-8", errors="replace"), timed_out)


def _bounded_reason(reason: str | None) -> str | None:
	if reason is None:
		return None
	return sanitize_text(reason[:4 * FAILURE_REASON_CHARS])[:FAILURE_REASON_CHARS]


def report_failure(reason: str | None, result: PiRun, draft_dir: Path) -> None:
	"""Prints why the draft failed to stderr, which the driver captures into
	the request's oracle-draft log: a failed pass must never leave an empty
	log. Everything model-controlled (file names, output) is sanitised and
	bounded: at most FAILURE_MAX_NAMES names of FAILURE_NAME_CHARS each, and a
	tail of the model process's stdout and stderr."""
	names: list[str] = []
	total = 0
	try:
		if draft_dir.is_dir():
			for entry in draft_dir.iterdir():
				total += 1
				if len(names) < FAILURE_MAX_NAMES:
					names.append(sanitize_text(entry.name)[:FAILURE_NAME_CHARS])
	except OSError:
		pass
	names.sort()
	print(f"draft_acceptance_oracles: FAILED: {_bounded_reason(reason)}", file=sys.stderr)
	print(f"draft_acceptance_oracles: pi exit code {result.returncode}; {total} entries in {draft_dir}, first {len(names)}: {names}", file=sys.stderr)
	print("--- pi output (summary) ---", file=sys.stderr)
	print(_tail(result.digest.summary()), file=sys.stderr)
	print("--- pi stderr (tail) ---", file=sys.stderr)
	print(_tail(result.stderr_tail), file=sys.stderr)


def validate_manifest(manifest: object, criteria: list[str], draft_dir: Path) -> list[dict]:
	"""Validates manifest is a well-formed JSON array with exactly one
	entry per criterion, in order, each entry's criterion text matching
	(by build_app.criterion_key -- see its own doc comment: a model that
	echoes a criterion without its leading "N. ", final period or
	backticks is not by itself a malformed response) and, when
	oracle_file is set, naming a plain, single-component filename that
	actually exists directly under draft_dir. Raises ValueError naming
	exactly what's wrong -- the caller treats any failure here as exit
	code 2, the same "model/route failure" bucket a missing draft
	already is, since an untrustworthy manifest is no more usable than
	no manifest at all.

	Deliberately all-or-nothing, not build_app.py's parse_conformity_verdicts'
	own dict-keyed, per-entry-degrading pattern for the same "match a
	model's echoed criterion back to the canonical list" problem
	(considered, found via review): conformity-review is judging an
	already-final diff, where "unavailable" is a legitimate per-criterion
	answer under -conformity-policy advisory. This script is itself
	validating an UNPROVEN classification heuristic, explicitly meant to
	"validate the classification heuristic... before automating
	anything" -- silently accepting a partially-matched manifest here
	would hide exactly the heuristic failures this validation pass
	exists to surface, in exchange for saving one re-run of a single pi
	invocation. Worth revisiting if/when this stops being an experimental
	validation pass.

	oracle_file is treated as adversarial input, not merely untrusted
	prose (found via review): this whole pipeline's own "agent output is
	evidence, never the oracle" principle applies to a filesystem path
	doubly, not less, than to a verdict string. A bare `draft_dir /
	oracle_file` join is exactly wrong for that -- Python's own
	Path.__truediv__ DISCARDS the left side entirely when the right side
	is absolute, and a relative "../../etc/passwd"-shaped value walks
	straight out of draft_dir -- so this rejects anything but a single
	path component up front (no "/", no os.sep, no ".." as the whole
	name) before ever joining it with a real directory, rather than
	joining first and hoping a later is_file()/read/write call happens
	to fail safely. This also forecloses draft_dir/out_dir ever
	disagreeing about whether a subdirectory needs to exist -- see the
	copy loop's own comment for why nested paths were rejected outright
	instead of being supported properly."""
	if not isinstance(manifest, list):
		raise ValueError("manifest is not a JSON array")
	if len(manifest) != len(criteria):
		raise ValueError(f"manifest has {len(manifest)} entries, want exactly {len(criteria)} (one per criterion)")
	keys = [build_app.criterion_key(c) for c in criteria]
	for i in range(len(keys)):
		for j in range(i):
			if keys[i] == keys[j]:
				raise ValueError(f"criteria {j} and {i} are the same after normalisation, so an echoed criterion cannot be matched to one of them")
	for i, (entry, criterion) in enumerate(zip(manifest, criteria)):
		if not isinstance(entry, dict):
			raise ValueError(f"manifest entry {i} is not a JSON object")
		got = str(entry.get("criterion") or "")
		if build_app.criterion_key(got) != build_app.criterion_key(criterion):
			raise ValueError(f"manifest entry {i} criterion {got!r} does not match expected {criterion!r}")
		oracle_file = entry.get("oracle_file")
		if oracle_file is not None:
			if not isinstance(oracle_file, str) or not oracle_file:
				raise ValueError(f"manifest entry {i} has a non-string/empty oracle_file")
			if oracle_file != Path(oracle_file).name or oracle_file in (".", ".."):
				raise ValueError(f"manifest entry {i} names oracle_file {oracle_file!r}, which must be a plain filename with no path separators or \"..\"")
			if not (draft_dir / oracle_file).is_file():
				raise ValueError(f"manifest entry {i} names oracle_file {oracle_file!r}, which was not written to {draft_dir}")
	return manifest


def normalize_target_path(value: object) -> str | None:
	"""Returns value when it is a clean, repo-relative, POSIX path outside
	.git/.buildgate/.oracle, else None. The model's proposal is adversarial
	input: anything not already in cleaned form is dropped, never repaired."""
	if not isinstance(value, str) or not value:
		return None
	if "\\" in value or any(ord(c) < 32 for c in value):
		return None
	if value.startswith("/") or posixpath.normpath(value) != value:
		return None
	parts = value.split("/")
	if any(p in ("", ".", "..") for p in parts):
		return None
	if any(p.lower() in FORBIDDEN_TARGET_COMPONENTS for p in parts):
		return None
	return value


def directory_spelling(value: str) -> str | None:
	"""Returns value as a bare directory path with a leading "./" and trailing
	"/" removed ("" for the repository root "."), or None when value is not a
	directory-shaped spelling. It does no safety validation: the caller passes
	the joined result through normalize_target_path."""
	if value.startswith("/"):
		return None
	v = value
	while v.startswith("./"):
		v = v[2:]
	v = v.rstrip("/")
	if v == ".":
		v = ""
	if v == "" and value not in (".", "./") and not value.endswith("/"):
		return None
	return v


def normalize_manifest(manifest: list[dict], draft_dir: Path, workspace: Path | None = None) -> tuple[list[dict], int]:
	"""Returns (entries, dropped_count): a validated manifest with the new
	fields set deterministically and the size caps applied in criterion
	order. Once the request cap trips, every later oracle is dropped."""
	out: list[dict] = []
	total = 0
	count = 0
	kept: set[str] = set()
	refused: dict[str, str] = {}
	capped = False
	dropped = 0
	for index, entry in enumerate(manifest, start=1):
		entry = dict(entry)
		entry["criterion_index"] = index
		raw_target = entry.get("target_path")
		entry["target_path"] = normalize_target_path(raw_target)
		entry["supersedes"] = []
		oracle_file = entry.get("oracle_file")
		# Found live 2026-09-20: a model gives the package DIRECTORY as target_path.
		# When it names an existing directory in the workspace, the file goes in it.
		if oracle_file and isinstance(raw_target, str):
			directory = directory_spelling(raw_target)
			if directory is not None and (directory == "" or raw_target.endswith("/") or (workspace is not None and (workspace / directory).is_dir())):
				entry["target_path"] = normalize_target_path(f"{directory}/{oracle_file}" if directory else oracle_file)
		if oracle_file:
			size = (draft_dir / oracle_file).stat().st_size
			reason = None
			# One file may cover several criteria (one entry each): it counts once.
			if oracle_file in kept:
				pass
			elif oracle_file in refused:
				reason = refused[oracle_file]
			elif capped:
				reason = "dropped: over request cap"
			elif size > MAX_ORACLE_FILE_BYTES:
				reason = "dropped: file over 16 KiB"
			elif total + size > MAX_TOTAL_ORACLE_BYTES or count >= MAX_ORACLE_FILES:
				capped = True
				reason = "dropped: over request cap"
			if reason:
				refused.setdefault(oracle_file, reason)
				entry["oracle_file"] = None
				entry["target_path"] = None
				entry["rationale"] = reason
				dropped += 1
			elif oracle_file not in kept:
				kept.add(oracle_file)
				total += size
				count += 1
		else:
			entry["target_path"] = None
		out.append(entry)
	return out, dropped


DEFAULT_CRITERION_FLOOR_MINUTES = 3


def per_criterion_minutes(total_minutes: int, n_criteria: int, floor_minutes: int = DEFAULT_CRITERION_FLOOR_MINUTES) -> int:
	"""Splits a request's overall drafting budget evenly across its
	criteria, each of which now gets its OWN pi invocation and its own
	bounded timeout. Floored so a request with many criteria never
	gets an unworkably short per-criterion budget: max(floor, total // n).
	cmd/factoryd's own oracleCriterionTimeoutMinutes mirrors this exact
	split and passes its result explicitly via --criterion-timeout-minutes,
	so the two languages never have to agree on the formula independently
	-- this function is only the fallback used when the script runs without
	that flag (a human operator, or a test)."""
	if n_criteria <= 0:
		return total_minutes
	return max(floor_minutes, total_minutes // n_criteria)


_FENCE_RE = re.compile(r"```[^\n`]*\n(.*?)```", re.DOTALL)
_LABEL_WINDOW_CHARS = 200


def _find_fenced_blocks(text: str) -> list[tuple[int, int, str]]:
	"""Returns (start, end, content) for each ``` ... ``` fenced block in
	text, in the order they appear."""
	return [(m.start(), m.end(), m.group(1)) for m in _FENCE_RE.finditer(text)]


def _iter_json_arrays(text: str):
	"""Like build_app.py's own _iter_json_objects, but for top-level JSON
	ARRAYS -- a manifest is a JSON array, never a bare object. Scans for
	each "[" and attempts a real parse there (json.JSONDecoder.raw_decode)
	rather than matching with a regex, for the same reason
	_iter_json_objects does."""
	decoder = json.JSONDecoder()
	i, n = 0, len(text)
	while i < n:
		if text[i] != "[":
			i += 1
			continue
		try:
			obj, end = decoder.raw_decode(text, i)
		except json.JSONDecodeError:
			i += 1
			continue
		yield obj
		i = end


def _is_safe_oracle_filename(name: object) -> bool:
	"""The same plain-single-component check validate_manifest applies to
	oracle_file, factored out so salvage_from_final_text can refuse an
	unsafe model-proposed name before ever writing it to disk (a manifest
	fragment inside the model's own prose is exactly as adversarial as one
	it actually wrote to MANIFEST.json)."""
	return isinstance(name, str) and bool(name) and name == Path(name).name and name not in (".", "..")


def salvage_from_final_text(text: str, criterion: str, draft_dir: Path) -> list[dict] | None:
	"""Recovers a manifest entry (and writes its oracle file to
	draft_dir) from pi's own final assistant text, for a criterion whose pi
	call exited 0 but wrote no MANIFEST.json. Found in a 2026-09-22 live
	validation run (attempt 3): the model composed a complete,
	well-formed manifest entry and test file in its own response text but
	never called its write tool.

	Never guesses: a fenced code block is only trusted as a specific
	oracle_file's content when the model's OWN final text ties the two
	together, one of two ways --
	  (a) the text contains exactly one well-formed, single-entry,
	      manifest-shaped JSON array naming a (safe, non-null) oracle_file
	      for this criterion, AND the text has exactly one fenced code
	      block overall (nothing else that entry could refer to), or
	  (b) a fenced code block is immediately preceded (within
	      _LABEL_WINDOW_CHARS characters) by that manifest entry's own
	      oracle_file name or target_path, as a literal token.
	Two or more candidate manifests that disagree, multiple unlabeled code
	blocks with neither (a) nor (b) applying, or no manifest-shaped JSON at
	all: this returns None -- the caller treats None exactly like "nothing
	salvaged", never a guess at which block might be the file.

	Salvaged content is written to disk and returned in the same shape
	validate_manifest itself accepts, so every downstream check (size
	caps, target_path cleaning, the Go host's own compile self-check)
	applies to it exactly as it would to a file the model actually wrote --
	it is untrusted model output either way."""
	candidate: dict | None = None
	for arr in _iter_json_arrays(text):
		if not (isinstance(arr, list) and len(arr) == 1 and isinstance(arr[0], dict)):
			continue
		entry = arr[0]
		if not _is_safe_oracle_filename(entry.get("oracle_file")):
			continue
		if candidate is not None and candidate.get("oracle_file") != entry.get("oracle_file"):
			return None  # disagreeing candidates: ambiguous, never guess
		candidate = entry

	blocks = _find_fenced_blocks(text)
	if not blocks or candidate is None:
		return None

	name = candidate["oracle_file"]
	raw_target = candidate.get("target_path")
	tokens = [t for t in (name, raw_target if isinstance(raw_target, str) else None) if t]
	token_res = [re.compile(r"(?<![A-Za-z0-9_./-])" + re.escape(t) + r"(?![A-Za-z0-9_./-])") for t in tokens]
	labeled = [b for b in blocks if any(tr.search(text[max(0, b[0] - _LABEL_WINDOW_CHARS):b[0]]) for tr in token_res)]
	if len(labeled) == 1:
		chosen = labeled[0]
	elif len(blocks) == 1:
		chosen = blocks[0]
	else:
		return None  # several unlabeled blocks: ambiguous, never guess

	content = chosen[2]
	if not content.strip():
		return None
	draft_dir.mkdir(parents=True, exist_ok=True)
	(draft_dir / name).write_text(content, encoding="utf-8")
	return [{
		"criterion": criterion,
		"oracle_file": name,
		"target_path": normalize_target_path(raw_target),
		"rationale": "salvaged from the model's final response text (it never called the write tool)",
	}]


class CriterionResult:
	"""Outcome of ONE criterion's own bounded pi invocation (draft_one_criterion)."""

	def __init__(self, entry: dict, draft_dir: Path, result: PiRun, usage: dict | None, salvaged: bool, failure_reason: str | None) -> None:
		self.entry = entry
		self.draft_dir = draft_dir
		self.result = result
		self.usage = usage
		self.salvaged = salvaged
		self.failure_reason = failure_reason


def oracle_filename_for(index: int, ecosystem: str = "go") -> str:
	"""The canonical, index-derived local file name every criterion's own
	pi invocation is told to use (draft_one_criterion) and assemble_manifest
	independently re-derives when it renames a drafted file -- a single
	source for the "oracle_{NNN:03d}_test.go" / "test_oracle_{NNN:03d}.py"
	shape so the two never drift. The Python shape matches
	oraclecanary.Classify's own test_oracle_*.py pattern (Go's own
	*_test.go pattern needs no such prefix)."""
	if ecosystem == "python":
		return f"test_oracle_{index:03d}.py"
	return f"oracle_{index:03d}_test.go"


# CRITERION_INDEX_ADDENDUM is appended (not merged into DRAFT_INSTRUCTIONS,
# which build_prompt's other callers/tests still exercise with a bare
# single-criterion list and no index) to each criterion's own prompt, so the
# model knows its TRUE position among ALL the request's criteria instead of
# defaulting to "1" every time. Found live 2026-09-24
# (add-a-get-todos-id-shout-endpoint-that-r-20260924-024340): every one of 6
# separate invocations independently named its file oracle_001_test.go
# (with a matching target_path), so 3 of them collided once assembled --
# this is a best-effort mitigation at the source; assemble_manifest's own
# rewrite/refuse logic is what actually guarantees no collision reaches the
# Go host, whether or not the model follows this instruction.
CRITERION_INDEX_ADDENDUM = prompt_templates.load("draft_acceptance_oracles.criterion_index", ("index", "total", "oracle_filename", "default_filename"))


def draft_one_criterion(
	index: int, criterion: str, workspace: Path, timeout_minutes: int, feedback: str | None, all_criteria: list[str],
	ecosystem: str = "go", prior_imports: list[str] | None = None, thinking: str | None = None,
	adapter=build_app.DEFAULT_ADAPTER,
) -> CriterionResult:
	"""Runs ONE bounded pi invocation to draft a single acceptance
	criterion, in its own scratch directory (so a slow or misbehaving
	criterion's output can never collide with another's). Always returns a
	CriterionResult carrying a well-formed single-entry manifest `entry`
	-- never raises for a model/route failure: a genuine failure or timeout
	gets a null oracle_file entry with a "not drafted: <reason>" rationale,
	the same shape a legitimate judgment-call null already has, so every
	criterion is accounted for in the assembled manifest a human reviews at
	oracle_review. prior_imports is forwarded to build_prompt
	unchanged -- see its own doc comment."""
	rel_draft_dir = DRAFT_RELATIVE_DIR / f"c{index:03d}"
	draft_dir = workspace / rel_draft_dir
	others = [c for i, c in enumerate(all_criteria, start=1) if i != index]
	prompt, _agents_md_used = build_prompt(
		[criterion], workspace, rel_draft_dir, feedback=feedback, context_criteria=others, ecosystem=ecosystem,
		prior_imports=prior_imports,
	)
	prompt += "\n\n" + CRITERION_INDEX_ADDENDUM.format(
		index=index, total=len(all_criteria),
		oracle_filename=oracle_filename_for(index, ecosystem), default_filename=oracle_filename_for(1, ecosystem),
	)

	session_dir = workspace / ".factory-oracle-draft" / "session" / f"c{index:03d}"
	session_dir.mkdir(parents=True, exist_ok=True)
	# One folder for every criterion's prompt: the host copies session/prompts.
	build_app.saved_prompts.save_prompt(session_dir.parent, f"draft-oracle-c{index:03d}", prompt)
	command = adapter.invocation(
		workspace, prompt=prompt, session_dir=session_dir,
		continue_session=False, thinking=thinking,
	)

	# A wall-clock overrun is a route failure like any other, not a crash for
	# THIS criterion; the other criteria still get their own full budget.
	result = run_pi_bounded(command, workspace, timeout_minutes * 60, adapter)

	manifest_path = draft_dir / MANIFEST_FILENAME
	failure_reason: str | None = None
	salvaged = False
	entry: dict | None = None
	if result.timed_out:
		failure_reason = f"the pi invocation exceeded its {timeout_minutes} minute wall-clock budget and was killed"
	elif result.returncode != 0:
		failure_reason = f"the pi invocation exited {result.returncode}"
	elif manifest_path.is_file():
		try:
			manifest = validate_manifest(
				json.loads(manifest_path.read_text(encoding="utf-8")), [criterion], draft_dir,
			)
			entry = dict(manifest[0])
		# See run_draft_oracles' own history of this same except clause:
		# a manifest that isn't valid UTF-8 is exactly as untrustworthy as
		# one that isn't valid JSON.
		except (ValueError, json.JSONDecodeError, UnicodeDecodeError) as exc:
			failure_reason = f"the drafted manifest was rejected: {exc}"
	else:
		salvaged_manifest = salvage_from_final_text(result.digest.last_full_text or "", criterion, draft_dir)
		if salvaged_manifest is not None:
			entry = salvaged_manifest[0]
			salvaged = True
		else:
			failure_reason = "the model finished without writing MANIFEST.json, and no unambiguous file content could be salvaged from its final response"

	if entry is None:
		entry = {"criterion": criterion, "oracle_file": None, "target_path": None, "rationale": f"not drafted: {_bounded_reason(failure_reason)}"}

	return CriterionResult(entry=entry, draft_dir=draft_dir, result=result, usage=result.digest.usage(), salvaged=salvaged, failure_reason=failure_reason)


def assemble_manifest(results: list[CriterionResult], workspace: Path, ecosystem: str = "go") -> tuple[list[dict], Path, list[dict]]:
	"""Copies each criterion's own file (drafted or salvaged) into one
	shared scratch directory, under a name unique to its criterion index,
	so normalize_manifest's cap/target_path logic -- written for one shared
	draft_dir -- runs unchanged over criteria drafted by SEPARATE pi
	invocations that never saw each other's output and so could easily
	have picked the same file name (e.g. every one of them choosing
	"oracle_001_test.go").

	When a criterion has a target_path, its FILE NAME (basename) is ALWAYS
	replaced with the same renamed, collision-free oracle_file -- never
	trusted as-is, and never merely checked against the model's own
	oracle_file name -- because live drafts show the model's target_path
	basename can legitimately be almost anything: sometimes it matches its
	own local scratch file name, sometimes it's the real convention-driven
	name of an existing file in the package (e.g. two DIFFERENT criteria
	both correctly targeting an existing "merge_test.go"). Only the
	DIRECTORY portion of target_path is kept -- that's the model's real
	information about where in the repo this test belongs. Renaming every
	basename to its own index-derived name makes every target_path unique
	BY CONSTRUCTION, so no cross-criterion collision can reach the Go
	host's own duplicate-target check.

	Found live 2026-09-24 in two separate incidents, both on this exact
	pattern: (1) add-a-get-todos-id-shout-endpoint-that-r-20260924-024340,
	3 of 6 separately-drafted criteria all independently named their local
	file AND target_path "oracle_001_test.go"; (2)
	implement-mergepatch-target-patch-byte-b-20260924-024340, 2 of 9
	criteria whose target_path was the real, correctly-shared
	"merge_test.go" (a basename that never matched their own local scratch
	file name at all). Both collided at the Go host and lost the WHOLE
	draft, including every criterion that had drafted correctly. An
	earlier version of this function rejected the second shape outright
	(treating a target_path/oracle_file basename mismatch as untrustworthy)
	-- wrong: that mismatch is normal, not a defect, so this now always
	rewrites instead of ever refusing on it."""
	assembled_dir = workspace / DRAFT_RELATIVE_DIR / "ASSEMBLED"
	if assembled_dir.exists():
		shutil.rmtree(assembled_dir)
	assembled_dir.mkdir(parents=True, exist_ok=True)
	manifest: list[dict] = []
	failures: list[dict] = []
	for index, result in enumerate(results, start=1):
		entry = dict(result.entry)
		src_name = entry.get("oracle_file")
		if not src_name:
			manifest.append(entry)
			continue

		unique_name = oracle_filename_for(index, ecosystem)
		raw_target = entry.get("target_path")
		new_target: str | None = None
		if isinstance(raw_target, str) and raw_target:
			directory = posixpath.dirname(raw_target)
			new_target = posixpath.join(directory, unique_name) if directory else unique_name

		shutil.copy2(result.draft_dir / src_name, assembled_dir / unique_name)
		entry["oracle_file"] = unique_name
		entry["target_path"] = new_target
		manifest.append(entry)
	return manifest, assembled_dir, failures


def run_draft_oracles(
	workspace: Path, criteria_path: Path, out_dir: Path, evidence_path: Path, timeout_minutes: int,
	feedback_path: Path | None = None, criterion_timeout_minutes: int | None = None, ecosystem: str = "go",
	thinking: str | None = None, adapter=build_app.DEFAULT_ADAPTER,
) -> int:
	criteria = build_app.read_acceptance_criteria(criteria_path)
	if not criteria:
		raise ValueError(f"{criteria_path} has no criteria")
	feedback_text, feedback_truncated = read_feedback_ex(feedback_path)
	agents_md_used = build_app.committed_agents_md_blob(workspace) is not None

	per_minutes = criterion_timeout_minutes if criterion_timeout_minutes and criterion_timeout_minutes > 0 else per_criterion_minutes(timeout_minutes, len(criteria))

	started = time.monotonic()
	# One bounded pi invocation per criterion, strictly sequential (this
	# host route is single-instance): a timeout or failure on one criterion
	# only costs its own share of the budget and never loses another's. The
	# loop (not a list comprehension, found 2026-09-24) accumulates
	# prior_imports as it goes: each Python oracle already written this run
	# contributes its own `from X import Y` lines to every LATER criterion's
	# prompt, so an isolated invocation that names a function another
	# criterion already imported reuses that same module instead of
	# guessing its own (see extract_python_imports/PRIOR_IMPORTS_HEADER).
	# Go oracles don't need this: their package path is explicit in
	# target_path, never guessed.
	results: list[CriterionResult] = []
	prior_imports: list[str] = []
	for i, c in enumerate(criteria, start=1):
		r = draft_one_criterion(
			i, c, workspace, per_minutes, feedback_text, criteria, ecosystem,
			prior_imports=list(prior_imports), thinking=thinking, adapter=adapter,
		)
		results.append(r)
		if ecosystem == "python":
			oracle_file = r.entry.get("oracle_file")
			if oracle_file:
				for line in extract_python_imports(r.draft_dir / oracle_file):
					if line not in prior_imports:
						prior_imports.append(line)
	duration_s = time.monotonic() - started

	usage: dict = {}
	for r in results:
		for key, value in (r.usage or {}).items():
			usage[key] = usage.get(key, 0) + value

	manifest, assembled_dir, assembly_failures = assemble_manifest(results, workspace, ecosystem)
	manifest, dropped_count = normalize_manifest(manifest, assembled_dir, workspace)
	oracle_count = len({e["oracle_file"] for e in manifest if e.get("oracle_file")})
	any_failure = any(r.failure_reason for r in results) or bool(assembly_failures)
	salvaged_count = sum(1 for r in results if r.salvaged)

	if oracle_count and dropped_count:
		status = "over_cap"
	elif oracle_count:
		status = "drafted"
	elif any_failure:
		# Zero criteria drafted AND at least one genuinely failed (as
		# opposed to every one being a legitimate judgment-call null): the
		# same "no usable manifest" bucket the old single-invocation script
		# used exit code 2 for.
		status = "failed"
	else:
		status = "none_eligible"

	failures = sorted(
		[{"criterion_index": i, "reason": r.failure_reason} for i, r in enumerate(results, start=1) if r.failure_reason]
		+ assembly_failures,
		key=lambda f: f["criterion_index"],
	)
	exit_code = 2 if status == "failed" else 0
	write_evidence(
		evidence_path, usage=(usage or None), agent_exit_code=exit_code,
		duration_s=duration_s, agents_md_used=agents_md_used,
		criteria_count=len(criteria), oracle_count=(None if status == "failed" else oracle_count),
		status=status, dropped_count=dropped_count, feedback_truncated=feedback_truncated,
		failure_reason=(failures[0]["reason"] if status == "failed" and failures else None),
		failures=failures, salvaged_count=salvaged_count, thinking=thinking,
	)

	if failures:
		# Per-criterion diagnostics on stderr for the driver's own log
		# capture -- every failed/timed-out criterion is reported
		# individually, reusing report_failure's own sanitisation/bounding
		# for each one's pi output, not just a single summary line.
		for i, r in enumerate(results, start=1):
			if r.failure_reason:
				print(f"draft_acceptance_oracles: criterion {i} not drafted: {_bounded_reason(r.failure_reason)}", file=sys.stderr)
				report_failure(r.failure_reason, r.result, r.draft_dir)
		for f in assembly_failures:
			# No new pi output to report here -- the criterion's own
			# invocation succeeded; assembly itself dropped the result.
			print(f"draft_acceptance_oracles: criterion {f['criterion_index']} not drafted: {_bounded_reason(f['reason'])}", file=sys.stderr)

	if status == "failed":
		return 2

	out_dir.mkdir(parents=True, exist_ok=True)
	for entry in manifest:
		oracle_file = entry.get("oracle_file")
		if oracle_file:
			# oracle_file is already validated as a plain, single-component
			# filename with no path separators (validate_manifest, applied
			# per criterion, plus normalize_manifest's own cap checks), so
			# this join can never escape assembled_dir/out_dir. shutil.copy2,
			# not a read_text/write_text round trip: a plain byte-level copy
			# can't mis-decode/mis-encode an oracle file whose content
			# happens not to be valid UTF-8, and it preserves mtime/perms.
			shutil.copy2(assembled_dir / oracle_file, out_dir / oracle_file)
	(out_dir / MANIFEST_FILENAME).write_text(json.dumps(manifest, indent=2) + "\n", encoding="utf-8")
	return 0


def main() -> int:
	parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
	parser.add_argument("--criteria", required=True, type=Path, help="path to the flat, one-per-line acceptance-criteria file (same shape as build_app.py's own --spec-acceptance-criteria)")
	parser.add_argument("--workspace", required=True, type=Path, help="repository checkout the oracles are drafted against, read from at HEAD")
	parser.add_argument("--out-dir", required=True, type=Path, help="directory the drafted MANIFEST.json and any oracle files are copied into on success")
	parser.add_argument("--evidence", required=True, type=Path, help="where usage/exit-code/duration/manifest-summary evidence JSON is always written, success or failure")
	parser.add_argument("--timeout-minutes", type=int, default=15, help="overall wall-clock budget, split evenly across criteria (each gets its own pi invocation) unless --criterion-timeout-minutes overrides the split")
	parser.add_argument("--criterion-timeout-minutes", type=int, default=None, help="wall-clock budget for EACH criterion's own pi invocation, overriding the --timeout-minutes split; cmd/factoryd's own oracle-drafting job always passes this explicitly, and sizes its own container's deadline to outlive this total budget by a fixed margin plus slack, not to match it exactly (see per_criterion_minutes)")
	parser.add_argument(
		"--thinking", choices=build_app.THINKING_LEVELS, default=None,
		help="Pi thinking level for this job; omitted inherits Pi's installed setting.",
	)
	parser.add_argument(
		"--harness", choices=sorted(harness_adapters.ADAPTERS), default="pi",
		help="Coding agent to drive (see harness_adapters.py).",
	)
	parser.add_argument("--feedback", type=Path, default=None, help="optional operator-feedback text file on a previous draft; appended to the prompt as guidance only (a missing or empty file is ignored)")
	parser.add_argument("--ecosystem", choices=sorted(DRAFT_INSTRUCTIONS_BY_ECOSYSTEM), default="go", help="which language's drafting instructions to send the model; cmd/factoryd's own oracle-drafting job always passes this explicitly (classifyDraftEcosystem), so a human running this script by hand gets the historic Go default")
	args = parser.parse_args()

	adapter = harness_adapters.get(args.harness)
	adapter.prepare()
	workspace = args.workspace.resolve()
	return run_draft_oracles(
		workspace, args.criteria.resolve(), args.out_dir, args.evidence, args.timeout_minutes,
		feedback_path=args.feedback, criterion_timeout_minutes=args.criterion_timeout_minutes,
		ecosystem=args.ecosystem, thinking=args.thinking, adapter=adapter,
	)


if __name__ == "__main__":
	sys.exit(main())
