"""Tests for agent/pi/scripts/prompt_templates.py."""

from __future__ import annotations

import re
import sys
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parent.parent / "scripts"))
import prompt_templates  # noqa: E402


@pytest.fixture
def template_dir(tmp_path, monkeypatch):
	monkeypatch.setattr(prompt_templates, "TEMPLATE_DIR", tmp_path)
	return tmp_path


def write(template_dir: Path, text: str) -> None:
	(template_dir / "t.prompt.md").write_text(text, encoding="utf-8")


def test_load_returns_the_text_unformatted(template_dir):
	write(template_dir, "Write to {out}. JSON looks like {{}}.\n")
	assert prompt_templates.load("t", ("out",)) == "Write to {out}. JSON looks like {{}}.\n"


def test_load_refuses_a_missing_placeholder(template_dir):
	write(template_dir, "Write the file.\n")
	with pytest.raises(ValueError, match=r"t\.prompt\.md: missing placeholder\(s\) \{out\}"):
		prompt_templates.load("t", ("out",))


@pytest.mark.parametrize("text", ["Write to {out} and {elsewhere}.\n", "Write to {out}. JSON looks like {}.\n", "Write to {out}. Lone { brace.\n"])
def test_load_refuses_an_unknown_placeholder_or_lone_brace(template_dir, text):
	write(template_dir, text)
	with pytest.raises(ValueError, match=r"t\.prompt\.md: .*the only placeholders are \{out\}"):
		prompt_templates.load("t", ("out",))


def test_load_refuses_a_placeholder_written_as_a_literal(template_dir):
	# {{out}} is the literal text "{out}": the value would never reach the model.
	write(template_dir, "Write to {{out}}.\n")
	with pytest.raises(ValueError, match=r"t\.prompt\.md: missing placeholder\(s\) \{out\}"):
		prompt_templates.load("t", ("out",))


def test_load_accepts_a_placeholder_with_a_format_spec(template_dir):
	write(template_dir, "At most {count:d} files in {out!r}.\n")
	assert prompt_templates.load("t", ("count", "out")) == "At most {count:d} files in {out!r}.\n"


def test_load_text_returns_literal_text_without_its_final_newline(template_dir):
	write(template_dir, 'Answer with {"findings": []} only.\n')
	assert prompt_templates.load_text("t") == 'Answer with {"findings": []} only.'


def test_every_shipped_template_is_loaded_by_a_script():
	scripts = Path(prompt_templates.__file__).resolve().parent
	sources = "".join(p.read_text(encoding="utf-8") for p in scripts.glob("*.py"))
	for template in scripts.glob("*" + prompt_templates.TEMPLATE_SUFFIX):
		name = template.name.removesuffix(prompt_templates.TEMPLATE_SUFFIX)
		loaded = re.search(r'prompt_templates\.load(?:_text)?\(\s*"' + re.escape(name) + '"', sources)
		assert loaded, f"{template.name} is loaded by no script"
