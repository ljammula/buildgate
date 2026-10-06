#!/usr/bin/env python3
"""Outer loop over a ticket-decomposed spec: one fresh build_app.py
invocation per ticket, gated by machine-checked evidence only.

A "pilot dir" holds a frozen spec, contract, ticket graph, and sliced
acceptance-test oracle in `spec/`, and the app under construction in `workspace/`. This script drives
`build_app.py` once per ticket, in order, and only advances past a ticket
when every one of six machine-checked conditions holds -- nothing the
agent *says* (including its own BUILD_REPORT.md prose beyond the one
parsed status line, and its PROGRESS.md entries) is trusted as evidence.

Position is derived from TWO sources together, not the git log alone
(regression fixed 2026-08-20 review: a ticket whose commit exists but
whose gate then FAILED -- verify-full red, oracle drift, state files
untouched -- was being silently treated as done on the next invocation,
because the old logic only checked for the commit. That breaks
stop-the-line across process restarts, which is this script's core
invariant). A ticket now counts as done only when BOTH hold: a commit
whose subject starts `ticket(NNN):` exists, AND
`reports/ticket-NNN/gate.json` records `passed: true`. If the commit
exists but the gate record is missing/failed, the ticket is re-gated
(the five/six checks re-run against current state) WITHOUT invoking
build_app.py again -- this is what makes the rescue flow below actually
work. There is still no separate "resume" mode: `--status` and a normal
run derive the same position, so crashing and rerunning is always safe.

When a build attempt stops before trustworthy report evidence, a normal
`make run` performs at most three total build attempts for that ticket,
persisting per-attempt logs under `reports/ticket-NNN/`. Real verification,
oracle-integrity, and frozen-surface failures remain stop-the-line failures;
they are never retried as infrastructure noise.

Rescue commits: if a ticket halts (this script exits non-zero) and a
human fixes it -- typically by running build_app.py by hand against the
ticket spec in a normal interactive session, or otherwise making the
same evidence hold -- that commit's message must still start with
`ticket(NNN):` (so position derivation still finds it) and should
additionally contain the literal tag `[rescued]` somewhere in the
subject or body, e.g. `ticket(004): onboarding screen [rescued]`. This
script counts rescued tickets separately in `--status` output -- it does
not treat them differently for gating (a rescued ticket must pass the
exact same six-check gate as every other ticket) or block on their
presence in any way. The next `make run` after a rescue commit finds the
commit already present, finds no passing gate record yet, and re-gates
it -- no flag or special invocation needed. Rescue is always a
human-invoked, out-of-band step; this script has no escalation path of
its own (Phase 3 of the plan).

The `[rescued]` tag is honor-system, not enforced (a 2026-08-20 pilot run
found `--status` undercounting real rescues because of it). `--status`
also reports `gate_revisit_count()` -- a structural signal, derived from
how many real gate-attempt records a ticket produced, that catches a
rescue whether or not anyone remembered to tag the commit, including
infra-only rescues (fixing a stale model route, say) that never touch
ticket content at all. Use `--amend-canon <workspace-file> --reason
"..."` when a rescue's actual fix is a legitimate correction to a frozen
verify-surface script or staged acceptance oracle (the gate is right to
reject an agent-authored change to either without review, but the
correction still needs a sanctioned way to become the new canon instead
of hand-editing `reports/ticket-001/verify-baseline` and
`spec/acceptance/NNN/` directly).

Usage:
    python3 ticket_runner.py --pilot-dir /path/to/pilot [--status]
        [--review-policy advisory|required|degraded]

The ticket runner defaults to advisory review: the independent reviewer still
runs and its verdict is archived, but only canonical verification and the
runner's deterministic gates stop the ticket. Use `--review-policy required`
for a release-hardening run that requires clean independent review.
"""

from __future__ import annotations

import argparse
import difflib
import fcntl
import hashlib
import json
import os
import re
import signal
import shutil
import subprocess
import sys
import time
from dataclasses import dataclass
from datetime import datetime, timezone
from pathlib import Path

SCRIPT_DIR = Path(__file__).resolve().parent
BUILD_APP = SCRIPT_DIR / "build_app.py"
STAGED_EXTENSIONS = {
	".go": "acceptance",
	".mod": "acceptance",
	".sum": "acceptance",
	".dart": "app/test",
	# Found via a code-review pass on the notes-demo-driven Python
	# acceptance-suite additions to /contract-plan's own conventions
	# (2026-09-06): the prompt template started producing .py slices +
	# helpers.py long before this line existed, so a Python pilot's
	# acceptance suite was silently never staged into the workspace at
	# all under this script's own drift-protection gate -- it could
	# neither test the implementation as part of a ticket_runner.py-
	# driven build nor be protected as frozen oracle content.
	".py": "acceptance",
}
# stage()'s own bookkeeping of which paths it wrote, so its next cleanup
# pass can tell "a file this function staged before, now stale" apart from
# "a file the agent wrote itself" -- see stage()'s own doc comment. Listed
# in build_app.py's ensure_git_repo() gitignore-seeding alongside
# BUILD_REPORT.md/BUILD_EVIDENCE.json: harness bookkeeping, not app source.
STAGED_MANIFEST_FILENAME = ".ticket-runner-staged.json"
TICKET_RE = re.compile(r"^(\d{3})-(.+)\.md$")
COMMIT_RE_TEMPLATE = r"^ticket\({nnn}\):"
BUILD_APP_TIMEOUT_S = 90 * 60
MAX_BUILD_ATTEMPTS = 3
MAX_BUILDER_ROUNDS = 3
BUILD_RETRY_BACKOFF_S = 30
DEFAULT_REVIEW_POLICY = "advisory"
REVIEW_POLICY_STRENGTH = {"advisory": 0, "degraded": 1, "required": 2}
TRANSIENT_BUILD_MARKERS = (
	"pi invocation timed out",
	"pi invocation failed",
	"stall-timeout",
	"review unavailable (request-failed)",
	"review unavailable (no-review-verdict)",
	# build_app.py's own outage short-circuit (agent_turn_errors()/
	# round_blockers() in build_app.py): every assistant turn in the round
	# errored out because the model route itself was unreachable, so no
	# implementation work was ever attempted. Without this marker,
	# retryable_build_state() treats an outage report exactly like a real
	# implementation failure and halts for human intervention instead of
	# using the bounded build-attempt retry this class of failure is meant
	# for (observed live: budget-pilot ticket 005, 2026-08-20 -- Codex
	# review of PR #28 flagged this gap).
	"model route unreachable",
)
# The Makefile and verify scripts are agent-writable but gate-trusted --
# nothing byte-checks them the way oracle_drift() byte-checks acceptance
# files, so a model could edit `make verify` to a no-op and pass checks 1-2
# vacuously (same threat class as editing a test). Ticket 001 is the one
# ticket allowed to shape these files (it's told to create them, with
# documented deviation latitude); once its gate passes, whatever it
# produced is frozen and every later ticket's gate checks the hashes
# haven't moved.
VERIFY_SURFACE_FILES = ["Makefile", "scripts/verify.sh", "scripts/verify-full.sh"]


def sh(args: list[str], *, cwd: Path | None = None, timeout: float | None = None) -> subprocess.CompletedProcess:
	return subprocess.run(args, cwd=cwd, text=True, capture_output=True, timeout=timeout, check=False)


