// The web half of start_token.dart's conditional export -- see that
// file's own doc comment. Only ever selected once compiled for the web,
// where dart:js_interop actually reaches a real `window.location`/
// `window.localStorage`/`window.history`.
import 'dart:js_interop';
import 'dart:js_interop_unsafe';

import 'start_token.dart' show parseStartTokenFromHash;

const _storageKey = 'factoryStartToken';

/// The start token last recorded via [setStoredStartToken] or
/// [captureStartTokenFromLocation], or null if none has been stored in
/// this browser yet.
String? getStoredStartToken() {
  final localStorage =
      globalContext.getProperty('localStorage'.toJS) as JSObject?;
  final value = localStorage?.callMethod('getItem'.toJS, _storageKey.toJS);
  if (value.isUndefinedOrNull) return null;
  return (value as JSString).toDart;
}

/// Persists [token] as this browser's start token.
void setStoredStartToken(String token) {
  final localStorage =
      globalContext.getProperty('localStorage'.toJS) as JSObject?;
  localStorage?.callMethod('setItem'.toJS, _storageKey.toJS, token.toJS);
}

/// Reads `window.location.hash` for the `#t=<token>` component
/// `factoryd serve`'s own printed console link carries (F: serve-start-
/// token), stores it via [setStoredStartToken] when present, and strips it
/// from the address bar with `history.replaceState` -- keeping the rest of
/// the URL (path + query) untouched -- so the token never lingers in
/// browser history, a shared/bookmarked link, or a screenshot of the
/// address bar. A no-op when the hash carries no `t=` component (an
/// ordinary reload of an already-captured console, or a build that never
/// got the tokenized link at all -- see parseStartTokenFromHash's own doc
/// comment for the exact parse).
void captureStartTokenFromLocation() {
  final location = globalContext.getProperty('location'.toJS) as JSObject?;
  final hashValue = location?.getProperty('hash'.toJS);
  if (hashValue == null || hashValue.isUndefinedOrNull) return;
  final token = parseStartTokenFromHash((hashValue as JSString).toDart);
  if (token == null) return;
  setStoredStartToken(token);

  // history.replaceState(state, title, url) takes a nullable `state`,
  // which callMethod (unlike callMethodVarArgs) cannot pass -- see that
  // extension's own doc comment on dart:js_interop_unsafe.
  final history = globalContext.getProperty('history'.toJS) as JSObject?;
  final pathname =
      (location?.getProperty('pathname'.toJS) as JSString?)?.toDart ?? '';
  final search =
      (location?.getProperty('search'.toJS) as JSString?)?.toDart ?? '';
  history?.callMethodVarArgs('replaceState'.toJS, [
    null,
    ''.toJS,
    (pathname + search).toJS,
  ]);
}
