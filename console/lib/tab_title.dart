// The "needs you" ambient signal: the browser tab
// title and favicon, so an operator who has the board open in a
// background tab still notices without remembering to look at it.
//
// The actual DOM-touching implementation lives in tab_title_web.dart,
// built on dart:js_interop/dart:js_interop_unsafe -- both fail to *compile*
// (not merely misbehave at runtime) on a non-web target such as the VM
// `flutter test` runs on, so it cannot be imported unconditionally here.
// tab_title_stub.dart is the same public API as a set of no-ops, selected
// instead whenever dart:js_interop is unavailable -- the same
// conditional-import shape api_client.dart's own history describes
// removing an SSE-transport pair like this because streaming moved onto
// package:http instead; this shim needs it because there is no
// platform-agnostic way to reach `document`/`window` at all.
export 'tab_title_stub.dart' if (dart.library.js_interop) 'tab_title_web.dart';

/// The tab title for a board whose last successful poll saw
/// [needsHumanCount] requests waiting on a human, given whether the most
/// recent poll attempt ([pollFailed]) succeeded.
///
/// Pure and platform-independent on purpose -- see `updateNeedsHumanSignal`
/// (the side-effecting counterpart callers actually use, exported above)
/// for the regression this guards: a poll failure must show `(?)`, never
/// the last-known count (which could now be wrong) and never `0` (which
/// reads as "nothing needs you", the opposite of "the board doesn't
/// know").
String needsHumanTabTitle({
  required int? needsHumanCount,
  required bool pollFailed,
}) {
  if (pollFailed) return '(?) Factory Console';
  if (needsHumanCount != null && needsHumanCount > 0) {
    return '($needsHumanCount) Factory Console';
  }
  return 'Factory Console';
}
