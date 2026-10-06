# Ticket: add multiply_numbers to math_ops

This is an existing repo. Deliberately tiny and dependency-free -- this
ticket exists as a fast standing fixture for `make live-smoke` (see
AGENTS.md's "Live validation" section), not to ship a product feature.
Target repo: the math_ops fixture (`testdata/fixtures/math_ops`) (single file, `add.py`, no
external dependencies, no build system beyond the Python standard
library).

## Goal

`add.py` defines `add_numbers(a, b)`. Add a sibling `multiply_numbers(a,
b)` function, same style, plus a standard-library test file covering
both functions.

## Required changes

**`add.py`**:

1. Add `multiply_numbers(a, b)` immediately after `add_numbers`, same
   docstring style ("""Multiplies a and b and returns the result.""").
2. Do not change `add_numbers` or the `if __name__ == "__main__":` block.

**`tests/test_add.py`** (new file):

1. A `unittest.TestCase` subclass importing `add_numbers` and
   `multiply_numbers` from `add`, using only the standard library (no
   pytest or other third-party imports).
2. At least: `add_numbers` with two positive numbers; `multiply_numbers`
   with two positive numbers; `multiply_numbers` with a negative and a
   positive number (asserting the correct negative product).

## Out of scope

- No new dependency of any kind.
- No change to `add_numbers`'s own behavior or signature.
- No `__main__` block changes.

Allowed-Files: add.py, tests/test_add.py

Required-Changed-Files: add.py, tests/test_add.py

Required-Content: multiply_numbers

## Acceptance / verification

Verify-Command: python3 -m unittest discover -s tests

- `python3 -m unittest discover -s tests` discovers and passes every
  test, including the new `multiply_numbers` cases.
- `add_numbers`'s existing behavior is unchanged.

Success criterion: `multiply_numbers(3, 4) == 12` and
`multiply_numbers(-2, 5) == -10`, both verified by a real test.
