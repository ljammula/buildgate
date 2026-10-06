import 'dart:async';
import 'dart:convert';

import 'package:console/api_client.dart';
import 'package:console/main.dart';
import 'package:console/models.dart';
import 'package:console/request_list_screen.dart';
import 'package:console/theme_mode_store_stub.dart';
import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:http/http.dart' as http;
import 'package:http/testing.dart';

import 'fixtures.dart';

RequestSummary _summary({
  required String id,
  required String state,
  String updatedAt = '2026-09-10T09:00:00Z',
  String enteredAt = '2026-09-10T09:00:00Z',
  String waitingSince = '',
}) => RequestSummary(
  id: id,
  workspace: '/repos/app',
  project: 'app',
  state: state,
  submittedAt: '2026-09-10T08:00:00Z',
  updatedAt: updatedAt,
  enteredAt: enteredAt,
  waitingSince: waitingSince,
);

void main() {
  group('sortedRequests', () {
    test(
      'a review-state request sorts above a working one regardless of age',
      () {
        final working = _summary(
          id: 'working',
          state: 'building',
          updatedAt: '2026-09-10T12:00:00Z',
        );
        final review = _summary(
          id: 'review',
          state: 'spec_review',
          enteredAt: '2026-09-10T05:00:00Z', // much older than working
        );

        final sorted = sortedRequests([working, review]);

        expect(sorted.map((r) => r.id).toList(), ['review', 'working']);
      },
    );

    test('two review-state requests sort oldest-wait first', () {
      final newer = _summary(
        id: 'newer',
        state: 'plan_review',
        waitingSince: '2026-09-10T09:00:00Z',
      );
      final older = _summary(
        id: 'older',
        state: 'spec_review',
        waitingSince: '2026-09-10T07:00:00Z',
      );

      final sorted = sortedRequests([newer, older]);

      expect(sorted.map((r) => r.id).toList(), ['older', 'newer']);
    });

    test('working states sort by updatedAt descending', () {
      final stale = _summary(
        id: 'stale',
        state: 'building',
        updatedAt: '2026-09-10T08:00:00Z',
      );
      final fresh = _summary(
        id: 'fresh',
        state: 'planning',
        updatedAt: '2026-09-10T10:00:00Z',
      );

      final sorted = sortedRequests([stale, fresh]);

      expect(sorted.map((r) => r.id).toList(), ['fresh', 'stale']);
    });

    test('a resume_review request needs a human and sorts with review', () {
      expect(requestStageGroup('resume_review'), RequestStageGroup.review);
      final review = _summary(id: 'lost', state: 'resume_review');
      final working = _summary(id: 'working', state: 'building');

      expect(sortedRequests([working, review]).first.id, 'lost');
    });

    test('done and failed requests sort after review and working', () {
      final done = _summary(id: 'done', state: 'done');
      final failed = _summary(id: 'failed', state: 'quarantined');
      final review = _summary(id: 'review', state: 'spec_review');
      final working = _summary(id: 'working', state: 'building');

      final sorted = sortedRequests([done, failed, review, working]);

      expect(sorted.first.id, 'review');
      expect(sorted[1].id, 'working');
      expect(sorted.sublist(2).map((r) => r.id).toSet(), {'done', 'failed'});
    });
  });

  group('waitingBadgeLabel', () {
    test('renders with the age for a review state', () {
      final request = _summary(
        id: 'req-1',
        state: 'spec_review',
        waitingSince: '2026-09-10T09:00:00Z',
      );
      final now = DateTime.parse('2026-09-10T09:45:00Z');

      expect(waitingBadgeLabel(request, now), 'Waiting on you · 45m');
    });

    test('renders hours and minutes past an hour', () {
      final request = _summary(
        id: 'req-1',
        state: 'plan_review',
        waitingSince: '2026-09-10T09:00:00Z',
      );
      final now = DateTime.parse('2026-09-10T11:05:00Z');

      expect(waitingBadgeLabel(request, now), 'Waiting on you · 2h 5m');
    });

    test('is null outside a review state', () {
      final request = _summary(id: 'req-1', state: 'building');
      expect(waitingBadgeLabel(request, DateTime.now()), isNull);
    });
  });

  testWidgets('request list renders each stage group and the waiting badge', (
    tester,
  ) async {
    final now = DateTime.now().toUtc();
    final waitingSince = now
        .subtract(const Duration(minutes: 45))
        .toIso8601String();
    final client = MockClient((request) async {
      // Avoid the worker banner pushing the list's own content out
      // of this test's asserted layout -- answer /daemons with a live
      // worker rather than letting it fall through to the requests
      // list below (which parses tolerantly but has no 'queue-run' entry,
      // which would otherwise show the "not running" banner here).
      if (request.url.path == '/daemons') {
        return http.Response(
          jsonEncode([
            {'name': 'queue-run', 'alive': true},
          ]),
          200,
        );
      }
      return http.Response(
        '[${requestJson(id: 'req-review', state: 'spec_review', title: 'Add idempotency keys', waitingSince: waitingSince)},'
        '${requestJson(id: 'req-working', state: 'building', title: 'Retry logic', ticketIndex: 1, ticketCount: 2)},'
        '${requestJson(id: 'req-done', state: 'done', title: 'Done request')}]',
        200,
      );
    });
    final api = RunApi(baseUrl: 'http://factory.test', client: client);

    await tester.pumpWidget(FactoryConsole(api: api));
    await tester.pumpAndSettle();

    expect(find.text('Add idempotency keys'), findsOneWidget);
    expect(find.text('Retry logic'), findsOneWidget);
    expect(find.text('Done request'), findsOneWidget);
    expect(find.textContaining('Waiting on you'), findsOneWidget);
    expect(find.text('Ticket 1 / 2'), findsOneWidget);

    // Review sorts before working sorts before done. The row title is now
    // wrapped in a Tooltip (full title on hover) around the same
    // Text.
    final listTiles = tester
        .widgetList<ListTile>(find.byType(ListTile))
        .toList();
    String titleOf(ListTile tile) =>
        ((tile.title! as Tooltip).child! as Text).data!;
    expect(titleOf(listTiles[0]), 'Add idempotency keys');
    expect(titleOf(listTiles[1]), 'Retry logic');
    expect(titleOf(listTiles[2]), 'Done request');
  });

  testWidgets('a refresh failure surfaces a stale-data warning, not silence', (
    tester,
  ) async {
    // Counts only GET /requests polls -- separate from the
    // `/requests/events` SSE connection RequestListScreen now also opens
    // in the background, which this test isn't exercising and which a
    // shared-path-blind counter would otherwise conflate with the polls
    // this assertion actually cares about.
    var fetchCount = 0;
    final client = MockClient((request) async {
      if (request.url.path != '/requests') {
        return http.Response('', 200);
      }
      fetchCount += 1;
      if (fetchCount == 1) {
        return http.Response(
          '[${requestJson(id: 'req-1', state: 'spec_review', title: 'Add idempotency keys')}]',
          200,
        );
      }
      return http.Response('{"error":"factory is unreachable"}', 500);
    });
    final api = RunApi(baseUrl: 'http://factory.test', client: client);

    await tester.pumpWidget(FactoryConsole(api: api));
    await tester.pumpAndSettle();

    expect(
      find.byKey(const ValueKey('request-list-stale-banner')),
      findsNothing,
    );
    expect(find.text('Add idempotency keys'), findsOneWidget);

    await tester.tap(find.byKey(const ValueKey('request-list-refresh-button')));
    await tester.pumpAndSettle();

    expect(fetchCount, 2);
    expect(find.text('Add idempotency keys'), findsOneWidget);
    expect(
      find.byKey(const ValueKey('request-list-stale-banner')),
      findsOneWidget,
    );
  });

  testWidgets('the run list button opens RunListScreen', (tester) async {
    final client = MockClient((request) async {
      if (request.url.path == '/requests') return http.Response('[]', 200);
      return http.Response('[]', 200);
    });
    final api = RunApi(baseUrl: 'http://factory.test', client: client);

    await tester.pumpWidget(FactoryConsole(api: api));
    await tester.pumpAndSettle();

    await tester.tap(find.byKey(const ValueKey('run-list-button')));
    await tester.pumpAndSettle();

    expect(find.text('Factory runs'), findsOneWidget);
  });

  testWidgets('the triage button opens the /triage view and back returns to the '
      'board', (tester) async {
    final client = MockClient((request) async {
      if (request.url.path == '/requests') {
        return http.Response(
          '[${requestJson(id: 'req-a', state: 'spec_review', title: 'Needs review')}]',
          200,
        );
      }
      return http.Response('[]', 200);
    });
    final api = RunApi(baseUrl: 'http://factory.test', client: client);

    await tester.pumpWidget(FactoryConsole(api: api));
    await tester.pumpAndSettle();

    await tester.tap(find.byKey(const ValueKey('open-triage-button')));
    await tester.pumpAndSettle();

    expect(find.text('Triage'), findsOneWidget);
    expect(find.byKey(const ValueKey('triage-row-req-a')), findsOneWidget);

    await tester.tap(find.byKey(const ValueKey('triage-exit-button')));
    await tester.pumpAndSettle();

    expect(find.text('Requests'), findsOneWidget);
  });

  testWidgets('section headers group requests into Needs you / Working / Finished '
      'with counts', (tester) async {
    final client = MockClient((request) async {
      return http.Response(
        '[${requestJson(id: 'req-review', state: 'spec_review', title: 'Needs review')},'
        '${requestJson(id: 'req-working', state: 'building', title: 'In progress')},'
        '${requestJson(id: 'req-done', state: 'done', title: 'Wrapped up')},'
        '${requestJson(id: 'req-failed', state: 'quarantined', title: 'Blocked')}]',
        200,
      );
    });
    final api = RunApi(baseUrl: 'http://factory.test', client: client);

    await tester.pumpWidget(FactoryConsole(api: api));
    await tester.pumpAndSettle();

    expect(find.text('Needs you (1)'), findsOneWidget);
    expect(find.text('Working (1)'), findsOneWidget);
    // done + quarantined both collapse into "Finished" (a
    // three-header example, not a fifth "other" bucket).
    expect(find.text('Finished (2)'), findsOneWidget);
  });

  testWidgets(
    'a halted request lands in Needs you, not Finished, and counts toward '
    'the tab-title badge (regression: status.dart already maps halted to '
    "Status.needsHuman -- the board's own grouping used to disagree, "
    'putting it under Finished and excluding it from the count)',
    (tester) async {
      final client = MockClient((request) async {
        return http.Response(
          '[${requestJson(id: 'req-halted', state: 'halted', title: 'Needs a retry')},'
          '${requestJson(id: 'req-done', state: 'done', title: 'Wrapped up')}]',
          200,
        );
      });
      final api = RunApi(baseUrl: 'http://factory.test', client: client);

      await tester.pumpWidget(FactoryConsole(api: api));
      await tester.pumpAndSettle();

      expect(find.text('Needs you (1)'), findsOneWidget);
      expect(find.text('Finished (1)'), findsOneWidget);
      expect(find.text('Needs a retry'), findsOneWidget);
    },
  );

  testWidgets('a project filter chip hides requests from other projects', (
    tester,
  ) async {
    final client = MockClient((request) async {
      return http.Response(
        '[${requestJson(id: 'req-a', state: 'done', project: 'checkouts', title: 'Checkout fix')},'
        '${requestJson(id: 'req-b', state: 'done', project: 'billing', title: 'Billing fix')}]',
        200,
      );
    });
    final api = RunApi(baseUrl: 'http://factory.test', client: client);

    await tester.pumpWidget(FactoryConsole(api: api));
    await tester.pumpAndSettle();

    expect(find.text('Checkout fix'), findsOneWidget);
    expect(find.text('Billing fix'), findsOneWidget);

    await tester.tap(
      find.byKey(const ValueKey('request-filter-project-checkouts')),
    );
    await tester.pumpAndSettle();

    expect(find.text('Checkout fix'), findsOneWidget);
    expect(find.text('Billing fix'), findsNothing);
  });

  testWidgets('free-text search filters the board by id/title/workspace', (
    tester,
  ) async {
    final client = MockClient((request) async {
      return http.Response(
        '[${requestJson(id: 'req-a', state: 'done', title: 'Add idempotency keys')},'
        '${requestJson(id: 'req-b', state: 'done', title: 'Retry logic')}]',
        200,
      );
    });
    final api = RunApi(baseUrl: 'http://factory.test', client: client);

    await tester.pumpWidget(FactoryConsole(api: api));
    await tester.pumpAndSettle();

    await tester.enterText(
      find.byKey(const ValueKey('request-search-field')),
      'idempotency',
    );
    await tester.pumpAndSettle();

    expect(find.text('Add idempotency keys'), findsOneWidget);
    expect(find.text('Retry logic'), findsNothing);
  });

  testWidgets(
    'a building request with tickets shows the fan-out roll-up strip',
    (tester) async {
      final client = MockClient((request) async {
        return http.Response(
          '[${requestJson(
            id: 'req-a',
            state: 'building',
            title: 'Multi-ticket request',
            tickets: [
              requestTicketJson(index: 1, prState: 'merged'),
              requestTicketJson(index: 2, prState: 'changes_requested'),
              requestTicketJson(index: 3, runId: 'run-3'),
            ],
          )}]',
          200,
        );
      });
      final api = RunApi(baseUrl: 'http://factory.test', client: client);

      await tester.pumpWidget(FactoryConsole(api: api));
      await tester.pumpAndSettle();

      expect(find.text('1 done'), findsOneWidget);
      expect(find.text('1 changes requested'), findsOneWidget);
      expect(find.text('1 building'), findsOneWidget);
    },
  );

  testWidgets('the inline token figure renders — for missing evidence, never 0', (
    tester,
  ) async {
    final client = MockClient((request) async {
      return http.Response(
        '[${requestJson(id: 'req-a', state: 'spec_drafting', title: 'No evidence yet')}]',
        200,
      );
    });
    final api = RunApi(baseUrl: 'http://factory.test', client: client);

    await tester.pumpWidget(FactoryConsole(api: api));
    await tester.pumpAndSettle();

    expect(
      tester
          .widget<Text>(find.byKey(const ValueKey('request-token-total')))
          .data,
      '—',
    );
  });

  testWidgets('the board row shows the server cost_summary usage figure when '
      'present', (tester) async {
    final client = MockClient((request) async {
      return http.Response(
        '[${requestJson(
          id: 'req-a',
          state: 'building',
          title: 'Has a cost rollup',
          costSummary: requestCostSummaryJson(total: 4.5, complete: true, tokens: 478300, byModel: const [
            {'model': 'gpt-5.6-luna', 'tokens': 478300},
          ]),
        )}]',
        200,
      );
    });
    final api = RunApi(baseUrl: 'http://factory.test', client: client);

    await tester.pumpWidget(FactoryConsole(api: api));
    await tester.pumpAndSettle();

    expect(find.byKey(const ValueKey('request-token-total')), findsNothing);
    expect(
      tester
          .widget<Text>(find.byKey(const ValueKey('request-cost-total')))
          .data,
      'gpt-5.6-luna · 478.3k tokens',
    );
  });

  testWidgets('an incomplete cost_summary renders "≥ " tokens, not an exact '
      'figure', (tester) async {
    final client = MockClient((request) async {
      return http.Response(
        '[${requestJson(id: 'req-a', state: 'building', title: 'Incomplete rollup', costSummary: requestCostSummaryJson(total: 2, complete: false, tokens: 500, tokensComplete: false))}]',
        200,
      );
    });
    final api = RunApi(baseUrl: 'http://factory.test', client: client);

    await tester.pumpWidget(FactoryConsole(api: api));
    await tester.pumpAndSettle();

    expect(
      tester
          .widget<Text>(find.byKey(const ValueKey('request-cost-total')))
          .data,
      '≥ 500 tokens',
    );
  });

  testWidgets(
    'a request rejected at least once shows a ↻N marker on the board row',
    (tester) async {
      final client = MockClient((request) async {
        return http.Response(
          '[${requestJson(
            id: 'req-a',
            state: 'spec_review',
            title: 'Rejected twice',
            rejections: [
              requestRejectionJson(by: 'jane', at: '2026-09-10T09:00:00Z', reason: 'too broad', fromState: 'spec_review'),
              requestRejectionJson(by: 'jane', at: '2026-09-11T09:00:00Z', reason: 'still too broad', fromState: 'spec_review'),
            ],
          )}]',
          200,
        );
      });
      final api = RunApi(baseUrl: 'http://factory.test', client: client);

      await tester.pumpWidget(FactoryConsole(api: api));
      await tester.pumpAndSettle();

      expect(find.byKey(const ValueKey('rejection-marker')), findsOneWidget);
      expect(find.text('↻2'), findsOneWidget);
    },
  );

  testWidgets('a request never rejected shows no ↻ marker', (tester) async {
    final client = MockClient((request) async {
      return http.Response(
        '[${requestJson(id: 'req-a', state: 'spec_review', title: 'Never rejected')}]',
        200,
      );
    });
    final api = RunApi(baseUrl: 'http://factory.test', client: client);

    await tester.pumpWidget(FactoryConsole(api: api));
    await tester.pumpAndSettle();

    expect(find.byKey(const ValueKey('rejection-marker')), findsNothing);
  });

  testWidgets('the board row title is the derived short title, markdown/'
      'backticks stripped, with the full raw title in a tooltip', (
    tester,
  ) async {
    final client = MockClient((request) async {
      return http.Response(
        '[${requestJson(id: 'req-a', state: 'spec_review', title: '`Add sub.py` and **div.py**')}]',
        200,
      );
    });
    final api = RunApi(baseUrl: 'http://factory.test', client: client);

    await tester.pumpWidget(FactoryConsole(api: api));
    await tester.pumpAndSettle();

    // The stripped, plain-text form is what's shown...
    expect(find.text('Add sub.py and div.py'), findsOneWidget);
    // ...and it's still searchable by the raw, unstripped text (the
    // filter matches request.title verbatim, not the display string).
    await tester.enterText(
      find.byKey(const ValueKey('request-search-field')),
      '`Add sub.py`',
    );
    await tester.pumpAndSettle();
    expect(find.text('Add sub.py and div.py'), findsOneWidget);

    // The full raw title (backticks included) is available via tooltip,
    // wrapping this same row's title Text (not some unrelated toolbar
    // icon's own tooltip).
    final tooltip = tester.widget<Tooltip>(
      find.ancestor(
        of: find.text('Add sub.py and div.py'),
        matching: find.byType(Tooltip),
      ),
    );
    expect(tooltip.message, '`Add sub.py` and **div.py**');
  });

  group('release policy warning banner (release-policy visibility gap)', () {
    MockClient emptyClient() => MockClient((request) async {
      return http.Response('[]', 200);
    });

    testWidgets('shows when the server reports a deny-all release policy', (
      tester,
    ) async {
      final api = RunApi(
        baseUrl: 'http://factory.test',
        client: emptyClient(),
        releasePolicyWarning:
            'release policy denies every PR unconditionally '
            '(release_max_files_changed=0) -- add release_max_files_changed, '
            'release_max_insertions, and release_rollback_plan to your '
            'session config',
      );

      await tester.pumpWidget(FactoryConsole(api: api));
      await tester.pumpAndSettle();

      expect(
        find.byKey(const ValueKey('release-policy-warning-banner')),
        findsOneWidget,
      );
      expect(
        find.textContaining('Release policy denies every PR'),
        findsOneWidget,
      );
    });

    testWidgets('is hidden when the server reports a usable release policy', (
      tester,
    ) async {
      final api = RunApi(baseUrl: 'http://factory.test', client: emptyClient());

      await tester.pumpWidget(FactoryConsole(api: api));
      await tester.pumpAndSettle();

      expect(
        find.byKey(const ValueKey('release-policy-warning-banner')),
        findsNothing,
      );
    });
  });

  group('worker heartbeat banner', () {
    MockClient clientFor({
      required http.Response Function(http.Request) queueRun,
      String requestsBody = '[]',
    }) {
      return MockClient((request) async {
        if (request.url.path == '/queue-run') return queueRun(request);
        if (request.url.path == '/requests') {
          return http.Response(requestsBody, 200);
        }
        return http.Response('[]', 200);
      });
    }

    testWidgets('shows when worker is stale', (tester) async {
      final api = RunApi(
        baseUrl: 'http://factory.test',
        client: clientFor(
          queueRun: (_) => http.Response(
            '{"state":"stale","last_heartbeat":"2026-09-24T09:00:00Z"}',
            200,
          ),
        ),
      );

      await tester.pumpWidget(FactoryConsole(api: api));
      await tester.pumpAndSettle();

      expect(
        find.byKey(const ValueKey('worker-down-banner')),
        findsOneWidget,
      );
      expect(find.textContaining('worker is not running'), findsOneWidget);
      expect(find.textContaining('start `factoryd worker`'), findsOneWidget);
    });

    testWidgets('shows when worker is absent and a request is in a '
        'worker-dependent working state', (tester) async {
      final api = RunApi(
        baseUrl: 'http://factory.test',
        client: clientFor(
          queueRun: (_) => http.Response('{"state":"absent"}', 200),
          requestsBody: '[${requestJson(id: 'req-a', state: 'building')}]',
        ),
      );

      await tester.pumpWidget(FactoryConsole(api: api));
      await tester.pumpAndSettle();

      expect(
        find.byKey(const ValueKey('worker-down-banner')),
        findsOneWidget,
      );
      expect(find.textContaining('no worker has run'), findsOneWidget);
      expect(find.textContaining('start `factoryd worker`'), findsOneWidget);
    });

    testWidgets(
      'is hidden when worker is absent but no request is working',
      (tester) async {
        final api = RunApi(
          baseUrl: 'http://factory.test',
          client: clientFor(
            queueRun: (_) => http.Response('{"state":"absent"}', 200),
            requestsBody: '[${requestJson(id: 'req-a', state: 'done')}]',
          ),
        );

        await tester.pumpWidget(FactoryConsole(api: api));
        await tester.pumpAndSettle();

        expect(
          find.byKey(const ValueKey('worker-down-banner')),
          findsNothing,
        );
      },
    );

    testWidgets('is hidden when worker is alive', (tester) async {
      final api = RunApi(
        baseUrl: 'http://factory.test',
        client: clientFor(
          queueRun: (_) => http.Response(
            '{"state":"alive","last_heartbeat":"2026-09-24T09:00:00Z"}',
            200,
          ),
        ),
      );

      await tester.pumpWidget(FactoryConsole(api: api));
      await tester.pumpAndSettle();

      expect(find.byKey(const ValueKey('worker-down-banner')), findsNothing);
    });

    testWidgets('is hidden on a 404 (an older server predating GET /queue-run) '
        'rather than false-alarming', (tester) async {
      final api = RunApi(
        baseUrl: 'http://factory.test',
        client: clientFor(queueRun: (_) => http.Response('not found', 404)),
      );

      await tester.pumpWidget(FactoryConsole(api: api));
      await tester.pumpAndSettle();

      expect(find.byKey(const ValueKey('worker-down-banner')), findsNothing);
    });
  });

  group('freshness indicator', () {
    test('requestBoardFreshness', () {
      expect(
        requestBoardFreshness(sseConnected: true, sseFailureCount: 5),
        RequestBoardFreshness.live,
        reason: 'connected always wins, regardless of any prior failures',
      );
      expect(
        requestBoardFreshness(sseConnected: false, sseFailureCount: 0),
        RequestBoardFreshness.recent,
      );
      expect(
        requestBoardFreshness(
          sseConnected: false,
          sseFailureCount: maxSseFailuresBeforeDisconnected,
        ),
        RequestBoardFreshness.disconnected,
      );
    });

    test('freshnessLabel', () {
      final now = DateTime.parse('2026-09-10T09:00:30Z');
      expect(freshnessLabel(RequestBoardFreshness.live, null, now), 'Live');
      expect(
        freshnessLabel(RequestBoardFreshness.disconnected, null, now),
        'Disconnected',
      );
      expect(
        freshnessLabel(RequestBoardFreshness.recent, null, now),
        'Connecting…',
      );
      expect(
        freshnessLabel(
          RequestBoardFreshness.recent,
          DateTime.parse('2026-09-10T09:00:00Z'),
          now,
        ),
        'Last updated 30s ago',
      );
    });

    // End-to-end: a live GET /requests/events connection (via the exact
    // SSE wire shape the server writes) drives the board's own indicator
    // to "Live" -- not just the pure freshnessLabel function above.
    testWidgets('shows "Live" once the SSE connection is open', (tester) async {
      // A body stream that never closes -- matching the real
      // /requests/events endpoint, which stays open for as long as the
      // connection is healthy, never ends on its own. `Stream.empty()`
      // closes (onDone) essentially immediately, which made this test
      // race the reconnect-backoff loop's own first retry: settling
      // could land either on "Live" (right after connecting) or
      // "Connecting…"/"Disconnected" (mid-retry), depending on exactly
      // how many microtasks the connect path took -- exactly the
      // flakiness a client-side change with no visible behavior
      // difference surfaced.
      final neverCloses = StreamController<List<int>>();
      addTearDown(neverCloses.close);
      final client = MockClient.streaming((request, bodyStream) async {
        if (request.url.path == '/requests/events') {
          return http.StreamedResponse(neverCloses.stream, 200);
        }
        return http.StreamedResponse(Stream.value(utf8.encode('[]')), 200);
      });
      final api = RunApi(baseUrl: 'http://factory.test', client: client);

      await tester.pumpWidget(FactoryConsole(api: api));
      await tester.pumpAndSettle();

      expect(find.byKey(const ValueKey('board-freshness')), findsOneWidget);
      expect(find.text('Live'), findsOneWidget);
    });

    // A GET /requests/events that always fails outright (never opens)
    // must settle on "Disconnected" once it has retried
    // maxSseFailuresBeforeDisconnected times -- not stay silently
    // "Connecting…" forever, and not flip to "Live".
    testWidgets('shows "Disconnected" once reconnects exceed the threshold', (
      tester,
    ) async {
      final client = MockClient((request) async {
        if (request.url.path == '/requests/events') {
          return http.Response('unreachable', 500);
        }
        return http.Response('[]', 200);
      });
      final api = RunApi(
        baseUrl: 'http://factory.test',
        client: client,
        watchInitialBackoff: const Duration(milliseconds: 1),
        watchMaxBackoff: const Duration(milliseconds: 2),
      );

      await tester.pumpWidget(FactoryConsole(api: api));
      // Enough real time for well over maxSseFailuresBeforeDisconnected
      // reconnect attempts at a 1-2ms backoff.
      await tester.pump(const Duration(milliseconds: 50));
      await tester.pump(const Duration(milliseconds: 50));
      await tester.pump(const Duration(milliseconds: 50));

      expect(find.text('Disconnected'), findsOneWidget);
    });
  });

  group('dark mode toggle', () {
    setUp(clearStoredThemeModeForTest);
    tearDown(clearStoredThemeModeForTest);

    testWidgets(
      'the app bar toggle cycles system -> light -> dark -> system and '
      'persists each choice',
      (tester) async {
        final client = MockClient((request) async {
          return http.Response('[]', 200);
        });
        final api = RunApi(baseUrl: 'http://factory.test', client: client);

        await tester.pumpWidget(FactoryConsole(api: api));
        await tester.pumpAndSettle();

        MaterialApp materialApp() =>
            tester.widget<MaterialApp>(find.byType(MaterialApp));

        expect(materialApp().themeMode, ThemeMode.system);

        await tester.tap(find.byKey(const ValueKey('theme-mode-toggle')));
        await tester.pumpAndSettle();
        expect(materialApp().themeMode, ThemeMode.light);
        expect(getStoredThemeMode(), ThemeMode.light);

        await tester.tap(find.byKey(const ValueKey('theme-mode-toggle')));
        await tester.pumpAndSettle();
        expect(materialApp().themeMode, ThemeMode.dark);
        expect(getStoredThemeMode(), ThemeMode.dark);

        await tester.tap(find.byKey(const ValueKey('theme-mode-toggle')));
        await tester.pumpAndSettle();
        expect(materialApp().themeMode, ThemeMode.system);
        expect(getStoredThemeMode(), ThemeMode.system);
      },
    );

    testWidgets('a previously stored theme mode is restored on launch', (
      tester,
    ) async {
      setStoredThemeMode(ThemeMode.dark);
      final client = MockClient((request) async {
        return http.Response('[]', 200);
      });
      final api = RunApi(baseUrl: 'http://factory.test', client: client);

      await tester.pumpWidget(FactoryConsole(api: api));
      await tester.pumpAndSettle();

      expect(
        tester.widget<MaterialApp>(find.byType(MaterialApp)).themeMode,
        ThemeMode.dark,
      );
    });
  });
}
