"""Read-only loader for the inline prompt assembly inventory.

The registry describes source locations and composition responsibilities. It
does not render prompts, load repository content, or declare runtime editing.
The JSON file beside this module is the source of truth for the metadata.
"""

from __future__ import annotations

import json
from pathlib import Path
from typing import Any


_REGISTRY_PATH = Path(__file__).with_name("prompt_sources.json")
_REPO_ROOT = Path(__file__).resolve().parents[3]
_FIELDS = {
	"id", "source_path", "fragment", "role", "stage", "kind", "template_refs", "description",
}
_ROLES = {"planning", "execution", "review", "challenger"}


def _load_registry() -> tuple[str, tuple[dict[str, Any], ...]]:
	try:
		payload = json.loads(_REGISTRY_PATH.read_text(encoding="utf-8"))
	except (OSError, json.JSONDecodeError) as exc:
		raise ValueError(f"cannot load prompt source registry {_REGISTRY_PATH.name}: {exc}") from exc
	if not isinstance(payload, dict) or payload.get("schema_version") != 1:
		raise ValueError("prompt source registry must be an object with schema_version 1")
	if not isinstance(payload.get("scope"), str) or not payload["scope"].strip():
		raise ValueError("prompt source registry scope must be a non-empty string")
	sources = payload.get("sources")
	if not isinstance(sources, list) or not sources:
		raise ValueError("prompt source registry sources must be a non-empty array")

	seen_ids: set[str] = set()
	validated = []
	for index, source in enumerate(sources):
		where = f"prompt source registry entry {index}"
		if not isinstance(source, dict) or set(source) != _FIELDS:
			raise ValueError(f"{where} must have exactly these fields: {', '.join(sorted(_FIELDS))}")
		if not isinstance(source["id"], str) or not source["id"].startswith("prompt-source:"):
			raise ValueError(f"{where} id must start with 'prompt-source:'")
		if source["id"] in seen_ids:
			raise ValueError(f"{where} has duplicate id {source['id']!r}")
		seen_ids.add(source["id"])
		for field in ("source_path", "fragment", "stage", "description"):
			if not isinstance(source[field], str) or not source[field].strip():
				raise ValueError(f"{where} {field} must be a non-empty string")
		path = Path(source["source_path"])
		if path.is_absolute() or ".." in path.parts:
			raise ValueError(f"{where} source_path must be a repository-relative path")
		if not isinstance(source["role"], str) or source["role"] not in _ROLES:
			raise ValueError(f"{where} role must be one of {', '.join(sorted(_ROLES))}")
		if source["kind"] != "inline-assembly":
			raise ValueError(f"{where} kind must be 'inline-assembly'")
		refs = source["template_refs"]
		if not isinstance(refs, list) or not all(
			isinstance(ref, str) and ref.endswith(".prompt.md") and Path(ref).name == ref for ref in refs
		):
			raise ValueError(f"{where} template_refs must contain prompt template filenames")
		for ref in refs:
			if not (_REGISTRY_PATH.parent / ref).is_file():
				raise ValueError(f"{where} references missing prompt template {ref!r}")
		validated.append(source)
	return payload["scope"], tuple(validated)


_SCOPE, _SOURCES = _load_registry()


def sources() -> list[dict[str, Any]]:
	"""Return a fresh JSON-serializable copy of the source metadata."""
	return [dict(source, template_refs=list(source["template_refs"])) for source in _SOURCES]


def to_json(*, indent: int = 2) -> str:
	"""Serialize the inventory for tooling; this is metadata, not prompt text."""
	return json.dumps({"schema_version": 1, "scope": _SCOPE, "sources": sources()}, indent=indent) + "\n"


def source_path_exists(source: dict[str, Any]) -> bool:
	"""Return whether an inventory source path exists in this checkout."""
	return (_REPO_ROOT / str(source["source_path"])).is_file()
