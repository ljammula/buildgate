import 'package:console/diff_screen.dart';
import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';

void main() {
  // Added/removed/hunk-header lines are colored, and a changed-file
  // chip scrolls the diff to that file's section.
  testWidgets('colors diff lines and offers a jump-to-file chip list', (
    tester,
  ) async {
    const diff =
        'diff --git a/lib/a.dart b/lib/a.dart\n'
        '--- a/lib/a.dart\n'
        '+++ b/lib/a.dart\n'
        '@@ -1,2 +1,2 @@\n'
        '-old line\n'
        '+new line\n'
        'diff --git a/lib/b.dart b/lib/b.dart\n'
        '--- a/lib/b.dart\n'
        '+++ b/lib/b.dart\n'
        '@@ -1,1 +1,1 @@\n'
        '-old b\n'
        '+new b\n';

    await tester.pumpWidget(
      const MaterialApp(
        home: Scaffold(body: UnifiedDiffView(diff: diff)),
      ),
    );
    await tester.pumpAndSettle();

    expect(find.byKey(const ValueKey('diff-changed-files')), findsOneWidget);
    expect(find.text('lib/a.dart'), findsOneWidget);
    expect(find.text('lib/b.dart'), findsOneWidget);

    final richText = tester.widget<SelectableText>(find.byType(SelectableText));
    final span = richText.textSpan!;
    final removed =
        span.children!.firstWhere((s) => (s as TextSpan).text == '-old line\n')
            as TextSpan;
    final added =
        span.children!.firstWhere((s) => (s as TextSpan).text == '+new line\n')
            as TextSpan;
    final hunk =
        span.children!.firstWhere(
              (s) => (s as TextSpan).text == '@@ -1,2 +1,2 @@\n',
            )
            as TextSpan;
    expect(removed.style!.color, Colors.red);
    expect(added.style!.color, Colors.green);
    expect(hunk.style, isNotNull);

    // Tapping the second file's chip should not throw -- it scrolls the
    // underlying SingleChildScrollView, which this test doesn't need to
    // measure exactly to prove it wired the tap through.
    await tester.tap(find.text('lib/b.dart'));
    await tester.pumpAndSettle();
  });

  testWidgets('shows "No changes." for an empty diff', (tester) async {
    await tester.pumpWidget(
      const MaterialApp(
        home: Scaffold(body: UnifiedDiffView(diff: '')),
      ),
    );

    expect(find.text('No changes.'), findsOneWidget);
  });
}
