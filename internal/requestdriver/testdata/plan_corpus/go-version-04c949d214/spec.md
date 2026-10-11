# Spec

## Problem

A `Constraints` value in `constraint.go` is a conjunction: `NewConstraint(">= 1.0, < 2.0")` matches a version that satisfies every comma-separated constraint. There is no way to say "this range or that one". A caller that supports two release lines, for example `>= 1.0, < 2.0` or `>= 3.0`, has to keep several `Constraints` values, loop over them by hand, and invent its own string form to store them in configuration. npm and Composer both write this as alternatives separated by `||`.

## Scope

Add a `ConstraintSet` type: an ordered list of alternative `Constraints`, satisfied by a version that satisfies at least one alternative. The work has three parts that build on each other.

Part 1, the core type, in a new file `constraint_set.go`: the type `ConstraintSet`, the parser `NewConstraintSet` for strings such as `">= 1.0, < 2.0 || >= 3.0"` (alternatives separated by `||`, each parsed by the existing `NewConstraint`), the helper `MustConstraintSet`, and the methods `Check` and `String`.

Part 2, integration with the rest of the package, in the same file: `Equals` to compare two sets, `Filter` to select from a `Collection` the versions that satisfy the set, and `MarshalText` / `UnmarshalText` so a set can be a field in JSON and other text-encoded configuration.

Part 3, documentation: runnable examples in a new file `constraint_set_example_test.go`, and a new `#### Constraint Sets` section in `README.md`.

Tests for parts 1 and 2 go in a new file `constraint_set_test.go`.

## Non-goals

- Changing `NewConstraint`, `Constraints`, `Constraint` or any existing operator; `NewConstraint("1.0 || 2.0")` stays an error.
- Parentheses, nesting, negation of an alternative, or any separator other than `||`.
- Simplifying, sorting or deduplicating alternatives, or deciding whether two sets are logically equivalent.
- Methods on `Collection`; `ConstraintSet.Filter` takes a `Collection` but `version_collection.go` is not edited.
- SQL `Scan` / `Value` support for `ConstraintSet`.
- Any change to `version.go`, `constraint.go` or `version_collection.go`, and any edit to `CHANGELOG.md`.

## Affected services and packages

The `version` package (module `github.com/hashicorp/go-version`) gains the exported type `ConstraintSet`, the functions `NewConstraintSet` and `MustConstraintSet` and six methods, all in the new file `constraint_set.go`, with tests in the new file `constraint_set_test.go` and examples in the new file `constraint_set_example_test.go`. `README.md` gains one section. No existing Go file changes and no dependency is added.

## Acceptance criteria

