#!/usr/bin/env python3
"""Outermost loop of the zero-human pipeline: one invocation drives
`/spec-plan`, a human checkpoint, `/contract-plan`, another checkpoint,
`ticket_runner.py`'s build loop, and halt/rescue handling, end to end.

This is `pi.dev`-native tooling that drives the local model via `pi`,
not a Claude-authoring skill. It reuses
`build_app.py`'s headless `pi` invocation builder and `ticket_runner.py`'s
robust subprocess/timeout wrapper and gate/halt-record helpers directly
(imported as sibling modules) rather than re-deriving any of it.

Usage:
    python3 goal_pilot.py --spec-input <path-or-text> --pilot-dir <dir>
        [--checkpoint review|skip]      (default: skip)
        [--on-halt report|auto-rescue]  (default: auto-rescue)
        [--review-policy advisory|required|degraded]  (default: advisory)

Both `--checkpoint` and `--on-halt` are always echoed back before anything
runs, whether passed explicitly or defaulted, so a resumed run and a fresh
run both make the active mode explicit. The spec-freeze checkpoint (step
3) and the post-ticket-001 checkpoint (step 5b) are outside both flags and
are never skippable -- see their docstrings for why.

Never escalates to cloud itself: every judgment step here shells out to
`pi` via harness_adapters.get("pi").invocation(), and the one rescue class that touches
implementation (`--on-halt=auto-rescue`, genuine no-progress halts) widens
`build_app.py`'s own round/timeout/thinking budget on that same route
rather than requesting a cloud provider/model. This holds even under
`auto-rescue` -- see `handle_implementation_gap_halt()`. Not itself a
zero-cloud-usage guarantee (found via a real GitHub Codex App review of
this PR, see write_verdict()'s own doc comment): pi_invocation() only
passes --provider/--model when PI_HARNESS_PROVIDER/PI_HARNESS_MODEL are
explicitly set, so every invocation here inherits whatever pi's own
configured default route already is when they aren't -- this script has
no visibility into whether that default is local or cloud.
"""

from __future__ import annotations

import argparse
import ast
import errno
import json
import os
import re
import sys
import time
from datetime import datetime, timezone
from pathlib import Path

SCRIPT_DIR = Path(__file__).resolve().parent
if str(SCRIPT_DIR) not in sys.path:
	sys.path.insert(0, str(SCRIPT_DIR))
import build_app  # noqa: E402  (sibling module -- verification and report helpers)
import harness_adapters  # noqa: E402  (sibling module -- pi invocation + JSONL parsing)
import ticket_runner  # noqa: E402  (sibling module -- gate/halt-record helpers, the build loop itself)

TICKET_RUNNER = SCRIPT_DIR / "ticket_runner.py"

DEFAULT_CHECKPOINT = "skip"
DEFAULT_ON_HALT = "auto-rescue"
DEFAULT_REVIEW_POLICY = ticket_runner.DEFAULT_REVIEW_POLICY  # "advisory"

# Headless pi invocations for /spec-plan and /contract-plan are judgment
# work, not the tight per-ticket implementation loop -- generous but
# bounded, so a genuinely stuck local model still halts instead of hanging
# the whole pilot forever.
SPEC_PLAN_TIMEOUT_S = 45 * 60
CONTRACT_PLAN_TIMEOUT_S = 60 * 60
# The overall ticket_runner.py subprocess's own worst-case bound is 15
# tickets x 3 build attempts x 90 minutes each (ticket_runner.py's
# BUILD_APP_TIMEOUT_S/MAX_BUILD_ATTEMPTS) = ~67 hours if every attempt of
# every ticket maxed its timeout, which real halts avoid (they stop well
# short via retryable_build_state()/append_halt_record() long before that).
# This is a backstop against a genuinely hung subprocess, generous enough
# not to fire during a real, if slow, run.
TICKET_RUNNER_TIMEOUT_S = 72 * 60 * 60

# Step 7, class 3 (genuine implementation gap): the bounded, single widened
# retry. build_app.py's own installed defaults are max-rounds=3,
# timeout-minutes=45 (its own --timeout-minutes default) and whatever
# settings.json's thinking policy is (currently medium, per pi/README.md);
# ticket_runner.py's builder_command() additionally fixes max-rounds=3,
# timeout-minutes=60. Widened here means visibly more of each, plus
# `--thinking xhigh` (user decision, 2026-08-21) -- still zero cloud
# tokens, still local, still bounded to exactly one extra attempt.
WIDENED_MAX_ROUNDS = 6
WIDENED_TIMEOUT_MINUTES = 90
WIDENED_THINKING = "xhigh"

# Step 7, class 1: on an infra halt, retry once against an alternate model
# host the operator names in AI_STACK_HOST_FALLBACK (e.g. the Tailscale
# name of the machine that actually serves the model when a LAN name
# stopped resolving -- the case this pilot's own history hit). No
# built-in default: a deployment-specific hostname does not belong in
# the source, and with none set the infra halt is simply reported.
AI_STACK_HOST_FALLBACK = os.environ.get("AI_STACK_HOST_FALLBACK", "")
MAX_INFRA_AUTO_RETRIES = 2

SPEC_STATUS_RE = re.compile(r"^STATUS:\s*(\S+)")


# --------------------------------------------------------------------------
# Disk-state helpers -- resume derives its position from these, not from
# any state goal_pilot.py itself remembers in memory (step 9).
# --------------------------------------------------------------------------


def spec_path_for(pilot_dir: Path) -> Path:
	return pilot_dir / "spec" / "spec.md"


def read_spec_status(pilot_dir: Path) -> str | None:
	path = spec_path_for(pilot_dir)
	if not path.exists():
		return None
	first_line = path.read_text(errors="ignore").splitlines()[:1]
	if not first_line:
		return None
	m = SPEC_STATUS_RE.match(first_line[0])
	return m.group(1) if m else None


def freeze_spec(pilot_dir: Path) -> None:
	path = spec_path_for(pilot_dir)
	lines = path.read_text().splitlines(keepends=True)
	if not lines:
		raise ValueError(f"{path} is empty -- cannot freeze")
	stamp = datetime.now(timezone.utc).isoformat()
	lines[0] = f"STATUS: FROZEN -- reviewed {stamp}\n"
	path.write_text("".join(lines))


def compile_complete_marker(pilot_dir: Path) -> Path:
	return pilot_dir / "spec" / ".compile-complete"


def is_compile_complete(pilot_dir: Path) -> bool:
	# Deliberately not `contract.md`'s mere existence -- a crash mid-
	# /contract-plan (after contract.md and some slices are written but
	# before the self-check finishes) must not read back as "step 4 done"
	# on resume (Codex review of PR #36). This marker is written only after
	# write_compile_complete_marker() below runs, which happens only once
	# the self-check output has actually been captured.
	return compile_complete_marker(pilot_dir).exists()


def write_compile_complete_marker(pilot_dir: Path, staged_ticket_count: int) -> None:
	compile_complete_marker(pilot_dir).write_text(
		json.dumps(
			{
				"completed": datetime.now(timezone.utc).isoformat(),
				"staged_ticket_count": staged_ticket_count,
			},
			indent=2,
		)
	)


def checkpoint5_ack_marker(pilot_dir: Path) -> Path:
	return pilot_dir / "spec" / ".checkpoint5-ack"


def checkpoint5b_ack_marker(pilot_dir: Path) -> Path:
	return pilot_dir / "spec" / ".checkpoint5b-ack"


# --------------------------------------------------------------------------
# Evidence trail -- goal_pilot.py's own actions get the same durable,
# machine-readable trail ticket_runner.py already gives per-ticket work, so
# a rescued/widened run doesn't repeat the tickets-013-016 reporting gap
# (parent plan) where cloud/human-touched work left no trace and was later
# misread as something else.
# --------------------------------------------------------------------------


def logs_dir(pilot_dir: Path) -> Path:
	d = pilot_dir / "logs"
	d.mkdir(parents=True, exist_ok=True)
	return d


def execution_log_path(pilot_dir: Path) -> Path:
	return pilot_dir / "EXECUTION_LOG.md"


def append_execution_log(pilot_dir: Path, heading: str, body: str = "") -> None:
	stamp = datetime.now(timezone.utc).isoformat()
	path = execution_log_path(pilot_dir)
	existing = path.read_text(errors="ignore") if path.exists() else "# goal_pilot.py execution log\n"
	block = f"\n\n## {heading} ({stamp})\n"
	if body:
		block += f"\n{body}\n"
	path.write_text(existing + block)


def rescue_log_path(pilot_dir: Path) -> Path:
	return pilot_dir / ".goal-pilot" / "rescues.jsonl"


def append_rescue_record(pilot_dir: Path, record: dict) -> None:
	path = rescue_log_path(pilot_dir)
	path.parent.mkdir(parents=True, exist_ok=True)
	record = {**record, "logged": datetime.now(timezone.utc).isoformat()}
	with path.open("a") as handle:
		handle.write(json.dumps(record) + "\n")
	append_execution_log(
		pilot_dir,
		f"RESCUE: ticket {record.get('ticket', '?')} :: {record.get('class', 'unknown')}",
		json.dumps(record, indent=2),
	)


def read_rescue_records(pilot_dir: Path) -> list[dict]:
	path = rescue_log_path(pilot_dir)
	if not path.exists():
		return []
	records = []
	for line in path.read_text(errors="ignore").splitlines():
		line = line.strip()
		if not line:
			continue
		try:
			records.append(json.loads(line))
		except json.JSONDecodeError:
			continue
	return records


# --------------------------------------------------------------------------
# Headless pi invocation -- reuses harness_adapters.py's PiAdapter.invocation() command
# builder and ticket_runner.py's invoke_build_app() subprocess wrapper.
# invoke_build_app() is generic despite its name (it just runs a command
# list under a timeout with robust process-group kill semantics on
# timeout) -- there is no reason to duplicate that logic for a plain `pi`
# invocation instead of a `build_app.py` one.
# --------------------------------------------------------------------------


