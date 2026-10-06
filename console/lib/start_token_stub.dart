// The non-web half of start_token.dart's conditional export -- see that
// file's own doc comment. Selected whenever dart:js_interop is unavailable
// (the VM `flutter test` runs on, and any future non-web target): there is
// no browser location/localStorage to capture from, so
// [captureStartTokenFromLocation] is a no-op and storage is an in-memory
// variable instead -- good enough for a single process's lifetime, and
// (deliberately) also the test seam widget tests use to control the
// stored start token without a real browser.
String? _token;

/// The start token last recorded via [setStoredStartToken] or
/// [captureStartTokenFromLocation], or null if none has been stored yet.
String? getStoredStartToken() => _token;

/// Persists [token] as this browser's start token.
void setStoredStartToken(String token) => _token = token;

/// No-op off the web: there is no `window.location` to read here.
void captureStartTokenFromLocation() {}

/// Test-only: resets the in-memory token so a test can exercise the
/// no-stored-token case. No equivalent exists in start_token_web.dart -- a
/// real browser's localStorage does not need in-process resetting between
/// test runs the way this stub's static field does.
void clearStoredStartTokenForTest() => _token = null;
