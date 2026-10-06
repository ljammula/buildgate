import 'dart:convert';

import 'package:console/api_client.dart';
import 'package:console/content_hash.dart';
import 'package:console/operator_identity.dart';
import 'package:console/request_detail_screen.dart';
import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:http/http.dart' as http;
import 'package:http/testing.dart';

import 'fixtures.dart';

const _ticket1 = 'Verify-Command: true\n## Ticket one\n';
const _ticket2 = 'Verify-Command: true\n## Ticket two\n';

// A fake factoryd at plan_review: two tickets, whose materialized oracle
// files are served from [oracle] (ticket index -> name -> bytes).
class _PlanServer {
  _PlanServer(this.oracle);

  Map<int, Map<String, List<int>>> oracle;
  int? oracleStatus;
  Map<int, List<String>> problems = {};
  String? approveError;
  Map<String, dynamic>? approveBody;
  final requests = <String>[];

  MockClient get client => MockClient((req) async {
    requests.add('${req.method} ${req.url.path}');
    final path = req.url.path;
    http.Response json(String body, [int status = 200]) =>
        http.Response.bytes(utf8.encode(body), status);
    final oracleMatch = RegExp(
      r'^/requests/req-1/tickets/(\d+)/oracle(?:/(.+))?$',
    ).firstMatch(path);
    if (oracleMatch != null) {
      if (oracleStatus != null) return json('{"error":"x"}', oracleStatus!);
      final files = oracle[int.parse(oracleMatch.group(1)!)] ?? {};
      final name = oracleMatch.group(2);
      if (name == null) {
        return json(
          jsonEncode({
            'files': [
              for (final e in files.entries)
                {
                  'name': e.key,
                  'size': e.value.length,
                  'sha256': sha256HexBytes(e.value),
                },
            ],
            'problems': problems[int.parse(oracleMatch.group(1)!)] ?? [],
            'state': 'plan_review',
          }),
        );
      }
      return http.Response.bytes(files[Uri.decodeComponent(name)]!, 200);
    }
    if (path == '/requests/req-1/approve') {
      approveBody = jsonDecode(req.body) as Map<String, dynamic>;
      if (approveError != null) {
        return json(jsonEncode({'error': approveError}), 409);
      }
      return json(requestJson(id: 'req-1', state: 'building', title: 'T'));
    }
    return json(
      requestJson(
        id: 'req-1',
        state: 'plan_review',
        title: 'T',
        tickets: [
          requestTicketJson(
            index: 1,
            specPath: '/srv/data/requests/req-1/tickets/001.spec.md',
            content: _ticket1,
          ),
          requestTicketJson(
            index: 2,
            specPath: '/srv/data/requests/req-1/tickets/002.spec.md',
            content: _ticket2,
          ),
        ],
      ),
    );
  });
}

Future<void> _pump(WidgetTester tester, _PlanServer server) async {
  tester.view.physicalSize = const Size(1200, 4000);
  tester.view.devicePixelRatio = 1;
  addTearDown(tester.view.resetPhysicalSize);
  final api = RunApi(
    baseUrl: 'http://factory.test',
    client: server.client,
    overrideToken: 'tok',
  );
  await tester.pumpWidget(
    MaterialApp(
      home: RequestDetailScreen(api: api, requestId: 'req-1'),
    ),
  );
  await tester.pumpAndSettle();
}

Finder _approve() => find.byKey(const ValueKey('approve-request-button'));

bool _approveEnabled(WidgetTester tester) =>
    tester.widget<FilledButton>(_approve()).onPressed != null;

Future<void> _toggle(WidgetTester tester, String keyId) async {
  await tester.tap(
    find
        .descendant(
          of: find.byKey(ValueKey('oracle-file-$keyId')),
          matching: find.byType(ListTile),
        )
        .first,
  );
  await tester.pumpAndSettle();
}

Future<void> _confirmApprove(WidgetTester tester) async {
  await tester.tap(_approve());
  await tester.pumpAndSettle();
  await tester.tap(find.byKey(const ValueKey('approve-confirm-button')));
  await tester.pumpAndSettle();
}

List<int> _b(String s) => utf8.encode(s);

