# Spec

## Problem

`tabulate()` in `tabulate/__init__.py` always prints every row it is given. For a long table (a query result, a large DataFrame) a caller who wants a preview has to slice the data first, which differs for every input type `tabulate()` accepts, loses the last rows, and gives the reader no sign that rows were left out.

## Scope

Add a keyword argument `maxrows=None` to `tabulate()` in `tabulate/__init__.py`, as its last parameter. With `maxrows=None`, or when the table has no more than `maxrows` data rows, the output is exactly what it is today. Otherwise `tabulate()` prints the first `(maxrows + 1) // 2` data rows, then one ellipsis row in which every cell is the three-character string `...`, then the last `maxrows // 2` data rows.

Rows are dropped right after the input is normalised to rows and headers, before anything else looks at the cell values: wrapping (`maxcolwidths`), column types, number formatting and column widths are worked out from the kept rows only. The ellipsis row is not data: it never changes a column's type or number format; it is aligned and padded like a string value of its column and counts towards the column width. Header rows, index columns and separating lines are handled as stated in the acceptance criteria. An invalid `maxrows` raises `ValueError`.

The scope also includes the documentation and tests the repository's rules require: a section in the `tabulate()` docstring with a doctest example, a "Limiting the number of rows" section in `README.md`, the new parameter in the signature check of `test/test_api.py`, and tests in a new file `test/test_maxrows.py`.

## Non-goals

- A command-line option for `maxrows` in `tabulate/cli.py`.
- A configurable ellipsis string, a configurable head/tail split, or limiting the number of columns.
- Changing `_format_table`, `TableFormat`, any table format, or how separating lines are drawn.
- Avoiding the consumption of an iterator or generator input: all rows are still read.
- Treating `maxrows=0` as "no data rows": it is an error.
- A `CHANGELOG` entry: it is written per release by the maintainer.

## Affected services and packages

The `tabulate` Python package is affected through the signature and behaviour of its public `tabulate()` function in `tabulate/__init__.py`, with `README.md`, `test/test_api.py` and a new `test/test_maxrows.py`. The command-line utility, persistent data and runtime dependencies are not affected.

## Acceptance criteria

In the criteria below `ROWS` is `[[f"r{i}", i, i * 1.5] for i in range(10)]`, and outputs are the listed lines joined by `"\n"`.

1. `tabulate` in `tabulate/__init__.py` has a last parameter `maxrows` with default `None`, immediately after `break_on_hyphens`; `test_tabulate_signature` in `test/test_api.py` lists `("maxrows", None)` as the last entry of its expected signature and passes.
2. `tabulate(ROWS, maxrows=None)` equals `tabulate(ROWS)`, and for `maxrows` of `10`, `11` or `1000`, `tabulate(ROWS, ["name", "n", "x"], tablefmt=fmt, maxrows=maxrows)` equals the same call without `maxrows` for every `fmt`: `tabulate` in `tabulate/__init__.py` changes nothing when the number of data rows is at most `maxrows`.
3. When the number of data rows exceeds `maxrows`, `tabulate` in `tabulate/__init__.py` prints the first `(maxrows + 1) // 2` data rows, one ellipsis row whose every cell is `...`, and the last `maxrows // 2` data rows, in that order. `tabulate(ROWS, ["name", "n", "x"], maxrows=4)` returns exactly:

   ```text
   name      n      x
   ------  ---  -----
   r0        0    0
   r1        1    1.5
   ...     ...  ...
   r8        8   12
   r9        9   13.5
   ```
4. With an odd `maxrows` `tabulate` in `tabulate/__init__.py` keeps one more row at the top than at the bottom, and with `maxrows=1` it prints the first row and the ellipsis row. `tabulate(ROWS, maxrows=3, tablefmt="grid")` returns the first block below and `tabulate(ROWS, maxrows=1)` the second:

   ```text
   +-----+-----+-------+
   | r0  |   0 |   0   |
   +-----+-----+-------+
   | r1  |   1 |   1.5 |
   +-----+-----+-------+
   | ... | ... | ...   |
   +-----+-----+-------+
   | r9  |   9 |  13.5 |
   +-----+-----+-------+
   ```

   ```text
   ---  ---  ---
   r0     0    0
   ...  ...  ...
   ---  ---  ---
   ```
5. The ellipsis cell printed by `tabulate` in `tabulate/__init__.py` is aligned exactly as the string `...` would be as a value of that column under the column's alignment (`left`, `right`, `center`, or `decimal`, where it is placed like a number without a decimal point, as the `x` column of criteria 3 and 4 shows), and it widens a column narrower than three characters. `tabulate(ROWS, ["name", "n", "x"], maxrows=2, numalign="left", stralign="right")` returns exactly:

   ```text
     name  n    x
   ------  ---  ----
       r0  0    0
      ...  ...  ...
       r9  9    13.5
   ```
