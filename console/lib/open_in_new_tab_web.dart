// The web half of open_in_new_tab.dart's conditional export -- see that
// file's own doc comment. Only ever selected once compiled for the web,
// where dart:js_interop actually reaches a real `window.open`.
import 'dart:js_interop';
import 'dart:js_interop_unsafe';

/// Opens [url] in a new browser tab.
void openInNewTab(String url) {
  globalContext.callMethod('open'.toJS, url.toJS, '_blank'.toJS);
}
