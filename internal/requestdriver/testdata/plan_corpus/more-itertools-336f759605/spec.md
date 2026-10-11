# Spec

## Problem

`more_itertools` pairs each of its combinatoric generators with direct access by position: `nth_combination` and `combination_index`, `nth_permutation` and `permutation_index`, `nth_product` and `product_index`, `nth_combination_with_replacement` and `combination_with_replacement_index`. `powerset` in `more_itertools/recipes.py` has no such pair. Finding the subset at a given position, or the position of a given subset, means materializing up to `2**n` tuples, which is impossible for an input of even 40 items.

## Scope

Add two public functions to `more_itertools/more.py`, directly after `permutation_index`:

- `nth_powerset(iterable, index)` returns the tuple that `list(powerset(iterable))[index]` would return, computed directly from the sizes of the groups of equal-length subsets, without generating the earlier subsets.
- `powerset_index(element, iterable)` returns the integer that `list(powerset(iterable)).index(element)` would return, computed the same way. It is the inverse of `nth_powerset`.

Both follow the order `powerset` already defines: subsets by increasing size, and within one size in the order of `itertools.combinations`. Both accept any iterable, including one-shot iterators, and read each argument once.

The scope also includes what the repository requires of a new public function: the names in `__all__` of `more_itertools/more.py` and `more_itertools/more.pyi` (alphabetical order), stub signatures, docstrings with `>>>` examples, lines in `docs/api.rst`, links in the `README.rst` table, an entry in `docs/versions.rst`, and the test classes `NthPowersetTests` and `PowersetIndexTests` in `tests/test_more.py`, directly after `PermutationIndexTests`.

## Non-goals

- Changing `powerset`, `powerset_of_sets` or any existing `nth_*` or `*_index` function.
- A random-subset function, or counting subsets.
- Treating equal items of the input as one item: like `powerset`, both functions work by position, so repeated items produce repeated subsets.
- Positional access into `powerset_of_sets`.

## Affected services and packages

The `more_itertools` Python package is affected through two new public functions in `more_itertools/more.py`, re-exported from the package root by the existing `from .more import *`. The stub, tests, API reference, README table and version history are updated with them. No runtime dependency, external service or existing function is affected.

## Acceptance criteria

