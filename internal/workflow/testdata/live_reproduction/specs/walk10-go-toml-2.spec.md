# go-toml: encoding a cyclic value returns an error

## Problem

`toml.Marshal` and `Encoder.Encode` follow pointers, maps, slices and interfaces
without remembering where they have been. Given a value that refers back to itself
(a struct whose pointer field points at an ancestor, a map that contains itself
through an `interface{}` value, a slice that contains itself), the encoder recurses
until the program runs out of stack or memory. A library must not crash its caller
on bad input; it should return an error.

## Wanted behaviour

- When the value being encoded contains a cycle, `Marshal` returns a nil byte slice
  and a non-nil error, and `Encoder.Encode` returns a non-nil error. Neither
  panics, hangs, or exhausts memory.
- The error message contains the word `cycle`.
- Cycles are detected whether they run through pointers to structs, maps (including
  a map stored in an `interface{}` value of itself), slices (including a
  `[]interface{}` that contains itself), or any mix of those, at any depth.
- Values that are shared but not cyclic still encode as before: the same pointer,
  map or slice reachable from two sibling fields or two array elements is written
  twice, with no error.
- Values whose types implement `encoding.TextMarshaler` or go-toml's own marshaler
  interface are encoded through that interface, as today; the encoder does not
  look inside them.
- Encoding values without cycles produces exactly the same output as today.

## Interface

No new exported names are required. The behaviour change is in
`toml.Marshal` and `(*toml.Encoder).Encode`. Document it in their doc comments.
