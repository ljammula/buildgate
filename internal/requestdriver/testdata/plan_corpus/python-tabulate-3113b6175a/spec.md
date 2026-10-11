# Spec

## Problem

`tabulate` prints tables as markup for LaTeX, HTML, AsciiDoc, MediaWiki and several other systems, but not for Typst. A user writing a Typst document has to convert one of the other outputs by hand, and has to escape the characters Typst treats as markup (`#`, `*`, `_`, `$` and others) in every cell.

## Scope

Add an output format named `typst` to `_table_formats` in `tabulate/__init__.py`. `tabulate(..., tablefmt="typst")` prints one Typst `#table(...)` call: a `columns:` line with the number of columns, an `align:` line with one alignment per column, an optional `table.header(...)` line, one line per data row with every cell as a Typst content block in square brackets, and a closing parenthesis. Cell text is stripped of the padding `tabulate` adds, the characters Typst treats as markup are escaped with a backslash, and a line break inside a cell becomes a Typst line break. The row and line builders are new module-level functions next to the other format helpers (`_asciidoc_row`, `_latex_line_begin_tabular`); `tabulate()`, `_format_table` and the existing formats are not changed.

The scope also includes documenting the format: an entry in the format list and an example in the "Table format" section of `README.md`, a paragraph with a doctest example in the `tabulate()` docstring, and the name in the "Supported output formats" list of the command-line help (the `_main` docstring in `tabulate/cli.py` and its copy in `README.md`). Tests go in `test/test_output.py`.

## Non-goals

- Changing `tabulate()`, `_format_table`, `TableFormat`, `multiline_formats` or any existing format.
- A `typst_raw` variant that does not escape, captions, figures, strokes, column widths or any other `#table` argument.
- Support for `SEPARATING_LINE` rows, ANSI escape sequences or hyperlinks in this format.
- Escaping markup that is only special at the start of a line or in a sequence (`-`, `+`, `=`, `/`, `//`, `'`, `"`).
- New command-line options: `-f typst` works because the utility accepts every name in `tabulate_formats`.
- A `CHANGELOG` entry: it is written per release by the maintainer.

## Affected services and packages

The `tabulate` Python package is affected through one new entry in its table formats (`tabulate/__init__.py`), the help text of its command-line utility (`tabulate/cli.py`), `README.md` and `test/test_output.py`. No public signature, persistent data or runtime dependency is affected.

## Acceptance criteria

1. `_table_formats` in `tabulate/__init__.py` has a `"typst"` entry, so `"typst"` is in `tabulate.tabulate_formats`, which stays a sorted list of strings.
2. `tabulate` in `tabulate/__init__.py`, called as `tabulate([["spam", 42], ["eggs", 451], ["bacon", 0]], ["item", "qty"], tablefmt="typst")`, returns exactly these eight lines joined by `"\n"`, with no trailing newline: `#table(`, `  columns: 2,`, `  align: (left, right),`, `  table.header([item], [qty]),`, `  [spam], [42],`, `  [eggs], [451],`, `  [bacon], [0],`, `)`. The second, third, header and row lines are indented by exactly two spaces; cells are separated by `, ` and each row line ends with a comma.
3. Without headers the `typst` output of `tabulate` in `tabulate/__init__.py` has no `table.header` line and is otherwise the same: the call of criterion 2 without the headers returns `#table(`, `  columns: 2,`, `  align: (left, right),`, `  [spam], [42],`, `  [eggs], [451],`, `  [bacon], [0],`, `)`.
4. The `align:` line of the `typst` format in `tabulate/__init__.py` lists the alignment of each data column in order, mapping `left` to `left`, `right` to `right`, `center` to `center` and `decimal` to `right`, separated by `, ` inside parentheses. `tabulate([[1.5, "x", "y"], [22.25, "z", "w"]], tablefmt="typst", colalign=("center", "right"))` has the line `  align: (center, right, left),` and the rows `  [1.5], [x], [y],` and `  [22.25], [z], [w],`.
5. For a table with one column the `align:` line of the `typst` format in `tabulate/__init__.py` has a trailing comma inside the parentheses, as a one-element Typst array needs: `tabulate([["a"], ["b"]], tablefmt="typst")` returns `#table(`, `  columns: 1,`, `  align: (left,),`, `  [a],`, `  [b],`, `)`.
6. With headers and no data rows, the `typst` format in `tabulate/__init__.py` aligns every column `left`: `tabulate([], ["h1", "h2"], tablefmt="typst")` returns `#table(`, `  columns: 2,`, `  align: (left, left),`, `  table.header([h1], [h2]),`, `)`. `tabulate([], tablefmt="typst")` returns `""`.
7. Each cell of the `typst` format in `tabulate/__init__.py`, header cells included, is the formatted cell text with leading and trailing whitespace removed, inside square brackets; an empty or missing value is `[]`. `tabulate([["x", 1.0]], ["a", "b"], tablefmt="typst", floatfmt=".2f", stralign="center")` has the lines `  align: (center, right),`, `  table.header([a], [b]),` and `  [x], [1.00],`. With `showindex=True` the call of criterion 2 has `  columns: 3,`, `  align: (right, left, right),`, `  table.header([], [item], [qty]),` and first row `  [0], [spam], [42],`.
8. In each cell of the `typst` format in `tabulate/__init__.py`, header cells included, each of these twelve characters is preceded by one backslash, and no other character is changed: backslash, `#`, `$`, `*`, `_`, backtick, `[`, `]`, `<`, `>`, `@`, `~`. With the Python literals below as the header and the two rows, the header line and the two row lines of the output are exactly the three lines of the second block.

   ```python
   headers = ["h_1", "h2", "h3"]
   table = [["a_b #1", "x*y", "[z]"], ["c\\d", "$5 `q`", "<l> @r ~"]]
   ```

   ```text
     table.header([h\_1], [h2], [h3]),
     [a\_b \#1], [x\*y], [\[z\]],
     [c\\d], [\$5 \`q\`], [\<l\> \@r \~],
   ```
