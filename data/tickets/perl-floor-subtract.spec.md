# Ticket: add subtract to Calc

This is an existing repo. Deliberately tiny and dependency-free: this
ticket exists as a standing fixture for `make live-smoke` that proves the
language-agnostic floor, a project in a language buildgate has no profile,
reproduction runner or toolchain for (plain Perl, no test framework), built
on its `verify_command`'s exit status and its committed `test_patterns`
alone. Target repo: the perl_floor fixture (`testdata/fixtures/perl_floor`).

## Goal

`lib/Calc.pm` defines `add`. Add a sibling `subtract($a, $b)` returning
`$a - $b`, same style, plus a plain Perl test for it.

## Required changes

**`lib/Calc.pm`**:

1. Add `subtract` immediately after `add`, with a one-line comment in the
   same style ("# subtract returns its first argument minus its second.").
2. Do not change `add`.

**`t/subtract.t`** (new file):

1. A plain Perl script in the style of `t/add.t`: `use strict; use
   warnings; use Calc;`, no test module.
2. It dies with a message naming the call when `Calc::subtract(7, 3)` is
   not `4` or `Calc::subtract(2, 5)` is not `-3`, and prints `ok subtract`
   otherwise.

## Out of scope

- No CPAN module, no test framework, no build file.
- No change to `add` or `t/add.t`.

Allowed-Files: lib/Calc.pm, t/subtract.t

## Acceptance / verification

Verify-Command: for f in t/*.t; do perl -Ilib "$f" || exit 1; done

- Every `t/*.t` script exits 0, including the new `t/subtract.t`.
- `add`'s behaviour is unchanged.

Success criterion: `Calc::subtract(7, 3) == 4` and
`Calc::subtract(2, 5) == -3`, both checked by `t/subtract.t`.
