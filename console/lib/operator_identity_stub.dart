// The non-web half of operator_identity.dart's conditional export -- see
// that file's own doc comment. Selected whenever dart:js_interop is
// unavailable (the VM `flutter test` runs on, and any future non-web
// target): there is no browser localStorage to persist to, so this holds
// the name in an in-memory variable instead -- good enough for a single
// process's lifetime, and (deliberately) also the test seam widget tests
// use to control the operator-name-prompt flow without a real browser.
String? _name;

/// The operator name last recorded via [setOperatorName], or null if none
/// has been stored yet.
String? getOperatorName() => _name;

/// Records [name] as the current operator name.
void setOperatorName(String name) => _name = name;

/// Test-only: resets the in-memory name so a test can exercise the
/// first-time prompt. No equivalent exists in operator_identity_web.dart
/// -- a real browser's localStorage does not need in-process resetting
/// between test runs the way this stub's static field does.
void clearOperatorNameForTest() => _name = null;