9. After escaping, each line break inside a cell of the `typst` format in `tabulate/__init__.py` (`"\r\n"`, `"\n"` or `"\r"`, the two-character form counting once) is replaced by a space, a backslash and a space. The row `["line1\nline2", "", None]` is printed as `  [line1 \ line2], [], [],` and the row `["a\r\nb", "c\rd", "e"]` as `  [a \ b], [c \ d], [e],`.
10. The `tabulate()` docstring in `tabulate/__init__.py` has, after its `latex_longtable` example, a paragraph introducing the `"typst"` format and a doctest example that prints a table with headers in it; the doctest passes.
11. `README.md` lists the format as the line `-   "typst"` immediately after the `-   "tsv"` line of the "Supported table formats" list, and has, after the `latex_longtable` paragraph and before the "Column alignment" heading, a paragraph describing the `typst` format followed by a `pycon` example `>>> print(tabulate(table, headers, tablefmt="typst"))` whose output is the eight lines of criterion 2; the README doctest passes.
12. The "Supported output formats" list in the `_main` docstring of `tabulate/cli.py`, and its copy in `README.md`, name `typst` between `tsv` and `unsafehtml`, so both contain the text `tsv, typst, unsafehtml.`; no other line of the usage text changes. `tabulate -f typst` on the standard input `a 1\nb 2\n` prints `#table(`, `  columns: 2,`, `  align: (left, right),`, `  [a], [1],`, `  [b], [2],`, `)`.
13. `test/test_output.py` has tests for the `typst` format with headers, without headers, with one column, with headers and no rows, for the alignment mapping, for escaping all twelve characters, and for line breaks and missing values, each comparing the whole output.
14. `python -m pytest`, `ruff check` and `ruff format --check` pass.

## Risks

- Row functions receive cells already padded to the column width and, for `decimal` alignment, padded on the right; the padding must be stripped per cell or it ends up inside the content blocks.
- The backslash is both an escaped character and the escape character, and the line-break replacement inserts one: escaping must happen once, before line breaks are replaced, or `\` is doubled twice or the inserted backslash is escaped.
- With headers and no rows `tabulate()` passes an empty list of column alignments to the line functions; the `align:` line must still have one entry per column.
- A format function is called with `(colwidths, colaligns)` for lines and `(cell_values, colwidths, colaligns)` for rows; the header row gets the header alignments, so the `align:` line must be built from the line call, not from the header row.
- Other pull requests add text elsewhere in `README.md`, the `tabulate()` docstring and the `_main` docstring; this change must touch only the places named above.

## Open questions

None.
