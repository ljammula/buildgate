import 'package:flutter/material.dart';
import 'package:markdown/markdown.dart' as md;

/// Minimal Markdown renderer for spec/plan content.
///
/// `flutter_markdown` -- the obvious off-the-shelf widget for this -- was
/// discontinued as of this implementation (pub.dev, checked 2026-09-13:
/// "This project has been discontinued, and will not receive further
/// updates", with an author-suggested replacement,
/// `flutter_markdown_plus`, that this repo has never audited per its own
/// third-party-skill/dependency policy). Rather than depend on an
/// unmaintained widget package or an unvetted replacement, this renders
/// the block/inline subset spec/plan documents actually use directly from
/// `package:markdown`'s AST -- the Dart-team-maintained parser
/// `flutter_markdown` itself depended on, not a rendering widget, so this
/// stays a small, auditable Flutter tree-builder this repo owns.
///
/// Content is agent-authored and therefore untrusted, same as a run's
/// diff/log text elsewhere in this console:
/// - `package:markdown` is used with its default `ExtensionSet.none` (no
///   inline or block HTML support), so a raw HTML block/tag in the source
///   is never interpreted -- it renders as inert text via the `default:`
///   case below, never passed through to Flutter as markup.
/// - A link's target is shown as plain, non-interactive text alongside
///   its label rather than a tappable navigation -- links are disabled
///   by default, chosen over a `rel=noopener` explicit-click affordance
///   because that would need a URL-launch dependency this console does
///   not otherwise have.
class MarkdownView extends StatelessWidget {
  const MarkdownView({required this.data, super.key});

  final String data;

  @override
  Widget build(BuildContext context) {
    final theme = Theme.of(context);
    final nodes = md.Document(encodeHtml: false).parseLines(data.split('\n'));
    return Column(
      crossAxisAlignment: CrossAxisAlignment.start,
      children: [for (final node in nodes) ..._blockWidgets(node, theme)],
    );
  }
}

List<Widget> _blockWidgets(md.Node node, ThemeData theme) {
  if (node is md.Text) {
    // Stray top-level text (rare in a well-formed document) -- still
    // rendered, not dropped.
    return [_paragraph(node.text, theme)];
  }
  final element = node as md.Element;
  switch (element.tag) {
    case 'h1':
    case 'h2':
    case 'h3':
    case 'h4':
    case 'h5':
    case 'h6':
      final style = switch (element.tag) {
        'h1' => theme.textTheme.headlineSmall,
        'h2' => theme.textTheme.titleLarge,
        'h3' => theme.textTheme.titleMedium,
        _ => theme.textTheme.titleSmall,
      };
      return [
        Padding(
          padding: const EdgeInsets.symmetric(vertical: 6),
          child: _richText(element.children ?? const [], style, theme),
        ),
      ];
    case 'p':
      return [
        Padding(
          padding: const EdgeInsets.symmetric(vertical: 4),
          child: _richText(
            element.children ?? const [],
            theme.textTheme.bodyMedium,
            theme,
          ),
        ),
      ];
    case 'ul':
    case 'ol':
      final items = <Widget>[];
      var index = 1;
      for (final child in element.children ?? const []) {
        if (child is md.Element && child.tag == 'li') {
          final bullet = element.tag == 'ol' ? '${index++}.' : '•';
          items.add(
            Padding(
              padding: const EdgeInsets.only(bottom: 2),
              child: Row(
                crossAxisAlignment: CrossAxisAlignment.start,
                children: [
                  SizedBox(width: 24, child: Text(bullet)),
                  Expanded(
                    child: _richText(
                      child.children ?? const [],
                      theme.textTheme.bodyMedium,
                      theme,
                    ),
                  ),
                ],
              ),
            ),
          );
        }
      }
      return items;
    case 'pre':
      md.Element? codeChild;
      for (final child in element.children ?? const []) {
        if (child is md.Element && child.tag == 'code') {
          codeChild = child;
          break;
        }
      }
      final text = codeChild?.textContent ?? element.textContent;
      return [
        Container(
          width: double.infinity,
          margin: const EdgeInsets.symmetric(vertical: 4),
          padding: const EdgeInsets.all(8),
          decoration: BoxDecoration(
            color: theme.colorScheme.surfaceContainerHighest,
            borderRadius: BorderRadius.circular(4),
          ),
          child: SelectableText(
            text,
            style: const TextStyle(fontFamily: 'monospace', fontSize: 12),
          ),
        ),
      ];
    case 'blockquote':
      return [
        Container(
          margin: const EdgeInsets.symmetric(vertical: 4),
          padding: const EdgeInsets.only(left: 12),
          decoration: BoxDecoration(
            border: Border(
              left: BorderSide(color: theme.dividerColor, width: 3),
            ),
          ),
          child: Column(
            crossAxisAlignment: CrossAxisAlignment.start,
            children: [
              for (final child in element.children ?? const [])
                ..._blockWidgets(child, theme),
            ],
          ),
        ),
      ];
    case 'hr':
      return const [Divider()];
    default:
      // Unrecognized block (a table, a raw HTML block, ...) -- fall back
      // to its own plain text content rather than dropping it silently or
      // (per this file's own doc comment) interpreting it as markup.
      final text = element.textContent;
      return text.isEmpty ? const [] : [_paragraph(text, theme)];
  }
}

Widget _paragraph(String text, ThemeData theme) => Padding(
  padding: const EdgeInsets.symmetric(vertical: 4),
  child: Text(text, style: theme.textTheme.bodyMedium),
);

Widget _richText(List<md.Node> nodes, TextStyle? style, ThemeData theme) {
  return SelectableText.rich(
    TextSpan(
      style: style,
      children: [for (final node in nodes) ..._inlineSpans(node, theme)],
    ),
  );
}

List<InlineSpan> _inlineSpans(md.Node node, ThemeData theme) {
  if (node is md.Text) {
    return [TextSpan(text: node.text)];
  }
  final element = node as md.Element;
  switch (element.tag) {
    case 'em':
      return [
        TextSpan(
          style: const TextStyle(fontStyle: FontStyle.italic),
          children: [
            for (final child in element.children ?? const [])
              ..._inlineSpans(child, theme),
          ],
        ),
      ];
    case 'strong':
      return [
        TextSpan(
          style: const TextStyle(fontWeight: FontWeight.bold),
          children: [
            for (final child in element.children ?? const [])
              ..._inlineSpans(child, theme),
          ],
        ),
      ];
    case 'code':
      return [
        TextSpan(
          text: element.textContent,
          style: TextStyle(
            fontFamily: 'monospace',
            backgroundColor: theme.colorScheme.surfaceContainerHighest,
          ),
        ),
      ];
    case 'a':
      // Links are disabled by default -- see this file's own doc comment
      // for why: agent-authored content is untrusted, and this console
      // has no URL-launch dependency to gate an explicit-click
      // affordance behind, so a link renders as plain,
      // non-interactive styled text with its target visible rather than
      // a tappable navigation.
      final href = element.attributes['href'] ?? '';
      final label = element.textContent;
      return [
        TextSpan(
          text: href.isEmpty ? label : '$label ($href)',
          style: TextStyle(
            decoration: TextDecoration.underline,
            color: theme.colorScheme.primary,
          ),
        ),
      ];
    case 'br':
      return const [TextSpan(text: '\n')];
    default:
      return [
        for (final child in element.children ?? const [])
          ..._inlineSpans(child, theme),
      ];
  }
}
