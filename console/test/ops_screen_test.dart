import 'dart:convert';

import 'package:console/api_client.dart';
import 'package:console/ops_screen.dart';
import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:http/http.dart' as http;
import 'package:http/testing.dart';

void main() {
  // Proves the ops view aggregates every project's own stats and
  // kill-switch state on one screen, from GET /projects plus a
  // GET /projects/{project}/stats and GET /projects/{project}/release per
  // project -- the exact aggregation the fable adoption review's
  // recommendation 5 asked for, without a new backend concept.
  testWidgets(
    'lists every project with its quarantine breakdown and kill-switch state',
    (tester) async {
      final client = MockClient((request) async {
        if (request.url.path == '/projects') {
          return http.Response(
            jsonEncode([
              {
                'project_path': '/repo/checkouts',
                'project': 'checkouts',
                'workspace_path': '/repo/checkouts',
                'spec_path': '/repo/spec/spec.md',
                'run_count': 5,
                'last_run_at': '2026-09-08T10:00:00Z',
              },
            ]),
            200,
          );
        }
        if (request.url.path == '/projects/checkouts/stats') {
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
              'median_accepted_cost_subscription_billed': true,
              'median_accepted_tokens': 478300,
            }),
            200,
          );
        }
        if (request.url.path == '/projects/checkouts/release') {
          return http.Response(
            jsonEncode({
              'project': 'checkouts',
              'kill_switch': {
                'project': 'checkouts',
                'engaged': true,
                'history': [],
              },
            }),
            200,
          );
        }
        return http.Response('not found', 404);
      });
      final api = RunApi(baseUrl: 'http://factory.test', client: client);

      await tester.pumpWidget(MaterialApp(home: OpsScreen(api: api)));
      await tester.pumpAndSettle();

      expect(find.text('checkouts'), findsOneWidget);
      expect(find.textContaining('Accepted 4 / 5 runs'), findsOneWidget);
      expect(find.textContaining('canonical_verify (1)'), findsOneWidget);
      expect(
        find.textContaining('Median accepted: 478.3k tokens'),
        findsOneWidget,
      );
      expect(
        find.byKey(const ValueKey('ops-kill-switch-engaged-checkouts')),
        findsOneWidget,
      );
    },
  );

  testWidgets(
    'a project whose stats/release calls fail still renders, marked unavailable',
    (tester) async {
      final client = MockClient((request) async {
        if (request.url.path == '/projects') {
          return http.Response(
            jsonEncode([
              {
                'project_path': '/repo/broken',
                'project': 'broken',
                'workspace_path': '/repo/broken',
                'spec_path': '/repo/spec/spec.md',
                'run_count': 1,
                'last_run_at': '2026-09-08T10:00:00Z',
              },
            ]),
            200,
          );
        }
        return http.Response('{"error":"forbidden"}', 403);
      });
      final api = RunApi(baseUrl: 'http://factory.test', client: client);

      await tester.pumpWidget(MaterialApp(home: OpsScreen(api: api)));
      await tester.pumpAndSettle();

      expect(find.text('broken'), findsOneWidget);
      expect(find.text('Stats unavailable for this project.'), findsOneWidget);
      // Regression test for a real Codex review finding, PR #64: a failed
      // release fetch used to coerce to "not engaged", rendering an
      // unavailable kill switch as clear -- the wrong direction to fail
      // toward on an operations screen. It must render as unknown instead.
      expect(
        find.byKey(const ValueKey('ops-kill-switch-unknown-broken')),
        findsOneWidget,
      );
      expect(find.text('Kill switch unknown'), findsOneWidget);
    },
  );

  testWidgets('no projects recorded yet is reported plainly', (tester) async {
    final client = MockClient((_) async => http.Response('[]', 200));
    final api = RunApi(baseUrl: 'http://factory.test', client: client);

    await tester.pumpWidget(MaterialApp(home: OpsScreen(api: api)));
    await tester.pumpAndSettle();

    expect(find.text('No projects recorded yet.'), findsOneWidget);
  });
}
