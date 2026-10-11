# Spec

## Problem

GJSON has built-in modifiers for transforming JSON values, but it has no built-in way to total numeric values returned as an array. As a result, a query such as `prices.#.amount|@sum` cannot currently produce the sum within GJSON, and callers must perform the aggregation themselves.

## Scope

Add the built-in `@sum` modifier to the `gjson` package. When the current value is a JSON array, `@sum` must add only its direct elements that are JSON numbers and return the result as a JSON number. Strings, booleans, nulls, objects, and nested arrays are ignored. Empty arrays and arrays with no numeric elements return numeric zero; every non-array input also returns numeric zero.

The sum and its raw JSON representation use float64 semantics, with formatting equivalent to `strconv.FormatFloat(sum, 'f', -1, 64)`, so integer totals remain unadorned while fractional totals retain their necessary decimal representation. `@sum` must be available through the built-in modifier registry and pipe syntax, including after array-producing paths such as `prices.#.amount`.

The requested behavior must be covered by executable cases in `gjson_test.go`, and the modifiers list in `SYNTAX.md` must describe `@sum` and its numeric-only, zero-on-no-match behavior.

## Non-goals

- Coercing numeric-looking strings or values nested inside objects or arrays into numbers.
- Adding arguments, alternate aggregation operations, or a new public aggregation API.
- Changing the semantics or registration behavior of existing modifiers, path selectors, or JSON number parsing.
- Providing exact arithmetic beyond the repository's float64 representation and the specified formatting.

## Affected services and packages

Only the `gjson` Go package's built-in modifier dispatch and query-result transformation behavior is affected at runtime; repository-local validation and documentation are also in scope. No external service, storage system, network dependency, or additional runtime package is involved.

## Acceptance criteria

1. In `gjson_test.go`, calling `gjson.Get` with JSON `{"a":[1,2,3.5,"x",null]}` and path `a|@sum` returns a result whose `Raw` is exactly `"6.5"`, whose result type is JSON number, and whose numeric value is `6.5`.
2. In `gjson_test.go`, calling `gjson.Get` with JSON `{"a":[1,2]}` and path `a|@sum` returns the JSON-number raw value `"3"`, with no unnecessary decimal suffix; the equivalent fractional input `[1,2,3.5]` returns `"6.5"`.
3. In `gjson_test.go`, calling `gjson.Get` with JSON `{"a":[1,"2",true,null,{"n":3},[4],-0.5]}` and path `a|@sum` sums only the direct JSON numbers and produces the JSON-number raw value `"0.5"`.
4. In `gjson_test.go`, calling `gjson.Get` with path `a|@sum` on JSON `{"a":[]}` and on JSON `{"a":["x",true,null,{},[]]}` produces the JSON-number raw value `"0"` in both cases.
5. In `gjson_test.go`, calling `gjson.Get` with path `a|@sum` on each of JSON `{"a":"text"}`, `{"a":{}}`, `{"a":true}`, `{"a":null}`, and `{"b":1}` produces the JSON-number raw value `"0"`.
6. In `gjson_test.go`, a `gjson.Get` pipe query such as JSON `{"prices":[{"amount":1},{"amount":2.5},{"amount":"3"}]}` with path `prices.#.amount|@sum` recognizes `@sum` without custom modifier registration and returns the JSON-number raw value `"3.5"`.
7. The raw output of `@sum` is an unquoted JSON number whose text matches `strconv.FormatFloat(sum, 'f', -1, 64)` for the float64 sum of the included array elements, including negative and fractional totals.
8. The built-in modifiers list in `SYNTAX.md` contains `@sum` with documentation stating that it sums JSON-number elements of an array, ignores non-number elements, returns `0` for an empty or number-free array, and returns `0` for non-array input.

## Risks

Float64 addition and formatting can lose precision for very large integers, very small values, or sums involving values outside exact float64 integer precision. This is an intentional dependency on the specified `strconv.FormatFloat`/float64 contract; treating JSON numbers as exact decimals instead would produce different output and behavior.

The modifier operates on direct array elements only. If an implementation recursively traverses nested arrays or objects, non-number data could be counted unexpectedly and queries relying on the stated zero/ignore behavior would be incorrect.

The built-in modifier registry and pipe evaluation must continue to distinguish a JSON number result from a quoted string. If the result is serialized as a JSON string, consumers of `Raw`, `Type`, or subsequent path operations will observe an incompatible result even when the displayed digits are correct.

## Open questions

None.
