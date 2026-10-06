#!/usr/bin/env python3
"""Regenerates internal/evidence/testdata/golden/*.json from the REAL
Python writers (build_app.write_evidence_json, conformity_review.
write_conformity_evidence_json, code_review.write_code_review_evidence_json,
draft_spec.write_evidence, plan_tickets.write_evidence,
draft_acceptance_oracles.write_evidence), called with a fixed, realistic
input and a fixed clock so the output is byte-stable across runs.

These goldens are the fixture half of the ABI test described in
cmd/factoryd/evidence_golden_test.go: that test parses the same files with
the REAL Go readers and asserts a few load-bearing fields, so a shape
drift on either side of the two-repo, unversioned-file contract shows up
as a failing test instead of a silent misparse.

test_evidence_goldens.py (this directory) regenerates into a temp dir on
every `pytest` run and asserts byte-equality against the committed
goldens -- if a writer's current output has drifted from what's
committed, that test fails with "run generate_evidence_goldens.py to
update", the drift-detection mechanism this whole ABI relies on.

Usage:
    python3 generate_evidence_goldens.py           # rewrite the committed goldens
    python3 generate_evidence_goldens.py --check   # exit 1 if they'd change
"""
from __future__ import annotations

import argparse
import importlib.util
import os
import shutil
import sys
from datetime import datetime, timezone
from pathlib import Path
from unittest import mock

SCRIPTS_DIR = Path(__file__).resolve().parents[1] / "scripts"
GOLDEN_DIR = Path(__file__).resolve().parents[3] / "internal" / "evidence" / "testdata" / "golden"

# FIXED_NOW is the single timestamp every generated golden's "generated"
# field carries -- arbitrary, but stable across regenerations so a
# re-run with no real content change produces byte-identical output.
FIXED_NOW = datetime(2026, 1, 1, 0, 0, 0, tzinfo=timezone.utc)


def _load(name: str):
	script = SCRIPTS_DIR / f"{name}.py"
	spec = importlib.util.spec_from_file_location(name, script)
	assert spec and spec.loader
	module = importlib.util.module_from_spec(spec)
	sys.modules[spec.name] = module
	spec.loader.exec_module(module)
	return module


class _FixedDatetime(datetime):
	"""Replaces a module's own `datetime` name so its
	`datetime.now(timezone.utc)` calls return FIXED_NOW -- each writer
	under test imported `datetime`/`timezone` directly (`from datetime
	import datetime, timezone`), so patching the module-level name is
	enough; no mock library needed for a single classmethod override."""

	@classmethod
	def now(cls, tz=None):
		return FIXED_NOW


def _freeze_clock(module) -> None:
	module.datetime = _FixedDatetime


