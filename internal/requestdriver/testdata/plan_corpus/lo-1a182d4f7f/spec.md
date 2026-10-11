# Spec

## Problem

`slice.go` pairs every "from the start" helper with a "from the end" one for dropping (`Drop` and `DropRight`, `DropWhile` and `DropRightWhile`), but taking is one-sided: `Take` and `TakeWhile` exist and nothing takes from the end. Getting the last three elements of a slice, or the trailing run that satisfies a predicate, means computing an offset for `Drop` or reversing twice with `Reverse`.

## Scope

Add two helpers to `slice.go`: `TakeRight`, which returns the last `n` elements of a slice, and `TakeRightWhile`, which returns the longest run of trailing elements for which a predicate holds. Both keep the elements in their original order, preserve a named slice type through the `Slice ~[]T` constraint, never modify their input and always return a newly allocated, non-nil slice that shares no memory with the input. `TakeRight` follows `Take` on a negative count: it panics. `TakeRightWhile` tests elements from the last one backwards and stops at the first that fails.

The two functions are appended at the end of `slice.go`, after `TrimSuffix`, with no change to the import block, so the line numbers existing `docs/data/*.md` pages record in `sourceRef` do not move.

The scope also includes what the contributing and benchmark guides require of new helpers: tests in `slice_test.go` that prove immutability, `Example` functions in `lo_example_test.go`, `README.md` index lines and sections, a `docs/data/` page each with links added to the pages of the four neighbouring helpers, lines in `docs/static/llms.txt`, and benchmarks in `benchmark/core_slice_bench_test.go`.

## Non-goals

- Iterator variants in `it/` (a sequence has no cheap end), and `mutable` or `parallel` variants.
- An index-aware `TakeRightWhileI` or an `Err` variant.
- Changing `Take`, `TakeWhile`, `DropRight`, `DropRightWhile` or any other existing helper, including the form of the empty slice each returns.
- `// Play:` links and `playUrl` values: the helpers are not released, so the playground cannot compile them.
- Editing any `sourceRef` of an existing page.

## Affected services and packages

Package `github.com/samber/lo` gains two exported functions in `slice.go`, with tests in `slice_test.go` and examples in `lo_example_test.go`. Package `benchmark` gains two benchmarks in `benchmark/core_slice_bench_test.go`. Documentation changes are in `README.md`, the new pages `docs/data/core-takeright.md` and `docs/data/core-takerightwhile.md`, the existing pages `docs/data/core-take.md`, `docs/data/core-takewhile.md`, `docs/data/core-dropright.md` and `docs/data/core-droprightwhile.md`, and `docs/static/llms.txt`. No dependency or existing signature changes.

## Acceptance criteria

