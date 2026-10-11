# Spec

## Problem

Click has no parameter type for a length of time. An option such as `--timeout` or `--interval` is declared as `int` or `float`, which leaves the unit to the help text and makes the user write `5400` for an hour and a half, or the application writes its own callback to parse `1h30m`. `click.DateTime` in `src/click/types.py` covers points in time only. A built-in type that converts `1h30m` to a `datetime.timedelta` removes that repeated code.

## Scope

Add a `Duration` parameter type to `src/click/types.py`, a `ParamType` subclass that converts a string made of number-and-unit components, such as `90s`, `1h30m` or `1.5h`, into a `datetime.timedelta`. The units are `w` (weeks), `d` (days), `h` (hours), `m` (minutes), `s` (seconds) and `ms` (milliseconds). An optional `default_unit` constructor argument gives the unit of a value that is only a number; without it such a value is rejected. Invalid input fails through `ParamType.fail` with one fixed message.

Export the class as `click.Duration` from `src/click/__init__.py`, beside the other `from .types import ...` lines.

The scope also includes a class docstring that lists the units, describes `default_unit` and carries `.. versionadded:: 8.6.0`; an `autoclass` entry for `Duration` in `docs/api.md` directly after the `DateTime` entry, and one in the list in `docs/parameter-types.md` directly after the `DateTime` entry, each in the same form as the `DateTime` entry beside it; a new test file `tests/test_types/test_Duration.py`; and one entry in `CHANGES.md` as the first bullet of the `## Version 8.6.0` list, directly under its `Unreleased` line, written in the style of the other entries and without a `{pr}` or `{issue}` role, since the number is not known when the change is built.

## Non-goals

- Negative durations, a sign prefix, upper-case units, long unit names such as `min` or `hours`, months, years, ISO 8601 durations such as `PT1H30M`, and clock notation such as `1:30:00`.
- Minimum or maximum bounds on the value.
- A module-level instance such as `click.DURATION`, and inferring the type from a `timedelta` default in `convert_type`.
- Changing how a `timedelta` default is shown by `show_default`, shell completion for the type, or any other parameter type.
- Adding a dependency. Only the standard library is used.

## Affected services and packages

The `click` Python package is affected through a new public class in `src/click/types.py`, its export in `src/click/__init__.py`, the API and parameter-type pages under `docs/`, a new test file under `tests/test_types/`, and one line in `CHANGES.md`. No external service, persistent data, or runtime dependency is affected.

## Acceptance criteria

