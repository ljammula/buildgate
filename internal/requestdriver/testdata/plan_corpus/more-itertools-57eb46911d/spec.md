# Spec

## Problem

Sorted streams (log files, database cursors, sorted ID lists) are routinely compared and combined, and the inputs are often too large for `set` or `collections.Counter`. The standard library's `heapq.merge` only concatenates sorted inputs in order; neither it nor `more_itertools` offers union, intersection, difference or symmetric difference of sorted iterables in one lazy pass with constant memory. `more_itertools/more.py` has `sort_together`, `is_sorted` and `unique_to_each`, none of which streams.

## Scope

Add four public generator functions to `more_itertools/more.py`, directly after `is_sorted`, that treat already-sorted iterables as multisets and yield the result in the same sorted order:

- `sorted_union(*iterables, key=None, reverse=False)`
- `sorted_intersection(*iterables, key=None, reverse=False)`
- `sorted_difference(iterable, *others, key=None, reverse=False)`
- `sorted_symmetric_difference(iterable, other, *, key=None, reverse=False)`

`key` and `reverse` are keyword-only and mean what they mean for the built-in `sorted`: every input must already be sorted by `key` (the items themselves when `key` is `None`), ascending, or descending when `reverse` is true. Within one input, consecutive items with equal keys form a run; the length of the run is that key's count in that input. For each distinct key, in sorted order, each function yields a number of items determined by the counts, taken from a stated input's run in their original order.

All four read their inputs lazily, in a single pass, holding at most one run of each input in memory, and so work with infinite inputs. They use only `<` and `==` on keys.

The work has three parts that build on each other:

1. The shared machinery that walks several sorted inputs together one run at a time (a private helper in `more_itertools/more.py`, whose shape is the implementer's choice), with `sorted_union` and `sorted_intersection` on top of it, their stubs and their test classes.
2. `sorted_difference` and `sorted_symmetric_difference` on the same machinery, with their stubs and test classes.
3. Documentation: the four lines in `docs/api.rst`, the four links in the `README.rst` table, the `docs/versions.rst` entry, and docstrings that cross-reference the four functions.

Each function gets its name in `__all__` of `more_itertools/more.py` and `more_itertools/more.pyi` (alphabetical order), a docstring with `>>>` examples, and a test class in `tests/test_more.py` (`SortedUnionTests`, `SortedIntersectionTests`, `SortedDifferenceTests`, `SortedSymmetricDifferenceTests`), placed directly after `IsSortedTests`.

## Non-goals

- Sorting the inputs, or detecting unsorted inputs: the result for an input that is not sorted as declared is undefined, and no error is required.
- Set semantics that drop repeated items; callers can apply `unique_justseen` to the inputs or the output.
- A symmetric difference of more than two inputs.
- Supporting a single run of equal keys that is infinite (for example `itertools.repeat(1)` as an input).
- Changing `sort_together`, `is_sorted`, `unique_to_each` or any other existing function.

## Affected services and packages

The `more_itertools` Python package is affected through four new public functions and one or more private helpers in `more_itertools/more.py`, re-exported from the package root by the existing `from .more import *`. The stub, tests, API reference, README table and version history are updated with them. No runtime dependency, external service or existing function is affected.

## Acceptance criteria

1. `sorted_union`, `sorted_intersection`, `sorted_difference` and `sorted_symmetric_difference` in `more_itertools/more.py` have exactly the signatures given in Scope, are listed in that module's `__all__` in alphabetical position (`'sorted_difference'`, `'sorted_intersection'`, `'sorted_symmetric_difference'`, `'sorted_union'`, directly after `'sort_together'`), and are reachable from the `more_itertools` package root.
2. Each of the four functions in `more_itertools/more.py` returns an iterator (`iter(result) is result`) and accepts one-shot iterators as inputs. `key` and `reverse` cannot be passed positionally: `sorted_symmetric_difference([1], [2], None)` raises `TypeError`, as do `sorted_symmetric_difference([1])` and `sorted_difference()`.
3. `sorted_union` in `more_itertools/more.py` yields, for each distinct key in sorted order, the whole run of the input whose run for that key is longest, and of the earliest such input when several are equally long. `list(sorted_union([1, 3, 5], [2, 3, 4]))` returns `[1, 2, 3, 4, 5]`; `list(sorted_union([1, 1, 2], [1, 2, 2, 3]))` returns `[1, 1, 2, 2, 3]`; `list(sorted_union([1, 4], [2, 4, 4], [0, 4, 9]))` returns `[0, 1, 2, 4, 4, 9]`; `list(sorted_union([1, 1, 2]))` returns `[1, 1, 2]`; `list(sorted_union())` returns `[]`.
4. `sorted_union` in `more_itertools/more.py` honours `key` and `reverse`: `list(sorted_union(['a', 'B'], ['A', 'c'], key=str.lower))` returns `['a', 'B', 'c']`; `list(sorted_union(['a'], ['A', 'a'], key=str.lower))` returns `['A', 'a']`; `list(sorted_union([5, 3, 1], [4, 3, 2], reverse=True))` returns `[5, 4, 3, 2, 1]`.
5. `sorted_intersection` in `more_itertools/more.py` yields, for each distinct key in sorted order, the first `m` items of the first input's run, where `m` is the smallest run length for that key over all inputs (zero when an input lacks the key). `list(sorted_intersection([1, 2, 3, 5], [2, 3, 4, 5], [0, 3, 5]))` returns `[3, 5]`; `list(sorted_intersection([1, 1, 2, 2], [1, 2, 2, 2]))` returns `[1, 2, 2]`; `list(sorted_intersection([1, 2], []))` returns `[]`; `list(sorted_intersection([1, 2, 2]))` returns `[1, 2, 2]`; `list(sorted_intersection())` returns `[]`.
6. `sorted_intersection` in `more_itertools/more.py` honours `key` and `reverse`: with `a = ['a', 'B', 'c']` and `b = ['A', 'C']`, `list(sorted_intersection(a, b, key=str.lower))` returns `['a', 'c']` and `list(sorted_intersection(b, a, key=str.lower))` returns `['A', 'C']`; `list(sorted_intersection(['A', 'a', 'b'], ['a', 'B'], key=str.lower))` returns `['A', 'b']`; `list(sorted_intersection([5, 3, 2, 1], [4, 3, 1], reverse=True))` returns `[3, 1]`.
7. `sorted_intersection` in `more_itertools/more.py` stops as soon as any input is exhausted: `list(sorted_intersection([1, 2, 3], itertools.count()))` and `list(sorted_intersection(itertools.count(), [1, 2, 3]))` each return `[1, 2, 3]` and terminate, reading only a few items of the infinite input past `3` (never an item as large as `1000`).
8. `sorted_difference` in `more_itertools/more.py` yields, for each distinct key of `iterable` in sorted order, the first `d` items of that run, where `d` is the run's length minus the sum of the run lengths for that key in every one of `others`, and nothing when `d` is zero or negative. `list(sorted_difference([1, 2, 3, 4, 5], [2, 4], [5, 6]))` returns `[1, 3]`; `list(sorted_difference([1, 1, 1, 2, 3], [1, 2], [1]))` returns `[1, 3]`; `list(sorted_difference([1, 2], [1, 1, 1], [0, 3]))` returns `[2]`; `list(sorted_difference([1, 2, 2]))` returns `[1, 2, 2]`; `list(sorted_difference([], [1, 2]))` returns `[]`.
9. `sorted_difference` in `more_itertools/more.py` honours `key` and `reverse`: `list(sorted_difference(['A', 'a', 'b'], ['a'], key=str.lower))` returns `['A', 'b']`; `list(sorted_difference([5, 4, 3, 2, 1], [4, 2], reverse=True))` returns `[5, 3, 1]`.
10. `sorted_difference` in `more_itertools/more.py` stops as soon as `iterable` is exhausted: `list(sorted_difference([1, 2, 3, 4], itertools.count(0, 2)))` returns `[1, 3]` and terminates, never reading an item as large as `1000` from the infinite input.
11. `sorted_symmetric_difference` in `more_itertools/more.py` yields, for each distinct key in sorted order, with run lengths `a` in `iterable` and `b` in `other`: the first `a - b` items of `iterable`'s run when `a > b`, the first `b - a` items of `other`'s run when `b > a`, and nothing when they are equal. `list(sorted_symmetric_difference([1, 2, 3, 4], [3, 4, 5]))` returns `[1, 2, 5]`; `list(sorted_symmetric_difference([1, 1, 2, 3], [1, 3, 3, 4]))` returns `[1, 2, 3, 4]`; with either input empty the other is returned unchanged; with both empty the result is `[]`.
12. `sorted_symmetric_difference` in `more_itertools/more.py` honours `key` and `reverse`: with `a = ['a', 'b']` and `b = ['A', 'a', 'B']`, both `list(sorted_symmetric_difference(a, b, key=str.lower))` and `list(sorted_symmetric_difference(b, a, key=str.lower))` return `['A']`; `list(sorted_symmetric_difference([4, 3, 2, 1], [5, 4, 3], reverse=True))` returns `[5, 2, 1]`.
13. For lists of integers sorted ascending, the four functions in `more_itertools/more.py` agree with `collections.Counter`: with `c_i = Counter(input_i)`, `sorted_union` yields `sorted((c_1 | c_2 | ...).elements())`, `sorted_intersection` yields `sorted((c_1 & c_2 & ...).elements())`, `sorted_difference` yields `sorted((c_1 - c_2 - ...).elements())`, and `sorted_symmetric_difference` yields `sorted(((c_1 - c_2) + (c_2 - c_1)).elements())`; with the same lists reversed and `reverse=True`, `sorted_union` yields the reverse of its ascending result.
14. The four functions in `more_itertools/more.py` work on infinite inputs: the first seven items of `sorted_union(count(0, 2), count(0, 3))` are `[0, 2, 3, 4, 6, 8, 9]`; the first four of `sorted_intersection(count(0, 2), count(0, 3))` are `[0, 6, 12, 18]`; the first four of `sorted_difference(count(), count(0, 2))` are `[1, 3, 5, 7]`; the first five of `sorted_symmetric_difference(count(0, 2), count(0, 3))` are `[2, 3, 4, 8, 9]`.
15. The docstring of each of the four functions in `more_itertools/more.py` states that the inputs must already be sorted and that the result is undefined otherwise, explains the multiset rule and which input's items are yielded, explains `key` and `reverse`, names the other three functions with `:func:` references, and contains at least one doctest; the doctests include `>>> list(sorted_union([1, 3, 5], [2, 3, 4]))` with output `[1, 2, 3, 4, 5]` and `>>> list(sorted_difference([1, 2, 3, 4, 5], [2, 4], [5, 6]))` with output `[1, 3]`.
16. `more_itertools/more.pyi` lists the four names in its `__all__` in the same alphabetical positions and declares, wrapped as `ruff format` requires: `def sorted_union(*iterables: Iterable[_T], key: Callable[[_T], Any] | None = ..., reverse: bool = ...) -> Iterator[_T]: ...`; `def sorted_intersection(*iterables: Iterable[_T], key: Callable[[_T], Any] | None = ..., reverse: bool = ...) -> Iterator[_T]: ...`; `def sorted_difference(iterable: Iterable[_T], *others: Iterable[_T], key: Callable[[_T], Any] | None = ..., reverse: bool = ...) -> Iterator[_T]: ...`; `def sorted_symmetric_difference(iterable: Iterable[_T], other: Iterable[_T], *, key: Callable[[_T], Any] | None = ..., reverse: bool = ...) -> Iterator[_T]: ...`; `stubtest more_itertools.more more_itertools.recipes` reports no issues.
17. `docs/api.rst` has, in the `Combining` section under `**New itertools**`, the lines `.. autofunction:: sorted_union`, `.. autofunction:: sorted_intersection`, `.. autofunction:: sorted_difference` and `.. autofunction:: sorted_symmetric_difference`, in that order, directly after `.. autofunction:: sort_together`.
18. In the `Combining` row of the table in `README.rst`, four lines are added directly after the `sort_together` line, each of the form `` `NAME <https://more-itertools.readthedocs.io/en/stable/api.html#more_itertools.NAME>`_, `` for `sorted_union`, `sorted_intersection`, `sorted_difference` and `sorted_symmetric_difference`, in that order, padded so the grid table's borders stay aligned.
19. Under `Unreleased` in `docs/versions.rst`, a new group `* New functions:` placed above `* Changes to existing functions:` has the bullet ``* :func:`sorted_union`, :func:`sorted_intersection`, :func:`sorted_difference`, and :func:`sorted_symmetric_difference` were added``, indented like the bullets of the group below it.
20. The four test classes in `tests/test_more.py` cover, for their function: the examples above, empty and single inputs, `key`, `reverse`, a randomized comparison with `collections.Counter` using a seeded `random.Random`, infinite inputs, early termination where criteria 7 and 10 require it, and the `TypeError` cases; `python -m unittest` passes with line coverage of `more_itertools/more.py` still at 100%.

## Risks

- `itertools.groupby` invalidates a group as soon as the underlying iterator advances; a helper that keeps group objects from several inputs must copy each run before moving on, or runs silently come out empty.
- Building on `heapq.merge` gives correct output for finite inputs but cannot stop early, so `sorted_intersection` and `sorted_difference` would never return when another input is infinite (criteria 7 and 10).
- Reading one run ahead is needed to know that an input is exhausted; reading further ahead than that breaks the laziness that the infinite-input criteria rely on.
- Unsorted input is not detected. The functions then yield a plausible but wrong result; the docstrings must say so.
- A run is buffered in a list, so memory is proportional to the longest run of equal keys, and an infinite run never completes.
- Items whose keys are equal but which are not identical (`'a'` and `'A'` under `str.lower`) make the choice of source input observable; the rules in criteria 3, 5, 8 and 11 fix it and the tests must pin it.
- `docs/versions.rst`, `README.rst`, `docs/api.rst` and the `__all__` lists are edited by other changes in flight and can conflict textually.

## Open questions

None.