1. `slice.go` declares, after every existing declaration in the file, exactly `func TakeRight[T any, Slice ~[]T](collection Slice, n int) Slice` and `func TakeRightWhile[T any, Slice ~[]T](collection Slice, predicate func(item T) bool) Slice`, and the import block of `slice.go` is unchanged.
2. `TakeRight` in `slice.go` returns the last `n` elements in their original order: `TakeRight([]int{0, 1, 2, 3, 4, 5}, 3)` is `[]int{3, 4, 5}` and `TakeRight([]int{0, 1, 2, 3, 4, 5}, 1)` is `[]int{5}`.
3. `TakeRight` in `slice.go` returns a copy of the whole slice when `n` is greater than or equal to its length: `TakeRight([]int{0, 1, 2}, 3)` and `TakeRight([]int{0, 1, 2}, 5)` are both `[]int{0, 1, 2}`.
4. `TakeRight` in `slice.go` returns a non-nil slice of length 0 when `n` is 0 or the collection is empty or nil: `TakeRight([]int{0, 1, 2}, 0)`, `TakeRight([]int{}, 2)`, `TakeRight([]int(nil), 2)` and `TakeRight([]int(nil), 0)`.
5. `TakeRight` in `slice.go` panics with exactly the string value `"lo.TakeRight: n must not be negative"` when `n` is negative, whatever the collection.
6. `TakeRightWhile` in `slice.go` returns, in their original order, the trailing elements for which `predicate` returns `true`, ending at the first element from the end for which it returns `false`: with `item > 3`, `[]int{0, 1, 2, 3, 4, 5}` gives `[]int{4, 5}`; with `item%2 == 0`, `[]int{6, 1, 2, 4}` gives `[]int{2, 4}`; with a predicate that is always `true`, `[]int{0, 1, 2}` gives `[]int{0, 1, 2}`.
7. `TakeRightWhile` in `slice.go` returns a non-nil slice of length 0 when the last element fails the predicate or the collection is empty or nil: `[]int{4, 5, 1}` with `item > 3`, `[]int{0, 1, 2}` with a predicate that is always `false`, and `[]int{}` and `[]int(nil)` with a predicate that is always `true`.
8. `TakeRightWhile` in `slice.go` calls `predicate` once per element starting from the last element and moving towards the first, and never again after the first `false`: for `[]int{0, 1, 2, 3}` with `item > 1` the predicate receives `3`, `2`, `1` in that order and nothing else.
9. `TakeRight` and `TakeRightWhile` in `slice.go` return the caller's slice type: for `type myStrings []string`, `TakeRight(myStrings{"a", "b", "c"}, 2)` is `myStrings{"b", "c"}` and `TakeRightWhile(myStrings{"", "foo", "bar"}, func(item string) bool { return item != "" })` is `myStrings{"foo", "bar"}`.
10. The result of `TakeRight` and of `TakeRightWhile` in `slice.go` never shares memory with the input, including when the whole slice is returned: after `out := TakeRight(in, n)` for `n` of 2, 4 and 10 with `in := []int{0, 1, 2, 3}`, and after `out := TakeRightWhile(in, alwaysTrue)`, assigning `out[0] = 42` leaves `in` equal to `[]int{0, 1, 2, 3}`.
11. The doc comment of `TakeRight` in `slice.go` starts with `// TakeRight takes the last n elements from a slice` and states that it panics when `n` is negative; the doc comment of `TakeRightWhile` starts with `// TakeRightWhile takes elements from the end of a slice while the predicate returns true`. Neither has a `// Play:` line.
12. `slice_test.go` has functions `TestTakeRight` and `TestTakeRightWhile`, each calling `t.Parallel()` and using `assert.New(t)`, which between them cover criteria 2 to 10, including the panic value with `PanicsWithValue`, the named slice type with `IsType`, and immutability of the input.
13. `lo_example_test.go` has, immediately after `ExampleTakeFilter` and in this order, `ExampleTakeRight`, which prints `TakeRight([]int{0, 1, 2, 3, 4, 5}, 3)` with `%v` and has the comment `// Output: [3 4 5]`, and `ExampleTakeRightWhile`, which prints `TakeRightWhile` of the same slice with the predicate `val > 3` and has the comment `// Output: [4 5]`.
14. `benchmark/core_slice_bench_test.go` has, immediately after `BenchmarkTakeWhile` and in this order, `BenchmarkTakeRight` and `BenchmarkTakeRightWhile`. Each loops over `lengths`, builds its input with `genSliceInt(n)`, and runs a sub-benchmark named `fmt.Sprintf("ints_%d", n)`; the first calls `lo.TakeRight(ints, n/4)` and the second `lo.TakeRightWhile(ints, func(v int) bool { return v < 50000 })`.
15. `README.md` lists `- [TakeRight](#takeright)` and `- [TakeRightWhile](#takerightwhile)`, in this order, on the lines after `- [TakeFilter](#takefilter)` under "Supported helpers for slices", and has sections `### TakeRight` and `### TakeRightWhile`, in this order, between the end of the `### TakeFilter` section and `### Drop`. The first shows `lo.TakeRight([]int{0, 1, 2, 3, 4, 5}, 3)` as `[]int{3, 4, 5}` and `lo.TakeRight([]int{0, 1, 2}, 5)` as `[]int{0, 1, 2}`; the second shows `lo.TakeRightWhile([]int{0, 1, 2, 3, 4, 5}, ...)` with the predicate `val > 3` as `[]int{4, 5}`. Neither section has a `[[play](...)]` line.
16. `docs/data/core-takeright.md` and `docs/data/core-takerightwhile.md` exist. Each has front matter with `name` (`TakeRight`, `TakeRightWhile`), `slug` (`takeright`, `takerightwhile`), `sourceRef: slice.go#L<line of that func declaration>`, `category: core`, `subCategory: slice`, an empty `playUrl:`, `variantHelpers` holding only its own key (`core#slice#takeright`, `core#slice#takerightwhile`), `similarHelpers` holding `core#slice#take`, `core#slice#takewhile`, the key of the other new helper, `core#slice#dropright` and `core#slice#droprightwhile` in this order, `position` (`176`, `196`), and `signatures` holding only its own signature exactly as in criterion 1 inside double quotes; then a one- or two-sentence description and a Go code block with the calls of criterion 15.
17. Each of `docs/data/core-take.md`, `docs/data/core-takewhile.md`, `docs/data/core-dropright.md` and `docs/data/core-droprightwhile.md` has `  - core#slice#takeright` and `  - core#slice#takerightwhile` added, in this order, as the last entries of its `similarHelpers` list and no other change.
18. `docs/static/llms.txt` has the lines `- TakeRight: Get last n elements from slice` and `- TakeRightWhile: Get elements from end while predicate is true`, in this order, immediately after `- TakeFilter / TakeFilterI: Filter and take first n matching elements (efficient)` in the core "### Slice" list.
19. `go test ./...` and `gofmt -l .` succeed (the latter prints nothing), `go test ./benchmark/ -run '^$' -bench 'BenchmarkTakeRight' -benchtime=10x` passes, and `node docs/scripts/check-similar-exists.js`, `node docs/scripts/check-similar-keys-exist-in-directory.js` and `node docs/scripts/check-cross-references.js` each exit 0.

## Risks

- `README.md`, `docs/static/llms.txt` and `lo_example_test.go` are edited by other helper additions in flight. The insertion points above are specific to the slice helpers so that the changes merge cleanly; inserting elsewhere invites conflicts.
- Returning a sub-slice (`collection[len(collection)-n:]`) passes every value test and fails criterion 10; callers of `lo` rely on results they can modify freely.
- The existing drop helpers disagree on how they build an empty result. Criteria 4 and 7 fix it for the new helpers as non-nil with length 0; a `nil` result would fail them.
- The docs site's `sourceRef` values are line numbers. Appending at the end of `slice.go` keeps the existing ones valid, at the cost of the two helpers not sitting beside `Take` in the source; moving them up would silently make every later `sourceRef` in the file stale.
- The predicate order in criterion 8 is observable by callers whose predicate has side effects; scanning from the front and keeping the last run would break it.

## Open questions

None.
