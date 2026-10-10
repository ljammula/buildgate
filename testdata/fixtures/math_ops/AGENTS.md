# AGENTS.md

A single-file Python module, `add.py`, with no dependencies outside the
standard library.

- Setup: none. Python 3 is all it needs.
- Test: `python3 -m unittest discover -s tests`
- Build: none.
- Lint: `python3 -m py_compile add.py`

Keep new functions in `add.py` in the style of `add_numbers`, and tests in
standard-library `unittest` files named `tests/test_*.py`.