void main() {
  setUp(() => setOperatorName('operator'));

  final files = {
    1: {
      'RUN_COMMAND.txt': _b('go test ./.oracle/...\n'),
      'a_oracle_test.go': _b('package a\n'),
    },
    2: {'RUN_COMMAND.txt': _b('go test ./.oracle/...\n')},
  };

  testWidgets('groups each ticket\'s oracle files, keeps Approve disabled '
      'until every one is open, then sends their hashes with the spec hashes', (
    tester,
  ) async {
    final server = _PlanServer(files);
    await _pump(tester, server);

    expect(find.text('Ticket oracle files'), findsOneWidget);
    expect(find.text('Ticket 1 (tickets/001.oracle/)'), findsOneWidget);
    expect(find.text('Ticket 2 (tickets/002.oracle/)'), findsOneWidget);
    expect(_approveEnabled(tester), isFalse);

    await _toggle(tester, '1/RUN_COMMAND.txt');
    await _toggle(tester, '1/a_oracle_test.go');
    expect(_approveEnabled(tester), isFalse);
    expect(find.text('2 of 3 shown'), findsOneWidget);
    await _toggle(tester, '2/RUN_COMMAND.txt');
    expect(_approveEnabled(tester), isTrue);

    await _confirmApprove(tester);
    expect(server.approveBody!['expected_sha256'], {
      'tickets/001.spec.md': sha256Hex(_ticket1),
      'tickets/002.spec.md': sha256Hex(_ticket2),
      for (final t in files.entries)
        for (final f in t.value.entries)
          'tickets/00${t.key}.oracle/${f.key}': sha256HexBytes(f.value),
    });
  });

  testWidgets('a collapsed ticket oracle file is not shown: Approve disables '
      'again', (tester) async {
    final server = _PlanServer({1: files[1]!});
    await _pump(tester, server);
    await _toggle(tester, '1/RUN_COMMAND.txt');
    await _toggle(tester, '1/a_oracle_test.go');
    expect(_approveEnabled(tester), isTrue);
    await _toggle(tester, '1/a_oracle_test.go');
    expect(_approveEnabled(tester), isFalse);
  });

  testWidgets('tickets with no oracle files approve immediately with just the '
      'spec hashes (no panel)', (tester) async {
    final server = _PlanServer({});
    await _pump(tester, server);
    expect(find.text('Ticket oracle files'), findsNothing);
    expect(_approveEnabled(tester), isTrue);
    await _confirmApprove(tester);
    expect(server.approveBody!['expected_sha256'], {
      'tickets/001.spec.md': sha256Hex(_ticket1),
      'tickets/002.spec.md': sha256Hex(_ticket2),
    });
  });

  testWidgets('a server without the ticket oracle route (404) is treated as '
      'having no files', (tester) async {
    final server = _PlanServer(files)..oracleStatus = 404;
    await _pump(tester, server);
    expect(_approveEnabled(tester), isTrue);
  });

  testWidgets('a failed ticket oracle listing shows an error, keeps Approve '
      'disabled, and Reload recovers', (tester) async {
    final server = _PlanServer(files)..oracleStatus = 500;
    await _pump(tester, server);
    expect(find.byKey(const ValueKey('oracle-stale-listing')), findsOneWidget);
    expect(_approveEnabled(tester), isFalse);

    server.oracleStatus = null;
    await tester.tap(find.byKey(const ValueKey('ticket-oracle-reload-button')));
    await tester.pumpAndSettle();
    expect(find.byKey(const ValueKey('oracle-stale-listing')), findsNothing);
    expect(find.text('Ticket 1 (tickets/001.oracle/)'), findsOneWidget);
    expect(_approveEnabled(tester), isFalse);
  });

  testWidgets('an existing but empty ticket oracle directory shows the '
      'approval-blocking problem and keeps Approve disabled', (tester) async {
    final server = _PlanServer({})
      ..problems = {
        1: ['no RUN_COMMAND.txt -- add it, or remove the directory to skip'],
      };
    await _pump(tester, server);

    expect(find.byKey(const ValueKey('oracle-problems')), findsOneWidget);
    expect(find.textContaining('no RUN_COMMAND.txt'), findsOneWidget);
    expect(_approveEnabled(tester), isFalse);
  });

  testWidgets('a ticket oracle file changed after it was shown is refused by '
      'the server and the plain message is shown', (tester) async {
    final server = _PlanServer({1: files[1]!})
      ..approveError =
          'request req-1: tickets/001.oracle/a_oracle_test.go artifact changed '
          'since it was fetched';
    await _pump(tester, server);
    await _toggle(tester, '1/RUN_COMMAND.txt');
    await _toggle(tester, '1/a_oracle_test.go');
    await _confirmApprove(tester);
    expect(
      find.textContaining('artifact changed since it was fetched'),
      findsOneWidget,
    );
    expect(find.textContaining('Run API request failed'), findsNothing);
    // The panel starts over: nothing counts as shown until re-opened.
    expect(_approveEnabled(tester), isFalse);
    await _toggle(tester, '1/RUN_COMMAND.txt');
    await _toggle(tester, '1/a_oracle_test.go');
    expect(_approveEnabled(tester), isTrue);
  });

  testWidgets('ticket oracle content shows hidden characters as escapes', (
    tester,
  ) async {
    final server = _PlanServer({
      1: {
        'RUN_COMMAND.txt': _b('go test\u202E ./...\n'),
        'b_oracle_test.go': [0x70, 0x80],
      },
    });
    await _pump(tester, server);
    await _toggle(tester, '1/RUN_COMMAND.txt');
    await _toggle(tester, '1/b_oracle_test.go');
    String content(String id) => tester
        .widget<SelectableText>(
          find.descendant(
            of: find.byKey(ValueKey('oracle-content-$id')),
            matching: find.byType(SelectableText),
          ),
        )
        .textSpan!
        .toPlainText();
    expect(content('1/RUN_COMMAND.txt'), 'go test\\u{202E} ./...\n');
    expect(content('1/b_oracle_test.go'), r'p\x80');
  });
}
