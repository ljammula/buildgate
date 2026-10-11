# Spec

## Problem

`tabulate` only writes tables. A table it printed into a README, a commit message, an issue or the output of a `psql` session cannot be read back by the library: a caller who wants to change its format, re-align it after an edit, or sort or filter its rows has to write a parser for the plain-text table first. The delimited and ruled formats `tabulate` prints carry enough structure to recover the headers and the rows.

## Scope

Add a table parser to the package in three parts that build on each other.

Part 1, the core: a new module `tabulate/parser.py` with a named tuple `ParsedTable(headers, rows)`, a sorted list `parse_formats`, and a function `parse_table(text, tablefmt="pipe", numparse=True)` that reads the formats in which every line of the table body is one row: `pipe`, `github`, `orgtbl` and `psql`. It classifies each line as blank, rule or content, splits content lines into stripped cells, finds the header through the rule line under it, and raises `ValueError` for an unsupported format or a malformed line. The module imports nothing from `tabulate/__init__.py`.

Part 2, the ruled formats and numbers: `parse_table` also reads `grid` and `outline`, whose header rule is the one drawn with `=`; in `grid` the content lines between two rule lines are one row, so multiline cells come back whole; and with `numparse=True` the cells of the rows that look like numbers become `int` or `float`.

Part 3, the public API and documentation: `tabulate/__init__.py` imports `ParsedTable`, `parse_formats` and `parse_table` from `tabulate/parser.py` and adds them to `__all__`; `README.md` gets a "Parsing tables" section with doctest examples; the `parse_table` docstring has doctest examples; and round-trip tests check that `parse_table(tabulate(...))` returns the original headers and rows for every supported format.

Tests go in a new file `test/test_parser.py`.

## Non-goals

- Parsing any other format (`simple`, `plain`, `rst`, `pretty`, `presto`, `tsv`, `html`, `latex`, the Unicode box-drawing grids and outlines, and the rest).
- Returning the column alignments encoded in a `pipe` delimiter row, or any information other than headers and rows.
- Validating that all rows have the same number of cells, or padding short rows.
- Recovering `|` characters inside cells of formats that do not escape them (every format except `pipe` and `github`), multiline cells in formats other than `grid`, or leading and trailing whitespace of a cell.
- Reading files, or adding a table input format to the command-line utility in `tabulate/cli.py`.
- Using `tabulate()`'s own number detection (`_isnumber`, thousands separators, `disable_numparse` column lists) for conversion.
- A `CHANGELOG` entry: it is written per release by the maintainer.

## Affected services and packages

The `tabulate` Python package is affected: a new module `tabulate/parser.py`, three new public names exported from `tabulate/__init__.py`, a new section in `README.md` and a new test file `test/test_parser.py`. `tabulate()`, the table formats, the command-line utility, persistent data and runtime dependencies are not affected.

## Acceptance criteria

