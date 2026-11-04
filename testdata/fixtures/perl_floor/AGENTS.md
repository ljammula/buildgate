# perl_floor

A tiny Perl module with plain Perl tests: `lib/Calc.pm` and `t/*.t`. There
is no test framework and no CPAN dependency; each test script exits non-zero
on failure.

- Run the tests: `for f in t/*.t; do perl -Ilib "$f" || exit 1; done`
- A new function goes in `lib/Calc.pm`, with its own `t/<name>.t`.
