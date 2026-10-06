import 'package:console/markdown_view.dart';
import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';

void main() {
  testWidgets('renders headings, paragraphs, and lists without their '
      'markdown syntax', (tester) async {
    await tester.pumpWidget(
      const MaterialApp(
        home: Scaffold(
          body: MarkdownView(data: '# Title\n\nA paragraph.\n\n- one\n- two\n'),
        ),
      ),
    );

    expect(find.text('Title'), findsOneWidget);
    expect(find.textContaining('# Title'), findsNothing);
    expect(find.text('A paragraph.'), findsOneWidget);
    expect(find.textContaining('one'), findsOneWidget);
    expect(find.textContaining('two'), findsOneWidget);
  });

  // Agent-authored content is untrusted -- a link
  // must never become a tappable navigation (this console has no
  // URL-launch dependency to gate an explicit-click affordance behind),
  // it renders as plain text with its target visible instead.
  testWidgets('renders a link as plain, non-interactive text with its '
      'target visible, never a tappable navigation', (tester) async {
    await tester.pumpWidget(
      const MaterialApp(
        home: Scaffold(
          body: MarkdownView(data: '[see the spec](https://example.com/x)'),
        ),
      ),
    );

    expect(find.textContaining('see the spec'), findsOneWidget);
    expect(find.textContaining('https://example.com/x'), findsOneWidget);
    expect(find.byType(GestureDetector), findsNothing);
    expect(find.byType(InkWell), findsNothing);
  });

  // No raw-HTML passthrough: a raw HTML block in
  // the source must never be interpreted as markup -- it renders as
  // inert text (or is otherwise not turned into live widgets), the same
  // trust boundary this console applies to a run's diff/log text.
  testWidgets('never interprets raw HTML as markup', (tester) async {
    await tester.pumpWidget(
      const MaterialApp(
        home: Scaffold(
          body: MarkdownView(
            data: '<script>window.x = 1</script>\n\nSafe paragraph.\n',
          ),
        ),
      ),
    );

    expect(tester.takeException(), isNull);
    expect(find.text('Safe paragraph.'), findsOneWidget);
  });

  testWidgets('renders a fenced code block as plain monospace text', (
    tester,
  ) async {
    await tester.pumpWidget(
      const MaterialApp(
        home: Scaffold(body: MarkdownView(data: '```\nmake verify\n```\n')),
      ),
    );

    expect(find.textContaining('make verify'), findsOneWidget);
  });
}
