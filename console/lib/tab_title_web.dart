// The web half of tab_title.dart's conditional export -- see that file's
// own doc comment. Only ever selected once compiled for the web, where
// dart:js_interop/dart:js_interop_unsafe actually reach a real
// `document`/`window`.
import 'dart:js_interop';
import 'dart:js_interop_unsafe';

import 'tab_title.dart' show needsHumanTabTitle;

/// Updates the browser tab title and favicon badge for the current
/// needs-human count. [needsHumanCount] is the last *successfully* polled
/// count (null before the first successful load); [pollFailed] reflects
/// only the most recent poll attempt. The favicon badge tracks the last
/// known good count regardless of [pollFailed] -- a stale-but-still-shown
/// count on the favicon is the same "don't blank on transient failure"
/// choice the rest of this console already makes for board/run data, not
/// a gap; the tab *title*'s own `(?)` is what tells the operator the most
/// recent poll didn't succeed.
void updateNeedsHumanSignal({
  required int? needsHumanCount,
  required bool pollFailed,
}) {
  _setTitle(
    needsHumanTabTitle(
      needsHumanCount: needsHumanCount,
      pollFailed: pollFailed,
    ),
  );
  _setFaviconBadge(needsHumanCount);
}

void _setTitle(String title) {
  final document = globalContext.getProperty('document'.toJS) as JSObject?;
  document?.setProperty('title'.toJS, title.toJS);
}

/// Calls `window.__factoryFavicon.setBadge(count)` -- a small helper
/// defined in `web/index.html` that draws [count] onto a copy of the
/// static favicon and swaps the page's `<link rel=icon>` to it (or
/// restores the plain favicon for `count == null`/`0`). Drawing on a
/// canvas is native browser JS; reimplementing that in Dart image code
/// for a console-only badge is not worth the weight, so this file stays a
/// thin call-through: a small dart:js_interop shim.
void _setFaviconBadge(int? count) {
  final favicon = globalContext.getProperty('__factoryFavicon'.toJS);
  if (favicon.isUndefinedOrNull) return;
  (favicon as JSObject).callMethod('setBadge'.toJS, (count ?? 0).toJS);
}
