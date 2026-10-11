# Spec

## Problem

`more_itertools/more.py` generates the partitions of an iterable (`partitions`, `set_partitions`), but nothing generates the partitions of an integer: the ways of writing `n` as a sum of positive integers when the order of the terms does not matter. It is a standard combinatorial object (change-making, Young diagrams, distributing identical items), and the usual hand-written version is a recursive generator that hits the recursion limit for large `n` and cannot restrict the number of parts without generating and discarding everything else.

## Scope

Add two public functions to `more_itertools/more.py`, directly after `set_partitions`:

- `integer_partitions(n, k=None)` returns an iterator over the partitions of the non-negative integer `n`. Each partition is a tuple of positive integers in non-increasing order whose sum is `n`. Partitions are yielded lazily in reverse lexicographic order (comparing the tuples, largest first), so the first is `(n,)` and the last is `(1,) * n`. With `k` given, only partitions with exactly `k` parts are yielded, in the same relative order, generated directly rather than by filtering.
- `integer_partition_count(n, k=None)` returns how many partitions `integer_partitions` yields for the same arguments, computed arithmetically without generating them.

Both validate their arguments identically. The generator must not use recursion whose depth grows with `n` or `k`.

The scope also includes what the repository requires of a new public function: the names in `__all__` of `more_itertools/more.py` and `more_itertools/more.pyi` (alphabetical order), stub signatures, docstrings with `>>>` examples, lines in `docs/api.rst`, links in the `README.rst` table, an entry in `docs/versions.rst`, and the test classes `IntegerPartitionsTests` and `IntegerPartitionCountTests` in `tests/test_more.py`, directly after `SetPartitionsTests`.

## Non-goals

- Restricting part sizes (minimum, maximum, distinct or odd parts), or "at most `k`" parts.
- Compositions (ordered sums), or ranking and unranking of partitions (`nth_...`, `..._index`).
- Changing `partitions`, `set_partitions` or any other existing function.
- Validating argument types; `n` and `k` are integers.
- Caching counts between calls.

## Affected services and packages

The `more_itertools` Python package is affected through two new public functions in `more_itertools/more.py`, re-exported from the package root by the existing `from .more import *`. The stub, tests, API reference, README table and version history are updated with them. No runtime dependency, external service or existing function is affected.

## Acceptance criteria