def git(workspace: Path, *args: str, timeout: float = 30) -> subprocess.CompletedProcess:
	return sh(["git", *args], cwd=workspace, timeout=timeout)


def has_own_git_repo(workspace: Path) -> bool:
	"""True only when `workspace` is itself a repo root, not merely a
	directory inside an ancestor's repo. Every git command run with
	cwd=workspace silently answers from the pilot dir's control repo until
	build_app.py's ensure_git_repo() gives the workspace a repo of its own,
	so any sha read before that point belongs to the wrong history."""
	toplevel = git(workspace, "rev-parse", "--show-toplevel")
	if toplevel.returncode != 0 or not toplevel.stdout.strip():
		return False
	return Path(toplevel.stdout.strip()).resolve() == workspace.resolve()


@dataclass
class Ticket:
	number: int
	slug: str
	path: Path

	@property
	def nnn(self) -> str:
		return f"{self.number:03d}"


def discover_tickets(spec_dir: Path) -> list[Ticket]:
	tickets = []
	for path in sorted((spec_dir / "tickets").glob("*.md")):
		m = TICKET_RE.match(path.name)
		if not m:
			continue
		tickets.append(Ticket(number=int(m.group(1)), slug=m.group(2), path=path))
	tickets.sort(key=lambda t: t.number)
	return tickets


def commit_sha_for(workspace: Path, number: int) -> str | None:
	"""Most recent commit whose subject matches `ticket(NNN):` for this
	ticket number, or None if there isn't one yet."""
	result = git(workspace, "log", "--format=%H\t%s")
	if result.returncode != 0:
		return None
	pattern = re.compile(COMMIT_RE_TEMPLATE.format(nnn=f"{number:03d}"))
	for line in result.stdout.splitlines():
		sha, _, subject = line.partition("\t")
		if pattern.match(subject):
			return sha
	return None


def committed_ticket_numbers(workspace: Path) -> dict[int, str]:
	"""Ticket number -> most recent matching commit subject, derived purely
	from git log. Empty dict on an unborn/missing repo -- that's ticket 1's
	starting state, not an error. Used for display/status only; gating
	position uses ticket_done() below, which also requires a passing gate
	record."""
	result = git(workspace, "log", "--format=%H\t%s")
	if result.returncode != 0:
		return {}
	found: dict[int, str] = {}
	for line in reversed(result.stdout.splitlines()):  # oldest first, so "most recent" overwrites
		if "\t" not in line:
			continue
		_sha, subject = line.split("\t", 1)
		m = re.match(r"^ticket\((\d{3})\):", subject)
		if m:
			found[int(m.group(1))] = subject
	return found


def read_gate(pilot_dir: Path, ticket: Ticket) -> dict | None:
	path = pilot_dir / "reports" / f"ticket-{ticket.nnn}" / "gate.json"
	if not path.exists():
		return None
	try:
		return json.loads(path.read_text())
	except (json.JSONDecodeError, OSError):
		return None


def failed_check_names(gate: dict | None) -> set[str]:
	if not gate:
		return set()
	return {
		str(check.get("name"))
		for check in gate.get("checks", [])
		if not check.get("ok")
	}


def gate_review_policy(gate: dict | None) -> str | None:
	"""Return the policy represented by gate evidence.

	Older gates predate the persisted policy field and were produced while
	`build_app.py` required clean review by default, so treat missing policy as
	`required` for backward-compatible, conservative evidence handling.
	"""
	if not gate:
		return None
	if "review_policy" not in gate:
		return "required"
	policy = gate.get("review_policy")
	return policy if policy in REVIEW_POLICY_STRENGTH else None


def gate_satisfies_review_policy(gate: dict | None, requested: str) -> bool:
	if not gate or gate.get("passed") is not True:
		return False
	recorded = gate_review_policy(gate)
	return recorded is not None and REVIEW_POLICY_STRENGTH[recorded] >= REVIEW_POLICY_STRENGTH[requested]


def _attempt_numbers(report_dir: Path, prefixes: tuple[str, ...]) -> list[int]:
	numbers = []
	for prefix in prefixes:
		for path in report_dir.glob(f"{prefix}-attempt-*"):
			match = re.search(r"-attempt-(\d+)(?:\.|$)", path.name)
			if match:
				numbers.append(int(match.group(1)))
	return numbers


def next_build_attempt(pilot_dir: Path, ticket: Ticket) -> int:
	"""Return the durable one-based number for the next build invocation."""
	report_dir = pilot_dir / "reports" / f"ticket-{ticket.nnn}"
	if not report_dir.exists():
		return 1
	started = {
		int(match.group(1))
		for path in report_dir.glob("build-attempt-*.started.json")
		if (match := re.search(r"-attempt-(\d+)\.started\.json$", path.name))
	}
	completed = {
		int(match.group(1))
		for path in report_dir.glob("build-attempt-*.log")
		if (match := re.search(r"-attempt-(\d+)\.log$", path.name))
	}
	incomplete = started - completed
	if incomplete:
		# A process can die after reserving a slot but before archiving its
		# build log. Resume that same slot; the interruption did not create a
		# fourth build attempt and the immutable start marker remains evidence.
		return max(incomplete)
	numbers = _attempt_numbers(report_dir, ("build", "BUILD_REPORT"))
	if numbers:
		return max(numbers) + 1
	# Older runners created the directory before invoking the builder and wrote
	# only build.log after it returned. An empty directory is the interrupted
	# form observed in the live pilot. Gate-only evidence does not consume the
	# build budget.
	if (report_dir / "build.log").exists() or not any(report_dir.iterdir()):
		return 2
	return 1


def next_gate_attempt(pilot_dir: Path, ticket: Ticket) -> int:
	"""Return the durable one-based number for the next archived gate."""
	report_dir = pilot_dir / "reports" / f"ticket-{ticket.nnn}"
	if not report_dir.exists():
		return 1
	started = {
		int(match.group(1))
		for path in report_dir.glob("gate-attempt-*.started.json")
		if (match := re.search(r"-attempt-(\d+)\.started\.json$", path.name))
	}
	completed = {
		int(match.group(1))
		for path in report_dir.glob("gate-attempt-*.json")
		if not path.name.endswith(".started.json")
		and (match := re.search(r"-attempt-(\d+)\.json$", path.name))
	}
	incomplete = started - completed
	if incomplete:
		return max(incomplete)
	numbers = _attempt_numbers(report_dir, ("gate",))
	if numbers:
		return max(numbers) + 1
	return 2 if (report_dir / "gate.json").exists() else 1


def write_once(path: Path, content: str | bytes) -> None:
	"""Create immutable per-attempt evidence; never overwrite an old run."""
	mode = "xb" if isinstance(content, bytes) else "x"
	with path.open(mode) as handle:
		handle.write(content)