1. `Duration` in `src/click/types.py` is a subclass of `ParamType` with the class attribute `name = "duration"`, and `__repr__` returns exactly `"Duration"`. `src/click/__init__.py` exports it, so `click.Duration is click.types.Duration`.
2. `Duration.__init__` in `src/click/types.py` has the signature `__init__(self, default_unit: str | None = None) -> None` and stores the argument unchanged as the instance attribute `default_unit`. When `default_unit` is neither `None` nor one of `"w"`, `"d"`, `"h"`, `"m"`, `"s"`, `"ms"`, it raises `ValueError` with exactly the message `default_unit must be one of 'w', 'd', 'h', 'm', 's', 'ms'.`
3. `Duration.convert` in `src/click/types.py` has the signature `convert(self, value, param, ctx)` of `ParamType.convert` and returns a `datetime.timedelta`. A `value` that is already a `timedelta` is returned as the same object, including a negative one. Any other value is turned into text with `str(value)`, and leading and trailing whitespace is removed with `str.strip()` before parsing.
4. `Duration.convert` in `src/click/types.py` accepts text that is one or more components with nothing between them. A component is a number followed directly by a unit. A number is one or more ASCII digits `0`-`9`, optionally followed by `.` and one or more ASCII digits. A unit is exactly one of the lower-case strings `w`, `d`, `h`, `m`, `s`, `ms`; a number followed by `ms` is always milliseconds. The result is the sum of `timedelta(weeks=n)`, `timedelta(days=n)`, `timedelta(hours=n)`, `timedelta(minutes=n)`, `timedelta(seconds=n)` or `timedelta(milliseconds=n)` for each component, with `n = float(number)`. So `"2d"` gives `timedelta(days=2)`, `"500ms"` gives `timedelta(milliseconds=500)`, `"0s"` gives `timedelta(0)`, `"1h30m"` gives `timedelta(hours=1, minutes=30)`, `"1m5ms"` gives `timedelta(minutes=1, milliseconds=5)`, `"1.5h"` gives `timedelta(minutes=90)`, `"1.5h30m"` gives `timedelta(hours=2)`, `"120m"` gives `timedelta(hours=2)`, `"  90s\n"` gives `timedelta(seconds=90)`, and `"1w2d3h4m5s6ms"` gives `timedelta(weeks=1, days=2, hours=3, minutes=4, seconds=5, milliseconds=6)`.
5. `Duration.convert` in `src/click/types.py` requires the units of a value to appear in the order `w`, `d`, `h`, `m`, `s`, `ms`, each at most once. `"30m1h"`, `"5s1m"`, `"5ms1s"`, `"1h1h"` and `"5ms5ms"` are invalid.
6. `Duration.convert` in `src/click/types.py` rejects every other text, including the empty string, `"abc"`, `"h"`, `"1 h"`, `"1h 30m"`, `"1H"`, `"5MS"`, `"-5s"`, `"+5s"`, `".5h"`, `"5.h"`, `"1e3s"`, `"5sec"`, `"5msm"` and `"1h30"`. With `default_unit=None`, a value that is only a number, such as `"90"` or `"1.5"`, is also invalid.
7. `Duration.convert` in `src/click/types.py`, when `default_unit` is set and the stripped text is only a number as defined in criterion 4, returns that number in the default unit: `Duration(default_unit="s")` converts `"90"` to `timedelta(seconds=90)`, `"1.5"` to `timedelta(milliseconds=1500)`, `" 7 "` to `timedelta(seconds=7)` and `"0"` to `timedelta(0)`; `Duration(default_unit="m")` converts `"90"` to `timedelta(minutes=90)`; `Duration(default_unit="ms")` converts `"250"` to `timedelta(milliseconds=250)`. Text with units is parsed as in criteria 4 to 6 whatever the default unit, so `Duration(default_unit="s")` converts `"1h"` to `timedelta(hours=1)` and still rejects `"1h30"`, `"30x"`, `"-5"`, `"1."`, `".5"`, `"1e3"` and the empty string.
8. `Duration.convert` in `src/click/types.py` reports every invalid value by calling `self.fail` with `param` and `ctx`, which raises `click.BadParameter`, with exactly the message `{value!r} is not a valid duration.`, where `value` is the value as passed in, before stripping. For `"abc"` the message is `'abc' is not a valid duration.` and for the empty string it is `'' is not a valid duration.` The message text is wrapped in the module's `_()` gettext function like the other messages in the file. A value too large for `timedelta`, such as `"999999999999w"`, which makes `timedelta` raise `OverflowError`, is reported the same way and no `OverflowError` escapes.
9. `Duration.to_info_dict` in `src/click/types.py` returns exactly `{"param_type": "Duration", "name": "duration", "default_unit": None}` for `Duration()` and the same dictionary with `"default_unit": "s"` for `Duration(default_unit="s")`.
10. `Duration` in `src/click/types.py` does not override `get_metavar`, so for a command with `@click.option("--timeout", type=click.Duration(), default="30s", help="How long.")` the help output contains the line `  --timeout DURATION  How long.`; running it with `--timeout 1h30m` passes `timedelta(seconds=5400)` to the callback; running it with no arguments passes `timedelta(seconds=30)`; and running it with `--timeout abc` exits with code 2 and prints the line `Error: Invalid value for '--timeout': 'abc' is not a valid duration.`
11. The `Duration` class docstring in `src/click/types.py` names all six units with their meaning, gives at least the example `1h30m`, documents `:param default_unit:` and its default of `None`, and has `.. versionadded:: 8.6.0`. `docs/api.md` and `docs/parameter-types.md` each have an `autoclass:: Duration` entry directly after their `DateTime` entry, in the same form as that entry.
12. `tests/test_types/test_Duration.py` exists and asserts the exact results stated in criteria 2 to 10, and `CHANGES.md` has one new entry naming `Duration` as the first bullet of the `## Version 8.6.0` list.
13. `python -m pytest -q tests -k "not pager"` passes, including `tests/test_imports.py`, which allows only a fixed set of standard-library imports in `click`; `re` and `datetime` are already in that set.

## Risks

- `m` and `ms` share a first letter. A parser that reads `m` before trying `ms` turns `5ms` into five minutes followed by a stray `s`; criteria 4 and 6 pin `500ms`, `1m5ms` and `5msm` for that reason.
- The components are converted with `float`, so a value with many decimal places is rounded to the microsecond resolution of `timedelta`. The stated examples are exact in binary floating point or at that resolution.
- `str.isdigit` and the `\d` pattern accept non-ASCII digits. The number must be limited to ASCII `0`-`9`, or values that look valid in other scripts are accepted contrary to criterion 4.
- `CHANGES.md` is edited by every change of the release. Adding the entry as the first bullet of the `## Version 8.6.0` list keeps it apart from entries other open changes add at the end of that list.
- The project's contributing guide (`docs/contributing.md`) asks for explicit `if`/`else` blocks instead of ternary expressions and for no new dependencies; the implementation has to follow both.

## Open questions

None.
