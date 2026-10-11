# Spec

## Problem

The `github.com/tidwall/gjson` parser does not consistently accept a UTF-8
byte-order mark (BOM, U+FEFF encoded as `EF BB BF`) before a JSON document.
Because the parser recognizes JSON whitespace but not this file header, a
BOM-prefixed response can be treated as nonexistent or invalid, causing JSON
lookups and validation to fail even though the payload after the header is
valid JSON.

## Scope

The package must treat one UTF-8 BOM at byte offset zero as an optional input
header for JSON parsing. The behavior applies consistently to `Parse`,
`ParseBytes`, `Get`, `GetBytes`, `GetMany`, `GetManyBytes`, `Valid`, and
`ValidBytes`, as well as the JSON Lines parsing exposed by `ForEachLine` and
the `..` paths accepted by `Get`.

After the header, normal JSON whitespace and the existing JSON grammar remain
unchanged. The BOM is input framing, not part of the parsed value: returned
`Result.Raw` and string values must describe only the JSON payload, while
`Result.Index` must continue to refer to offsets in the original input.

## Non-goals

- Supporting UTF-16, UTF-32, or any encoding-specific marker other than the
  UTF-8 BOM.
- Removing U+FEFF bytes that occur after byte zero, inside a JSON string, or in
  any other position within the payload.
- Accepting incomplete or malformed BOM byte sequences, or changing the
  package's handling of otherwise invalid JSON and non-whitespace trailing
  data.
- Adding a BOM to serialized results, modifier output, or any other generated
  JSON.
- Changing public API signatures, result types, or the existing no-error
  reporting model.

## Affected services and packages

This is limited to the `github.com/tidwall/gjson` package. No external
service, network integration, or additional package is affected.

## Acceptance criteria

1. `Parse` and `ParseBytes` accept a single `EF BB BF` prefix before each
   valid top-level JSON type (object, array, string, number, boolean, or
   null), and return the same type and value as parsing the corresponding
   BOM-free payload; the returned `Result.Raw` does not contain the BOM.
2. `Get` and `GetBytes` return the same path results for a BOM-prefixed JSON
   object or array as for the equivalent BOM-free input, and `GetMany` and
   `GetManyBytes` provide the same result for every requested path.
3. `Valid` and `ValidBytes` return `true` for a BOM-prefixed valid JSON
   document and return `false` for a BOM-only input, a BOM followed only by
   JSON whitespace, a BOM followed by malformed JSON, or a valid JSON value
   followed by non-whitespace data.
4. `ForEachLine` and `Get` JSON Lines paths accept one BOM at the beginning of
   the input stream, emit or return the same records and values as the
   BOM-free stream, and do not expose the BOM as part of the first record.
5. BOM handling is limited to the exact three-byte prefix at byte zero: a BOM
   after leading JSON whitespace, a BOM between JSON tokens, an incomplete
   prefix, or any other non-leading BOM is not silently ignored and does not
   make otherwise invalid JSON valid; a U+FEFF encoded inside a quoted JSON
   string remains part of that string's value.
6. For BOM-prefixed input, `Result.Index` from `Parse`, `ParseBytes`, `Get`,
   and `GetBytes` identifies the result's location in the original input,
   including the three BOM bytes and any preceding whitespace. For a result
   with a nonzero index, slicing the original byte input at
   `result.Index:result.Index+len(result.Raw)` yields the same BOM-free raw
   JSON represented by `result.Raw`.
7. Inputs without a BOM continue to produce the same results as before, and
   the accepted BOM behavior does not introduce panics for empty, whitespace,
   or malformed input.

## Risks

- If BOM removal is implemented by replacing the input rather than adjusting
  offsets, `Result.Index` and the documented zero-copy byte slicing pattern
  can point at the wrong bytes; callers may read corrupted values or panic
  while slicing. Original-input offsets are therefore a compatibility
  dependency.
- `GetBytes` relies on string/byte result ownership and aliasing details. A
  normalization path that invalidates those assumptions could produce stale
  `Raw` or `Str` data, unexpected allocations, or unsafe memory behavior even
  when lookup values are correct.
- If only one parser entry point recognizes the marker, modifiers, path
  chaining, validation, and JSON Lines may disagree about the same document;
  this would leave BOM-bearing responses intermittently unusable.
- The contract assumes the source is UTF-8 and places the BOM at the first
  three bytes. A source using another encoding or placing a marker after
  whitespace will remain unsupported and may still be reported invalid.

## Open questions

None.