def retryable_build_state(pilot_dir: Path, workspace: Path, ticket: Ticket) -> bool:
	"""Return true when a prior build attempt stopped before report evidence.

	A missing report is recoverable infrastructure state. A gate with any real
	verification, oracle, state-file, or review-equivalent failure is not: the
	line must stop and require a human decision rather than silently repeating
	implementation work.
	"""
	report = workspace / "BUILD_REPORT.md"
	transient_report = False
	if report.exists():
		outcome_line = next(
			(
				line.strip().lower()
				for line in report.read_text(errors="ignore").splitlines()
				if line.startswith("Outcome:")
			),
			"",
		)
		transient_report = any(marker in outcome_line for marker in TRANSIENT_BUILD_MARKERS)
		if not transient_report:
			return False
	gate = read_gate(pilot_dir, ticket)
	if gate is None:
		# run_ticket creates this directory before invoking build_app.py. Its
		# existence distinguishes an interrupted prior build from a fresh pilot.
		return (pilot_dir / "reports" / f"ticket-{ticket.nnn}").exists()
	failed = failed_check_names(gate)
	if "BUILD_REPORT.md SUCCEEDED" not in failed:
		return False
	if transient_report:
		return not (failed & {"oracle integrity", "verify-surface frozen"})
	# A builder timeout/nonzero exit is sufficient to retry even if the
	# incomplete workspace also makes verification red. Oracle or frozen-surface
	# drift is never treated as transient infrastructure.
	if "build_app.py invocation" in failed:
		return not (failed & {"oracle integrity", "verify-surface frozen"})
	# This is the recovery shape produced when the outer runner was interrupted
	# before it could archive the builder's invocation result.
	return failed <= {"BUILD_REPORT.md SUCCEEDED"}


def ticket_done(
	pilot_dir: Path,
	workspace: Path,
	ticket: Ticket,
	review_policy: str = DEFAULT_REVIEW_POLICY,
) -> bool:
	if commit_sha_for(workspace, ticket.number) is None:
		return False
	gate = read_gate(pilot_dir, ticket)
	return gate_satisfies_review_policy(gate, review_policy)


def next_ticket(
	tickets: list[Ticket],
	pilot_dir: Path,
	workspace: Path,
	review_policy: str = DEFAULT_REVIEW_POLICY,
) -> tuple[Ticket | None, str | None]:
	"""Returns (ticket, mode) where mode is "build" (no commit yet -- run
	build_app.py), "retry" (a previous build stopped before report evidence),
	or "regate" (a commit exists but no passing gate record -- re-run the gate
	only, e.g. after a rescue commit), or "policy" (a passing gate exists but
	was produced under a weaker review policy and must be rebuilt). (None, None)
	means every ticket is done."""
	for t in tickets:
		gate = read_gate(pilot_dir, t)
		if ticket_done(pilot_dir, workspace, t, review_policy):
			continue
		if commit_sha_for(workspace, t.number) is not None and gate and gate.get("passed") is True:
			return t, "policy"
		if retryable_build_state(pilot_dir, workspace, t):
			return t, "retry"
		mode = "regate" if commit_sha_for(workspace, t.number) is not None else "build"
		return t, mode
	return None, None


def prior_boundary_sha(workspace: Path, tickets: list[Ticket], ticket: Ticket) -> str | None:
	"""The commit this ticket's changes should be diffed against: the
	previous ticket's commit, or None for the first ticket in the ordered
	list (see the idx == 0 branch below for why that's None rather than
	the workspace's own root commit), or the repo's root commit as a
	fallback if some *later* ticket's immediately-prior commit is somehow
	missing. Deliberately NOT "current HEAD before invoking build_app.py"
	-- that was wrong for regate mode, where HEAD already includes this
	ticket's commit before the gate even starts.

	None until the workspace has a repo of its own. This runs before
	build_app.py -- and therefore before ensure_git_repo() -- so on ticket 1
	of a fresh pilot every git read here still resolves to the pilot dir's
	control repo. Returning that repo's root commit would hand build_app.py
	a --review-base-sha that does not exist in the workspace repo it is
	about to create, which fails commit_and_state_files_ok()'s diff as a
	non-retryable gate failure and leaves cross-model-review.ts diffing
	against an unresolvable base (it treats the non-zero exit as "nothing to
	review"). None is already the supported "caller doesn't know the
	boundary" answer for both."""
	if not has_own_git_repo(workspace):
		return None
	idx = tickets.index(ticket)
	for prior in reversed(tickets[:idx]):
		sha = commit_sha_for(workspace, prior.number)
		if sha:
			return sha
	if idx == 0:
		# The first ticket in the ordered list has no prior state to diff
		# against on any call -- not the first run_ticket() invocation
		# (already handled by the has_own_git_repo() guard above, since the
		# workspace has no repo yet), and not a later regate call either,
		# once the repo and this ticket's own commit both already exist.
		# Falling through to the root-commit fallback below in that second
		# case is wrong: with exactly one commit in a fresh workspace, that
		# root commit *is* the commit being gated, so `diff_range =
		# f"{sha}..{sha}"` in commit_and_state_files_ok() is an empty
		# self-diff that falsely reports state files untouched even when
		# they are plainly in the commit (reproduced live 2026-08-21, a
		# second calculator-pilot run, post-067c666: gate 1 failed on the
		# nested-repo bug that fix addresses, gate 2 -- the regate -- failed
		# on this one instead). None here makes the caller diff the single
		# commit against its own parent (or the empty tree, for a true root
		# commit) via plain `git show`, which build_app.py's own base_sha=
		# None fallback already does correctly.
		return None
	result = git(workspace, "rev-list", "--max-parents=0", "HEAD")
	if result.returncode == 0 and result.stdout.strip():
		return result.stdout.strip().splitlines()[0]
	return None


# Filenames that keep their literal name when staged instead of the usual
# per-ticket `NNN_` prefix: they must be importable/loadable under one
# fixed name to work at all (a Go module file, a shared Python helpers
# module later slices `from helpers import ...`), and /contract-plan's
# own convention is that only the first slice provides one -- if it did
# get the NNN_ treatment, "from helpers import ..." in every later slice
# would silently reference a module that was never actually staged under
# that name.
SHARED_MODULE_FILENAMES = {"go.mod", "go.sum", "helpers.py"}


def staged_pairs(pilot_dir: Path, workspace: Path, upto: int) -> list[tuple[Path, Path]]:
	"""(canonical_path, staged_path) pairs for every slice activated up to
	and including ticket `upto`. Only .go/.mod/.sum/.dart files are staged
	-- MANIFEST.md and other slice documentation stays control-dir-only
	(every slice names it identically, so staging it would collide).

	Test files (.go/.dart) are staged under their ticket number
	(`NNN_<name>`) so two slices that happen to pick the same ordinary
	basename -- e.g. two `handler_test.go` -- don't overwrite each other
	when flattened into one staged directory. `go.mod`/`go.sum` are the
	one exception: they must keep their literal name to be a valid Go
	module, and per /contract-plan's convention only the first slice
	provides one."""
	pairs = []
	acceptance_dir = pilot_dir / "spec" / "acceptance"
	if not acceptance_dir.exists():
		return pairs
	for slice_dir in sorted(acceptance_dir.iterdir()):
		if not slice_dir.is_dir() or not slice_dir.name.isdigit():
			continue
		if int(slice_dir.name) > upto:
			continue
		for f in sorted(slice_dir.iterdir()):
			if not f.is_file() or f.suffix not in STAGED_EXTENSIONS:
				continue
			dest_root = STAGED_EXTENSIONS[f.suffix]
			if f.name in SHARED_MODULE_FILENAMES:
				dest_name = f.name
			else:
				dest_name = f"{slice_dir.name}_{f.name}"
			pairs.append((f, workspace / dest_root / dest_name))
	return pairs