def run_pi_prompt(pilot_dir: Path, prompt: str, *, session_dir: Path, timeout_s: float) -> tuple[bool, str, dict]:
	adapter = harness_adapters.get("pi")
	command = adapter.invocation(
		pilot_dir,
		prompt=prompt,
		session_dir=session_dir,
		continue_session=False,
		thinking=None,
	)
	# invoke_build_app() (reused from ticket_runner.py) launches `pi` via a
	# bare subprocess.Popen with no explicit `env=`, so it inherits whatever
	# this process's AI_STACK_HOST currently is -- default it here the same
	# way build_app.py's own run_build() does, rather than assuming the
	# caller's shell already exported one.
	os.environ.setdefault("AI_STACK_HOST", "127.0.0.1")
	# `pi` has no --cwd flag: its bash tool operates directly on this
	# process's own working directory, which subprocess.Popen otherwise
	# inherits from whatever directory goal_pilot.py itself happened to be
	# launched from -- silently the caller's own dev checkout, not the pilot
	# dir, if goal_pilot.py was run from inside one (reproduced live: the
	# model's bash exploration during a /contract-plan self-check wandered
	# into agent-configs' own unrelated test output instead of staying
	# scoped to the pilot). Pin it explicitly instead of trusting the
	# caller's shell to already be in the right place.
	returncode, stdout, stderr, timed_out = ticket_runner.invoke_build_app(command, timeout_s, cwd=pilot_dir)
	errored, total = adapter.parse(stdout).turn_errors
	# `pi` exits 0 and can still leave a real artifact (e.g. a resumed
	# /contract-plan run's already-partial contract.md) on disk even when
	# the model route was unreachable for the entire invocation -- every
	# assistant turn errored with stopReason: error, no real model work
	# happened (build_app.py's own round_blockers() treats this the same
	# way, as "model route unreachable", never as a completed round).
	# Without this check, step4_compile() would accept a stale/partial
	# artifact and write .compile-complete for a self-check that never
	# actually ran (Codex review of PR #37).
	fully_errored = total > 0 and errored == total
	ok = returncode == 0 and not timed_out and not fully_errored
	diagnostics = {
		"returncode": returncode,
		"timed_out": timed_out,
		"turn_errors": [errored, total],
		"fully_errored": fully_errored,
		"stderr_tail": stderr[-2000:],
	}
	return ok, stdout, diagnostics


# --------------------------------------------------------------------------
# Step 1/2: intake + draft spec/tickets via /spec-plan
# --------------------------------------------------------------------------


def goal_pilot_session_root(pilot_dir: Path) -> Path:
	"""Sibling of `pilot_dir`, not a path inside it -- see step2_draft_spec()'s
	comment for why: `pi --session-dir` creates its directory before the
	model's first turn, which would otherwise poison /spec-plan's own
	step-0 "is this dir empty" scaffold check on every fresh pilot_dir."""
	root = pilot_dir.parent / f".{pilot_dir.name}.goal-pilot-sessions"
	root.mkdir(parents=True, exist_ok=True)
	return root


def resolve_spec_input(raw: str, pilot_dir: Path) -> Path:
	"""If `raw` is an existing file, use it as-is. Otherwise treat it as
	literal rough-input text and write it to a scratch file -- /spec-plan's
	own argument-hint expects a path, and every downstream re-invocation on
	resume should read the same frozen input.

	The scratch file is a `pilot_dir` *sibling*, not something written
	inside it (Codex review of PR #37): `/spec-plan`'s own step-0 scaffold
	check refuses to run against any existing, non-empty directory that
	has no `Makefile` yet, so writing anything into an unscaffolded
	pilot_dir before that check runs would make every fresh run using
	literal --spec-input text immediately unscaffoldable."""
	candidate = Path(raw).expanduser()
	if candidate.is_file():
		return candidate.resolve()
	scratch = pilot_dir.parent / f".{pilot_dir.name}.goal-pilot-raw-spec-input.txt"
	scratch.parent.mkdir(parents=True, exist_ok=True)
	if not scratch.exists():
		scratch.write_text(raw)
	return scratch


# --------------------------------------------------------------------------
# internal/ticketspec key injection -- bridges gap 1 of buildgate's
# lights-off-agent-factory-plan.md (2026-08-30 entries have the full
# account). /spec-plan's generated tickets satisfy buildgate's
# policy.TicketStructure (## Goal/## Required changes/## Verification/
# ## Commit) but never declared the separate Verify-Command:/
# Allowed-Files:/Required-Changed-Files: keys internal/ticketspec parses
# -- the format policy.EvaluateRun's real per-run gates (diff_scope,
# canonical_verify, required_files_changed, required_content_present)
# actually read. A generated ticket could pass every structural check
# and still be useless to the gates that decide accept/quarantine.
#
# This is deliberately NOT a prompt instruction to the model (spec-plan.md
# does still ask for it too, as a harmless belt-and-braces attempt for a
# different/future model) -- three real validation runs against this
# pipeline's own local Qwen3.8-27B route showed the model reliably names
# correct file paths in every generated ## Required changes list, but
# does not reliably restate them in the new key format regardless of how
# firmly the prompt asks or whether a worked example is included. The
# paths are already right; only a mechanical restatement was missing, so
# that restatement belongs in code, not in a request to the model --
# matching this whole ecosystem's own standing principle that a judgment
# a model won't reliably repeat belongs behind a real check.
# --------------------------------------------------------------------------

# spec/contract.md and spec/spec.md are this pipeline's own frozen,
# read-only reference documents -- every ticket after 001 names them in
# its own context line ("Read ARCHITECTURE.md, PROGRESS.md, and
# spec/contract.md before changing anything"), and `## Required changes`
# prose routinely cites them again ("...per `spec/contract.md`") purely
# as a citation, never as a file this ticket edits. Found live (2026-08-30
# end-to-end validation run): without this exclusion, a ticket whose
# `## Required changes` cited `spec/contract.md` for its expected error
# text got it listed in `Allowed-Files:`/`Required-Changed-Files:` as if
# this ticket were supposed to modify the frozen contract itself.
#
# ARCHITECTURE.md/PROGRESS.md/README.md deliberately do NOT belong here,
# despite an earlier version of this set including all three. Found via
# review: that was a real, more severe bug than the one it looked like it
# fixed -- buildgate's own harnessByproducts exemption list
# (internal/policy/policy.go) is only [".gitignore", "BUILD_REPORT.md",
# "BUILD_EVIDENCE.json", ".pi-build-session/"]; ARCHITECTURE.md and
# PROGRESS.md are NOT in it. Every ticket after 001 ends its own `##
# Required changes` with the mandatory, verbatim instruction "Update
# `ARCHITECTURE.md` and append a `PROGRESS.md` entry before finishing" --
# a real, required edit `diff_scope` genuinely checks for -- so excluding
# them here meant every single generated ticket's own injected
# `Allowed-Files:` omitted two files its own agent is instructed to
# change, quarantining every one of them on diff_scope in practice.
# README.md was excluded on the same (wrong) assumption; unlike the
# other two it isn't even universally edited, so a ticket that
# explicitly names it as a real required change (e.g. "update README.md
# to document the new flag") had it silently stripped from scope for no
# reason at all.
TICKETSPEC_EXCLUDED_FILES = frozenset({"spec/contract.md", "spec/spec.md"})
BACKTICK_SPAN_RE = re.compile(r"`([^`\s]+)`")
# A file's real extension never starts with a digit -- a version number or
# numeric example (`3.5`, illustrating a non-integer CLI argument) does.
# This is deliberately a shape check, not a fixed allowlist of known
# extensions: an allowlist silently misses a real extension it didn't
# happen to name (found via review: `schema.graphql`), while this still
# correctly excludes `3.5` the same way the allowlist version did.
#
# Lowercase-only (found via a real brownfield /spec-plan run against
# example-app, 2026-09-10): a ticket's own "## Required changes"
# prose routinely backtick-quotes a qualified Go identifier it is adding
# alongside the real file, e.g. `domain.ErrNoteTitleTooLong` -- the old
# case-insensitive class treated "ErrNoteTitleTooLong" as a plausible
# extension and extracted the whole identifier as a required-changed
# path. No real file extension in this repo's own conventions (or any
# extension named in this function's own test suite -- .go, .py, .md,
# .graphql, .yml, ...) is anything but lowercase, so requiring that
# excludes every CamelCase/PascalCase identifier shape without needing a
# fixed extension allowlist. An unsatisfiable declaration like this one
# quarantines an otherwise-correct run forever (required_files_changed
# can never be satisfied by a path that can't appear in a real diff) --
# the same failure class this function's own extraction rules already
# guard against for routes/URLs/numeric examples.
FILE_EXTENSION_RE = re.compile(r"\.[a-z][a-z0-9+]*$")
# Known residual, not closed here (found via adversarial review,
# 2026-09-11): a lowercase two-segment reference with no CamelCase at all
# -- `resp.body`, `cfg.timeout` -- still matches this regex and can still
# be misextracted the same way the CamelCase case was. A length or
# shape-based guard to exclude it was considered and rejected: real short
# lowercase filenames (`app.py`, `csv.py`) are indistinguishable from a
# short receiver-name reference by shape alone, so closing this would
# trade the CamelCase false-positive (confirmed, hit in real validation)
# for a new false-negative on real short files (not yet confirmed to
# occur, but the exact class this module's own established shape-over-
# allowlist design already accepts as a tradeoff -- see `schema.graphql`
# above). Left open rather than force a heuristic with symmetric,
# unverified risk in the other direction.
# Mixed-case conventional filenames with no extension at all -- tools that
# name their own config file this way use TitleCase, not SCREAMING_CASE,
# so the shape rule below (ALL_CAPS_FILENAME_RE) doesn't cover them.
EXTENSIONLESS_FILENAMES = frozenset(
	{"Makefile", "Dockerfile", "Rakefile", "Gemfile", "Procfile", "Vagrantfile", "Justfile", "Podfile", "Brewfile", "Guardfile"}
)
# A SCREAMING_CASE bare filename (WORKSPACE, BUILD, CODEOWNERS, LICENSE,
# NOTICE, AUTHORS, ...) is a real, established convention (Bazel build
# files, repo metadata) that a fixed name list can never enumerate
# exhaustively (found via review). What actually distinguishes these
# from a bare non-file token in the same prose -- a CLI-argument or
# error-message example like `abc`, `3.5`, `-4` -- is exactly this shape:
# those are never SCREAMING_CASE. A 2-letter minimum avoids matching
# single/double-letter placeholders (`N`, `OK`) that read more like prose
# than a filename.
ALL_CAPS_FILENAME_RE = re.compile(r"^[A-Z][A-Z0-9_]{2,}$")
TICKET_NUMBER_RE = re.compile(r"^(\d+)-")


