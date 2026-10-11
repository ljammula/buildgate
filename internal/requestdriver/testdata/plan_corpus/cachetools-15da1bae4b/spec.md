# Spec

## Problem

The key functions in `src/cachetools/keys.py` (`hashkey`, `methodkey`, `typedkey`, `typedmethodkey`) require every argument of the memoized function to be hashable. A function that takes a `list`, `dict` or `set` argument therefore cannot be used with `@cached` or `@cachedmethod` without a hand-written key function; the documentation only shows how to write one for a single `dict` keyword argument (`envkey`).

## Scope

Add three public functions to `src/cachetools/keys.py`:

- `freeze(obj)`: returns a hashable equivalent of a built-in container, converting recursively, and returns any other object unchanged.
- `frozenkey(*args, **kwargs)`: a key function that applies `freeze` to every positional and keyword argument value and then builds the key with the existing `hashkey`.
- `frozenmethodkey(self, *args, **kwargs)`: the same, ignoring its first positional argument, for use with `@cachedmethod`.

The conversion rules of `freeze` are fixed by the acceptance criteria below; type checks use `isinstance`, so subclasses of the listed built-in types are converted like their base type and lose their subclass.

The scope also includes the stubs in `src/cachetools/keys.pyi`, the `__all__` tuples of both files, documentation in the `cachetools.keys` section of `docs/index.rst`, and tests in `tests/test_keys.py`.

## Non-goals

- Changing `hashkey`, `methodkey`, `typedkey`, `typedmethodkey` or `_HashedTuple`.
- Typed variants of the new key functions.
- Converting any type other than `dict`, `list`, `tuple`, `set`, `frozenset` and `bytearray` (and their subclasses); in particular no general `collections.abc.Mapping`, `Sequence` or `Set` handling, and no handling of dataclasses, NumPy arrays or generators.
- Detecting reference cycles: a self-referencing container raises `RecursionError` from `freeze`, and that is not caught.
- Keeping containers of different types distinct: `freeze([1, 2])` and `freeze((1, 2))` are equal by design.
- Changing the default `key` of `cached` or `cachedmethod`, or anything under `src/cachetools/` other than `keys.py` and `keys.pyi`.
- Editing `CHANGELOG.rst` (the maintainer writes it at release time) or `README.rst`.

## Affected services and packages

The `cachetools` Python package is affected through its public `cachetools.keys` module, that module's type stub, its documentation section and its tests. No external service, persistent data, or runtime dependency is affected.

## Acceptance criteria

1. `cachetools.keys.__all__` in `src/cachetools/keys.py` is exactly `("freeze", "frozenkey", "frozenmethodkey", "hashkey", "methodkey", "typedkey", "typedmethodkey")`.
2. `freeze` in `src/cachetools/keys.py` has the signature `freeze(obj)` and, for an `obj` that is an instance of `list` or `tuple`, returns a plain `tuple` (exactly `type(result) is tuple`) of `freeze(item)` for each item in order: `freeze([1, [2, 3]]) == (1, (2, 3))` and `freeze((1, [2])) == (1, (2,))`.
3. `freeze` in `src/cachetools/keys.py`, for an instance of `dict`, returns a `frozenset` of `(key, freeze(value))` pairs, leaving keys unchanged: `freeze({"a": [1], "b": {2}}) == frozenset({("a", (1,)), ("b", frozenset({2}))})`, and `freeze(collections.OrderedDict(a=1)) == frozenset({("a", 1)})`.
4. `freeze` in `src/cachetools/keys.py`, for an instance of `set` or `frozenset`, returns a `frozenset` of `freeze(item)` for each item: `freeze({(1, 2), (3,)}) == frozenset({(1, 2), (3,)})` and `freeze(frozenset({1})) == frozenset({1})`.
5. `freeze` in `src/cachetools/keys.py`, for an instance of `bytearray`, returns `bytes(obj)`: `freeze(bytearray(b"ab")) == b"ab"` with `type(result) is bytes`.
6. `freeze` in `src/cachetools/keys.py` returns any other object itself (`freeze(obj) is obj`), including `str`, `bytes`, `int`, `None` and instances of arbitrary classes, whether or not they are hashable; it never calls `hash()` on its argument.
7. `frozenkey` in `src/cachetools/keys.py` has the signature `frozenkey(*args, **kwargs)` and returns `hashkey(*[freeze(a) for a in args], **{k: freeze(v) for k, v in kwargs.items()})`, so `frozenkey(1, [2, 3], x={"a": [4]}) == hashkey(1, (2, 3), x=frozenset({("a", (4,))}))`, `frozenkey() == hashkey()`, and for hashable non-container arguments `frozenkey(1, "a", x=None) == hashkey(1, "a", x=None)`.
8. The result of `frozenkey` in `src/cachetools/keys.py` is hashable whenever every leaf value is hashable, is independent of `dict` insertion order and of `set` iteration order (`frozenkey({"a": 1, "b": 2}) == frozenkey({"b": 2, "a": 1})` with equal hashes), and depends on `list` order (`frozenkey([1, 2]) != frozenkey([2, 1])`). Calling `hash()` on the result raises `TypeError` when a leaf value is unhashable.
9. `frozenmethodkey` in `src/cachetools/keys.py` has the signature `frozenmethodkey(self, *args, **kwargs)` and returns `frozenkey(*args, **kwargs)`: `frozenmethodkey("x", [1], a={2}) == frozenmethodkey("y", [1], a={2}) == frozenkey([1], a={2})`.
10. A function decorated with `@cachetools.cached(cachetools.LRUCache(maxsize=8), key=cachetools.keys.frozenkey)` and called twice as `f([1, 2, 3], weights={2: 10})` runs its body once.
11. `src/cachetools/keys.pyi` declares `def freeze(obj: Any) -> Any: ...`, `def frozenkey(*args: Any, **kwargs: Any) -> tuple[Hashable, ...]: ...` and `def frozenmethodkey(self: Unused, /, *args: Any, **kwargs: Any) -> tuple[Hashable, ...]: ...`, and its `__all__` matches criterion 1.
12. The `cachetools.keys` section of `docs/index.rst` has `.. autofunction:: freeze`, `.. autofunction:: frozenkey` and `.. autofunction:: frozenmethodkey` entries, in that order, placed directly after the existing `typedmethodkey` entry and before the paragraph that starts "These functions can also be helpful". The `freeze` entry lists the five conversion rules of criteria 2 to 6 and states that containers of different types may freeze to equal values; the `frozenkey` entry includes a `doctest` block that memoizes a function taking a `list` and a `dict` argument.
13. `tests/test_keys.py` has tests for `freeze`, `frozenkey` and `frozenmethodkey` covering criteria 2 to 10, and the whole suite passes with `python -m pytest -q tests`.

## Risks

- Two calls with arguments of different container types can share a cache entry: `f([1, 2])` and `f((1, 2))`, or a `dict` and a `set` of the same `(key, value)` pairs. This is accepted and documented; a caller who needs them distinct writes a key function of their own.
- A `dict` with values that are unequal but freeze to equal values, or a `set` whose items do, produces a smaller `frozenset` than the original container; this is the same accepted collision.
- `freeze` walks the whole argument on every call of the memoized function, so for large containers the key computation can cost more than a cache hit saves.
- The repository's own checks (`tox`: ruff, ruff-format, pyright, the Sphinx build and doctests with warnings as errors) apply to the new code, stubs and documentation; a doctest whose output does not match fails `tox -e doctest` even though the unit tests pass.

## Open questions

None.