def generate(out_dir: Path) -> dict[str, Path]:
	"""Writes all six golden files into out_dir and returns {name: path}."""
	out_dir.mkdir(parents=True, exist_ok=True)
	written: dict[str, Path] = {}

	# Each fixed-name writer (BUILD_EVIDENCE.json, CONFORMITY_EVIDENCE.json,
	# CODE_REVIEW_EVIDENCE.json) gets its own scratch subdirectory before
	# its output is moved to out_dir under this repo's lowercase golden
	# name: on a case-insensitive filesystem (the macOS default),
	# "BUILD_EVIDENCE.json" and "build_evidence.json" are the SAME file,
	# so writing and renaming within out_dir itself would read the file
	# back, "write" it to itself, and then unlink the only copy.
	build_scratch = out_dir / "_scratch_build"
	build_scratch.mkdir(exist_ok=True)

	# 1. BUILD_EVIDENCE.json, via build_app.write_evidence_json.
	build_app = _load("build_app")
	_freeze_clock(build_app)
	result = build_app.BuildResult(
		workspace=build_scratch,
		spec_path=Path("ticket.spec.md"),
		review_policy="advisory",
		conformity_policy="required",
		agents_md_used=True,
		agents_md_git_blob="f55d748f1f4a5821f806fe1809936f827ea088e8",
		review_verdicts=[],
		succeeded=True,
		stopped_reason="canonical verification passed; advisory review (unavailable: no-review-verdict)",
		rounds=[
			build_app.Round(
				index=1,
				agent="pi",
				command=["pi", "chat", "--json"],
				agent_returncode=0,
				agent_timed_out=False,
				usage={"input": 26852, "output": 2835, "cacheRead": 49664, "cacheWrite": 0, "reasoning": 933, "totalTokens": 79351},
				traces=[],
				reviewer=build_app.ReviewSignal("unavailable", "no-review-verdict"),
				verify_command="make verify",
				verify_passed=True,
				verify_timed_out=False,
				verify_output_tail="",
				duration_s=210.255867137108,
			),
		],
	)
	with mock.patch.dict(os.environ, {"PI_HARNESS_PROVIDER": "factoryd-relay", "PI_HARNESS_MODEL": "gpt-5.6-luna"}):
		path = build_app.write_evidence_json(result)
	target = out_dir / "build_evidence.json"
	target.write_text(path.read_text())
	written["build_evidence.json"] = target

	# 2. CONFORMITY_EVIDENCE.json, via
	# conformity_review.write_conformity_evidence_json. The evidence dict
	# is built by hand (mirroring run_conformity_review's own shape)
	# rather than through run_conformity_review itself, which requires a
	# live model turn -- the writer under test is the file-write, not the
	# review pipeline.
	conformity_scratch = out_dir / "_scratch_conformity"
	conformity_scratch.mkdir(exist_ok=True)
	conformity_review = _load("conformity_review")
	conformity_evidence = {
		"schema_version": conformity_review.CONFORMITY_EVIDENCE_SCHEMA_VERSION,
		"generated": FIXED_NOW.isoformat(),
		"conformity_policy": "required",
		"thinking": "max",
		"succeeded": False,
		"stopped_reason": "spec conformity review flagged 1 of 2 criteria",
		"error": "",
		"review_verdicts": [
			{"criterion": "1. Returns 200 on success.", "verdict": "clean", "detail": ""},
			{"criterion": "2. Handles empty payload.", "verdict": "flagged", "detail": "crashes on nil"},
		],
	}
	path = conformity_review.write_conformity_evidence_json(conformity_scratch, conformity_evidence)
	target = out_dir / "conformity_evidence.json"
	target.write_text(path.read_text())
	written["conformity_evidence.json"] = target

	# 3. CODE_REVIEW_EVIDENCE.json, via
	# code_review.write_code_review_evidence_json -- same "hand-built
	# evidence dict, real file-writer" split as conformity above.
	code_review_scratch = out_dir / "_scratch_code_review"
	code_review_scratch.mkdir(exist_ok=True)
	code_review = _load("code_review")
	code_review_evidence = {
		"schema_version": code_review.CODE_REVIEW_EVIDENCE_SCHEMA_VERSION,
		"generated": FIXED_NOW.isoformat(),
		"review_policy": "required",
		"thinking": None,
		"available": True,
		"succeeded": False,
		"stopped_reason": "code review found 1 blocking high-severity finding(s)",
		"error": "",
		"findings": [
			{
				"severity": "high",
				"file": "internal/foo/bar.go",
				"line": 42,
				"summary": "Nil pointer dereference on empty input.",
				"failure_scenario": "Calling Bar(nil) panics instead of returning an error.",
			},
		],
	}
	path = code_review.write_code_review_evidence_json(code_review_scratch, code_review_evidence)
	target = out_dir / "code_review_evidence.json"
	target.write_text(path.read_text())
	written["code_review_evidence.json"] = target

	usage = {"input": 200, "output": 80, "cacheRead": 0, "cacheWrite": 0, "reasoning": 0, "totalTokens": 280}

	# 4. Spec draft evidence.json, via draft_spec.write_evidence.
	draft_spec = _load("draft_spec")
	_freeze_clock(draft_spec)
	target = out_dir / "spec_draft_evidence.json"
	draft_spec.write_evidence(
		target, usage=usage, agent_exit_code=0, duration_s=45.2,
		agents_md_used=True, thinking="high",
	)
	written["spec_draft_evidence.json"] = target

	# 5. Plan evidence.json, via plan_tickets.write_evidence.
	plan_tickets = _load("plan_tickets")
	_freeze_clock(plan_tickets)
	target = out_dir / "plan_evidence.json"
	plan_tickets.write_evidence(
		target, usage=usage, agent_exit_code=0, duration_s=63.4,
		agents_md_used=True, thinking="high",
	)
	written["plan_evidence.json"] = target

	# 6. Oracle draft evidence.json, via
	# draft_acceptance_oracles.write_evidence.
	draft_acceptance_oracles = _load("draft_acceptance_oracles")
	_freeze_clock(draft_acceptance_oracles)
	target = out_dir / "oracle_draft_evidence.json"
	draft_acceptance_oracles.write_evidence(
		target, usage=usage, agent_exit_code=0, duration_s=90.0,
		agents_md_used=True, criteria_count=3, oracle_count=2,
		status="drafted", dropped_count=0, feedback_truncated=False,
		failure_reason=None,
		failures=[{"criterion_index": 3, "reason": "model produced no usable oracle"}],
		salvaged_count=1, thinking="max",
	)
	written["oracle_draft_evidence.json"] = target

	# The three fixed-name writers' own scratch subdirectories are no
	# longer needed once their content is copied into out_dir under this
	# repo's golden names -- never left behind in a committed golden dir.
	shutil.rmtree(build_scratch)
	shutil.rmtree(conformity_scratch)
	shutil.rmtree(code_review_scratch)

	return written


def main() -> int:
	parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
	parser.add_argument("--check", action="store_true", help="Exit 1 if regenerating would change any committed golden, without writing.")
	args = parser.parse_args()

	if args.check:
		import tempfile
		with tempfile.TemporaryDirectory() as tmp:
			fresh = generate(Path(tmp))
			changed = []
			for name, path in fresh.items():
				committed = GOLDEN_DIR / name
				if not committed.is_file() or committed.read_text() != path.read_text():
					changed.append(name)
			if changed:
				print(f"out of date: {', '.join(changed)} -- run generate_evidence_goldens.py to update", file=sys.stderr)
				return 1
			print("goldens up to date")
			return 0

	written = generate(GOLDEN_DIR)
	for name in written:
		print(f"wrote {GOLDEN_DIR / name}")
	return 0


if __name__ == "__main__":
	sys.exit(main())