def is_valid_workspace_relative_path(candidate: str) -> bool:
	"""Mirrors `buildgate`'s own `internal/ticketspec.
	isValidWorkspaceRelativePath` exactly (see that function's doc
	comment for the full reasoning): a path that can never match a real
	workspace-relative git diff path -- absolute, containing a `..`/`.`
	segment, or an internal empty segment from a doubled `//` -- would
	make the injected declaration itself unsatisfiable, quarantining an
	otherwise-correct run forever (`required_files_changed` can never be
	satisfied by a path that can't appear in a real diff). Also rejects a
	URL (`://`) -- `internal/ticketspec`'s own check doesn't need this,
	since a ticket author would never hand-write one, but a naive
	backtick-span extractor can pick one up straight from prose (found
	via review: `` `https://example.com/schema` ``)."""
	if not candidate or candidate.startswith("/") or "://" in candidate:
		return False
	segments = candidate.split("/")
	for i, seg in enumerate(segments):
		if seg in ("..", "."):
			return False
		if seg == "" and i != len(segments) - 1:
			return False
	return True


def looks_like_file_path(candidate: str) -> bool:
	"""True for a backtick span from a ticket's own text that plausibly
	names a real file this ticket touches, not prose (a shell command, a
	commit-message template, a numeric or CLI-argument example, a
	route/URL/directory reference, a reference document already excluded
	above) that happens to also be backtick-quoted."""
	if candidate in TICKETSPEC_EXCLUDED_FILES:
		return False
	if candidate.startswith(("ticket(", "STATUS:")) or " " in candidate:
		return False
	if not is_valid_workspace_relative_path(candidate):
		return False
	basename = candidate.rsplit("/", 1)[-1]
	return (
		bool(FILE_EXTENSION_RE.search(candidate))
		or basename in EXTENSIONLESS_FILENAMES
		or bool(ALL_CAPS_FILENAME_RE.match(basename))
	)


def extract_markdown_section(content: str, heading: str) -> tuple[str, int, int]:
	"""Returns (text, start_line, end_line) for the body of a `## <heading>`
	section -- from the line after that heading up to (not including) the
	next `## ` heading, or end of file. (text, -1, -1) if the heading isn't
	found. Line indices are into content.splitlines(keepends=True), so a
	caller can both read the section and later splice content back in at
	its boundary without re-scanning.
	"""
	lines = content.splitlines(keepends=True)
	start = None
	for i, line in enumerate(lines):
		if line.strip() == f"## {heading}":
			start = i + 1
			break
	if start is None:
		return "", -1, -1
	end = len(lines)
	for j in range(start, len(lines)):
		if lines[j].startswith("## "):
			end = j
			break
	return "".join(lines[start:end]), start, end


def extract_required_change_paths(ticket_content: str) -> list[str]:
	"""The file paths this ticket's own `## Required changes` list already
	names -- the one part of this whole exercise that real validation
	showed the model reliably gets right on its own. Order-preserving,
	de-duplicated."""
	section_text, _, _ = extract_markdown_section(ticket_content, "Required changes")
	paths: list[str] = []
	seen: set[str] = set()
	for candidate in BACKTICK_SPAN_RE.findall(section_text):
		if looks_like_file_path(candidate) and candidate not in seen:
			seen.add(candidate)
			paths.append(candidate)
	return paths


def verify_command_for_ticket(ticket_content: str, verify_command: str | None = None) -> str:
	"""`ticket_runner.py`'s own gate (`run_ticket`) requires BOTH `make
	verify` and `make verify-full` to pass, and every generated ticket's
	own `## Verification` prose already promises both ("`make verify`
	must pass. Confirm `make verify-full` still passes."). Found via
	review: declaring only `make verify` here reports `factoryd`'s
	canonical_verify as passing even when `make verify-full` alone would
	fail -- a materially weaker bar than what this same ticket already
	requires of `ticket_runner.py`'s own gate. Join both with `&&` so
	they decide `Verify-Command` the same way they decide the ticket's
	own pass/fail; `sh -c` (how `canonical_verify` invokes this) accepts
	`&&` directly, no ticket seen in real validation runs against a
	scaffolded-by-this-pipeline app needed a different command.

	`verify_command`, when given, overrides that hardcoded default
	entirely (found via a real brownfield /spec-plan run against
	example-app, 2026-09-10): `make verify && make verify-full`
	assumes the target repo's own Makefile defines both targets, which
	only holds for a repo this pipeline itself scaffolded. An existing
	repo being onboarded (`factoryd onboard`/`-preflight-profile
	brownfield`) has no reason to define a `verify-full` target at all --
	the caller already knows the repo's real verify command (from
	`.factory.yml`'s `verify_command`, or an operator-supplied
	`goal_pilot.py --verify-command`), and declaring a command that can
	never pass would permanently quarantine an otherwise-correct ticket,
	the same failure class every other guard in this module exists to
	prevent.

	An empty string is treated the same as None (falls back to the
	hardcoded default), not a distinct "explicitly opt out" signal
	(considered via adversarial review, 2026-09-11, and kept as-is): this
	module's own CLI product path (intake.go) already guards its call
	site and never passes an empty string through at all, only omitting
	the flag entirely when unset, so the ambiguity cannot reach here via
	the actual product surface -- and treating empty-as-unset matches
	this same codebase's existing convention elsewhere."""
	del ticket_content  # not yet needed; kept as a parameter for a future ticket that legitimately needs a different command
	return verify_command if verify_command else "make verify && make verify-full"


VERIFY_COMMAND_LINE_RE = re.compile(r"^Verify-Command:[ \t]*(.*)$", re.MULTILINE)


def replace_stale_verify_command_line(content: str, verify_command: str) -> str:
	"""Strips an already-declared `Verify-Command: <value>` line whose
	value differs from `verify_command`, so `inject_ticketspec_keys` (the
	caller) re-injects the override fresh instead of leaving a stale line
	alone -- see that function's own doc comment for why. A no-op when no
	such line exists, or its value already matches (idempotent: a second
	call with the same override changes nothing)."""
	match = VERIFY_COMMAND_LINE_RE.search(content)
	if match is None or match.group(1) == verify_command:
		return content
	line_start, line_end = match.span()
	# Consume the line's own trailing newline too (not just the matched
	# text), or the injection below would land the fresh replacement after
	# a now-blank line instead of exactly where the stale one was.
	if line_end < len(content) and content[line_end] == "\n":
		line_end += 1
	return content[:line_start] + content[line_end:]


# Prefix, not the trailing colon+value: matching just the key name is
# what lets a partially-declared ticket (found via review -- a hand-edit
# or a future caller that adds `Verify-Command:` on its own but not the
# other two) be detected as such per-key, not lumped into "already fully
# declared" by the presence of any one of the three.
TICKETSPEC_KEY_PREFIXES = ("Verify-Command:", "Allowed-Files:", "Required-Changed-Files:")

# Every ticket's mandatory closing instruction ("Update `ARCHITECTURE.md`
# and append a `PROGRESS.md` entry before finishing") always contributes
# these two paths to extract_required_change_paths, regardless of whether
# the ticket's own "## Required changes" list names any real implementation
# path at all. inject_ticketspec_keys' "nothing concrete to declare, skip"
# fallback therefore never actually triggers on `not paths` alone -- a
# ticket describing its changes by package/endpoint/directory name rather
# than a literal backtick-quoted file path (calc-app tickets 002-003,
# 2026-09-05; notes-demo tickets 002-005, 2026-09-06 -- both needed hand
# correction after the fact) gets Allowed-Files: ARCHITECTURE.md,
# PROGRESS.md injected anyway, which then quarantines any correct
# implementation the moment it touches real source files. Used below to
# extend the skip to "found nothing beyond the boilerplate two" as well --
# not to exclude these two paths from extraction generally: a ticket that
# *also* names real paths must still declare them (see
# ExtractRequiredChangePathsTests.test_finds_real_paths_including_the_
# mandatory_state_files -- excluding them outright was tried and reverted
# as a real P1 review finding, since it silently dropped ARCHITECTURE.md/
# PROGRESS.md from Allowed-Files even when the ticket legitimately touches
# both, quarantining on diff_scope instead).
MANDATORY_CLOSING_PATHS = frozenset({"ARCHITECTURE.md", "PROGRESS.md"})


