# Spec

## Problem

GJSON has built-in modifiers that reshape an array (`@reverse`, `@flatten`) but none that removes repeated elements. A path such as `friends.#.city` commonly returns the same value several times, and callers who want each distinct value once must copy the result into a Go map themselves or register a custom modifier with `AddModifier` in every program.

## Scope

Add a built-in `@unique` modifier to `gjson.go`: a function `modUnique(json, arg string) string` registered under the name `unique` in the `stockModifiers` map built by `init`. Applied to a JSON array it returns a new array holding the first occurrence of each distinct element, in the original order; applied to anything else it returns its input unchanged. Two elements are the same when their raw JSON text is identical after the whitespace outside string literals is removed. The modifier argument is ignored.

Place `modUnique` directly after `modGroup` in `gjson.go`. The scope also includes one line for `@unique` in the built-in modifier list of `README.md` and of `SYNTAX.md`, and tests in a new file `gjson_unique_test.go` (package `gjson`).

## Non-goals

- Semantic equality: `1` and `1.0`, `1` and `"1"`, and two objects with the same members in a different order stay distinct elements.
- Removing duplicate keys from an object, or deduplicating arrays nested inside the elements.
- Options through the modifier argument (case folding, a key to compare by, sorting).
- Changing any other modifier, `AddModifier`, `ModifierExists`, the path parser, or the `Index` and `Indexes` fields of any result.
- New module dependencies.

## Affected services and packages

The `github.com/tidwall/gjson` Go package is affected through its path syntax: `gjson.go` (the `stockModifiers` map in `init` and the new `modUnique`), the modifier lists in `README.md` and `SYNTAX.md`, and the new test file `gjson_unique_test.go`. No external service, persistent data, or runtime dependency is affected.

## Acceptance criteria

1. `gjson.go` defines `func modUnique(json, arg string) string`, and `init` in `gjson.go` registers it in `stockModifiers` under the key `"unique"`, so `ModifierExists("unique", nil)` returns `true`.
2. `modUnique` in `gjson.go` keeps the first occurrence of each element and the original order: `Get("[1,2,1,3,2]", "@unique").Raw` is exactly `[1,2,3]` and `Get(`["a","b","a"]`, "@unique").Raw` is exactly `["a","b"]`.
3. `modUnique` in `gjson.go` compares elements by raw JSON text and not by value: `Get(`[1,"1",1.0,true,"true",null,null]`, "@unique").Raw` is exactly `[1,"1",1.0,true,"true",null]`.
4. `modUnique` in `gjson.go` ignores whitespace outside string literals when comparing, keeps whitespace inside string literals significant, and writes each kept element with its original raw text: `Get(`[{"a":1},{"a": 1},{"a":1,"b":2},{"b":2,"a":1},[1, 2],[1,2]]`, "@unique").Raw` is exactly `[{"a":1},{"a":1,"b":2},{"b":2,"a":1},[1, 2]]`, and `Get(`["a b","ab","a b"]`, "@unique").Raw` is exactly `["a b","ab"]`.
5. `modUnique` in `gjson.go` writes its output as `[`, the kept elements separated by a single `,` with no other whitespace between elements, then `]`: `Get(" [ 1 , 1 , 2 ] ", "@unique").Raw` is exactly `[1,2]` and `Get("[]", "@unique").Raw` is exactly `[]`.
6. `modUnique` in `gjson.go` returns its input unchanged when the input is not a JSON array: `Get(`{"a":[1,1]}`, "@unique").Raw` is exactly `{"a":[1,1]}` and `Get(`"abc"`, "@unique").Raw` is exactly `"abc"`.
7. `modUnique` in `gjson.go` ignores its `arg` parameter: `Get("[1,2,1]", `@unique:{"x":true}`).Raw` is exactly `[1,2]`.
8. The modifier registered by `init` in `gjson.go` chains like the other built-in modifiers: with `json` set to `{"tags":["x","y","x"]}`, `Get(json, "tags.@unique").Raw` is exactly `["x","y"]`, `Get(json, "tags.@unique.#").Raw` is exactly `2` and `Get(json, "tags.@unique.1").Raw` is exactly `"y"`; `Get(`{"f":[{"c":"NY"},{"c":"LA"},{"c":"NY"}]}`, "f.#.c|@unique").Raw` is exactly `["NY","LA"]`; and `Get("[3,1,3,2,1]", "@unique|@reverse").Raw` is exactly `[2,1,3]`.
9. The built-in modifier list in `README.md` (section "Modifiers and path chaining") and the one in `SYNTAX.md` (section "Modifiers") each gain, directly after the `@dig` line, the line ``- `@unique`: Removes duplicate elements from an array, keeping the first occurrence of each.``
10. A new file `gjson_unique_test.go` in package `gjson` has a test function `TestModifierUnique` asserting the exact results of criteria 2 to 8, and `go test ./...` and `go vet ./...` pass.

## Risks

- Comparing by raw text is deliberate and cheap, but users may expect `1` and `1.0`, or objects with reordered members, to collapse into one. The documentation line and criterion 3 fix the behaviour; widening it later would change results callers depend on.
- The modifier holds one map entry per distinct element, so memory grows with the size of the array, as `@reverse` and `@group` already do.
- `unique` becomes a stock modifier name. `AddModifier` ignores attempts to replace stock modifiers, so a program that registers its own `unique` modifier silently gets the built-in one after this change.

## Open questions

None.