def _load_staged_manifest(workspace: Path) -> set[Path]:
	"""Reads back the set of paths `stage()` itself wrote on a prior call,
	from STAGED_MANIFEST_FILENAME. Missing or unreadable is treated as "no
	prior manifest" (e.g. the first ticket, or a pilot dir from before this
	manifest existed) -- never an error, since it only narrows what the
	next cleanup pass is allowed to touch."""
	manifest_path = workspace / STAGED_MANIFEST_FILENAME
	if not manifest_path.exists():
		return set()
	try:
		names = json.loads(manifest_path.read_text())
	except (json.JSONDecodeError, OSError):
		return set()
	return {workspace / name for name in names}


def _save_staged_manifest(workspace: Path, staged: set[Path]) -> None:
	manifest_path = workspace / STAGED_MANIFEST_FILENAME
	relative = sorted(str(path.relative_to(workspace)) for path in staged)
	manifest_path.write_text(json.dumps(relative) + "\n")


def stage(pilot_dir: Path, workspace: Path, upto: int) -> None:
	"""Materialize every active slice's staged files, then remove any file
	*this function itself staged on a prior call* that's no longer
	expected (a ticket dropping out of the active window).

	Tracked via STAGED_MANIFEST_FILENAME, not by sweeping every existing
	file of a staged extension in `acceptance/`/`app/test/` (found via a
	real GitHub Codex App review of this PR): that swept the whole
	directory as if this function owned it outright, so a normal product
	test the agent legitimately wrote alongside the oracle (e.g. `app/test/
	dashboard_screen_test.dart`) or a Python acceptance helper it added was
	silently deleted on the very next ticket's stage() call, before
	build_app.py ever got to see it -- correctness bugs disguised as a
	"stale file" cleanup. Cleanup is now scoped exactly to paths this
	function's own manifest says it wrote, so an agent-authored file with
	no entry in that manifest is never a cleanup candidate, no matter its
	extension or directory."""
	pairs = staged_pairs(pilot_dir, workspace, upto)
	expected = {staged for _, staged in pairs}
	for stale in _load_staged_manifest(workspace) - expected:
		if stale.is_file():
			stale.unlink()
	for canon, staged in pairs:
		staged.parent.mkdir(parents=True, exist_ok=True)
		# A workspace staged by a pre-ticket-prefix ticket_runner.py can
		# still hold this exact oracle file under its old, un-prefixed
		# name -- unremoved, it would compile/import alongside the
		# now-prefixed copy and duplicate every declaration in it. Narrow
		# and name-exact (unlike the old extension-wide sweep this
		# function replaces): only a bare file sharing this specific
		# canonical oracle's own basename is ever a legacy-migration
		# candidate, never an unrelated agent-authored file that happens
		# to share an extension.
		if staged.name != canon.name:
			legacy = staged.parent / canon.name
			if legacy != staged and legacy.is_file():
				legacy.unlink()
		shutil.copyfile(canon, staged)
	_save_staged_manifest(workspace, expected)
	contract_dest = workspace / "spec" / "contract.md"
	contract_dest.parent.mkdir(parents=True, exist_ok=True)
	shutil.copyfile(pilot_dir / "spec" / "contract.md", contract_dest)


def oracle_drift(pilot_dir: Path, workspace: Path, upto: int) -> list[str]:
	drifted = []
	for canon, staged in staged_pairs(pilot_dir, workspace, upto):
		if not staged.exists() or staged.read_bytes() != canon.read_bytes():
			drifted.append(str(staged.relative_to(workspace)))
	return drifted


def sha256_of(path: Path) -> str | None:
	if not path.exists():
		return None
	return hashlib.sha256(path.read_bytes()).hexdigest()


def verify_baseline_json(pilot_dir: Path) -> Path:
	return pilot_dir / "reports" / "ticket-001" / "verify-baseline.json"


def verify_baseline_dir(pilot_dir: Path) -> Path:
	return pilot_dir / "reports" / "ticket-001" / "verify-baseline"


def save_verify_baseline(pilot_dir: Path, workspace: Path) -> None:
	baseline_dir = verify_baseline_dir(pilot_dir)
	baseline_dir.mkdir(parents=True, exist_ok=True)
	hashes: dict[str, str | None] = {}
	for rel in VERIFY_SURFACE_FILES:
		src = workspace / rel
		hashes[rel] = sha256_of(src)
		if src.exists():
			(baseline_dir / rel.replace("/", "__")).write_bytes(src.read_bytes())
	verify_baseline_json(pilot_dir).write_text(json.dumps(hashes, indent=2))


def restore_verify_baseline(pilot_dir: Path, workspace: Path) -> None:
	baseline_dir = verify_baseline_dir(pilot_dir)
	for rel in VERIFY_SURFACE_FILES:
		src = baseline_dir / rel.replace("/", "__")
		if src.exists():
			dest = workspace / rel
			dest.parent.mkdir(parents=True, exist_ok=True)
			dest.write_bytes(src.read_bytes())


def check_verify_surface_frozen(pilot_dir: Path, workspace: Path) -> tuple[bool, str]:
	baseline_path = verify_baseline_json(pilot_dir)
	if not baseline_path.exists():
		return False, "no verify-baseline.json recorded yet -- ticket 001 must gate-pass first"
	try:
		baseline = json.loads(baseline_path.read_text())
	except (json.JSONDecodeError, OSError) as exc:
		return False, f"could not read verify-baseline.json: {exc}"
	changed = [f for f in VERIFY_SURFACE_FILES if baseline.get(f) != sha256_of(workspace / f)]
	if changed:
		return False, f"verify surface changed since ticket 001's frozen baseline: {changed}"
	return True, "unchanged since ticket 001"


AMEND_CANON_LOG = "AMEND_CANON_LOG.md"


