// Shared pieces of the oracle review surfaces (the request-level panel at
// oracle_review and the per-ticket panel at plan_review): the fetched-file
// store that decides what counts as "shown", and the file tile itself.
import 'package:flutter/material.dart';

import 'error_display.dart';
import 'models.dart';
import 'text_escape.dart';

/// Selectable text with hidden characters and invalid bytes made visible
/// (see text_escape.dart); the synthetic escapes are highlighted so they
/// cannot be mistaken for characters of the source.
class EscapedText extends StatelessWidget {
  const EscapedText(this.text, {this.style, super.key});

  final String text;
  final TextStyle? style;

  @override
  Widget build(BuildContext context) {
    final segments = segmentEscapes(text);
    if (!segments.any((s) => s.escaped)) {
      return SelectableText(text, style: style);
    }
    final colors = Theme.of(context).colorScheme;
    return SelectableText.rich(
      TextSpan(
        style: style,
        children: [
          for (final s in segments)
            TextSpan(
              text: s.text,
              style: s.escaped
                  ? TextStyle(
                      color: colors.onErrorContainer,
                      backgroundColor: colors.errorContainer,
                      fontWeight: FontWeight.bold,
                    )
                  : null,
            ),
        ],
      ),
    );
  }
}

/// Fetched oracle file contents, keyed by an opaque string, plus which tiles
/// are open. A file is "shown" only while its tile is open, its content is
/// loaded, and that content's hash equals the listing's -- so an approval
/// built from [shown] never carries a hash for bytes the operator did not see.
class OracleFileStore extends ChangeNotifier {
  final _contents = <String, OracleFileContent>{};
  final _errors = <String, Object>{};
  final _loading = <String>{};
  final _opened = <String>{};
  final _fetchers = <String, Future<OracleFileContent> Function()>{};

  OracleFileContent? content(String key) => _contents[key];
  Object? error(String key) => _errors[key];
  bool isOpen(String key) => _opened.contains(key);

  /// Reconciles with a fresh listing ([listed]: key -> listed sha256):
  /// forgets content whose hash moved or that is no longer listed, and
  /// re-fetches open files that lost their content. [fetcher] fetches one key.
  void sync(
    Map<String, String> listed,
    Future<OracleFileContent> Function(String key) fetcher,
  ) {
    _fetchers
      ..clear()
      ..addEntries(listed.keys.map((k) => MapEntry(k, () => fetcher(k))));
    _contents.removeWhere((k, c) => listed[k] != c.sha256);
    _opened.removeWhere((k) => !listed.containsKey(k));
    notifyListeners();
    for (final key in _opened.toList()) {
      if (!_contents.containsKey(key)) load(key);
    }
  }

  void setOpen(String key, bool open) {
    if (open) {
      _opened.add(key);
      if (!_contents.containsKey(key)) load(key);
    } else {
      _opened.remove(key);
    }
    notifyListeners();
  }

  /// Marks [key] open and drops its content so it is re-fetched (after an
  /// edit); call [load] once the listing is fresh.
  void reopen(String key) {
    _opened.add(key);
    _contents.remove(key);
  }

  Future<void> load(String key) async {
    final fetch = _fetchers[key];
    if (fetch == null || _loading.contains(key)) return;
    _loading.add(key);
    _errors.remove(key);
    notifyListeners();
    try {
      _contents[key] = await fetch();
    } on Object catch (error) {
      _errors[key] = error;
    } finally {
      _loading.remove(key);
      notifyListeners();
    }
  }

  /// key -> hash for every listed file that is currently shown.
  Map<String, String> shown(Map<String, String> listed) => {
    for (final e in listed.entries)
      if (_opened.contains(e.key) && _contents[e.key]?.sha256 == e.value)
        e.key: e.value,
  };
}

/// One file of an oracle directory: name, size, short hash, and (open) its
/// content in monospace, or [bodyBuilder]'s replacement for it.
class OracleFileTile extends StatelessWidget {
  const OracleFileTile({
    required this.keyId,
    required this.storeKey,
    required this.file,
    required this.store,
    required this.shown,
    this.bodyBuilder,
    super.key,
  });

  final String keyId;
  final String storeKey;
  final OracleFileInfo file;
  final OracleFileStore store;
  final bool shown;

  /// Replaces the default content view once content is loaded.
  final List<Widget> Function(OracleFileContent content)? bodyBuilder;

  @override
  Widget build(BuildContext context) {
    final content = store.content(storeKey);
    final error = store.error(storeKey);
    final sha = file.sha256;
    return ExpansionTile(
      key: ValueKey('oracle-file-$keyId'),
      tilePadding: EdgeInsets.zero,
      initiallyExpanded: store.isOpen(storeKey),
      onExpansionChanged: (open) => store.setOpen(storeKey, open),
      leading: Icon(
        shown ? Icons.check_circle : Icons.radio_button_unchecked,
        size: 18,
        key: ValueKey('oracle-file-shown-$keyId-$shown'),
      ),
      title: Text(escapeInvisible(file.name)),
      subtitle: Text(
        '${file.size} bytes · sha256 ${sha.length > 12 ? sha.substring(0, 12) : sha}',
      ),
      childrenPadding: const EdgeInsets.only(bottom: 8),
      expandedCrossAxisAlignment: CrossAxisAlignment.start,
      children: [
        if (error != null)
          ErrorCallout(error: error)
        else if (content == null)
          const LinearProgressIndicator()
        else if (bodyBuilder != null)
          ...bodyBuilder!(content)
        else
          OracleContentBox(keyId: keyId, content: content),
      ],
    );
  }
}

/// The bordered monospace content view (or an "(empty file)" marker).
class OracleContentBox extends StatelessWidget {
  const OracleContentBox({
    required this.keyId,
    required this.content,
    super.key,
  });

  final String keyId;
  final OracleFileContent content;

  @override
  Widget build(BuildContext context) => Container(
    key: ValueKey('oracle-content-$keyId'),
    width: double.infinity,
    padding: const EdgeInsets.all(12),
    decoration: BoxDecoration(
      border: Border.all(color: Theme.of(context).dividerColor),
      borderRadius: BorderRadius.circular(4),
    ),
    child: content.text.isEmpty
        ? const Text(
            '(empty file)',
            key: ValueKey('oracle-empty-file'),
            style: TextStyle(fontStyle: FontStyle.italic),
          )
        : EscapedText(
            content.text,
            style: const TextStyle(fontFamily: 'monospace'),
          ),
  );
}
