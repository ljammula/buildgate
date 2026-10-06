// The non-web half of open_in_new_tab.dart's conditional export -- see
// that file's own doc comment. Selected whenever dart:js_interop is
// unavailable (the VM `flutter test` runs on, and any future non-web
// target): there is no browser to open a tab in, so this is a deliberate
// no-op, not a placeholder pending an implementation.
void openInNewTab(String url) {}
