# Spec

## Problem

`matchAcceptEncoding` in `middleware/compress.go` currently uses substring matching when deciding whether a requested content coding is accepted. This makes a coding such as `b` appear to match `br`, and `gzip` appear to match `bgzip`. It also treats an entry such as `gzip;q=0` as an acceptable request for gzip, even though a zero quality value explicitly makes that coding unacceptable. Because `Compressor.selectEncoder` relies on this result, the middleware can select an encoder the client did not accept or select an encoder for a token that was never requested.

## Scope

The scope is the acceptance decision made by `matchAcceptEncoding` in `middleware/compress.go` and its effect on encoder selection:

- Treat the portion before the first semicolon in each accepted entry as the encoding token, trim surrounding whitespace, and compare that token exactly with the requested encoding. The comparison uses the normalized values already prepared by `Compressor.selectEncoder`.
- Inspect semicolon-delimited parameters on a matching entry so an explicit zero quality value is rejected. Valid zero-valued forms such as `q=0`, `q=0.0`, and `q=0.000` are not acceptable; an absent or positive quality value remains eligible.
- Preserve the existing configured encoder precedence for eligible encodings. A positive quality value does not reorder encoders.
- Ensure that `Compress` does not select an encoder, and therefore does not add a `Content-Encoding` for that encoder, when no eligible exact match remains.

## Non-goals

- Adding support for wildcard (`*`) Accept-Encoding negotiation or implementing complete quality-weight ranking.
- Changing encoder registration, encoder precedence, pooling, compression levels, supported content types, or response-writer behavior unrelated to the acceptance decision.
- Adding or changing compression algorithms, including Brotli or other encoders.
- Defining new handling for malformed Accept-Encoding fields or parameters beyond the exact-token and zero-quality behavior in scope.

## Affected services and packages

The affected package is `github.com/go-chi/chi/v5/middleware`. The relevant behavior is in `middleware/compress.go`, where `matchAcceptEncoding` is used by `Compressor.selectEncoder` and ultimately by `Compressor.Handler` and `Compress`.

## Acceptance criteria

1. `matchAcceptEncoding` in `middleware/compress.go` returns true for an exact accepted token with no zero quality value, including `[]string{"gzip"}` with encoding `"gzip"` and an entry with surrounding whitespace such as `[]string{" gzip ; q=0.5 "}`.
2. `matchAcceptEncoding` in `middleware/compress.go` returns false for substring-only matches, including `[]string{"br"}` with encoding `"b"` and `[]string{"bgzip"}` with encoding `"gzip"`.
3. `matchAcceptEncoding` in `middleware/compress.go` returns false when an exact matching entry specifies zero quality, including `[]string{"gzip;q=0"}`, `[]string{" gzip ; q=0 "}`, and valid equivalent zero-valued forms such as `[]string{"gzip;q=0.0"}`.
4. `matchAcceptEncoding` in `middleware/compress.go` treats an exact matching entry with a positive quality value, such as `q=0.5` or `q=1`, as eligible and does not confuse the quality parameter with part of the encoding name.
5. `Compressor.selectEncoder` in `middleware/compress.go` returns no encoder for an `Accept-Encoding` value of `gzip;q=0` when gzip is the only eligible configured encoder, and `Compress` leaves the response without `Content-Encoding` in that case.
6. `Compressor.selectEncoder` in `middleware/compress.go` skips a higher-precedence encoder explicitly marked `q=0` and selects another exact, eligible configured encoder; with the default encoders, `gzip;q=0, deflate` selects `deflate`.
7. `Compressor.selectEncoder` and `Compressor.Handler` in `middleware/compress.go` continue to select and apply gzip for an exact `Accept-Encoding: gzip` request and continue to select the configured encoder according to existing precedence when multiple exact eligible tokens are present.
8. A configured encoder whose name is only a substring of an accepted token is not selected by `Compressor.selectEncoder` in `middleware/compress.go`; for example, a configured `b` encoder is not selected for `Accept-Encoding: br`.

## Risks

- The matching contract depends on `Compressor.selectEncoder` continuing to lowercase the header values and on `SetEncoder` continuing to normalize encoder names. If either normalization changes independently, valid mixed-case requests may stop matching under exact comparison.
- The change intentionally keeps precedence-based selection rather than ranking encoders by positive q-values. If a caller expects `q=0.5` to outrank `q=0.1`, that expectation remains unsupported; only explicit zero-quality rejection is guaranteed here.
- Wildcard negotiation remains outside this change. If a client sends only `Accept-Encoding: *`, the middleware will continue to behave according to its existing named-token matching behavior, so assuming wildcard support would leave that client uncompressed.
- Treating a malformed quality parameter as zero could suppress compression unexpectedly, while treating an explicit zero as positive could violate the client's request. The specification therefore only assigns the new rejection behavior to syntactically recognizable zero-valued q parameters and leaves other malformed-field behavior unchanged.

## Open questions

None.