def inject_ticketspec_keys(pilot_dir: Path, verify_command: str | None = None) -> list[Path]:
	"""Adds whichever of Verify-Command:/Allowed-Files:/
	Required-Changed-Files: a ticket is missing to the end of its `##
	Verification` section, except: ticket 001 (the walking-skeleton
	exemption -- see spec-plan.md's own doc comment: a brand-new repo has
	no real paths yet to name honestly), a ticket that already declares
	all three (hand-edited, or a prior run of this same function --
	idempotent, never double-injects), and a ticket whose `## Required
	changes` names no extractable path at all (nothing concrete to
	declare; leaving it undeclared skips diff_scope/required_files_changed
	cleanly rather than risk a wrong, guessed declaration causing a false
	quarantine later).

	Checks each key independently (found via review): a ticket declaring
	only `Verify-Command:` by hand used to be treated as "already fully
	declared" and skipped entirely, silently leaving Allowed-Files:/
	Required-Changed-Files: undeclared -- the scope gates skip cleanly on
	an absent declaration, but that's not what a caller who added one key
	by hand was asking for.

	`verify_command`, when given, is passed straight through to
	`verify_command_for_ticket` in place of its own hardcoded `make
	verify && make verify-full` default -- see that function's own doc
	comment for why an existing (brownfield-onboarded) repo needs this.

	An explicit `verify_command` also REPLACES an already-declared
	Verify-Command: line whose value differs, rather than leaving it
	alone (found via a real brownfield /spec-plan run against
	example-app, 2026-09-10/11): this function's own "already
	declares all three, skip" rule otherwise makes `--inject-only
	<dir> --verify-command ...` -- the documented remediation for a
	ticket that already has the unsatisfiable hardcoded default baked
	in -- a complete no-op on exactly the ticket it exists to fix,
	since that ticket already has SOME Verify-Command: line. Every
	other declared key (Allowed-Files:/Required-Changed-Files:) is left
	alone regardless -- only Verify-Command: has a caller-supplied
	override to reconcile against.

	Returns the ticket paths actually modified, for the caller to log.
	"""
	tickets_dir = pilot_dir / "spec" / "tickets"
	if not tickets_dir.exists():
		return []
	modified: list[Path] = []
	for ticket_path in sorted(tickets_dir.glob("*.md")):
		content = ticket_path.read_text()
		if verify_command:
			content = replace_stale_verify_command_line(content, verify_command)
		missing_prefixes = [p for p in TICKETSPEC_KEY_PREFIXES if p not in content]
		if not missing_prefixes:
			continue
		number_match = TICKET_NUMBER_RE.match(ticket_path.name)
		if number_match and int(number_match.group(1)) == 1:
			continue
		paths = extract_required_change_paths(content)
		# Verify-Command: doesn't depend on knowing any file path -- only
		# Allowed-Files:/Required-Changed-Files: do. A ticket with nothing
		# concrete to declare for those two must still get Verify-Command:
		# injected if it's missing (found via Codex review of this same
		# PR): omitting it lets a direct `factoryd` run skip canonical_verify
		# entirely, accepting code that fails `make verify`/`make
		# verify-full`.
		keys_to_inject = list(missing_prefixes)
		if not paths or set(paths) <= MANDATORY_CLOSING_PATHS:
			keys_to_inject = [p for p in keys_to_inject if p == "Verify-Command:"]
		if not keys_to_inject:
			continue
		_, _, verification_end = extract_markdown_section(content, "Verification")
		if verification_end < 0:
			continue  # no ## Verification heading at all -- TicketStructure will already reject this ticket on its own terms
		joined = ", ".join(paths)
		candidate_lines = {
			"Verify-Command:": f"Verify-Command: {verify_command_for_ticket(content, verify_command)}",
			"Allowed-Files:": f"Allowed-Files: {joined}",
			"Required-Changed-Files:": f"Required-Changed-Files: {joined}",
		}
		new_lines = [candidate_lines[p] for p in TICKETSPEC_KEY_PREFIXES if p in keys_to_inject]
		declaration = "\n" + "\n".join(new_lines) + "\n\n"
		lines = content.splitlines(keepends=True)
		updated = "".join(lines[:verification_end]) + declaration + "".join(lines[verification_end:])
		ticket_path.write_text(updated)
		modified.append(ticket_path)
	return modified


# --------------------------------------------------------------------------
# ARCHITECTURE.md bootstrap -- closes a second, distinct bridging gap found
# via review (2026-08-30 end-to-end validation): buildgate's own
# `factoryd init` scaffolds ARCHITECTURE.md as a structural stub precisely
# so a new project has something for the mandatory project-bootstrap
# preflight to validate before ticket 001 ever runs (ticket 001 is what
# creates ARCHITECTURE.md's *real* content -- the preflight can't wait for
# that without a chicken-and-egg problem). But `factoryd init` refuses to
# write any of its three files if even one already exists, and /spec-plan's
# own step 1 always writes spec/spec.md for real content immediately after
# its own step-0 scaffold -- so there is no point in this pipeline's own
# flow where `factoryd init` could run and still find spec/spec.md absent,
# short of invoking it as a completely separate step no one currently
# does. Rather than couple goal_pilot.py to the buildgate Go binary
# being installed at all (this pipeline's own stated design is zero
# dependencies beyond `pi` itself), this writes the same structural stub
# directly, in Python, once /spec-plan's own tickets already exist.
# --------------------------------------------------------------------------


def write_architecture_stub(pilot_dir: Path) -> bool:
	"""Writes `<pilot_dir>/ARCHITECTURE.md` with the three headings, in the
	required order, that buildgate's `policy.ArchitectureStructure`
	checks for (`Repo layout`, `Verification`, `Known deviations`) -- the
	exact same stub shape `factoryd init` itself writes, so a project this
	pipeline drives passes `factoryd check-project`'s mandatory preflight
	before ticket 001 ever runs, the same way an operator-run `factoryd
	init` would have provided for a hand-authored project. Never
	overwrites an existing ARCHITECTURE.md (ticket 001's own real content,
	or a prior run of this same function) -- returns False in that case,
	True if it wrote the stub.

	Creates the file with `os.open(..., O_CREAT | O_EXCL | O_NOFOLLOW)`
	rather than a `Path.exists()` (or `os.path.lexists()`) check followed
	by a separate `write_text()`: a check-then-write pair leaves a race
	between the check and the write where another process could create,
	replace, or plant a symlink at this path in between, and a plain
	`write_text()` follows a symlink -- writing the stub to whatever it
	points at, outside `pilot_dir`, despite this function's own contract
	never to touch an existing path (found via review, in two rounds: the
	first fix replaced `Path.exists()`, which reports False for a
	*dangling* symlink, with `os.path.lexists()`, but that still left the
	check and the write as two separate syscalls). `O_EXCL` fails the
	open if a regular file is already there; `O_NOFOLLOW` fails it if a
	symlink -- dangling or not -- is there instead; either failure means
	"already there," so both are treated as this function's normal
	no-write return path.
	"""
	path = pilot_dir / "ARCHITECTURE.md"
	title = pilot_dir.name.replace("-", " ").replace("_", " ").strip().title() or "Project"
	content = (
		f"# {title} — Architecture\n\n"
		"## Repo layout\n\n"
		"- Describe the repo layout here.\n\n"
		"## Verification\n\n"
		"- Describe how `make verify` (or an equivalent canonical command) proves this app works.\n\n"
		"## Known deviations\n\n"
		"- None yet.\n"
	)
	try:
		fd = os.open(path, os.O_CREAT | os.O_EXCL | os.O_WRONLY | os.O_NOFOLLOW, 0o644)
	except FileExistsError:
		# A regular file is already there -- this function's own contract
		# for returning False without touching the existing path.
		return False
	except OSError as exc:
		if exc.errno == errno.ELOOP:
			# A symlink -- dangling or not -- is already there: same
			# "already there, leave it" contract as FileExistsError above.
			return False
		# A real failure (e.g. ENOENT: pilot_dir itself doesn't exist;
		# EACCES: no permission to write here) must not be swallowed as
		# "already present" -- found via review: doing so let callers like
		# --inject-only report success while never actually writing the
		# artifact, silently deferring the failure to a much later,
		# harder-to-diagnose `factoryd` preflight rejection instead.
		raise
	with os.fdopen(fd, "w") as f:
		f.write(content)
	return True


def step2_draft_spec(pilot_dir: Path, spec_input: Path, verify_command: str | None = None) -> bool:
	print(f"\n=== step 2: drafting spec + tickets via /spec-plan ({spec_input}) ===")
	# Deliberately a `pilot_dir` *sibling*, not something written inside it
	# (same reasoning as resolve_spec_input()'s scratch file above): `pi
	# --session-dir` creates this directory as soon as the session starts,
	# before the model's first turn -- i.e. before /spec-plan's own step-0
	# scaffold check ever runs. On a fresh, not-yet-scaffolded pilot_dir,
	# that check sees "$2 exists, is non-empty, has no Makefile" and
	# correctly refuses to scaffold, every single time, on account of a
	# directory this script itself just created (reproduced live: two
	# separate fresh-pilot-dir runs, same refusal, same evidence in
	# logs/spec-plan-output.jsonl -- not a model failure).
	session_dir = goal_pilot_session_root(pilot_dir) / "pi-spec-plan-session"
	prompt = f"/spec-plan {spec_input} {pilot_dir}"
	ok, stdout, diagnostics = run_pi_prompt(pilot_dir, prompt, session_dir=session_dir, timeout_s=SPEC_PLAN_TIMEOUT_S)
	(logs_dir(pilot_dir) / "spec-plan-output.jsonl").write_text(stdout)
	if not ok:
		append_execution_log(
			pilot_dir,
			"step 2 FAILED: /spec-plan invocation did not complete",
			json.dumps(diagnostics, indent=2),
		)
		print(f"/spec-plan invocation failed: {diagnostics}", file=sys.stderr)
		return False
	if not spec_path_for(pilot_dir).exists():
		append_execution_log(pilot_dir, "step 2 FAILED: /spec-plan ran but spec/spec.md was not written")
		print("/spec-plan completed but spec/spec.md does not exist -- see logs/spec-plan-output.jsonl", file=sys.stderr)
		return False
	modified = inject_ticketspec_keys(pilot_dir, verify_command)
	if modified:
		append_execution_log(
			pilot_dir,
			f"step 2: injected internal/ticketspec keys into {len(modified)} ticket(s)",
			"\n".join(str(p) for p in modified),
		)
	if write_architecture_stub(pilot_dir):
		append_execution_log(
			pilot_dir,
			"step 2: wrote ARCHITECTURE.md structural stub",
			"so a later factoryd <run>'s mandatory project-bootstrap preflight has something to validate before ticket 001 creates its real content",
		)
	append_execution_log(pilot_dir, "step 2 complete: spec + tickets drafted via /spec-plan")
	return True


# --------------------------------------------------------------------------
# Step 3: spec-freeze checkpoint -- hard stop, never skippable
# --------------------------------------------------------------------------


def step3_freeze_checkpoint(pilot_dir: Path, *, non_interactive: bool = False) -> bool:
	spec_path = spec_path_for(pilot_dir)
	print("\n=== step 3: human checkpoint -- spec freeze (never skippable) ===")
	print(spec_path.read_text())
	print(
		"\nReview the Assumptions & Interpretations section above line by "
		"line -- this is the one place this pipeline resolves ambiguity "
		"rather than refusing to guess."
	)
	if non_interactive:
		print("--non-interactive: refusing to auto-approve the spec freeze. Halting.", file=sys.stderr)
		append_execution_log(pilot_dir, "step 3 HALT: non-interactive run cannot approve the spec freeze")
		return False
	answer = input("Approve and freeze this spec? [y/N] ").strip().lower()
	if answer != "y":
		print(
			f"Not approved. Edit {spec_path} by hand, or re-run /spec-plan in an interactive "
			"pi session against the same input for another draft, then re-invoke goal_pilot.py "
			"against the same --pilot-dir to continue.",
			file=sys.stderr,
		)
		append_execution_log(pilot_dir, "step 3 HALT: spec freeze not approved")
		return False
	freeze_spec(pilot_dir)
	append_execution_log(pilot_dir, "step 3 complete: spec frozen")
	return True


