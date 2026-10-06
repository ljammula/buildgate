import 'dart:convert';

import 'package:console/api_client.dart';
import 'package:console/elapsed.dart';
import 'package:console/models.dart';
import 'package:console/request_detail_screen.dart';
import 'package:console/run_detail_screen.dart';
import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:http/http.dart' as http;
import 'package:http/testing.dart';

import 'fixtures.dart';

void main() {
  // C4 (operator demo, 2026-09-26): the run detail screen's own Created/
  // Updated fields showed raw UTC verbatim -- both now route through the
  // shared formatLocalTimestamp.
  testWidgets('run detail shows local time, not raw UTC, for created_at', (
    tester,
  ) async {
    final client = MockClient((request) async {
      if (request.url.path.endsWith('/progress')) {
        return http.Response('', 200);
      }
      return http.Response(acceptedRunJson, 200);
    });
    final api = RunApi(baseUrl: 'http://factory.test', client: client);

    await tester.pumpWidget(
      MaterialApp(
        home: RunDetailScreen(api: api, runId: 'run-accepted'),
      ),
    );
    await tester.pumpAndSettle();

    expect(
      find.text(formatLocalTimestamp('2026-08-26T11:00:00Z')),
      findsOneWidget,
    );
    expect(find.text('2026-08-26T11:00:00Z'), findsNothing);
  });

  // Found in the 2026-09-29 todo-kafka-service demo: the run page said
  // nothing about the compose sidecars each phase launched.
  testWidgets('run detail lists the compose services each phase launched', (
    tester,
  ) async {
    final withCompose = jsonDecode(acceptedRunJson) as Map<String, dynamic>
      ..['compose_phases'] = [
        {
          'phase': 'build',
          'enabled': true,
          'services': [
            {
              'name': 'kafka',
              'alias': 'kafka',
              'port': 19092,
              'image': 'apache/kafka:3.8.0',
              'digest': 'apache/kafka@sha256:c89f',
            },
          ],
        },
        {
          'phase': 'lint',
          'enabled': false,
          'disabled_reason': 'no compose file found in the target repository',
        },
      ];
    final client = MockClient((request) async {
      if (request.url.path.endsWith('/progress')) {
        return http.Response('', 200);
      }
      return http.Response(jsonEncode(withCompose), 200);
    });
    final api = RunApi(baseUrl: 'http://factory.test', client: client);

    await tester.pumpWidget(
      MaterialApp(
        home: RunDetailScreen(api: api, runId: 'run-accepted'),
      ),
    );
    await tester.pumpAndSettle();

    final line = find.text(
      'kafka: apache/kafka:3.8.0 at kafka:19092 (apache/kafka@sha256:c89f)',
    );
    await tester.scrollUntilVisible(
      line,
      300,
      scrollable: find.byType(Scrollable).first,
    );
    expect(find.text('Compose services'), findsOneWidget);
    expect(line, findsOneWidget);
    expect(
      find.text('Not launched: no compose file found in the target repository'),
      findsOneWidget,
    );
  });

  testWidgets('run detail omits the compose section without compose data', (
    tester,
  ) async {
    final client = MockClient((request) async {
      if (request.url.path.endsWith('/progress')) {
        return http.Response('', 200);
      }
      return http.Response(acceptedRunJson, 200);
    });
    final api = RunApi(baseUrl: 'http://factory.test', client: client);

    await tester.pumpWidget(
      MaterialApp(
        home: RunDetailScreen(api: api, runId: 'run-accepted'),
      ),
    );
    await tester.pumpAndSettle();
    expect(find.text('Compose services'), findsNothing);
  });

  // P0e (role evidence): an attempt's session-config role, worker model,
  // and requested-vs-sent thinking level render as one line on its
  // Attempt card. acceptedRunJson's fixture attempt is role "execution",
  // thinking "max", but relay_reasoning_effort "high" -- a deliberate
  // mismatch, so this also covers the "(sent: X)" clamp hint.
  testWidgets('attempt card shows role, model, and a genuine '
      'requested/sent clamp', (tester) async {
    // acceptedRunJson's fixture attempt sets expected_effort "max" (no
    // thinkingLevelMap translation declared) alongside relay_reasoning_
    // effort "high" -- a real silent clamp, which must still show.
    final client = MockClient((request) async {
      if (request.url.path.endsWith('/progress')) {
        return http.Response('', 200);
      }
      return http.Response(acceptedRunJson, 200);
    });
    final api = RunApi(baseUrl: 'http://factory.test', client: client);

    await tester.pumpWidget(
      MaterialApp(
        home: RunDetailScreen(api: api, runId: 'run-accepted'),
      ),
    );
    await tester.pumpAndSettle();

    final line = find.text(
      'Role: execution · Harness: pifork · Model: gpt-5.6-luna · '
      'Thinking: max (sent: high)',
    );
    await tester.scrollUntilVisible(
      line,
      300,
      scrollable: find.byType(Scrollable).first,
    );
    expect(line, findsOneWidget);
  });

  // Found via review: comparing `thinking` directly against
  // `relay_reasoning_effort` false-positives whenever a model's own
  // thinkingLevelMap legitimately renames a level (e.g. "max" ->
  // "xhigh"). expected_effort carries that translated value, and the
  // hint must be suppressed once it agrees with what the relay observed
  // sending.
  testWidgets(
    'attempt card shows no clamp hint when a declared thinkingLevelMap '
    'translation matches what was sent',
    (tester) async {
      final runJson = jsonDecode(acceptedRunJson) as Map<String, dynamic>;
      final attempt = (runJson['attempts'] as List)[0] as Map<String, dynamic>;
      attempt['expected_effort'] = 'xhigh';
      attempt['relay_reasoning_effort'] = 'xhigh';
      final translatedRunJson = jsonEncode(runJson);

      final client = MockClient((request) async {
        if (request.url.path.endsWith('/progress')) {
          return http.Response('', 200);
        }
        return http.Response(translatedRunJson, 200);
      });
      final api = RunApi(baseUrl: 'http://factory.test', client: client);

      await tester.pumpWidget(
        MaterialApp(
          home: RunDetailScreen(api: api, runId: 'run-accepted'),
        ),
      );
      await tester.pumpAndSettle();

      final line = find.text(
        'Role: execution · Harness: pifork · Model: gpt-5.6-luna · Thinking: max',
      );
      await tester.scrollUntilVisible(
        line,
        300,
        scrollable: find.byType(Scrollable).first,
      );
      expect(line, findsOneWidget);
      expect(find.textContaining('(sent:'), findsNothing);
    },
  );

  // C6 (operator demo, 2026-09-26): a run with no reference_oracle_dir
  // (no -draft-oracles) never visits commit_oracles/
  // post_oracle_commit_verify, so the timeline must not list them.
  testWidgets(
    'timeline omits commit_oracles/post_oracle_commit_verify for a run '
    'with no oracle stage',
    (tester) async {
      final client = MockClient((request) async {
        if (request.url.path.endsWith('/progress')) {
          return http.Response('', 200);
        }
        return http.Response(acceptedRunJson, 200);
      });
      final api = RunApi(baseUrl: 'http://factory.test', client: client);

      await tester.pumpWidget(
        MaterialApp(
          home: RunDetailScreen(api: api, runId: 'run-accepted'),
        ),
      );
      await tester.pumpAndSettle();

      expect(
        find.byKey(const ValueKey('timeline-row-commit_oracles')),
        findsNothing,
      );
      expect(
        find.byKey(const ValueKey('timeline-row-post_oracle_commit_verify')),
        findsNothing,
      );
    },
  );

  testWidgets(
    'timeline shows commit_oracles/post_oracle_commit_verify for a run '
    'with an oracle stage',
    (tester) async {
      final runJson = jsonDecode(acceptedRunJson) as Map<String, dynamic>;
      runJson['reference_oracle_dir'] = '/data/requests/req-1/oracle';
      final withOracleDir = jsonEncode(runJson);

      final client = MockClient((request) async {
        if (request.url.path.endsWith('/progress')) {
          return http.Response('', 200);
        }
        return http.Response(withOracleDir, 200);
      });
      final api = RunApi(baseUrl: 'http://factory.test', client: client);

      await tester.pumpWidget(
        MaterialApp(
          home: RunDetailScreen(api: api, runId: 'run-accepted'),
        ),
      );
      await tester.pumpAndSettle();

      expect(
        find.byKey(const ValueKey('timeline-row-commit_oracles')),
        findsOneWidget,
      );
      expect(
        find.byKey(const ValueKey('timeline-row-post_oracle_commit_verify')),
        findsOneWidget,
      );
    },
  );

  // A quarantined run is not terminal (see Run.isTerminal's own doc
  // comment): an operator override can still move it to accepted/halted
  // later, so RunDetailScreen must keep watching it, not treat quarantine
  // as final. watchRun is a streamed HTTP request (not a native browser
  // EventSource — see its own doc comment), so a mocked server-sent event
  // genuinely reaches the widget through the exact same parsing path a
  // real server's writeStateEvent output would: this proves the
  // subscription was both attempted and actually wired to live updates,
  // not merely that the initial connection didn't error.
  testWidgets(
    'quarantined run detail screen watches for and applies later state changes',
    (tester) async {
      final client = MockClient((request) async {
        if (request.url.path.endsWith('/events')) {
          final compactJson = jsonEncode(jsonDecode(acceptedRunJson));
          return http.Response(
            'event: state\ndata: $compactJson\n\n',
            200,
            headers: {'content-type': 'text/event-stream'},
          );
        }
        return http.Response(quarantinedRunJson, 200);
      });
      final api = RunApi(baseUrl: 'http://factory.test', client: client);

      await tester.pumpWidget(
        MaterialApp(
          home: RunDetailScreen(api: api, runId: 'run-quarantined'),
        ),
      );
      await tester.pumpAndSettle();

      // The stream's single event updates the run in place to accepted —
      // proof the subscription was attempted and its data applied, not
      // just that the initial connection succeeded.
      expect(find.text('ticket-accepted'), findsOneWidget);
      // findsWidgets, not findsOneWidget: the Timeline section's own status
      // strip (Phase 4) now also shows a StateBadge with the same label,
      // alongside the "Run" section's own.
      expect(find.text('Accepted'), findsWidgets);
    },
  );

  // Contrast case: a failing connection must retry quietly in the
  // background (see RunApi.watchRun's own doc comment), the same way a
  // native browser EventSource would, rather than surface as a visible,
  // permanent "Live updates: Disconnected" state -- an earlier version of
  // this test asserted the opposite (from before watchRun retried at
  // all), which would otherwise leave the console showing a stale
  // "Disconnected" banner through what is really just an ordinary,
  // recovering reconnect.
  testWidgets(
    'quarantined run detail screen keeps retrying rather than showing disconnected',
    (tester) async {
      final client = MockClient((request) async {
        if (request.url.path.endsWith('/events')) {
          return http.Response('', 500);
        }
        return http.Response(quarantinedRunJson, 200);
      });
      final api = RunApi(
        baseUrl: 'http://factory.test',
        client: client,
        watchInitialBackoff: const Duration(milliseconds: 1),
        watchMaxBackoff: const Duration(milliseconds: 5),
      );

      await tester.pumpWidget(
        MaterialApp(
          home: RunDetailScreen(api: api, runId: 'run-quarantined'),
        ),
      );
      await tester.pump();
      await tester.pump(const Duration(milliseconds: 50));

      expect(find.text('ticket-quarantined'), findsOneWidget);
      expect(find.textContaining('Disconnected'), findsNothing);
    },
  );

  // Contrast case: an accepted run genuinely is terminal, so no
  // subscription is attempted at all and no "Live updates" field appears.
  testWidgets(
    'accepted run detail screen does not attempt to watch further updates',
    (tester) async {
      final client = MockClient(
        (_) async => http.Response(acceptedRunJson, 200),
      );
      final api = RunApi(baseUrl: 'http://factory.test', client: client);

      await tester.pumpWidget(
        MaterialApp(
          home: RunDetailScreen(api: api, runId: 'run-accepted'),
        ),
      );
      await tester.pumpAndSettle();

      expect(find.text('ticket-accepted'), findsOneWidget);
      expect(find.textContaining('Live updates'), findsNothing);
    },
  );

  testWidgets('quarantined run override posts attribution and updates state', (
    tester,
  ) async {
    http.Request? submitted;
    final client = MockClient((request) async {
      if (request.url.path.endsWith('/override')) {
        submitted = request;
        return http.Response(acceptedRunJson, 200);
      }
      return http.Response('', 200);
    });
    final api = RunApi(
      baseUrl: 'http://factory.test',
      client: client,
      authToken: 'override-token',
    );

    await tester.pumpWidget(
      MaterialApp(
        home: RunDetailScreen(
          api: api,
          runId: 'run-quarantined',
          initialRun: Run.fromJson(
            jsonDecode(quarantinedRunJson) as Map<String, dynamic>,
          ),
        ),
      ),
    );
    await tester.pumpAndSettle();
    // Below the fold now that the Timeline section sits above "Run" --
    // same reasoning as the diff/release button scrolls elsewhere in this
    // file.
    await tester.scrollUntilVisible(
      find.byKey(const ValueKey('override-run-button')),
      300,
      scrollable: find.byType(Scrollable).first,
    );
    await tester.pumpAndSettle();
    await tester.tap(find.byKey(const ValueKey('override-run-button')));
    await tester.pumpAndSettle();

    await tester.enterText(
      find.byKey(const ValueKey('override-by')),
      'operator@example.com',
    );
    await tester.enterText(
      find.byKey(const ValueKey('override-reason')),
      'Reviewed the verification evidence',
    );
    await tester.tap(find.text('Apply'));
    await tester.pump();
    await tester.pump(const Duration(milliseconds: 100));

    expect(submitted, isNotNull);
    expect(submitted!.method, 'POST');
    expect(
      submitted!.url.toString(),
      'http://factory.test/runs/run-quarantined/override',
    );
    expect(submitted!.headers['authorization'], 'Bearer override-token');
    expect(jsonDecode(submitted!.body), {
      'by': 'operator@example.com',
      'reason': 'Reviewed the verification evidence',
      'state': 'accepted',
    });
    expect(find.text('Accepted'), findsOneWidget);
  });

  // An adversarial review (2026-09-24) found: the override button must
  // be disabled when writesEnabled is true (the loopback no-token
  // relaxation applies to this console) but no build-time override token
  // exists -- unlike every other write button, gated on api.canWrite
  // (writesEnabled || hasOverrideToken), the server's own
  // authorizeOverride never grants that relaxation to POST
  // /runs/{id}/override, so a button enabled on canWrite alone would let
  // an operator click it and get a 403 back with no token this console
  // has any way to supply.
  testWidgets(
    'quarantined run override button is disabled without an override token even when writesEnabled',
    (tester) async {
      final client = MockClient(
        (_) async => http.Response(quarantinedRunJson, 200),
      );
      final api = RunApi(
        baseUrl: 'http://factory.test',
        client: client,
        writesEnabled: true,
      );
      expect(api.canWrite, isTrue);
      expect(api.hasOverrideToken, isFalse);

      await tester.pumpWidget(
        MaterialApp(
          home: RunDetailScreen(
            api: api,
            runId: 'run-quarantined',
            initialRun: Run.fromJson(
              jsonDecode(quarantinedRunJson) as Map<String, dynamic>,
            ),
          ),
        ),
      );
      await tester.pumpAndSettle();
      await tester.scrollUntilVisible(
        find.byKey(const ValueKey('override-run-button')),
        300,
        scrollable: find.byType(Scrollable).first,
      );
      await tester.pumpAndSettle();

      final button = tester.widget<FilledButton>(
        find.byKey(const ValueKey('override-run-button')),
      );
      expect(button.onPressed, isNull);
    },
  );

  // Proves the "View diff" button both appears (the run has a ResultSHA)
  // and actually navigates to a screen that fetches and displays the real
  // diff content from the backing endpoint, not just diff_stat's own
  // file-count/insertion/deletion summary already shown on this screen.
  testWidgets('accepted run detail screen navigates to the diff screen', (
    tester,
  ) async {
    final client = MockClient((request) async {
      if (request.url.path.endsWith('/diff')) {
        return http.Response(
          jsonEncode({
            'diff': '+added line\n-removed line\n',
            'truncated': false,
          }),
          200,
        );
      }
      return http.Response(acceptedRunJson, 200);
    });
    final api = RunApi(baseUrl: 'http://factory.test', client: client);

    await tester.pumpWidget(
      MaterialApp(
        home: RunDetailScreen(api: api, runId: 'run-accepted'),
      ),
    );
    await tester.pumpAndSettle();

    // The button lives in the "Changed files" section, below the fold in
    // this screen's ListView — the Sliver machinery backing it only
    // mounts children near the current viewport, so it isn't in the
    // widget tree at all until scrolled into view.
    await tester.scrollUntilVisible(
      find.byKey(const ValueKey('view-diff-button')),
      300,
      scrollable: find.byType(Scrollable).first,
    );
    await tester.pumpAndSettle();

    expect(find.byKey(const ValueKey('view-diff-button')), findsOneWidget);
    await tester.tap(find.byKey(const ValueKey('view-diff-button')));
    await tester.pumpAndSettle();

    expect(find.textContaining('+added line'), findsOneWidget);
  });

  // Proves the run detail screen actually reaches the release screen, and
  // that the release screen there renders the run's real recorded decision
  // from the backing endpoint — not a locally inferred one.
  testWidgets('run detail screen navigates to the release screen', (
    tester,
  ) async {
    final client = MockClient((request) async {
      if (request.url.path.endsWith('/release')) {
        return http.Response(deniedReleaseJson, 200);
      }
      return http.Response(acceptedRunJson, 200);
    });
    final api = RunApi(baseUrl: 'http://factory.test', client: client);

    await tester.pumpWidget(
      MaterialApp(
        home: RunDetailScreen(api: api, runId: 'run-accepted'),
      ),
    );
    await tester.pumpAndSettle();

    // Below the fold in this screen's ListView, same as the diff button.
    await tester.scrollUntilVisible(
      find.byKey(const ValueKey('view-release-button')),
      300,
      scrollable: find.byType(Scrollable).first,
    );
    await tester.pumpAndSettle();
    await tester.tap(find.byKey(const ValueKey('view-release-button')));
    await tester.pumpAndSettle();

    expect(
      find.byKey(const ValueKey('release-decision-denied')),
      findsOneWidget,
    );
    expect(find.byKey(const ValueKey('kill-switch-engaged')), findsOneWidget);
  });

  // Proves a truncated diff is visibly flagged, not silently presented as
  // the complete change — the regression test for a real P2 finding from
  // codex review: the client used to discard the server's "truncated" flag
  // entirely.
  testWidgets('truncated diff shows a warning banner', (tester) async {
    final client = MockClient((request) async {
      if (request.url.path.endsWith('/diff')) {
        return http.Response(
          jsonEncode({'diff': '+added line\n', 'truncated': true}),
          200,
        );
      }
      return http.Response(acceptedRunJson, 200);
    });
    final api = RunApi(baseUrl: 'http://factory.test', client: client);

    await tester.pumpWidget(
      MaterialApp(
        home: RunDetailScreen(api: api, runId: 'run-accepted'),
      ),
    );
    await tester.pumpAndSettle();

    await tester.scrollUntilVisible(
      find.byKey(const ValueKey('view-diff-button')),
      300,
      scrollable: find.byType(Scrollable).first,
    );
    await tester.pumpAndSettle();
    await tester.tap(find.byKey(const ValueKey('view-diff-button')));
    await tester.pumpAndSettle();

    expect(find.textContaining('truncated'), findsOneWidget);
    expect(find.textContaining('+added line'), findsOneWidget);
  });

  // The log pane is off by default (no
  // `GET .../log` request until the operator opts in) and, once toggled
  // on, renders the streamed text as plain SelectableText.
  group('live build log pane', () {
    testWidgets('is off by default -- no log request until toggled on', (
      tester,
    ) async {
      var logRequested = false;
      final client = MockClient((request) async {
        if (request.url.path.endsWith('/log')) {
          logRequested = true;
          return http.Response('some log text', 200);
        }
        if (request.url.path.endsWith('/events')) {
          return http.Response('', 200);
        }
        return http.Response(acceptedRunJson, 200);
      });
      final api = RunApi(baseUrl: 'http://factory.test', client: client);

      await tester.pumpWidget(
        MaterialApp(
          home: RunDetailScreen(api: api, runId: 'run-accepted'),
        ),
      );
      await tester.pumpAndSettle();
      // Below the fold now that the Timeline section sits above "Run".
      await tester.scrollUntilVisible(
        find.byKey(const ValueKey('log-pane-toggle')),
        300,
        scrollable: find.byType(Scrollable).first,
      );

      expect(logRequested, isFalse);
      expect(find.byKey(const ValueKey('log-pane-content')), findsNothing);
      expect(find.byKey(const ValueKey('log-pane-toggle')), findsOneWidget);
    });

    testWidgets('toggling it on streams and renders the log as plain text', (
      tester,
    ) async {
      final client = MockClient.streaming((request, bodyStream) async {
        if (request.url.path.endsWith('/log')) {
          expect(request.url.queryParameters['follow'], '1');
          return http.StreamedResponse(
            Stream.value(
              utf8.encode(
                'building ticket-1\n'
                'FACTORY_PROGRESS {"stage": "agent", "event": "note", "round": 1, "detail": "read: main.go"}\n'
                'done\n',
              ),
            ),
            200,
          );
        }
        if (request.url.path.endsWith('/events')) {
          return http.StreamedResponse(const Stream.empty(), 200);
        }
        return http.StreamedResponse(
          Stream.value(utf8.encode(acceptedRunJson)),
          200,
        );
      });
      final api = RunApi(baseUrl: 'http://factory.test', client: client);

      await tester.pumpWidget(
        MaterialApp(
          home: RunDetailScreen(api: api, runId: 'run-accepted'),
        ),
      );
      await tester.pumpAndSettle();

      await tester.scrollUntilVisible(
        find.byKey(const ValueKey('log-pane-toggle')),
        300,
        scrollable: find.byType(Scrollable).first,
      );
      await tester.tap(find.byKey(const ValueKey('log-pane-toggle')));
      await tester.pumpAndSettle();

      expect(find.byKey(const ValueKey('log-pane-content')), findsOneWidget);
      expect(find.textContaining('building ticket-1'), findsOneWidget);
      // A worker's protocol line reads as a step, never as raw JSON.
      expect(find.textContaining('agent        read: main.go'), findsOneWidget);
      expect(find.textContaining('FACTORY_PROGRESS'), findsNothing);
    });
  });

  // Phase 4 (follow-along console): the Timeline section, built from
  // `GET /runs/{id}/progress`'s SSE feed (progress-contract.md). Each
  // fixture's mocked stream ends with the contract's own terminal marker
  // (`stage: "finished", event: "end"`) so watchRunProgress's reconnect
  // loop closes for good instead of scheduling a background retry.
  group('Timeline (Phase 4 follow-along console)', () {
    String progressFrame(Map<String, dynamic> line) {
      return 'event: progress\ndata: ${jsonEncode(line)}\n\n';
    }

    testWidgets('stages advance from pending to running to passed', (
      tester,
    ) async {
      final client = MockClient((request) async {
        if (request.url.path.endsWith('/progress')) {
          final body =
              progressFrame({
                'ts': '2026-09-17T10:00:00.000Z',
                'source': 'factory',
                'stage': 'prepare_workspace',
                'event': 'start',
              }) +
              progressFrame({
                'ts': '2026-09-17T10:00:05.000Z',
                'source': 'factory',
                'stage': 'prepare_workspace',
                'event': 'end',
                'outcome': 'pass',
              }) +
              progressFrame({
                'ts': '2026-09-17T10:00:05.000Z',
                'source': 'factory',
                'stage': 'preflight',
                'event': 'start',
              });
          return http.Response(
            body,
            200,
            headers: {'content-type': 'text/event-stream'},
          );
        }
        if (request.url.path.endsWith('/events')) {
          return http.Response('', 200);
        }
        return http.Response(inProgressRunJson, 200);
      });
      // A non-terminal run's progress feed (like its /events state feed)
      // never reaches a "reachedTerminal"/permanent-failure condition here
      // -- it just reconnects forever, the same as a real EventSource
      // would -- so this uses the same tiny-backoff + manual-pump pattern
      // as "quarantined run detail screen keeps retrying..." above, rather
      // than pumpAndSettle (which never settles while a reconnect loop
      // keeps scheduling frames).
      final api = RunApi(
        baseUrl: 'http://factory.test',
        client: client,
        watchInitialBackoff: const Duration(milliseconds: 1),
        watchMaxBackoff: const Duration(milliseconds: 5),
      );

      await tester.pumpWidget(
        MaterialApp(
          home: RunDetailScreen(api: api, runId: 'run-progress'),
        ),
      );
      await tester.pump();
      await tester.pump(const Duration(milliseconds: 50));

      expect(
        find.byKey(const ValueKey('timeline-row-prepare_workspace')),
        findsOneWidget,
      );
      expect(
        find.byKey(const ValueKey('timeline-row-preflight')),
        findsOneWidget,
      );
      expect(find.byIcon(Icons.check_circle), findsOneWidget);
      expect(find.byType(CircularProgressIndicator), findsWidgets);
    });

    testWidgets(
      'each stage row is its own semantics node that names its status '
      '(regression: the whole Timeline merged into one node and the '
      'pass/running glyphs had no label, so a screen reader or Playwright '
      'could not tell which stage passed)',
      (tester) async {
        final semantics = tester.ensureSemantics();
        final client = MockClient((request) async {
          if (request.url.path.endsWith('/progress')) {
            final body =
                progressFrame({
                  'ts': '2026-09-17T10:00:00.000Z',
                  'source': 'factory',
                  'stage': 'prepare_workspace',
                  'event': 'start',
                }) +
                progressFrame({
                  'ts': '2026-09-17T10:00:05.000Z',
                  'source': 'factory',
                  'stage': 'prepare_workspace',
                  'event': 'end',
                  'outcome': 'pass',
                }) +
                progressFrame({
                  'ts': '2026-09-17T10:00:05.000Z',
                  'source': 'factory',
                  'stage': 'preflight',
                  'event': 'start',
                });
            return http.Response(
              body,
              200,
              headers: {'content-type': 'text/event-stream'},
            );
          }
          if (request.url.path.endsWith('/events')) {
            return http.Response('', 200);
          }
          return http.Response(inProgressRunJson, 200);
        });
        final api = RunApi(
          baseUrl: 'http://factory.test',
          client: client,
          watchInitialBackoff: const Duration(milliseconds: 1),
          watchMaxBackoff: const Duration(milliseconds: 5),
        );

        await tester.pumpWidget(
          MaterialApp(
            home: RunDetailScreen(api: api, runId: 'run-progress'),
          ),
        );
        await tester.pump();
        await tester.pump(const Duration(milliseconds: 50));

        expect(
          tester.getSemantics(
            find.byKey(const ValueKey('timeline-row-prepare_workspace')),
          ),
          matchesSemantics(label: 'Passed\nPrepare workspace\n00:05'),
        );
        // A running stage also carries its live elapsed time.
        expect(
          tester
              .getSemantics(
                find.byKey(const ValueKey('timeline-row-preflight')),
              )
              .label,
          startsWith('Running\nPreflight\n'),
        );
        expect(
          tester.getSemantics(
            find.byKey(const ValueKey('timeline-row-verify')),
          ),
          matchesSemantics(label: 'Pending\nVerify'),
        );
        semantics.dispose();
      },
    );

    testWidgets('a worker round line renders under Build', (tester) async {
      final client = MockClient((request) async {
        if (request.url.path.endsWith('/progress')) {
          final body =
              progressFrame({
                'ts': '2026-09-17T10:00:00.000Z',
                'source': 'factory',
                'stage': 'build',
                'event': 'start',
              }) +
              progressFrame({
                'ts': '2026-09-17T10:00:01.000Z',
                'source': 'worker',
                'stage': 'round',
                'event': 'start',
                'round': 2,
                'max_rounds': 6,
              }) +
              progressFrame({
                'ts': '2026-09-17T10:00:10.000Z',
                'source': 'worker',
                'stage': 'round',
                'event': 'end',
                'round': 2,
                'max_rounds': 6,
                'outcome': 'fail',
                'detail': 'verify failed: go test ./...',
              });
          return http.Response(
            body,
            200,
            headers: {'content-type': 'text/event-stream'},
          );
        }
        if (request.url.path.endsWith('/events')) {
          return http.Response('', 200);
        }
        return http.Response(inProgressRunJson, 200);
      });
      final api = RunApi(
        baseUrl: 'http://factory.test',
        client: client,
        watchInitialBackoff: const Duration(milliseconds: 1),
        watchMaxBackoff: const Duration(milliseconds: 5),
      );

      await tester.pumpWidget(
        MaterialApp(
          home: RunDetailScreen(api: api, runId: 'run-progress'),
        ),
      );
      await tester.pump();
      await tester.pump(const Duration(milliseconds: 50));

      expect(
        find.textContaining('Round 2 of 6 · verify failed: go test ./...'),
        findsOneWidget,
      );
    });

    testWidgets('agent notes are capped at the last 8, newest at the bottom', (
      tester,
    ) async {
      final client = MockClient((request) async {
        if (request.url.path.endsWith('/progress')) {
          final buffer = StringBuffer();
          buffer.write(
            progressFrame({
              'ts': '2026-09-17T10:00:00.000Z',
              'source': 'factory',
              'stage': 'build',
              'event': 'start',
            }),
          );
          buffer.write(
            progressFrame({
              'ts': '2026-09-17T10:00:01.000Z',
              'source': 'worker',
              'stage': 'round',
              'event': 'start',
              'round': 1,
              'max_rounds': 3,
            }),
          );
          for (var i = 1; i <= 10; i++) {
            buffer.write(
              progressFrame({
                'ts': '2026-09-17T10:00:0${i % 9}.000Z',
                'source': 'worker',
                'stage': 'agent',
                'event': 'note',
                'round': 1,
                'detail': 'note-$i',
              }),
            );
          }
          return http.Response(
            buffer.toString(),
            200,
            headers: {'content-type': 'text/event-stream'},
          );
        }
        if (request.url.path.endsWith('/events')) {
          return http.Response('', 200);
        }
        return http.Response(inProgressRunJson, 200);
      });
      final api = RunApi(
        baseUrl: 'http://factory.test',
        client: client,
        watchInitialBackoff: const Duration(milliseconds: 1),
        watchMaxBackoff: const Duration(milliseconds: 5),
      );

      await tester.pumpWidget(
        MaterialApp(
          home: RunDetailScreen(api: api, runId: 'run-progress'),
        ),
      );
      await tester.pump();
      await tester.pump(const Duration(milliseconds: 50));

      final notesFinder = find.byKey(const ValueKey('timeline-agent-notes'));
      expect(notesFinder, findsOneWidget);
      final lines = tester
          .widget<SelectableText>(notesFinder)
          .data!
          .split('\n');
      // Exactly the last 8 of the 10 notes sent, oldest first (newest at
      // the bottom) -- comparing whole lines (not substrings) since
      // "note-1" is otherwise itself a substring of "note-10".
      expect(lines, [for (var i = 3; i <= 10; i++) 'note-$i']);
    });

    testWidgets('a terminal run still renders its recorded Timeline history', (
      tester,
    ) async {
      final client = MockClient((request) async {
        if (request.url.path.endsWith('/progress')) {
          final body =
              progressFrame({
                'ts': '2026-08-26T11:00:00.000Z',
                'source': 'factory',
                'stage': 'prepare_workspace',
                'event': 'start',
              }) +
              progressFrame({
                'ts': '2026-08-26T11:00:05.000Z',
                'source': 'factory',
                'stage': 'prepare_workspace',
                'event': 'end',
                'outcome': 'pass',
              }) +
              progressFrame({
                'ts': '2026-08-26T11:04:00.000Z',
                'source': 'factory',
                'stage': 'finished',
                'event': 'end',
                'outcome': 'accepted',
              });
          return http.Response(
            body,
            200,
            headers: {'content-type': 'text/event-stream'},
          );
        }
        return http.Response(acceptedRunJson, 200);
      });
      final api = RunApi(baseUrl: 'http://factory.test', client: client);

      await tester.pumpWidget(
        MaterialApp(
          home: RunDetailScreen(api: api, runId: 'run-accepted'),
        ),
      );
      await tester.pumpAndSettle();

      expect(
        find.byKey(const ValueKey('timeline-row-prepare_workspace')),
        findsOneWidget,
      );
      expect(
        find.byKey(const ValueKey('timeline-row-finished')),
        findsOneWidget,
      );
      // A duration is shown for the completed stage (00:05 between the
      // start/end timestamps above).
      expect(find.textContaining('00:05'), findsOneWidget);
    });

    // "Factory build note" (progress-contract.md's "Additions"): a terminal
    // run with AgentEvidence.Rounds renders one factory-authored sub-row
    // per round, and a factory `build`/`note` progress line renders as the
    // Build row's own subtitle.
    testWidgets(
      'a terminal run with evidence rounds renders round rows and the note subtitle',
      (tester) async {
        final runJson = jsonDecode(acceptedRunJson) as Map<String, dynamic>;
        runJson['agent_evidence'] = {
          'schema_version': 1,
          'succeeded': true,
          'stopped_reason': '',
          'rounds': [
            {
              'index': 1,
              'agent': 'pi',
              'agent_returncode': 0,
              'agent_timed_out': false,
              'usage': {'input': 20000, 'output': 1200},
              'verify_passed': false,
              'verify_timed_out': false,
              'duration_s': 41.0,
            },
            {
              'index': 2,
              'agent': 'pi',
              'agent_returncode': 0,
              'agent_timed_out': false,
              'usage': {'input': 3800, 'output': 200},
              'verify_passed': true,
              'verify_timed_out': false,
              'duration_s': 12.0,
            },
          ],
        };
        final withEvidence = jsonEncode(runJson);

        final client = MockClient((request) async {
          if (request.url.path.endsWith('/progress')) {
            final body =
                progressFrame({
                  'ts': '2026-08-26T11:04:00.000Z',
                  'source': 'factory',
                  'stage': 'build',
                  'event': 'note',
                  'detail':
                      '2 rounds · r1 fail (verify) · r2 pass · 25.2k tokens',
                }) +
                progressFrame({
                  'ts': '2026-08-26T11:05:00.000Z',
                  'source': 'factory',
                  'stage': 'finished',
                  'event': 'end',
                  'outcome': 'accepted',
                });
            // charset=utf-8 explicitly: package:http defaults an
            // unspecified text/* charset to latin1, which mangles the
            // note detail's non-ASCII "·" separators into invalid UTF-8
            // once _sseOnce's utf8.decoder gets them, silently dropping
            // every line in this response (found live while writing this
            // test).
            return http.Response(
              body,
              200,
              headers: {'content-type': 'text/event-stream; charset=utf-8'},
            );
          }
          return http.Response(withEvidence, 200);
        });
        final api = RunApi(baseUrl: 'http://factory.test', client: client);

        await tester.pumpWidget(
          MaterialApp(
            home: RunDetailScreen(api: api, runId: 'run-accepted'),
          ),
        );
        await tester.pumpAndSettle();

        expect(find.textContaining('Round 1 · fail (verify)'), findsOneWidget);
        expect(find.textContaining('Round 2 · pass'), findsOneWidget);
        expect(
          find.textContaining(
            '2 rounds · r1 fail (verify) · r2 pass · 25.2k tokens',
          ),
          findsOneWidget,
        );
      },
    );

    // C8 (operator demo, 2026-09-26): a round's usage with no totalTokens
    // field must still count cacheRead/cacheWrite, not just input/output,
    // so this per-round figure agrees with the cached drafting totals
    // elsewhere in the console.
    testWidgets('a round with no totalTokens counts cacheRead/cacheWrite too', (
      tester,
    ) async {
      final runJson = jsonDecode(acceptedRunJson) as Map<String, dynamic>;
      runJson['agent_evidence'] = {
        'schema_version': 1,
        'succeeded': true,
        'stopped_reason': '',
        'rounds': [
          {
            'index': 1,
            'agent': 'pi',
            'agent_returncode': 0,
            'agent_timed_out': false,
            'usage': {
              'input': 1000,
              'output': 200,
              'cacheRead': 500,
              'cacheWrite': 300,
            },
            'verify_passed': true,
            'verify_timed_out': false,
            'duration_s': 5.0,
          },
        ],
      };
      final withEvidence = jsonEncode(runJson);

      final client = MockClient((request) async {
        if (request.url.path.endsWith('/progress')) {
          return http.Response('', 200);
        }
        return http.Response(withEvidence, 200);
      });
      final api = RunApi(baseUrl: 'http://factory.test', client: client);

      await tester.pumpWidget(
        MaterialApp(
          home: RunDetailScreen(api: api, runId: 'run-accepted'),
        ),
      );
      await tester.pumpAndSettle();

      expect(
        find.textContaining('Round 1 · pass · 2.0k tokens · 5s'),
        findsOneWidget,
      );
    });

    // "Silence is a bug" (progress-contract.md, 2026-09-18): the status
    // strip shows the server-reported waiting_reason in place of the
    // ordinary stage label, and a "Last activity" figure -- these come
    // from GET /runs/{id}'s own run record, independent of the live SSE
    // progress feed, so an empty progress stream still explains itself.
    testWidgets('status strip shows waiting_reason in place of the stage', (
      tester,
    ) async {
      final client = MockClient((request) async {
        if (request.url.path.endsWith('/progress')) {
          return http.Response('', 200);
        }
        if (request.url.path.endsWith('/events')) {
          return http.Response('', 200);
        }
        return http.Response(
          waitingRunJson(
            id: 'run-waiting',
            waitingReason: 'behind 1 run(s) on foo/bar',
          ),
          200,
        );
      });
      final api = RunApi(
        baseUrl: 'http://factory.test',
        client: client,
        watchInitialBackoff: const Duration(milliseconds: 1),
        watchMaxBackoff: const Duration(milliseconds: 5),
      );

      await tester.pumpWidget(
        MaterialApp(
          home: RunDetailScreen(api: api, runId: 'run-waiting'),
        ),
      );
      await tester.pump();
      await tester.pump(const Duration(milliseconds: 50));

      expect(
        find.descendant(
          of: find.byKey(const ValueKey('timeline-status-strip')),
          matching: find.text('behind 1 run(s) on foo/bar'),
        ),
        findsOneWidget,
      );
      expect(
        find.descendant(
          of: find.byKey(const ValueKey('timeline-status-strip')),
          matching: find.textContaining('Last activity'),
        ),
        findsOneWidget,
      );
    });

    // A non-terminal run whose progress has gone quiet past the
    // 5-minute threshold shows the shared stalled chip in the status
    // strip -- inProgressRunJson's fixed 2026-08-26 timestamps are always
    // long past that threshold relative to whenever this test runs.
    testWidgets('status strip shows a stalled chip for a quiet run', (
      tester,
    ) async {
      final client = MockClient((request) async {
        if (request.url.path.endsWith('/progress')) {
          return http.Response('', 200);
        }
        if (request.url.path.endsWith('/events')) {
          return http.Response('', 200);
        }
        return http.Response(inProgressRunJson, 200);
      });
      final api = RunApi(
        baseUrl: 'http://factory.test',
        client: client,
        watchInitialBackoff: const Duration(milliseconds: 1),
        watchMaxBackoff: const Duration(milliseconds: 5),
      );

      await tester.pumpWidget(
        MaterialApp(
          home: RunDetailScreen(api: api, runId: 'run-progress'),
        ),
      );
      await tester.pump();
      await tester.pump(const Duration(milliseconds: 50));

      expect(find.byKey(const ValueKey('stalled-chip')), findsOneWidget);
    });

    // Live bug (operator walk on a Flutter + Go app repo, 2026-09-26, Temporal path): the
    // built-in policy gates (canonical_verify, diff_scope, ...) never emit
    // their own `gate` progress events -- only optional named gates
    // (lint/security_audit/...) do -- so a run with no named gates
    // configured showed "Gates" with the neutral/skipped icon even though
    // run.gate_results recorded every check passed. The Gates row must
    // fall back to gate_results whenever the feed has no gate events.
    testWidgets(
      'Gates row renders passed from gate_results when the feed has no gate events',
      (tester) async {
        final runJson = jsonDecode(acceptedRunJson) as Map<String, dynamic>;
        runJson['gate_results'] = [
          for (final check in [
            'canonical_verify',
            'diff_scope',
            'required_files_changed',
            'tests_added',
            'full_suite_verify',
            'spec_conformity',
          ])
            {
              'check': check,
              'command': ['make', 'verify'],
              'passed': true,
              'exit_code': 0,
              'duration_ms': 1000,
              'log_sha256': 'hash-$check',
            },
        ];
        final withGateResults = jsonEncode(runJson);

        final client = MockClient((request) async {
          if (request.url.path.endsWith('/progress')) {
            final body = progressFrame({
              'ts': '2026-08-26T11:04:00.000Z',
              'source': 'factory',
              'stage': 'finished',
              'event': 'end',
              'outcome': 'accepted',
            });
            return http.Response(
              body,
              200,
              headers: {'content-type': 'text/event-stream'},
            );
          }
          return http.Response(withGateResults, 200);
        });
        final api = RunApi(baseUrl: 'http://factory.test', client: client);

        await tester.pumpWidget(
          MaterialApp(
            home: RunDetailScreen(api: api, runId: 'run-accepted'),
          ),
        );
        await tester.pumpAndSettle();

        final gateRow = find.byKey(const ValueKey('timeline-row-gate'));
        expect(gateRow, findsOneWidget);
        expect(
          find.descendant(
            of: gateRow,
            matching: find.byIcon(Icons.check_circle),
          ),
          findsOneWidget,
        );
        expect(
          find.textContaining(
            '6 passed: canonical_verify, diff_scope, '
            'required_files_changed, tests_added, full_suite_verify, '
            'spec_conformity',
          ),
          findsOneWidget,
        );
      },
    );

    testWidgets(
      'Gates row renders failed from gate_results when one check failed',
      (tester) async {
        final runJson = jsonDecode(quarantinedRunJson) as Map<String, dynamic>;
        runJson['gate_results'] = [
          {
            'check': 'canonical_verify',
            'command': ['make', 'verify'],
            'passed': true,
            'exit_code': 0,
            'duration_ms': 1000,
            'log_sha256': 'hash-canonical_verify',
          },
          {
            'check': 'tests_added',
            'command': ['make', 'verify'],
            'passed': false,
            'exit_code': 1,
            'duration_ms': 500,
            'log_sha256': 'hash-tests_added',
          },
        ];
        final withFailedGate = jsonEncode(runJson);

        final client = MockClient((request) async {
          if (request.url.path.endsWith('/progress')) {
            final body = progressFrame({
              'ts': '2026-08-26T10:03:00.000Z',
              'source': 'factory',
              'stage': 'finished',
              'event': 'end',
              'outcome': 'quarantined',
            });
            return http.Response(
              body,
              200,
              headers: {'content-type': 'text/event-stream'},
            );
          }
          return http.Response(withFailedGate, 200);
        });
        final api = RunApi(baseUrl: 'http://factory.test', client: client);

        await tester.pumpWidget(
          MaterialApp(
            home: RunDetailScreen(api: api, runId: 'run-quarantined'),
          ),
        );
        await tester.pumpAndSettle();

        final gateRow = find.byKey(const ValueKey('timeline-row-gate'));
        expect(gateRow, findsOneWidget);
        expect(
          find.descendant(of: gateRow, matching: find.byIcon(Icons.error)),
          findsOneWidget,
        );
        expect(
          find.textContaining(
            '1 failed of 2: canonical_verify (pass), tests_added (fail)',
          ),
          findsOneWidget,
        );
      },
    );

    // An in-progress run with neither gate progress events nor recorded
    // gate_results yet keeps today's pending behaviour -- the fallback
    // above must not fire before the built-in gates have actually run.
    testWidgets(
      'Gates row stays pending when neither gate events nor gate_results exist',
      (tester) async {
        final client = MockClient((request) async {
          if (request.url.path.endsWith('/progress')) {
            return http.Response('', 200);
          }
          if (request.url.path.endsWith('/events')) {
            return http.Response('', 200);
          }
          return http.Response(inProgressRunJson, 200);
        });
        final api = RunApi(
          baseUrl: 'http://factory.test',
          client: client,
          watchInitialBackoff: const Duration(milliseconds: 1),
          watchMaxBackoff: const Duration(milliseconds: 5),
        );

        await tester.pumpWidget(
          MaterialApp(
            home: RunDetailScreen(api: api, runId: 'run-progress'),
          ),
        );
        await tester.pump();
        await tester.pump(const Duration(milliseconds: 50));

        final gateRow = find.byKey(const ValueKey('timeline-row-gate'));
        expect(gateRow, findsOneWidget);
        expect(
          find.descendant(
            of: gateRow,
            matching: find.byIcon(Icons.circle_outlined),
          ),
          findsOneWidget,
        );
      },
    );
  });

  // A run recorded against a request links back to it.
  group('request linkage', () {
    testWidgets(
      'open-request button appears and navigates when requestId is set',
      (tester) async {
        final runJson = jsonDecode(acceptedRunJson) as Map<String, dynamic>;
        runJson['request_id'] = 'request-1';
        final client = MockClient((request) async {
          if (request.url.path == '/requests/request-1') {
            return http.Response(
              jsonEncode({
                'id': 'request-1',
                'workspace': '/workspaces/request-1',
                'project': 'app',
                'state': 'done',
                'submitted_at': '2026-08-26T10:00:00Z',
                'updated_at': '2026-08-26T11:04:00Z',
                'title': 'Add the widget',
              }),
              200,
            );
          }
          if (request.url.path.endsWith('/events')) {
            return http.Response('', 200);
          }
          return http.Response(jsonEncode(runJson), 200);
        });
        final api = RunApi(baseUrl: 'http://factory.test', client: client);

        await tester.pumpWidget(
          MaterialApp(
            home: RunDetailScreen(api: api, runId: 'run-accepted'),
          ),
        );
        await tester.pumpAndSettle();

        expect(
          find.byKey(const ValueKey('open-request-button')),
          findsOneWidget,
        );
        expect(find.text('Add the widget'), findsOneWidget);

        await tester.tap(find.byKey(const ValueKey('open-request-button')));
        await tester.pumpAndSettle();

        expect(find.byType(RequestDetailScreen), findsOneWidget);
      },
    );

    testWidgets('open-request button is absent when the run has no requestId', (
      tester,
    ) async {
      final client = MockClient(
        (_) async => http.Response(acceptedRunJson, 200),
      );
      final api = RunApi(baseUrl: 'http://factory.test', client: client);

      await tester.pumpWidget(
        MaterialApp(
          home: RunDetailScreen(api: api, runId: 'run-accepted'),
        ),
      );
      await tester.pumpAndSettle();

      expect(find.byKey(const ValueKey('open-request-button')), findsNothing);
    });
  });

  // The raw docker argv is collapsed by default, expandable on tap.
  testWidgets('attempt command is collapsed by default and expandable', (
    tester,
  ) async {
    final client = MockClient((_) async => http.Response(acceptedRunJson, 200));
    final api = RunApi(baseUrl: 'http://factory.test', client: client);

    await tester.pumpWidget(
      MaterialApp(
        home: RunDetailScreen(api: api, runId: 'run-accepted'),
      ),
    );
    await tester.pumpAndSettle();

    await tester.scrollUntilVisible(
      find.text('Command'),
      300,
      scrollable: find.byType(Scrollable).first,
    );
    await tester.pumpAndSettle();
    expect(
      find.textContaining('python3 build_app.py ticket-accepted'),
      findsNothing,
    );

    await tester.tap(find.text('Command'));
    await tester.pumpAndSettle();

    expect(
      find.textContaining('python3 build_app.py ticket-accepted'),
      findsOneWidget,
    );
  });

  // An attempt's log path is an in-app "Open log" action into the
  // existing build-log pane, not plain unclickable text.
  testWidgets('attempt "Open log" enables the existing log pane', (
    tester,
  ) async {
    final client = MockClient((request) async {
      if (request.url.path.endsWith('/log')) {
        return http.Response('log text', 200);
      }
      if (request.url.path.endsWith('/events')) {
        return http.Response('', 200);
      }
      return http.Response(acceptedRunJson, 200);
    });
    final api = RunApi(baseUrl: 'http://factory.test', client: client);

    await tester.pumpWidget(
      MaterialApp(
        home: RunDetailScreen(api: api, runId: 'run-accepted'),
      ),
    );
    await tester.pumpAndSettle();

    await tester.scrollUntilVisible(
      find.textContaining('Open log'),
      300,
      scrollable: find.byType(Scrollable).first,
    );
    await tester.pumpAndSettle();
    await tester.tap(find.text('Open log'));
    await tester.pumpAndSettle();

    expect(find.byKey(const ValueKey('log-pane-content')), findsOneWidget);
  });

  // "Open in Temporal UI" only appears when both the server config
  // and the run itself carry the necessary fields.
  group('Temporal UI link', () {
    testWidgets(
      'appears when temporalUiUrl and temporalWorkflowId are both set',
      (tester) async {
        final runJson = jsonDecode(acceptedRunJson) as Map<String, dynamic>;
        runJson['temporal_workflow_id'] = 'wf-123';
        final client = MockClient(
          (_) async => http.Response(jsonEncode(runJson), 200),
        );
        final api = RunApi(
          baseUrl: 'http://factory.test',
          client: client,
          temporalUiUrl: 'http://temporal.test',
        );

        await tester.pumpWidget(
          MaterialApp(
            home: RunDetailScreen(api: api, runId: 'run-accepted'),
          ),
        );
        await tester.pumpAndSettle();

        await tester.scrollUntilVisible(
          find.byKey(const ValueKey('temporal-ui-button')),
          300,
          scrollable: find.byType(Scrollable).first,
        );
        expect(
          find.byKey(const ValueKey('temporal-ui-button')),
          findsOneWidget,
        );
      },
    );

    testWidgets('is absent when temporalWorkflowId is empty', (tester) async {
      final client = MockClient(
        (_) async => http.Response(acceptedRunJson, 200),
      );
      final api = RunApi(
        baseUrl: 'http://factory.test',
        client: client,
        temporalUiUrl: 'http://temporal.test',
      );

      await tester.pumpWidget(
        MaterialApp(
          home: RunDetailScreen(api: api, runId: 'run-accepted'),
        ),
      );
      await tester.pumpAndSettle();

      expect(find.byKey(const ValueKey('temporal-ui-button')), findsNothing);
    });
  });

  // A terminal run's evidence-round sub-row ("Round N · outcome ·
  // tokens · duration") is not duplicated by the worker-relayed
  // "Round N of M · outcome" line for the same round index.
  testWidgets(
    'evidence round line is not duplicated by the worker-relayed round line',
    (tester) async {
      final runJson = jsonDecode(acceptedRunJson) as Map<String, dynamic>;
      runJson['agent_evidence'] = {
        'schema_version': 1,
        'succeeded': true,
        'stopped_reason': '',
        'rounds': [
          {
            'index': 1,
            'agent': 'pi',
            'agent_returncode': 0,
            'agent_timed_out': false,
            'usage': {'input': 20000, 'output': 1200},
            'verify_passed': true,
            'verify_timed_out': false,
            'duration_s': 41.0,
          },
        ],
      };
      final withEvidence = jsonEncode(runJson);

      final client = MockClient((request) async {
        if (request.url.path.endsWith('/progress')) {
          final body =
              'event: progress\ndata: ${jsonEncode({'ts': '2026-08-26T11:00:00.000Z', 'source': 'worker', 'stage': 'round', 'event': 'start', 'round': 1, 'max_rounds': 3})}\n\n'
              'event: progress\ndata: ${jsonEncode({'ts': '2026-08-26T11:00:41.000Z', 'source': 'worker', 'stage': 'round', 'event': 'end', 'round': 1, 'max_rounds': 3, 'outcome': 'pass'})}\n\n'
              'event: progress\ndata: ${jsonEncode({'ts': '2026-08-26T11:04:00.000Z', 'source': 'factory', 'stage': 'finished', 'event': 'end', 'outcome': 'accepted'})}\n\n';
          return http.Response(
            body,
            200,
            headers: {'content-type': 'text/event-stream'},
          );
        }
        return http.Response(withEvidence, 200);
      });
      final api = RunApi(baseUrl: 'http://factory.test', client: client);

      await tester.pumpWidget(
        MaterialApp(
          home: RunDetailScreen(api: api, runId: 'run-accepted'),
        ),
      );
      await tester.pumpAndSettle();

      // The evidence-authored line is shown once...
      expect(find.textContaining('Round 1 · pass'), findsOneWidget);
      // ...and the worker-relayed "Round 1 of 3 · pass" duplicate for the
      // same round index is gone.
      expect(find.textContaining('Round 1 of 3'), findsNothing);
    },
  );
}
