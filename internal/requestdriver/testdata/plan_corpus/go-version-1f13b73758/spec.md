# Spec

## Problem

`Collection` in `version_collection.go` only implements `sort.Interface`. The common jobs around a list of versions are left to every caller: parsing a `[]string` into versions (the README's sorting example does it with a hand-written loop that discards the error), getting a sorted copy without reordering the input, keeping the versions that satisfy a `Constraints` value, and picking the highest version that satisfies one, which is what a dependency resolver or an "is there an update" check needs.

## Scope

Extend `version_collection.go` with one constructor and six methods on `Collection`:

- `NewCollection` parses a slice of strings with `NewVersion`, passing its options through, and reports the index of the first string that fails.
- `Strings` returns the canonical string of each version.
- `Sorted` returns an ascending, stably sorted copy.
- `Filter` returns the versions that satisfy a `Constraints` value, in their original order.
- `Min` and `Max` return the lowest and highest version.
- `Latest` returns the highest version that satisfies a `Constraints` value.

No method changes its receiver, and every method that returns a `Collection` or a slice returns a non-nil value, also for an empty or nil receiver. The scope also includes doc comments, tests in `version_collection_test.go`, and an addition to the `#### Version Sorting` section of `README.md`.

## Non-goals

- Changing `Collection`'s `Len`, `Less` or `Swap`, or the behaviour of `sort.Sort(version.Collection(...))`.
- Supporting a `Collection` that contains nil elements; behaviour for one is undefined.
- Removing duplicates, set operations between collections, or a descending sort.
- Changing `Constraints.Check` or how constraints treat prerelease versions; `Filter` and `Latest` use `Constraints.Check` as it is.
- Any change to `version.go` or `constraint.go`, and any edit to `CHANGELOG.md`.

## Affected services and packages

The `version` package (module `github.com/hashicorp/go-version`) gains the exported function `NewCollection` and six exported methods on `Collection`, all in `version_collection.go`, with tests in `version_collection_test.go`. `README.md` gains text in its `#### Version Sorting` section. No dependency is added and no other file changes.

## Acceptance criteria

1. `version_collection.go` declares exactly these exported identifiers, each with a doc comment: `func NewCollection(raw []string, opts ...Option) (Collection, error)`, `func (v Collection) Strings() []string`, `func (v Collection) Sorted() Collection`, `func (v Collection) Filter(cs Constraints) Collection`, `func (v Collection) Min() *Version`, `func (v Collection) Max() *Version` and `func (v Collection) Latest(cs Constraints) *Version`. The existing `Len`, `Less` and `Swap` methods are unchanged.
2. `NewCollection` in `version_collection.go` returns one `*Version` per input string, in input order, each the result of `NewVersion(raw[i], opts...)`, and a nil error: `NewCollection([]string{"1.1", "v0.7.1", "1.4-beta", "2+meta"})` returns a collection of length 4 whose `Strings()` is exactly `[]string{"1.1.0", "0.7.1", "1.4.0-beta", "2.0.0+meta"}` and whose second element's `Original()` is `"v0.7.1"`. `NewCollection([]string{"rel-1.2.3", "rel-v2.0"}, WithPrefix("rel-"))` returns versions with `Strings()` `[]string{"1.2.3", "2.0.0"}` and `Prefix()` `"rel-"`.
3. `NewCollection` in `version_collection.go` called with a nil or empty slice returns a non-nil `Collection` of length 0 and a nil error.
4. `NewCollection` in `version_collection.go` stops at the first string `NewVersion` rejects and returns a nil `Collection` and an error built as `fmt.Errorf("index %d: %w", i, err)`, where `i` is the zero-based index of that string and `err` is the error from `NewVersion`. `NewCollection([]string{"1.0", "foo", "bar"})` returns the error text exactly `index 1: malformed version: foo`, and `errors.Unwrap` of it has the text `malformed version: foo`. `NewCollection([]string{"rel-1.2.3", "1.0"}, WithPrefix("rel-"))` returns the error text exactly `index 1: version "1.0" does not have prefix "rel-"`.
5. `Collection.Strings` in `version_collection.go` returns a slice of the same length as the receiver holding `String()` of each element in order; for an empty or nil receiver it returns a non-nil slice of length 0.
6. `Collection.Sorted` in `version_collection.go` returns a new `Collection` holding the same `*Version` pointers as the receiver in ascending order as defined by `Version.Compare`, with elements that compare equal kept in their original relative order (a stable sort). For a collection parsed from `"1.1", "0.7.1", "1.4-beta", "1.4", "2", "1.4+a", "1.4+b"`, `Sorted().Strings()` is exactly `[]string{"0.7.1", "1.1.0", "1.4.0-beta", "1.4.0", "1.4.0+a", "1.4.0+b", "2.0.0"}`. The receiver's element order is unchanged afterwards. For an empty or nil receiver it returns a non-nil `Collection` of length 0.
7. `Collection.Filter` in `version_collection.go` returns a new `Collection` holding, in the receiver's order, the same `*Version` pointers for which `cs.Check` returns `true`, and leaves the receiver unchanged. For a collection parsed from `"2.1", "1.1", "0.7.1", "1.4-beta", "1.4", "2"`: with `NewConstraint(">= 1.0, < 2.0")` the result's `Strings()` is exactly `[]string{"1.1.0", "1.4.0"}` (the prerelease `1.4.0-beta` is left out because `Constraints.Check` rejects it); with `NewConstraint(">= 1.4-alpha")` it is exactly `[]string{"2.1.0", "1.4.0-beta", "1.4.0", "2.0.0"}`.
8. `Collection.Filter` in `version_collection.go` returns a non-nil `Collection` of length 0 when nothing matches or when the receiver is empty or nil, and returns every element when `cs` is nil or empty.
9. `Collection.Max` in `version_collection.go` returns the element that is greater than or equal to every other element by `Version.Compare`, and `Collection.Min` the element that is less than or equal to every other; when several elements compare equal for that place, each returns the one with the lowest index. Both return the receiver's own `*Version` pointer, and both return `nil` for an empty or nil receiver. For a collection `c` parsed from `"1.1", "2.0+first", "0.7.1+first", "1.4-beta", "2", "0.7.1", "2.0.0+third"`, `c.Max()` is `c[1]` and `c.Min()` is `c[2]`. For one parsed from `"1.0.0-beta", "1.0.0-rc1", "1.0.0-alpha"`, `Max()` is the element `1.0.0-rc1` and `Min()` is the element `1.0.0-alpha`.
10. `Collection.Latest` in `version_collection.go` returns exactly what `v.Filter(cs).Max()` returns. For the seven-element collection `c` of criterion 9: `c.Latest` with `NewConstraint("< 2.0")` is `c[0]` (`1.1.0`; the prerelease `1.4.0-beta` does not satisfy the constraint); with `NewConstraint("~> 1.0")` it is `c[0]`; with `NewConstraint("< 1.0")` it is `c[2]`; with a nil `Constraints` it is `c[1]`; with `NewConstraint("> 9")` it is `nil`. `Collection(nil).Latest(nil)` is `nil`.
11. `version_collection_test.go` contains tests in package `version`, using only the standard `testing` package, for every function and method of criterion 1, covering criteria 2 to 10 including the empty-receiver, nil-receiver, error and equal-versions cases. The existing `TestCollection` still passes unchanged, and `go test ./...` passes.
12. The first line of both `version_collection.go` and `version_collection_test.go` is exactly `// Copyright IBM Corp. 2014, 2026` (the end year changes from `2025` because the files are edited).
13. The `#### Version Sorting` section of `README.md` gains, after its existing code block and immediately before the `## Issues and Contributing` heading, a short paragraph and one `go` code block that show `version.NewCollection` on a `[]string`, then `Sorted()`, `Filter(constraints)` and `Latest(constraints)` on the result, and that state that `Latest` returns `nil` when no version satisfies the constraints. The existing text and code block of the section, and every other part of `README.md`, are unchanged.
14. `go.mod` is unchanged (the code must compile under its `go 1.16` directive: no generics, no `slices` or `cmp` package), `go vet ./...` reports nothing and `gofmt -l .` prints nothing.

## Risks

- `Version.Compare` ignores build metadata, so versions such as `2.0.0+first` and `2.0.0` compare equal. Without the lowest-index rule of criterion 9 and the stable sort of criterion 6, the result would depend on the implementation; tests must cover equal versions.
- `Filter` and `Latest` inherit the prerelease rules of `Constraints.Check`: a constraint with no prerelease never matches a prerelease version. The README text and doc comments should say that these methods use `Constraints.Check`, so a missing prerelease is not read as a bug.
- Returning the receiver's own slice from `Sorted` or `Filter`, or sorting in place, would change the caller's data. The tests must check that the receiver's order is unchanged.
- Three other changes are being made to this repository at the same time, each adding to `README.md`. Keeping the addition exactly where criterion 13 places it avoids a merge conflict.

## Open questions

None.