# --------------------------------------------------------------------------
# Step 4: compile the spec via /contract-plan
# --------------------------------------------------------------------------


def staged_ticket_count(pilot_dir: Path) -> int:
	acceptance_dir = pilot_dir / "spec" / "acceptance"
	if not acceptance_dir.exists():
		return 0
	return sum(1 for p in acceptance_dir.iterdir() if p.is_dir() and p.name.isdigit())


# A parent-directory "climb" of at least this many levels above __file__
# is treated as "computing an app-root-like path", the shape of the real
# bug (3 levels: NNN -> acceptance -> spec -> pilot_dir). 2 is the
# threshold rather than 3 so a slightly different guess (e.g. landing on
# `spec/` instead of the pilot dir root, or on `workspace/` one level
# short of where the real bug landed) is still caught -- a single level
# (finding a sibling file next to the slice itself) is normal and not
# flagged.
_MIN_SUSPICIOUS_FILE_CLIMB = 2


def _file_dunder_referenced(node: ast.AST) -> bool:
	return any(isinstance(sub, ast.Name) and sub.id == "__file__" for sub in ast.walk(node))


def _climb_count(node: ast.AST) -> int:
	"""How many directory levels an expression climbs above wherever it
	starts: each `".."` path-join argument, each `os.path.dirname(...)`
	call, and each pathlib `.parent` access counts as one level. Only
	meaningful when combined with `_file_dunder_referenced` -- climbing
	from some other root (a fixture directory, a temp dir) is unrelated
	to this check's concern."""
	count = 0
	for sub in ast.walk(node):
		if isinstance(sub, ast.Constant) and sub.value == "..":
			count += 1
		elif isinstance(sub, ast.Call) and isinstance(sub.func, ast.Attribute) and sub.func.attr == "dirname":
			count += 1
		elif isinstance(sub, ast.Attribute) and sub.attr == "parent":
			count += 1
	return count


def _file_relative_app_root_lines(content: str) -> list[int]:
	"""Line numbers of a top-level assignment whose value expression
	references `__file__` and climbs at least `_MIN_SUSPICIOUS_FILE_CLIMB`
	directory levels above it -- the shape of computing "where the app
	under test lives" from a slice's own file location, per-flow-fragile
	for the reason `check_acceptance_suite_paths` documents.

	AST-based, not regex, and not a literal-string match against
	"workspace" (an earlier version of this check looked for exactly
	that): the real bug is the *computation*, not any particular string
	it happens to land on -- a different wrong guess (`spec/`, or a
	`workspace/` reached by one different `..` count) is an equally real
	instance of the same mistake, just spelled differently, and a
	literal-string check would miss all of them (found via a code-review
	pass: the specific bug this check was built for turned out not to be
	the general problem at all -- see that function's own docstring).
	"""
	try:
		tree = ast.parse(content)
	except SyntaxError:
		return []
	lines: list[int] = []
	for node in ast.walk(tree):
		if not isinstance(node, ast.Assign):
			continue
		if _file_dunder_referenced(node.value) and _climb_count(node.value) >= _MIN_SUSPICIOUS_FILE_CLIMB:
			lines.append(node.lineno)
	return lines


def check_acceptance_suite_paths(pilot_dir: Path) -> list[str]:
	"""Deterministic, mechanical guard against the exact bug a real build
	hit live (notes-demo, 2026-09-06): an acceptance slice computed "where
	the app under test lives" as a fixed number of `__file__`-relative
	parent-directory hops (`<this file's own location>/../../../workspace`)
	-- which has no single correct answer, because this pipeline has more
	than one way to actually build a ticket (`factoryd`'s own default
	merges an isolated worktree into the pilot dir's own root; a pilot
	dir's `make run` invokes `ticket_runner.py`'s own standalone flow,
	which builds directly inside `<pilot-dir>/workspace/` for real), and a
	slice has no reliable way to know in advance which one produced the
	checkout it's about to test. The one answer that's correct in every
	flow -- `os.getcwd()`, since `make verify`/`make verify-full` always
	runs with its working directory already set to wherever the app
	actually is -- needs no `__file__` arithmetic at all. The template's
	own prompt (contract-plan.md) now says so, but a prompt is
	probabilistic; this check is not.

	Scans every `.py` file under `spec/acceptance/*/` for the shape
	`_file_relative_app_root_lines` recognizes. Returns one human-readable
	warning per match (empty if none); does not attempt to fix anything --
	this is a mechanical self-check in the same spirit as step4_compile's
	own Go/Dart/Python compile check, not a substitute for the human/cloud
	review the acceptance suite still requires either way.

	Known gap, left as residual risk rather than chased further here:
	Python-only -- an identical bug in a Go or Dart slice is caught only
	by contract-plan.md's own prose, the same probabilistic path this
	check exists to stop relying on for Python.
	"""
	acceptance_dir = pilot_dir / "spec" / "acceptance"
	if not acceptance_dir.exists():
		return []
	warnings: list[str] = []
	for path in sorted(acceptance_dir.rglob("*.py")):
		content = path.read_text(errors="ignore")
		for lineno in _file_relative_app_root_lines(content):
			warnings.append(
				f"{path.relative_to(pilot_dir)}:{lineno}: computes a path by climbing multiple "
				f"directory levels above __file__ -- this pipeline has more than one way to "
				f"build a ticket, and no fixed number of parent hops is correct for all of "
				f"them. Use os.getcwd() instead: make verify/make verify-full always runs "
				f"with its working directory already set to the real app root, in every flow."
			)
	return warnings


def step4_compile(pilot_dir: Path) -> bool:
	print("\n=== step 4: compiling contract + acceptance suite via /contract-plan ===")
	# Same sibling-not-inside placement as step2_draft_spec() -- pilot_dir is
	# already scaffolded by this point (Makefile exists), so /contract-plan's
	# own preconditions don't hit the same hazard step2 does, but there is no
	# reason for this one to risk it either.
	session_dir = goal_pilot_session_root(pilot_dir) / "pi-contract-plan-session"
	prompt = f"/contract-plan {pilot_dir}"
	ok, stdout, diagnostics = run_pi_prompt(pilot_dir, prompt, session_dir=session_dir, timeout_s=CONTRACT_PLAN_TIMEOUT_S)
	(logs_dir(pilot_dir) / "contract-plan-output.jsonl").write_text(stdout)
	if not ok:
		append_execution_log(
			pilot_dir,
			"step 4 FAILED: /contract-plan invocation did not complete",
			json.dumps(diagnostics, indent=2),
		)
		print(f"/contract-plan invocation failed: {diagnostics}", file=sys.stderr)
		return False
	contract_path = pilot_dir / "spec" / "contract.md"
	if not contract_path.exists():
		append_execution_log(pilot_dir, "step 4 FAILED: /contract-plan ran but spec/contract.md was not written")
		print("/contract-plan completed but spec/contract.md does not exist -- see logs/contract-plan-output.jsonl", file=sys.stderr)
		return False
	count = staged_ticket_count(pilot_dir)
	write_compile_complete_marker(pilot_dir, count)
	path_warnings = check_acceptance_suite_paths(pilot_dir)
	if path_warnings:
		# Not a hard failure: step 5's own mandatory human/cloud checkpoint
		# exists precisely to catch semantic problems like this one, and an
		# AST-shape guard can only be conservative, never exhaustive --
		# surfaced prominently (the execution log here, plus
		# step5_checkpoint recomputing and reprinting this same check right
		# at the human's actual decision point, since a cloud-review edit
		# between step 4 and step 5's confirmation can change the answer)
		# rather than silently blocking the pipeline on a check that might
		# itself have a false positive.
		print("\n*** acceptance-suite path check found likely bugs -- see below ***")
		for warning in path_warnings:
			print(f"  - {warning}")
		append_execution_log(
			pilot_dir,
			f"step 4: acceptance-suite path check found {len(path_warnings)} likely bug(s)",
			"\n".join(path_warnings),
		)
	append_execution_log(pilot_dir, "step 4 complete: contract + acceptance suite compiled", f"staged ticket slices: {count}")
	return True


# --------------------------------------------------------------------------
# Step 5: human/cloud checkpoint over the acceptance suite
# --------------------------------------------------------------------------


def step5_checkpoint(pilot_dir: Path, checkpoint_mode: str, *, non_interactive: bool = False) -> bool:
	print("\n=== step 5: human/cloud checkpoint -- acceptance-suite review ===")
	if checkpoint5_ack_marker(pilot_dir).exists():
		print("already acknowledged on a prior run -- skipping straight to step 5b.")
		return True

	if checkpoint_mode == "review":
		contract_path = pilot_dir / "spec" / "contract.md"
		print(f"\n--- {contract_path} ---\n{contract_path.read_text()}")
		spec_output = (logs_dir(pilot_dir) / "contract-plan-output.jsonl")
		if spec_output.exists():
			print("\n--- /contract-plan's real output (self-check transcript included) ---")
			print(spec_output.read_text()[-8000:])
		# Recomputed here, not just read from whatever step4_compile logged
		# earlier: this is the human's actual decision point, and the
		# "bring spec/acceptance/ to a cloud session for review-and-correct"
		# instruction just below means the on-disk content can have changed
		# since step 4 ran -- printing a stale step-4 result here would let
		# a bug introduced (or left unfixed) during that edit go unseen
		# right where it matters most (found via a code-review pass: step
		# 4's own print/log was easy to have scrolled past, and nothing
		# re-checked after the cloud-review edit this banner asks for).
		path_warnings = check_acceptance_suite_paths(pilot_dir)
		if path_warnings:
			print("\n*** acceptance-suite path check found likely bugs -- see below ***")
			for warning in path_warnings:
				print(f"  - {warning}")
			# Logged here too, not just printed: this is the state that was
			# live at the moment of human sign-off below, which the
			# execution log should be able to show even if the human
			# answers "y" anyway (found via a code-review pass -- step 4's
			# own identical block already logs, this one hadn't).
			append_execution_log(
				pilot_dir,
				f"step 5: acceptance-suite path check found {len(path_warnings)} likely bug(s) at review time",
				"\n".join(path_warnings),
			)
		print(
			"\nThis is the highest-stakes artifact in the pipeline "
			'(/contract-plan\'s own words) -- "a wrong contract or test '
			'oracle doesn\'t fail loudly, it silently certifies broken app '
			'code as correct later." Bring spec/contract.md and '
			"spec/acceptance/ to a separate cloud session yourself for "
			"review-and-correct before proceeding -- goal_pilot.py cannot "
			"and does not do this step for you."
		)
		if non_interactive:
			print("--non-interactive: refusing to auto-confirm the acceptance-suite checkpoint. Halting.", file=sys.stderr)
			append_execution_log(pilot_dir, "step 5 HALT: non-interactive run cannot confirm the review checkpoint")
			return False
		answer = input("Has that cloud review-and-correct happened, and should goal_pilot.py proceed? [y/N] ").strip().lower()
		if answer != "y":
			append_execution_log(pilot_dir, "step 5 HALT: acceptance-suite review not confirmed")
			return False
		checkpoint5_ack_marker(pilot_dir).write_text(
			json.dumps({"mode": "review", "confirmed": datetime.now(timezone.utc).isoformat()}, indent=2)
		)
		append_execution_log(pilot_dir, "step 5 complete: cloud review confirmed by human")
		return True

	# checkpoint_mode == "skip": proceed on local self-check evidence alone.
	# Still logged unconditionally, unmissably, into the run's own evidence
	# trail -- not left discoverable only by reading /contract-plan's banner
	# text after the fact.
	disclaimer = (
		"--checkpoint=skip: proceeding on /contract-plan's local self-check "
		"alone. The acceptance suite -- the highest-stakes artifact in this "
		"pipeline -- was never independently (cloud/human) reviewed for "
		"this run."
	)
	print(disclaimer)
	append_execution_log(pilot_dir, "step 5 SKIPPED (--checkpoint=skip)", disclaimer)
	checkpoint5_ack_marker(pilot_dir).write_text(
		json.dumps({"mode": "skip", "confirmed": datetime.now(timezone.utc).isoformat()}, indent=2)
	)
	return True