1. `tabulate/parser.py` defines `ParsedTable`, a `collections.namedtuple` with exactly the fields `("headers", "rows")`; `parse_formats`, the list `["github", "grid", "orgtbl", "outline", "pipe", "psql"]`; and `parse_table(text, tablefmt="pipe", numparse=True)`, which returns a `ParsedTable` whose `headers` is a list of `str` and whose `rows` is a list of lists. The module has no import of `tabulate` or of `.` at any level.
2. `parse_table` in `tabulate/parser.py` raises `ValueError` with the exact message `<tablefmt> is not a supported input table format` (for example `simple is not a supported input table format`) when `tablefmt` is not in `parse_formats`, before looking at `text`.
3. `parse_table` in `tabulate/parser.py` splits `text` with `str.splitlines()`, strips each line of leading and trailing whitespace, and ignores lines that are then empty. A remaining line is a rule line if it consists only of the characters `|`, `+`, `:`, `=` and `-` and contains at least one `-` or `=`. Any other line is a content line and must be at least two characters long, start with `|` and end with `|`; if it is not, `parse_table` raises `ValueError` with the exact message `line <n> is not a <tablefmt> table row: <line>`, where `<n>` is the 1-based number of the line in `text` (blank lines counted) and `<line>` is the `repr()` of the stripped line. `parse_table("| a | b |\nnot a row\n", "pipe")` raises `line 2 is not a pipe table row: 'not a row'`, `parse_table("| a |\n\n  | b", "orgtbl")` raises `line 3 is not a orgtbl table row: '| b'`, and `parse_table("|", "psql")` raises `line 1 is not a psql table row: '|'`.
4. `parse_table` in `tabulate/parser.py` turns a content line into cells by removing its first and its last character, splitting the rest on `|`, and stripping each piece of leading and trailing whitespace. For `pipe` and `github` only, the split is on every `|` not immediately preceded by a backslash, and each `\|` in a piece is then replaced by `|`. So `parse_table(tabulate([["a|b", "c"]], ["h|1", "h2"], tablefmt="pipe"), "pipe")` returns `(["h|1", "h2"], [["a|b", "c"]])`, and `parse_table("| a\\|b | c |", "orgtbl")` (one backslash in the text) returns `([], [["a\\", "b", "c"]])`.
5. `parse_table` in `tabulate/parser.py` finds the header rule as the first rule line that starts with `|` for `pipe`, `github`, `orgtbl` and `psql`, and as the first rule line that contains `=` for `grid` and `outline`. If there is a header rule, the content lines before it are the header and the content lines after it are the body; `headers` is `[]` if no content line precedes it. If there is no header rule, `headers` is `[]` and every content line is body. Other rule lines only separate. `parse_table("| item   |   qty |\n|:-------|------:|\n| spam   |    42 |")` returns `ParsedTable(headers=["item", "qty"], rows=[["spam", 42]])`; the `psql` text with lines `+--------+-------+`, `| item   |   qty |`, `|--------+-------|`, `| spam   |    42 |`, `| eggs   |   451 |`, `+--------+-------+` gives `(["item", "qty"], [["spam", 42], ["eggs", 451]])`; the `outline` text `+------+-----+`, `| spam |  42 |`, `| eggs | 451 |`, `+------+-----+` gives `([], [["spam", 42], ["eggs", 451]])`.
6. In `parse_table` in `tabulate/parser.py`, several content lines are merged into one list of cells column by column: the cells in the same position are joined with `"\n"` in line order, a line with fewer cells contributing `""`, and the joined string is stripped of leading and trailing whitespace. All header content lines are merged this way into `headers`, in every format. In `grid`, each run of consecutive content lines in the body (not separated by a rule line) is merged into one row; in the other five formats every body content line is a row of its own.
7. For the `grid` text with lines `+------+---------+`, `| h1   | multi   |`, `|      | line    |`, `+======+=========+`, `| a    | c       |`, `+------+---------+`, `| more | 1       |`, `| spam |         |`, `|      |         |`, `| eggs |         |`, `+------+---------+`, `| x    | y       |`, `|      | z       |`, `+------+---------+`, `parse_table` in `tabulate/parser.py` returns `(["h1", "multi\nline"], [["a", "c"], ["more\nspam\n\neggs", 1], ["x", "y\nz"]])`. For the `psql` text `+------+---------+`, `| h1   | multi   |`, `|      | line    |`, `|------+---------|`, `| more |       1 |`, `| spam |         |`, `+------+---------+` it returns `(["h1", "multi\nline"], [["more", 1], ["spam", ""]])`.
8. With `numparse=True`, `parse_table` in `tabulate/parser.py` converts each cell of `rows`, cell by cell: to `int` if the whole cell matches `[+-]?[0-9]+`; otherwise to `float` if the whole cell matches `[+-]?([0-9]+\.?[0-9]*|\.[0-9]+)([eE][+-]?[0-9]+)?`; otherwise it stays a `str`. `headers` are never converted. The `orgtbl` line `| 1 | +2 | -3 | 1. | .5 | 1e3 | 1_000 | nan | inf | 0x10 | 1,000 | 007 | - | 1.5E-3 |` gives the single row `[1, 2, -3, 1.0, 0.5, 1000.0, "1_000", "nan", "inf", "0x10", "1,000", 7, "-", 0.0015]` with exactly those types, and the `orgtbl` text `| 1 | 2.5 |`, `|---+-----|`, `| 3 | 4.5 |` gives `(["1", "2.5"], [[3, 4.5]])`.
9. With `numparse=False`, `parse_table` in `tabulate/parser.py` returns every cell of `rows` as a `str`: for `tabulate([["spam", 42, 1.5], ["eggs", 451, -2.25]], ["item", "qty", "x y"], tablefmt="psql")` it returns rows `[["spam", "42", "1.5"], ["eggs", "451", "-2.25"]]`.
10. `parse_table` in `tabulate/parser.py` returns `ParsedTable([], [])` for `""` and for text with only blank lines, in every format, and ignores indentation and blank lines between rows: `parse_table("  | a | 1 |\n\n  | b | 2.0 |  \n", "orgtbl")` returns `([], [["a", 1], ["b", 2.0]])`.
11. For every `fmt` in `parse_formats`, with `ROWS = [["spam", 42, 1.5], ["eggs", 451, -2.25], ["ba con", 0, 1e-10], ["", -7, 300.5]]` and `HEADERS = ["item", "qty", "x y"]`, `parse_table` in `tabulate/parser.py` round-trips the output of `tabulate()`: `parse_table(tabulate(ROWS, HEADERS, tablefmt=fmt), fmt) == (HEADERS, ROWS)`, `parse_table(tabulate(ROWS, tablefmt=fmt), fmt) == ([], ROWS)`, and `parse_table(tabulate([], ["a", "b"], tablefmt=fmt), fmt) == (["a", "b"], [])`.
12. `tabulate/__init__.py` imports `ParsedTable`, `parse_formats` and `parse_table` from `tabulate/parser.py`, so `from tabulate import ParsedTable, parse_formats, parse_table` works and gives the same objects as `tabulate.parser`; its `__all__` contains those three names in addition to `"tabulate"`, `"tabulate_formats"` and `"simple_separated_format"`.
13. The `parse_table` docstring in `tabulate/parser.py` describes `tablefmt`, `numparse`, the stripping of cells and the empty `headers` of a headerless table, and has at least two doctest examples (one with a header, one with `numparse=False`); they pass under `python -m pytest`, which collects the module's doctests.
14. `README.md` has a section `### Parsing tables` immediately before the `Usage of the command line utility` heading. It names `parse_table`, `ParsedTable` with its two fields, `parse_formats` and its six formats, the `numparse` argument, the empty `headers` of a headerless table, and the difference between `grid` (lines between rule lines are one row) and the other formats; it has `pycon` examples, passing as doctests, that include a line starting `>>> parse_table(` and a call with `numparse=False`.
15. A new file `test/test_parser.py` has tests for criteria 2 to 12: every format with and without headers, both error messages, escaped pipes, multiline `grid` cells, the number-conversion table, `numparse=False`, empty input, and the round trips of criterion 11 parametrised over `parse_formats`.
16. `python -m pytest`, `ruff check` and `ruff format --check` pass, and the existing tests, including `test/test_api.py` and `test/test_cli.py` (which runs `tabulate/cli.py` as a script), are unchanged and pass.

## Risks

- `tabulate/__init__.py` importing `tabulate/parser.py` at import time makes any import of the package from the parser circular; the parser has to be self-contained, with its own number patterns.
- Classifying lines by characters alone treats a content line such as `|-|` (a cell `-` without padding) as a rule line. Output of `tabulate()` always pads cells with a space, so content lines contain whitespace and rule lines do not; hand-written tables without padding are outside what this parser promises.
- The header rule differs by format (`|`-started rule against `=` rule), and `psql` and `outline` also have border rules starting with `+` that must not be taken for it; a headerless `pipe` table starts with its delimiter row, which must yield empty headers, not a missing first row.
- `tabulate()` prints the float `300.0` as `300` and `0.0` as `0`, which parse back as `int`; the round trip holds under `==` but not under a type check, so the round-trip tests compare by equality.
- `tabulate/cli.py` falls back to `from tabulate import ...` when run as a script; the new relative import in `tabulate/__init__.py` must not break that path.
- Other pull requests add an entry to `_table_formats`, a parameter to `tabulate()` and text elsewhere in `README.md`; this change must touch only the import and `__all__` lines of `tabulate/__init__.py` and the README place named above.

## Open questions

None.
