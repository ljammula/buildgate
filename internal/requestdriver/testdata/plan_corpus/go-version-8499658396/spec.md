# Spec

## Problem

`NewConstraint` in `constraint.go` understands `=`, `!=`, `>`, `<`, `>=`, `<=` and the pessimistic operator `~>`, but not the caret operator `^` that npm, Cargo and Composer use for "any release compatible with this one". A constraint string taken from such an ecosystem, for example `^1.2.3`, fails with `malformed constraint: ^1.2.3`, and `~>` is not a substitute: `~> 1.2.3` stops at `1.3.0`, where `^1.2.3` allows everything below `2.0.0`.

## Scope

Add the caret operator to constraint parsing and checking in `constraint.go`. `^C` matches a version that is not lower than `C` and that has the same leading segments as `C` up to and including a pivot segment. The pivot is found among the segments the constraint wrote, looking only at the first three (major, minor, patch): it is the first of them that is not zero, or, when all of them are zero, the last of them. So `^1.2.3` means `>= 1.2.3, < 2.0.0`, `^0.2.3` means `>= 0.2.3, < 0.3.0`, `^0.0.3` means `>= 0.0.3, < 0.0.4`, `^0.0` means `>= 0.0.0, < 0.1.0` and `^0` means `>= 0.0.0, < 1.0.0`. Prerelease versions are treated as the `>=` operator treats them.

The scope also includes tests in `constraint_test.go`, the doc comment of `NewConstraint`, and a table of operators in the `#### Version Constraints` section of `README.md`.

## Non-goals

- Changing the behaviour of any existing operator, of `Constraints.Check`, or of the prerelease rules in `prereleaseCheck`.
- Changing the logic of `Constraint.Equals`, `Constraints.Equals` or the `sort.Interface` methods of `Constraints`; they work for the new operator through its operator value, as they do for the others.
- Wildcards (`1.x`, `*`), hyphen ranges, the tilde operator `~` without `>`, or `||` alternatives.
- Accepting `^` in `NewVersion` or `NewSemver`.
- Any change to `version.go` or `version_collection.go`, and any edit to `CHANGELOG.md`.

## Affected services and packages

The `version` package (module `github.com/hashicorp/go-version`) is affected through `NewConstraint` and `Constraint.Check` in `constraint.go`, with tests in `constraint_test.go`. `README.md` gains an operator table. The exported API has no new identifier; no dependency is added and no other file changes.

## Acceptance criteria

