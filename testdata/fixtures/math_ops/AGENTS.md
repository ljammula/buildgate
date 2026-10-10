# AGENTS.md

One Python module, `add.py`, with no dependencies outside the standard
library and no build system. The repository has no README and no tests yet.

## Commands

- Setup: none. Python 3 is all it needs.
- Syntax check: `python3 -m py_compile add.py`
- Test: `python3 -m unittest discover -s tests`, once a `tests/` directory
  with a test file exists. Until then that command fails.
- Build and lint: none.

Tests are standard-library `unittest` files, `tests/test_*.py`, that import
from `add`.
