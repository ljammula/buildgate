import 'package:console/api_client.dart';
import 'package:console/operator_identity.dart';
import 'package:console/triage_screen.dart';
import 'package:flutter/material.dart';
import 'package:flutter/services.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:http/http.dart' as http;
import 'package:http/testing.dart';

import 'fixtures.dart';

void main() {
  setUp(() => setOperatorName('operator'));

  testWidgets('shows only needsHuman requests, oldest wait first', (
    tester,
  ) async {
    final client = MockClient((request) async {
      if (request.url.path == '/requests/req-old') {
        return http.Response(
          requestJson(
            id: 'req-old',
            state: 'spec_review',
            title: 'Old review',
            enteredAt: '2026-09-10T08:00:00Z',
            spec: '# Old spec',
          ),
          200,
        );
      }
      return http.Response(
        '[${requestJson(id: 'req-working', state: 'building', title: 'Building')},'
        '${requestJson(id: 'req-old', state: 'spec_review', title: 'Old review', enteredAt: '2026-09-10T08:00:00Z')},'
        '${requestJson(id: 'req-new', state: 'plan_review', title: 'New review', enteredAt: '2026-09-12T08:00:00Z')}]',
        200,
      );
    });
    final api = RunApi(baseUrl: 'http://factory.test', client: client);

    await tester.pumpWidget(MaterialApp(home: TriageScreen(api: api)));
    await tester.pumpAndSettle();

    expect(find.byKey(const ValueKey('triage-row-req-working')), findsNothing);
    expect(find.byKey(const ValueKey('triage-row-req-old')), findsOneWidget);
    expect(find.byKey(const ValueKey('triage-row-req-new')), findsOneWidget);
    // Oldest wait first -- the focused (first) row's detail pane shows
    // the older request's own title.
    expect(find.text('Old review'), findsWidgets);
    // The focused request's own artifact content, fetched separately
    // from the board list.
    expect(find.textContaining('# Old spec'), findsOneWidget);
  });

  testWidgets('never lists an oracle_review request (CLI-only until step 7)', (
    tester,
  ) async {
    final client = MockClient((request) async {
      return http.Response(
        '[${requestJson(id: 'req-oracle', state: 'oracle_review', title: 'Oracle')},'
        '${requestJson(id: 'req-spec', state: 'spec_review', title: 'Spec')}]',
        200,
      );
    });
    final api = RunApi(baseUrl: 'http://factory.test', client: client);

    await tester.pumpWidget(MaterialApp(home: TriageScreen(api: api)));
    await tester.pumpAndSettle();

    expect(find.byKey(const ValueKey('triage-row-req-oracle')), findsNothing);
    expect(find.byKey(const ValueKey('triage-row-req-spec')), findsOneWidget);
  });

  testWidgets('j/k move the focused row', (tester) async {
    final client = MockClient((request) async {
      if (request.url.path == '/requests/req-a') {
        return http.Response(
          requestJson(id: 'req-a', state: 'spec_review', title: 'First'),
          200,
        );
      }
      if (request.url.path == '/requests/req-b') {
        return http.Response(
          requestJson(id: 'req-b', state: 'spec_review', title: 'Second'),
          200,
        );
      }
      return http.Response(
        '[${requestJson(id: 'req-a', state: 'spec_review', title: 'First', enteredAt: '2026-09-10T08:00:00Z')},'
        '${requestJson(id: 'req-b', state: 'spec_review', title: 'Second', enteredAt: '2026-09-11T08:00:00Z')}]',
        200,
      );
    });
    final api = RunApi(baseUrl: 'http://factory.test', client: client);

    await tester.pumpWidget(MaterialApp(home: TriageScreen(api: api)));
    await tester.pumpAndSettle();

    expect(find.text('First'), findsWidgets);

    await tester.sendKeyEvent(LogicalKeyboardKey.keyJ);
    await tester.pumpAndSettle();

    expect(find.text('Second'), findsWidgets);

    await tester.sendKeyEvent(LogicalKeyboardKey.keyK);
    await tester.pumpAndSettle();

    expect(find.text('First'), findsWidgets);
  });

  testWidgets(
    'a/r approve/reject the focused request through the confirm flow',
    (tester) async {
      var approveCalled = false;
      final client = MockClient((request) async {
        if (request.method == 'POST' &&
            request.url.path == '/requests/req-a/approve') {
          approveCalled = true;
          return http.Response(
            requestJson(id: 'req-a', state: 'planning', title: 'First'),
            200,
          );
        }
        if (request.method == 'GET' && request.url.path == '/requests/req-a') {
          return http.Response(
            requestJson(id: 'req-a', state: 'spec_review', title: 'First'),
            200,
          );
        }
        return http.Response(
          '[${requestJson(id: 'req-a', state: 'spec_review', title: 'First')}]',
          200,
        );
      });
      final api = RunApi(
        baseUrl: 'http://factory.test',
        client: client,
        overrideToken: 'test-token',
      );

      await tester.pumpWidget(MaterialApp(home: TriageScreen(api: api)));
      await tester.pumpAndSettle();

      await tester.sendKeyEvent(LogicalKeyboardKey.keyA);
      await tester.pumpAndSettle();

      expect(
        find.byKey(const ValueKey('approve-confirm-button')),
        findsOneWidget,
      );
      await tester.tap(find.byKey(const ValueKey('approve-confirm-button')));
      await tester.pumpAndSettle();

      expect(approveCalled, isTrue);
    },
  );

  testWidgets('shows the ticket plan, not the already-approved spec, for a '
      'plan_review request (regression: content-presence selection always '
      'showed the old spec since it is already non-empty by plan_review)', (
    tester,
  ) async {
    final client = MockClient((request) async {
      if (request.url.path == '/requests/req-a') {
        return http.Response(
          requestJson(
            id: 'req-a',
            state: 'plan_review',
            title: 'First',
            spec: '# Already-approved spec',
            tickets: [
              requestTicketJson(index: 1, content: 'Ticket plan under review'),
            ],
          ),
          200,
        );
      }
      return http.Response(
        '[${requestJson(id: 'req-a', state: 'plan_review', title: 'First')}]',
        200,
      );
    });
    final api = RunApi(baseUrl: 'http://factory.test', client: client);

    await tester.pumpWidget(MaterialApp(home: TriageScreen(api: api)));
    await tester.pumpAndSettle();

    final artifactFinder = find.byKey(
      const ValueKey('triage-artifact-content'),
    );
    expect(
      find.descendant(
        of: artifactFinder,
        matching: find.textContaining('Ticket plan under review'),
      ),
      findsOneWidget,
    );
    expect(
      find.descendant(
        of: artifactFinder,
        matching: find.textContaining('Already-approved spec'),
      ),
      findsNothing,
    );
  });

  testWidgets('approve/reject actions are absent when no override token is '
      'configured', (tester) async {
    final client = MockClient((request) async {
      return http.Response(
        '[${requestJson(id: 'req-a', state: 'spec_review', title: 'First')}]',
        200,
      );
    });
    final api = RunApi(baseUrl: 'http://factory.test', client: client);

    await tester.pumpWidget(MaterialApp(home: TriageScreen(api: api)));
    await tester.pumpAndSettle();

    expect(find.byKey(const ValueKey('triage-approve-button')), findsNothing);
    expect(find.byKey(const ValueKey('triage-reject-button')), findsNothing);

    // The 'a' keyboard shortcut must also be a no-op, not merely the
    // button hidden -- pressing it must not open the confirm sheet.
    await tester.sendKeyEvent(LogicalKeyboardKey.keyA);
    await tester.pumpAndSettle();
    expect(find.byKey(const ValueKey('approve-confirm-button')), findsNothing);
  });

  testWidgets('shows a message when nothing needs a human', (tester) async {
    final client = MockClient((request) async {
      return http.Response(
        '[${requestJson(id: 'req-a', state: 'building', title: 'Building')}]',
        200,
      );
    });
    final api = RunApi(baseUrl: 'http://factory.test', client: client);

    await tester.pumpWidget(MaterialApp(home: TriageScreen(api: api)));
    await tester.pumpAndSettle();

    expect(find.text('Nothing needs you right now.'), findsOneWidget);
  });

  testWidgets('excludes a halted request (regression: halted counts toward the '
      "board's needs-you badge via requestStageGroup, but this screen's "
      'a/r actions only work from spec_review/plan_review -- listing it '
      'here would offer an approve/reject the server refuses)', (tester) async {
    final client = MockClient((request) async {
      return http.Response(
        '[${requestJson(id: 'req-halted', state: 'halted', title: 'Halted')},'
        '${requestJson(id: 'req-a', state: 'spec_review', title: 'Needs review')}]',
        200,
      );
    });
    final api = RunApi(baseUrl: 'http://factory.test', client: client);

    await tester.pumpWidget(MaterialApp(home: TriageScreen(api: api)));
    await tester.pumpAndSettle();

    expect(find.byKey(const ValueKey('triage-row-req-halted')), findsNothing);
    expect(find.byKey(const ValueKey('triage-row-req-a')), findsOneWidget);
  });
}
