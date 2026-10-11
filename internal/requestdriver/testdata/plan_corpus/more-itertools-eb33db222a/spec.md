# Spec

## Problem

`more_itertools/recipes.py` can factor an integer (`factor`), count its totatives (`totient`) and test primality (`is_prime`), but it has no way to list the divisors of an integer. Callers who need them write a trial-division loop over `range(1, n + 1)`, which is unusable for large `n`, or rebuild the divisors from `factor` by hand.

## Scope

Add a public function `divisors(n)` to `more_itertools/recipes.py`, directly after `totient`. It returns an iterator over every positive divisor of the integer `n`, each exactly once, in ascending order, starting with `1` and ending with `n`. It builds the divisors from the prime factorization that `factor` already provides, so it stays fast for numbers with large prime factors. `n` less than `1` is an error.

The scope also includes everything the repository requires of a new public function: the name in `__all__` of `more_itertools/recipes.py` and of `more_itertools/recipes.pyi` (both kept in alphabetical order), a signature in the stub, a docstring with `>>>` examples (they run as tests), a line in `docs/api.rst`, a link in the `README.rst` table, an entry in `docs/versions.rst`, and a `DivisorsTests` class in `tests/test_recipes.py`, directly after `TotientTests`.

## Non-goals

- Proper divisors only, divisor counts, divisor sums, or any other divisor function.
- Accepting negative integers or zero, or yielding negative divisors.
- Validating the type of `n` (floats, strings, `bool`); only integers are supported.
- Changing `factor`, `totient`, `is_prime` or any other existing function.

## Affected services and packages

The `more_itertools` Python package is affected through one new public function in `more_itertools/recipes.py`, re-exported from the package root by the existing `from .recipes import *`. Its stub, tests, API reference, README table and version history are updated with it. No runtime dependency, external service or existing function is affected.

## Acceptance criteria

1. `divisors` in `more_itertools/recipes.py` has the signature `divisors(n)`, is listed in that module's `__all__` in alphabetical position (between `'convolve'` and `'dotproduct'`), and is reachable as `more_itertools.divisors`.
2. `divisors` in `more_itertools/recipes.py` returns an iterator (`iter(result) is result`), and `list(divisors(12))` returns exactly `[1, 2, 3, 4, 6, 12]`, `list(divisors(36))` returns exactly `[1, 2, 3, 4, 6, 9, 12, 18, 36]`, `list(divisors(13))` returns exactly `[1, 13]` and `list(divisors(1))` returns exactly `[1]`.
3. For every integer `n` from `1` to `2000`, `list(divisors(n))` in `more_itertools/recipes.py` equals `[d for d in range(1, n + 1) if n % d == 0]`: every divisor once, in ascending order.
4. `divisors` in `more_itertools/recipes.py` does not use trial division up to `n`: `list(divisors(2**61 - 1))` returns `[1, 2305843009213693951]`, `list(divisors(2 * (2**61 - 1)))` returns `[1, 2, 2305843009213693951, 4611686018427387902]`, and `list(divisors(10**18))` has exactly `361` items in ascending order, starting `1, 2, 4, 5` and ending with `10**18`.
5. `list(divisors(n))` in `more_itertools/recipes.py` raises `ValueError` with the message exactly `n must be a positive integer` for `n = 0`, `n = -1` and `n = -12`.
6. The docstring of `divisors` in `more_itertools/recipes.py` says the divisors are yielded in ascending order, says `ValueError` is raised when `n` is less than `1`, and contains at least the doctest `>>> list(divisors(12))` with the output `[1, 2, 3, 4, 6, 12]`.
7. `more_itertools/recipes.pyi` lists `'divisors'` in its `__all__` in the same alphabetical position and declares exactly `def divisors(n: int) -> Iterator[int]: ...`, so that `stubtest more_itertools.more more_itertools.recipes` reports no issues.
8. `docs/api.rst` has the line `.. autofunction:: divisors` in the `Integer Math` section, under `**Itertools recipes**`, directly above `.. autofunction:: factor`.
9. The `Integer math` row of the table in `README.rst` has a new line directly after the `factor` line containing `` `divisors <https://more-itertools.readthedocs.io/en/stable/api.html#more_itertools.divisors>`_, ``, padded so the grid table's borders stay aligned with the neighbouring lines.
10. Under `Unreleased` in `docs/versions.rst`, a new group `* New functions:` placed above `* Changes to existing functions:` has the bullet ``* :func:`divisors` was added``, indented like the bullets of the group below it.
11. `DivisorsTests` in `tests/test_recipes.py` covers the examples of criterion 2, a brute-force comparison over a range of `n`, a large prime, the iterator behaviour and the `ValueError` message, and `python -m unittest` passes with line coverage of `more_itertools/recipes.py` still at 100%.

## Risks

- `divisors` depends on `factor`; a number whose factorization is slow (a product of two very large primes) makes `divisors` equally slow. That is inherent to the approach and not addressed here.
- The number of divisors can be large for highly composite inputs, and the divisors must be sorted before the first is yielded, so memory is proportional to the divisor count.
- `README.rst` is a grid table that `twine check` renders during `make package`; a line with misaligned borders breaks that check even though the unit tests still pass.
- `docs/versions.rst`, `README.rst`, `docs/api.rst` and the `__all__` lists are edited by other changes in flight; the edits here are single lines, but they can conflict textually.

## Open questions

None.