# --------------------------------------------------------------------------
# Step 5b: post-ticket-001 checkpoint -- always on, not covered by
# --checkpoint. See the plan for why this is the FIRST review of a
# model-authored verify surface, not a check on a pre-pinned one.
# --------------------------------------------------------------------------


def step5b_checkpoint(
	pilot_dir: Path, workspace: Path, *, non_interactive: bool = False, verify_command: str | None = None
) -> bool:
	print("\n=== step 5b: checkpoint after ticket 001 gates green (always on) ===")
	if checkpoint5b_ack_marker(pilot_dir).exists():
		print("already acknowledged on a prior run -- skipping.")
		return True
	# verify_command, when given, replaces the hardcoded `make verify-full`
	# the same way it does in ticket_runner.run_ticket -- see that
	# function's own comment. This branch's print message below also
	# changes: "the verify surface was just authored by the local model in
	# ticket 001" describes a from-scratch scaffold, not an existing
	# (brownfield-onboarded) repo whose Makefile predates this run entirely.
	if verify_command:
		verify_full_ok, verify_full_out = ticket_runner.run_shell_command(workspace, verify_command, timeout=40 * 60)
		print(verify_full_out[-6000:])
		print(
			"\nThe caller's own resolved verify command (not a Makefile authored "
			"by ticket 001 -- this pilot is running against an existing repo) "
			"was just run against ticket 001's real committed change. This is "
			"the first independent confirmation that command actually passes "
			"against a real diff, not just against the unchanged baseline."
		)
		if not verify_full_ok:
			print("the verify command did NOT pass above -- this is worth stopping on regardless of the answer below.", file=sys.stderr)
	else:
		verify_full_ok, verify_full_out = ticket_runner.run_make(workspace, "verify-full", timeout=40 * 60)
		print(verify_full_out[-6000:])
		print(
			"\nThe workspace verify surface (Makefile, scripts/verify.sh, "
			"scripts/verify-full.sh) was just authored by the local model in "
			"ticket 001, with no pre-pinned template to check it against -- "
			"this is the FIRST review of that surface, not a sanity check on a "
			"known-good one. It is now frozen; from here on it's immutable "
			"except through `ticket_runner.py --amend-canon`."
		)
		if not verify_full_ok:
			print("`make verify-full` did NOT pass above -- this is worth stopping on regardless of the answer below.", file=sys.stderr)
	if non_interactive:
		print("--non-interactive: refusing to auto-confirm the post-ticket-001 checkpoint. Halting.", file=sys.stderr)
		append_execution_log(pilot_dir, "step 5b HALT: non-interactive run cannot confirm this checkpoint")
		return False
	answer = input("Reviewed the booted app / verify-full output -- proceed with the remaining tickets? [y/N] ").strip().lower()
	if answer != "y":
		append_execution_log(pilot_dir, "step 5b HALT: post-ticket-001 review not confirmed")
		return False
	checkpoint5b_ack_marker(pilot_dir).write_text(
		json.dumps({"confirmed": datetime.now(timezone.utc).isoformat(), "verify_full_passed": verify_full_ok}, indent=2)
	)
	append_execution_log(pilot_dir, "step 5b complete: post-ticket-001 verify surface reviewed")
	return True


# --------------------------------------------------------------------------
# Step 6: run (or resume) the build loop, in up to two ticket_runner.py
# invocations around the 5b checkpoint.
# --------------------------------------------------------------------------


def run_ticket_runner(
	pilot_dir: Path, review_policy: str, *, stop_after_ticket: int | None, verify_command: str | None = None
) -> tuple[int, str]:
	cmd = [sys.executable, str(TICKET_RUNNER), "--pilot-dir", str(pilot_dir), "--review-policy", review_policy]
	if stop_after_ticket is not None:
		cmd += ["--stop-after-ticket", str(stop_after_ticket)]
	if verify_command:
		cmd += ["--verify-command", verify_command]
	print(f"\n=== step 6: running {' '.join(cmd)} ===")
	stamp = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ")
	suffix = f"-stop{stop_after_ticket}" if stop_after_ticket is not None else ""
	log_path = logs_dir(pilot_dir) / f"run-{stamp}{suffix}.log"
	returncode, stdout, stderr, timed_out = ticket_runner.invoke_build_app(cmd, TICKET_RUNNER_TIMEOUT_S)
	combined = stdout + "\n--- stderr ---\n" + stderr
	log_path.write_text(combined)
	print(combined[-4000:])
	if timed_out:
		returncode = -1
	append_execution_log(
		pilot_dir,
		f"step 6 ticket_runner.py invocation finished (returncode={returncode}, stop_after_ticket={stop_after_ticket})",
		f"log: {log_path}",
	)
	return returncode, combined


def lock_contention(output: str) -> bool:
	return "refusing to race it" in output


# --------------------------------------------------------------------------
# Step 7: halt classification + class-aware handling
# --------------------------------------------------------------------------


def latest_gate(pilot_dir: Path, ticket: "ticket_runner.Ticket") -> dict | None:
	return ticket_runner.read_gate(pilot_dir, ticket)


def classify_halt(pilot_dir: Path, ticket: "ticket_runner.Ticket") -> str:
	"""Heuristic, string-matched against the same evidence a human rescuer
	would read -- gate.json's failed check names/details. Deliberately
	conservative: anything not clearly infra or clearly canon-drift is
	treated as an implementation gap, since that's the class with the
	narrowest (single, bounded, local-only) auto behavior."""
	gate = latest_gate(pilot_dir, ticket)
	if gate is None:
		return "unknown"
	failed = {c["name"]: str(c.get("detail", "")) for c in gate.get("checks", []) if not c.get("ok")}
	if "oracle integrity" in failed or "verify-surface frozen" in failed:
		return "canon-drift"
	infra_markers = ticket_runner.TRANSIENT_BUILD_MARKERS
	for detail in failed.values():
		lowered = detail.lower()
		if any(marker in lowered for marker in infra_markers):
			return "infra"
	return "implementation-gap"


def handle_infra_halt(pilot_dir: Path, on_halt: str, attempt_count: int) -> bool:
	"""Returns True if a retry was attempted and the caller should loop
	back to run_ticket_runner() again."""
	if on_halt != "auto-rescue":
		append_execution_log(pilot_dir, "halt class=infra: --on-halt=report, not retrying")
		return False
	if attempt_count >= MAX_INFRA_AUTO_RETRIES:
		append_execution_log(pilot_dir, f"halt class=infra: {MAX_INFRA_AUTO_RETRIES} auto-retries already used, stopping")
		return False
	current = os.environ.get("AI_STACK_HOST")
	if not AI_STACK_HOST_FALLBACK:
		append_execution_log(pilot_dir, "halt class=infra: no AI_STACK_HOST_FALLBACK set, not retrying")
		return False
	if current != AI_STACK_HOST_FALLBACK:
		print(f"infra halt: trying AI_STACK_HOST fallback ({current!r} -> {AI_STACK_HOST_FALLBACK!r})")
		os.environ["AI_STACK_HOST"] = AI_STACK_HOST_FALLBACK
	append_rescue_record(
		pilot_dir,
		{
			"ticket": "n/a",
			"class": "infra",
			"action": f"retry with AI_STACK_HOST={os.environ.get('AI_STACK_HOST')}",
			"auto_applied": True,
		},
	)
	return True


