# Spec

## Problem

Package `lo` has string helpers that shorten text (`Ellipsis`, `Substring` in `string.go`) but none that lengthen it to a fixed width. Formatting an identifier as `00042` or aligning a column needs a hand-written loop, and the usual `strings.Repeat` version counts bytes, so it misaligns any string with accented, CJK or emoji characters. Lodash, which this library follows, offers `padStart`, `padEnd` and `pad`.

## Scope

Add three helpers to `string.go`: `PadLeft`, `PadRight` and `Pad`. Each takes a string, a target length and a pad string, and returns the string lengthened to exactly the target length by repeating the pad string, cut short when it does not fit a whole number of times. Lengths are counted in runes (Unicode code points), as `RuneLength` and `Ellipsis` already do, never in bytes. `PadLeft` adds the padding before the string, `PadRight` after it, and `Pad` on both sides, giving the right side the extra rune when the padding cannot be split evenly. On each side the pad string starts again from its first rune. A string that is already long enough, a non-positive target length and an empty pad string all return the string unchanged.

The three functions and any unexported helper they share are appended at the end of `string.go`, after `Ellipsis`, using only packages `string.go` already imports, so the import block and the line numbers existing `docs/data/*.md` pages record in `sourceRef` do not move.

The scope also includes what the contributing guide requires of new helpers: tests in `string_test.go`, one `Example` function each in `lo_example_test.go`, `README.md` index lines and sections, one `docs/data/` page each linked both ways with the `Ellipsis` page, and lines in `docs/static/llms.txt`.

## Non-goals

- Truncating a string that is longer than the target length.
- Counting grapheme clusters or display width: one rune counts as one, so a combined emoji counts as several and a wide CJK character as one.
- A single-rune or `byte` pad parameter, a default pad of one space, or a variadic pad argument.
- `it`, `mutable` or `parallel` variants, and a benchmark.
- `// Play:` links and `playUrl` values: the helpers are not released, so the playground cannot compile them.
- Changing `Ellipsis` or any other existing helper, or editing any `sourceRef` of an existing page.

## Affected services and packages

Package `github.com/samber/lo` gains three exported functions in `string.go`, with tests in `string_test.go` and examples in `lo_example_test.go`. Documentation changes are in `README.md`, the new pages `docs/data/core-padleft.md`, `docs/data/core-padright.md` and `docs/data/core-pad.md`, the existing page `docs/data/core-ellipsis.md`, and `docs/static/llms.txt`. No sub-package, dependency or existing signature changes.

## Acceptance criteria