1. `NewConstraint` in `constraint.go` accepts `^` as an operator wherever the existing operators are accepted, with optional whitespace around the operator and the version and an optional `v` before the version: `NewConstraint("^1.2.3")`, `NewConstraint("^ 1.2.3")`, `NewConstraint("  ^v1.2.3  ")` and `NewConstraint(">= 1.0, ^ 1.2.3-beta")` all return a nil error, the last with two constraints. The regular expression built in `getConstraintRegexp` gains the operator, and `parseSingle` maps it to a new unexported constant `caret operator = '^'` and a new unexported function `constraintCaret(v, c *Version) bool`.
2. Strings that are not one operator followed by one version stay errors with the existing text: `NewConstraint` in `constraint.go` returns an error whose text is exactly `malformed constraint: ` followed by the input for each of `"^"`, `"^^1.0"`, `"^>1.0"` and `"1.0^"`.
3. `Constraint.String` and `Constraints.String` in `constraint.go` return caret constraints as written: for `NewConstraint(">= 1.0, ^ 1.2.3-beta")`, `String()` on the result is exactly `">= 1.0, ^ 1.2.3-beta"`, `String()` on its second element is exactly `" ^ 1.2.3-beta"`, and `Prerelease()` is `true` on the second element and `false` on the first.
4. `constraintCaret` in `constraint.go` returns `false` when the version is lower than the constraint version by `Version.Compare`. Otherwise it finds the pivot index: with `n` the number of segments written in the constraint string (before `NewVersion` pads to three) capped at 3, the pivot is the lowest index `i < n` whose constraint segment is not `0`, or `n - 1` when all of the first `n` segments are `0`. It returns `true` only if the version's segments at every index from `0` to the pivot equal the constraint's.
5. When the first non-zero written segment is the major one, `Check` in `constraint.go` behaves as `>= C` and below the next major: `^1.2.3` matches `1.2.3`, `1.2.4`, `1.9.0`, `1.2.3.1` and `1.2.3+build`, and does not match `1.2.2`, `1.1.9`, `0.9.9`, `2.0.0` or `3.0.0`; `^1.2` matches `1.2.0` and `1.9.9` and does not match `1.1.9` or `2.0`; `^1` matches `1.0.0` and `1.99.5` and does not match `0.9` or `2`; `^10.20.30` matches `10.99.0` and does not match `11.0.0`.
6. When the major segment is `0` and the minor segment is written and not zero, `Check` in `constraint.go` stays below the next minor: `^0.2.3` matches `0.2.3` and `0.2.9` and does not match `0.2.2`, `0.3.0` or `1.0.0`; `^0.2` and `^0.2.0` match `0.2.0`, `0.2.5` and `0.2.7` and do not match `0.1.9` or `0.3.0`.
7. When major and minor are `0` and the patch segment is written and not zero, `Check` in `constraint.go` stays below the next patch: `^0.0.3` matches `0.0.3` and `0.0.3.5` and does not match `0.0.2`, `0.0.4` or `0.1.0`.
8. When every written segment among the first three is `0`, the last written one is the pivot: `^0` matches `0.0.0` and `0.9.9` and does not match `1.0.0`; `^0.0` matches `0.0.0` and `0.0.9` and does not match `0.1.0`; `^0.0.0` matches `0.0.0` and does not match `0.0.1` or `0.1.0`.
9. Segments after the third never pick the pivot but still take part in the lower bound: `^1.2.3.4` matches `1.2.3.4` and `1.9` and does not match `1.2.3.3` or `2.0`; `^0.0.0.5` matches `0.0.0.5` and `0.0.0.9` and does not match `0.0.0.4` or `0.0.1`.
10. `constraintCaret` in `constraint.go` applies the existing `prereleaseCheck` first, exactly as `constraintGreaterThanEqual` does, and adds no other prerelease rule. A caret constraint without a prerelease never matches a prerelease version: `^1.2.3` does not match `1.2.3-beta`, `1.3.0-beta` or `2.0.0-beta`. A caret constraint with a prerelease matches prerelease versions only when they have the same segments, and still matches releases in its range: `^1.2.3-beta.2` matches `1.2.3-beta.2`, `1.2.3-beta.3`, `1.2.3-rc.1`, `1.2.3` and `1.5.0`, and does not match `1.2.3-beta.1`, `1.2.3-alpha`, `1.2.4-beta`, `1.5.0-rc.1` or `2.0.0`; `^0.0.3-beta` matches `0.0.3` and does not match `0.0.4`.
11. A caret constraint combines with others in one `Constraints` value: `^1.2, != 1.5.0` matches `1.5.1` and not `1.5.0`; `>= 1.4, ^1.2` matches `1.4.0` and not `1.3.0`.
12. Through the new operator value, the unchanged `Constraints.Equals` in `constraint.go` gives: `^1.2` equals `^ 1.2.0`; `^1.2, < 1.8` equals `< 1.8, ^1.2`; `^1.2` does not equal `^1.3`, `~> 1.2`, `>= 1.2` or `1.2`.
13. The doc comment of `NewConstraint` in `constraint.go` lists the supported operators, `=`, `!=`, `>`, `<`, `>=`, `<=`, `~>` and `^`, and says in one or two sentences what `^` matches.
14. `constraint_test.go` gains table-driven cases, in package `version` and using only the standard `testing` package, that cover criteria 1 to 12 (they may extend `TestNewConstraint`, `TestConstraintCheck`, `TestConstraintEqual` and `TestConstraintsString` or be new test functions). Every existing test case stays and passes, and `go test ./...` passes.
15. The first line of both `constraint.go` and `constraint_test.go` is exactly `// Copyright IBM Corp. 2014, 2026` (the end year changes from `2025` because the files are edited).
16. The `#### Version Constraints` section of `README.md` gains, between its heading and its existing code block, a Markdown table with the columns `Operator` and `Meaning` and one row for each of `=`, `!=`, `>`, `<`, `>=`, `<=`, `~>` and `^`, followed by one sentence giving the caret examples `^1.2.3` is `>= 1.2.3, < 2.0.0`, `^0.2.3` is `>= 0.2.3, < 0.3.0` and `^0.0.3` is `>= 0.0.3, < 0.0.4`. The existing code block of the section and every other part of `README.md` are unchanged.
17. `go.mod` is unchanged, `go vet ./...` reports nothing and `gofmt -l .` prints nothing.

## Risks

- The pivot depends on how many segments the constraint wrote, which `NewVersion` hides by padding to three; `^0.0` and `^0.0.0` mean different ranges. The implementation must read the unexported written-segment count on `Version` (the `si` field that `constraintPessimistic` already uses), not the padded segments.
- In the regular expression `^` is an anchor; the operator must be escaped in the alternation, or every constraint string stops parsing as before.
- `Constraints.Less` orders by operator value, so the new constant must differ from every existing one or `Equals` would confuse `^` with another operator. Criterion 12 pins this.
- Three other changes are being made to this repository at the same time, each adding to `README.md`. Keeping the table exactly where criterion 16 places it avoids a merge conflict.

## Open questions

None.
