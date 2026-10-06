// The web half of operator_identity.dart's conditional export -- see that
// file's own doc comment. Only ever selected once compiled for the web,
// where dart:js_interop actually reaches a real `window.localStorage`.
import 'dart:js_interop';
import 'dart:js_interop_unsafe';

const _storageKey = 'factoryOperatorName';

/// The operator name last recorded via [setOperatorName], or null if none
/// has been stored in this browser yet.
String? getOperatorName() {
  final localStorage =
      globalContext.getProperty('localStorage'.toJS) as JSObject?;
  final value = localStorage?.callMethod('getItem'.toJS, _storageKey.toJS);
  if (value.isUndefinedOrNull) return null;
  return (value as JSString).toDart;
}

/// Persists [name] as this browser's operator name.
void setOperatorName(String name) {
  final localStorage =
      globalContext.getProperty('localStorage'.toJS) as JSObject?;
  localStorage?.callMethod('setItem'.toJS, _storageKey.toJS, name.toJS);
}