1. `string.go` declares, after every existing declaration in the file, exactly `func PadLeft(str string, length int, pad string) string`, `func PadRight(str string, length int, pad string) string` and `func Pad(str string, length int, pad string) string`, and the import block of `string.go` is unchanged.
2. `PadLeft` in `string.go` prepends the pad string, repeated from its first rune and cut to the exact number of missing runes: `PadLeft("42", 5, "0")` is `"00042"`, `PadLeft("abc", 8, "_-")` is `"_-_-_abc"`, `PadLeft("abc", 5, "12345")` is `"12abc"` and `PadLeft("", 3, "ab")` is `"aba"`.
3. `PadRight` in `string.go` appends the pad string, repeated from its first rune and cut to the exact number of missing runes: `PadRight("42", 5, "0")` is `"42000"`, `PadRight("abc", 8, "_-")` is `"abc_-_-_"`, `PadRight("abc", 5, "12345")` is `"abc12"` and `PadRight("", 3, "ab")` is `"aba"`.
4. `Pad` in `string.go` puts `missing / 2` runes (integer division) of padding on the left and the remaining runes on the right, where `missing` is the target length minus the rune count of `str`, and builds each side separately from the first rune of the pad string: `Pad("abc", 7, "*")` is `"**abc**"`, `Pad("abc", 6, "*")` is `"*abc**"`, `Pad("abc", 4, "*")` is `"abc*"`, `Pad("abc", 8, "_-")` is `"_-abc_-_"`, `Pad("abc", 9, "_-")` is `"_-_abc_-_"`, `Pad("abc", 5, "12345")` is `"1abc1"` and `Pad("", 3, "ab")` is `"aab"`.
5. `PadLeft`, `PadRight` and `Pad` in `string.go` return `str` unchanged when its rune count is greater than or equal to `length`, including a zero or negative `length`: for each of the three, `("abc", 3, "x")`, `("abc", 0, "x")` and `("abc", -1, "x")` give `"abc"`, and `("abcdef", 3, "x")` gives `"abcdef"`.
6. `PadLeft`, `PadRight` and `Pad` in `string.go` return `str` unchanged when `pad` is `""`: for each of the three, `("abc", 6, "")` gives `"abc"`.
7. `PadLeft`, `PadRight` and `Pad` in `string.go` count the length of `str` and cut the pad string in runes, never in bytes: `PadLeft("héllo", 7, "*")` is `"**héllo"`, `PadRight("héllo", 7, "*")` is `"héllo**"`, `PadLeft("ab", 5, "世界")` is `"世界世ab"`, `PadRight("ab", 5, "世界")` is `"ab世界世"`, `Pad("世界", 5, "é")` is `"é世界éé"` and `Pad("é", 4, "世界")` is `"世é世界"`.
8. Each of the three doc comments in `string.go` starts with the function name, says that the length is counted in runes, that the pad string is repeated and truncated, and that the string is returned unchanged when it is already long enough or the pad string is empty; the comment of `Pad` also says that the right side receives the extra rune. None has a `// Play:` line.
9. `string_test.go` has functions `TestPadLeft`, `TestPadRight` and `TestPad`, each calling `t.Parallel()` and using a table of cases that covers a single-rune pad, a multi-rune pad that is truncated, a string already long enough, a negative length, an empty pad string, an empty string and a multi-byte string or pad; `TestPad` also covers an odd number of missing runes.
10. `lo_example_test.go` has, immediately after `ExampleChunkString` and in this order, `ExamplePadLeft`, `ExamplePadRight` and `ExamplePad`. Each prints three results one per line under `// Output:`: `PadLeft` of `("42", 5, "0")`, `("abc", 8, "_-")`, `("abc", 2, "_")` printing `00042`, `_-_-_abc`, `abc`; `PadRight` of the same arguments printing `42000`, `abc_-_-_`, `abc`; `Pad` of `("abc", 7, "*")`, `("abc", 8, "_-")`, `("abc", 2, "_")` printing `**abc**`, `_-abc_-_`, `abc`.
11. `README.md` lists `- [PadLeft](#padleft)`, `- [PadRight](#padright)` and `- [Pad](#pad)`, in this order, on the lines after `- [Ellipsis](#ellipsis)` under "Supported helpers for strings", and has sections `### PadLeft`, `### PadRight` and `### Pad`, in this order, between the end of the `### Ellipsis` section and `### T2 -> T9`. Each section has a short description that mentions runes and a Go code block showing the three calls of that helper from criterion 10 with their results. No section has a `[[play](...)]` line.
12. `docs/data/core-padleft.md`, `docs/data/core-padright.md` and `docs/data/core-pad.md` exist. Each has front matter with `name` (`PadLeft`, `PadRight`, `Pad`), `slug` (`padleft`, `padright`, `pad`), `sourceRef: string.go#L<line of that func declaration>`, `category: core`, `subCategory: string`, an empty `playUrl:`, `variantHelpers` holding only its own key (`core#string#padleft`, `core#string#padright`, `core#string#pad`), `similarHelpers` holding the keys of the other two pad helpers followed by `core#string#ellipsis`, `position` (`110`, `120`, `130`), and `signatures` holding only its own signature exactly as in criterion 1 inside double quotes; then a short description and a Go code block with at least the three calls of criterion 10.
13. `docs/data/core-ellipsis.md` has `  - core#string#padleft`, `  - core#string#padright` and `  - core#string#pad` added, in this order, as the last entries of its `similarHelpers` list and no other change.
14. `docs/static/llms.txt` has the lines `- PadLeft: Pad string on the left to a length in runes`, `- PadRight: Pad string on the right to a length in runes` and `- Pad: Pad string on both sides to a length in runes`, in this order, immediately after `- Ellipsis: Truncate string to length with ellipsis` in the core "### String" list.
15. `go test ./...` and `gofmt -l .` succeed (the latter prints nothing), and `node docs/scripts/check-similar-exists.js`, `node docs/scripts/check-similar-keys-exist-in-directory.js` and `node docs/scripts/check-cross-references.js` each exit 0.

## Risks

- `README.md`, `docs/static/llms.txt` and `lo_example_test.go` are edited by other helper additions in flight. The insertion points above are specific to the string helpers so that the changes merge cleanly; inserting elsewhere invites conflicts.
- Byte-based shortcuts (`len(str)`, `strings.Repeat(pad, n)[:n]`) pass every ASCII test and fail criterion 7; they can also cut a multi-byte rune in half and return invalid UTF-8.
- `Pad` favours the right side and restarts the pad string on each side, as lodash does. An implementation that pads one long run and splits it around the string gives a different result for multi-rune pads (`"_-abc-_-"` instead of `"_-abc_-_"`).
- A pad string or input that is not valid UTF-8 is outside this contract; its invalid bytes are counted and copied however the rune conversion treats them.
- A very large `length` allocates a string of that size. No upper bound is enforced, as with `strings.Repeat`.

## Open questions

None.
