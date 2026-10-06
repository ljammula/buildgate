import 'dart:async';
import 'dart:convert';

import 'package:console/api_client.dart';
import 'package:console/elapsed.dart';
import 'package:console/operator_identity.dart';
import 'package:console/request_detail_screen.dart';
import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:http/http.dart' as http;
import 'package:http/testing.dart';

import 'fixtures.dart';

// Every state internal/request.State defines -- the request board
// requires a request in each of these to render without error against a
// recorded fixture.
const _allStates = [
  'submitted',
  'spec_drafting',
  'spec_review',
  'planning',
  'plan_review',
  'building',
  'pr_review',
  'done',
  'quarantined',
  'halted',
  'resume_review',
  'cancelled',
];

const _reviewStates = {'spec_review', 'plan_review'};

void main() {
  // The approve/reject flow now prompts for an
  // operator name (once per browser) before the confirm sheet/reason
  // dialog -- seed it here so tests not specifically exercising that
  // prompt aren't tripped up by it, mirroring how a returning operator's
  // browser would already have one stored.
  setUp(() => setOperatorName('operator'));

  for (final state in _allStates) {
    testWidgets('renders a request in state $state without error', (
      tester,
    ) async {
      final client = MockClient((request) async {
        return http.Response(
          requestJson(
            id: 'req-1',
            state: state,
            title: 'Add idempotency keys',
            spec: '# Spec\n\nDetail.',
          ),
          200,
        );
      });
      final api = RunApi(baseUrl: 'http://factory.test', client: client);

      await tester.pumpWidget(
        MaterialApp(
          home: RequestDetailScreen(api: api, requestId: 'req-1'),
        ),
      );
      await tester.pumpAndSettle();

      expect(tester.takeException(), isNull);
      expect(find.text('Add idempotency keys'), findsWidgets);

      final approveFinder = find.byKey(
        const ValueKey('approve-request-button'),
      );
      final rejectFinder = find.byKey(const ValueKey('reject-request-button'));
      if (_reviewStates.contains(state)) {
        expect(approveFinder, findsOneWidget);
        expect(rejectFinder, findsOneWidget);
      } else {
        expect(approveFinder, findsNothing);
        expect(rejectFinder, findsNothing);
      }
    });
  }

  testWidgets('renders ticket plan content and PR/run links for plan_review', (
    tester,
  ) async {
    final client = MockClient((request) async {
      return http.Response(
        requestJson(
          id: 'req-1',
          state: 'plan_review',
          title: 'Add idempotency keys',
          tickets: [
            requestTicketJson(
              index: 1,
              specPath: 'tickets/001.spec.md',
              runId: 'run-1',
              prUrl: 'https://github.com/acme/app/pull/1',
              prState: 'open',
              content: 'Verify-Command: true\n## Goal\n',
            ),
          ],
        ),
        200,
      );
    });
    final api = RunApi(baseUrl: 'http://factory.test', client: client);

    // A tall surface so every section (including the ticket links below
    // the plan content and review buttons) is actually built by the
    // ListView's sliver, not just the portion the default test viewport
    // would otherwise cover.
    tester.view.physicalSize = const Size(1200, 2400);
    tester.view.devicePixelRatio = 1.0;
    addTearDown(tester.view.resetPhysicalSize);
    addTearDown(tester.view.resetDevicePixelRatio);

    await tester.pumpWidget(
      MaterialApp(
        home: RequestDetailScreen(api: api, requestId: 'req-1'),
      ),
    );
    await tester.pumpAndSettle();

    expect(find.textContaining('## Goal'), findsOneWidget);
    expect(
      find.textContaining('https://github.com/acme/app/pull/1'),
      findsOneWidget,
    );
    expect(find.byKey(const ValueKey('ticket-run-link-1')), findsOneWidget);
  });

  // Phase 4 (follow-along console): the server's ticket JSON has no
  // run_state field (checked against internal/api/server.go's
  // requestSummaryView/requestTicketView and internal/request.Ticket --
  // see ticket_rollup.dart's ticketStatus doc comment), so this screen
  // fetches the ticket's own run with a plain GET /runs/{id} and shows its
  // real StateBadge, instead of the board-wide roll-up's runId-implies-
  // "building" approximation.
  testWidgets('a ticket card shows its own run\'s real state, not a guess', (
    tester,
  ) async {
    final client = MockClient((request) async {
      if (request.url.path == '/runs/run-1') {
        return http.Response(quarantinedRunJson, 200);
      }
      return http.Response(
        requestJson(
          id: 'req-1',
          state: 'building',
          title: 'Add idempotency keys',
          tickets: [requestTicketJson(index: 1, runId: 'run-1')],
        ),
        200,
      );
    });
    final api = RunApi(baseUrl: 'http://factory.test', client: client);

    tester.view.physicalSize = const Size(1200, 2400);
    tester.view.devicePixelRatio = 1.0;
    addTearDown(tester.view.resetPhysicalSize);
    addTearDown(tester.view.resetDevicePixelRatio);

    await tester.pumpWidget(
      MaterialApp(
        home: RequestDetailScreen(api: api, requestId: 'req-1'),
      ),
    );
    await tester.pumpAndSettle();

    // The ticket card's own badge reflects run-1's real (quarantined)
    // state -- not the roll-up strip's runId-implies-"building" guess,
    // which a request in the 'building' state also shows above.
    expect(find.byKey(const ValueKey('state-quarantined')), findsOneWidget);
    expect(find.byKey(const ValueKey('ticket-run-link-1')), findsOneWidget);
  });

  // "Silence is a bug" (progress-contract.md, 2026-09-18): a ticket card
  // shows the same stalled/waiting chip as the run list and run detail
  // screens, next to its own run's state badge -- it already fetches
  // that run (see the test above), so this is the same GET /runs/{id}
  // response, just with the progress fields set.
  testWidgets('a ticket card shows a waiting chip for its queued run', (
    tester,
  ) async {
    final client = MockClient((request) async {
      if (request.url.path == '/runs/run-1') {
        return http.Response(
          waitingRunJson(
            id: 'run-1',
            waitingReason: 'behind 1 run(s) on foo/bar',
          ),
          200,
        );
      }
      return http.Response(
        requestJson(
          id: 'req-1',
          state: 'building',
          title: 'Add idempotency keys',
          tickets: [requestTicketJson(index: 1, runId: 'run-1')],
        ),
        200,
      );
    });
    final api = RunApi(baseUrl: 'http://factory.test', client: client);

    tester.view.physicalSize = const Size(1200, 2400);
    tester.view.devicePixelRatio = 1.0;
    addTearDown(tester.view.resetPhysicalSize);
    addTearDown(tester.view.resetDevicePixelRatio);

    await tester.pumpWidget(
      MaterialApp(
        home: RequestDetailScreen(api: api, requestId: 'req-1'),
      ),
    );
    await tester.pumpAndSettle();

    expect(find.byKey(const ValueKey('waiting-chip')), findsOneWidget);
    expect(find.byKey(const ValueKey('stalled-chip')), findsNothing);
  });

  // Raw by default (this screen's pre-existing
  // behavior, asserted above), with a toggle to switch to rendered
  // markdown -- and back.
  testWidgets(
    'the markdown raw/rendered toggle switches between plain text and a '
    'rendered heading',
    (tester) async {
      final client = MockClient((request) async {
        return http.Response(
          requestJson(
            id: 'req-1',
            state: 'spec_review',
            title: 'Add idempotency keys',
            spec: '## Goal\n\nDo the thing.\n',
          ),
          200,
        );
      });
      final api = RunApi(baseUrl: 'http://factory.test', client: client);

      // A tall surface: the new Pipeline section above Request pushes the
      // raw/rendered toggle below the default 600px viewport.
      tester.view.physicalSize = const Size(1200, 2400);
      tester.view.devicePixelRatio = 1.0;
      addTearDown(tester.view.resetPhysicalSize);
      addTearDown(tester.view.resetDevicePixelRatio);

      await tester.pumpWidget(
        MaterialApp(
          home: RequestDetailScreen(api: api, requestId: 'req-1'),
        ),
      );
      await tester.pumpAndSettle();

      // Raw by default: the literal markdown source, including its `##`
      // marker, is visible as plain text.
      expect(
        find.byKey(const ValueKey('markdown-raw-content')),
        findsOneWidget,
      );
      expect(find.textContaining('## Goal'), findsOneWidget);

      await tester.tap(find.byKey(const ValueKey('markdown-raw-toggle')));
      await tester.pumpAndSettle();

      // Rendered: the `##` marker is gone (it became heading styling, not
      // literal text) but the heading's own text content still renders.
      expect(
        find.byKey(const ValueKey('markdown-rendered-content')),
        findsOneWidget,
      );
      expect(find.textContaining('## Goal'), findsNothing);
      expect(find.text('Goal'), findsOneWidget);
      expect(find.text('Do the thing.'), findsOneWidget);
    },
  );

  testWidgets('approving a request calls the approve endpoint and refreshes', (
    tester,
  ) async {
    var approveCalled = false;
    final client = MockClient((request) async {
      if (request.method == 'POST' &&
          request.url.path == '/requests/req-1/approve') {
        approveCalled = true;
        return http.Response(
          requestJson(
            id: 'req-1',
            state: 'planning',
            title: 'Add idempotency keys',
          ),
          200,
        );
      }
      return http.Response(
        requestJson(
          id: 'req-1',
          state: 'spec_review',
          title: 'Add idempotency keys',
        ),
        200,
      );
    });
    final api = RunApi(
      baseUrl: 'http://factory.test',
      client: client,
      overrideToken: 'test-token',
    );

    await tester.pumpWidget(
      MaterialApp(
        home: RequestDetailScreen(api: api, requestId: 'req-1'),
      ),
    );
    await tester.pumpAndSettle();

    await tester.tap(find.byKey(const ValueKey('approve-request-button')));
    await tester.pumpAndSettle();

    // Approve now goes through a confirm sheet
    // before actually calling the endpoint.
    expect(
      find.byKey(const ValueKey('approve-confirm-button')),
      findsOneWidget,
    );
    await tester.tap(find.byKey(const ValueKey('approve-confirm-button')));
    await tester.pumpAndSettle();

    expect(approveCalled, isTrue);
    expect(find.byKey(const ValueKey('approve-request-button')), findsNothing);
  });

  testWidgets(
    'approving keeps the title and spec visible from the approve response',
    (tester) async {
      final client = MockClient((request) async {
        if (request.method == 'POST' &&
            request.url.path == '/requests/req-1/approve') {
          // The approve response is expected to carry the same
          // enriched fields (title, spec) GET /requests/{id} does --
          // this is what request_handlers_test.go's Go-side assertions
          // guard; here we confirm the screen actually renders them
          // from that response rather than dropping to the request id
          // and a blank spec.
          return http.Response(
            requestJson(
              id: 'req-1',
              state: 'planning',
              title: 'Add idempotency keys',
              spec: '# Spec\n\nDetail.',
            ),
            200,
          );
        }
        return http.Response(
          requestJson(
            id: 'req-1',
            state: 'spec_review',
            title: 'Add idempotency keys',
            spec: '# Spec\n\nDetail.',
          ),
          200,
        );
      });
      final api = RunApi(
        baseUrl: 'http://factory.test',
        client: client,
        overrideToken: 'test-token',
      );

      await tester.pumpWidget(
        MaterialApp(
          home: RequestDetailScreen(api: api, requestId: 'req-1'),
        ),
      );
      await tester.pumpAndSettle();

      await tester.tap(find.byKey(const ValueKey('approve-request-button')));
      await tester.pumpAndSettle();
      await tester.tap(find.byKey(const ValueKey('approve-confirm-button')));
      await tester.pumpAndSettle();

      expect(find.text('Add idempotency keys'), findsWidgets);
      expect(find.textContaining('Detail.'), findsOneWidget);
    },
  );

  testWidgets(
    'rejecting a request requires a reason and calls the reject endpoint',
    (tester) async {
      String? sentReason;
      final client = MockClient((request) async {
        if (request.method == 'POST' &&
            request.url.path == '/requests/req-1/reject') {
          sentReason = request.body;
          return http.Response(
            requestJson(
              id: 'req-1',
              state: 'spec_drafting',
              title: 'Add idempotency keys',
            ),
            200,
          );
        }
        return http.Response(
          requestJson(
            id: 'req-1',
            state: 'spec_review',
            title: 'Add idempotency keys',
          ),
          200,
        );
      });
      final api = RunApi(
        baseUrl: 'http://factory.test',
        client: client,
        overrideToken: 'test-token',
      );

      await tester.pumpWidget(
        MaterialApp(
          home: RequestDetailScreen(api: api, requestId: 'req-1'),
        ),
      );
      await tester.pumpAndSettle();

      await tester.tap(find.byKey(const ValueKey('reject-request-button')));
      await tester.pumpAndSettle();

      // Confirming with an empty reason must not submit.
      await tester.tap(find.byKey(const ValueKey('reject-confirm-button')));
      await tester.pumpAndSettle();
      expect(sentReason, isNull);
      expect(find.byKey(const ValueKey('reject-reason')), findsOneWidget);

      await tester.enterText(
        find.byKey(const ValueKey('reject-reason')),
        'scope is too broad',
      );
      await tester.tap(find.byKey(const ValueKey('reject-confirm-button')));
      await tester.pumpAndSettle();

      expect(sentReason, contains('scope is too broad'));
    },
  );

  testWidgets('shows the audit line when approvedBy/approvedAt are set', (
    tester,
  ) async {
    final client = MockClient((request) async {
      return http.Response(
        requestJson(
          id: 'req-1',
          state: 'building',
          title: 'Add idempotency keys',
          approvedBy: 'jane',
          approvedAt: '2026-09-12T10:00:00Z',
        ),
        200,
      );
    });
    final api = RunApi(baseUrl: 'http://factory.test', client: client);

    await tester.pumpWidget(
      MaterialApp(
        home: RequestDetailScreen(api: api, requestId: 'req-1'),
      ),
    );
    await tester.pumpAndSettle();

    // Local time, same format as Submitted/Updated; computed the same
    // way here so the test holds in any TZ the suite runs under.
    final local = DateTime.parse('2026-09-12T10:00:00Z').toLocal();
    String two(int n) => n.toString().padLeft(2, '0');
    expect(
      find.textContaining(
        'Approved by jane at ${local.year}-${two(local.month)}-'
        '${two(local.day)} ${two(local.hour)}:${two(local.minute)}:00',
      ),
      findsOneWidget,
    );
  });

  testWidgets('shows no audit line when the request has never been approved', (
    tester,
  ) async {
    final client = MockClient((request) async {
      return http.Response(
        requestJson(
          id: 'req-1',
          state: 'spec_review',
          title: 'Not yet approved',
        ),
        200,
      );
    });
    final api = RunApi(baseUrl: 'http://factory.test', client: client);

    await tester.pumpWidget(
      MaterialApp(
        home: RequestDetailScreen(api: api, requestId: 'req-1'),
      ),
    );
    await tester.pumpAndSettle();

    expect(find.textContaining('Approved by'), findsNothing);
  });

  testWidgets('shows the rejection history when rejections are present', (
    tester,
  ) async {
    final client = MockClient((request) async {
      return http.Response(
        requestJson(
          id: 'req-1',
          state: 'spec_review',
          title: 'Rejected once',
          rejections: [
            requestRejectionJson(
              by: 'jane',
              at: '2026-09-12T10:00:00Z',
              reason: 'scope is too broad',
              fromState: 'spec_review',
            ),
          ],
        ),
        200,
      );
    });
    final api = RunApi(baseUrl: 'http://factory.test', client: client);

    await tester.pumpWidget(
      MaterialApp(
        home: RequestDetailScreen(api: api, requestId: 'req-1'),
      ),
    );
    await tester.pumpAndSettle();

    final historyTile = find.byKey(const ValueKey('rejection-history'));
    expect(historyTile, findsOneWidget);
    await tester.tap(historyTile);
    await tester.pumpAndSettle();

    expect(
      find.textContaining(
        'Rejected by jane at ${formatLocalTimestamp('2026-09-12T10:00:00Z')}',
      ),
      findsOneWidget,
    );
    expect(find.text('scope is too broad'), findsOneWidget);
  });

  testWidgets(
    'approve/reject are disabled again while a Refresh is in flight or '
    'has failed (regression: detailLoaded previously stayed true after '
    'the first successful load, so a later failed Refresh left the '
    'buttons enabled against stale content)',
    (tester) async {
      var callCount = 0;
      final client = MockClient((request) async {
        callCount++;
        if (callCount == 1) {
          return http.Response(
            requestJson(id: 'req-1', state: 'spec_review', title: 'First'),
            200,
          );
        }
        return http.Response('server error', 500);
      });
      final api = RunApi(
        baseUrl: 'http://factory.test',
        client: client,
        overrideToken: 'test-token',
      );

      await tester.pumpWidget(
        MaterialApp(
          home: RequestDetailScreen(api: api, requestId: 'req-1'),
        ),
      );
      await tester.pumpAndSettle();

      FilledButton approveButton() => tester.widget<FilledButton>(
        find.byKey(const ValueKey('approve-request-button')),
      );

      expect(approveButton().onPressed, isNotNull);

      await tester.tap(
        find.byKey(const ValueKey('request-detail-refresh-button')),
      );
      // Mid-flight: detailLoaded must already be false, not just once the
      // failure lands.
      await tester.pump();
      expect(approveButton().onPressed, isNull);

      await tester.pumpAndSettle();
      // The refresh failed (500) -- still disabled, not restored.
      expect(approveButton().onPressed, isNull);
    },
  );

  testWidgets('the approve confirm sheet is absent when no override token is '
      'configured', (tester) async {
    final client = MockClient((request) async {
      return http.Response(
        requestJson(id: 'req-1', state: 'spec_review', title: 'No token'),
        200,
      );
    });
    // Deliberately no overrideToken -- the server-fail-closed shape
    // every write route in this console shares.
    final api = RunApi(baseUrl: 'http://factory.test', client: client);

    await tester.pumpWidget(
      MaterialApp(
        home: RequestDetailScreen(api: api, requestId: 'req-1'),
      ),
    );
    await tester.pumpAndSettle();

    expect(
      find.byKey(const ValueKey('approve-request-button')),
      findsOneWidget,
    );
    await tester.tap(find.byKey(const ValueKey('approve-request-button')));
    await tester.pumpAndSettle();

    expect(find.byKey(const ValueKey('approve-confirm-button')), findsNothing);
    expect(find.byKey(const ValueKey('operator-name-field')), findsNothing);
  });

  testWidgets('the reject reason dialog is absent when no override token is '
      'configured', (tester) async {
    final client = MockClient((request) async {
      return http.Response(
        requestJson(id: 'req-1', state: 'spec_review', title: 'No token'),
        200,
      );
    });
    final api = RunApi(baseUrl: 'http://factory.test', client: client);

    await tester.pumpWidget(
      MaterialApp(
        home: RequestDetailScreen(api: api, requestId: 'req-1'),
      ),
    );
    await tester.pumpAndSettle();

    await tester.tap(find.byKey(const ValueKey('reject-request-button')));
    await tester.pumpAndSettle();

    expect(find.byKey(const ValueKey('reject-reason')), findsNothing);
  });

  testWidgets('no Approve/Reject for a halted request (regression: broadening '
      "requestStageGroup's review classification to include halted, for "
      "the board's needs-you grouping, must not also make this screen "
      'think halted is approve/reject-eligible -- the server refuses '
      'Approve/Reject from any state but spec_review/plan_review)', (
    tester,
  ) async {
    final client = MockClient((request) async {
      return http.Response(
        requestJson(id: 'req-1', state: 'halted', title: 'Halted'),
        200,
      );
    });
    final api = RunApi(
      baseUrl: 'http://factory.test',
      client: client,
      overrideToken: 'test-token',
    );

    await tester.pumpWidget(
      MaterialApp(
        home: RequestDetailScreen(api: api, requestId: 'req-1'),
      ),
    );
    await tester.pumpAndSettle();

    expect(find.byKey(const ValueKey('approve-request-button')), findsNothing);
    expect(find.byKey(const ValueKey('reject-request-button')), findsNothing);
  });

  testWidgets(
    'the quarantined callout is shown with disabled actions when no write '
    'access is configured (never "waiting on you" with nothing to do)',
    (tester) async {
      final client = MockClient((request) async {
        return http.Response(
          requestJson(
            id: 'req-1',
            state: 'quarantined',
            title: 'Quarantined, no token',
            tickets: [requestTicketJson(index: 1, runId: 'run-1')],
          ),
          200,
        );
      });
      final api = RunApi(baseUrl: 'http://factory.test', client: client);

      await tester.pumpWidget(
        MaterialApp(
          home: RequestDetailScreen(api: api, requestId: 'req-1'),
        ),
      );
      await tester.pumpAndSettle();

      // The callout itself is always shown for a quarantined/halted request
      // -- only the Retry/Cancel buttons are disabled without write
      // access, never the whole callout hidden.
      expect(find.byKey(const ValueKey('recovery-callout')), findsOneWidget);
      final retryButton = tester.widget<FilledButton>(
        find.byKey(const ValueKey('retry-request-button')),
      );
      expect(retryButton.onPressed, isNull);
    },
  );

  testWidgets('a live event re-fetches the full detail instead of applying the '
      'summary-shaped event (console-walk 2026-09-24: the Spec section '
      'vanished on a live redraft)', (tester) async {
    final events = StreamController<List<int>>();
    addTearDown(events.close);
    var spec = '# Spec\n\nFIRST DRAFT';
    var updatedAt = '2026-09-10T09:05:00Z';
    var detailFetches = 0;
    final client = MockClient.streaming((request, _) async {
      if (request.url.path == '/requests/events') {
        return http.StreamedResponse(events.stream, 200);
      }
      if (request.url.path == '/requests/req-1') {
        detailFetches += 1;
        return http.StreamedResponse(
          Stream.value(
            utf8.encode(
              requestJson(
                id: 'req-1',
                state: 'spec_review',
                title: 'Live',
                spec: spec,
                updatedAt: updatedAt,
              ),
            ),
          ),
          200,
        );
      }
      return http.StreamedResponse(Stream.value(utf8.encode('[]')), 200);
    });
    final api = RunApi(baseUrl: 'http://factory.test', client: client);
    tester.view.physicalSize = const Size(1200, 4000);
    tester.view.devicePixelRatio = 1.0;
    addTearDown(tester.view.resetPhysicalSize);
    addTearDown(tester.view.resetDevicePixelRatio);

    await tester.pumpWidget(
      MaterialApp(
        home: RequestDetailScreen(api: api, requestId: 'req-1'),
      ),
    );
    await tester.pumpAndSettle();
    expect(find.textContaining('FIRST DRAFT'), findsWidgets);
    final before = detailFetches;

    // The server redrafts; the event carries only the summary shape
    // (no spec), exactly like GET /requests/events does.
    spec = '# Spec\n\nREDRAFT';
    updatedAt = '2026-09-10T09:07:00Z';
    final summary = requestJson(
      id: 'req-1',
      state: 'spec_review',
      title: 'Live',
      updatedAt: updatedAt,
    );
    events.add(utf8.encode('event: state\ndata: $summary\n\n'));
    await tester.pumpAndSettle();

    expect(detailFetches, before + 1);
    expect(find.textContaining('REDRAFT'), findsWidgets);
    expect(find.textContaining('FIRST DRAFT'), findsNothing);

    // A repeat of the same event (same updated_at/state) is a no-op.
    events.add(utf8.encode('event: state\ndata: $summary\n\n'));
    await tester.pumpAndSettle();
    expect(detailFetches, before + 1);
  });

  testWidgets(
    'the stepper shows a -draft-oracles request its oracle steps before it '
    'reaches them, and marks accepted-awaiting-PR as needs-you, not failed',
    (tester) async {
      var body = requestJson(
        id: 'req-1',
        state: 'spec_review',
        title: 'Oracles ahead',
        draftOracles: true,
      );
      final client = MockClient((request) async => http.Response(body, 200));
      final api = RunApi(baseUrl: 'http://factory.test', client: client);
      tester.view.physicalSize = const Size(1600, 3000);
      tester.view.devicePixelRatio = 1.0;
      addTearDown(tester.view.resetPhysicalSize);
      addTearDown(tester.view.resetDevicePixelRatio);

      await tester.pumpWidget(
        MaterialApp(
          home: RequestDetailScreen(api: api, requestId: 'req-1'),
        ),
      );
      await tester.pumpAndSettle();
      expect(
        find.byKey(const ValueKey('pipeline-step-oracle_review')),
        findsOneWidget,
      );

      body = requestJson(
        id: 'req-1',
        state: 'halted',
        haltKind: 'accepted_no_pr',
        title: 'Accepted',
        history: [
          requestHistoryEntryJson(
            from: 'building',
            to: 'pr_review',
            at: '2026-09-10T09:06:00Z',
            by: 'factory',
          ),
          requestHistoryEntryJson(
            from: 'pr_review',
            to: 'halted',
            at: '2026-09-10T09:07:00Z',
            by: 'factory',
            reason: 'denied by release policy ${'x' * 2000}',
          ),
        ],
      );
      await tester.pumpWidget(
        MaterialApp(
          home: RequestDetailScreen(
            key: const ValueKey('second'),
            api: api,
            requestId: 'req-1',
          ),
        ),
      );
      await tester.pumpAndSettle();
      final prStep = find.byKey(const ValueKey('pipeline-step-pr_review'));
      expect(
        find.descendant(of: prStep, matching: find.byIcon(Icons.error)),
        findsNothing,
      );
      expect(
        find.descendant(
          of: prStep,
          matching: find.byIcon(Icons.pending_actions),
        ),
        findsOneWidget,
      );
      expect(tester.takeException(), isNull);
    },
  );

  testWidgets(
    'Tickets sit right under the stepper once tickets are building, and '
    'below the plan before that',
    (tester) async {
      var state = 'plan_review';
      final client = MockClient(
        (request) async => http.Response(
          requestJson(
            id: 'req-1',
            state: state,
            title: 'Order',
            tickets: [requestTicketJson(index: 1, runId: 'run-1')],
          ),
          200,
        ),
      );
      final api = RunApi(baseUrl: 'http://factory.test', client: client);
      tester.view.physicalSize = const Size(1600, 6000);
      tester.view.devicePixelRatio = 1.0;
      addTearDown(tester.view.resetPhysicalSize);
      addTearDown(tester.view.resetDevicePixelRatio);

      Future<bool> ticketsAboveRequest(String key) async {
        await tester.pumpWidget(
          MaterialApp(
            home: RequestDetailScreen(
              key: ValueKey(key),
              api: api,
              requestId: 'req-1',
            ),
          ),
        );
        await tester.pumpAndSettle();
        final tickets = tester.getTopLeft(find.text('Tickets').first).dy;
        final req = tester.getTopLeft(find.text('Request').first).dy;
        return tickets < req;
      }

      expect(await ticketsAboveRequest('review'), isFalse);
      state = 'building';
      expect(await ticketsAboveRequest('building'), isTrue);
    },
  );

  testWidgets(
    'accepted-awaiting-PR shows a neutral "built and verified" callout whose '
    'retry says it rebuilds (walkthrough 2026-09-24)',
    (tester) async {
      final client = MockClient((request) async {
        return http.Response(
          requestJson(
            id: 'req-1',
            state: 'halted',
            haltKind: 'accepted_no_pr',
            title: 'Accepted, no PR',
            nextAction: 'Merge branch factoryd/run-1 by hand.',
            tickets: [requestTicketJson(index: 1, runId: 'run-1')],
          ),
          200,
        );
      });
      final api = RunApi(
        baseUrl: 'http://factory.test',
        client: client,
        overrideToken: 'test-token',
      );

      await tester.pumpWidget(
        MaterialApp(
          home: RequestDetailScreen(api: api, requestId: 'req-1'),
        ),
      );
      await tester.pumpAndSettle();

      expect(
        find.text('Built and verified; no pull request was opened.'),
        findsOneWidget,
      );
      expect(find.text('This request is halted.'), findsNothing);
      expect(find.text('Retry request (rebuilds)'), findsOneWidget);
      expect(find.text('Cancel request'), findsOneWidget);
      expect(find.text('Merge branch factoryd/run-1 by hand.'), findsOneWidget);
    },
  );

  testWidgets('the quarantined callout links to the run override form with an '
      'override token configured', (tester) async {
    final client = MockClient((request) async {
      return http.Response(
        requestJson(
          id: 'req-1',
          state: 'quarantined',
          title: 'Quarantined',
          ticketIndex: 1,
          tickets: [requestTicketJson(index: 1, runId: 'run-1')],
        ),
        200,
      );
    });
    final api = RunApi(
      baseUrl: 'http://factory.test',
      client: client,
      overrideToken: 'test-token',
    );

    await tester.pumpWidget(
      MaterialApp(
        home: RequestDetailScreen(api: api, requestId: 'req-1'),
      ),
    );
    await tester.pumpAndSettle();

    expect(find.byKey(const ValueKey('recovery-callout')), findsOneWidget);
    expect(
      find.byKey(const ValueKey('quarantined-override-link')),
      findsOneWidget,
    );
    // Regression: retry and the run override are independent actions,
    // not a required sequence -- the callout must name factoryd retry as
    // the actual recovery action and say the override is a separate,
    // optional one, not imply retry needs the override first (found in
    // review).
    expect(find.textContaining('factoryd retry req-1'), findsOneWidget);
    expect(find.textContaining('separate, optional action'), findsOneWidget);
  });

  testWidgets(
    'compare-with-revision is opt-in and renders a diff once a revision '
    'is selected',
    (tester) async {
      final client = MockClient((request) async {
        if (request.url.path == '/requests/req-1/revisions') {
          return http.Response(
            '[${revisionSummaryJson(index: 1, at: '2026-09-10T09:00:00Z', by: 'jane', reason: 'too broad', fromState: 'spec_review', files: ['spec.md'])}]',
            200,
          );
        }
        if (request.url.path == '/requests/req-1/revisions/1') {
          return http.Response(
            revisionDetailJson(
              index: 1,
              at: '2026-09-10T09:00:00Z',
              by: 'jane',
              reason: 'too broad',
              fromState: 'spec_review',
              files: {'spec.md': '# Old spec\n\nOld detail.'},
            ),
            200,
          );
        }
        return http.Response(
          requestJson(
            id: 'req-1',
            state: 'spec_review',
            title: 'Rejected once',
            spec: '# New spec\n\nNew detail.',
            rejections: [
              requestRejectionJson(
                by: 'jane',
                at: '2026-09-10T09:00:00Z',
                reason: 'too broad',
                fromState: 'spec_review',
              ),
            ],
          ),
          200,
        );
      });
      final api = RunApi(baseUrl: 'http://factory.test', client: client);

      tester.view.physicalSize = const Size(1200, 2400);
      tester.view.devicePixelRatio = 1.0;
      addTearDown(tester.view.resetPhysicalSize);
      addTearDown(tester.view.resetDevicePixelRatio);

      await tester.pumpWidget(
        MaterialApp(
          home: RequestDetailScreen(api: api, requestId: 'req-1'),
        ),
      );
      await tester.pumpAndSettle();

      // Default view: no diff shown, current content only.
      expect(find.byKey(const ValueKey('revision-diff')), findsNothing);
      expect(find.textContaining('New detail.'), findsOneWidget);

      await tester.tap(find.byKey(const ValueKey('compare-revision-toggle')));
      await tester.pumpAndSettle();

      await tester.tap(find.byKey(const ValueKey('revision-select')));
      await tester.pumpAndSettle();
      await tester.tap(
        find
            .text(
              'Revision 1 — rejected by jane at '
              '${formatLocalTimestamp('2026-09-10T09:00:00Z')}',
            )
            .last,
      );
      await tester.pumpAndSettle();

      expect(find.byKey(const ValueKey('revision-diff')), findsOneWidget);
      expect(find.textContaining('Old detail.'), findsWidgets);
      expect(find.textContaining('New detail.'), findsWidgets);
    },
  );

  // Pipeline stepper: "where is my ask in the pipeline?" -- a horizontal
  // stepper over the canonical states, each one
  // showing the history entry that reached it, never a bare state word.
  testWidgets(
    'the pipeline stepper renders the right glyphs and timestamps for a '
    '4-entry history (a reject loop revisits spec_drafting/spec_review)',
    (tester) async {
      final client = MockClient((request) async {
        return http.Response(
          requestJson(
            id: 'req-1',
            state: 'spec_review',
            title: 'Add idempotency keys',
            history: [
              requestHistoryEntryJson(
                from: 'submitted',
                to: 'spec_drafting',
                at: '2026-09-15T09:00:00Z',
                by: 'factory',
              ),
              requestHistoryEntryJson(
                from: 'spec_drafting',
                to: 'spec_review',
                at: '2026-09-15T09:05:00Z',
                by: 'factory',
                reason: 'spec drafted',
              ),
              requestHistoryEntryJson(
                from: 'spec_review',
                to: 'spec_drafting',
                at: '2026-09-15T09:10:00Z',
                by: 'bob',
                reason: 'too vague',
              ),
              requestHistoryEntryJson(
                from: 'spec_drafting',
                to: 'spec_review',
                at: '2026-09-15T09:20:00Z',
                by: 'factory',
                reason: 'redrafted after feedback',
              ),
            ],
          ),
          200,
        );
      });
      final api = RunApi(baseUrl: 'http://factory.test', client: client);

      await tester.pumpWidget(
        MaterialApp(
          home: RequestDetailScreen(api: api, requestId: 'req-1'),
        ),
      );
      await tester.pumpAndSettle();

      // submitted: done.
      expect(
        find.descendant(
          of: find.byKey(const ValueKey('pipeline-step-submitted')),
          matching: find.byIcon(Icons.check_circle),
        ),
        findsOneWidget,
      );
      // spec_drafting: also done (it was left again after the reject
      // loop), and its shown entry is the LATEST one to reach it (bob's
      // rejection), not the original one.
      expect(
        find.descendant(
          of: find.byKey(const ValueKey('pipeline-step-spec_drafting')),
          matching: find.byIcon(Icons.check_circle),
        ),
        findsOneWidget,
      );
      expect(
        find.descendant(
          of: find.byKey(const ValueKey('pipeline-step-spec_drafting')),
          matching: find.textContaining('bob'),
        ),
        findsOneWidget,
      );
      expect(
        find.descendant(
          of: find.byKey(const ValueKey('pipeline-step-spec_drafting')),
          matching: find.text('too vague'),
        ),
        findsOneWidget,
      );
      // spec_review: current, showing the latest (redraft) entry, not the
      // first one.
      expect(
        find.descendant(
          of: find.byKey(const ValueKey('pipeline-step-spec_review')),
          matching: find.byIcon(Icons.radio_button_checked),
        ),
        findsOneWidget,
      );
      // Not the first entry that reached spec_review -- the latest one,
      // by reason (a time-of-day assertion would be timezone-dependent
      // since the widget renders in local time).
      expect(
        find.descendant(
          of: find.byKey(const ValueKey('pipeline-step-spec_review')),
          matching: find.text('redrafted after feedback'),
        ),
        findsOneWidget,
      );
      // planning: pending, nothing under it.
      expect(
        find.descendant(
          of: find.byKey(const ValueKey('pipeline-step-planning')),
          matching: find.byIcon(Icons.radio_button_unchecked),
        ),
        findsOneWidget,
      );
    },
  );

  testWidgets(
    'the pipeline stepper hides the opt-in oracle steps entirely unless '
    'the request actually visited one',
    (tester) async {
      Future<void> pump(String state, List<String> history) async {
        final client = MockClient((request) async {
          return http.Response(
            requestJson(
              id: 'req-1',
              state: state,
              title: 'Oracle stage',
              history: [
                for (var i = 0; i + 1 < history.length; i++)
                  requestHistoryEntryJson(
                    from: history[i],
                    to: history[i + 1],
                    at: '2026-09-19T09:0$i:00Z',
                    by: 'factory',
                  ),
              ],
            ),
            200,
          );
        });
        final api = RunApi(baseUrl: 'http://factory.test', client: client);
        // A fresh tree per call: RequestDetailScreen keeps its loaded request
        // in State, which a plain second pumpWidget would reuse.
        await tester.pumpWidget(const SizedBox());
        await tester.pumpWidget(
          MaterialApp(
            home: RequestDetailScreen(api: api, requestId: 'req-1'),
          ),
        );
        await tester.pumpAndSettle();
      }

      // Flag-less and still before planning: not rendered at all.
      await pump('spec_review', ['spec_drafting', 'spec_review']);
      expect(
        find.byKey(const ValueKey('pipeline-step-oracle_review')),
        findsNothing,
      );

      // Flag-less and past the oracle stage: hidden entirely (C1, operator
      // demo, 2026-09-26 -- previously shown as a "skipped" step nobody
      // could act on).
      await pump('planning', ['spec_review', 'planning']);
      for (final step in ['oracle_drafting', 'oracle_review']) {
        expect(find.byKey(ValueKey('pipeline-step-$step')), findsNothing);
      }

      // oracle_review: labelled, current, and Approve/Reject offered.
      await pump('oracle_review', [
        'spec_review',
        'oracle_drafting',
        'oracle_review',
      ]);
      expect(find.text('Oracle drafting'), findsOneWidget);
      expect(
        find.descendant(
          of: find.byKey(const ValueKey('pipeline-step-oracle_review')),
          matching: find.byIcon(Icons.radio_button_checked),
        ),
        findsOneWidget,
      );
      expect(
        find.descendant(
          of: find.byKey(const ValueKey('pipeline-step-oracle_drafting')),
          matching: find.byIcon(Icons.check_circle),
        ),
        findsOneWidget,
      );
    },
  );

  // C1 (operator demo, 2026-09-26): a Wrap of ~10 steps at 180px+16px
  // spacing pushed "Done" onto a second line even at a normal desktop
  // width. Every step must now sit in a single row -- verified the only
  // way that's observable from outside the widget: every pipeline-step-*
  // key renders at the same vertical position (dy).
  testWidgets(
    'the pipeline stepper renders every step in a single row at 1440px',
    (tester) async {
      tester.view.physicalSize = const Size(1440, 1200);
      tester.view.devicePixelRatio = 1.0;
      addTearDown(tester.view.resetPhysicalSize);
      addTearDown(tester.view.resetDevicePixelRatio);

      final client = MockClient((request) async {
        return http.Response(
          requestJson(
            id: 'req-1',
            state: 'building',
            title: 'Wide layout',
            history: [
              requestHistoryEntryJson(
                from: 'submitted',
                to: 'spec_drafting',
                at: '2026-09-15T09:00:00Z',
                by: 'factory',
              ),
              requestHistoryEntryJson(
                from: 'spec_drafting',
                to: 'spec_review',
                at: '2026-09-15T09:05:00Z',
                by: 'factory',
              ),
              requestHistoryEntryJson(
                from: 'spec_review',
                to: 'planning',
                at: '2026-09-15T09:10:00Z',
                by: 'jane',
              ),
              requestHistoryEntryJson(
                from: 'planning',
                to: 'plan_review',
                at: '2026-09-15T09:15:00Z',
                by: 'factory',
              ),
              requestHistoryEntryJson(
                from: 'plan_review',
                to: 'building',
                at: '2026-09-15T09:20:00Z',
                by: 'jane',
              ),
            ],
          ),
          200,
        );
      });
      final api = RunApi(baseUrl: 'http://factory.test', client: client);

      await tester.pumpWidget(
        MaterialApp(
          home: RequestDetailScreen(api: api, requestId: 'req-1'),
        ),
      );
      await tester.pumpAndSettle();

      // No oracle stage was visited, so those two steps are hidden --
      // the remaining eight (submitted..done) must all share one dy.
      const steps = [
        'submitted',
        'spec_drafting',
        'spec_review',
        'planning',
        'plan_review',
        'building',
        'pr_review',
        'done',
      ];
      final dys = [
        for (final step in steps)
          tester.getTopLeft(find.byKey(ValueKey('pipeline-step-$step'))).dy,
      ];
      expect(dys.toSet(), hasLength(1));
    },
  );

  testWidgets('the pipeline stepper marks the failed step for a halted '
      'request, using the halt error as that step\'s own reason', (
    tester,
  ) async {
    final client = MockClient((request) async {
      return http.Response(
        requestJson(
          id: 'req-1',
          state: 'halted',
          title: 'Halted mid-build',
          error: 'ticket 1/2 halted: build failed',
          history: [
            requestHistoryEntryJson(
              from: 'plan_review',
              to: 'building',
              at: '2026-09-15T09:00:00Z',
              by: 'jane',
              reason: 'approved',
            ),
            requestHistoryEntryJson(
              from: 'building',
              to: 'halted',
              at: '2026-09-15T09:30:00Z',
              by: 'factory',
              reason: 'ticket 1/2 halted: build failed',
            ),
          ],
        ),
        200,
      );
    });
    final api = RunApi(baseUrl: 'http://factory.test', client: client);

    await tester.pumpWidget(
      MaterialApp(
        home: RequestDetailScreen(api: api, requestId: 'req-1'),
      ),
    );
    await tester.pumpAndSettle();

    expect(
      find.descendant(
        of: find.byKey(const ValueKey('pipeline-step-building')),
        matching: find.byIcon(Icons.error),
      ),
      findsOneWidget,
    );
    expect(
      find.descendant(
        of: find.byKey(const ValueKey('pipeline-step-building')),
        matching: find.text('ticket 1/2 halted: build failed'),
      ),
      findsOneWidget,
    );
    // Every step after the failure point stays pending.
    expect(
      find.descendant(
        of: find.byKey(const ValueKey('pipeline-step-pr_review')),
        matching: find.byIcon(Icons.radio_button_unchecked),
      ),
      findsOneWidget,
    );
  });

  testWidgets(
    'the pipeline stepper still renders a legacy request with no history, '
    'from state/enteredAt alone',
    (tester) async {
      final client = MockClient((request) async {
        return http.Response(
          requestJson(
            id: 'req-1',
            state: 'building',
            title: 'Legacy request',
            enteredAt: '2026-09-15T09:00:00Z',
            ticketIndex: 1,
            ticketCount: 2,
          ),
          200,
        );
      });
      final api = RunApi(baseUrl: 'http://factory.test', client: client);

      await tester.pumpWidget(
        MaterialApp(
          home: RequestDetailScreen(api: api, requestId: 'req-1'),
        ),
      );
      await tester.pumpAndSettle();

      expect(tester.takeException(), isNull);
      expect(
        find.descendant(
          of: find.byKey(const ValueKey('pipeline-step-building')),
          matching: find.byIcon(Icons.radio_button_checked),
        ),
        findsOneWidget,
      );
      // No history to draw an entry from, but the building step is
      // current, so it still shows ticket progress and elapsed time
      // rather than nothing at all.
      expect(
        find.descendant(
          of: find.byKey(const ValueKey('pipeline-step-building')),
          matching: find.textContaining('ticket 1/2'),
        ),
        findsOneWidget,
      );
    },
  );

  testWidgets('compare-with-revision matches a ticket by its absolute spec_path '
      'against the revision\'s relative key (regression: a real '
      'RequestTicket.specPath is absolute in production, unlike this '
      'suite\'s other fixtures)', (tester) async {
    final client = MockClient((request) async {
      if (request.url.path == '/requests/req-1/revisions') {
        return http.Response(
          '[${revisionSummaryJson(index: 1, at: '2026-09-10T09:00:00Z', by: 'jane', reason: 'too broad', fromState: 'plan_review', files: ['tickets/001.spec.md'])}]',
          200,
        );
      }
      if (request.url.path == '/requests/req-1/revisions/1') {
        return http.Response(
          revisionDetailJson(
            index: 1,
            at: '2026-09-10T09:00:00Z',
            by: 'jane',
            reason: 'too broad',
            fromState: 'plan_review',
            files: {'tickets/001.spec.md': 'Old ticket detail.'},
          ),
          200,
        );
      }
      return http.Response(
        requestJson(
          id: 'req-1',
          state: 'plan_review',
          title: 'Rejected once',
          tickets: [
            requestTicketJson(
              index: 1,
              // Absolute, as cmd/factoryd/request_driver.go actually
              // builds it -- not the bare relative key SnapshotRevision
              // stores.
              specPath: '/data/requests/req-1/tickets/001.spec.md',
              content: 'New ticket detail.',
            ),
          ],
          rejections: [
            requestRejectionJson(
              by: 'jane',
              at: '2026-09-10T09:00:00Z',
              reason: 'too broad',
              fromState: 'plan_review',
            ),
          ],
        ),
        200,
      );
    });
    final api = RunApi(baseUrl: 'http://factory.test', client: client);

    tester.view.physicalSize = const Size(1200, 2400);
    tester.view.devicePixelRatio = 1.0;
    addTearDown(tester.view.resetPhysicalSize);
    addTearDown(tester.view.resetDevicePixelRatio);

    await tester.pumpWidget(
      MaterialApp(
        home: RequestDetailScreen(api: api, requestId: 'req-1'),
      ),
    );
    await tester.pumpAndSettle();

    await tester.tap(find.byKey(const ValueKey('compare-revision-toggle')));
    await tester.pumpAndSettle();
    await tester.tap(find.byKey(const ValueKey('revision-select')));
    await tester.pumpAndSettle();
    await tester.tap(
      find
          .text(
            'Revision 1 — rejected by jane at '
            '${formatLocalTimestamp('2026-09-10T09:00:00Z')}',
          )
          .last,
    );
    await tester.pumpAndSettle();

    // Before the fix, the absolute specPath never matched the
    // revision's relative key, so _currentContentFor fell through to
    // '' and the diff computed against an empty "current" side -- no
    // '+' line for the ticket's real current content. Scoped to
    // descendants of 'revision-diff' itself (not the whole screen) so
    // this can't pass merely because the ticket's own content section
    // shows the same text elsewhere on the page.
    final diffFinder = find.byKey(const ValueKey('revision-diff'));
    expect(diffFinder, findsOneWidget);
    expect(
      find.descendant(
        of: diffFinder,
        matching: find.textContaining('Old ticket detail.'),
      ),
      findsWidgets,
    );
    expect(
      find.descendant(
        of: diffFinder,
        matching: find.textContaining('New ticket detail.'),
      ),
      findsWidgets,
    );
  });

  testWidgets(
    'the spec Edit toggle shows an editor pre-filled with the current content',
    (tester) async {
      final client = MockClient((request) async {
        return http.Response(
          requestJson(
            id: 'req-1',
            state: 'spec_review',
            title: 'Add idempotency keys',
            spec: '# Spec\n\nOriginal detail.',
          ),
          200,
        );
      });
      final api = RunApi(
        baseUrl: 'http://factory.test',
        client: client,
        overrideToken: 'test-token',
      );

      await tester.pumpWidget(
        MaterialApp(
          home: RequestDetailScreen(api: api, requestId: 'req-1'),
        ),
      );
      await tester.pumpAndSettle();

      expect(
        find.byKey(const ValueKey('edit-content-field-spec.md')),
        findsNothing,
      );
      await tester.tap(
        find.byKey(const ValueKey('edit-content-button-spec.md')),
      );
      await tester.pumpAndSettle();

      final field = find.byKey(const ValueKey('edit-content-field-spec.md'));
      expect(field, findsOneWidget);
      expect(
        tester.widget<TextField>(field).controller!.text,
        '# Spec\n\nOriginal detail.',
      );
    },
  );

  testWidgets('saving an edited spec calls PUT with the edited body', (
    tester,
  ) async {
    String? sentMethod;
    String? sentBody;
    final client = MockClient((request) async {
      if (request.url.path == '/requests/req-1/spec') {
        sentMethod = request.method;
        sentBody = request.body;
        return http.Response(
          requestJson(
            id: 'req-1',
            state: 'spec_review',
            title: 'Add idempotency keys',
            spec: '# Spec\n\nEdited detail.',
          ),
          200,
        );
      }
      return http.Response(
        requestJson(
          id: 'req-1',
          state: 'spec_review',
          title: 'Add idempotency keys',
          spec: '# Spec\n\nOriginal detail.',
        ),
        200,
      );
    });
    final api = RunApi(
      baseUrl: 'http://factory.test',
      client: client,
      overrideToken: 'test-token',
    );

    await tester.pumpWidget(
      MaterialApp(
        home: RequestDetailScreen(api: api, requestId: 'req-1'),
      ),
    );
    await tester.pumpAndSettle();

    await tester.tap(find.byKey(const ValueKey('edit-content-button-spec.md')));
    await tester.pumpAndSettle();
    await tester.enterText(
      find.byKey(const ValueKey('edit-content-field-spec.md')),
      '# Spec\n\nEdited detail.',
    );
    await tester.ensureVisible(
      find.byKey(const ValueKey('save-content-button-spec.md')),
    );
    await tester.pumpAndSettle();
    await tester.tap(find.byKey(const ValueKey('save-content-button-spec.md')));
    await tester.pumpAndSettle();

    expect(sentMethod, 'PUT');
    // base_sha256 is the hash of the content the editor was opened
    // with, not something this test can predict without duplicating
    // sha256Hex -- assert the shape instead of the exact hash.
    final decodedBody = jsonDecode(sentBody!) as Map<String, dynamic>;
    expect(decodedBody['content'], '# Spec\n\nEdited detail.');
    expect(decodedBody['base_sha256'], isA<String>());
    expect((decodedBody['base_sha256'] as String).isNotEmpty, isTrue);
    // The editor closes and the screen shows the saved content, fed
    // straight from the PUT response (no extra GET round trip needed).
    expect(
      find.byKey(const ValueKey('edit-content-field-spec.md')),
      findsNothing,
    );
    expect(find.textContaining('Edited detail.'), findsOneWidget);
  });

  testWidgets('a 422 from saving an edited spec renders the message inline', (
    tester,
  ) async {
    final client = MockClient((request) async {
      if (request.url.path == '/requests/req-1/spec') {
        return http.Response(
          jsonEncode({'error': 'spec is missing required heading "## Risks"'}),
          422,
        );
      }
      return http.Response(
        requestJson(
          id: 'req-1',
          state: 'spec_review',
          title: 'Add idempotency keys',
          spec: '# Spec\n\nOriginal detail.',
        ),
        200,
      );
    });
    final api = RunApi(
      baseUrl: 'http://factory.test',
      client: client,
      overrideToken: 'test-token',
    );

    await tester.pumpWidget(
      MaterialApp(
        home: RequestDetailScreen(api: api, requestId: 'req-1'),
      ),
    );
    await tester.pumpAndSettle();

    await tester.tap(find.byKey(const ValueKey('edit-content-button-spec.md')));
    await tester.pumpAndSettle();
    await tester.enterText(
      find.byKey(const ValueKey('edit-content-field-spec.md')),
      'not a spec',
    );
    await tester.ensureVisible(
      find.byKey(const ValueKey('save-content-button-spec.md')),
    );
    await tester.pumpAndSettle();
    await tester.tap(find.byKey(const ValueKey('save-content-button-spec.md')));
    await tester.pumpAndSettle();

    expect(
      find.textContaining('spec is missing required heading "## Risks"'),
      findsOneWidget,
    );
    // The editor stays open with the operator's edit intact, rather than
    // silently discarding it on a rejected save.
    expect(
      find.byKey(const ValueKey('edit-content-field-spec.md')),
      findsOneWidget,
    );
  });

  testWidgets(
    'Approve still calls the approve route with the Edit control present',
    (tester) async {
      bool approveCalled = false;
      final client = MockClient((request) async {
        if (request.method == 'POST' &&
            request.url.path == '/requests/req-1/approve') {
          approveCalled = true;
          return http.Response(
            requestJson(
              id: 'req-1',
              state: 'planning',
              title: 'Add idempotency keys',
            ),
            200,
          );
        }
        return http.Response(
          requestJson(
            id: 'req-1',
            state: 'spec_review',
            title: 'Add idempotency keys',
            spec: '# Spec\n\nOriginal detail.',
          ),
          200,
        );
      });
      final api = RunApi(
        baseUrl: 'http://factory.test',
        client: client,
        overrideToken: 'test-token',
      );

      await tester.pumpWidget(
        MaterialApp(
          home: RequestDetailScreen(api: api, requestId: 'req-1'),
        ),
      );
      await tester.pumpAndSettle();

      expect(
        find.byKey(const ValueKey('edit-content-button-spec.md')),
        findsOneWidget,
      );
      await tester.tap(find.byKey(const ValueKey('approve-request-button')));
      await tester.pumpAndSettle();
      await tester.tap(find.byKey(const ValueKey('approve-confirm-button')));
      await tester.pumpAndSettle();

      expect(approveCalled, isTrue);
    },
  );

  testWidgets(
    'the Next banner shows the server-supplied next_action prominently '
    'for a non-terminal state',
    (tester) async {
      final client = MockClient((request) async {
        return http.Response(
          requestJson(
            id: 'req-1',
            state: 'building',
            title: 'Add idempotency keys',
            nextAction: 'Wait for the build to finish.',
          ),
          200,
        );
      });
      final api = RunApi(baseUrl: 'http://factory.test', client: client);

      await tester.pumpWidget(
        MaterialApp(
          home: RequestDetailScreen(api: api, requestId: 'req-1'),
        ),
      );
      await tester.pumpAndSettle();

      expect(find.byKey(const ValueKey('next-action-banner')), findsOneWidget);
      expect(find.text('Wait for the build to finish.'), findsOneWidget);
    },
  );

  testWidgets(
    'Retry on a halted request calls POST /requests/{id}/retry with a '
    'reason',
    (tester) async {
      String? sentBody;
      final client = MockClient((request) async {
        if (request.method == 'POST' &&
            request.url.path == '/requests/req-1/retry') {
          sentBody = request.body;
          return http.Response(
            requestJson(id: 'req-1', state: 'building', title: 'T'),
            200,
          );
        }
        return http.Response(
          requestJson(id: 'req-1', state: 'halted', title: 'T'),
          200,
        );
      });
      final api = RunApi(
        baseUrl: 'http://factory.test',
        client: client,
        overrideToken: 'test-token',
      );

      await tester.pumpWidget(
        MaterialApp(
          home: RequestDetailScreen(api: api, requestId: 'req-1'),
        ),
      );
      await tester.pumpAndSettle();

      await tester.tap(find.byKey(const ValueKey('retry-request-button')));
      await tester.pumpAndSettle();
      await tester.enterText(
        find.byKey(const ValueKey('retry-reason')),
        'fixed the setup error',
      );
      await tester.tap(find.byKey(const ValueKey('retry-confirm-button')));
      await tester.pumpAndSettle();

      expect(sentBody, isNotNull);
      final decoded = jsonDecode(sentBody!) as Map<String, dynamic>;
      expect(decoded['reason'], 'fixed the setup error');
      expect(decoded['by'], 'operator');
    },
  );

  testWidgets(
    'Cancel on a quarantined request calls POST /requests/{id}/cancel with '
    'a reason',
    (tester) async {
      String? sentBody;
      final client = MockClient((request) async {
        if (request.method == 'POST' &&
            request.url.path == '/requests/req-1/cancel') {
          sentBody = request.body;
          return http.Response(
            requestJson(id: 'req-1', state: 'cancelled', title: 'T'),
            200,
          );
        }
        return http.Response(
          requestJson(id: 'req-1', state: 'quarantined', title: 'T'),
          200,
        );
      });
      final api = RunApi(
        baseUrl: 'http://factory.test',
        client: client,
        overrideToken: 'test-token',
      );

      await tester.pumpWidget(
        MaterialApp(
          home: RequestDetailScreen(api: api, requestId: 'req-1'),
        ),
      );
      await tester.pumpAndSettle();

      await tester.tap(find.byKey(const ValueKey('cancel-request-button')));
      await tester.pumpAndSettle();
      await tester.enterText(
        find.byKey(const ValueKey('cancel-reason')),
        'no longer needed',
      );
      await tester.tap(find.byKey(const ValueKey('cancel-confirm-button')));
      await tester.pumpAndSettle();

      expect(sentBody, isNotNull);
      final decoded = jsonDecode(sentBody!) as Map<String, dynamic>;
      expect(decoded['reason'], 'no longer needed');
    },
  );

  testWidgets(
    'the send-back button is shown for a quarantined request that can '
    'send back',
    (tester) async {
      final client = MockClient(
        (request) async => http.Response(
          requestJson(
            id: 'req-1',
            state: 'quarantined',
            title: 'T',
            canSendBack: true,
          ),
          200,
        ),
      );
      final api = RunApi(
        baseUrl: 'http://factory.test',
        client: client,
        overrideToken: 'test-token',
      );

      await tester.pumpWidget(
        MaterialApp(
          home: RequestDetailScreen(api: api, requestId: 'req-1'),
        ),
      );
      await tester.pumpAndSettle();

      expect(
        find.byKey(const ValueKey('send-back-request-button')),
        findsOneWidget,
      );
    },
  );

  testWidgets(
    'the send-back button is absent when the server says a ticket has '
    'already been accepted',
    (tester) async {
      // A separate widget tree from the test above (pumpWidget reuses
      // State -- and so its already-fetched request -- across calls when
      // the widget type is unchanged, so exercising "shown" and "hidden"
      // in one test would silently check the first fetch twice).
      final client = MockClient(
        (request) async => http.Response(
          requestJson(
            id: 'req-2',
            state: 'quarantined',
            title: 'T',
            canSendBack: false,
          ),
          200,
        ),
      );
      final api = RunApi(
        baseUrl: 'http://factory.test',
        client: client,
        overrideToken: 'test-token',
      );

      await tester.pumpWidget(
        MaterialApp(
          home: RequestDetailScreen(api: api, requestId: 'req-2'),
        ),
      );
      await tester.pumpAndSettle();

      expect(
        find.byKey(const ValueKey('send-back-request-button')),
        findsNothing,
      );
    },
  );

  testWidgets(
    'Send back on a quarantined request calls POST /requests/{id}/reject '
    'with reason and to',
    (tester) async {
      String? sentBody;
      final client = MockClient((request) async {
        if (request.method == 'POST' &&
            request.url.path == '/requests/req-1/reject') {
          sentBody = request.body;
          return http.Response(
            requestJson(id: 'req-1', state: 'planning', title: 'T'),
            200,
          );
        }
        return http.Response(
          requestJson(
            id: 'req-1',
            state: 'quarantined',
            title: 'T',
            canSendBack: true,
            canSendBackToPlan: true,
          ),
          200,
        );
      });
      final api = RunApi(
        baseUrl: 'http://factory.test',
        client: client,
        overrideToken: 'test-token',
      );

      await tester.pumpWidget(
        MaterialApp(
          home: RequestDetailScreen(api: api, requestId: 'req-1'),
        ),
      );
      await tester.pumpAndSettle();

      await tester.tap(find.byKey(const ValueKey('send-back-request-button')));
      await tester.pumpAndSettle();
      await tester.enterText(
        find.byKey(const ValueKey('send-back-reason')),
        'diff_scope: allow the contract test',
      );
      await tester.tap(find.byKey(const ValueKey('send-back-confirm-button')));
      await tester.pumpAndSettle();

      expect(sentBody, isNotNull);
      final decoded = jsonDecode(sentBody!) as Map<String, dynamic>;
      expect(decoded['reason'], 'diff_scope: allow the contract test');
      expect(decoded['to'], 'plan');
    },
  );

  testWidgets(
    'Send back defaults to spec when the server disallows the plan target '
    '(can_send_back_to_plan false)',
    (tester) async {
      String? sentBody;
      final client = MockClient((request) async {
        if (request.method == 'POST' &&
            request.url.path == '/requests/req-1/reject') {
          sentBody = request.body;
          return http.Response(
            requestJson(id: 'req-1', state: 'planning', title: 'T'),
            200,
          );
        }
        return http.Response(
          requestJson(
            id: 'req-1',
            state: 'quarantined',
            title: 'T',
            canSendBack: true,
          ),
          200,
        );
      });
      final api = RunApi(
        baseUrl: 'http://factory.test',
        client: client,
        overrideToken: 'test-token',
      );

      await tester.pumpWidget(
        MaterialApp(
          home: RequestDetailScreen(api: api, requestId: 'req-1'),
        ),
      );
      await tester.pumpAndSettle();

      await tester.tap(find.byKey(const ValueKey('send-back-request-button')));
      await tester.pumpAndSettle();
      await tester.enterText(
        find.byKey(const ValueKey('send-back-reason')),
        'diff_scope: allow the contract test',
      );
      await tester.tap(find.byKey(const ValueKey('send-back-confirm-button')));
      await tester.pumpAndSettle();

      expect(sentBody, isNotNull);
      final decoded = jsonDecode(sentBody!) as Map<String, dynamic>;
      expect(decoded['reason'], 'diff_scope: allow the contract test');
      expect(decoded['to'], 'spec');
    },
  );

  // Follow-up B (operator demo, 2026-09-26): the send-back button's label
  // and the recovery callout's CLI hint must both name the actual target
  // (spec) rather than always reading "planning" / `factoryd retry`.
  testWidgets('the send-back button reads "Send back to spec" when only the '
      'spec target is allowed, and the CLI hint stays on the leading Retry', (
    tester,
  ) async {
    final client = MockClient(
      (request) async => http.Response(
        requestJson(
          id: 'req-1',
          state: 'quarantined',
          title: 'T',
          canSendBack: true,
        ),
        200,
      ),
    );
    final api = RunApi(
      baseUrl: 'http://factory.test',
      client: client,
      overrideToken: 'test-token',
    );

    await tester.pumpWidget(
      MaterialApp(
        home: RequestDetailScreen(api: api, requestId: 'req-1'),
      ),
    );
    await tester.pumpAndSettle();

    expect(find.text('Send back to spec'), findsOneWidget);
    expect(find.text('Send back to planning'), findsNothing);
    // Retry leads for a quarantine that isn't spec_conformity, so the
    // copyable command is the retry, not a send-back.
    expect(find.text('factoryd retry req-1'), findsOneWidget);
    expect(find.textContaining('factoryd reject'), findsNothing);
  });

  // Follow-up B (operator demo, 2026-09-26): a spec_conformity quarantine's
  // only sensible recovery is sending it back to spec, so that action
  // leads (a FilledButton, ahead of Retry) rather than sitting last as a
  // plain outlined button behind the generic Retry/Cancel pair.
  testWidgets('"Send back to spec" is the primary, first action for a '
      'spec_conformity quarantine', (tester) async {
    final client = MockClient(
      (request) async => http.Response(
        requestJson(
          id: 'req-1',
          state: 'quarantined',
          title: 'T',
          canSendBack: true,
          canSendBackToPlan: true,
          quarantineCheck: 'spec_conformity',
        ),
        200,
      ),
    );
    final api = RunApi(
      baseUrl: 'http://factory.test',
      client: client,
      overrideToken: 'test-token',
    );

    await tester.pumpWidget(
      MaterialApp(
        home: RequestDetailScreen(api: api, requestId: 'req-1'),
      ),
    );
    await tester.pumpAndSettle();

    final sendBackFinder = find.byKey(
      const ValueKey('send-back-request-button'),
    );
    expect(sendBackFinder, findsOneWidget);
    // A FilledButton (or its _FilledButtonWithIcon subtype, which
    // find.byType's exact-runtimeType match would miss) -- the key sits
    // on the button widget itself, not a descendant of it.
    expect(tester.widget(sendBackFinder), isA<FilledButton>());
    // "First action": send-back sits before Retry among the recovery
    // callout's own action buttons.
    final calloutFinder = find.byKey(const ValueKey('recovery-callout'));
    final sendBackTop = tester
        .getTopLeft(
          find.descendant(of: calloutFinder, matching: sendBackFinder),
        )
        .dx;
    final retryTop = tester
        .getTopLeft(
          find.descendant(
            of: calloutFinder,
            matching: find.byKey(const ValueKey('retry-request-button')),
          ),
        )
        .dx;
    expect(sendBackTop, lessThan(retryTop));
    // Targets spec even though planning is allowed, and the CLI hint
    // mirrors that leading action.
    expect(find.text('Send back to spec'), findsOneWidget);
    expect(
      find.text('factoryd reject -to spec -reason "<what to change>" req-1'),
      findsOneWidget,
    );
  });

  testWidgets('the acceptance criteria list renders numbered items parsed from '
      'the spec, with a count, under the spec editor', (tester) async {
    const spec =
        '# Spec\n\n'
        '## Acceptance criteria\n\n'
        '1. Adding two numbers returns their sum\n'
        '2. Dividing by zero raises ValueError\n\n'
        '## Non-goals\n\n'
        'Not covering strings.\n';
    final client = MockClient((request) async {
      return http.Response(
        requestJson(id: 'req-1', state: 'spec_review', title: 'T', spec: spec),
        200,
      );
    });
    final api = RunApi(baseUrl: 'http://factory.test', client: client);

    await tester.pumpWidget(
      MaterialApp(
        home: RequestDetailScreen(api: api, requestId: 'req-1'),
      ),
    );
    await tester.pumpAndSettle();
    // The spec content section (and this list under it) can sit below
    // the initial viewport/cache extent for this fixture -- scroll it
    // into the built range before asserting.
    await tester.drag(find.byType(ListView), const Offset(0, -1200));
    await tester.pumpAndSettle();

    expect(
      find.byKey(const ValueKey('acceptance-criteria-list')),
      findsOneWidget,
    );
    expect(find.text('Acceptance criteria (2)'), findsOneWidget);
    expect(
      find.text('1. Adding two numbers returns their sum'),
      findsOneWidget,
    );
    expect(find.text('2. Dividing by zero raises ValueError'), findsOneWidget);
  });

  testWidgets(
    'the approve confirm sheet uses the server-supplied approve_next_state '
    'instead of the client-side table',
    (tester) async {
      final client = MockClient((request) async {
        return http.Response(
          requestJson(
            id: 'req-1',
            state: 'spec_review',
            title: 'T',
            spec: '# Spec',
            approveNextState: 'oracle_drafting',
          ),
          200,
        );
      });
      final api = RunApi(
        baseUrl: 'http://factory.test',
        client: client,
        overrideToken: 'test-token',
      );

      await tester.pumpWidget(
        MaterialApp(
          home: RequestDetailScreen(api: api, requestId: 'req-1'),
        ),
      );
      await tester.pumpAndSettle();

      await tester.tap(find.byKey(const ValueKey('approve-request-button')));
      await tester.pumpAndSettle();

      expect(find.text('Spec review → Drafting oracles'), findsOneWidget);
    },
  );

  testWidgets(
    'a 409 conflict on saving a spec edit fetches and diffs the current '
    'server content, and "Discard mine and reload" drops the unsaved edit',
    (tester) async {
      var getCount = 0;
      var putCalled = false;
      final client = MockClient((request) async {
        if (request.method == 'PUT' &&
            request.url.path == '/requests/req-1/spec') {
          putCalled = true;
          return http.Response(
            jsonEncode({
              'error': 'spec.md changed since you started editing',
              'current_sha256': 'deadbeef',
            }),
            409,
          );
        }
        getCount++;
        // The first GET (initial load) sees the original content; the
        // second (triggered by the conflict callout's onFetchCurrent)
        // sees what "someone else" saved in between.
        final spec = getCount == 1
            ? '# Spec\n\nOriginal detail.'
            : '# Spec\n\nServer updated detail.';
        return http.Response(
          requestJson(
            id: 'req-1',
            state: 'spec_review',
            title: 'Add idempotency keys',
            spec: spec,
          ),
          200,
        );
      });
      final api = RunApi(
        baseUrl: 'http://factory.test',
        client: client,
        overrideToken: 'test-token',
      );

      await tester.pumpWidget(
        MaterialApp(
          home: RequestDetailScreen(api: api, requestId: 'req-1'),
        ),
      );
      await tester.pumpAndSettle();

      await tester.tap(
        find.byKey(const ValueKey('edit-content-button-spec.md')),
      );
      await tester.pumpAndSettle();
      await tester.enterText(
        find.byKey(const ValueKey('edit-content-field-spec.md')),
        'My unsaved edit.',
      );
      await tester.ensureVisible(
        find.byKey(const ValueKey('save-content-button-spec.md')),
      );
      await tester.pumpAndSettle();
      await tester.tap(
        find.byKey(const ValueKey('save-content-button-spec.md')),
      );
      await tester.pumpAndSettle();

      expect(putCalled, isTrue);
      expect(
        find.byKey(const ValueKey('edit-content-conflict-spec.md')),
        findsOneWidget,
      );
      expect(find.textContaining('current hash: deadbeef'), findsOneWidget);
      final diffFinder = find.byKey(
        const ValueKey('edit-content-conflict-diff-spec.md'),
      );
      expect(diffFinder, findsOneWidget);
      final diffText = tester
          .widget<SelectableText>(
            find.descendant(
              of: diffFinder,
              matching: find.byType(SelectableText),
            ),
          )
          .textSpan!
          .toPlainText();
      expect(diffText, contains('Server updated detail.'));
      expect(diffText, contains('My unsaved edit.'));
      // The operator's own text is still intact, untouched by the fetch.
      expect(find.text('My unsaved edit.'), findsOneWidget);

      // The conflict callout (with its own 240px diff pane) can push the
      // action buttons below the initial viewport/ensureVisible's own
      // scroll target -- drag the outer list first, same as this file's
      // other below-the-fold assertions.
      await tester.drag(find.byType(ListView).first, const Offset(0, -1200));
      await tester.pumpAndSettle();
      await tester.tap(
        find.byKey(const ValueKey('edit-content-conflict-discard-spec.md')),
      );
      await tester.pumpAndSettle();

      // The editor is gone; the screen shows the server's current content
      // (already fetched during the conflict fetch) rather than the
      // discarded edit.
      expect(
        find.byKey(const ValueKey('edit-content-field-spec.md')),
        findsNothing,
      );
      expect(find.textContaining('Server updated detail.'), findsOneWidget);
    },
  );

  testWidgets('"Keep editing (base = current)" on a 409 conflict re-bases '
      'base_sha256 to the reported current hash and keeps the edit open', (
    tester,
  ) async {
    var putBodies = <Map<String, dynamic>>[];
    final client = MockClient((request) async {
      if (request.method == 'PUT' &&
          request.url.path == '/requests/req-1/spec') {
        final body = jsonDecode(request.body) as Map<String, dynamic>;
        putBodies.add(body);
        if (putBodies.length == 1) {
          return http.Response(
            jsonEncode({
              'error': 'spec.md changed since you started editing',
              'current_sha256': 'deadbeef',
            }),
            409,
          );
        }
        return http.Response(
          requestJson(
            id: 'req-1',
            state: 'planning',
            title: 'Add idempotency keys',
          ),
          200,
        );
      }
      return http.Response(
        requestJson(
          id: 'req-1',
          state: 'spec_review',
          title: 'Add idempotency keys',
          spec: '# Spec\n\nServer updated detail.',
        ),
        200,
      );
    });
    final api = RunApi(
      baseUrl: 'http://factory.test',
      client: client,
      overrideToken: 'test-token',
    );

    await tester.pumpWidget(
      MaterialApp(
        home: RequestDetailScreen(api: api, requestId: 'req-1'),
      ),
    );
    await tester.pumpAndSettle();

    await tester.tap(find.byKey(const ValueKey('edit-content-button-spec.md')));
    await tester.pumpAndSettle();
    await tester.enterText(
      find.byKey(const ValueKey('edit-content-field-spec.md')),
      'My unsaved edit.',
    );
    await tester.ensureVisible(
      find.byKey(const ValueKey('save-content-button-spec.md')),
    );
    await tester.pumpAndSettle();
    await tester.tap(find.byKey(const ValueKey('save-content-button-spec.md')));
    await tester.pumpAndSettle();

    expect(putBodies.single['base_sha256'], isNot('deadbeef'));

    await tester.drag(find.byType(ListView).first, const Offset(0, -1200));
    await tester.pumpAndSettle();
    await tester.tap(
      find.byKey(const ValueKey('edit-content-conflict-keep-spec.md')),
    );
    await tester.pumpAndSettle();

    // The editor is still open, with the operator's text intact.
    expect(
      find.byKey(const ValueKey('edit-content-field-spec.md')),
      findsOneWidget,
    );
    expect(find.text('My unsaved edit.'), findsOneWidget);

    await tester.ensureVisible(
      find.byKey(const ValueKey('save-content-button-spec.md')),
    );
    await tester.tap(find.byKey(const ValueKey('save-content-button-spec.md')));
    await tester.pumpAndSettle();

    expect(putBodies, hasLength(2));
    // The second Save re-sent the operator's own text, now explicitly
    // based on the hash the 409 reported.
    expect(putBodies[1]['content'], 'My unsaved edit.');
    expect(putBodies[1]['base_sha256'], 'deadbeef');
  });

  group('resume_review', () {
    String lostBuild({List<String> refused = const []}) => requestJson(
      id: 'req-1',
      state: 'resume_review',
      title: 'T',
      error: 'the factoryd worker stopped while the build ran',
      resume: {
        'from_state': 'building',
        'lost_run_id': 'run-1',
        'generation': 1,
        'at': '2026-09-10T09:05:00Z',
        if (refused.isNotEmpty) 'refused': refused,
      },
    );

    // Serves the request in resume_review and answers POST /resume with
    // [resumeResponse]; each POST body is appended to [bodies].
    RunApi apiFor(
      List<Map<String, dynamic>> bodies, {
      String? detail,
      http.Response? resumeResponse,
    }) {
      final client = MockClient((request) async {
        if (request.method == 'POST' &&
            request.url.path == '/requests/req-1/resume') {
          bodies.add(jsonDecode(request.body) as Map<String, dynamic>);
          return resumeResponse ??
              http.Response(
                requestJson(id: 'req-1', state: 'building', title: 'T'),
                200,
              );
        }
        return http.Response(detail ?? lostBuild(), 200);
      });
      return RunApi(
        baseUrl: 'http://factory.test',
        client: client,
        overrideToken: 'test-token',
      );
    }

    Future<void> open(WidgetTester tester, RunApi api) async {
      await tester.pumpWidget(
        MaterialApp(
          home: RequestDetailScreen(api: api, requestId: 'req-1'),
        ),
      );
      await tester.pumpAndSettle();
    }

    testWidgets('callout shows the lost step, the prompt and three actions', (
      tester,
    ) async {
      await open(tester, apiFor([]));

      expect(find.byKey(const ValueKey('resume-callout')), findsOneWidget);
      expect(find.textContaining('Building step was lost'), findsOneWidget);
      expect(
        find.textContaining('the factoryd worker stopped while the build ran'),
        findsWidgets,
      );
      expect(
        find.byKey(const ValueKey('resume-request-button')),
        findsOneWidget,
      );
      expect(
        find.byKey(const ValueKey('resume-scratch-button')),
        findsOneWidget,
      );
      expect(
        find.byKey(const ValueKey('resume-cancel-button')),
        findsOneWidget,
      );
      expect(
        find.byKey(const ValueKey('resume-refusal-reason-0')),
        findsNothing,
      );
    });

    testWidgets('refusal reasons are shown and Resume is withheld', (
      tester,
    ) async {
      await open(
        tester,
        apiFor(
          [],
          detail: lostBuild(
            refused: ['a sandbox container is still running', 'history moved'],
          ),
        ),
      );

      expect(
        find.text('- a sandbox container is still running'),
        findsOneWidget,
      );
      expect(find.text('- history moved'), findsOneWidget);
      expect(find.byKey(const ValueKey('resume-request-button')), findsNothing);
      expect(
        find.byKey(const ValueKey('resume-scratch-button')),
        findsOneWidget,
      );
    });

    for (final (key, from) in [
      ('resume-request-button', 'round'),
      ('resume-scratch-button', 'scratch'),
    ]) {
      testWidgets('$key posts from=$from after confirmation', (tester) async {
        final bodies = <Map<String, dynamic>>[];
        await open(tester, apiFor(bodies));

        await tester.tap(find.byKey(ValueKey(key)));
        await tester.pumpAndSettle();
        expect(bodies, isEmpty, reason: 'nothing is sent before confirming');
        await tester.tap(find.byKey(const ValueKey('resume-confirm-button')));
        await tester.pumpAndSettle();

        expect(bodies, hasLength(1));
        expect(bodies.single['from'], from);
        expect(bodies.single['by'], 'operator');
        expect(find.byKey(const ValueKey('resume-callout')), findsNothing);
      });
    }

    testWidgets('backing out of the confirmation sends nothing', (
      tester,
    ) async {
      final bodies = <Map<String, dynamic>>[];
      await open(tester, apiFor(bodies));

      await tester.tap(find.byKey(const ValueKey('resume-request-button')));
      await tester.pumpAndSettle();
      await tester.tap(find.byKey(const ValueKey('resume-dismiss-button')));
      await tester.pumpAndSettle();

      expect(bodies, isEmpty);
      expect(find.byKey(const ValueKey('resume-callout')), findsOneWidget);
    });

    testWidgets('a 409 refusal shows the server reasons', (tester) async {
      final bodies = <Map<String, dynamic>>[];
      await open(
        tester,
        apiFor(
          bodies,
          resumeResponse: http.Response(
            jsonEncode({
              'error':
                  'request req-1: cannot resume the lost build: a sandbox '
                  'container is still running',
            }),
            409,
          ),
        ),
      );

      await tester.tap(find.byKey(const ValueKey('resume-request-button')));
      await tester.pumpAndSettle();
      await tester.tap(find.byKey(const ValueKey('resume-confirm-button')));
      await tester.pumpAndSettle();

      expect(
        find.descendant(
          of: find.byKey(const ValueKey('resume-callout')),
          matching: find.textContaining('a sandbox container is still running'),
        ),
        findsOneWidget,
      );
      expect(find.byKey(const ValueKey('resume-action-error')), findsOneWidget);
    });

    testWidgets('a 503 is shown as retryable', (tester) async {
      await open(
        tester,
        apiFor(
          [],
          resumeResponse: http.Response(
            jsonEncode({'error': 'could not check the lost build'}),
            503,
          ),
        ),
      );

      await tester.tap(find.byKey(const ValueKey('resume-request-button')));
      await tester.pumpAndSettle();
      await tester.tap(find.byKey(const ValueKey('resume-confirm-button')));
      await tester.pumpAndSettle();

      expect(find.textContaining('temporary: try again'), findsOneWidget);
    });

    testWidgets('Cancel runs the existing cancel flow', (tester) async {
      String? cancelBody;
      final client = MockClient((request) async {
        if (request.method == 'POST' &&
            request.url.path == '/requests/req-1/cancel') {
          cancelBody = request.body;
          return http.Response(
            requestJson(id: 'req-1', state: 'cancelled', title: 'T'),
            200,
          );
        }
        return http.Response(lostBuild(), 200);
      });
      await open(
        tester,
        RunApi(
          baseUrl: 'http://factory.test',
          client: client,
          overrideToken: 'test-token',
        ),
      );

      await tester.tap(find.byKey(const ValueKey('resume-cancel-button')));
      await tester.pumpAndSettle();
      await tester.enterText(
        find.byKey(const ValueKey('cancel-reason')),
        'not needed',
      );
      await tester.tap(find.byKey(const ValueKey('cancel-confirm-button')));
      await tester.pumpAndSettle();

      expect((jsonDecode(cancelBody!) as Map)['reason'], 'not needed');
    });

    String lostStep(String step) => requestJson(
      id: 'req-1',
      state: 'resume_review',
      title: 'T',
      resume: {
        'from_state': step,
        'generation': 1,
        'at': '2026-09-10T09:05:00Z',
      },
    );

    testWidgets('a lost drafting step offers one Rerun step button', (
      tester,
    ) async {
      final bodies = <Map<String, dynamic>>[];
      await open(tester, apiFor(bodies, detail: lostStep('spec_drafting')));

      expect(find.byKey(const ValueKey('resume-scratch-button')), findsNothing);
      expect(find.text('Rerun step'), findsOneWidget);
      expect(find.text('Resume'), findsNothing);
      expect(find.text('Rebuild from scratch'), findsNothing);

      await tester.tap(find.byKey(const ValueKey('resume-request-button')));
      await tester.pumpAndSettle();
      expect(
        find.textContaining('Run the lost Drafting spec step again'),
        findsOneWidget,
      );
      expect(find.textContaining('paid run'), findsNothing);
      await tester.tap(find.byKey(const ValueKey('resume-confirm-button')));
      await tester.pumpAndSettle();

      expect(bodies.single['from'], 'round');
    });

    testWidgets('a lost build still offers Resume and Rebuild', (tester) async {
      await open(tester, apiFor([], detail: lostStep('building')));

      expect(find.text('Resume'), findsOneWidget);
      expect(find.text('Rebuild from scratch'), findsOneWidget);
      expect(find.text('Rerun step'), findsNothing);
    });

    testWidgets('a failed cancel is labelled as a cancel failure', (
      tester,
    ) async {
      final client = MockClient((request) async {
        if (request.method == 'POST') {
          return http.Response(jsonEncode({'error': 'cannot cancel now'}), 409);
        }
        return http.Response(lostBuild(), 200);
      });
      await open(
        tester,
        RunApi(
          baseUrl: 'http://factory.test',
          client: client,
          overrideToken: 'test-token',
        ),
      );

      await tester.tap(find.byKey(const ValueKey('resume-cancel-button')));
      await tester.pumpAndSettle();
      await tester.enterText(
        find.byKey(const ValueKey('cancel-reason')),
        'no longer needed',
      );
      await tester.tap(find.byKey(const ValueKey('cancel-confirm-button')));
      await tester.pumpAndSettle();

      expect(
        find.textContaining('Could not cancel: cannot cancel now'),
        findsOneWidget,
      );
      expect(find.byKey(const ValueKey('resume-action-error')), findsNothing);
      expect(find.textContaining('Could not resume'), findsNothing);
    });

    testWidgets('a resume error clears once the refusal reasons change', (
      tester,
    ) async {
      var detail = lostBuild();
      final client = MockClient((request) async {
        if (request.method == 'POST') {
          return http.Response(jsonEncode({'error': 'container alive'}), 409);
        }
        return http.Response(detail, 200);
      });
      await open(
        tester,
        RunApi(
          baseUrl: 'http://factory.test',
          client: client,
          overrideToken: 'test-token',
        ),
      );

      await tester.tap(find.byKey(const ValueKey('resume-request-button')));
      await tester.pumpAndSettle();
      await tester.tap(find.byKey(const ValueKey('resume-confirm-button')));
      await tester.pumpAndSettle();
      expect(find.byKey(const ValueKey('resume-action-error')), findsOneWidget);

      // A poll with the same state keeps it.
      await tester.tap(
        find.byKey(const ValueKey('request-detail-refresh-button')),
      );
      await tester.pumpAndSettle();
      expect(find.byKey(const ValueKey('resume-action-error')), findsOneWidget);

      detail = lostBuild(refused: ['container alive']);
      await tester.tap(
        find.byKey(const ValueKey('request-detail-refresh-button')),
      );
      await tester.pumpAndSettle();
      expect(find.byKey(const ValueKey('resume-action-error')), findsNothing);
      expect(find.text('- container alive'), findsOneWidget);
    });
  });
}
