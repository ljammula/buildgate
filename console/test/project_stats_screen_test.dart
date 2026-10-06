import 'dart:convert';

import 'package:console/api_client.dart';
import 'package:console/project_stats_screen.dart';
import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:http/http.dart' as http;
import 'package:http/testing.dart';

void main() {
  // Proves the screen reaches a project's acceptance-rate figures by
  // project id alone, and renders the override rate and cost the way a
  // team lead deciding whether to trust this factory with a real sprint
  // needs to see them.
  testWidgets('entering a project id loads its acceptance-rate figures', (
    tester,
  ) async {
    http.Request? fetched;
    final client = MockClient((request) async {
      fetched = request;
      return http.Response(
        jsonEncode({
          'project': 'checkouts',
          'total_runs': 5,
          'accepted': 4,
          'accepted_via_override': 1,
          'override_rate_percent': 25,
          'quarantined_by_cause': {'canonical_verify': 1},
          'halted': 0,
          'median_accepted_cost_micro_usd': 250000,
          'median_accepted_tokens': 478300,
        }),
        200,
      );
    });
    final api = RunApi(
      baseUrl: 'http://factory.test',
      client: client,
      authToken: 'control-token',
    );

    await tester.pumpWidget(MaterialApp(home: ProjectStatsScreen(api: api)));
    await tester.enterText(
      find.byKey(const ValueKey('project-stats-input')),
      'checkouts',
    );
    await tester.tap(find.byKey(const ValueKey('project-stats-load-button')));
    await tester.pumpAndSettle();

    expect(
      fetched!.url.toString(),
      'http://factory.test/projects/checkouts/stats',
    );
    expect(fetched!.headers['authorization'], 'Bearer control-token');

    expect(
      tester
          .widget<SelectableText>(
            find.byKey(const ValueKey('project-stats-override-rate')),
          )
          .data,
      '25% (1 of 4)',
    );
    expect(
      tester
          .widget<SelectableText>(
            find.byKey(const ValueKey('project-stats-median-cost')),
          )
          .data,
      '478.3k tokens',
    );
    expect(find.textContaining('canonical_verify'), findsOneWidget);
  });

  // The console renders tokens spent, never a dollar figure, for the
  // project's median accepted run -- even when the server's dollar fields
  // (kept for other API callers) are present alongside it.
  testWidgets(
    'renders the median accepted tokens even when the server also carries '
    'dollar fields',
    (tester) async {
      final client = MockClient(
        (_) async => http.Response(
          jsonEncode({
            'project': 'checkouts',
            'total_runs': 2,
            'accepted': 2,
            'accepted_via_override': 0,
            'override_rate_percent': 0,
            'halted': 0,
            'median_accepted_cost_micro_usd': 250000,
            'median_accepted_cost_subscription_billed': true,
            'median_accepted_tokens': 12300,
          }),
          200,
        ),
      );
      final api = RunApi(baseUrl: 'http://factory.test', client: client);

      await tester.pumpWidget(MaterialApp(home: ProjectStatsScreen(api: api)));
      await tester.enterText(
        find.byKey(const ValueKey('project-stats-input')),
        'checkouts',
      );
      await tester.tap(find.byKey(const ValueKey('project-stats-load-button')));
      await tester.pumpAndSettle();

      expect(
        tester
            .widget<SelectableText>(
              find.byKey(const ValueKey('project-stats-median-cost')),
            )
            .data,
        '12.3k tokens',
      );
    },
  );

  // Proves a project with no accepted runs renders "no data", not a
  // misleading 0%/\$0 -- the exact distinction ProjectStats' own doc
  // comment and JSON encoding (omitempty pointers) exist to preserve
  // through the wire, tested here at the render layer too.
  testWidgets('a project with no accepted runs shows no data, not 0%/\$0', (
    tester,
  ) async {
    final client = MockClient(
      (_) async => http.Response(
        jsonEncode({
          'project': 'brand-new',
          'total_runs': 1,
          'accepted': 0,
          'accepted_via_override': 0,
          'halted': 0,
        }),
        200,
      ),
    );
    final api = RunApi(baseUrl: 'http://factory.test', client: client);

    await tester.pumpWidget(MaterialApp(home: ProjectStatsScreen(api: api)));
    await tester.enterText(
      find.byKey(const ValueKey('project-stats-input')),
      'brand-new',
    );
    await tester.tap(find.byKey(const ValueKey('project-stats-load-button')));
    await tester.pumpAndSettle();

    expect(
      tester
          .widget<SelectableText>(
            find.byKey(const ValueKey('project-stats-override-rate')),
          )
          .data,
      'No accepted runs yet',
    );
    expect(
      tester
          .widget<SelectableText>(
            find.byKey(const ValueKey('project-stats-median-cost')),
          )
          .data,
      'No accepted runs yet',
    );
  });

  testWidgets('a failed lookup is reported, not shown as a stale success', (
    tester,
  ) async {
    final client = MockClient(
      (_) async => http.Response('{"error":"not found"}', 404),
    );
    final api = RunApi(baseUrl: 'http://factory.test', client: client);

    await tester.pumpWidget(MaterialApp(home: ProjectStatsScreen(api: api)));
    await tester.enterText(
      find.byKey(const ValueKey('project-stats-input')),
      'unknown-project',
    );
    await tester.tap(find.byKey(const ValueKey('project-stats-load-button')));
    await tester.pumpAndSettle();

    expect(find.byKey(const ValueKey('project-stats-error')), findsOneWidget);
    expect(
      find.byKey(const ValueKey('project-stats-override-rate')),
      findsNothing,
    );
  });
}
