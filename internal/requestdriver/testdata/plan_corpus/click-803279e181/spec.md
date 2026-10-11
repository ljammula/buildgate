# Spec

## Problem

`click.style` accepts named colours, integer palette indices, and RGB tuples,
but it does not accept the `#rrggbb` strings commonly produced by design
tools and configuration files. Passing such a value for `fg` or `bg` is
rejected instead of producing the true-colour ANSI sequence that the
equivalent RGB tuple produces. Because `click.secho` delegates styling to
`click.style`, it has the same usability gap.

## Scope

The colour interpretation used by `click.style` must recognize a string that
starts with `#` and contains exactly six hexadecimal digits, accepting upper-
and lower-case digits. It must interpret the pairs as the red, green, and blue
components and use the same ANSI true-colour output as the equivalent
`(r, g, b)` tuple for both `fg` and `bg`. `click.secho` must consequently
accept the same form through its styling arguments. Existing named-colour,
integer, and tuple behaviour remains supported. The `click.style` docstring
must describe the new input form and its constraints, and the existing style
test area must gain regression coverage for the accepted and rejected forms.

## Non-goals

- Supporting shorthand `#rgb`, alpha-bearing values such as `#rrggbbaa`,
  bare six-digit strings such as `rrggbb`, `0x`-prefixed values, or other CSS
  colour syntaxes.
- Changing the set or interpretation of named colours, integer palette
  indices, RGB tuples, or ANSI true-colour sequences.
- Adding general configuration-file parsing, colour conversion, terminal
  capability detection, or a new public colour-parsing API.
- Extending hex-colour acceptance to Click APIs other than the styling inputs
  of `click.style` and `click.secho`.

## Affected services and packages

The Click Python package's terminal UI styling API is affected, specifically
the public `click.style` and `click.secho` functions and the shared colour
interpretation that generates their ANSI codes. No external service or
additional package is involved.

## Acceptance criteria

1. `click.style("x", fg="#ff8000")` returns exactly the same value as
   `click.style("x", fg=(255, 128, 0))`, including the ANSI foreground
   sequence `\x1b[38;2;255;128;0m`; `click.style("x", fg="#FF8000")`
   produces that same result.
2. `click.style("x", bg="#00aBcD")` returns exactly the same value as
   `click.style("x", bg=(0, 171, 205))`, including the ANSI background
   sequence `\x1b[48;2;0;171;205m`.
3. For both `fg` and `bg`, passing `"#fff"`, `"#gggggg"`, or `"ff8000"`
   to `click.style` raises `TypeError` with the corresponding existing
   unknown-colour message (`Unknown color '#fff'`, `Unknown color '#gggggg'`,
   or `Unknown color 'ff8000'`), and any other string that is neither a valid
   hex value nor a known colour name is rejected the same way.
4. Existing forms passed to `click.style` retain their current observable
   ANSI output: for example, `fg="red"` produces `\x1b[31m`, an integer such
   as `fg=255` produces the 256-colour sequence, and
   `fg=(0, 1, 2)` produces `\x1b[38;2;0;1;2m`; the equivalent `bg` forms keep
   their background offsets.
5. With colour output enabled, `click.secho` accepts six-digit hex strings in
   both `fg` and `bg` and writes the same styled ANSI output that
   `click.echo(click.style(...))` would write for the equivalent RGB tuple.
6. The `click.style` docstring explicitly documents `#rrggbb` as an accepted
   six-hex-digit RGB form for foreground and background colours, including
   that its output is true-colour ANSI output and that shorter or malformed
   strings are not part of the form.

## Risks

The required true-colour sequences depend on the same terminal support as
existing RGB tuples; terminals without true-colour support may render them
imperfectly, but they must not receive a different interpretation from the
tuple form. The parser must enforce the exact six-digit boundary, or malformed
strings could be silently accepted with incorrect colour components. The
explicit `TypeError` requirement for invalid strings is treated as normative;
if the repository's prior exception class differs, changing or retaining it
incorrectly could either break callers that catch the prior class or fail the
required rejection contract.

## Open questions

None.
