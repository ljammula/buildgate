# Spec

## Problem

`README.md` says go-version "can increment versions", but the `version` package has no way to do it. A caller who wants the next major, minor or patch release of a parsed `*Version` has to read `Segments64()`, do the arithmetic, format a string and parse it again with `NewVersion`, and has to decide alone what happens to the prerelease and metadata parts.

## Scope

Add three methods on `*Version` in a new file `version_increment.go`: `IncMajor`, `IncMinor` and `IncPatch`. Each returns a new `*Version` and leaves the receiver unchanged. `IncMajor` adds one to the first segment and `IncMinor` adds one to the second; both set every later segment to zero. `IncPatch` adds one to the third segment and sets every later segment to zero, except on a version with a prerelease, where it returns the release that the prerelease leads to (the same segments, unchanged). The result never carries a prerelease, metadata or prefix, keeps the number of segments of the receiver, and is the same value `NewVersion` returns for the result's canonical string.

The scope also includes table-driven tests in a new file `version_increment_test.go`, doc comments on the three methods, and a new `#### Version Incrementing` section in `README.md`.

## Non-goals

- Changing the receiver in place, or adding setters for individual segments.
- Incrementing a segment after the third, or incrementing or generating prerelease identifiers.
- Detecting `int64` overflow of a segment.
- Changing `NewVersion`, `NewSemver`, `Core`, comparison or any constraint behaviour.
- Editing `CHANGELOG.md`.

## Affected services and packages

The `version` package (module `github.com/hashicorp/go-version`) gains three exported methods on `*Version`, in the new file `version_increment.go`, with tests in the new file `version_increment_test.go`. `README.md` gains one section. No existing function, dependency or file other than `README.md` changes.

## Acceptance criteria

1. `version_increment.go` declares exactly these exported methods, each with a doc comment: `func (v *Version) IncMajor() *Version`, `func (v *Version) IncMinor() *Version` and `func (v *Version) IncPatch() *Version`. The file is in package `version` and starts with the two lines `// Copyright IBM Corp. 2014, 2026` and `// SPDX-License-Identifier: MPL-2.0` followed by a blank line; `version_increment_test.go` starts the same way.
2. `IncMajor` in `version_increment.go` returns a version whose first segment is the receiver's plus one and whose other segments are all `0`: for `NewVersion` inputs `"1.2.3"`, `"1"`, `"v1.9.9"`, `"0.0.0"` and `"1.2.3.4"`, `IncMajor().String()` is exactly `"2.0.0"`, `"2.0.0"`, `"2.0.0"`, `"1.0.0"` and `"2.0.0.0"`.
3. `IncMinor` in `version_increment.go` returns a version whose first segment is unchanged, whose second segment is the receiver's plus one and whose later segments are all `0`: for inputs `"1.2.3"`, `"1"`, `"v1.9.9"`, `"0.0.0"` and `"1.2.3.4"`, `IncMinor().String()` is exactly `"1.3.0"`, `"1.1.0"`, `"1.10.0"`, `"0.1.0"` and `"1.3.0.0"`.
4. `IncPatch` in `version_increment.go`, on a receiver whose `Prerelease()` is `""`, returns a version whose first two segments are unchanged, whose third segment is the receiver's plus one and whose later segments are all `0`: for inputs `"1.2.3"`, `"1"`, `"1.2"`, `"v1.9.9"`, `"0.0.0"`, `"1.2.3.4"` and `"1.2.3+build.5"`, `IncPatch().String()` is exactly `"1.2.4"`, `"1.0.1"`, `"1.2.1"`, `"1.9.10"`, `"0.0.1"`, `"1.2.4.0"` and `"1.2.4"`.
5. `IncPatch` in `version_increment.go`, on a receiver whose `Prerelease()` is not `""`, returns a version with exactly the receiver's segments and no prerelease: for inputs `"1.2.3-beta.1"`, `"1.2.3-beta.1+build.5"`, `"2.0.0-rc1"` and `"1.2.3.4-beta"`, `IncPatch().String()` is exactly `"1.2.3"`, `"1.2.3"`, `"2.0.0"` and `"1.2.3.4"`.
6. `IncMajor` and `IncMinor` in `version_increment.go` always increment, with or without a prerelease on the receiver: for input `"2.0.0-rc1"`, `IncMajor().String()` is `"3.0.0"` and `IncMinor().String()` is `"2.1.0"`; for input `"1.2.3-beta.1+build.5"` they are `"2.0.0"` and `"1.3.0"`.
7. For every receiver `v` and each of the three methods in `version_increment.go`, the result `r` is a non-nil pointer different from `v`; `r.Prerelease()`, `r.Metadata()` and `r.Prefix()` are `""`; `r.Original()` equals `r.String()`; and `reflect.DeepEqual(r, Must(NewVersion(r.String())))` is `true`. This holds for a receiver parsed with `WithPrefix` too: `Must(NewVersion("deployment-v1.2.3-beta+meta", WithPrefix("deployment-"))).IncMinor()` has `String()` `"1.3.0"`, `Original()` `"1.3.0"` and `Prefix()` `""`.
8. None of the three methods in `version_increment.go` changes its receiver: after any of the calls, the receiver's `String()`, `Original()`, `Prefix()`, `Prerelease()`, `Metadata()` and `Segments64()` return what they returned before.
9. On the zero value `&Version{}`, which the package treats as `0.0.0`, the methods in `version_increment.go` do not panic: `IncMajor().String()` is `"1.0.0"`, `IncMinor().String()` is `"0.1.0"` and `IncPatch().String()` is `"0.0.1"`.
10. `version_increment_test.go` contains table-driven tests in package `version`, using only the standard `testing` package, that cover criteria 2 to 9, and `go test ./...` passes.
11. `README.md` has a new section headed `#### Version Incrementing`, placed immediately before the existing `#### Version Parsing and Comparison with Prefixes` heading. It states that the three methods return a new version and drop prerelease and metadata, states the `IncPatch` rule for a prerelease, and has one `go` code block that calls `IncMajor`, `IncMinor` and `IncPatch` on `1.2.3` and shows the results `2.0.0`, `1.3.0` and `1.2.4`. No other part of `README.md` changes.
12. `go.mod` is unchanged, `go vet ./...` reports nothing and `gofmt -l .` prints nothing.

## Risks

- Callers may expect `IncMajor` of `2.0.0-rc1` to be `2.0.0`, as some other tools do. The doc comments and the README section must state the rule chosen here so the behaviour is not a surprise.
- The result must be built the way `NewVersion` builds a version (including the unexported count of written segments that the `~>` operator reads), or a result used as a constraint operand or compared with `reflect.DeepEqual` behaves differently from a parsed version. Criterion 7 pins this.
- Three other changes are being made to this repository at the same time, each adding a section to `README.md`. Keeping the new section exactly where criterion 11 places it avoids a merge conflict.

## Open questions

None.