def handle_canon_drift_halt(pilot_dir: Path, workspace: Path, tickets: list, ticket: "ticket_runner.Ticket", *, non_interactive: bool) -> bool:
	"""Never auto-applied, regardless of --on-halt -- goal_pilot.py is also
	the artifact's author (via /spec-plan and /contract-plan), so amending
	its own frozen oracle without a human decision would remove the last
	independent check in a run that already defaults to advisory-only
	review. Returns True if at least one file was amended and the caller
	should retry the gate."""
	gate = latest_gate(pilot_dir, ticket)
	failed = {c["name"]: c.get("detail") for c in (gate or {}).get("checks", []) if not c.get("ok")}
	candidates: list[Path] = []
	drifted = failed.get("oracle integrity")
	if isinstance(drifted, list):
		candidates += [workspace / rel for rel in drifted]
	if "verify-surface frozen" in failed:
		candidates += [workspace / rel for rel in ticket_runner.VERIFY_SURFACE_FILES if (workspace / rel).exists()]
	if not candidates:
		print("canon-drift halt classified, but no concrete drifted file found in gate evidence -- reporting.", file=sys.stderr)
		return False
	print(
		f"\n=== ticket {ticket.nnn}: frozen-artifact-canon drift ===\n"
		"The model's change to a frozen file may be a correct fix to a genuine bug in that "
		"file (tickets 010/012's class in the parent plan), or it may not be -- this always "
		"needs a human decision, never an automatic one."
	)
	if non_interactive:
		print("--non-interactive: cannot review canon drift. Halting.", file=sys.stderr)
		return False
	amended_any = False
	for candidate in candidates:
		# ticket_runner.run_ticket() already restored the canonical copy
		# over this exact file before returning failure (oracle drift's
		# stage() call, or restore_verify_baseline() for verify-surface
		# drift) -- by the time a halt reaches this handler, the on-disk
		# workspace copy is back to canon, not the model's proposed fix.
		# amend_canon() diffs the on-disk file against canon, so reading it
		# directly here would always find "no difference, nothing to
		# amend" and the whole class-2 recovery path would silently never
		# work (Codex review of PR #37). Recover the model's actual
		# proposed content from the ticket's own commit instead, and
		# restore *that* into the workspace right before calling
		# amend_canon() so there is a real diff to accept or reject.
		recovered = recover_drifted_content(workspace, ticket, candidate)
		if recovered is None:
			print(f"could not recover {candidate}'s drifted content from ticket {ticket.nnn}'s commit -- skipping", file=sys.stderr)
			continue
		candidate.write_text(recovered)
		reason = input(f"Reason to accept {candidate} as the new canon (blank to skip this file): ").strip()
		if not reason:
			continue
		rc = ticket_runner.amend_canon(pilot_dir, workspace, tickets, candidate, reason, auto_yes=False)
		if rc == 0:
			amended_any = True
			append_rescue_record(
				pilot_dir,
				{"ticket": ticket.nnn, "class": "canon-drift", "action": f"amend-canon {candidate}", "reason": reason, "auto_applied": False},
			)
	return amended_any


def recover_drifted_content(workspace: Path, ticket: "ticket_runner.Ticket", candidate: Path) -> str | None:
	"""Recover the model's actual proposed content for `candidate` (a path
	inside `workspace`) from the ticket's own commit, since
	ticket_runner.run_ticket() has already restored the on-disk copy to
	canon by the time a halt reaches goal_pilot.py. Returns None if the
	ticket has no commit yet or the file isn't present at that commit."""
	sha = ticket_runner.commit_sha_for(workspace, ticket.number)
	if not sha:
		return None
	rel = candidate.relative_to(workspace).as_posix()
	result = ticket_runner.git(workspace, "show", f"{sha}:{rel}")
	if result.returncode != 0:
		return None
	return result.stdout


def handle_implementation_gap_halt(pilot_dir: Path, workspace: Path, tickets: list, ticket: "ticket_runner.Ticket", review_policy: str, on_halt: str) -> bool:
	"""Bounded, single, unconditionally-local widened retry -- see the
	module docstring and WIDENED_* constants above. Returns True if the
	widened attempt itself completed (regardless of whether it then gates
	green -- the caller re-invokes ticket_runner.py either way, which will
	regate a fresh commit or report the same halt again if nothing
	changed)."""
	if on_halt != "auto-rescue":
		append_execution_log(pilot_dir, f"halt class=implementation-gap: ticket {ticket.nnn}, --on-halt=report, not retrying")
		return False
	already_widened = any(
		r.get("ticket") == ticket.nnn and r.get("class") == "implementation-gap" for r in read_rescue_records(pilot_dir)
	)
	if already_widened:
		append_execution_log(pilot_dir, f"halt class=implementation-gap: ticket {ticket.nnn} already had its one widened retry, stopping")
		return False
	base_sha = ticket_runner.prior_boundary_sha(workspace, tickets, ticket)
	cmd = [
		sys.executable, str(ticket_runner.BUILD_APP),
		"--workspace", str(workspace),
		"--spec", str(ticket.path),
		"--max-rounds", str(WIDENED_MAX_ROUNDS),
		"--timeout-minutes", str(WIDENED_TIMEOUT_MINUTES),
		"--thinking", WIDENED_THINKING,
		"--review-policy", review_policy,
	]
	if base_sha:
		cmd += ["--review-base-sha", base_sha]
	print(f"\n=== widened rescue attempt for ticket {ticket.nnn}: {' '.join(cmd)} ===")
	returncode, stdout, stderr, timed_out = ticket_runner.invoke_build_app(cmd, WIDENED_TIMEOUT_MINUTES * 60 + 300)
	log_path = logs_dir(pilot_dir) / f"rescue-ticket-{ticket.nnn}-widened.log"
	log_path.write_text(stdout + "\n--- stderr ---\n" + stderr)
	append_rescue_record(
		pilot_dir,
		{
			"ticket": ticket.nnn,
			"class": "implementation-gap",
			"action": "widened local retry",
			"max_rounds": WIDENED_MAX_ROUNDS,
			"timeout_minutes": WIDENED_TIMEOUT_MINUTES,
			"thinking": WIDENED_THINKING,
			"returncode": returncode,
			"timed_out": timed_out,
			"auto_applied": True,
		},
	)
	return True  # let the caller regate; success/failure is read from the next gate, not this returncode


def handle_halt(pilot_dir: Path, workspace: Path, tickets: list, review_policy: str, on_halt: str, *, non_interactive: bool, infra_attempts: dict) -> bool:
	"""Returns True if the caller should loop back and re-invoke
	ticket_runner.py; False if goal_pilot.py should stop and report."""
	ticket, mode = ticket_runner.next_ticket(tickets, pilot_dir, workspace, review_policy)
	if ticket is None:
		return False  # nothing outstanding -- odd for a halt, but nothing to rescue
	halt_class = classify_halt(pilot_dir, ticket)
	print(f"\nhalt classified as: {halt_class} (ticket {ticket.nnn})")
	if halt_class == "infra":
		count = infra_attempts.get(ticket.nnn, 0)
		retried = handle_infra_halt(pilot_dir, on_halt, count)
		infra_attempts[ticket.nnn] = count + 1
		return retried
	if halt_class == "canon-drift":
		return handle_canon_drift_halt(pilot_dir, workspace, tickets, ticket, non_interactive=non_interactive)
	if halt_class == "implementation-gap":
		return handle_implementation_gap_halt(pilot_dir, workspace, tickets, ticket, review_policy, on_halt)
	append_execution_log(pilot_dir, f"halt class=unknown for ticket {ticket.nnn} -- no automatic handling, stopping")
	return False


# --------------------------------------------------------------------------
# Step 8: verdict
# --------------------------------------------------------------------------


def write_verdict(pilot_dir: Path, tickets: list, review_policy: str, checkpoint_mode: str, on_halt_mode: str) -> Path:
	workspace = pilot_dir / "workspace"
	rescues = read_rescue_records(pilot_dir)
	by_class: dict[str, int] = {}
	for r in rescues:
		by_class[r.get("class", "unknown")] = by_class.get(r.get("class", "unknown"), 0) + 1
	lines = [
		"# goal_pilot.py verdict",
		"",
		f"Generated: {datetime.now(timezone.utc).isoformat()}",
		f"Pilot dir: {pilot_dir}",
		f"Tickets: {len(tickets)}",
		f"Settings: --checkpoint={checkpoint_mode} --on-halt={on_halt_mode} --review-policy={review_policy}",
		"",
		"## Rescues",
		"",
	]
	if not rescues:
		lines.append("None -- every ticket gated green with no intervention beyond the standard checkpoints.")
	else:
		for kind, count in sorted(by_class.items()):
			lines.append(f"- {kind}: {count}")
		lines.append("")
		lines.append("Detail (see .goal-pilot/rescues.jsonl for the full machine-readable record):")
		for r in rescues:
			lines.append(f"- ticket {r.get('ticket')}: {r.get('class')} -- {r.get('action')}")
	# "No cloud model was invoked" used to be asserted here unconditionally
	# (found via a real GitHub Codex App review of this PR): the actual
	# guarantee this script provides is narrower and doesn't depend on
	# whatever route pi's own configuration happens to default to, which
	# this script has no visibility into -- PiAdapter.invocation()
	# passes --provider/--model only when PI_HARNESS_PROVIDER/
	# PI_HARNESS_MODEL are explicitly set (see harness_adapters.py's
	# PiAdapter.invocation); left unset, every invocation this run
	# made -- including a widened-but-still-local auto-rescue retry --
	# inherited whatever pi's own default route already was, cloud or
	# local. This run never itself requested escalation to a cloud
	# provider/model; it cannot certify that the inherited default wasn't
	# already one. Echoing PI_HARNESS_PROVIDER/PI_HARNESS_MODEL's actual
	# values makes that distinction explicit instead of collapsing it into
	# a single unconditional claim.
	provider = os.environ.get("PI_HARNESS_PROVIDER")
	model = os.environ.get("PI_HARNESS_MODEL")
	cloud_escalation_note = (
		"Verified from the evidence trail above: no rescue this run performed "
		"(if any) -- infra retries, human-approved --amend-canon calls, or "
		"widened-but-still-local build_app.py retries -- itself requested a "
		"cloud provider or model; every pi invocation used "
		f"PI_HARNESS_PROVIDER={provider!r} PI_HARNESS_MODEL={model!r} "
		"(pi's own configured default route when both are unset, as they are "
		"here unless the operator set them). This is NOT a zero-cloud-usage "
		"guarantee if pi's own default route is itself configured to a cloud "
		"provider -- this script cannot see or verify that from here."
	)
	if provider is None and model is None:
		cloud_escalation_note += (
			" Both are unset for this run; to actually guarantee a local route, "
			"set PI_HARNESS_PROVIDER/PI_HARNESS_MODEL explicitly, or confirm "
			"pi's own configured default is local."
		)
	lines += [
		"",
		"## Cloud-escalation claim",
		"",
		cloud_escalation_note,
	]
	verdict_path = pilot_dir / "VERDICT.md"
	verdict_path.write_text("\n".join(lines) + "\n")
	return verdict_path


# --------------------------------------------------------------------------
# Orchestration
# --------------------------------------------------------------------------


