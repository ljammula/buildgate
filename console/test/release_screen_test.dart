import 'dart:async';

import 'package:console/api_client.dart';
import 'package:console/elapsed.dart';
import 'package:console/release_screen.dart';
import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:http/http.dart' as http;
import 'package:http/testing.dart';

import 'fixtures.dart';

void main() {
  // Proves the release screen surfaces the durable audit fields of a denied
  // decision — the reasons included — plus the project kill switch's current
  // state and its attributable who/why/when history.
  testWidgets('denied decision shows reasons and kill-switch attribution', (
    tester,
  ) async {
    http.Request? fetched;
    final client = MockClient((request) async {
      fetched = request;
      return http.Response(deniedReleaseJson, 200);
    });
    final api = RunApi(
      baseUrl: 'http://factory.test',
      client: client,
      authToken: 'control-token',
    );

    await tester.pumpWidget(
      MaterialApp(
        home: ReleaseScreen(api: api, runId: 'run-accepted'),
      ),
    );
    await tester.pumpAndSettle();

    expect(
      fetched!.url.toString(),
      'http://factory.test/runs/run-accepted/release',
    );
    // The route is authenticated server-side (it exposes operator
    // attribution), so the client must actually present the credential.
    expect(fetched!.headers['authorization'], 'Bearer control-token');

    expect(
      find.byKey(const ValueKey('release-decision-denied')),
      findsOneWidget,
    );
    expect(
      find.byKey(const ValueKey('release-decision-allowed')),
      findsNothing,
    );
    expect(
      find.textContaining('kill switch is engaged for project "checkouts"'),
      findsOneWidget,
    );
    expect(find.byKey(const ValueKey('kill-switch-engaged')), findsOneWidget);
    expect(find.textContaining('By: operator@example.com'), findsOneWidget);
    expect(find.textContaining('Reason: incident 42'), findsOneWidget);
    expect(
      find.textContaining(
        'At: ${formatLocalTimestamp('2026-09-03T09:00:00Z')}',
      ),
      findsOneWidget,
    );
  });

  // Proves an allowed decision is presented as recorded evidence and not as
  // a pending action: nothing in this system merges, pushes, or deploys from
  // one, and the screen says so rather than leaving an operator to infer it.
  testWidgets('allowed decision states that nothing acts on it', (
    tester,
  ) async {
    final client = MockClient(
      (_) async => http.Response(allowedReleaseJson, 200),
    );
    final api = RunApi(baseUrl: 'http://factory.test', client: client);

    await tester.pumpWidget(
      MaterialApp(
        home: ReleaseScreen(api: api, runId: 'run-accepted'),
      ),
    );
    await tester.pumpAndSettle();

    expect(
      find.byKey(const ValueKey('release-decision-allowed')),
      findsOneWidget,
    );
    expect(
      find.textContaining('No merge, push, or deploy is performed'),
      findsOneWidget,
    );
    expect(
      find.byKey(const ValueKey('kill-switch-disengaged')),
      findsOneWidget,
    );
    expect(find.textContaining('Never engaged'), findsOneWidget);
  });

  // Proves the fail-closed presentation: a run with no recorded decision is
  // shown as having none, never as an allowed one.
  testWidgets('a run with no recorded decision is not shown as allowed', (
    tester,
  ) async {
    final client = MockClient(
      (_) async => http.Response(undecidedReleaseJson, 200),
    );
    final api = RunApi(baseUrl: 'http://factory.test', client: client);

    await tester.pumpWidget(
      MaterialApp(
        home: ReleaseScreen(api: api, runId: 'run-quarantined'),
      ),
    );
    await tester.pumpAndSettle();

    expect(find.byKey(const ValueKey('release-decision-none')), findsOneWidget);
    expect(
      find.byKey(const ValueKey('release-decision-allowed')),
      findsNothing,
    );
    expect(find.byKey(const ValueKey('release-decision-denied')), findsNothing);
    expect(
      find.textContaining('No release decision has been recorded'),
      findsOneWidget,
    );
  });

  // Proves the console distinguishes "recording failed" from "no decision
  // recorded": the run WAS evaluated, but the decision itself could not be
  // durably saved (e.g. a corrupted kill-switch.json), which must never
  // read the same as a run that was simply never evaluated.
  testWidgets(
    'a decision recording failure is shown distinctly from no decision',
    (tester) async {
      final client = MockClient(
        (_) async => http.Response(recordingFailedReleaseJson, 200),
      );
      final api = RunApi(baseUrl: 'http://factory.test', client: client);

      await tester.pumpWidget(
        MaterialApp(
          home: ReleaseScreen(api: api, runId: 'run-accepted'),
        ),
      );
      await tester.pumpAndSettle();

      expect(
        find.byKey(const ValueKey('release-decision-recording-failed')),
        findsOneWidget,
      );
      expect(find.byKey(const ValueKey('release-decision-none')), findsNothing);
      expect(
        find.byKey(const ValueKey('release-decision-allowed')),
        findsNothing,
      );
      expect(
        find.byKey(const ValueKey('release-decision-denied')),
        findsNothing,
      );
      expect(
        find.textContaining('release decision could not be recorded'),
        findsOneWidget,
      );
      expect(find.textContaining('factoryd retry'), findsOneWidget);
    },
  );

  // Proves the screen offers no way to change the kill switch — it is
  // CLI-only on purpose, so that reaching for it does not depend on a
  // healthy factoryd serve. This is a real invariant of the design, so it
  // gets a real test rather than a comment.
  testWidgets('the screen exposes no kill-switch control', (tester) async {
    final client = MockClient(
      (_) async => http.Response(deniedReleaseJson, 200),
    );
    final api = RunApi(baseUrl: 'http://factory.test', client: client);

    await tester.pumpWidget(
      MaterialApp(
        home: ReleaseScreen(api: api, runId: 'run-accepted'),
      ),
    );
    await tester.pumpAndSettle();

    expect(find.byType(ElevatedButton), findsNothing);
    expect(find.byType(FilledButton), findsNothing);
    expect(find.byType(OutlinedButton), findsNothing);
    expect(find.byType(TextButton), findsNothing);
    expect(find.byType(Switch), findsNothing);
    expect(find.textContaining('factoryd kill-switch'), findsOneWidget);
  });

  // Regression for a real Codex finding on this PR: _load previously ran
  // only once, from initState, so a screen left open across a run's
  // acceptance or a kill-switch transition kept showing whatever it first
  // loaded — including "No decision recorded" for a run that has since been
  // accepted, with no way to see the update short of navigating away and
  // back. Proves the refresh action re-fetches and the view actually
  // updates to reflect a decision that now exists.
  testWidgets('refresh reloads the release view after the decision changes', (
    tester,
  ) async {
    var fetchCount = 0;
    final client = MockClient((_) async {
      fetchCount += 1;
      final body = fetchCount == 1 ? undecidedReleaseJson : allowedReleaseJson;
      return http.Response(body, 200);
    });
    final api = RunApi(baseUrl: 'http://factory.test', client: client);

    await tester.pumpWidget(
      MaterialApp(
        home: ReleaseScreen(api: api, runId: 'run-accepted'),
      ),
    );
    await tester.pumpAndSettle();

    expect(fetchCount, 1);
    expect(find.byKey(const ValueKey('release-decision-none')), findsOneWidget);

    await tester.tap(find.byKey(const ValueKey('release-refresh-button')));
    await tester.pumpAndSettle();

    expect(fetchCount, 2);
    expect(
      find.byKey(const ValueKey('release-decision-allowed')),
      findsOneWidget,
    );
    expect(find.byKey(const ValueKey('release-decision-none')), findsNothing);
  });

  // Regression for a real Codex finding on this PR: a refresh failure after
  // an earlier successful load used to be silently discarded — _error was
  // set but the retained _release rendered with no indication anything had
  // gone wrong, so an operator refreshing right after a kill-switch
  // transition could keep seeing the old switch state as current with no
  // warning that the refresh itself had failed. Proves the retained data
  // stays visible (it is still meaningful recorded evidence) but a failed
  // refresh is surfaced plainly alongside it.
  testWidgets('a refresh failure surfaces a stale-data warning, not silence', (
    tester,
  ) async {
    var fetchCount = 0;
    final client = MockClient((_) async {
      fetchCount += 1;
      if (fetchCount == 1) return http.Response(deniedReleaseJson, 200);
      return http.Response('{"error":"factory is unreachable"}', 500);
    });
    final api = RunApi(baseUrl: 'http://factory.test', client: client);

    await tester.pumpWidget(
      MaterialApp(
        home: ReleaseScreen(api: api, runId: 'run-accepted'),
      ),
    );
    await tester.pumpAndSettle();

    expect(find.byKey(const ValueKey('release-stale-banner')), findsNothing);
    expect(
      find.byKey(const ValueKey('release-decision-denied')),
      findsOneWidget,
    );

    await tester.tap(find.byKey(const ValueKey('release-refresh-button')));
    await tester.pumpAndSettle();

    expect(fetchCount, 2);
    // The stale decision must still be showing -- a failed refresh must
    // never blank data that was already successfully loaded.
    expect(
      find.byKey(const ValueKey('release-decision-denied')),
      findsOneWidget,
    );
    expect(find.byKey(const ValueKey('release-stale-banner')), findsOneWidget);
    expect(find.textContaining('refresh failed'), findsOneWidget);
  });

  // Regression for a real Codex finding on this PR: the banner's Retry
  // action stayed enabled even while _loading was already true (unlike the
  // app-bar refresh button, which _loading already gated). A second tap
  // while the first retry was still in flight could start a concurrent
  // request; if responses ever completed out of order, an older snapshot
  // could overwrite a newer one and silently clear this stale-data warning.
  // Proves Retry disables for the duration of an in-flight request, the
  // same way the refresh button already does.
  testWidgets(
    'the stale-banner retry action disables while a request is in flight',
    (tester) async {
      var fetchCount = 0;
      final hold = Completer<void>();
      final client = MockClient((_) async {
        fetchCount += 1;
        if (fetchCount == 1) return http.Response(deniedReleaseJson, 200);
        if (fetchCount == 2) return http.Response('{"error":"boom"}', 500);
        // The retry tap below: block until the test explicitly lets it
        // through, so the button's disabled state can be observed mid-flight.
        await hold.future;
        return http.Response(deniedReleaseJson, 200);
      });
      final api = RunApi(baseUrl: 'http://factory.test', client: client);

      await tester.pumpWidget(
        MaterialApp(
          home: ReleaseScreen(api: api, runId: 'run-accepted'),
        ),
      );
      await tester.pumpAndSettle();
      await tester.tap(find.byKey(const ValueKey('release-refresh-button')));
      await tester.pumpAndSettle();
      expect(
        find.byKey(const ValueKey('release-stale-banner')),
        findsOneWidget,
      );

      await tester.tap(find.text('Retry'));
      await tester.pump();

      final retryButton = tester.widget<TextButton>(
        find.widgetWithText(TextButton, 'Retry'),
      );
      expect(
        retryButton.onPressed,
        isNull,
        reason:
            'Retry must disable itself while its own request is in '
            'flight, the same way the app-bar refresh button does',
      );

      hold.complete();
      await tester.pumpAndSettle();
      expect(fetchCount, 3);
    },
  );

  testWidgets('a failed release fetch is reported, not shown as allowed', (
    tester,
  ) async {
    final client = MockClient(
      (_) async =>
          http.Response('{"error":"release endpoint is not authorized"}', 403),
    );
    final api = RunApi(baseUrl: 'http://factory.test', client: client);

    await tester.pumpWidget(
      MaterialApp(
        home: ReleaseScreen(api: api, runId: 'run-accepted'),
      ),
    );
    await tester.pumpAndSettle();

    // GET /runs/{id}/release is start-token-gated -- a 403 here now
    // renders through ErrorCallout with startClass guidance (F:
    // serve-start-token) rather than a bare "Could not load release
    // decision: ..." line.
    expect(find.byKey(const ValueKey('release-error')), findsOneWidget);
    expect(find.text('Not authorized'), findsOneWidget);
    expect(find.textContaining('factoryd serve'), findsOneWidget);
    expect(
      find.byKey(const ValueKey('release-decision-allowed')),
      findsNothing,
    );
  });
}
