import 'package:flutter/material.dart';

import 'api_client.dart';
import 'models.dart';

/// Shows the full unified diff between a run's BaseSHA and ResultSHA — the
/// plan's own "diff" console screen, giving an operator real visibility
/// into what a run actually changed, not just the file-count/insertion/
/// deletion summary RunDetailScreen's own evidence section already shows.
class DiffScreen extends StatefulWidget {
  const DiffScreen({required this.api, required this.runId, super.key});

  final RunApi api;
  final String runId;

  @override
  State<DiffScreen> createState() => _DiffScreenState();
}

class _DiffScreenState extends State<DiffScreen> {
  RunDiff? _diff;
  Object? _error;

  @override
  void initState() {
    super.initState();
    _load();
  }

  Future<void> _load() async {
    try {
      final diff = await widget.api.getRunDiff(widget.runId);
      if (mounted) setState(() => _diff = diff);
    } on Object catch (error) {
      if (mounted) setState(() => _error = error);
    }
  }

  @override
  Widget build(BuildContext context) {
    final diff = _diff;
    final error = _error;
    return Scaffold(
      appBar: AppBar(title: Text('Diff — ${widget.runId}')),
      body: switch ((diff, error)) {
        (null, final Object err) => Center(
          child: Text('Could not load diff: $err'),
        ),
        (null, null) => const Center(child: CircularProgressIndicator()),
        (final RunDiff value, _) => UnifiedDiffView(
          diff: value.diff,
          truncated: value.truncated,
        ),
      },
    );
  }
}

const _diffTextStyle = TextStyle(fontFamily: 'monospace', fontSize: 12);

/// The unified-diff text block this screen renders -- extracted so
/// request_detail_screen.dart's revision compare view can reuse the
/// identical rendering instead of duplicating it.
/// Deliberately does not size or scroll itself: a full-screen [Scaffold]
/// body (this screen's own use) bounds it implicitly, while an embedded
/// caller (a `ListView` child) needs to wrap it in its own bounded
/// height (e.g. a `SizedBox`) for the `Expanded` below to have a parent
/// that isn't itself unbounded.
///
/// Colours added lines green, removed lines red, and `@@` hunk headers a
/// muted secondary colour -- everything else (file headers,
/// context lines) stays the default text colour. A chip row above the
/// diff, one per changed file parsed out of the diff text's own
/// `diff --git a/X b/X` headers (falling back to `+++ b/X` when a diff
/// has no `diff --git` lines, e.g. a single-file compare), scrolls the
/// diff to that file's first line on tap.
class UnifiedDiffView extends StatefulWidget {
  const UnifiedDiffView({
    required this.diff,
    this.truncated = false,
    super.key,
  });

  final String diff;
  final bool truncated;

  @override
  State<UnifiedDiffView> createState() => _UnifiedDiffViewState();
}

class _UnifiedDiffViewState extends State<UnifiedDiffView> {
  final _scrollController = ScrollController();

  @override
  void dispose() {
    _scrollController.dispose();
    super.dispose();
  }

  // The vertical offset of diff line [index], estimated from one line's
  // rendered height under _diffTextStyle -- every line uses the same
  // monospace style, so a single TextPainter measurement stands in for
  // all of them rather than laying out the whole diff a second time just
  // to find a scroll target.
  double _offsetForLine(int index) {
    final painter = TextPainter(
      text: const TextSpan(text: 'M', style: _diffTextStyle),
      textDirection: TextDirection.ltr,
    )..layout();
    return index * painter.height;
  }

  void _jumpToLine(int index) {
    _scrollController.animateTo(
      _offsetForLine(index),
      duration: const Duration(milliseconds: 200),
      curve: Curves.easeInOut,
    );
  }

  @override
  Widget build(BuildContext context) {
    final diff = widget.diff;
    if (diff.isEmpty) {
      return const Center(child: Text('No changes.'));
    }
    final lines = diff.split('\n');
    final files = _changedFileLines(lines);
    return Column(
      children: [
        // Found via review: the server truncates a diff that exceeds its
        // own storage cap, and this client used to discard that flag
        // entirely — silently presenting an incomplete diff as if it were
        // the whole change.
        if (widget.truncated)
          Container(
            width: double.infinity,
            color: Theme.of(context).colorScheme.errorContainer,
            padding: const EdgeInsets.all(8),
            child: Text(
              'This diff was too large and has been truncated.',
              style: TextStyle(
                color: Theme.of(context).colorScheme.onErrorContainer,
              ),
            ),
          ),
        if (files.isNotEmpty)
          Padding(
            padding: const EdgeInsets.fromLTRB(16, 8, 16, 0),
            child: Wrap(
              key: const ValueKey('diff-changed-files'),
              spacing: 8,
              runSpacing: 4,
              children: [
                for (final file in files)
                  ActionChip(
                    label: Text(file.key),
                    onPressed: () => _jumpToLine(file.value),
                  ),
              ],
            ),
          ),
        Expanded(
          child: SingleChildScrollView(
            controller: _scrollController,
            padding: const EdgeInsets.all(16),
            child: SelectableText.rich(
              TextSpan(
                children: [for (final line in lines) _lineSpan(context, line)],
              ),
              style: _diffTextStyle,
            ),
          ),
        ),
      ],
    );
  }
}

TextSpan _lineSpan(BuildContext context, String line) {
  Color? color;
  if (line.startsWith('+') && !line.startsWith('+++')) {
    color = Colors.green;
  } else if (line.startsWith('-') && !line.startsWith('---')) {
    color = Colors.red;
  } else if (line.startsWith('@@')) {
    color = Theme.of(context).colorScheme.outline;
  }
  return TextSpan(
    text: '$line\n',
    style: color == null ? null : TextStyle(color: color),
  );
}

final _diffGitHeader = RegExp(r'^diff --git a/(.*) b/(.*)$');
final _plusPlusPlusHeader = RegExp(r'^\+\+\+ b/(.*)$');

/// Each changed file's name and the diff line index its section starts
/// at, in first-seen order -- `diff --git` headers preferred (present
/// for a multi-file git diff and unambiguous even for a rename), falling
/// back to `+++ b/X` headers when there are none.
List<MapEntry<String, int>> _changedFileLines(List<String> lines) {
  final fromGitHeader = <MapEntry<String, int>>[];
  for (var i = 0; i < lines.length; i++) {
    final match = _diffGitHeader.firstMatch(lines[i]);
    if (match != null) fromGitHeader.add(MapEntry(match.group(2)!, i));
  }
  if (fromGitHeader.isNotEmpty) return fromGitHeader;

  final fromPlusPlusPlus = <MapEntry<String, int>>[];
  for (var i = 0; i < lines.length; i++) {
    final match = _plusPlusPlusHeader.firstMatch(lines[i]);
    if (match != null) fromPlusPlusPlus.add(MapEntry(match.group(1)!, i));
  }
  return fromPlusPlusPlus;
}
