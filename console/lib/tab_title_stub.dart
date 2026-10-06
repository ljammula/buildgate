// The non-web half of tab_title.dart's conditional export -- see that
// file's own doc comment. Selected whenever dart:js_interop is
// unavailable (the VM `flutter test` runs on, and any future non-web
// target): there is no DOM to update, so this is a deliberate no-op, not
// a placeholder pending an implementation.
void updateNeedsHumanSignal({
  required int? needsHumanCount,
  required bool pollFailed,
}) {}
