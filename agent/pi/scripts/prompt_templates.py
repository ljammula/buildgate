"""Loads the prompt templates the harness scripts send to a model.

A template is `<name>.prompt.md` beside the scripts: the instruction text
only, with `str.format` placeholders its script fills. Keeping the text
out of the scripts lets it be read and changed as prose. The files are
embedded and staged with the scripts (internal/sandbox's
HarnessSiblingModules), so a run's HarnessScriptsSHA256 covers them too.
"""

from __future__ import annotations

import string
from pathlib import Path

TEMPLATE_DIR = Path(__file__).resolve().parent
TEMPLATE_SUFFIX = ".prompt.md"


def load(name: str, placeholders: tuple[str, ...] = ()) -> str:
	"""Returns the template's text. Raises ValueError, naming the file, when
	the text does not use exactly `placeholders`: a missing one drops a value
	the script must pass to the model, and an unknown one (or a lone brace)
	would fail later, inside `str.format`, with no file name."""
	path = TEMPLATE_DIR / f"{name}{TEMPLATE_SUFFIX}"
	text = path.read_text(encoding="utf-8")
	allowed = ", ".join("{" + p + "}" for p in placeholders) or "none"
	try:
		used = {field for _, field, _, _ in string.Formatter().parse(text) if field is not None}
	except ValueError as exc:
		raise ValueError(f"{path.name}: {exc}; the only placeholders are {allowed}, and a literal brace is written doubled") from exc
	missing = [p for p in placeholders if p not in used]
	if missing:
		raise ValueError(f"{path.name}: missing placeholder(s) " + ", ".join("{" + p + "}" for p in missing))
	unknown = sorted(used - set(placeholders))
	if unknown:
		raise ValueError(
			f"{path.name}: unknown placeholder(s) " + ", ".join("{" + p + "}" for p in unknown)
			+ f"; the only placeholders are {allowed}, and a literal brace is written doubled"
		)
	return text


def load_text(name: str) -> str:
	"""Returns a template that is literal text, used as written and never
	passed through `str.format` itself: braces in it are plain characters.
	The file's final newline is not part of the text."""
	return (TEMPLATE_DIR / f"{name}{TEMPLATE_SUFFIX}").read_text(encoding="utf-8").removesuffix("\n")
