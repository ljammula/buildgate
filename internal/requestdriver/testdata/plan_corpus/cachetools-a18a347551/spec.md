# Spec

## Problem

`cachetools.func` in `src/cachetools/func.py` offers a `functools.lru_cache`-style decorator for every cache class except `TLRUCache`: there are `fifo_cache`, `lfu_cache`, `lru_cache`, `rr_cache` and `ttl_cache`, but a caller who wants a thread-safe memoizing decorator whose results expire at a per-result time has to assemble `cached`, `TLRUCache`, a key function and a `threading.Condition` by hand.

## Scope

Add a `tlru_cache` decorator factory to `src/cachetools/func.py` with the signature `tlru_cache(ttu, maxsize=128, timer=time.monotonic, typed=False)`. It builds on the module's existing private `_cache` helper exactly as `ttl_cache` does, storing results in a `cachetools.TLRUCache(maxsize, ttu, timer)`, so the wrapper has the same `cache_info()`, `cache_clear()` and `cache_parameters()` functions, the same `threading.Condition` locking and the same `typed` handling as the other decorators in the module. `maxsize=None` gives a cache with no size limit in which results still expire, through a private `_UnboundTLRUCache` subclass modelled on the existing `_UnboundTTLCache`.

Because `ttu` is a required first argument, `tlru_cache` has no bare `@tlru_cache` form that takes the user function directly; a first argument that is not callable is rejected.

The scope also includes the type stub in `src/cachetools/func.pyi`, the `__all__` tuples of both files, a documentation entry in `docs/index.rst`, and tests in `tests/test_func.py`.

## Non-goals

- Changing `fifo_cache`, `lfu_cache`, `lru_cache`, `rr_cache`, `ttl_cache`, `_cache` or `_UnboundTTLCache`.
- Changing `TLRUCache` or anything else in `src/cachetools/__init__.py` or `src/cachetools/__init__.pyi`.
- A bare `@tlru_cache` form, or a default `ttu`.
- Editing `CHANGELOG.rst` (the maintainer writes it at release time) or `README.rst`.

## Affected services and packages

The `cachetools` Python package is affected through its public `cachetools.func` module, that module's type stub, its documentation section and its tests. No external service, persistent data, or runtime dependency is affected.

## Acceptance criteria

1. `tlru_cache` in `src/cachetools/func.py` has the signature `tlru_cache(ttu, maxsize=128, timer=time.monotonic, typed=False)`, and `"tlru_cache"` is in `cachetools.func.__all__`, which stays sorted alphabetically: `("fifo_cache", "lfu_cache", "lru_cache", "rr_cache", "tlru_cache", "ttl_cache")`.
2. `tlru_cache(ttu, maxsize=2, timer=t)(f)` in `src/cachetools/func.py` returns a wrapper for which `cache_parameters()` returns exactly `{"maxsize": 2, "typed": False}` and `cache_info()` on the fresh wrapper equals `(0, 0, 2, 0)`; calling the wrapper twice with the same argument calls `f` once and `cache_info()` then equals `(1, 1, 2, 1)`.
3. On each cache miss, `tlru_cache` in `src/cachetools/func.py` causes `ttu` to be called once as `ttu(key, value, now)`, where `key` equals `cachetools.keys.hashkey(*args, **kwargs)` for the call's arguments (`cachetools.keys.typedkey(*args, **kwargs)` when `typed=True`), `value` is the result of `f`, and `now` is the value returned by `timer`; for `f = lambda n: n` and a timer returning `0`, the call `wrapper(10)` calls `ttu((10,), 10, 0)`.
4. A result cached through `tlru_cache` in `src/cachetools/func.py` is returned without calling `f` while `timer() < expires`, where `expires` is the value `ttu` returned for it; once `timer() < expires` is false the next call with the same arguments calls `f` again and counts as a miss. With `ttu = lambda key, value, now: now + 10` and a timer that returns `0` and later `10`, `wrapper(1)` at `0`, `wrapper(1)` at `0`, `wrapper(1)` at `10` calls `f` exactly twice and `cache_info()` equals `(1, 2, 2, 1)` afterwards.
5. `tlru_cache(ttu, maxsize=None)` in `src/cachetools/func.py` never evicts for size: after 200 calls with 200 distinct arguments and a `ttu` that returns `now + 1000` on a frozen timer, `cache_info()` equals `(0, 200, None, 200)` and `cache_parameters()` returns `{"maxsize": None, "typed": False}`. The cache behind it is an instance of a private class `_UnboundTLRUCache(TLRUCache)` defined in `src/cachetools/func.py`, whose `maxsize` property returns `None`.
6. `tlru_cache(ttu, typed=True)` in `src/cachetools/func.py` caches `wrapper(1)` and `wrapper(1.0)` separately (two misses), and `cache_parameters()` returns `{"maxsize": 128, "typed": True}`; with the default `typed=False` the second of those calls is a hit.
7. `tlru_cache` in `src/cachetools/func.py` raises `TypeError` with the exact message `ttu must be callable` when its first argument is not callable, for example `tlru_cache(128)`, at the time `tlru_cache` is called and before any function is decorated.
8. `src/cachetools/func.pyi` declares `def tlru_cache(ttu: Callable[[Any, Any, Any], Any], maxsize: int | None = 128, timer: Callable[[], Any] = ..., typed: bool = False) -> Callable[[Callable[_P, _R]], _cachetools_cache_wrapper[_P, _R]]: ...` (a single signature, no overloads), and its `__all__` matches the tuple in criterion 1.
9. `docs/index.rst` has a `.. decorator:: tlru_cache(ttu, maxsize=128, timer=time.monotonic, typed=False)` entry in the `cachetools.func` section, placed between the `rr_cache` entry and the `ttl_cache` entry. It states that the expiration time of a result is `ttu(key, value, timer())`, refers to `cachetools.TLRUCache`, and states that the decorator cannot be applied directly to a user function because `ttu` is required.
10. `tests/test_func.py` has a test class `TLRUDecoratorTest` with tests for criteria 2 to 7, each using an explicit fake timer rather than sleeping, and the whole suite passes with `python -m pytest -q tests`.

## Risks

- `ttu` receives the cache key, not the original arguments. A caller who expects `ttu(n, value, now)` gets `ttu((n,), value, now)`; the documentation entry has to say so plainly.
- Writing `@tlru_cache` without arguments passes the user function as `ttu` and returns a decorator instead of a wrapper. This cannot be detected, because `ttu` is itself a callable; the documentation states the restriction.
- The repository's own checks (`tox`: ruff, ruff-format, pyright, the Sphinx build with warnings as errors) apply to the new code, stub and documentation entry; a stub that disagrees with the implementation fails pyright even though the unit tests pass.

## Open questions

None.