def amend_canon(
	pilot_dir: Path,
	workspace: Path,
	tickets: list[Ticket],
	target: Path,
	reason: str,
	auto_yes: bool,
) -> int:
	"""Human-invoked rescue: accept the workspace's current version of a
	frozen file (a verify-surface script or a staged acceptance oracle) as
	the new canon.

	The verify-surface-frozen and oracle-integrity gates are right to
	distrust any agent-authored change to these files without review --
	but when the change turns out to be a genuine fix to a bug in the
	canonical artifact itself (not the app), accepting it was, before this
	command existed, pure archaeology: grep the build session's .jsonl
	transcript for the agent's edit tool call, hand-recover the diff,
	hand-update reports/ticket-001/verify-baseline's hash and byte copy or
	the spec/acceptance/NNN/ canonical file. That's exactly what tickets
	010 and 012 of the 2026-08-20 budget-pilot needed by hand. This is the
	explicit, logged, human-invoked escape hatch that design always
	intended to need -- it does not weaken the gate itself, which still
	refuses any agent-authored drift it hasn't been told about here.
	"""
	target = target.resolve()
	try:
		rel = target.relative_to(workspace.resolve())
	except ValueError:
		print(f"amend-canon target must be a path inside the workspace: {target}", file=sys.stderr)
		return 1
	rel_str = str(rel)

	if rel_str in VERIFY_SURFACE_FILES:
		kind = "verify-surface"
		canon_path = verify_baseline_dir(pilot_dir) / rel_str.replace("/", "__")
	else:
		kind = "acceptance-oracle"
		upto = max((t.number for t in tickets), default=0)
		canon_path = next(
			(canon for canon, staged in staged_pairs(pilot_dir, workspace, upto) if staged.resolve() == target),
			None,
		)
		if canon_path is None:
			print(
				f"{rel_str} is not a recognized frozen file (not one of {VERIFY_SURFACE_FILES} "
				"and not a staged acceptance slice) -- nothing to amend",
				file=sys.stderr,
			)
			return 1

	if not target.exists():
		print(f"workspace file does not exist: {target}", file=sys.stderr)
		return 1
	if not reason.strip():
		print("--reason is required: explain why the workspace version is the correct canon", file=sys.stderr)
		return 1

	new_bytes = target.read_bytes()
	old_bytes = canon_path.read_bytes() if canon_path.exists() else b""
	if new_bytes == old_bytes:
		print(f"{rel_str}: workspace already matches canon, nothing to amend")
		return 0

	diff = "".join(difflib.unified_diff(
		old_bytes.decode(errors="replace").splitlines(keepends=True),
		new_bytes.decode(errors="replace").splitlines(keepends=True),
		fromfile=f"canon/{rel_str}", tofile=f"workspace/{rel_str}",
	))
	print(f"=== amend-canon: {kind} :: {rel_str} ===")
	print(diff or "(binary or non-text change -- no line diff to show)")
	if not auto_yes:
		answer = input(f"Accept this as the new canonical {rel_str}? [y/N] ").strip().lower()
		if answer != "y":
			print("aborted, canon unchanged")
			return 1

	canon_path.parent.mkdir(parents=True, exist_ok=True)
	canon_path.write_bytes(new_bytes)
	if kind == "verify-surface":
		baseline_json = verify_baseline_json(pilot_dir)
		data = json.loads(baseline_json.read_text()) if baseline_json.exists() else {}
		data[rel_str] = sha256_of(target)
		baseline_json.write_text(json.dumps(data, indent=2))

	stamp = datetime.now(timezone.utc).isoformat()
	log_path = pilot_dir / AMEND_CANON_LOG
	existing = log_path.read_text(errors="ignore") if log_path.exists() else ""
	block = (
		f"\n\n## {rel_str} ({kind}) -- {stamp}\n\n"
		f"Reason: {reason}\n\n"
		f"- old sha256: {hashlib.sha256(old_bytes).hexdigest() if old_bytes else '(none)'}\n"
		f"- new sha256: {hashlib.sha256(new_bytes).hexdigest()}\n"
	)
	log_path.write_text(existing + block)
	print(f"canon updated: {rel_str} -- logged to {log_path}")
	return 0


def run_make(workspace: Path, target: str, timeout: float) -> tuple[bool, str]:
	try:
		result = sh(["make", target], cwd=workspace, timeout=timeout)
		return result.returncode == 0, (result.stdout + "\n" + result.stderr)[-6000:]
	except subprocess.TimeoutExpired as exc:
		out = text_output(exc.stdout) + "\n" + text_output(exc.stderr)
		return False, f"`make {target}` timed out after {timeout:.0f}s\n{out[-4000:]}"


def run_shell_command(workspace: Path, command: str, timeout: float) -> tuple[bool, str]:
	"""Runs an arbitrary shell command (e.g. a ticket's own declared
	Verify-Command:) via `sh -c`, the same invocation convention
	`internal/ticketspec`'s own canonical_verify already uses on the Go
	side -- so `&&` works directly, matching run_make's own return shape
	(ok, tail-of-output) for a caller that needs to treat the two
	interchangeably (see run_ticket's own verify_command branch)."""
	try:
		result = sh(["sh", "-c", command], cwd=workspace, timeout=timeout)
		return result.returncode == 0, (result.stdout + "\n" + result.stderr)[-6000:]
	except subprocess.TimeoutExpired as exc:
		out = text_output(exc.stdout) + "\n" + text_output(exc.stderr)
		return False, f"verify command timed out after {timeout:.0f}s\n{out[-4000:]}"


def build_report_succeeded(
	workspace: Path,
	review_policy: str = DEFAULT_REVIEW_POLICY,
) -> tuple[bool, str]:
	report = workspace / "BUILD_REPORT.md"
	if not report.exists():
		return False, "BUILD_REPORT.md not found"
	text = report.read_text(errors="ignore")
	line = next((l for l in text.splitlines() if l.startswith("Outcome:")), "")
	if not line.startswith("Outcome: SUCCEEDED"):
		return False, line or "no Outcome line found"
	policy_line = next((l for l in text.splitlines() if l.startswith("Review policy:")), "")
	if not policy_line:
		recorded_policy = "required"
	else:
		match = re.fullmatch(r"Review policy: `([^`]+)`", policy_line)
		recorded_policy = match.group(1) if match and match.group(1) in REVIEW_POLICY_STRENGTH else None
	if recorded_policy is None:
		return False, f"unrecognized review policy evidence: {policy_line}"
	if REVIEW_POLICY_STRENGTH[recorded_policy] < REVIEW_POLICY_STRENGTH[review_policy]:
		return False, f"report review policy {recorded_policy!r} is weaker than requested {review_policy!r}"
	return True, line


def commit_and_state_files_ok(workspace: Path, ticket: Ticket, base_sha: str | None) -> tuple[bool, str]:
	commit_sha = commit_sha_for(workspace, ticket.number)
	if not commit_sha:
		return False, f"no commit found matching ^ticket({ticket.nnn}):"
	diff_range = f"{base_sha}..{commit_sha}" if base_sha else commit_sha
	diff = git(workspace, "diff", "--name-only", diff_range) if base_sha else git(workspace, "show", "--name-only", "--format=", commit_sha)
	if diff.returncode != 0:
		return False, f"could not diff {diff_range}: {diff.stderr}"
	touched = set(diff.stdout.splitlines())
	missing = {"ARCHITECTURE.md", "PROGRESS.md"} - touched
	if missing:
		return False, f"commit {commit_sha[:12]} did not touch {sorted(missing)}"
	return True, f"commit {commit_sha[:12]} ok"


def text_output(value: str | bytes | None) -> str:
	"""Normalizes a subprocess output stream to str, tolerating bytes.

	subprocess.Popen(..., text=True)/subprocess.run(..., text=True) decode
	stdout/stderr to str on a normal return, but a subprocess.TimeoutExpired
	raised mid-communicate() carries whatever partial output it had already
	buffered *before* that decoding step runs -- raw bytes, despite
	text=True (a real, documented CPython quirk, not a bug in the caller).
	Every TimeoutExpired.stdout/.stderr this file reads must go through
	this, not be concatenated with a str directly (found via a real GitHub
	Codex App review of this PR on run_make(): the same bytes-vs-str gap
	invoke_build_app already guarded against for its own TimeoutExpired
	handling was still open here, and `(exc.stdout or "") + "\\n" + ...`
	raises TypeError against a real bytes value, crashing the runner
	instead of returning the failed-gate result this function promises)."""
	if value is None:
		return ""
	if isinstance(value, bytes):
		return value.decode(errors="replace")
	return value


