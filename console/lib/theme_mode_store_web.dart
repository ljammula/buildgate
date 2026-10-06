// The web half of theme_mode_store.dart's conditional export -- see that
// file's own doc comment. Only ever selected once compiled for the web,
// where dart:js_interop actually reaches a real `window.localStorage`.
import 'dart:js_interop';
import 'dart:js_interop_unsafe';

import 'package:flutter/material.dart';

const _storageKey = 'factoryThemeMode';

/// The theme mode last recorded via [setStoredThemeMode], or null if none
/// has been stored in this browser yet (or the stored value is no longer
/// one of [ThemeMode]'s names).
ThemeMode? getStoredThemeMode() {
  final localStorage =
      globalContext.getProperty('localStorage'.toJS) as JSObject?;
  final value = localStorage?.callMethod('getItem'.toJS, _storageKey.toJS);
  if (value.isUndefinedOrNull) return null;
  return _parse((value as JSString).toDart);
}

/// Persists [mode] as this browser's theme mode choice.
void setStoredThemeMode(ThemeMode mode) {
  final localStorage =
      globalContext.getProperty('localStorage'.toJS) as JSObject?;
  localStorage?.callMethod('setItem'.toJS, _storageKey.toJS, mode.name.toJS);
}

ThemeMode? _parse(String value) {
  for (final mode in ThemeMode.values) {
    if (mode.name == value) return mode;
  }
  return null;
}
