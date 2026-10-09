Verify-Command: python -m pip install -q -e . pytest freezegun && python -m pytest -q tests
Allowed-Files: src/humanize/time.py, tests/test_time.py, tests/test_i18n.py, README.md
Required-Changed-Files: src/humanize/time.py, tests/test_time.py, tests/test_i18n.py, README.md

## Goal

Make `precisedelta()` preserve the sign of supported negative `datetime.timedelta` and numeric-seconds inputs with exactly one leading ASCII `-`, while leaving the existing absolute-magnitude rendering, positive/zero output, fallback behavior, errors, localization, and formatting semantics unchanged.

## Plan

### Files to touch

- `src/humanize/time.py`
- `tests/test_time.py`
- `tests/test_i18n.py`
- `README.md`

### Steps

1. Capture whether the original supported input is negative before `_date_and_delta()` normalizes it to an absolute magnitude. Keep sign detection separate from conversion so unsupported values still return their existing `str(value)` fallback and existing conversion/validation errors are not changed.
2. Continue decomposing, rounding, suppressing, formatting, pluralizing, and translating only the absolute magnitude. Apply the sign once to the fully assembled result, outside localized or custom-formatted text, including the branch where rounding at `minimum_unit` produces zero.
3. Extend `precisedelta()`'s public docstring and the README precise-time-delta documentation to state that supported negative inputs receive a leading `-`, the remainder describes the absolute magnitude, and a negative input can remain signed when its rounded magnitude is zero. Include representative negative and zero examples without changing `naturaldelta()` documentation or behavior.
4. Add regression tests for the exact multi-unit timedelta example, equal positive/negative magnitudes across numeric and timedelta inputs, fractional numeric seconds, `minimum_unit="minutes"`, suppression, and custom formats. Assert that negative rounded-zero output is signed while exact zero is not, and retain tests for `None`, unsupported/fallback values, and existing errors.
5. Extend the existing localization coverage to verify that a localized negative result has one ASCII prefix sign and an otherwise localized absolute magnitude, with no sign added to the individual components; run the complete verification command.

### Tests to add

- Parameterized `precisedelta()` sign-preservation cases for multi-unit timedeltas, numeric seconds, fractional values, and `minimum_unit`/suppression/custom-format combinations, comparing each negative result with `"-" +` the equal positive result.
- Exact assertions for `-3661` seconds, `minimum_unit="minutes"`, `-0.1` seconds rounded to `"-0 minutes"`, and exact zero remaining unsigned.
- Compatibility assertions covering `None`, unsupported inputs, and the existing validation/error paths.
- Localized negative `precisedelta()` assertions in the existing i18n test coverage, including the invariant leading ASCII hyphen and localized magnitude.

### Acceptance criteria covered

- 1
- 2
- 3
- 4
- 5
- 6
- 7

## Out of scope

Do not change `naturaldelta()`, `naturaltime()`, unit selection, rounding rules, suppression semantics, custom-format syntax, translations, output vocabulary, or the accepted/invalid input set. Do not add tense such as “ago” or “from now”, reject negative inputs, or create a separate wiring/docs-only ticket.
