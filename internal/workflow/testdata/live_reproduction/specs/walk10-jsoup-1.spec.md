# jsoup: an opt-in limit on parser input size, with truncation reporting

## Problem

The `File`, `Path`, `InputStream`, `Reader` and `String` parsing APIs read their
input without a size limit, including data that is decompressed automatically from
gzip files. Applications that parse untrusted or compressed documents have no
simple way to bound the work, and if they cut the input themselves they cannot tell
afterwards whether the DOM they got is the whole document or a partial one.

Unlimited must stay the default: these APIs are expected to parse a caller-chosen
document completely.

## Wanted behaviour

- A `Parser` can be given a maximum input size, counted in characters of decoded
  input (after charset decoding and after gzip decompression). `0` means unlimited
  and is the default. A negative value is rejected with an
  `IllegalArgumentException`.
- When the limit is set, exactly the first that-many characters of input are parsed
  (all of the input when it is shorter), as if the input ended there, and reading
  stops soon after the limit: an input that never ends still finishes parsing. The
  result is a normal, well-formed `Document` built from that prefix (open elements
  are closed as at end of input).
- After a parse, the parser reports whether its input was truncated: `true` when
  the input was longer than the limit, `false` when the whole input was parsed
  (including when its length equals the limit exactly, and always when there is no
  limit). It reflects the most recent parse made with that `Parser`.
- The limit and the truncation report work the same for every entry point that
  takes a `Parser`: `Jsoup.parse(String, Parser)` and the overloads with a base
  URI, `Jsoup.parse(File|Path, charset, baseUri, Parser)`,
  `Jsoup.parse(InputStream, charset, baseUri, Parser)`, `Parser.parseInput(...)`
  with a `String` or `Reader`, and `StreamParser` built on that parser. For the HTML
  and XML parsers alike.
- A copy of a parser (`Parser.newInstance()`) keeps the configured limit.
- Parses with no limit behave exactly as today.
- `Connection` / HTTP responses are out of scope for this change.

## Interface

On `org.jsoup.parser.Parser`:

- `Parser setMaxInputSize(int maxChars)`, chainable like the other setters.
- `int getMaxInputSize()`.
- `boolean isInputTruncated()`.

Documented in the methods' Javadoc and in `CHANGES.md`.
