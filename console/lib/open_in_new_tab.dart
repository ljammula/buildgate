// Opens a URL in a new browser tab (found via a console operator
// walkthrough -- the run detail screen's log-path and "Open in Temporal
// UI" links).
//
// The actual DOM-touching implementation lives in
// open_in_new_tab_web.dart, built on dart:js_interop -- which fails to
// *compile* (not merely misbehave at runtime) on a non-web target such as
// the VM `flutter test` runs on, so it cannot be imported unconditionally
// here. open_in_new_tab_stub.dart is the same public API as a no-op,
// selected instead whenever dart:js_interop is unavailable -- the same
// conditional-import shape tab_title.dart/theme_mode_store.dart already
// use for the identical reason (no platform-agnostic way to reach
// `window.open` at all).
export 'open_in_new_tab_stub.dart'
    if (dart.library.js_interop) 'open_in_new_tab_web.dart';
