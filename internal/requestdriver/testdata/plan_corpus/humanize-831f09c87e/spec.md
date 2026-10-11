# Spec

## Problem

`naturalsize` in `src/humanize/filesize.py` turns a number of bytes into text such as `3.0 MB`, `2.9 KiB` or `2.9K`, but the package has no way back. Callers that read sizes from configuration files, command-line options or log lines, including text that `naturalsize` itself produced, have to write their own parser and repeat the suffix tables that `src/humanize/filesize.py` already holds.

## Scope

Add a public function `parsesize(value: str, binary: bool = False) -> int` to `src/humanize/filesize.py` that parses a human-readable file size and returns a whole number of bytes, and export it from `src/humanize/__init__.py` as `humanize.parsesize`. It reads the three notations `naturalsize` writes in English: decimal suffixes (`kB` to `QB`), binary suffixes (`KiB` to `QiB`) and GNU single-letter suffixes (`K` to `Q`), plus `B`, `Byte`, `Bytes` and a bare number. Suffixes are matched without regard to case, and the suffix tables come from the existing `suffixes` dictionary in `src/humanize/filesize.py` rather than a second copy.

The result is computed exactly, with no binary floating-point rounding, and a fractional number of bytes is truncated toward zero. Text that is not a size raises `ValueError`, and a value that is not a `str` raises `TypeError`. The scope also includes a Google-style docstring with examples and new tests in `tests/test_filesize.py`. `docs/filesize.md` renders the whole module, so it needs no edit.

## Non-goals

- Changing `naturalsize` or the `suffixes` dictionary in `src/humanize/filesize.py`.
- Parsing translated output (for example French `octets` or `Ko`), bit units, thousands separators, a decimal comma, or exponent notation such as `1e3`.
- Accepting numbers, `bytes` or `None` as the value.
- Adding translatable phrases or changing the translation catalogs; the error messages are plain English strings that do not go through gettext.
- Returning a float, or offering a rounding mode other than truncation toward zero.

## Affected services and packages

The `humanize` Python package is affected: a new public function in `src/humanize/filesize.py`, its export in `src/humanize/__init__.py`, and tests in `tests/test_filesize.py`. No external service, persistent data, or runtime dependency is affected; only the standard library is used.

## Acceptance criteria

