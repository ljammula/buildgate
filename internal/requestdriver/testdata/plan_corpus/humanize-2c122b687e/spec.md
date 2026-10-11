# Spec

## Problem

`natural_list` in `src/humanize/lists.py` always hardcodes `and` as the final conjunction for lists with two or more items. This prevents callers from rendering alternatives such as `one, two or three`, while existing callers need the current `and` output to remain unchanged.

## Scope

Extend `natural_list` in `src/humanize/lists.py` with a `conjunction: str = "and"` argument that callers can provide by keyword. For two or more items, the supplied string is used as the final conjunction while the existing comma placement and spacing remain unchanged. Empty iterables and single-item iterables retain their current output regardless of the argument, and calls that omit the argument retain the current default behavior.

The scope also includes documenting the new argument and its default in the `natural_list` docstring and adding regression coverage to the existing `natural_list` tests for the requested two-item, three-item, single-item, and empty-iterable cases.

## Non-goals

- Changing the comma or spacing rules used by `natural_list`.
- Changing how `natural_list` accepts iterables or converts their items to strings.
- Adding conjunction validation, normalization, localization, or translation-catalog changes.
- Changing any other humanization helper, public API, or documentation outside the `natural_list` docstring.

## Affected services and packages

The `humanize` Python package is affected through its public `natural_list` list-formatting API and the implementation and tests associated with it. No external service, persistent data, or runtime dependency is affected.

## Acceptance criteria

1. `natural_list` in `src/humanize/lists.py` exposes a `conjunction` argument annotated as `str` with default value `"and"`, and `humanize.natural_list(["one", "two", "three"])` returns exactly `"one, two and three"`.
2. `natural_list` in `src/humanize/lists.py` called as `humanize.natural_list(["one", "two", "three"], conjunction="or")` returns exactly `"one, two or three"`.
3. `natural_list` in `src/humanize/lists.py` called as `humanize.natural_list(["one", "two"], conjunction="or")` returns exactly `"one or two"`.
4. `natural_list` in `src/humanize/lists.py` returns exactly `"one"` for `humanize.natural_list(["one"], conjunction="or")` and exactly `""` for `humanize.natural_list([], conjunction="or")`, with the same results for other supplied conjunction strings.
5. For two or more items, `natural_list` in `src/humanize/lists.py` replaces only the final `and` with the caller-supplied conjunction, so a supplied string such as `"nor"` produces `"one, two nor three"` for three items without changing the earlier comma-separated items.
6. The `natural_list` docstring in `src/humanize/lists.py` documents `conjunction` as a string argument, identifies `"and"` as its default, and explains that it controls the conjunction used for multi-item output.
7. Running the existing `natural_list` test cases in `tests/test_lists.py` reports the exact expected results for the default output, explicit `conjunction="or"` with two and three items, and non-default conjunctions with single-item and empty iterables.

## Risks

- Backward compatibility depends on preserving the default value `"and"` and the current separator formatting. If either changes, existing callers that compare humanized strings can regress even when they do not pass `conjunction`.
- The supplied conjunction is assumed to be caller-provided display text. Empty, whitespace-only, or otherwise unsuitable values may produce awkward output, and non-string values may violate the annotated contract; adding validation or normalization would create behavior not defined by this request.
- The existing tests rely on exact output strings. If a future localization or punctuation requirement is introduced without expanding this contract, those requirements may conflict with the intentionally unchanged English formatting.

## Open questions

None.
