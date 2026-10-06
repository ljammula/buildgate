import 'dart:async';
import 'dart:convert';

import 'package:console/api_client.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:http/http.dart' as http;
import 'package:http/testing.dart';

import 'fixtures.dart';

void main() {
  test(
    'listRuns parses in-progress, accepted, and quarantined shapes',
    () async {
      final client = MockClient((request) async {
        expect(request.url.toString(), 'http://factory.test/runs');
        return http.Response(
          '[${inProgressRunJson.trim()},${acceptedRunJson.trim()},'
          '${quarantinedRunJson.trim()}]',
          200,
          headers: {'content-type': 'application/json'},
        );
      });
      final api = RunApi(baseUrl: 'http://factory.test', client: client);

      final runs = await api.listRuns();

      expect(runs, hasLength(3));
      expect(runs[0].state, 'slice_running');
      expect(runs[0].changedFiles, isNull);
      expect(runs[0].resultSha, isNull);

      final accepted = runs[1];
      expect(accepted.resultSha, 'result-accepted');
      expect(accepted.committedByFactoryd, isTrue);
      expect(accepted.changedFiles, ['lib/app.dart', 'test/app_test.dart']);
      expect(accepted.diffStat?.insertions, 18);
      expect(accepted.attempts.single.command, [
        'python3',
        'build_app.py',
        'ticket-accepted',
      ]);
      expect(accepted.attempts.single.logPath, '/logs/attempt.log');
      expect(accepted.gateResults.single.durationMs, 4200);
      expect(accepted.gateResults.single.logSha256, 'verify-log-hash');
      expect(accepted.overrides.single.priorState, 'quarantined');

      final quarantined = runs[2];
      expect(quarantined.changedFiles, isEmpty);
      expect(
        quarantined.notifications.single.reason,
        'canonical verification failed',
      );
      // Not terminal: an operator override can still move a quarantined
      // run to accepted/halted later, so a screen must keep watching it
      // rather than treat quarantine itself as final.
      expect(quarantined.isTerminal, isFalse);
    },
  );

  test('empty baseUrl (same-origin console) sends relative paths', () async {
    final client = MockClient((request) async {
      expect(request.url.toString(), '/runs');
      return http.Response('[]', 200);
    });
    final api = RunApi(baseUrl: '', client: client);

    final runs = await api.listRuns();

    expect(runs, isEmpty);
  });

  test('getRun fetches an encoded run id and parses the response', () async {
    final client = MockClient((request) async {
      expect(request.url.toString(), 'http://factory.test/runs/run-accepted');
      return http.Response(acceptedRunJson, 200);
    });
    final api = RunApi(baseUrl: 'http://factory.test', client: client);

    final run = await api.getRun('run-accepted');

    expect(run.id, 'run-accepted');
    expect(run.gateResults.single.passed, isTrue);
  });

  test('a halted run is terminal only once halt_confirmed is true', () async {
    final client = MockClient((request) async {
      return http.Response(
        '[${haltedRunJson.trim()},${confirmedHaltedRunJson.trim()}]',
        200,
        headers: {'content-type': 'application/json'},
      );
    });
    final api = RunApi(baseUrl: 'http://factory.test', client: client);

    final runs = await api.listRuns();

    final unconfirmed = runs[0];
    expect(unconfirmed.haltConfirmed, isFalse);
    expect(unconfirmed.isTerminal, isFalse);

    final confirmed = runs[1];
    expect(confirmed.haltConfirmed, isTrue);
    expect(confirmed.isTerminal, isTrue);
  });

  test(
    'getRunRelease parses decided, undecided, and omitempty shapes',
    () async {
      final client = MockClient((request) async {
        if (request.url.path.endsWith('/run-quarantined/release')) {
          return http.Response(undecidedReleaseJson, 200);
        }
        if (request.url.path.endsWith('/run-clean/release')) {
          return http.Response(allowedReleaseJson, 200);
        }
        expect(
          request.url.toString(),
          'http://factory.test/runs/run-accepted/release',
        );
        return http.Response(deniedReleaseJson, 200);
      });
      final api = RunApi(baseUrl: 'http://factory.test', client: client);

      final denied = await api.getRunRelease('run-accepted');
      expect(denied.project, 'checkouts');
      expect(denied.decision?.allowed, isFalse);
      expect(
        denied.decision?.reasons.single,
        contains('kill switch is engaged'),
      );
      expect(denied.decision?.evaluatedAt, '2026-09-03T10:05:00Z');
      expect(denied.killSwitch.engaged, isTrue);
      expect(denied.killSwitch.history.single.by, 'operator@example.com');
      expect(denied.killSwitch.history.single.reason, 'incident 42');

      // An allowed decision omits "reasons" entirely and a never-engaged
      // switch serializes a null history — both must decode as empty, not
      // throw.
      final allowed = await api.getRunRelease('run-clean');
      expect(allowed.decision?.allowed, isTrue);
      expect(allowed.decision?.reasons, isEmpty);
      expect(allowed.killSwitch.engaged, isFalse);
      expect(allowed.killSwitch.history, isEmpty);

      // A run with no decision recorded decodes as null — never as allowed.
      final undecided = await api.getRunRelease('run-quarantined');
      expect(undecided.decision, isNull);
    },
  );

  test(
    'read routes send the configured read token as a bearer header',
    () async {
      final seenAuthHeaders = <String, String?>{};
      final client = MockClient((request) async {
        seenAuthHeaders[request.url.path] = request.headers['Authorization'];
        if (request.url.path == '/projects') return http.Response('[]', 200);
        if (request.url.path == '/runs') return http.Response('[]', 200);
        if (request.url.path == '/runs/run-1') {
          return http.Response(acceptedRunJson, 200);
        }
        if (request.url.path == '/requests') return http.Response('[]', 200);
        if (request.url.path == '/requests/req-1') {
          return http.Response(
            jsonEncode({
              'id': 'req-1',
              'workspace': '/repos/app',
              'project': 'app',
              'state': 'spec_review',
              'submitted_at': '2026-09-10T09:00:00Z',
              'updated_at': '2026-09-10T09:00:00Z',
            }),
            200,
          );
        }
        return http.Response(jsonEncode({'diff': 'diff --git a b\n'}), 200);
      });
      final api = RunApi(
        baseUrl: 'http://factory.test',
        client: client,
        readToken: 'read-secret',
      );

      await api.listProjects();
      await api.listRuns();
      await api.getRun('run-1');
      await api.getRunDiff('run-1');
      await api.listRequests();
      await api.getRequest('req-1');

      expect(seenAuthHeaders['/projects'], 'Bearer read-secret');
      expect(seenAuthHeaders['/runs'], 'Bearer read-secret');
      expect(seenAuthHeaders['/runs/run-1'], 'Bearer read-secret');
      expect(seenAuthHeaders['/runs/run-1/diff'], 'Bearer read-secret');
      // GET /requests(+/{id}) moved server-side to the read token
      // (see listRequests' own doc comment) so the console's
      // request board works with read-only credentials, not the
      // override token approveRequest/rejectRequest still need.
      expect(seenAuthHeaders['/requests'], 'Bearer read-secret');
      expect(seenAuthHeaders['/requests/req-1'], 'Bearer read-secret');
    },
  );

  test(
    'approveRequest and rejectRequest send the override token, not the read token',
    () async {
      final seenAuthHeaders = <String, String?>{};
      final client = MockClient((request) async {
        seenAuthHeaders[request.url.path] = request.headers['Authorization'];
        return http.Response(
          jsonEncode({
            'id': 'req-1',
            'workspace': '/repos/app',
            'project': 'app',
            'state': 'planning',
            'submitted_at': '2026-09-10T09:00:00Z',
            'updated_at': '2026-09-10T09:00:00Z',
          }),
          200,
        );
      });
      final api = RunApi(
        baseUrl: 'http://factory.test',
        client: client,
        readToken: 'read-secret',
        overrideToken: 'override-secret',
      );

      await api.approveRequest('req-1');
      await api.rejectRequest('req-1', reason: 'scope is too broad');

      expect(
        seenAuthHeaders['/requests/req-1/approve'],
        'Bearer override-secret',
      );
      expect(
        seenAuthHeaders['/requests/req-1/reject'],
        'Bearer override-secret',
      );
    },
  );

  test(
    'read routes send no Authorization header when no read token is configured',
    () async {
      final client = MockClient((request) async {
        expect(request.headers.containsKey('Authorization'), isFalse);
        return http.Response('[]', 200);
      });
      final api = RunApi(baseUrl: 'http://factory.test', client: client);

      await api.listProjects();
    },
  );

  test(
    'watchRun sends the read token as a header, never in the request URL',
    () async {
      http.BaseRequest? seenRequest;
      final client = MockClient.streaming((request, bodyStream) async {
        seenRequest = request;
        final compactJson = jsonEncode(jsonDecode(acceptedRunJson));
        return http.StreamedResponse(
          Stream.value(utf8.encode('event: state\ndata: $compactJson\n\n')),
          200,
        );
      });
      final api = RunApi(
        baseUrl: 'http://factory.test',
        client: client,
        readToken: 'read-secret',
      );

      final run = await api.watchRun('run-1').first;

      expect(run.id, 'run-accepted');
      // The regression this covers (a real GitHub Codex App review
      // finding on this PR): an earlier version placed the read token in
      // the request's query string instead, which a reverse proxy or
      // access log in front of a non-loopback deployment can persist far
      // beyond this one request.
      expect(seenRequest?.url.queryParameters, isEmpty);
      expect(seenRequest?.headers['Authorization'], 'Bearer read-secret');
    },
  );

  // The regression test for a real GitHub Codex App review finding on
  // this PR: a native browser EventSource auto-reconnects on a dropped or
  // failed connection by default, but the manual streamed-request
  // transport this client replaced it with (see watchRun's own doc
  // comment) does not do that on its own -- an intermediate version
  // surfaced a single error and stopped subscribing entirely, leaving the
  // console permanently stale until reloaded. watchRun must retry with
  // backoff instead of surfacing a non-2xx response as a terminal stream
  // error.
  test(
    'watchRun retries with backoff on a non-2xx response instead of throwing',
    () async {
      var attempts = 0;
      final client = MockClient((request) async {
        attempts++;
        return http.Response('server error', 500);
      });
      final api = RunApi(
        baseUrl: 'http://factory.test',
        client: client,
        watchInitialBackoff: const Duration(milliseconds: 1),
        watchMaxBackoff: const Duration(milliseconds: 5),
      );

      final subscription = api
          .watchRun('run-1')
          .listen((_) {}, onError: (Object _) {});
      await Future<void>.delayed(const Duration(milliseconds: 50));
      await subscription.cancel();

      expect(
        attempts,
        greaterThan(1),
        reason:
            'want more than one connection attempt -- a single failure '
            'must not end the subscription for good',
      );
    },
  );

  // Same regression, for a connection that closes early (a 200 response
  // whose body ends with no event at all) rather than failing outright --
  // this must also retry, not be treated as a normal, final completion.
  test(
    'watchRun retries with backoff if the connection closes before a terminal state',
    () async {
      var attempts = 0;
      final client = MockClient((request) async {
        attempts++;
        return http.Response('', 200);
      });
      final api = RunApi(
        baseUrl: 'http://factory.test',
        client: client,
        watchInitialBackoff: const Duration(milliseconds: 1),
        watchMaxBackoff: const Duration(milliseconds: 5),
      );

      final subscription = api
          .watchRun('run-1')
          .listen((_) {}, onError: (Object _) {});
      await Future<void>.delayed(const Duration(milliseconds: 50));
      await subscription.cancel();

      expect(attempts, greaterThan(1));
    },
  );

  // Contrast case: once a terminal state is actually observed, watchRun
  // must stop for good rather than keep reconnecting forever.
  test('watchRun stops retrying once a terminal state is observed', () async {
    var attempts = 0;
    final client = MockClient((request) async {
      attempts++;
      final compactJson = jsonEncode(jsonDecode(acceptedRunJson));
      return http.Response('event: state\ndata: $compactJson\n\n', 200);
    });
    final api = RunApi(
      baseUrl: 'http://factory.test',
      client: client,
      watchInitialBackoff: const Duration(milliseconds: 1),
      watchMaxBackoff: const Duration(milliseconds: 5),
    );

    final run = await api.watchRun('run-1').first;
    expect(run.id, 'run-accepted');

    await Future<void>.delayed(const Duration(milliseconds: 50));
    expect(
      attempts,
      1,
      reason:
          'want exactly one connection attempt -- a terminal state must '
          'stop the subscription for good, not just its first event',
    );
  });

  // The regression test for a real GitHub Codex App review finding on
  // this PR: an earlier version's outer retry loop only set a `stopped`
  // flag and cancelled a pending retry timer on cancellation, but never
  // cancelled the *active* inner connection -- so navigating away while
  // mid-request (a quarantined run can stay nonterminal indefinitely,
  // and the server sends no heartbeat) left the HTTP connection and its
  // server-side handler alive indefinitely.
  test(
    'watchRun cancels the active inner connection when its subscription is cancelled',
    () async {
      final bodyController = StreamController<List<int>>();
      var innerCancelled = false;
      bodyController.onCancel = () => innerCancelled = true;

      final client = MockClient.streaming((request, bodyStream) async {
        return http.StreamedResponse(bodyController.stream, 200);
      });
      final api = RunApi(baseUrl: 'http://factory.test', client: client);

      final subscription = api
          .watchRun('run-1')
          .listen((_) {}, onError: (Object _) {});
      await Future<void>.delayed(Duration.zero);
      await subscription.cancel();
      await Future<void>.delayed(Duration.zero);

      expect(
        innerCancelled,
        isTrue,
        reason:
            'want the active connection\'s own body stream to be '
            'cancelled, not left open, once the outer subscription is '
            'cancelled',
      );
    },
  );

  // The regression test for the other half of the same review round: a
  // permanent (4xx) failure must surface as a real error, not be
  // swallowed and retried forever like a transient one -- otherwise a
  // rotated read token (403) or a pruned run (404) leaves the console
  // silently showing stale state.
  test(
    'watchRun forwards a permanent 4xx failure instead of retrying it',
    () async {
      var attempts = 0;
      final client = MockClient((request) async {
        attempts++;
        return http.Response('forbidden', 403);
      });
      final api = RunApi(
        baseUrl: 'http://factory.test',
        client: client,
        watchInitialBackoff: const Duration(milliseconds: 1),
        watchMaxBackoff: const Duration(milliseconds: 5),
      );

      await expectLater(
        api.watchRun('run-1'),
        emitsError(isA<RunApiException>()),
      );
      await Future<void>.delayed(const Duration(milliseconds: 20));
      expect(
        attempts,
        1,
        reason:
            'want exactly one attempt -- a permanent 4xx must not be '
            'retried',
      );
    },
  );

  test('approveRequest/rejectRequest send an optional "by"', () async {
    final seenBodies = <String, String>{};
    final client = MockClient((request) async {
      seenBodies[request.url.path] = request.body;
      return http.Response(
        jsonEncode({
          'id': 'req-1',
          'workspace': '/repos/app',
          'project': 'app',
          'state': 'planning',
          'submitted_at': '2026-09-10T09:00:00Z',
          'updated_at': '2026-09-10T09:00:00Z',
        }),
        200,
      );
    });
    final api = RunApi(
      baseUrl: 'http://factory.test',
      client: client,
      overrideToken: 'override-secret',
    );

    // Omitted `by` sends no body at all -- unchanged from before this
    // field existed, matching the server's own fallback.
    await api.approveRequest('req-1');
    expect(seenBodies['/requests/req-1/approve'], isEmpty);

    await api.approveRequest('req-1', by: 'jane');
    expect(jsonDecode(seenBodies['/requests/req-1/approve']!), {'by': 'jane'});

    await api.rejectRequest('req-1', reason: 'scope creep');
    expect(jsonDecode(seenBodies['/requests/req-1/reject']!), {
      'reason': 'scope creep',
    });

    await api.rejectRequest('req-1', reason: 'scope creep', by: 'jane');
    expect(jsonDecode(seenBodies['/requests/req-1/reject']!), {
      'reason': 'scope creep',
      'by': 'jane',
    });
  });

  test('approveRequest sends expected_sha256 alongside by, binding approval '
      'to the artifact shown', () async {
    final seenBodies = <String, String>{};
    final client = MockClient((request) async {
      seenBodies[request.url.path] = request.body;
      return http.Response(
        jsonEncode({
          'id': 'req-1',
          'workspace': '/repos/app',
          'project': 'app',
          'state': 'planning',
          'submitted_at': '2026-09-10T09:00:00Z',
          'updated_at': '2026-09-10T09:00:00Z',
        }),
        200,
      );
    });
    final api = RunApi(
      baseUrl: 'http://factory.test',
      client: client,
      overrideToken: 'override-secret',
    );

    await api.approveRequest(
      'req-1',
      by: 'jane',
      expectedSha256: {'spec.md': 'abc123'},
    );
    expect(jsonDecode(seenBodies['/requests/req-1/approve']!), {
      'by': 'jane',
      'expected_sha256': {'spec.md': 'abc123'},
    });

    // An empty map is the same as omitting it -- no expected_sha256
    // key at all, not an empty object the server would then treat as
    // "expects nothing" ambiguously.
    await api.approveRequest('req-1', by: 'jane', expectedSha256: {});
    expect(jsonDecode(seenBodies['/requests/req-1/approve']!), {'by': 'jane'});
  });

  test(
    'hasOverrideToken reflects whether an override token was configured',
    () {
      expect(
        RunApi(
          baseUrl: 'http://factory.test',
          client: MockClient((_) async => http.Response('', 200)),
        ).hasOverrideToken,
        isFalse,
      );
      expect(
        RunApi(
          baseUrl: 'http://factory.test',
          client: MockClient((_) async => http.Response('', 200)),
          overrideToken: 'override-secret',
        ).hasOverrideToken,
        isTrue,
      );
    },
  );

  test('listRevisions/getRevision parse the revision routes', () async {
    final client = MockClient((request) async {
      if (request.url.path == '/requests/req-1/revisions') {
        return http.Response(
          '[${revisionSummaryJson(index: 1, at: '2026-09-10T09:00:00Z', by: 'jane', reason: 'too broad', fromState: 'spec_review', files: ['spec.md'])}]',
          200,
        );
      }
      expect(request.url.path, '/requests/req-1/revisions/1');
      return http.Response(
        revisionDetailJson(
          index: 1,
          at: '2026-09-10T09:00:00Z',
          by: 'jane',
          reason: 'too broad',
          fromState: 'spec_review',
          files: {'spec.md': '# Old spec'},
        ),
        200,
      );
    });
    final api = RunApi(
      baseUrl: 'http://factory.test',
      client: client,
      readToken: 'read-secret',
    );

    final revisions = await api.listRevisions('req-1');
    expect(revisions.single.index, 1);
    expect(revisions.single.by, 'jane');
    expect(revisions.single.files, ['spec.md']);

    final detail = await api.getRevision('req-1', 1);
    expect(detail.reason, 'too broad');
    expect(detail.files['spec.md'], '# Old spec');
  });

  test('parseStateEvent decodes a raw state event', () {
    final compactJson = jsonEncode(jsonDecode(acceptedRunJson));

    final run = parseStateEvent('event: state\ndata: $compactJson\n\n');

    expect(run.id, 'run-accepted');
    expect(run.state, 'accepted');
    expect(run.gateResults.single.exitCode, 0);
  });

  // The `/requests/events` counterpart to parseStateEvent -- same wire
  // shape, decoded into a RequestSummary.
  test('parseRequestStateEvent decodes a raw state event', () {
    final compactJson = jsonDecode(
      requestJson(id: 'req-1', state: 'plan_review'),
    );

    final request = parseRequestStateEvent(
      'event: state\ndata: ${jsonEncode(compactJson)}\n\n',
    );

    expect(request.id, 'req-1');
    expect(request.state, 'plan_review');
  });

  test(
    'watchRequests sends the read token as a header, never in the request URL',
    () async {
      http.BaseRequest? seenRequest;
      final client = MockClient.streaming((request, bodyStream) async {
        seenRequest = request;
        final compactJson = jsonEncode(
          jsonDecode(requestJson(id: 'req-1', state: 'plan_review')),
        );
        return http.StreamedResponse(
          Stream.value(utf8.encode('event: state\ndata: $compactJson\n\n')),
          200,
        );
      });
      final api = RunApi(
        baseUrl: 'http://factory.test',
        client: client,
        readToken: 'read-secret',
      );

      final request = await api.watchRequests().first;

      expect(request.id, 'req-1');
      expect(seenRequest?.url.path, '/requests/events');
      expect(seenRequest?.url.queryParameters, isEmpty);
      expect(seenRequest?.headers['Authorization'], 'Bearer read-secret');
    },
  );

  // watchRequests has no terminal state (unlike watchRun) -- it must keep
  // retrying transient failures with backoff indefinitely, the same
  // reconnect behavior watchRun's own regression tests cover.
  test(
    'watchRequests retries with backoff on a non-2xx response instead of throwing',
    () async {
      var attempts = 0;
      final client = MockClient((request) async {
        attempts++;
        return http.Response('server error', 500);
      });
      final api = RunApi(
        baseUrl: 'http://factory.test',
        client: client,
        watchInitialBackoff: const Duration(milliseconds: 1),
        watchMaxBackoff: const Duration(milliseconds: 5),
      );

      final subscription = api.watchRequests().listen(
        (_) {},
        onError: (Object _) {},
      );
      await Future<void>.delayed(const Duration(milliseconds: 50));
      await subscription.cancel();

      expect(
        attempts,
        greaterThan(1),
        reason:
            'want more than one connection attempt -- a single failure '
            'must not end the subscription for good',
      );
    },
  );

  test(
    'watchRequests forwards a permanent 4xx failure instead of retrying it',
    () async {
      var attempts = 0;
      final client = MockClient((request) async {
        attempts++;
        return http.Response('forbidden', 403);
      });
      final api = RunApi(
        baseUrl: 'http://factory.test',
        client: client,
        watchInitialBackoff: const Duration(milliseconds: 1),
        watchMaxBackoff: const Duration(milliseconds: 5),
      );

      await expectLater(
        api.watchRequests(),
        emitsError(isA<RunApiException>()),
      );
      await Future<void>.delayed(const Duration(milliseconds: 20));
      expect(
        attempts,
        1,
        reason:
            'want exactly one attempt -- a permanent 4xx must not be '
            'retried',
      );
    },
  );

  // The freshness indicator relies on this callback to
  // tell "connected" apart from "reconnecting" -- see watchRequests' own
  // doc comment.
  test(
    'watchRequests reports connection open/close via onConnectionChange',
    () async {
      var attempts = 0;
      final client = MockClient((request) async {
        attempts++;
        if (attempts == 1) {
          // A 200 whose body ends with no event -- a connection that
          // opened successfully but then closed, exactly like the
          // "retries if the connection closes before a terminal state"
          // regression watchRun's own tests cover.
          return http.Response('', 200);
        }
        final compactJson = jsonEncode(
          jsonDecode(requestJson(id: 'req-1', state: 'plan_review')),
        );
        return http.Response('event: state\ndata: $compactJson\n\n', 200);
      });
      final api = RunApi(
        baseUrl: 'http://factory.test',
        client: client,
        watchInitialBackoff: const Duration(milliseconds: 1),
        watchMaxBackoff: const Duration(milliseconds: 5),
      );

      final connectionStates = <bool>[];
      final subscription = api
          .watchRequests(onConnectionChange: connectionStates.add)
          .listen((_) {}, onError: (Object _) {});
      await Future<void>.delayed(const Duration(milliseconds: 50));
      await subscription.cancel();

      // First attempt opens (true) then ends without an event (false,
      // fired before the retry's backoff delay); a later attempt opens
      // again (true).
      expect(connectionStates, contains(true));
      expect(connectionStates, contains(false));
      expect(connectionStates.first, isTrue);
    },
  );

  test('watchRequests releases the connection if cancelled before it '
      'finishes connecting (regression: onCancel only ever cancelled the '
      "SSE line subscription, which doesn't exist yet while _client.send "
      'is still in flight -- a screen disposed mid-connect used to leave '
      "the eventual response's HTTP connection open for good once it "
      'arrived, since nothing was left to cancel it)', () async {
    final sendCalled = Completer<void>();
    final releaseResponse = Completer<http.StreamedResponse>();
    final bodyCancelled = Completer<void>();
    final bodyController = StreamController<List<int>>(
      onCancel: () => bodyCancelled.complete(),
    );

    final client = MockClient.streaming((request, bodyStream) async {
      sendCalled.complete();
      return releaseResponse.future;
    });
    final api = RunApi(baseUrl: 'http://factory.test', client: client);

    final subscription = api.watchRequests().listen(
      (_) {},
      onError: (Object _) {},
    );
    // The request is now in flight, awaiting our own response --
    // exactly the window the fix targets.
    await sendCalled.future;
    await subscription.cancel();

    // The response "arrives" only after cancellation. Without the
    // fix, _sseOnce would attach a real listener to bodyController's
    // stream and keep it open; with the fix, it must recognize the
    // cancellation and release the stream immediately instead.
    releaseResponse.complete(http.StreamedResponse(bodyController.stream, 200));

    await bodyCancelled.future.timeout(
      const Duration(seconds: 2),
      onTimeout: () => fail(
        'the response body stream was never released after a '
        'cancel-before-connect -- the connection leaked',
      ),
    );
  });

  test(
    'watchRunLog sends the read token as a header, never in the request URL, '
    'and streams the response body as text chunks',
    () async {
      http.BaseRequest? seenRequest;
      final client = MockClient.streaming((request, bodyStream) async {
        seenRequest = request;
        return http.StreamedResponse(
          Stream.value(utf8.encode('building ticket-1\n')),
          200,
        );
      });
      final api = RunApi(
        baseUrl: 'http://factory.test',
        client: client,
        readToken: 'read-secret',
      );

      final chunks = await api.watchRunLog('run-1').toList();

      expect(chunks.join(), 'building ticket-1\n');
      expect(seenRequest?.url.path, '/runs/run-1/log');
      expect(seenRequest?.url.queryParameters['follow'], '1');
      expect(seenRequest?.url.queryParameters['token'], isNull);
      expect(seenRequest?.headers['Authorization'], 'Bearer read-secret');
    },
  );

  test('watchRunLog releases the connection if cancelled before it '
      'finishes connecting (regression: the same mid-connect leak '
      'watchRequests above was fixed for, left in watchRunLog -- leaving a '
      "run page while its log tail connected kept the ?follow=1 "
      'connection open for good)', () async {
    final sendCalled = Completer<void>();
    final releaseResponse = Completer<http.StreamedResponse>();
    final bodyCancelled = Completer<void>();
    final bodyController = StreamController<List<int>>(
      onCancel: () => bodyCancelled.complete(),
    );

    final client = MockClient.streaming((request, bodyStream) async {
      sendCalled.complete();
      return releaseResponse.future;
    });
    final api = RunApi(baseUrl: 'http://factory.test', client: client);

    final subscription = api
        .watchRunLog('run-1')
        .listen((_) {}, onError: (Object _) {});
    await sendCalled.future;
    await subscription.cancel();

    releaseResponse.complete(http.StreamedResponse(bodyController.stream, 200));

    await bodyCancelled.future.timeout(
      const Duration(seconds: 2),
      onTimeout: () => fail(
        'the log response body stream was never released after a '
        'cancel-before-connect -- the connection leaked',
      ),
    );
  });

  test('watchRunLog surfaces a non-2xx response as an error', () async {
    final client = MockClient((request) async {
      return http.Response('not found', 404);
    });
    final api = RunApi(baseUrl: 'http://factory.test', client: client);

    await expectLater(
      api.watchRunLog('missing-run'),
      emitsError(isA<RunApiException>()),
    );
  });

  test(
    'non-ASCII JSON bodies decode as UTF-8 even though the server sends '
    'application/json with no charset (package:http would use Latin-1)',
    () async {
      http.Response utf8Json(String body, int status) => http.Response.bytes(
        utf8.encode(body),
        status,
        headers: {'content-type': 'application/json'},
      );
      final client = MockClient((request) async {
        if (request.url.path.endsWith('/oracle')) {
          return utf8Json(
            '{"files":[],"problems":["caf\u00e9 \u202e"],"state":"oracle_review"}',
            200,
          );
        }
        return utf8Json('{"error":"caf\u00e9 \u65e5"}', 400);
      });
      final api = RunApi(baseUrl: 'http://factory.test', client: client);

      final listing = await api.getRequestOracle('req-1');
      expect(listing.problems, ['caf\u00e9 \u202e']);
      await expectLater(
        api.getRequest('req-1'),
        throwsA(
          isA<RunApiException>().having(
            (e) => e.message,
            'message',
            'caf\u00e9 \u65e5',
          ),
        ),
      );
    },
  );

  test('listDaemons sends the start token, which is what the server gates '
      'GET /daemons on', () async {
    String? auth;
    final client = MockClient((request) async {
      auth = request.headers['Authorization'];
      return http.Response('[]', 200);
    });
    final api = RunApi(
      baseUrl: 'http://factory.test',
      client: client,
      startToken: 'start-tok',
      readToken: 'read-tok',
    );
    await api.listDaemons();
    expect(auth, 'Bearer start-tok');
  });

  test('getQueueRunStatus sends the read token, which is what the server '
      'gates GET /queue-run on', () async {
    String? auth;
    final client = MockClient((request) async {
      auth = request.headers['Authorization'];
      return http.Response('{"state":"alive"}', 200);
    });
    final api = RunApi(
      baseUrl: 'http://factory.test',
      client: client,
      startToken: 'start-tok',
      readToken: 'read-tok',
    );
    final status = await api.getQueueRunStatus();
    expect(auth, 'Bearer read-tok');
    expect(status.state, 'alive');
  });
}