def run_build_loop_until_settled(
	pilot_dir: Path,
	workspace: Path,
	tickets: list,
	review_policy: str,
	on_halt: str,
	*,
	stop_after_ticket: int | None,
	non_interactive: bool,
	infra_attempts: dict,
	verify_command: str | None = None,
) -> bool:
	"""Invoke ticket_runner.py (bounded by `stop_after_ticket` if given),
	and on any halt, classify it and apply step 7's class-aware handling,
	looping back for another invocation whenever handle_halt() says to.
	Shared by both the ticket-1-only phase (before the 5b checkpoint) and
	the remainder-of-the-run phase -- a halt on ticket 1 gets exactly the
	same treatment as a halt on any later ticket, not a bare give-up.
	Returns True once ticket_runner.py exits 0 (the requested boundary, or
	the whole run, settled); False if goal_pilot.py should stop and
	report."""
	while True:
		returncode, output = run_ticket_runner(
			pilot_dir, review_policy, stop_after_ticket=stop_after_ticket, verify_command=verify_command
		)
		if lock_contention(output):
			print("another goal_pilot.py/ticket_runner.py run is already in progress against this pilot dir.", file=sys.stderr)
			return False
		if returncode == 0:
			return True
		should_retry = handle_halt(
			pilot_dir, workspace, tickets, review_policy, on_halt,
			non_interactive=non_interactive, infra_attempts=infra_attempts,
		)
		if not should_retry:
			print("\ngoal_pilot.py stopping -- see EXECUTION_LOG.md and PROGRESS.md for the halt this stopped on.", file=sys.stderr)
			return False
		time.sleep(5)


def main() -> int:
	parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
	parser.add_argument("--spec-input", help="Path to a rough-input file, or literal rough-input text. Required unless --inject-only is used.")
	parser.add_argument("--pilot-dir", type=Path, help="Required unless --inject-only is used.")
	parser.add_argument(
		"--inject-only",
		type=Path,
		metavar="PILOT_DIR",
		help=(
			"Run only inject_ticketspec_keys() and write_architecture_stub() "
			"against an existing pilot dir, then exit. For a /spec-plan "
			"invocation run standalone (see prompts/spec-plan.md's own step "
			"2), outside goal_pilot.py's usual orchestration, which already "
			"calls both itself after every /spec-plan run -- found via "
			"review: the standalone entry point README.md documents "
			"(`/spec-plan <rough-input> <pilot-dir>` on its own, with no "
			"goal_pilot.py process ever running) had no way to reach either "
			"one at all before this flag existed, so a later `factoryd` run "
			"against a standalone /spec-plan pilot would still fail its "
			"mandatory ARCHITECTURE.md preflight even after that gap was "
			"first closed for --inject-only's ticketspec-key half alone."
		),
	)
	parser.add_argument("--checkpoint", choices=("review", "skip"), default=DEFAULT_CHECKPOINT)
	parser.add_argument("--on-halt", choices=("report", "auto-rescue"), default=DEFAULT_ON_HALT)
	parser.add_argument(
		"--review-policy",
		choices=("advisory", "required", "degraded"),
		default=DEFAULT_REVIEW_POLICY,
		help="Passed through to ticket_runner.py. advisory is the stated default -- see the plan's step 6.",
	)
	parser.add_argument(
		"--non-interactive",
		action="store_true",
		help="Refuse every checkpoint instead of prompting (for CI/dry-run use) -- never auto-approves.",
	)
	parser.add_argument(
		"--verify-command",
		default=None,
		help=(
			"Overrides verify_command_for_ticket()'s own hardcoded `make verify "
			"&& make verify-full` default when injecting a ticket's "
			"Verify-Command: key. `make verify-full` is a convention every app "
			"this pipeline scaffolds from scratch defines -- an existing repo "
			"being onboarded (factoryd onboard / -preflight-profile brownfield) "
			"has no reason to, and declaring a command that can never pass "
			"permanently quarantines an otherwise-correct ticket. Pass the "
			"target repo's own real verify command here (its .factory.yml "
			"verify_command, typically) when drafting against an existing repo."
		),
	)
	args = parser.parse_args()
	# Its own pi invocations below need the relay route rendered like any
	# other harness-driving script's (a no-op outside a relay-routed worker).
	harness_adapters.get("pi").prepare()

	if args.inject_only:
		inject_only_dir = args.inject_only.resolve()
		modified = inject_ticketspec_keys(inject_only_dir, args.verify_command)
		if modified:
			print(f"inject-ticketspec-keys: modified {len(modified)} ticket(s):")
			for path in modified:
				print(f"  {path}")
		else:
			print("inject-ticketspec-keys: no tickets modified (already declared, ticket 001, or no extractable path)")
		if write_architecture_stub(inject_only_dir):
			print(f"write-architecture-stub: wrote {inject_only_dir / 'ARCHITECTURE.md'}")
		else:
			print("write-architecture-stub: ARCHITECTURE.md already present, left untouched")
		return 0

	if not args.spec_input or not args.pilot_dir:
		parser.error("--spec-input and --pilot-dir are required unless --inject-only is used")

	pilot_dir = args.pilot_dir.resolve()
	pilot_dir.mkdir(parents=True, exist_ok=True)

	print(f"goal_pilot.py: pilot-dir={pilot_dir} checkpoint={args.checkpoint} on-halt={args.on_halt} review-policy={args.review_policy}")
	run_started_note = (
		f"checkpoint={args.checkpoint} on-halt={args.on_halt} review-policy={args.review_policy} "
		f"spec-input={args.spec_input}"
	)
	# Do not write anything into pilot_dir before it's confirmed safe to:
	# /spec-plan's own step-0 scaffold check refuses to run against any
	# existing, non-empty directory that has no Makefile yet, so writing
	# EXECUTION_LOG.md here unconditionally would make every fresh,
	# not-yet-scaffolded pilot_dir fail that check on its very first run
	# (Codex review of PR #37). Log immediately only if this is already a
	# scaffolded pilot dir (a resume); otherwise defer until step2 confirms
	# /spec-plan has scaffolded it.
	already_scaffolded = (pilot_dir / "Makefile").exists()
	if already_scaffolded:
		append_execution_log(pilot_dir, "run started", run_started_note)

	# --- steps 1-3: intake, draft, freeze ---
	status = read_spec_status(pilot_dir)
	if status != "FROZEN":
		spec_input = resolve_spec_input(args.spec_input, pilot_dir)
		if status is None or status == "DRAFT":
			if not step2_draft_spec(pilot_dir, spec_input, args.verify_command):
				return 1
		if not already_scaffolded:
			if not (pilot_dir / "Makefile").exists():
				print(f"/spec-plan completed but {pilot_dir}/Makefile still does not exist -- refusing to proceed.", file=sys.stderr)
				return 1
			append_execution_log(pilot_dir, "run started", run_started_note)
		if not step3_freeze_checkpoint(pilot_dir, non_interactive=args.non_interactive):
			return 1

	# Bootstrap ARCHITECTURE.md unconditionally, not just inside
	# step2_draft_spec() -- a pilot already FROZEN on entry (a resume, or
	# one whose spec/tickets were produced by an earlier version of this
	# script, or by a standalone /spec-plan invocation outside
	# goal_pilot.py entirely) skips step2_draft_spec() above and so would
	# never call write_architecture_stub(), leaving the later mandatory
	# `factoryd` preflight to fail on a missing ARCHITECTURE.md that
	# nothing in this run would otherwise create (found via review). The
	# function is idempotent -- a no-op once ARCHITECTURE.md exists -- so
	# calling it again here for the fresh-pilot path too is harmless.
	if write_architecture_stub(pilot_dir):
		append_execution_log(
			pilot_dir,
			"wrote ARCHITECTURE.md structural stub",
			"so a later factoryd <run>'s mandatory project-bootstrap preflight has something to validate before ticket 001 creates its real content",
		)

	# --- step 4: compile ---
	if not is_compile_complete(pilot_dir):
		if not step4_compile(pilot_dir):
			return 1

	# --- step 5: acceptance-suite checkpoint ---
	if not step5_checkpoint(pilot_dir, args.checkpoint, non_interactive=args.non_interactive):
		return 1

	# --- steps 5b/6/7: ticket 001, checkpoint, remaining tickets, halts ---
	workspace = pilot_dir / "workspace"
	tickets = ticket_runner.discover_tickets(pilot_dir / "spec")
	if not tickets:
		print(f"no tickets found under {pilot_dir / 'spec' / 'tickets'}", file=sys.stderr)
		return 1

	# infra_attempts is shared across both the ticket-1-only phase and the
	# remainder below -- an infra halt on ticket 1 gets the same bounded
	# auto-retry treatment as any other ticket, it doesn't just give up
	# (an earlier version of this function did exactly that; a halt on
	# ticket 1 is not structurally different from a halt on ticket 5).
	infra_attempts: dict[str, int] = {}

	ticket_one_done = ticket_runner.ticket_done(pilot_dir, workspace, tickets[0], args.review_policy)
	if not checkpoint5b_ack_marker(pilot_dir).exists():
		if not ticket_one_done:
			ok = run_build_loop_until_settled(
				pilot_dir, workspace, tickets, args.review_policy, args.on_halt,
				stop_after_ticket=tickets[0].number, non_interactive=args.non_interactive,
				infra_attempts=infra_attempts, verify_command=args.verify_command,
			)
			if not ok:
				write_verdict(pilot_dir, tickets, args.review_policy, args.checkpoint, args.on_halt)
				return 1
		if not step5b_checkpoint(pilot_dir, workspace, non_interactive=args.non_interactive, verify_command=args.verify_command):
			return 1

	ok = run_build_loop_until_settled(
		pilot_dir, workspace, tickets, args.review_policy, args.on_halt,
		stop_after_ticket=None, non_interactive=args.non_interactive,
		infra_attempts=infra_attempts, verify_command=args.verify_command,
	)
	if not ok:
		write_verdict(pilot_dir, tickets, args.review_policy, args.checkpoint, args.on_halt)
		return 1

	# --- step 8: verdict ---
	verdict_path = write_verdict(pilot_dir, tickets, args.review_policy, args.checkpoint, args.on_halt)
	print(f"\nall tickets complete. verdict written to {verdict_path}")
	append_execution_log(pilot_dir, "run complete: all tickets gated green")
	return 0


if __name__ == "__main__":
	sys.exit(main())
