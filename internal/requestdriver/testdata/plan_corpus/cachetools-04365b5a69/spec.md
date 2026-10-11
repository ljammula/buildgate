# Spec

## Problem

`src/cachetools/__init__.py` provides FIFO, LFU, LRU, random-replacement and time-based caches, but no cache that keeps recently read items without reordering on every read. `LRUCache.__getitem__` moves the item on each hit, and `FIFOCache` ignores reads altogether. The SIEVE algorithm sits between the two: a read only sets a flag on the item, and eviction sweeps a hand over the items in insertion order, sparing each flagged item once.

## Scope

Add a `SIEVECache(maxsize, getsizeof=None)` class to `src/cachetools/__init__.py`, derived from `Cache` and following the conventions of the existing subclasses (parent methods called through default parameters such as `cache_setitem=Cache.__setitem__`, an O(1) `clear()`, the `"%s is empty" % type(self).__name__` message from `popitem()`). The class is placed after `RRCache` and before `_TimedCache`.

The algorithm, which the acceptance criteria make exact:

- Items are kept in insertion order, oldest to newest. Each item has a *visited* flag, initially false.
- Reading an item with `cache[key]` sets its flag. Nothing is reordered.
- A *hand* marks where the next eviction starts. `popitem()` starts at the hand's item, or at the oldest item when the hand has no item. While the item under the hand is visited, it clears that flag and moves the hand to the next newer item, wrapping from the newest item to the oldest. It removes the first item it finds whose flag is not set, and leaves the hand on the item that was next newer than the removed one. If the removed item was the newest, the hand has no item, and the next `popitem()` starts at the oldest item even if newer items have been inserted meanwhile.

`popitem()`, `__getitem__`, `__setitem__` and `__delitem__` are O(1) apart from the sweep itself (a doubly-linked list with a key-to-node dictionary, as `LFUCache` and `TTLCache` use, is the expected structure).

The scope also includes the stub in `src/cachetools/__init__.pyi`, the `__all__` tuples of both files, a documentation entry in `docs/index.rst`, and a new test module `tests/test_sieve.py`.

## Non-goals

- A `sieve_cache` decorator in `src/cachetools/func.py`; `func.py` and `func.pyi` are not changed.
- Changing `Cache` or any existing cache class.
- Exposing the hand or the visited flags as public attributes or methods.
- Defining the order of iteration over a `SIEVECache`; it is whatever `Cache.__iter__` gives.
- Supporting or documenting pickling beyond what the shared `CacheTestMixin` tests already exercise.
- Editing `CHANGELOG.rst` (the maintainer writes it at release time) or `README.rst`.

## Affected services and packages

The `cachetools` Python package is affected through its top-level public API (`cachetools.SIEVECache`), the top-level type stub, the "Cache implementations" documentation section and a new test module. No external service, persistent data, or runtime dependency is affected.

## Acceptance criteria

