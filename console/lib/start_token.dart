// Delivers `factoryd serve`'s per-process start token (Go: internal/api's
// startToken, generated when FACTORYD_API_START_TOKEN is unset -- see
// cmd/factoryd/serve_cmd.go's own doc comment) from the console URL
// fragment serve prints (`#t=<token>`) into this browser's localStorage,
// so New run/release/stats/ops work on a default install with no
// build-time API_START_TOKEN dart-define (F: serve-start-token, operator
// decision 2026-09-24 -- start-class routes stay token-gated, this is
// token DELIVERY, not relaxation). The fragment is deliberately never a
// query param -- see serve_cmd.go's console-link log line's own doc
// comment for why a fragment never reaches the server or an access log --
// and is stripped from the address bar immediately after capture so it
// never lingers in browser history or a shared/bookmarked link.
//
// The actual DOM-touching implementation lives in start_token_web.dart,
// built on dart:js_interop/dart:js_interop_unsafe -- both fail to
// *compile* (not merely misbehave at runtime) on a non-web target such as
// the VM `flutter test` runs on, so it cannot be imported unconditionally
// here. start_token_stub.dart is the same public API backed by an
// in-memory variable and a no-op capture, selected instead whenever
// dart:js_interop is unavailable -- the same conditional-import shape
// operator_identity.dart/theme_mode_store.dart/tab_title.dart already use
// for the identical reason (no platform-agnostic way to reach
// `window.location`/`window.localStorage`/`window.history` at all).
export 'start_token_stub.dart'
    if (dart.library.js_interop) 'start_token_web.dart';

/// Extracts the start token from a `window.location.hash`-shaped string
/// (leading `#` optional -- e.g. `#t=abc123` or `t=abc123`), or null if the
/// hash carries no `t=` component. Pure and platform-independent on
/// purpose -- see `captureStartTokenFromLocation` (the side-effecting
/// counterpart callers actually use, exported above) for what parses this
/// out of a real browser location and persists it.
///
/// Only the exact `t` key is recognized, matching `&`-joined components so
/// a future fragment component could be added alongside it without
/// breaking this parse (serve_cmd.go itself only ever emits `#t=<token>`
/// alone today). An empty token value (`#t=`) parses as "no token", the
/// same as a hash with no `t=` component at all -- there is nothing
/// meaningful to store either way.
String? parseStartTokenFromHash(String hash) {
  var value = hash;
  if (value.startsWith('#')) value = value.substring(1);
  if (value.isEmpty) return null;
  for (final part in value.split('&')) {
    final eq = part.indexOf('=');
    if (eq < 0) continue;
    if (part.substring(0, eq) != 't') continue;
    final token = Uri.decodeComponent(part.substring(eq + 1));
    return token.isEmpty ? null : token;
  }
  return null;
}