def invoke_build_app(build_cmd: list[str], timeout: float, *, cwd: Path | None = None) -> tuple[int, str, str, bool]:
	"""Returns (returncode, stdout, stderr, timed_out). A timeout is
	reported as a failed invocation, never an uncaught exception that
	would crash the runner with no gate archived and no halt recorded.

	`cwd` defaults to None (inherit the caller's own process cwd), which is
	fine for the build_app.py/ticket_runner.py invocations this function was
	written for -- they take --workspace as an explicit argument and never
	rely on ambient cwd internally. It is not fine for a bare `pi` invocation
	(goal_pilot.py's run_pi_prompt()): `pi` has no --cwd flag, so its bash
	tool operates directly on whatever directory this process happened to
	start from -- silently the caller's own dev checkout if goal_pilot.py was
	launched from inside it, not the pilot dir. goal_pilot.py passes `cwd`
	explicitly for exactly that reason."""
	process = subprocess.Popen(
		build_cmd,
		cwd=cwd,
		text=True,
		stdin=subprocess.DEVNULL,
		stdout=subprocess.PIPE,
		stderr=subprocess.PIPE,
		start_new_session=True,
	)

	try:
		stdout, stderr = process.communicate(timeout=timeout)
		return process.returncode, text_output(stdout), text_output(stderr), False
	except subprocess.TimeoutExpired as exc:
		stdout = exc.stdout
		stderr = exc.stderr
		try:
			os.killpg(process.pid, signal.SIGTERM)
		except ProcessLookupError:
			pass
		try:
			term_stdout, term_stderr = process.communicate(timeout=10)
			stdout = term_stdout or stdout
			stderr = term_stderr or stderr
		except subprocess.TimeoutExpired as term_exc:
			stdout = term_exc.stdout or stdout
			stderr = term_exc.stderr or stderr
		# The direct parent may exit promptly on SIGTERM while a nested agent or
		# command ignores it. Always address the original process group with
		# SIGKILL after the grace period before permitting a retry.
		try:
			os.killpg(process.pid, signal.SIGKILL)
		except ProcessLookupError:
			pass
		if process.poll() is None:
			try:
				kill_stdout, kill_stderr = process.communicate(timeout=5)
				stdout = kill_stdout or stdout
				stderr = kill_stderr or stderr
			except subprocess.TimeoutExpired:
				process.kill()
				kill_stdout, kill_stderr = process.communicate()
				stdout = kill_stdout or stdout
				stderr = kill_stderr or stderr
		return -1, text_output(stdout), text_output(stderr), True


def builder_command(
	workspace: Path,
	ticket: Ticket,
	base_sha: str | None,
	review_policy: str = DEFAULT_REVIEW_POLICY,
) -> list[str]:
	cmd = [
		sys.executable, str(BUILD_APP),
		"--workspace", str(workspace),
		"--spec", str(ticket.path),
		"--max-rounds", str(MAX_BUILDER_ROUNDS),
		"--timeout-minutes", "60",
		"--review-policy", review_policy,
	]
	# Anchors the independent reviewer's diff scope to this ticket's real
	# starting commit rather than build_app.py's own default of "HEAD when
	# this process happens to start" -- without it, a retried invocation
	# against work an earlier, interrupted attempt already committed sees an
	# empty diff and can never get a decisive review verdict for a diff
	# nobody actually reviewed. base_sha is None only for ticket 1 in a repo
	# with no root commit yet; build_app.py's own fallback covers that.
	if base_sha:
		cmd += ["--review-base-sha", base_sha]
	return cmd


def append_halt_record(workspace: Path, ticket: Ticket, reasons: list[str]) -> None:
	progress = workspace / "PROGRESS.md"
	existing = progress.read_text(errors="ignore") if progress.exists() else ""
	stamp = datetime.now(timezone.utc).isoformat()
	block = (
		f"\n\n## [runner-written] HALT at ticket {ticket.nnn} ({stamp})\n\n"
		f"ticket_runner.py stopped the line here. Gate failures:\n"
		+ "".join(f"- {r}\n" for r in reasons)
		+ "\nThis section is written by the runner, not the agent, and is "
		"never trusted as ticket-completion evidence -- it exists only as "
		"a human-readable record alongside the archived gate log.\n"
	)
	progress.write_text(existing + block)


def archive_evidence(
	pilot_dir: Path,
	ticket: Ticket,
	gate_result: dict,
	*,
	gate_attempt: int,
	build_attempt: int | None,
) -> None:
	dest = pilot_dir / "reports" / f"ticket-{ticket.nnn}"
	dest.mkdir(parents=True, exist_ok=True)
	report = pilot_dir / "workspace" / "BUILD_REPORT.md"
	if report.exists():
		shutil.copyfile(report, dest / "BUILD_REPORT.md")
		report_name = (
			f"BUILD_REPORT-attempt-{build_attempt:02d}.md"
			if build_attempt is not None
			else f"BUILD_REPORT-regate-{gate_attempt:02d}.md"
		)
		write_once(dest / report_name, report.read_bytes())
	(dest / "gate.json").write_text(json.dumps(gate_result, indent=2))
	log_lines = [
		f"# Gate log -- ticket {ticket.nnn}",
		f"review policy: {gate_result.get('review_policy', 'unknown')}",
		f"passed: {gate_result['passed']}",
		"",
	]
	for check in gate_result["checks"]:
		log_lines.append(f"## {check['name']}: {'PASS' if check['ok'] else 'FAIL'}")
		if check.get("detail"):
			log_lines.append("```")
			log_lines.append(str(check["detail"])[:4000])
			log_lines.append("```")
	log_text = "\n".join(log_lines)
	(dest / "gate.log").write_text(log_text)
	write_once(dest / f"gate-attempt-{gate_attempt:02d}.json", json.dumps(gate_result, indent=2))
	write_once(dest / f"gate-attempt-{gate_attempt:02d}.log", log_text)


