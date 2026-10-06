// The non-web half of theme_mode_store.dart's conditional export -- see
// that file's own doc comment. Selected whenever dart:js_interop is
// unavailable (the VM `flutter test` runs on, and any future non-web
// target): there is no browser localStorage to persist to, so this holds
// the choice in an in-memory variable instead -- good enough for a single
// process's lifetime, and (deliberately) also the test seam widget tests
// use to control the stored theme mode without a real browser.
import 'package:flutter/material.dart';

ThemeMode? _mode;

/// The theme mode last recorded via [setStoredThemeMode], or null if none
/// has been stored yet.
ThemeMode? getStoredThemeMode() => _mode;

/// Records [mode] as the current stored theme mode.
void setStoredThemeMode(ThemeMode mode) => _mode = mode;

/// Test-only: resets the in-memory value so a test can exercise the
/// no-stored-preference case. No equivalent exists in
/// theme_mode_store_web.dart -- a real browser's localStorage does not
/// need in-process resetting between test runs the way this stub's
/// static field does.
void clearStoredThemeModeForTest() => _mode = null;