6. `tabulate` in `tabulate/__init__.py` works out column types, number formats and widths from the kept rows only, and the ellipsis row does not affect a column's type: `tabulate([[1, "a"], [2, "b"], ["x", "c"], [4, "d"]], maxrows=2)` returns `---  ---`, `  1  a`, `...  ...`, `  4  d`, `---  ---` (the first column is numeric and right-aligned although a dropped row holds `"x"`).
7. `tabulate` in `tabulate/__init__.py` counts data rows after the input is normalised, for every supported input type: a header row taken with `headers="firstrow"` is not counted, and an iterator is accepted. `tabulate([["h1", "h2"]] + [[i, "abc"] for i in range(6)], headers="firstrow", maxrows=2)` returns `  h1  h2`, `----  ----`, `   0  abc`, ` ...  ...`, `   5  abc`; `tabulate({"a": [1, 2, 3, 4], "b": [5, 6, 7, 8]}, headers="keys", maxrows=2)` returns `  a    b`, `---  ---`, `  1    5`, `...  ...`, `  4    8`; `tabulate(iter(ROWS), maxrows=2)` returns `---  ---  -----`, `r0     0    0`, `...  ...  ...`, `r9     9   13.5`, `---  ---  -----`.
8. With `showindex`, `tabulate` in `tabulate/__init__.py` keeps each kept row's original index and prints `...` in the index column of the ellipsis row. `tabulate(ROWS, ["name", "n", "x"], maxrows=5, showindex=True, tablefmt="pipe")` returns exactly:

   ```text
   |     | name   |   n |     x |
   |----:|:-------|----:|------:|
   |   0 | r0     |   0 |   0   |
   |   1 | r1     |   1 |   1.5 |
   |   2 | r2     |   2 |   3   |
   | ... | ...    | ... | ...   |
   |   8 | r8     |   8 |  12   |
   |   9 | r9     |   9 |  13.5 |
   ```
9. `SEPARATING_LINE` rows are not data rows for `tabulate` in `tabulate/__init__.py`: they are not counted against `maxrows`, and when rows are dropped a separating line is kept if and only if the nearest data row before it is kept (a separating line before the first data row is kept); a kept separating line stays directly after that row, so one that follows the last row of the top part is printed before the ellipsis row. With `T = [["a", 1], ["b", 2], SEPARATING_LINE, ["c", 3], ["d", 4], SEPARATING_LINE, ["e", 5], ["f", 6], SEPARATING_LINE, ["g", 7]]`, `tabulate(T, maxrows=7)` equals `tabulate(T)`; `tabulate(T, maxrows=4)` returns `---  ---`, `a      1`, `b      2`, `---  ---`, `...  ...`, `f      6`, `---  ---`, `g      7`, `---  ---`; and `tabulate(T, maxrows=3)` returns `---  ---`, `a      1`, `b      2`, `---  ---`, `...  ...`, `g      7`, `---  ---`.
10. `tabulate` in `tabulate/__init__.py` drops rows before wrapping, and never wraps the ellipsis cell: `tabulate([["aaaa bbbb cccc", 1], ["dd", 2], ["eeee ffff", 3]], maxrows=2, maxcolwidths=[4, None], tablefmt="grid")` returns exactly:

    ```text
    +------+-----+
    | aaaa |   1 |
    | bbbb |     |
    | cccc |     |
    +------+-----+
    | ...  | ... |
    +------+-----+
    | eeee |   3 |
    | ffff |     |
    +------+-----+
    ```
11. `tabulate` in `tabulate/__init__.py` raises `ValueError` with the exact message `maxrows must be a positive integer or None` when `maxrows` is not `None` and is not an `int` greater than or equal to 1; a `bool` is rejected. The values `0`, `-1`, `2.0`, `"3"` and `True` each raise it, and it is raised whatever the number of rows (`tabulate([[1]], maxrows=0)` raises).
12. The `tabulate()` docstring in `tabulate/__init__.py` ends with a section headed `Limiting the number of rows`, underlined with hyphens like the docstring's other sections and placed after the sentence about `maxheadercolwidths`, which explains `maxrows` and has a doctest example with `maxrows=4`; the doctest passes.
13. `README.md` has a section `### Limiting the number of rows` immediately before the `### Adding Separating lines` section. It states that at most `maxrows` data rows are printed, the first `(maxrows + 1) // 2` and the last `maxrows // 2`, with a row of `...` cells for the rows left out, and that column types, number formats and widths come from the printed rows; it has a `pycon` example calling `tabulate` with `maxrows=4` whose output passes as a doctest.
14. A new file `test/test_maxrows.py` has tests for criteria 2 to 11, comparing whole outputs and the exact error message.
15. `python -m pytest`, `ruff check` and `ruff format --check` pass.

## Risks

- `tabulate()` removes separating lines into a list of positions and re-inserts them by position after the rows are formatted; inserting the ellipsis row shifts the positions of the separating lines after it, and one directly after the last top row must not shift.
- Inserting the ellipsis as a data value before column types are detected would turn numeric columns into text columns and lose number formatting and decimal alignment; it has to enter after values are formatted and before columns are aligned.
- Under `decimal` alignment the ellipsis is padded like a number without a decimal point, which the exact outputs above depend on.
- `test/test_api.py` pins the full signature of `tabulate()`; the parameter must be last, and the test's expected list must be updated in the same change.
- Other pull requests add text elsewhere in the `tabulate()` docstring and `README.md`, and one adds an entry to `_table_formats`; this change must touch only the places named above.

## Open questions

None.