1. `integer_partitions` and `integer_partition_count` in `more_itertools/more.py` have the signatures `integer_partitions(n, k=None)` and `integer_partition_count(n, k=None)`, accept `k` by position or by keyword, are listed in that module's `__all__` in alphabetical position (`'integer_partition_count'` then `'integer_partitions'`, between `'ilen'` and `'interleave'`), and are reachable from the `more_itertools` package root.
2. `integer_partitions` in `more_itertools/more.py` returns an iterator (`iter(result) is result`) of tuples of `int`; `list(integer_partitions(4))` returns exactly `[(4,), (3, 1), (2, 2), (2, 1, 1), (1, 1, 1, 1)]`, `list(integer_partitions(1))` returns `[(1,)]` and `list(integer_partitions(0))` returns `[()]`.
3. For every `n` from `0` to `18`, `list(integer_partitions(n))` in `more_itertools/more.py` contains every partition of `n` exactly once, each as a non-increasing tuple of positive integers summing to `n`, and the list equals itself sorted in descending tuple order.
4. With `k` given, `integer_partitions` in `more_itertools/more.py` yields exactly the partitions that have `k` parts, in the same relative order as without `k`: `list(integer_partitions(7, 3))` returns `[(5, 1, 1), (4, 2, 1), (3, 3, 1), (3, 2, 2)]` and `list(integer_partitions(6, k=2))` returns `[(5, 1), (4, 2), (3, 3)]`; for every `n` from `0` to `18` and `k` from `0` to `n + 2` the result equals `[p for p in integer_partitions(n) if len(p) == k]`.
5. The boundary values of `k` in `integer_partitions` in `more_itertools/more.py` behave as follows: `list(integer_partitions(0, 0))` returns `[()]`; `list(integer_partitions(5, 0))`, `list(integer_partitions(0, 2))` and `list(integer_partitions(5, 6))` each return `[]`; `list(integer_partitions(5, 1))` returns `[(5,)]`; `list(integer_partitions(5, 5))` returns `[(1, 1, 1, 1, 1)]`.
6. `integer_partitions` in `more_itertools/more.py` is lazy: the first three items of `integer_partitions(1000)` are `(1000,)`, `(999, 1)` and `(998, 2)`, and the first item of `integer_partitions(1000, 3)` is `(998, 1, 1)`, each obtained without generating further partitions.
7. With `k` given, `integer_partitions` in `more_itertools/more.py` generates the matching partitions directly instead of filtering all partitions of `n`: `list(integer_partitions(200, 2))` has exactly `100` items, the first `(199, 1)` and the last `(100, 100)`.
8. `integer_partitions` in `more_itertools/more.py` does not depend on the interpreter's recursion limit: `list(integer_partitions(3000, 3000))` returns `[(1,) * 3000]` under the default limit.
9. `integer_partition_count` in `more_itertools/more.py` returns an `int`: `1` for `n = 0`, `1` for `n = 1`, `5` for `n = 4`, `42` for `n = 10`, `190569292` for `n = 100`, `3972999029388` for `n = 200` and `24061467864032622473692149727991` for `n = 1000`, without generating the partitions.
10. With `k` given, `integer_partition_count` in `more_itertools/more.py` counts the partitions with exactly `k` parts: `integer_partition_count(7, 3)` returns `4`, `integer_partition_count(200, 2)` returns `100`, `integer_partition_count(100, 3)` returns `833`, `integer_partition_count(100, k=10)` returns `2977866`, `integer_partition_count(1000, 5)` returns `350697875`, `integer_partition_count(0, 0)` returns `1`, and `integer_partition_count(5, 0)`, `integer_partition_count(0, 2)` and `integer_partition_count(5, 6)` each return `0`.
11. For every `n` from `0` to `18` and every `k` from `0` to `n + 2`, `integer_partition_count(n)` and `integer_partition_count(n, k)` in `more_itertools/more.py` equal the number of items yielded by `integer_partitions(n)` and `integer_partitions(n, k)`.
12. Both functions in `more_itertools/more.py` raise `ValueError` with the message exactly `n must be non-negative` when `n < 0` (whatever `k` is), and otherwise `ValueError` with the message exactly `k must be non-negative` when `k` is not `None` and `k < 0`. `integer_partition_count` raises when called; for `integer_partitions` the error is raised no later than the first `next()`, so `list(integer_partitions(-1))`, `list(integer_partitions(-5, 2))`, `list(integer_partitions(3, -1))` and `list(integer_partitions(0, -2))` raise.
13. The docstrings of both functions in `more_itertools/more.py` describe the order of the output or what is counted, the meaning of `k`, and the `ValueError`, and contain at least the doctests `>>> list(integer_partitions(4))` with output `[(4,), (3, 1), (2, 2), (2, 1, 1), (1, 1, 1, 1)]` and `>>> integer_partition_count(100)` with output `190569292`.
14. `more_itertools/more.pyi` lists both names in its `__all__` in the same alphabetical positions and declares exactly `def integer_partitions(n: int, k: int | None = ...) -> Iterator[tuple[int, ...]]: ...` and `def integer_partition_count(n: int, k: int | None = ...) -> int: ...` (wrapped as `ruff format` requires), so that `stubtest more_itertools.more more_itertools.recipes` reports no issues.
15. `docs/api.rst` has, in the `Combinatorics` section under `**New itertools**`, the lines `.. autofunction:: integer_partitions` and `.. autofunction:: integer_partition_count`, in that order, directly after `.. autofunction:: set_partitions`.
16. In the `Combinatorics` row of the table in `README.rst`, two lines are added directly after the `set_partitions` line, containing `` `integer_partitions <https://more-itertools.readthedocs.io/en/stable/api.html#more_itertools.integer_partitions>`_, `` and `` `integer_partition_count <https://more-itertools.readthedocs.io/en/stable/api.html#more_itertools.integer_partition_count>`_, `` in that order, padded so the grid table's borders stay aligned.
17. Under `Unreleased` in `docs/versions.rst`, a new group `* New functions:` placed above `* Changes to existing functions:` has the bullet ``* :func:`integer_partitions` and :func:`integer_partition_count` were added``, indented like the bullets of the group below it.
18. `IntegerPartitionsTests` and `IntegerPartitionCountTests` in `tests/test_more.py` cover the examples above, a comparison with an independent brute-force generator for small `n` with and without `k`, the boundary values of `k`, laziness, the recursion-limit case, agreement between the two functions and every error message, and `python -m unittest` passes with line coverage of `more_itertools/more.py` still at 100%.

## Risks

- The number of partitions grows quickly (`190569292` for `n = 100`), so exhausting `integer_partitions(n)` is only practical for small `n`; the tests must exhaust only small inputs or inputs restricted by `k`.
- A recursive generator is the obvious implementation and passes every small case; it fails criterion 8 and is slow for deep partitions. The iterative algorithms are easy to get subtly wrong at the boundaries (`n = 0`, `k = 0`, `k = n`, `k > n`), which the brute-force comparison covers.
- A count that enumerates or filters partitions passes the small cases and never finishes for `n = 1000`; criteria 9 and 10 require an arithmetic method (a recurrence over part sizes, or equivalent).
- The names are close to the existing `partitions`, which partitions an iterable; the docstrings should point to each other's purpose to avoid confusion.
- `docs/versions.rst`, `README.rst`, `docs/api.rst` and the `__all__` lists are edited by other changes in flight and can conflict textually.

## Open questions

None.
