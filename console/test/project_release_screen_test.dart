import 'package:console/api_client.dart';
import 'package:console/project_release_screen.dart';
import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:http/http.dart' as http;
import 'package:http/testing.dart';

import 'fixtures.dart';

void main() {
  // Proves the project-level release screen reaches a project's kill
  // switch by project id alone — no run needs to exist for that project —
  // closing the console gap ReleaseScreen (per-run only) left.
  testWidgets('entering a project id loads its kill-switch state and history', (
    tester,
  ) async {
    http.Request? fetched;
    final client = MockClient((request) async {
      fetched = request;
      return http.Response(projectReleaseJson, 200);
    });
    final api = RunApi(
      baseUrl: 'http://factory.test',
      client: client,
      authToken: 'control-token',
    );

    await tester.pumpWidget(MaterialApp(home: ProjectReleaseScreen(api: api)));
    await tester.enterText(
      find.byKey(const ValueKey('project-release-input')),
      'checkouts',
    );
    await tester.tap(find.byKey(const ValueKey('project-release-load-button')));
    await tester.pumpAndSettle();

    expect(
      fetched!.url.toString(),
      'http://factory.test/projects/checkouts/release',
    );
    expect(fetched!.headers['authorization'], 'Bearer control-token');

    expect(
      find.byKey(const ValueKey('project-kill-switch-engaged')),
      findsOneWidget,
    );
    expect(find.textContaining('By: operator@example.com'), findsOneWidget);
    expect(find.textContaining('Reason: incident 42'), findsOneWidget);
  });

  // Proves the screen offers no way to change the kill switch — same
  // CLI-only invariant ReleaseScreen enforces, tested for real rather than
  // left to a comment.
  testWidgets('the screen exposes no kill-switch control', (tester) async {
    final client = MockClient(
      (_) async => http.Response(projectReleaseJson, 200),
    );
    final api = RunApi(baseUrl: 'http://factory.test', client: client);

    await tester.pumpWidget(MaterialApp(home: ProjectReleaseScreen(api: api)));
    await tester.enterText(
      find.byKey(const ValueKey('project-release-input')),
      'checkouts',
    );
    await tester.tap(find.byKey(const ValueKey('project-release-load-button')));
    await tester.pumpAndSettle();

    expect(find.byType(Switch), findsNothing);
    expect(
      find.textContaining('deliberately not a console action'),
      findsOneWidget,
    );
  });

  // Regression for a real GitHub Codex App review finding: a failed
  // lookup for a *different* project used to leave the prior project's
  // kill-switch state on screen alongside the new error -- on this
  // safety-oriented screen, an operator could read project B's stale
  // "Engaged"/"Disengaged" chip (actually project A's) as B's own current
  // state. Proves loading project A successfully, then failing to load
  // project B, clears A's data rather than leaving it displayed under B's
  // error.
  testWidgets(
    'a failed lookup for a different project clears the prior project\'s data',
    (tester) async {
      var call = 0;
      final client = MockClient((_) async {
        call += 1;
        if (call == 1) return http.Response(projectReleaseJson, 200);
        return http.Response('{"error":"kill switch is unreadable"}', 500);
      });
      final api = RunApi(baseUrl: 'http://factory.test', client: client);

      await tester.pumpWidget(
        MaterialApp(home: ProjectReleaseScreen(api: api)),
      );
      await tester.enterText(
        find.byKey(const ValueKey('project-release-input')),
        'checkouts',
      );
      await tester.tap(
        find.byKey(const ValueKey('project-release-load-button')),
      );
      await tester.pumpAndSettle();
      expect(
        find.byKey(const ValueKey('project-kill-switch-engaged')),
        findsOneWidget,
      );

      await tester.enterText(
        find.byKey(const ValueKey('project-release-input')),
        'other-project',
      );
      await tester.tap(
        find.byKey(const ValueKey('project-release-load-button')),
      );
      await tester.pumpAndSettle();

      expect(
        find.byKey(const ValueKey('project-release-error')),
        findsOneWidget,
      );
      expect(find.text('Request failed (500)'), findsOneWidget);
      // checkouts's own engaged chip must be gone -- not still showing
      // under other-project's error.
      expect(
        find.byKey(const ValueKey('project-kill-switch-engaged')),
        findsNothing,
      );
      expect(
        find.byKey(const ValueKey('project-kill-switch-disengaged')),
        findsNothing,
      );
      expect(find.text('checkouts'), findsNothing);
    },
  );

  testWidgets('a failed lookup is reported, not shown as disengaged', (
    tester,
  ) async {
    final client = MockClient(
      (_) async => http.Response(
        '{"error":"project must be a single path component"}',
        400,
      ),
    );
    final api = RunApi(baseUrl: 'http://factory.test', client: client);

    await tester.pumpWidget(MaterialApp(home: ProjectReleaseScreen(api: api)));
    await tester.enterText(
      find.byKey(const ValueKey('project-release-input')),
      '../etc',
    );
    await tester.tap(find.byKey(const ValueKey('project-release-load-button')));
    await tester.pumpAndSettle();

    expect(find.byKey(const ValueKey('project-release-error')), findsOneWidget);
    expect(find.text('Request failed (400)'), findsOneWidget);
    expect(
      find.byKey(const ValueKey('project-kill-switch-disengaged')),
      findsNothing,
    );
  });
}
