// Persists the operator's manual dark/light/system choice across
// reloads.
//
// The actual DOM-touching implementation lives in
// theme_mode_store_web.dart, built on dart:js_interop -- which fails to
// *compile* (not merely misbehave at runtime) on a non-web target such as
// the VM `flutter test` runs on, so it cannot be imported unconditionally
// here. theme_mode_store_stub.dart is the same public API backed by an
// in-memory variable, selected instead whenever dart:js_interop is
// unavailable -- the same conditional-import shape operator_identity.dart/
// tab_title.dart already use for the identical reason (no
// platform-agnostic way to reach `window.localStorage` at all).
export 'theme_mode_store_stub.dart'
    if (dart.library.js_interop) 'theme_mode_store_web.dart';
