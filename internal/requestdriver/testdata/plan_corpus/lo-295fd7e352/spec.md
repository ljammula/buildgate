# Spec

## Problem

Package `lo` can clamp a value to bounds with `Clamp` in `math.go`, but it has no helper that answers whether a value lies between two bounds. Callers write `value >= start && value < end` by hand and must remember which bound is exclusive and what happens when the bounds arrive in the wrong order. Lodash, which this library follows, offers `inRange` for this.

## Scope

Add one generic helper, `InRange`, to `math.go`. It reports whether a value is inside the half-open interval `[start, end)`: `start` is inclusive, `end` is exclusive, and when `start` is greater than `end` the two bounds are swapped before the check. It works for every `constraints.Ordered` type and uses nothing newer than Go 1.18.

The function is appended at the end of `math.go`, after `modeSmall`, with no change to the import block, so the line numbers that existing `docs/data/*.md` pages record in `sourceRef` do not move.

The scope also includes what the contributing guide requires of a new helper: table-driven tests in `math_test.go`, an `ExampleInRange` function in `lo_example_test.go`, a `README.md` index line and section, a `docs/data/core-inrange.md` page linked both ways with the `Clamp` page, and a line in `docs/static/llms.txt`.

## Non-goals

- Variants: no `InRangeBy`, no inclusive-end variant, no `it`, `mutable` or `parallel` version.
- A one-bound form such as lodash's `inRange(value, end)`.
- A benchmark: the helper is two comparisons.
- A `// Play:` link and a `playUrl` value: the helper is not released, so the playground cannot compile it.
- Changing `Clamp` or any other existing helper, or editing any `sourceRef` of an existing page.

## Affected services and packages

Package `github.com/samber/lo` gains one exported function in `math.go`, with its tests in `math_test.go` and its example in `lo_example_test.go`. Documentation changes are in `README.md`, `docs/data/core-inrange.md`, `docs/data/core-clamp.md` and `docs/static/llms.txt`. No sub-package, dependency or existing signature changes.

## Acceptance criteria

1. `math.go` declares, after every existing declaration in the file, exactly `func InRange[T constraints.Ordered](value, start, end T) bool`, and the import block of `math.go` is unchanged.
2. `InRange` in `math.go` returns `true` when `start <= value < end`: `InRange(3, 1, 5)`, `InRange(1, 1, 5)` and `InRange(4, 1, 5)` are `true`; `InRange(5, 1, 5)`, `InRange(0, 1, 5)` and `InRange(6, 1, 5)` are `false`.
3. `InRange` in `math.go` swaps the bounds when `start > end`, so the smaller bound stays inclusive and the larger stays exclusive: `InRange(3, 5, 1)` and `InRange(1, 5, 1)` are `true`; `InRange(5, 5, 1)` and `InRange(0, 5, 1)` are `false`.
4. `InRange` in `math.go` returns `false` for every value when `start == end`: `InRange(1, 1, 1)` and `InRange(0, 0, 0)` are `false`.
5. `InRange` in `math.go` works for every ordered type, including named ones: `InRange(0.5, 0.0, 1.0)`, `InRange("b", "a", "c")`, `InRange(uint8(200), uint8(100), uint8(255))` and `InRange(-3, -5, -1)` are `true`; `InRange(1.0, 0.0, 1.0)`, `InRange("c", "a", "c")` and `InRange(math.NaN(), 0.0, 1.0)` are `false`; `InRange[celsius]` compiles for `type celsius float64`.
6. The doc comment of `InRange` in `math.go` starts with `// InRange`, states that `start` is inclusive, that `end` is exclusive and that the bounds are swapped when `start` is greater than `end`, and has no `// Play:` line.
7. `math_test.go` has a function `TestInRange` that calls `t.Parallel()`, uses `assert.New(t)` and covers criteria 2 to 5 with at least one integer, one float and one string case.
8. `lo_example_test.go` has, immediately after `ExampleMeanByErr`, a function `ExampleInRange` that prints `InRange(3, 1, 5)`, `InRange(5, 1, 5)` and `InRange(3, 5, 1)` one per line, with the output comment `// Output:` followed by `// true`, `// false`, `// true`.
9. `README.md` lists `- [InRange](#inrange)` on the line after `- [Mode](#mode)` under "Supported math helpers", and has a `### InRange` section between the end of the `### Mode` section and `### RandomString` with one sentence describing the half-open interval and the swap, and a Go code block showing `lo.InRange(3, 1, 5)` as `true`, `lo.InRange(5, 1, 5)` as `false` and `lo.InRange(3, 5, 1)` as `true`. The section has no `[[play](...)]` line.
10. `docs/data/core-inrange.md` exists with front matter `name: InRange`, `slug: inrange`, `sourceRef: math.go#L<line of the func InRange declaration>`, `category: core`, `subCategory: math`, an empty `playUrl:`, `variantHelpers` holding only `core#math#inrange`, `similarHelpers` holding only `core#math#clamp`, `position: 110` and `signatures` holding only `"func InRange[T constraints.Ordered](value T, start T, end T) bool"`, followed by a one-sentence description and a Go code block with at least the three calls of criterion 9.
11. `docs/data/core-clamp.md` has `  - core#math#inrange` added as the last entry of its `similarHelpers` list and no other change.
12. `docs/static/llms.txt` has the line `- InRange: Check if value is within a half-open interval [start, end)` immediately after `- Mode: Find most frequently occurring value` in the core "### Math" list.
13. `go test ./...` and `gofmt -l .` succeed (the latter prints nothing), and `node docs/scripts/check-similar-exists.js`, `node docs/scripts/check-similar-keys-exist-in-directory.js` and `node docs/scripts/check-cross-references.js` each exit 0.

## Risks

- `README.md`, `docs/static/llms.txt` and `lo_example_test.go` are edited by other helper additions in flight. The insertion points above are specific to the math helpers so that the changes merge cleanly; inserting elsewhere invites conflicts.
- Swapping the bounds follows lodash, but a caller who expects an inverted interval to be empty gets `true` instead. The doc comment, README and docs page must state the swap.
- `NaN` compares false with everything, so `InRange` is `false` for a `NaN` value and behaves as an ordinary comparison chain for `NaN` bounds. No special handling is added.
- The docs site's `sourceRef` values are line numbers. Appending at the end of `math.go` keeps the existing ones valid; placing the function higher or adding an import would silently make every later `sourceRef` in the file stale.

## Open questions

None.