1. `nth_powerset` and `powerset_index` in `more_itertools/more.py` have the signatures `nth_powerset(iterable, index)` and `powerset_index(element, iterable)`, are listed in that module's `__all__` in alphabetical position (`'nth_powerset'` directly after `'nth_permutation'`, `'powerset_index'` directly before `'powerset_of_sets'`), and are reachable as `more_itertools.nth_powerset` and `more_itertools.powerset_index`.
2. `nth_powerset` in `more_itertools/more.py` returns a tuple equal to `list(powerset(iterable))[index]` for every valid non-negative index: `nth_powerset('abc', 0)` returns `()`, `nth_powerset('abc', 5)` returns `('a', 'c')`, `nth_powerset('abc', 7)` returns `('a', 'b', 'c')`, `nth_powerset([], 0)` returns `()`, and `nth_powerset([1, 1, 0], 5)` returns `(1, 0)`.
3. `nth_powerset` in `more_itertools/more.py` accepts a negative `index` counting from the end, as list indexing does: for an input of `n` items, every `index` with `-2**n <= index < 0` returns the same tuple as `index + 2**n`; `nth_powerset('abc', -1)` returns `('a', 'b', 'c')` and `nth_powerset('abc', -8)` returns `()`.
4. `nth_powerset` in `more_itertools/more.py` raises `IndexError` when `index >= 2**n` or `index < -2**n` for an input of `n` items: `nth_powerset('abc', 8)`, `nth_powerset('abc', -9)`, `nth_powerset('', 1)` and `nth_powerset('', -2)` each raise `IndexError`.
5. `nth_powerset` in `more_itertools/more.py` does not generate the preceding subsets: `nth_powerset(range(100), 5153)` returns `(0, 2, 7)`, `nth_powerset(range(100), 10**20)` returns `(2, 14, 15, 22, 23, 28, 33, 48, 61, 69, 72, 73, 78, 82, 83, 84, 86, 93, 97)`, and `nth_powerset(range(100), 2**100 - 1)` returns `tuple(range(100))`.
6. `powerset_index` in `more_itertools/more.py` returns an `int` equal to `list(powerset(iterable)).index(tuple(element))` whenever `element` is one of the subsets: `powerset_index('', 'abc')` returns `0`, `powerset_index('ac', 'abc')` returns `5`, `powerset_index(('a', 'b', 'c'), 'abc')` returns `7`, and `powerset_index((), [])` returns `0`.
7. When the input has repeated items, `powerset_index` in `more_itertools/more.py` returns the index of the first equal subset, as `list.index` does: `powerset_index((1, 0), [1, 1, 0])` returns `5`, and for every subset of `'abacba'` the result is the position of that subset's first occurrence in `list(powerset('abacba'))`.
8. `powerset_index` in `more_itertools/more.py` raises `ValueError` with the message exactly `element is not a subsequence of iterable` when `element` is not one of the subsets: items out of order (`powerset_index('ca', 'abc')`), an item that is absent (`powerset_index('ax', 'abc')`), an item used more often than it occurs (`powerset_index('aa', 'abc')`), an element longer than the input (`powerset_index('abcd', 'abc')`), and a non-empty element with an empty input (`powerset_index('a', '')`).
9. `powerset_index` in `more_itertools/more.py` does not generate the preceding subsets and inverts `nth_powerset`: `powerset_index((0, 2, 7), range(100))` returns `5153`, `powerset_index(range(100), range(100))` returns `2**100 - 1`, and `powerset_index(nth_powerset(range(60), i), range(60))` returns `i` for `i` in `0`, `1`, `60`, `61`, `123456789`, `2**59` and `2**60 - 1`.
10. Both functions in `more_itertools/more.py` accept one-shot iterators for every iterable argument: `nth_powerset(iter('abc'), 5)` returns `('a', 'c')` and `powerset_index(iter('ac'), iter('abc'))` returns `5`.
11. The docstrings of `nth_powerset` and `powerset_index` in `more_itertools/more.py` state the equivalence to `list(powerset(iterable))[index]` and `list(powerset(iterable)).index(element)`, name the exception each raises, and contain at least the doctests `>>> nth_powerset('abc', 5)` with output `('a', 'c')` and `>>> powerset_index('ac', 'abc')` with output `5`.
12. `more_itertools/more.pyi` lists both names in its `__all__` in the same alphabetical positions and declares exactly `def nth_powerset(iterable: Iterable[_T], index: int) -> tuple[_T, ...]: ...` and `def powerset_index(element: Iterable[_T], iterable: Iterable[_T]) -> int: ...`, so that `stubtest more_itertools.more more_itertools.recipes` reports no issues.
13. `docs/api.rst` has, in the `Combinatorics` section under `**New itertools**`, the lines `.. autofunction:: powerset_index` and `.. autofunction:: nth_powerset`, in that order, directly after `.. autofunction:: combination_with_replacement_index`.
14. In the `Combinatorics` row of the table in `README.rst`, a line containing `` `powerset_index <https://more-itertools.readthedocs.io/en/stable/api.html#more_itertools.powerset_index>`_ `` is added to the block that ends with `product_index`, and a line containing `` `nth_powerset <https://more-itertools.readthedocs.io/en/stable/api.html#more_itertools.nth_powerset>`_ `` to the block that ends with `nth_product`; each new line is the last of its block, the line before it gains a trailing comma, and the grid table's borders stay aligned.
15. Under `Unreleased` in `docs/versions.rst`, a new group `* New functions:` placed above `* Changes to existing functions:` has the bullet ``* :func:`nth_powerset` and :func:`powerset_index` were added``, indented like the bullets of the group below it.
16. `NthPowersetTests` and `PowersetIndexTests` in `tests/test_more.py` cover comparison with `powerset` for several small inputs, negative indexes, repeated items, the long inputs of criteria 5 and 9, the round trip, iterator inputs and every error case, and `python -m unittest` passes with line coverage of `more_itertools/more.py` still at 100%.

## Risks

- The offsets are sums of binomial coefficients; an off-by-one between "subsets smaller than `r`" and "subsets of size up to `r`" yields results that are right for index `0` and wrong elsewhere. The exhaustive comparison with `powerset` in the tests guards against it.
- With repeated input items several positions hold equal subsets. `powerset_index` returns the first, so `nth_powerset(iterable, powerset_index(e, iterable))` equals `e`, but `powerset_index(nth_powerset(iterable, i), iterable)` may be smaller than `i` for such inputs.
- Both functions hold the whole input in memory as a tuple, like the existing `nth_*` functions.
- `docs/versions.rst`, `README.rst`, `docs/api.rst` and the `__all__` lists are edited by other changes in flight and can conflict textually.

## Open questions

None.
