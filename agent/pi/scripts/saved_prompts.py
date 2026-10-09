"""Saves each prompt a script hands to a coding-agent harness, as sent, in the
session folder, so the operator can read what the model was told.

`save_prompt(session_dir, name, text)` writes `text` to
`<session_dir>/prompts/<name>.md` (0600, UTF-8). The host copies that folder
into the run's (or the request's) own directory before the session folder is
removed (internal/evidence/prompts.go), redacting it there. Saving is an aid:
it never raises and never changes the turn; a failure is noted in the progress
feed by its exception class name alone (a message could quote the prompt).
"""
from __future__ import annotations

import json
import os
import re
from pathlib import Path

PROMPTS_DIR = "prompts"
MAX_PROMPT_BYTES = 2 << 20
MAX_PROMPT_FILES = 50
NAME_RE = re.compile(r"^[a-z0-9-]{1,64}$")


def _note(detail: str) -> None:
	# The same line build_app.print_progress writes (not imported: this module
	# must not depend on build_app).
	try:
		print(f"FACTORY_PROGRESS {json.dumps({'stage': 'agent', 'event': 'note', 'detail': detail})}", flush=True)
	except Exception:
		pass


def _bounded(text: str) -> bytes:
	data = text.encode("utf-8", errors="replace")
	if len(data) <= MAX_PROMPT_BYTES:
		return data
	cut = len(data) - MAX_PROMPT_BYTES
	head = data[:MAX_PROMPT_BYTES].decode("utf-8", errors="ignore").encode("utf-8")
	return head + f"\n[saved prompt cut: {cut} bytes not saved]\n".encode("utf-8")


def _free_path(folder: Path, name: str) -> Path | None:
	"""folder/<name>.md, or <name>-2.md, <name>-3.md ... when a relaunch of the
	same script already saved that name; None at the file cap."""
	existing = {p.name for p in folder.iterdir()} if folder.exists() else set()
	if len([n for n in existing if n.endswith(".md")]) >= MAX_PROMPT_FILES:
		return None
	candidate, n = f"{name}.md", 1
	while candidate in existing:
		n += 1
		suffix = f"-{n}"
		candidate = f"{name[:64 - len(suffix)]}{suffix}.md"
	return folder / candidate


def save_prompt(session_dir, name: str, text: str) -> Path | None:
	"""Returns the path written, or None when nothing was saved."""
	try:
		if not NAME_RE.match(name):
			raise ValueError("bad prompt name")
		folder = Path(session_dir) / PROMPTS_DIR
		if folder.is_symlink():
			raise OSError("prompts folder is a link")
		folder.mkdir(parents=True, exist_ok=True)
		path = _free_path(folder, name)
		if path is None:
			_note("prompt not saved: more than %d prompts in this launch" % MAX_PROMPT_FILES)
			return None
		fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL | getattr(os, "O_NOFOLLOW", 0), 0o600)
		with os.fdopen(fd, "wb") as handle:
			handle.write(_bounded(text))
		return path
	except Exception as exc:
		_note(f"prompt not saved: {type(exc).__name__}")
		return None
