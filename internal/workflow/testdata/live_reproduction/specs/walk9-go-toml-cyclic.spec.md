# Spec

## Problem

The go-toml encoder recursively follows pointers, maps, slices, and interface
values while converting a Go value to TOML, but it does not remember the
references on the current traversal path. A self-referential struct, map, or
slice can therefore recurse until the process exhausts its stack or memory
instead of reporting invalid input. This is unsafe for callers of
`toml.Marshal` and `(*toml.Encoder).Encode`.

## Scope

This spec covers cycle handling for values encoded through `toml.Marshal` and
`(*toml.Encoder).Encode`. A cycle is a traversal path that reaches a pointer,
map, or slice already active on that path, including paths crossing structs,
arrays, interfaces, and any combination of those reference-bearing values at
any depth.

For cyclic input, the public encoding APIs return an error containing the word
`cycle`; `toml.Marshal` also returns a nil byte slice. The behavior applies to
cycles through pointers to structs, maps held in interface values, slices held
in interface values, and mixed reference graphs. Reference values shared by
separate acyclic branches remain ordinary repeated encoding inputs rather than
being treated as cycles.

Existing dispatch for `encoding.TextMarshaler` and go-toml's own marshaler
interface remains in force. Once a value is handled through either interface,
the encoder treats it as opaque and does not inspect references inside it for
structural cycle detection. Values without cycles retain their current TOML
output and encoding result. The public API surface gains no exported names.

## Non-goals

- Supporting serialization of cyclic object graphs or preserving Go object
  identity in TOML output.
- Deduplicating shared pointers, maps, or slices; each acyclic occurrence is
  still encoded as it is today.
- Changing decoding, TOML syntax, encoder options, marshaler interface
  contracts, or the enablement rules for go-toml's marshaler interface.
- Inspecting or protecting against recursion created inside a user-provided
  `encoding.TextMarshaler` or go-toml marshaler implementation.
- Adding exported cycle-detection types, configuration, or error names.

## Affected services and packages

The affected component is the root `toml` package's encoding path, specifically
the public `toml.Marshal` function and `(*toml.Encoder).Encode` method and the
structural traversal they share. The behavior intersects with the standard
library `encoding.TextMarshaler` interface and the existing go-toml
`unstable.Marshaler` integration, but it does not change those interfaces or
any external service.

## Acceptance criteria

1. AC-1: When `toml.Marshal` or `(*toml.Encoder).Encode` encodes an acyclic Go value, the system shall produce exactly the same TOML bytes, writer output, and success or error result as the current behavior.
2. AC-2: When `toml.Marshal` receives a value containing a pointer cycle through a struct, including a pointer back to an ancestor at any nesting depth, the system shall return a nil byte slice and a non-nil error whose message contains the word `cycle`.
3. AC-3: When `(*toml.Encoder).Encode` receives a value containing a pointer cycle through a struct, including a pointer back to an ancestor at any nesting depth, the system shall return a non-nil error whose message contains the word `cycle` without reporting successful encoding.
4. AC-4: When `toml.Marshal` or `(*toml.Encoder).Encode` receives a map that reaches itself through an `interface{}` value, the system shall return a non-nil error whose message contains the word `cycle`, and `toml.Marshal` shall return a nil byte slice.
5. AC-5: When `toml.Marshal` or `(*toml.Encoder).Encode` receives a cyclic slice, including a `[]interface{}` element that refers to the same slice, the system shall return a non-nil error whose message contains the word `cycle`, and `toml.Marshal` shall return a nil byte slice.
6. AC-6: When `toml.Marshal` or `(*toml.Encoder).Encode` receives a value whose cycle combines pointers, maps, slices, and interfaces at any nesting depth, the system shall return the applicable non-nil error containing the word `cycle`, with `toml.Marshal` returning a nil byte slice.
7. AC-7: When `toml.Marshal` or `(*toml.Encoder).Encode` is called with a cyclic value involving pointers, maps, slices, interfaces, or any combination of them, the system shall complete by returning the required cycle error without panicking, hanging, or exhausting the caller's stack or memory.
8. AC-8: When `toml.Marshal` or `(*toml.Encoder).Encode` encodes an acyclic value in which the same pointer, map, or slice is reachable from two sibling fields or two array elements, the system shall encode both occurrences as before, without a cycle error or identity-based deduplication.
9. AC-9: When `toml.Marshal` or `(*toml.Encoder).Encode` encounters a value handled by `encoding.TextMarshaler`, the system shall preserve the existing text-marshaling output and shall not inspect references reachable only through that interface for structural cycle detection.
10. AC-10: When `(*toml.Encoder).Encode` has enabled go-toml's marshaler interface through `Encoder.EnableMarshalerInterface` and encounters an `unstable.Marshaler`, the system shall preserve the existing raw-TOML marshaling output and shall not inspect references reachable only through that interface for structural cycle detection.
11. AC-11: Where the API documentation for `toml.Marshal` and `(*toml.Encoder).Encode` is read, their doc comments shall state that cyclic input returns an error, that `toml.Marshal` returns a nil byte slice for that input, and that the cycle error contains the word `cycle`.

## Risks

- Cycle detection depends on tracking only references active on the current
  traversal path. If all previously seen references are treated as cycles,
  acyclic shared values will fail or be silently omitted, violating AC-8.
- Pointer, map, and slice identity must be represented consistently across
  interfaces and nested containers. If identity is missed, a cycle can still
  recurse; if unrelated values collide, valid input can receive a false cycle
  error.
- Marshaler dispatch must remain an opaque boundary. If structural traversal
  runs before the existing `encoding.TextMarshaler` or enabled
  `unstable.Marshaler` handling, custom values can change output or be rejected
  despite AC-9 and AC-10.
- Encoding state is reused internally. If active-cycle state is not cleared
  after both success and error paths, a later independent call can inherit a
  false cycle and change behavior for unrelated input.
- A user marshaler can recurse internally without the encoder seeing its
  references. If that behavior is assumed to be covered by this change, the
  encoder cannot guarantee the requested protection at the interface boundary;
  such recursion remains the marshaler implementation's responsibility.

## Open questions

None.
