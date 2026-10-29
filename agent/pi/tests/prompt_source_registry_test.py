"""Tests for the read-only inline prompt source inventory."""

from __future__ import annotations

import json
import sys
from pathlib import Path


SCRIPTS = Path(__file__).resolve().parents[1] / "scripts"
sys.path.insert(0, str(SCRIPTS))

import prompt_source_registry  # noqa: E402


def test_inventory_is_json_serializable_and_sources_exist():
	encoded = prompt_source_registry.to_json()
	data = json.loads(encoded)

	assert data["schema_version"] == 1
	assert "Harness-native" in data["scope"] and "out of scope" in data["scope"]
	assert data["sources"] == prompt_source_registry.sources()
	assert data["sources"]
	assert all(prompt_source_registry.source_path_exists(source) for source in data["sources"])

	for source in data["sources"]:
		assert set(source) == {
			"id", "source_path", "fragment", "role", "stage", "kind", "template_refs", "description",
		}
		assert source["id"].startswith("prompt-source:")
		assert isinstance(source["fragment"], str) and source["fragment"]
		assert source["role"] in {"planning", "execution", "review", "challenger"}
		assert isinstance(source["stage"], str) and source["stage"]
		assert source["kind"] == "inline-assembly"
		assert isinstance(source["description"], str) and source["description"]
		assert isinstance(source["template_refs"], list)
		assert prompt_source_registry.source_path_exists(source)
		for template in source["template_refs"]:
			assert (SCRIPTS / template).is_file()


def test_inventory_has_stable_unique_ids_and_does_not_expose_prompt_text():
	first = prompt_source_registry.sources()
	ids = [source["id"] for source in first]
	assert len(ids) == len(set(ids))
	assert {
		"prompt-source:draft-spec.context",
		"prompt-source:draft-agents.context",
		"prompt-source:plan-tickets.context",
		"prompt-source:build-corrective.context",
		"prompt-source:combined-review",
		"prompt-source:review-instructions-diff",
	} <= set(ids)

	first[0]["template_refs"].append("caller mutation")
	assert "caller mutation" not in prompt_source_registry.sources()[0]["template_refs"]
	assert all("text" not in source for source in prompt_source_registry.sources())
