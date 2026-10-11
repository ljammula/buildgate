# Spec

## Problem

`Result.ForEach`, `All`, `Keys` and `Values` in `gjson.go` visit only the direct children of an object or array. A caller who needs every value in a document together with the path that reaches it (to flatten a document into `path = value` lines, to diff two documents, or to list every field) must write the recursion and build the escaped path by hand, and it is easy to get the escaping of keys that contain `.`, `*` or `?` wrong.

## Scope

Add two exported methods on `Result` in `gjson.go`, appended at the end of the file after `Values`:

- `func (t Result) Walk(fn func(path string, value Result) bool)` calls `fn` once for every value nested inside `t`, at any depth, in document order, a container before its own children (depth-first, pre-order). `t` itself is not passed to `fn`. `path` is the GJSON path of the value relative to `t`: the components from `t` down to the value joined with `.`, where the component for an object member is `Escape` applied to the decoded key (the key's `String()`), and the component for an array element is its zero-based index in decimal. When `fn` returns `false` the walk ends at once and `fn` is not called again.
- `func (t Result) Leaves() iter.Seq2[string, Result]` yields the same `(path, value)` pairs as `Walk`, in the same order, restricted to values that are neither an object nor an array. Empty objects and empty arrays are not yielded. Breaking out of the `range` loop ends the walk.

When `t` is not an object or array (a string, number, boolean, `null`, or a result that does not exist), `Walk` never calls `fn` and `Leaves` yields nothing. The scope also includes doc comments on both methods, a `README.md` section, and tests in a new file `gjson_walk_test.go` (package `gjson`).

## Non-goals

- Any requirement on the `Index` or `Indexes` fields of the values passed to `fn` or yielded by `Leaves`; they are unspecified and the tests must not assert them.
- A way to skip the children of one container while continuing the walk, a depth limit, or post-order traversal.
- Guaranteeing that `t.Get(path)` finds the value for every possible key: an empty key or duplicate keys in one object produce paths `Get` cannot resolve uniquely, and that is accepted.
- A package-level `Walk` function, a `[]byte` variant, a new path-syntax operator or modifier, or any change to `ForEach`, `All`, `Keys`, `Values`, `Escape`, `Path` or `Paths`.
- New module dependencies.

## Affected services and packages

The `github.com/tidwall/gjson` Go package is affected through its public `Result` API: the new `Walk` and `Leaves` methods at the end of `gjson.go`, a new `README.md` section, and the new test file `gjson_walk_test.go`. No external service, persistent data, or runtime dependency is affected.

## Acceptance criteria

The criteria use `doc` for the string `{"name":{"first":"Tom","last.name":"A"},"tags":["x",{"k":null}],"n":1.5,"e":{},"l":[]}`.

1. `gjson.go` declares exactly `func (t Result) Walk(fn func(path string, value Result) bool)` and `func (t Result) Leaves() iter.Seq2[string, Result]`, both placed after `Values` at the end of the file, each with a doc comment that starts with the method name.
2. `Walk` in `gjson.go` on `Parse(doc)`, with an `fn` that always returns `true`, calls `fn` exactly ten times, with these `path` and `value.Raw` pairs in this order: `name` and `{"first":"Tom","last.name":"A"}`; `name.first` and `"Tom"`; `name.last\.name` and `"A"`; `tags` and `["x",{"k":null}]`; `tags.0` and `"x"`; `tags.1` and `{"k":null}`; `tags.1.k` and `null`; `n` and `1.5`; `e` and `{}`; `l` and `[]`.
3. In the `Walk` call of criterion 2, `value.Type` is `JSON` for `name`, `tags`, `tags.1`, `e` and `l`, `String` for `name.first`, `name.last\.name` and `tags.0`, `Null` for `tags.1.k`, and `Number` for `n`.
4. `Walk` in `gjson.go` stops when `fn` returns `false`: on `Parse(doc)` with an `fn` that returns `false` only for the path `tags.0`, `fn` is called with exactly the paths `name`, `name.first`, `name.last\.name`, `tags`, `tags.0` and no others; with an `fn` that returns `false` on its first call, `fn` is called exactly once, with the path `name`.
5. `Walk` in `gjson.go` uses decimal indexes for array elements at every depth: on `Parse(`[10,[20,{"a":true}],"z"]`)` it visits, in order, `0` (`10`), `1` (`[20,{"a":true}]`), `1.0` (`20`), `1.1` (`{"a":true}`), `1.1.a` (`true`), `2` (`"z"`), where the parenthesised text is `value.Raw`.
6. `Walk` in `gjson.go` reports paths relative to the result it is called on: on `Get(doc, "tags")` it visits, in order, `0` (`"x"`), `1` (`{"k":null}`), `1.k` (`null`).
7. `Walk` in `gjson.go` builds each object-member component with `Escape` on the decoded key: on `Parse(`{"a.b":1,"c*d":{"x?":2},"@m":3,"sp ace":4,"12":5}`)` it visits, in order, the paths `a\.b`, `c\*d`, `c\*d.x\?`, `\@m`, `sp ace`, `12`; and on `Parse(`{"a\"b":1}`)` it visits exactly one path, the four characters `a\"b` (the key `a"b` after `Escape`).
8. `Walk` in `gjson.go` never calls `fn`, and `Leaves` in `gjson.go` yields nothing, for each of `Parse(`"str"`)`, `Parse("12")`, `Parse("true")`, `Parse("null")`, `Parse("{}")`, `Parse("[]")`, the zero value `Result{}`, and `Get(doc, "missing")`; neither panics.
9. `Leaves` in `gjson.go` on `Parse(doc)` yields exactly five pairs, in this order, as `path` and `value.Raw`: `name.first` and `"Tom"`; `name.last\.name` and `"A"`; `tags.0` and `"x"`; `tags.1.k` and `null`; `n` and `1.5`.
10. `Leaves` in `gjson.go` supports early exit: a `for path := range Parse(doc).Leaves()` loop that executes `break` when `path` is `name.last\.name` runs its body exactly twice, for `name.first` and `name.last\.name`, and does not panic.
11. `README.md` gains a section headed `## Walk every value`, placed directly after the section `## Iterate through an object or array` and before `## Simple Parse and Get`, that names `Walk` and `Leaves`, states that paths are relative to the result and that keys are escaped as `Escape` does, and shows one Go code block calling `Walk` and one ranging over `Leaves()`.
12. A new file `gjson_walk_test.go` in package `gjson` has test functions `TestWalk` and `TestLeaves` asserting the exact sequences of criteria 2 to 10, and `go test ./...` and `go vet ./...` pass.

## Risks

- Recursion depth equals the nesting depth of the document, so a pathologically deep document can exhaust the stack, as it already can in `@dig` and `@flatten` with `deep`.
- Each visited value costs one path string allocation; a walk over a large document allocates in proportion to the number of values. Callers who need only the top level should keep using `ForEach`.
- Paths are meant for `Get`, but documents with an empty key or with duplicate keys give paths that do not round-trip. The doc comments must say so, so callers do not assume a guarantee the method does not make.
- `Leaves` omits empty objects and arrays, so a consumer that rebuilds a document from the leaves loses them; `Walk` reports them and is the method to use for that.

## Open questions

None.