1. `constraint_set.go` declares `type ConstraintSet []Constraints` with a doc comment saying a version satisfies the set when it satisfies at least one alternative. The three new files are in package `version`, and each starts with the two lines `// Copyright IBM Corp. 2014, 2026` and `// SPDX-License-Identifier: MPL-2.0` followed by a blank line.
2. `func NewConstraintSet(s string) (ConstraintSet, error)` in `constraint_set.go` splits `s` on every occurrence of `||`, trims each piece with `strings.TrimSpace`, parses each trimmed piece with `NewConstraint`, and returns the results in order with a nil error. `NewConstraintSet(">= 1.0")` has 1 alternative of 1 constraint; `NewConstraintSet(">= 1.0, < 2.0")` has 1 alternative of 2 constraints; `NewConstraintSet(">= 1.0, < 2.0 || >= 3.0")` has 2 alternatives of 2 and 1 constraints; `NewConstraintSet("1.0||2.0||~> 3.1")` has 3 alternatives of 1 constraint each.
3. `NewConstraintSet` in `constraint_set.go` returns a nil `ConstraintSet` and an error built as `fmt.Errorf("constraint set %q: alternative %d is empty", s, n)` when a trimmed piece is the empty string, where `s` is the whole input and `n` is the one-based position of the first such piece. The error text is exactly `constraint set "": alternative 1 is empty` for `""`, `constraint set "   ": alternative 1 is empty` for three spaces, `constraint set "|| >= 1.0": alternative 1 is empty` for `"|| >= 1.0"`, `constraint set ">= 1.0 ||": alternative 2 is empty` for `">= 1.0 ||"`, and `constraint set ">= 1.0 || || 2.0": alternative 2 is empty` for `">= 1.0 || || 2.0"`.
4. `NewConstraintSet` in `constraint_set.go` returns a nil `ConstraintSet` and an error built as `fmt.Errorf("constraint set %q: alternative %d: %w", s, n, err)` when `NewConstraint` rejects a trimmed piece, where `n` is the one-based position of the first piece that is empty or rejected (pieces are examined left to right) and `err` is the error from `NewConstraint`. For `">= 1.0 || foo || bar"` the text is exactly `constraint set ">= 1.0 || foo || bar": alternative 2: malformed constraint: foo` and `errors.Unwrap` of it has the text `malformed constraint: foo`. A single `|` is not a separator: for `"1.0 | 2.0"` the text is exactly `constraint set "1.0 | 2.0": alternative 1: malformed constraint: 1.0 | 2.0`.
5. `func MustConstraintSet(s ConstraintSet, err error) ConstraintSet` in `constraint_set.go` returns `s` when `err` is nil and panics with `err` otherwise, like the existing `MustConstraints`.
6. `func (s ConstraintSet) Check(v *Version) bool` in `constraint_set.go` returns `true` when `Constraints.Check(v)` is `true` for at least one alternative, and `false` otherwise, including for an empty or nil set. The set `>= 1.0, < 2.0 || >= 3.0` matches `1.5`, `3.0` and `4.2.1` and does not match `0.9`, `2.0`, `2.5` or `1.5-beta`; `>= 1.0, < 2.0 || >= 3.0-alpha` matches `3.0-beta`; `1.0 || 2.0 || 3.0` matches `2.0.0` and not `2.0.1`; `~> 1.2 || ~> 2.4` matches `2.9` and not `2.3`.
7. `func (s ConstraintSet) String() string` in `constraint_set.go` returns `Constraints.String()` of each alternative joined with exactly `" || "` (space, two pipes, space), and `""` for an empty or nil set. Because the pieces were trimmed and `Constraints.String()` keeps each constraint as written, `NewConstraintSet(">= 1.0, < 2.0 || >= 3.0").String()` is `">= 1.0, < 2.0 || >= 3.0"`, `NewConstraintSet("1.0||2.0||~> 3.1").String()` is `"1.0 || 2.0 || ~> 3.1"`, and `NewConstraintSet("  = 1.0   ||\t>2.0,<3  ").String()` is `"= 1.0 || >2.0,<3"`.
8. `func (s ConstraintSet) Equals(o ConstraintSet) bool` in `constraint_set.go` returns `true` exactly when both sets have the same number of alternatives and, for every index `i`, `s[i].Equals(o[i])` (the existing `Constraints.Equals`) is `true`. The order of alternatives matters; the order of constraints inside an alternative does not. `>= 1.0, < 2.0 || >= 3.0` equals `>=1.0,<2.0||>=3.0`; `< 2.0, >= 1.0 || = 3.0` equals `>= 1.0, < 2.0 || 3.0.0`; `>= 1.0 || >= 3.0` does not equal `>= 3.0 || >= 1.0`, `>= 1.0` or `>= 1.0 || > 3.0`; `>= 1.0, < 2.0` does not equal `>= 1.0 || < 2.0`; an empty set equals a nil set.
9. `func (s ConstraintSet) Filter(versions Collection) Collection` in `constraint_set.go` returns a new `Collection` holding, in the order of `versions`, the same `*Version` pointers for which `s.Check` is `true`, without changing `versions`. For `versions` parsed from `"3.1", "0.9", "1.5", "2.0", "1.5-beta", "1.0", "4.0"` and the set `>= 1.0, < 2.0 || >= 3.0`, the result holds the elements at indexes 0, 2, 5 and 6 in that order. It returns a non-nil `Collection` of length 0 when nothing matches, when `versions` is empty or nil, and when the set is empty.
10. `func (s ConstraintSet) MarshalText() ([]byte, error)` in `constraint_set.go` has a value receiver and returns `[]byte(s.String())` and a nil error, so `ConstraintSet` implements `encoding.TextMarshaler`. `json.Marshal` of a struct with a `ConstraintSet` field holding `NewConstraintSet(">= 1.0,< 2.0||>= 3.0")` encodes the field as a JSON string that decodes to `>= 1.0,< 2.0 || >= 3.0`.
11. `func (s *ConstraintSet) UnmarshalText(b []byte) error` in `constraint_set.go` has a pointer receiver, parses `string(b)` with `NewConstraintSet`, and on success replaces `*s` with the result and returns nil, so `*ConstraintSet` implements `encoding.TextUnmarshaler`. On failure it returns the error from `NewConstraintSet` unchanged and leaves `*s` as it was: after a successful `UnmarshalText([]byte("~> 1.2 || 2.0"))`, a call with `[]byte("~> 1.2 || ")` returns an error with the text `constraint set "~> 1.2 || ": alternative 2 is empty` and `String()` is still `"~> 1.2 || 2.0"`. `json.Unmarshal` of the output of criterion 10 into the same struct type gives a set that `Equals` the original, and `json.Unmarshal` of `{"allowed":"1.0 || nope"}` into a struct whose `ConstraintSet` field has the tag `json:"allowed"` returns a non-nil error.
12. Every exported identifier in `constraint_set.go` has a doc comment; the one on `NewConstraintSet` gives an example input and states that `||` separates alternatives and that an empty alternative is an error.
13. `constraint_set_test.go` contains table-driven tests in package `version`, using only the standard library, that cover criteria 2 to 11, including every error text of criteria 3 and 4, the empty and nil set, and the JSON round trip.
14. `constraint_set_example_test.go` is in package `version` and contains two examples that `go test` runs. `ExampleNewConstraintSet` parses `">= 1.0, < 2.0 || >= 3.0"` and, for each of `1.5`, `2.5` and `3.1` in that order, prints the string as written, a space and the result of `Check`; its `// Output:` block is exactly the three lines `1.5 true`, `2.5 false` and `3.1 true`. `ExampleConstraintSet_Filter` builds a `Collection` from `"0.9", "1.5", "2.5", "3.1"`, filters it with the same set and prints each remaining version's `String()` on its own line; its `// Output:` block is exactly the two lines `1.5.0` and `3.1.0`.
15. `README.md` has a new section headed `#### Constraint Sets`, placed immediately before the existing `#### Version Sorting` heading (after the code block of `#### Version Constraints`). It states that `||` separates alternatives and that a version satisfies the set when it satisfies any one of them, and has one `go` code block that calls `version.NewConstraintSet(">= 1.0, < 2.0 || >= 3.0")` and `Check`. It also states in one sentence that a set can be marshalled to and from text and JSON. No other part of `README.md` changes.
16. `go.mod` is unchanged (the code must compile under its `go 1.16` directive: no generics, no `any`), `go test ./...` passes with every existing test unchanged, `go vet ./...` reports nothing and `gofmt -l .` prints nothing.

## Risks

- `Constraints.String()` joins constraints with `,` and keeps each constraint's original text, including its leading whitespace. `ConstraintSet.String()` therefore reproduces the spacing inside an alternative but normalises the spacing around `||`; criterion 7 fixes the expected strings, and the round trip `NewConstraintSet(s.String())` must give a set that `Equals` `s`.
- `encoding/json` escapes `<` and `>` inside strings as `<` and `>`. Tests must compare the decoded string, not the raw JSON bytes.
- `UnmarshalText` needs a pointer receiver while `MarshalText`, `Check` and the others need a value receiver so that a `ConstraintSet` struct field (not a pointer) is marshalled by `encoding/json`. Mixing these up compiles but silently encodes the field as a JSON array.
- An empty set matches nothing, while an empty `Constraints` matches everything. The doc comment on `Check` must say so, since `NewConstraintSet` never returns an empty set but a caller can build one.
- Three other changes are being made to this repository at the same time: one adds methods to `Collection` in `version_collection.go`, one adds an operator in `constraint.go`, and each adds to `README.md`. This work must stay in its three new files and in the README position of criterion 15 to avoid merge conflicts.

## Open questions

None.
