# AGENTS.md

One Python module, `add.py`, with no dependencies outside the standard
library. Tests run under pytest installed in a virtual environment at
`venv/`, as many Python repositories do; the system Python has no pytest.

## Commands

- Setup: `python3 -m venv venv && venv/bin/pip install pytest`
- Test: `venv/bin/python -m pytest -q tests`.
- Build and lint: none.

Tests are pytest files, `tests/test_*.py`, that import from `add`.