1. `SIEVECache` in `src/cachetools/__init__.py` is a subclass of `Cache` with the constructor signature `SIEVECache(maxsize, getsizeof=None)`, and `"SIEVECache"` is in `cachetools.__all__` between `"RRCache"` and `"TLRUCache"`.
2. `SIEVECache.popitem()` in `src/cachetools/__init__.py` returns the removed `(key, value)` pair and, on an empty cache, raises `KeyError` with the exact message `SIEVECache is empty` and with `__cause__` and `__context__` both `None`.
3. With no item read, `SIEVECache` in `src/cachetools/__init__.py` evicts in insertion order: after `cache = SIEVECache(maxsize=3)` and inserting keys `1, 2, 3` (each with a value equal to its key), three `popitem()` calls return `(1, 1)`, `(2, 2)`, `(3, 3)`.
4. Reading with `cache[key]` in `SIEVECache.__getitem__` (`src/cachetools/__init__.py`) sets the item's visited flag and spares it once: for `SIEVECache(maxsize=3)` with keys `"a", "b", "c"` inserted in that order and `cache["a"]` read once, inserting `"d"` evicts `"b"` (remaining keys `{"a", "c", "d"}`); then inserting `"e"` evicts `"c"`; then inserting `"f"` evicts `"d"`, leaving `{"a", "e", "f"}`.
5. The hand in `SIEVECache.popitem()` (`src/cachetools/__init__.py`) wraps from the newest item to the oldest: for `SIEVECache(maxsize=3)` with keys `1, 2, 3` inserted and all three read once, `popitem()` returns `(1, 1)`, then `(2, 2)`, then `(3, 3)`.
6. After `SIEVECache.popitem()` (`src/cachetools/__init__.py`) removes the newest item, the next eviction starts at the oldest item: for `SIEVECache(maxsize=4)` with keys `1, 2, 3, 4` inserted and keys `1, 2, 3` read once, `popitem()` returns `(4, 4)`; after then inserting key `5`, `popitem()` returns `(1, 1)`.
7. `SIEVECache.__delitem__` in `src/cachetools/__init__.py` (and therefore `del cache[key]` and `cache.pop(key)`) on the item under the hand moves the hand to the next newer item: for `SIEVECache(maxsize=4)` with keys `1, 2, 3, 4` inserted and key `1` read once, `popitem()` returns `(2, 2)`, then after `del cache[3]`, `popitem()` returns `(4, 4)` and the next `popitem()` returns `(1, 1)`. Deleting any other item leaves the hand where it is.
8. `SIEVECache.__setitem__` in `src/cachetools/__init__.py` for a key already in the cache replaces the value without moving the item and without setting or clearing its visited flag: for `SIEVECache(maxsize=2)` with keys `1, 2` inserted, `cache[1] = 10` followed by `popitem()` returns `(1, 10)`. `key in cache`, `len(cache)` and iteration do not set the flag either: for `SIEVECache(maxsize=2)` with keys `1, 2` inserted, evaluating `1 in cache` and `list(cache)` and then calling `popitem()` returns `(1, 1)`. `cache.get(key)` on a present key sets the flag, as it reads through `__getitem__`.
9. In a `SIEVECache` subclass that defines `__missing__` (`src/cachetools/__init__.py`), `cache[key]` for an absent key returns the `__missing__` result and does not raise, whether or not `__missing__` stored the key.
10. `SIEVECache.clear()` in `src/cachetools/__init__.py` empties the cache and resets the hand without calling `popitem()`: after inserting `1, 2, 3` into `SIEVECache(maxsize=3)`, reading all three, calling `popitem()` once and then `clear()`, the cache has `len(cache) == 0` and `currsize == 0`, and after inserting `7, 8` a `popitem()` returns `(7, 7)`.
11. `src/cachetools/__init__.pyi` declares `class SIEVECache(Cache[_KT, _VT]): ...` after `RRCache`, and its `__all__` lists `"SIEVECache"` between `"RRCache"` and `"TLRUCache"`.
12. `docs/index.rst` has an `.. autoclass:: SIEVECache(maxsize, getsizeof=None)` entry with `:members: popitem` in the "Cache implementations" section, placed between the `RRCache` entry and the `TTLCache` entry. It describes the visited flag, the hand moving from the oldest item towards the newest and wrapping, and states that reading an item does not reorder the cache and that replacing an existing key's value neither moves the item nor sets its flag.
13. `tests/test_sieve.py` defines `class SIEVECacheTest(unittest.TestCase, CacheTestMixin)` with `Cache = SIEVECache`, so every shared `CacheTestMixin` test runs against the new class, plus tests for criteria 3 to 8 and 10, and the whole suite passes with `python -m pytest -q tests`.

## Risks

- `Cache.__setitem__` may evict items, including the key being replaced, before the subclass sees the new value. `SIEVECache.__setitem__` must decide whether the key is new only after the parent call returns, or a replaced key that was evicted on the way is left without a list node.
- `popitem()` removes through `self.pop(key)`, which runs `__getitem__` and so sets the visited flag of the item being removed; that is harmless only because the item is deleted in the same call. An implementation that reads the flag after `pop` would misbehave.
- The shared `CacheTestMixin` pickles each cache class. A linked structure with `__slots__` nodes pickles as `LFUCache`'s does; a node type that cannot be pickled fails those shared tests.
- The repository's own checks (`tox`: ruff, ruff-format, pyright, the Sphinx build with warnings as errors) apply to the new class, stub, documentation entry and tests.

## Open questions

None.
