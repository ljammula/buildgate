# Spec

## Problem

The `tabulate` command-line utility in `tabulate/cli.py` cannot set the alignment of numeric or text columns, although the library's `tabulate()` has `numalign` and `stralign` for it. The one alignment option it has, `--colalign`, is missing from the `--help` text and from `README.md`, and its short form `-C`, which `_main` already tests for, is not registered with `getopt`, so `tabulate -C "left right"` exits with `option -C not recognized`. None of the three validates its value, so a misspelt alignment is silently ignored.

## Scope

Extend `_main` in `tabulate/cli.py` with two long options, `--numalign ALIGN` and `--stralign ALIGN`, whose values are passed to `tabulate()` as `numalign` and `stralign` for every input file, and make `-C ALIGNS` work as the short form of `--colalign ALIGNS`. Without the new options the output of the utility is unchanged. The value of `--colalign`/`-C` stays a whitespace-separated list of alignments, one per column from the left.

Each of the three options validates its value when it is read: `--numalign` and every item of `--colalign`/`-C` must be one of `right`, `center`, `left`, `decimal`; `--stralign` must be one of `right`, `center`, `left`. An unsupported value is reported the way an unsupported `--format` already is: a one-line message on standard error, the usage text on standard output, and exit status 3.

The scope also includes documenting the three options in the `_main` docstring (which is the `--help` text) and in the copy of that text under "Usage of the command line utility" in `README.md`, and adding tests for them to `test/test_cli.py`.

## Non-goals

- Changing `tabulate()` or anything else in `tabulate/__init__.py`.
- Adding the header alignment arguments (`headersalign`, `headersglobalalign`), `colglobalalign` or `rowalign` to the command line.
- Accepting `global` as an item of `--colalign`, or any separator other than whitespace between its items.
- Changing the parsing, defaults or messages of any other option.
- A `CHANGELOG` entry: it is written per release by the maintainer.

## Affected services and packages

The `tabulate` Python package is affected through its command-line utility only: `tabulate/cli.py`, its tests in `test/test_cli.py`, and the usage text in `README.md`. The library API, persistent data and runtime dependencies are not affected.

## Acceptance criteria

1. `_main` in `tabulate/cli.py`, run with no options on the standard input `a 1 x\nbb 22 yy\n`, prints exactly the four lines `--  --  --`, `a    1  x`, `bb  22  yy`, `--  --  --`, as it does today.
2. `_main` in `tabulate/cli.py` accepts `--numalign ALIGN` and passes it to `tabulate()` as `numalign`: with arguments `--numalign left` and the input of criterion 1 it prints exactly `--  --  --`, `a   1   x`, `bb  22  yy`, `--  --  --`.
3. `_main` in `tabulate/cli.py` accepts `--stralign ALIGN` and passes it to `tabulate()` as `stralign`: with arguments `--stralign right` and the input of criterion 1 it prints exactly `--  --  --`, ` a   1   x`, `bb  22  yy`, `--  --  --`.
4. `_main` in `tabulate/cli.py` accepts `-C ALIGNS` as well as `--colalign ALIGNS`, with identical behaviour: with arguments `-C "right left"` or `--colalign "right left"` (one argument holding both words) and the input of criterion 1 it prints exactly `--  --  --`, ` a  1   x`, `bb  22  yy`, `--  --  --`.
5. The options of criteria 2 to 4 combine with each other and with `-f`: `_main` in `tabulate/cli.py` with arguments `--numalign center --stralign center -f grid` and the input of criterion 1 prints exactly `+----+----+----+`, `| a  | 1  | x  |`, `+----+----+----+`, `| bb | 22 | yy |`, `+----+----+----+`.
6. `_main` in `tabulate/cli.py` rejects a `--numalign` value that is not one of `right`, `center`, `left`, `decimal`, a `--stralign` value that is not one of `right`, `center`, `left` (so `--stralign decimal` is rejected), and a `--colalign`/`-C` value in which any whitespace-separated item is not one of `right`, `center`, `left`, `decimal`. In each case it prints exactly `<value> is not a supported alignment` followed by a newline to standard error, where `<value>` is the offending option value (for `--colalign`/`-C`, the first offending item: `--colalign "left up"` reports `up`), prints the usage text to standard output, prints no table, and exits with status 3.
7. The `_main` docstring in `tabulate/cli.py`, which is the `--help` text, has the seven lines below in its `FORMAT:` section, immediately after the `-I INTFMT, --int INTFMT` line and before the `-f FMT, --format FMT` line, at the docstring's indentation (shown here without it); `tabulate --help` prints them and exits with status 0.

   ```text
   -C ALIGNS, --colalign ALIGNS
                             column alignments, space-separated, each one of:
                             right, center, left, decimal (e.g. "left right")
   --numalign ALIGN          alignment of numeric columns, one of:
                             right, center, left, decimal (default: decimal)
   --stralign ALIGN          alignment of text columns, one of:
                             right, center, left (default: left)
   ```
8. The usage block under "Usage of the command line utility" in `README.md` has the same lines in the same place, so that it still matches the `--help` output.
9. `test/test_cli.py` has in-process tests (using its `run_main_in_process` helper or `_main` directly) for criteria 2 to 6, each comparing the exact output or the exact error message and exit status.
10. `python -m pytest`, `ruff check` and `ruff format --check` pass.

## Risks

- `_main` and `_pprint_file` pass the options down by keyword through `_open_and_pprint_file`; a new argument that is read but not forwarded all the way to `tabulate()` leaves the output unchanged without any error, which only the exact-output tests catch.
- `test/test_cli.py` has an existing test that passes `--colalign "left left left"`; validation must keep accepting it.
- The usage text exists twice, in the `_main` docstring and in `README.md`; the two can drift apart if only one is edited.
- A pull request that adds an output format edits the "Supported output formats" lines of the same docstring and README block; this change must leave those lines alone.

## Open questions

None.