1. `parsesize` in `src/humanize/filesize.py` has the signature `parsesize(value: str, binary: bool = False) -> int` and always returns an `int`. `src/humanize/__init__.py` imports it from `humanize.filesize` and lists `"parsesize"` in `__all__`, keeping `__all__` sorted, so `humanize.parsesize` is the same object as `humanize.filesize.parsesize`.
2. `parsesize` in `src/humanize/filesize.py` accepts, after removing leading and trailing whitespace from `value`, exactly this form: an optional sign (`+` or `-`), a number, zero or more spaces or tabs, and an optional unit made of ASCII letters. The number is ASCII digits with an optional decimal point, in one of the forms `12`, `12.5`, `12.` or `.5`. So `humanize.parsesize("42")` returns `42`, `humanize.parsesize("0")` returns `0`, `humanize.parsesize("  10 GB ")` returns `10000000000`, `humanize.parsesize("10GB")` returns `10000000000`, `humanize.parsesize("1\tMiB")` returns `1048576`, `humanize.parsesize(".5 kB")` returns `500`, `humanize.parsesize("5. kB")` returns `5000` and `humanize.parsesize("+1 MB")` returns `1000000`.
3. `parsesize` in `src/humanize/filesize.py` treats no unit, `B`, `Byte` and `Bytes` as a multiplier of 1: `humanize.parsesize("300 Bytes")` returns `300`, `humanize.parsesize("1 Byte")` returns `1` and `humanize.parsesize("300B")` returns `300`.
4. `parsesize` in `src/humanize/filesize.py` reads the decimal suffixes `kB`, `MB`, `GB`, `TB`, `PB`, `EB`, `ZB`, `YB`, `RB`, `QB` as `1000**1` to `1000**10` when `binary` is `False`: `humanize.parsesize("3.0 MB")` returns `3000000`, `humanize.parsesize("10.0 RB")` returns `10**28` and `humanize.parsesize("30000.0 QB")` returns `3 * 10**34`.
5. `parsesize` in `src/humanize/filesize.py` reads the binary suffixes `KiB`, `MiB`, `GiB`, `TiB`, `PiB`, `EiB`, `ZiB`, `YiB`, `RiB`, `QiB` and the GNU suffixes `K`, `M`, `G`, `T`, `P`, `E`, `Z`, `Y`, `R`, `Q` as `1024**1` to `1024**10`, whatever `binary` is: `humanize.parsesize("2.9 KiB")` returns `2969`, `humanize.parsesize("-4.0 KiB")` returns `-4096`, `humanize.parsesize("2.9K")` returns `2969`, `humanize.parsesize("1.5K")` returns `1536`, `humanize.parsesize("1 Q")` returns `1024**10` and `humanize.parsesize("1 QiB")` returns `1024**10`.
6. `parsesize` in `src/humanize/filesize.py` with `binary=True` reads the decimal suffixes of criterion 4 as powers of 1024 instead, and changes nothing else: `humanize.parsesize("3.0 MB", binary=True)` returns `3145728`, `humanize.parsesize("10 GB", binary=True)` returns `10737418240`, `humanize.parsesize("300 Bytes", binary=True)` returns `300` and `humanize.parsesize("2.9 KiB", binary=True)` returns `2969`.
7. `parsesize` in `src/humanize/filesize.py` matches units without regard to case: `humanize.parsesize("1 kb")`, `humanize.parsesize("1 KB")` and `humanize.parsesize("1 kB")` each return `1000`, `humanize.parsesize("1 kib")` returns `1024`, `humanize.parsesize("1 m")` returns `1048576`, `humanize.parsesize("10 gb")` returns `10000000000` and `humanize.parsesize("5 BYTES")` returns `5`.
8. `parsesize` in `src/humanize/filesize.py` computes the product of the number and the multiplier exactly (for example with `fractions.Fraction` or `decimal.Decimal`, not `float`) and then truncates toward zero: `humanize.parsesize("0.1 QB")` returns exactly `10**29`, `humanize.parsesize("1.5 Bytes")` returns `1`, `humanize.parsesize("-1.5 Bytes")` returns `-1`, `humanize.parsesize("0.0009 kB")` returns `0`, `humanize.parsesize("1.0005 kB")` returns `1000` and `humanize.parsesize("2.930K")` returns `3000`.
9. `parsesize` in `src/humanize/filesize.py` raises `ValueError` whose message is exactly `invalid size: ` followed by `repr(value)` of the original, unstripped argument, for any string that does not match criterion 2 or whose unit is not one named in criteria 3 to 5. This covers `""`, `"   "`, `"MB"`, `"1 XB"`, `"1 KiBB"`, `"1 ki"`, `"1e3"`, `"1,000 B"`, `"1 M B"`, `"1.2.3 MB"`, `"--1"`, `"nan"` and `"inf"`; for example `humanize.parsesize("1 XB")` raises `ValueError("invalid size: '1 XB'")` and `humanize.parsesize(" MB ")` raises `ValueError("invalid size: ' MB '")`.
10. `parsesize` in `src/humanize/filesize.py` raises `TypeError` whose message is exactly `value must be a str, not ` followed by the type name of the argument, when `value` is not a `str`: `humanize.parsesize(1000)` raises `TypeError("value must be a str, not int")`, `humanize.parsesize(None)` raises `TypeError("value must be a str, not NoneType")` and `humanize.parsesize(b"1 kB")` raises `TypeError("value must be a str, not bytes")`.
11. `parsesize` in `src/humanize/filesize.py` reads back what `naturalsize` wrote wherever `naturalsize` did not round: `humanize.parsesize(humanize.naturalsize(n))` returns `n` for every `n` in `0`, `1`, `300`, `1000`, `3000000`, `10**12` and `-4000`, and `humanize.parsesize(humanize.naturalsize(n, binary=True))` returns `n` for every `n` in `0`, `1`, `300`, `1024`, `3 * 1024**2`, `1024**4` and `-4096`. `humanize.parsesize(humanize.naturalsize(3000, gnu=True))` returns `2969`, because `naturalsize` rounded the text to `2.9K`.
12. The `parsesize` docstring in `src/humanize/filesize.py` follows Google style with `Args:`, `Returns:` and `Raises:` sections, states that it is the inverse of `naturalsize`, that fractional bytes are truncated toward zero and what `binary` changes, and has examples that include `parsesize("3.0 MB")` giving `3000000` and `parsesize("3.0 MB", binary=True)` giving `3145728`.
13. `tests/test_filesize.py` has parametrized tests for `parsesize` covering each of criteria 2 to 11 with at least one case, including a `ValueError` message and a `TypeError` message, and `python -m pytest -q tests` passes.

## Risks

- The suffix letters overlap between notations once case is ignored: `m` is the GNU suffix (1024 squared) while `mb` is the decimal one (1000 squared, or 1024 squared with `binary=True`). The unit must be compared as a whole, not by its first letter, or sizes come out wrong by a factor that is easy to miss.
- A float-based implementation passes most cases and fails on large units: `0.1 * 10**30` as a float is not `10**29`. The exactness of criterion 8 is what makes the function safe for the `RB` and `QB` range.
- `naturalsize` rounds to one decimal place by default, so parsing its output does not always return the original number; criterion 11 lists values where it does and one where it does not. Callers may still expect a perfect round trip.
- `naturalsize` output in an activated locale uses translated units that `parsesize` rejects with `ValueError`. That is intended here and should be stated in the docstring rather than papered over.
- `src/humanize/__init__.py` is a shared file; another change that edits its imports or `__all__` at the same time can conflict with the one-line additions made here.

## Open questions

None.