def run_ticket(
	pilot_dir: Path,
	workspace: Path,
	ticket: Ticket,
	tickets: list[Ticket],
	*,
	skip_build: bool,
	gate_attempt: int,
	build_attempt: int | None,
	review_policy: str,
	verify_command: str | None = None,
) -> bool:
	mode_label = "regate only" if skip_build else ("build retry" if build_attempt and build_attempt > 1 else "build")
	print(f"\n=== ticket {ticket.nnn}: {ticket.slug} ({mode_label}, gate {gate_attempt}) ===")
	stage(pilot_dir, workspace, ticket.number)
	base_sha = prior_boundary_sha(workspace, tickets, ticket)

	checks: list[dict] = []
	report_dir = pilot_dir / "reports" / f"ticket-{ticket.nnn}"
	report_dir.mkdir(parents=True, exist_ok=True)
	gate_started = report_dir / f"gate-attempt-{gate_attempt:02d}.started.json"
	if not gate_started.exists():
		write_once(
			gate_started,
			json.dumps({"ticket": ticket.nnn, "started": datetime.now(timezone.utc).isoformat()}),
		)

	if skip_build:
		print("commit already exists but no passing gate record -- re-gating only, not invoking build_app.py (rescue flow)")
	else:
		if build_attempt is None:
			raise ValueError("build_attempt is required when invoking build_app.py")
		# A stale BUILD_REPORT.md from a previous ticket/run must never be
		# able to satisfy THIS run's "reports SUCCEEDED" check -- e.g. if
		# build_app.py crashes or is killed by our own timeout below before
		# writing a fresh one.
		report_path = workspace / "BUILD_REPORT.md"
		if report_path.exists():
			report_path.unlink()

		build_cmd = builder_command(workspace, ticket, base_sha, review_policy)
		print(f"running: {' '.join(build_cmd)}")
		build_started = report_dir / f"build-attempt-{build_attempt:02d}.started.json"
		if not build_started.exists():
			write_once(
				build_started,
				json.dumps({"ticket": ticket.nnn, "started": datetime.now(timezone.utc).isoformat()}),
			)
		returncode, stdout, stderr, timed_out = invoke_build_app(build_cmd, BUILD_APP_TIMEOUT_S)
		print(stdout[-2000:])
		if timed_out:
			print(f"build_app.py timed out after {BUILD_APP_TIMEOUT_S}s", file=sys.stderr)
			checks.append({"name": "build_app.py invocation", "ok": False, "detail": f"timed out after {BUILD_APP_TIMEOUT_S}s"})
		elif returncode != 0:
			print(f"build_app.py exited {returncode}", file=sys.stderr)
			checks.append({"name": "build_app.py invocation", "ok": False, "detail": f"exited with code {returncode}"})
		build_log = (
			f"$ {' '.join(build_cmd)}\ntimed_out: {timed_out}\nreturncode: {returncode}\n\n"
			f"--- stdout ---\n{stdout}\n\n--- stderr ---\n{stderr}\n"
		)
		(report_dir / "build.log").write_text(build_log)
		write_once(report_dir / f"build-attempt-{build_attempt:02d}.log", build_log)

	# verify_command, when given, replaces BOTH `make verify` and `make
	# verify-full` with the caller's own single resolved command (found
	# via a real brownfield /spec-plan + ticket build against
	# example-app, 2026-09-10/11): those two hardcoded Make targets
	# are a convention every app this pipeline scaffolds from scratch
	# defines, but an existing repo being onboarded has no reason to
	# define a `verify-full` target at all -- requiring it unconditionally
	# here, independent of whatever Verify-Command: a ticket declares or
	# goal_pilot.py's own --verify-command resolves, silently defeated
	# that override for every run that actually reaches this gate (the
	# ticket's own declared Verify-Command: only ever reached
	# factoryd's external canonical_verify gate, never this one). There is
	# no natural second brownfield-scoped tier to run as "the full suite"
	# distinct from "verify" when the caller already named one explicit
	# command, so this runs it once, not twice.
	if verify_command:
		verify_ok, verify_out = run_shell_command(workspace, verify_command, timeout=40 * 60)
		checks.append({"name": "verify command", "ok": verify_ok, "detail": verify_out})
	else:
		verify_ok, verify_out = run_make(workspace, "verify", timeout=25 * 60)
		checks.append({"name": "make verify", "ok": verify_ok, "detail": verify_out})

		verify_full_ok, verify_full_out = (False, "skipped: make verify failed")
		if verify_ok:
			verify_full_ok, verify_full_out = run_make(workspace, "verify-full", timeout=40 * 60)
		checks.append({"name": "make verify-full", "ok": verify_full_ok, "detail": verify_full_out})

	drift = oracle_drift(pilot_dir, workspace, ticket.number)
	checks.append({"name": "oracle integrity", "ok": not drift, "detail": drift})

	frozen_ok = True
	if ticket.number != 1:
		frozen_ok, frozen_detail = check_verify_surface_frozen(pilot_dir, workspace)
		checks.append({"name": "verify-surface frozen", "ok": frozen_ok, "detail": frozen_detail})

	report_ok, report_detail = build_report_succeeded(workspace, review_policy)
	checks.append({"name": "BUILD_REPORT.md SUCCEEDED", "ok": report_ok, "detail": report_detail})

	commit_ok, commit_detail = commit_and_state_files_ok(workspace, ticket, base_sha)
	checks.append({"name": "ticket commit + state files", "ok": commit_ok, "detail": commit_detail})

	passed = all(c["ok"] for c in checks)

	if ticket.number == 1 and passed:
		save_verify_baseline(pilot_dir, workspace)

	gate_result = {
		"ticket": ticket.nnn,
		"review_policy": review_policy,
		"passed": passed,
		"checks": checks,
	}
	archive_evidence(
		pilot_dir,
		ticket,
		gate_result,
		gate_attempt=gate_attempt,
		build_attempt=build_attempt,
	)

	if not passed:
		reasons = [c["name"] for c in checks if not c["ok"]]
		recoverable = retryable_build_state(pilot_dir, workspace, ticket)
		if drift:
			print(f"oracle drift detected, restoring canonical copies: {drift}")
			stage(pilot_dir, workspace, ticket.number)
		if ticket.number != 1 and not frozen_ok:
			print("verify-surface drift detected, restoring ticket 001's frozen baseline")
			restore_verify_baseline(pilot_dir, workspace)
		if recoverable:
			print(
				f"recoverable build evidence failure for ticket {ticket.nnn}: "
				f"{', '.join(reasons)}",
				file=sys.stderr,
			)
		else:
			append_halt_record(workspace, ticket, reasons)
			print(f"GATE FAILED for ticket {ticket.nnn}: {', '.join(reasons)}", file=sys.stderr)
	else:
		print(f"GATE PASSED for ticket {ticket.nnn}")
	return passed


def gate_revisit_count(pilot_dir: Path, ticket: Ticket) -> int:
	"""How many times this ticket's gate produced a real (non-`.started`)
	gate-attempt-*.json record. This is a raw diagnostic, not a rescue
	signal: build_app.py's own bounded build-attempt retry (a transient
	marker like a route outage or a stall-timeout) can legitimately
	archive more than one real gate-attempt on its way to an automatic
	pass, with no human ever involved (Codex review of PR #29 flagged
	`print_status()`'s earlier use of this count as a false-positive
	rescue proxy for exactly that reason). Use `halt_record_exists()` to
	ask whether a ticket actually needed a human."""
	reports = pilot_dir / "reports" / f"ticket-{ticket.nnn}"
	if not reports.exists():
		return 0
	return sum(
		1 for p in reports.glob("gate-attempt-*.json")
		if not p.name.endswith(".started.json")
	)


def halt_record_exists(workspace: Path, ticket: Ticket) -> bool:
	"""True if PROGRESS.md contains a runner-written HALT record for this
	ticket -- append_halt_record() writes one only on the non-recoverable
	path, i.e. only when retryable_build_state() found no automatic/
	transient-retry route and the line genuinely stopped for a human.
	Unlike a raw gate-attempt count, this structurally excludes tickets
	that passed purely through build_app.py's bounded automatic retry
	(each retried attempt can still archive a real, non-`.started`
	gate-attempt-*.json -- see gate_revisit_count()'s docstring)."""
	progress = workspace / "PROGRESS.md"
	if not progress.exists():
		return False
	return f"HALT at ticket {ticket.nnn} (" in progress.read_text(errors="ignore")


