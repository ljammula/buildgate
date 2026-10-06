import 'dart:convert';

import 'package:console/api_client.dart';
import 'package:console/run_list_screen.dart';
import 'package:console/project_release_screen.dart';
import 'package:console/status.dart';
import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:http/http.dart' as http;
import 'package:http/testing.dart';

import 'fixtures.dart';

void main() {
  // "Silence is a bug" (progress-contract.md, 2026-09-18): a run list row
  // shows a red 'stalled' chip once a non-terminal run's progress feed has
  // gone quiet past the 5-minute threshold.
  testWidgets('run list row shows a stalled chip for a quiet run', (
    tester,
  ) async {
    final client = MockClient((request) async {
      // inProgressRunJson's created_at/updated_at are fixed at
      // 2026-08-26, with no last_progress_at -- always long past the
      // 5-minute threshold relative to whenever this test runs.
      return http.Response('[${inProgressRunJson.trim()}]', 200);
    });
    final api = RunApi(baseUrl: 'http://factory.test', client: client);

    await tester.pumpWidget(MaterialApp(home: RunListScreen(api: api)));
    await tester.pumpAndSettle();

    expect(find.byKey(const ValueKey('stalled-chip')), findsOneWidget);
  });

  // A run with a waiting_reason but recent progress shows the amber
  // waiting chip, not the stalled one -- the factory has already
  // explained the silence.
  testWidgets('run list row shows a waiting chip for a queued run', (
    tester,
  ) async {
    final client = MockClient((request) async {
      return http.Response(
        '[${waitingRunJson(waitingReason: 'behind 1 run(s) on foo/bar')}]',
        200,
      );
    });
    final api = RunApi(baseUrl: 'http://factory.test', client: client);

    await tester.pumpWidget(MaterialApp(home: RunListScreen(api: api)));
    await tester.pumpAndSettle();

    expect(find.byKey(const ValueKey('waiting-chip')), findsOneWidget);
    expect(find.byKey(const ValueKey('stalled-chip')), findsNothing);
    expect(find.textContaining('waiting: behind 1 run(s)'), findsOneWidget);
  });

  // A fresh, non-terminal run with no waiting_reason shows neither chip,
  // but does show its current stage and a "last activity" figure.
  testWidgets('run list row shows current stage and last activity', (
    tester,
  ) async {
    final client = MockClient((request) async {
      return http.Response('[${waitingRunJson(currentStage: 'build')}]', 200);
    });
    final api = RunApi(baseUrl: 'http://factory.test', client: client);

    await tester.pumpWidget(MaterialApp(home: RunListScreen(api: api)));
    await tester.pumpAndSettle();

    expect(find.byKey(const ValueKey('stalled-chip')), findsNothing);
    expect(find.byKey(const ValueKey('waiting-chip')), findsNothing);
    expect(find.textContaining('· build ·'), findsOneWidget);
    expect(find.textContaining('last activity'), findsOneWidget);
  });

  testWidgets('run list renders terminal and in-progress states', (
    tester,
  ) async {
    final client = MockClient((request) async {
      return http.Response(
        '[${acceptedRunJson.trim()},${haltedRunJson.trim()},'
        '${quarantinedRunJson.trim()},${inProgressRunJson.trim()}]',
        200,
      );
    });
    final api = RunApi(baseUrl: 'http://factory.test', client: client);

    await tester.pumpWidget(MaterialApp(home: RunListScreen(api: api)));
    await tester.pumpAndSettle();

    expect(find.text('ticket-accepted'), findsOneWidget);
    expect(find.text('Accepted'), findsOneWidget);
    expect(find.text('Halted'), findsOneWidget);
    expect(find.text('Quarantined'), findsOneWidget);
    expect(find.text('Slice running'), findsOneWidget);

    // Checked against the shared status.dart vocabulary, not a raw
    // color -- 'halted' is Status.needsHuman (amber), not a raw
    // Colors.orange, now that every chip in the console agrees on what
    // each state means.
    expect(
      tester
          .widget<StatusChip>(find.byKey(const ValueKey('state-accepted')))
          .status,
      Status.done,
    );
    expect(
      tester
          .widget<StatusChip>(find.byKey(const ValueKey('state-halted')))
          .status,
      Status.needsHuman,
    );
    expect(
      tester
          .widget<StatusChip>(find.byKey(const ValueKey('state-quarantined')))
          .status,
      Status.failed,
    );
    expect(
      tester
          .widget<StatusChip>(find.byKey(const ValueKey('state-slice_running')))
          .status,
      Status.working,
    );
  });

  // Proves a project's release/kill-switch surface is reachable from the
  // run list even when no run exists to derive a project id from — the
  // console gap ReleaseScreen alone left (CLAIMS.md, Phase 1).
  testWidgets('the project release button opens ProjectReleaseScreen', (
    tester,
  ) async {
    final client = MockClient((_) async => http.Response('[]', 200));
    final api = RunApi(baseUrl: 'http://factory.test', client: client);

    await tester.pumpWidget(MaterialApp(home: RunListScreen(api: api)));
    await tester.pumpAndSettle();

    await tester.tap(find.byKey(const ValueKey('project-release-button')));
    await tester.pumpAndSettle();

    expect(find.byType(ProjectReleaseScreen), findsOneWidget);
  });

  // Phase 4 (follow-along console): each row now shows the project and an
  // elapsed/duration figure derived from createdAt/updatedAt, not just the
  // ticket -- the run id stays as a secondary line.
  testWidgets('run list row shows project, elapsed time, and run id', (
    tester,
  ) async {
    final client = MockClient((request) async {
      // acceptedRunJson: created 2026-08-26T11:00:00Z, updated
      // 2026-08-26T11:04:00Z -- a terminal run's elapsed is that fixed
      // 4-minute duration to updatedAt, not "now".
      return http.Response('[${acceptedRunJson.trim()}]', 200);
    });
    final api = RunApi(baseUrl: 'http://factory.test', client: client);

    await tester.pumpWidget(MaterialApp(home: RunListScreen(api: api)));
    await tester.pumpAndSettle();

    expect(find.textContaining('/projects/app'), findsOneWidget);
    expect(find.textContaining('Elapsed 04:00'), findsOneWidget);
    expect(find.textContaining('Run ID: run-accepted'), findsOneWidget);
  });

  // Mirrors release_screen_test.dart's "a refresh failure surfaces a
  // stale-data warning, not silence": a transient GET /runs failure after
  // an earlier successful load must not blank the list or replace it with
  // an error screen -- the last successfully loaded runs are still
  // meaningful, and the failed refresh is surfaced alongside them instead
  // (found via adversarial review of PR #111, should-fix 3).
  testWidgets('a refresh failure surfaces a stale-data warning, not silence', (
    tester,
  ) async {
    var runsFetchCount = 0;
    final client = MockClient((request) async {
      if (request.url.path == '/requests') {
        return http.Response('[]', 200);
      }
      runsFetchCount += 1;
      if (runsFetchCount == 1) {
        return http.Response('[${acceptedRunJson.trim()}]', 200);
      }
      return http.Response('{"error":"factory is unreachable"}', 500);
    });
    final api = RunApi(baseUrl: 'http://factory.test', client: client);

    await tester.pumpWidget(MaterialApp(home: RunListScreen(api: api)));
    await tester.pumpAndSettle();

    expect(find.byKey(const ValueKey('run-list-stale-banner')), findsNothing);
    expect(find.text('ticket-accepted'), findsOneWidget);

    await tester.tap(find.byKey(const ValueKey('refresh-button')));
    await tester.pumpAndSettle();

    expect(runsFetchCount, 2);
    // The stale run must still be showing -- a failed refresh must never
    // blank data that was already successfully loaded.
    expect(find.text('ticket-accepted'), findsOneWidget);
    expect(find.byKey(const ValueKey('run-list-stale-banner')), findsOneWidget);
    expect(find.textContaining('refresh failed'), findsOneWidget);
  });

  // A run row whose requestId resolves against GET /requests shows
  // the request's title as the primary line, with the ticket id kept as
  // a secondary line -- a run with no requestId (or one /requests didn't
  // resolve) keeps the plain ticket-id-as-title rendering.
  testWidgets('run list row shows the request title when requestId resolves', (
    tester,
  ) async {
    final runJson = jsonDecode(acceptedRunJson) as Map<String, dynamic>;
    runJson['request_id'] = 'request-1';
    final client = MockClient((request) async {
      if (request.url.path == '/requests') {
        return http.Response(
          jsonEncode([
            {
              'id': 'request-1',
              'workspace': '/workspaces/request-1',
              'project': 'app',
              'state': 'done',
              'submitted_at': '2026-08-26T10:00:00Z',
              'updated_at': '2026-08-26T11:04:00Z',
              'title': 'Add the widget',
            },
          ]),
          200,
        );
      }
      return http.Response('[${jsonEncode(runJson)}]', 200);
    });
    final api = RunApi(baseUrl: 'http://factory.test', client: client);

    await tester.pumpWidget(MaterialApp(home: RunListScreen(api: api)));
    await tester.pumpAndSettle();

    expect(find.text('Add the widget'), findsOneWidget);
    expect(find.text('Ticket ticket-accepted'), findsOneWidget);
  });
}
