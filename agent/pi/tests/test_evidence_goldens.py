"""Drift test for internal/evidence/testdata/golden/*.json: regenerates
every golden into a temp dir via generate_evidence_goldens.generate (the
REAL writer functions, see that module's own doc comment) and asserts
byte-equality against what's committed. A writer whose current output has
moved since the golden was last regenerated fails here, not silently in
cmd/factoryd/evidence_golden_test.go's Go-side parse.
"""
from __future__ import annotations

import tempfile
import unittest
from pathlib import Path

import generate_evidence_goldens as goldens


class EvidenceGoldensDoNotDriftTest(unittest.TestCase):
	def test_generated_goldens_match_committed(self) -> None:
		with tempfile.TemporaryDirectory() as tmp:
			fresh = goldens.generate(Path(tmp))
			self.assertTrue(fresh, "generate() produced no files")
			stale = []
			for name, path in fresh.items():
				committed = goldens.GOLDEN_DIR / name
				if not committed.is_file():
					stale.append(f"{name} (missing)")
					continue
				if committed.read_text() != path.read_text():
					stale.append(name)
			if stale:
				self.fail(
					f"golden(s) out of date: {', '.join(stale)} -- "
					"run agent/pi/tests/generate_evidence_goldens.py to update"
				)

	def test_recorded_files_are_still_valid_json(self) -> None:
		# The two hand-copied real-run files (build_evidence.recorded.json,
		# conformity_evidence.recorded.json) aren't regenerated -- they're a
		# real run's actual output, scrubbed of host-identifying content.
		# This only guards against an editing mistake corrupting them.
		import json

		for name, version in (("build_evidence.recorded.json", 2), ("conformity_evidence.recorded.json", 1)):
			path = goldens.GOLDEN_DIR / name
			with self.subTest(name=name):
				data = json.loads(path.read_text())
				self.assertEqual(data.get("schema_version"), version)


if __name__ == "__main__":
	unittest.main()