def print_status(
	pilot_dir: Path,
	tickets: list[Ticket],
	workspace: Path,
	review_policy: str = DEFAULT_REVIEW_POLICY,
) -> None:
	done_commits = committed_ticket_numbers(workspace)
	# The `[rescued]` commit tag is honor-system -- nothing enforces an
	# agent or human actually adds it (see the 2026-08-20 budget-pilot
	# verdict: --status reported "rescued: 1" against a true count of 5).
	# halt_record_exists() is the structural counterpart: it doesn't know
	# *why* a ticket needed a human, but it can't be forgotten to tag (and,
	# unlike a raw gate-attempt count, it doesn't fire on a ticket that
	# passed purely through automatic retry), so report both and let an
	# untagged halt stand out rather than silently undercounting.
	tagged = {n for n, subject in done_commits.items() if "[rescued]" in subject}
	halted = {t.number for t in tickets if t.number in done_commits and halt_record_exists(workspace, t)}
	rescued = tagged | halted
	nxt, mode = next_ticket(tickets, pilot_dir, workspace, review_policy)
	print(f"pilot dir: {pilot_dir}")
	print(f"tickets total: {len(tickets)}")
	print(
		f"tickets committed: {len(done_commits)} "
		f"(of which rescued: {len(rescued)} -- {len(tagged)} tagged, "
		f"{len(halted - tagged)} untagged halts)"
	)
	if nxt is None:
		print("next ticket: (none -- all tickets complete)")
	else:
		print(f"next ticket: {nxt.nnn}-{nxt.slug} ({mode})")
	for t in tickets:
		done = ticket_done(pilot_dir, workspace, t, review_policy)
		has_commit = t.number in done_commits
		marker = "x" if done else ("~" if has_commit else " ")
		if has_commit and t.number in tagged:
			rescue_tag = " [rescued]"
		elif has_commit and t.number in halted:
			rescue_tag = " [rescued: untagged]"
		else:
			rescue_tag = ""
		print(f"  [{marker}] {t.nnn}-{t.slug}{rescue_tag}")
	print("  ('x' = done -- commit + passing gate; '~' = commit exists but gate not passing yet)")
	report_dir = pilot_dir / "reports"
	if report_dir.exists():
		latest = sorted(report_dir.glob("ticket-*/gate.json"))
		if latest:
			last = json.loads(latest[-1].read_text())
			print(f"last gate outcome ({last['ticket']}): {'PASS' if last['passed'] else 'FAIL'}")


def main() -> int:
	parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
	parser.add_argument("--pilot-dir", required=True, type=Path)
	parser.add_argument("--status", action="store_true", help="Read-only position report; runs nothing.")
	parser.add_argument(
		"--amend-canon",
		type=Path,
		metavar="WORKSPACE_FILE",
		help=(
			"Human rescue: accept a workspace file's current content as the new canon "
			"(a verify-surface script or a staged acceptance oracle), after showing the "
			"diff and requiring confirmation. Requires --reason; use --yes for non-interactive use."
		),
	)
	parser.add_argument("--reason", default="", help="Required with --amend-canon: why the workspace version is correct.")
	parser.add_argument("--yes", action="store_true", help="Skip the --amend-canon confirmation prompt.")
	parser.add_argument(
		"--review-policy",
		choices=("advisory", "required", "degraded"),
		default=DEFAULT_REVIEW_POLICY,
		help="Pass the review policy to build_app.py; advisory is the default, required is intended for release-hardening runs.",
	)
	parser.add_argument(
		"--stop-after-ticket",
		type=int,
		default=None,
		metavar="N",
		help=(
			"Return (exit 0) as soon as ticket N's gate settles -- pass or fail -- instead of "
			"continuing to ticket N+1 in the same process. Added for goal_pilot.py, which needs "
			"to pause for a human checkpoint right after ticket 001 without SIGTERM-ing a running "
			"build. Purely "
			"additive control flow around the main loop -- does not change what any gate checks, "
			"and a plain `make run`/no-flag invocation behaves exactly as before."
		),
	)
	parser.add_argument(
		"--verify-command",
		default=None,
		help=(
			"Overrides run_ticket()'s own hardcoded `make verify` + `make "
			"verify-full` gate with this single shell command (run via `sh "
			"-c`, same as internal/ticketspec's canonical_verify). Those two "
			"Make targets are a convention every app this pipeline scaffolds "
			"from scratch defines -- an existing repo being onboarded has no "
			"reason to define verify-full at all, so a ticket declaring a "
			"Verify-Command: (or goal_pilot.py's own --verify-command) that "
			"never reaches this gate would otherwise still fail here "
			"unconditionally. Passed through as-is from goal_pilot.py when "
			"it invokes this script as a subprocess."
		),
	)
	args = parser.parse_args()

	pilot_dir = args.pilot_dir.resolve()
	spec_dir = pilot_dir / "spec"
	workspace = pilot_dir / "workspace"
	tickets = discover_tickets(spec_dir)
	if not tickets:
		print(f"no tickets found under {spec_dir / 'tickets'}", file=sys.stderr)
		return 1

	if args.status:
		print_status(pilot_dir, tickets, workspace, args.review_policy)
		return 0

	if args.amend_canon:
		lock_path = pilot_dir / ".ticket_runner.lock"
		lock_handle = lock_path.open("w")
		try:
			fcntl.flock(lock_handle, fcntl.LOCK_EX | fcntl.LOCK_NB)
		except OSError:
			print(
				f"another ticket_runner.py is already running against {pilot_dir} "
				f"(lock held on {lock_path}); refusing to race it",
				file=sys.stderr,
			)
			return 1
		return amend_canon(pilot_dir, workspace, tickets, args.amend_canon, args.reason, args.yes)

	lock_path = pilot_dir / ".ticket_runner.lock"
	lock_handle = lock_path.open("w")
	try:
		fcntl.flock(lock_handle, fcntl.LOCK_EX | fcntl.LOCK_NB)
	except OSError:
		print(
			f"another ticket_runner.py is already running against {pilot_dir} "
			f"(lock held on {lock_path}); refusing to race it",
			file=sys.stderr,
		)
		return 1

	while True:
		t, mode = next_ticket(tickets, pilot_dir, workspace, args.review_policy)
		if t is None:
			print("\nall tickets complete.")
			return 0
		build_attempt: int | None = None
		if mode in ("build", "retry", "policy"):
			build_attempt = next_build_attempt(pilot_dir, t)
			if mode != "policy" and build_attempt > MAX_BUILD_ATTEMPTS:
				gate = read_gate(pilot_dir, t)
				reasons = sorted(failed_check_names(gate)) or ["build retry limit exhausted"]
				append_halt_record(workspace, t, reasons)
				print(
					f"build attempt limit ({MAX_BUILD_ATTEMPTS}) exhausted for ticket {t.nnn}; "
					"stopping with evidence",
					file=sys.stderr,
				)
				return 1
		if mode == "retry":
			retry_count = build_attempt - 1
			print(
				f"previous build attempt ended without report evidence; "
				f"retrying {retry_count}/{MAX_BUILD_ATTEMPTS - 1} after {BUILD_RETRY_BACKOFF_S}s",
				file=sys.stderr,
			)
			time.sleep(BUILD_RETRY_BACKOFF_S)
		gate_attempt = next_gate_attempt(pilot_dir, t)
		ok = run_ticket(
			pilot_dir,
			workspace,
			t,
			tickets,
			skip_build=(mode == "regate"),
			gate_attempt=gate_attempt,
			build_attempt=build_attempt,
			review_policy=args.review_policy,
			verify_command=args.verify_command,
		)
		if not ok:
			if mode in ("build", "retry", "policy") and retryable_build_state(pilot_dir, workspace, t):
				continue
			return 1
		if args.stop_after_ticket is not None and t.number == args.stop_after_ticket:
			print(f"\nstopping after ticket {t.nnn} as requested (--stop-after-ticket {args.stop_after_ticket}); gate passed.")
			return 0


if __name__ == "__main__":
	sys.exit(main())
