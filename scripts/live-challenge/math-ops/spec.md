# Spec

## Problem

`add.py` offers `add_numbers` but no way to multiply two numbers, so callers write the product inline. A small sibling function gives them one tested place to do it.

## Scope

Add `multiply_numbers(a, b)` to `add.py`, immediately after `add_numbers`, in the same style, returning the product of its two arguments. Add a standard-library `unittest` file `tests/test_add.py` covering `add_numbers` and `multiply_numbers`.

## Non-goals

- Changing `add_numbers` or the `if __name__ == "__main__":` block.
- Adding a dependency, a build system, input validation or type coercion.

## Affected services and packages

- `add.py`, the module's only source file.
- `tests/test_add.py`, new.

## Acceptance criteria

1. `add.py` defines `multiply_numbers(a, b)` and `multiply_numbers(6, 7)` returns `42`.
2. `multiply_numbers` returns the arithmetic product for a negative operand (`multiply_numbers(-3, 4)` is `-12`) and for a zero operand (`multiply_numbers(0, 9)` is `0`).
3. `add_numbers(2, 3)` still returns `5`, and `python3 add.py` still prints its sum line.
4. `tests/test_add.py` uses only the standard library and passes under `python3 -m unittest discover -s tests`.

## Risks

- None beyond a mistaken edit of `add_numbers`.

## Open questions

None.
