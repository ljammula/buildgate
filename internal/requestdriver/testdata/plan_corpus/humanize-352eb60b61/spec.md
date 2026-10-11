# Spec

## Problem

`fractional` in `src/humanize/number.py` always approximates the fractional part of its input with a denominator of at most 1000, because the limit is hardcoded in its call to `Fraction.limit_denominator`. Callers that want coarser, more readable fractions, such as eighths or sixteenths of an inch or `3 1/7` instead of `3 16/113`, cannot ask for them, and callers that want a finer approximation cannot ask for that either.

## Scope

Extend `fractional` in `src/humanize/number.py` with a `max_denominator: int = 1000` argument that sets the largest denominator the returned fraction may have. The fractional part of the value is approximated by the closest fraction whose denominator does not exceed `max_denominator`; every other rule of the function (whole numbers, mixed fractions, the sign of negative mixed fractions, folding a fractional part that rounds to a whole into the integer part, non-finite values and values that cannot be converted) stays as it is. A `max_denominator` below 1 is rejected with a `ValueError` before the value is looked at. Calls that omit the argument return exactly what they return today.

The scope also includes documenting the new argument, its default and the error in the `fractional` docstring, with at least one docstring example that passes `max_denominator`, and adding regression coverage to the existing `fractional` tests in `tests/test_number.py`.

## Non-goals

- Changing the output of `fractional` when `max_denominator` is omitted.
- Adding other rounding controls, such as a fixed denominator, a list of allowed denominators or a rounding direction.
- Validating the type of `max_denominator`, or accepting it as a string.
- Changing any other function in `src/humanize/number.py`, the exports in `src/humanize/__init__.py`, or the translation catalogs.

## Affected services and packages

The `humanize` Python package is affected through its public `fractional` number-formatting API in `src/humanize/number.py` and the tests in `tests/test_number.py`. No external service, persistent data, or runtime dependency is affected.

## Acceptance criteria

1. `fractional` in `src/humanize/number.py` has the signature `fractional(value: NumberOrString, max_denominator: int = 1000) -> str`; `max_denominator` is an ordinary parameter that can be passed by position or by keyword.
2. `fractional` in `src/humanize/number.py` called without `max_denominator` returns what it returns today: `humanize.fractional(0.333)` returns exactly `"333/1000"`, `humanize.fractional(3.14159)` returns exactly `"3 16/113"`, `humanize.fractional(0.3)` returns exactly `"3/10"` and `humanize.fractional(1)` returns exactly `"1"`.
3. `fractional` in `src/humanize/number.py` approximates the fractional part with the closest fraction whose denominator is at most `max_denominator`: `humanize.fractional(0.333, max_denominator=10)` returns exactly `"1/3"`, `humanize.fractional(0.126, max_denominator=8)` returns exactly `"1/8"`, `humanize.fractional(0.0625, max_denominator=16)` returns exactly `"1/16"`, `humanize.fractional(0.0625, max_denominator=10)` returns exactly `"1/10"` and `humanize.fractional("0.25", max_denominator=4)` returns exactly `"1/4"`.
4. `fractional` in `src/humanize/number.py` keeps its mixed-fraction and sign rules with a supplied `max_denominator`: `humanize.fractional(3.14159, max_denominator=10)` returns exactly `"3 1/7"`, `humanize.fractional(3.14159, max_denominator=100)` returns exactly `"3 14/99"`, `humanize.fractional(1.3, max_denominator=5)` returns exactly `"1 1/3"`, `humanize.fractional(-1.3, max_denominator=5)` returns exactly `"-1 1/3"` and `humanize.fractional(-0.3, max_denominator=3)` returns exactly `"-1/3"`.
5. `fractional` in `src/humanize/number.py` folds a fractional part that approximates to a whole number into the integer part: `humanize.fractional(0.9, max_denominator=1)` returns exactly `"1"`, `humanize.fractional(2.9, max_denominator=1)` returns exactly `"3"`, `humanize.fractional(-2.8, max_denominator=1)` returns exactly `"-3"`, `humanize.fractional(0.1, max_denominator=1)` returns exactly `"0"` and `humanize.fractional(5, max_denominator=1)` returns exactly `"5"`.
6. `fractional` in `src/humanize/number.py` raises `ValueError` with the exact message `max_denominator must be at least 1` when `max_denominator` is less than 1, for example `humanize.fractional(0.3, max_denominator=0)` and `humanize.fractional(0.3, max_denominator=-5)`. The check runs before the value is converted, so `humanize.fractional("ten", max_denominator=0)` and `humanize.fractional(float("nan"), max_denominator=0)` raise the same error.
7. `fractional` in `src/humanize/number.py` with a valid `max_denominator` keeps its handling of other inputs: `humanize.fractional("ten", max_denominator=10)` returns exactly `"ten"`, `humanize.fractional(None, max_denominator=10)` returns exactly `"None"`, `humanize.fractional(float("nan"), max_denominator=10)` returns exactly `"NaN"` and `humanize.fractional(float("inf"), max_denominator=10)` returns exactly `"+Inf"`.
8. The `fractional` docstring in `src/humanize/number.py` follows Google style, lists `max_denominator` under `Args:` as an int with default `1000` that is the largest denominator of the returned fraction, has a `Raises:` entry for the `ValueError` of criterion 6, and includes the example `fractional(3.14159, max_denominator=10)` with the result `'3 1/7'`.
9. `tests/test_number.py` has test cases for `fractional` that cover a supplied `max_denominator` for a plain fraction, a mixed fraction, a negative mixed fraction, `max_denominator=1`, and the `ValueError` and its message for `max_denominator=0`, and `python -m pytest -q tests` passes.

## Risks

- Backward compatibility depends on the default staying `1000` and on the existing folding and sign rules being left alone. Callers that compare humanized strings regress if any default output changes.
- An input that lies exactly halfway between two candidate fractions, such as `0.75` with `max_denominator=2`, takes whichever one `Fraction.limit_denominator` picks. This spec does not define those ties and the tests should not assert on them.
- `max_denominator` is assumed to be an int. A float or a string may fail inside the standard library with a different exception; adding type validation would create behaviour this request does not define.

## Open questions

None.
