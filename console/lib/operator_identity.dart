// The one-time "operator name" prompt: recorded once per browser and
// sent as `by` on every approve/reject this console makes afterward.
//
// The actual DOM-touching implementation lives in
// operator_identity_web.dart, built on dart:js_interop -- which fails to
// *compile* (not merely misbehave at runtime) on a non-web target such as
// the VM `flutter test` runs on, so it cannot be imported unconditionally
// here. operator_identity_stub.dart is the same public API backed by an
// in-memory variable, selected instead whenever dart:js_interop is
// unavailable -- the same conditional-import shape tab_title.dart's own
// doc comment describes for the tab-title/favicon shim, which needs it
// for the identical reason (no platform-agnostic way to reach
// `window.localStorage`/`document` at all).
export 'operator_identity_stub.dart'
    if (dart.library.js_interop) 'operator_identity_web.dart';
