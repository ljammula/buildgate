import 'dart:convert';

import 'package:console/api_client.dart';
import 'package:console/approve_reject.dart';
import 'package:console/content_hash.dart';
import 'package:console/models.dart';
import 'package:console/operator_identity.dart';
import 'package:console/text_escape.dart';
import 'package:console/request_detail_screen.dart';
import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:http/http.dart' as http;
import 'package:http/testing.dart';

import 'fixtures.dart';

const _manifest =
    '[{"criterion":"1. Returns 200.","oracle_file":"a_oracle_test.go",'
    '"criterion_index":1,"target_path":"internal/a/a.go","supersedes":[]},'
    '{"criterion":"2. Code is clean.","oracle_file":null,'
    '"rationale":"judgment call","criterion_index":2,"supersedes":[]}]';

// A fake factoryd serving the oracle routes from a mutable file map.
http.Response _utf8Response(String body, int status) =>
    http.Response.bytes(utf8.encode(body), status);

class _Server {
  _Server(this.files, {this.state = 'oracle_review'});

  Map<String, String> files;
  String state;
  List<String> problems = [];
  Map<String, dynamic>? draft;
  // Files served as raw bytes (may be invalid UTF-8); override [files].
  Map<String, List<int>> raw = {};
  bool failListing = false;
  String? putError;
  String? approveError;
  final requests = <String>[];
  Map<String, dynamic>? approveBody;
  Map<String, dynamic>? putBody;

  MockClient get client => MockClient((req) async {
    requests.add('${req.method} ${req.url.path}');
    final path = req.url.path;
    if (path == '/requests/req-1/oracle') {
      if (failListing) return _utf8Response('boom', 500);
      return _utf8Response(
        jsonEncode({
          'files': [
            for (final e in {
              for (final f in files.entries) f.key: utf8.encode(f.value),
              ...raw,
            }.entries)
              {
                'name': e.key,
                'size': e.value.length,
                'sha256': sha256HexBytes(e.value),
              },
          ],
          'problems': problems,
          'oracle_draft': draft,
          'state': state,
        }),
        200,
      );
    }
    if (path.startsWith('/requests/req-1/oracle/')) {
      final name = path.substring('/requests/req-1/oracle/'.length);
      if (req.method == 'PUT') {
        putBody = jsonDecode(req.body) as Map<String, dynamic>;
        if (putError != null) {
          return _utf8Response(jsonEncode({'error': putError}), 422);
        }
        files[name] = putBody!['content'] as String;
        return _utf8Response(
          jsonEncode({'name': name, 'sha256': sha256Hex(files[name]!)}),
          200,
        );
      }
      return http.Response.bytes(raw[name] ?? utf8.encode(files[name]!), 200);
    }
    if (path == '/requests/req-1/approve') {
      approveBody = jsonDecode(req.body) as Map<String, dynamic>;
      if (approveError != null) {
        return _utf8Response(jsonEncode({'error': approveError}), 400);
      }
      return _utf8Response(
        requestJson(id: 'req-1', state: 'planning', title: 'T'),
        200,
      );
    }
    return _utf8Response(
      requestJson(id: 'req-1', state: state, title: 'T', oracleDraft: draft),
      200,
    );
  });
}

Future<void> _pump(
  WidgetTester tester,
  _Server server, {
  String? overrideToken = 'tok',
  bool settle = true,
}) async {
  tester.view.physicalSize = const Size(1200, 3000);
  tester.view.devicePixelRatio = 1;
  addTearDown(tester.view.resetPhysicalSize);
  final api = RunApi(
    baseUrl: 'http://factory.test',
    client: server.client,
    overrideToken: overrideToken,
  );
  await tester.pumpWidget(
    MaterialApp(
      home: RequestDetailScreen(api: api, requestId: 'req-1'),
    ),
  );
  // The drafting notice's spinner never settles.
  if (settle) {
    await tester.pumpAndSettle();
  } else {
    await tester.pump();
    await tester.pump(const Duration(milliseconds: 100));
  }
}

Finder _approve() => find.byKey(const ValueKey('approve-request-button'));

Future<void> _open(WidgetTester tester, String name) async {
  await tester.tap(
    find
        .descendant(
          of: find.byKey(ValueKey('oracle-file-$name')),
          matching: find.byType(ListTile),
        )
        .first,
  );
  await tester.pumpAndSettle();
}

bool _approveEnabled(WidgetTester tester) =>
    tester.widget<FilledButton>(_approve()).onPressed != null;

void main() {
  setUp(() => setOperatorName('operator'));

  final files = {
    'MANIFEST.json': _manifest,
    'RUN_COMMAND.txt': 'go test ./...\n',
    'a_oracle_test.go': 'package a\n',
  };

  testWidgets('lists files with sha256, keeps Approve disabled until every '
      'file has been opened, then approves with the displayed hashes', (
    tester,
  ) async {
    final server = _Server(Map.of(files));
    await _pump(tester, server);

    expect(find.text('RUN_COMMAND.txt'), findsOneWidget);
    expect(
      find.textContaining(sha256Hex('go test ./...\n').substring(0, 12)),
      findsOneWidget,
    );
    expect(_approveEnabled(tester), isFalse);
    expect(
      find.text('Files (0 of 3 shown) -- open every one to enable Approve'),
      findsOneWidget,
    );
    expect(find.text('Open 3 more files to approve.'), findsOneWidget);

    await _open(tester, 'RUN_COMMAND.txt');
    final content = tester.widget<SelectableText>(
      find.descendant(
        of: find.byKey(const ValueKey('oracle-content-RUN_COMMAND.txt')),
        matching: find.byType(SelectableText),
      ),
    );
    expect(content.data, 'go test ./...\n');
    expect(content.style?.fontFamily, 'monospace');
    expect(_approveEnabled(tester), isFalse);

    await _open(tester, 'a_oracle_test.go');
    expect(_approveEnabled(tester), isFalse);
    expect(find.text('Open 1 more file to approve.'), findsOneWidget);
    await _open(tester, 'MANIFEST.json');
    expect(_approveEnabled(tester), isTrue);

    await tester.tap(_approve());
    await tester.pumpAndSettle();
    await tester.tap(find.byKey(const ValueKey('approve-confirm-button')));
    await tester.pumpAndSettle();

    expect(server.approveBody!['expected_sha256'], {
      for (final e in files.entries) 'oracle/${e.key}': sha256Hex(e.value),
    });
  });

  testWidgets('renders MANIFEST criteria coverage, including uncovered '
      'criteria and their rationale', (tester) async {
    await _pump(tester, _Server(Map.of(files)));

    final coverage = find.byKey(const ValueKey('oracle-coverage'));
    expect(coverage, findsOneWidget);
    expect(
      find.descendant(
        of: coverage,
        matching: find.textContaining('Covered by a_oracle_test.go'),
      ),
      findsOneWidget,
    );
    expect(
      find.descendant(
        of: coverage,
        matching: find.textContaining(
          'Not covered by an oracle: judgment call',
        ),
      ),
      findsOneWidget,
    );
    // Fetching MANIFEST for coverage does not count as showing its file.
    expect(_approveEnabled(tester), isFalse);
  });

  testWidgets('shows the problems list and keeps Approve disabled even with '
      'every file opened', (tester) async {
    final server = _Server({'a_oracle_test.go': 'package a\n'})
      ..problems = ['no RUN_COMMAND.txt -- add it before approving'];
    await _pump(tester, server);

    expect(
      find.textContaining('no RUN_COMMAND.txt -- add it before approving'),
      findsOneWidget,
    );
    await _open(tester, 'a_oracle_test.go');
    expect(_approveEnabled(tester), isFalse);
  });

  testWidgets('an empty oracle/ approves as a skip with no hashes', (
    tester,
  ) async {
    final server = _Server({});
    await _pump(tester, server);

    expect(find.byKey(const ValueKey('oracle-empty')), findsOneWidget);
    expect(find.text('Approve (skip oracle)'), findsOneWidget);
    expect(_approveEnabled(tester), isTrue);
    await tester.tap(_approve());
    await tester.pumpAndSettle();
    await tester.tap(find.byKey(const ValueKey('approve-confirm-button')));
    await tester.pumpAndSettle();
    expect(server.approveBody!.containsKey('expected_sha256'), isFalse);
  });

  testWidgets('approving an empty oracle/ after a failed draft warns in the '
      'confirm sheet, and the approval is still allowed', (tester) async {
    final server = _Server({})
      ..draft = {'status': 'failed', 'detail': 'oracle drafting failed: boom'};
    await _pump(tester, server);
    await tester.tap(_approve());
    await tester.pumpAndSettle();
    final warning = find.byKey(const ValueKey('approve-oracle-skip-warning'));
    expect(warning, findsOneWidget);
    expect(
      tester.widget<Text>(warning).data,
      contains('skips the oracle stage'),
    );
    expect(find.textContaining('boom'), findsWidgets);
    await tester.tap(find.byKey(const ValueKey('approve-confirm-button')));
    await tester.pumpAndSettle();
    expect(server.approveBody, isNotNull);
  });

  testWidgets('a very long skip warning scrolls instead of overflowing', (
    tester,
  ) async {
    final server = _Server({})
      ..draft = {'status': 'failed', 'detail': 'boom ' * 4000};
    await _pump(tester, server);
    await tester.ensureVisible(_approve());
    await tester.pumpAndSettle();
    await tester.tap(_approve());
    await tester.pumpAndSettle();
    expect(tester.takeException(), isNull);
    expect(
      find.descendant(
        of: find.byType(ApproveConfirmSheet),
        matching: find.byType(SingleChildScrollView),
      ),
      findsOneWidget,
    );
    final confirm = find.byKey(const ValueKey('approve-confirm-button'));
    await tester.ensureVisible(confirm);
    await tester.pumpAndSettle();
    await tester.tap(confirm);
    await tester.pumpAndSettle();
    expect(server.approveBody, isNotNull);
  });

  testWidgets('a none_eligible draft skips quietly', (tester) async {
    final server = _Server({})
      ..draft = {'status': 'none_eligible', 'detail': 'nothing checkable'};
    await _pump(tester, server);
    await tester.tap(_approve());
    await tester.pumpAndSettle();
    expect(
      find.byKey(const ValueKey('approve-oracle-skip-warning')),
      findsNothing,
    );
    // Quiet means no error-coloured warning, not silence: the sheet still
    // states what skipping costs (console-walk, 2026-09-24).
    expect(
      find.byKey(const ValueKey('approve-oracle-skip-consequence')),
      findsOneWidget,
    );
  });

  testWidgets('the request detail shows the server-recorded oracle skip '
      'warning after the approval', (tester) async {
    final client = MockClient(
      (request) async => http.Response(
        requestJson(
          id: 'req-1',
          state: 'planning',
          title: 'T',
          oracleDraft: {'status': 'failed', 'detail': 'boom'},
          oracleSkipWarning: 'the oracle stage is being skipped: test',
        ),
        200,
      ),
    );
    final api = RunApi(baseUrl: 'http://factory.test', client: client);
    await tester.pumpWidget(
      MaterialApp(
        home: RequestDetailScreen(api: api, requestId: 'req-1'),
      ),
    );
    await tester.pumpAndSettle();
    final callout = find.byKey(const ValueKey('oracle-skip-warning'));
    expect(callout, findsOneWidget);
    expect(
      tester.widget<Text>(callout).data,
      contains('oracle stage is being skipped'),
    );
  });

  test('oracle_skip_warning parses tolerantly', () {
    RequestSummary parse(Object? value) => RequestSummary.fromJson(
      jsonDecode(
            requestJson(id: 'r', state: 'planning', oracleSkipWarning: value),
          )
          as Map<String, dynamic>,
    );
    expect(parse('warn').oracleSkipWarning, 'warn');
    expect(parse(42).oracleSkipWarning, '');
    expect(parse(null).oracleSkipWarning, '');
  });

  testWidgets('a file that changed on the server after it was shown must be '
      're-shown before approval, and the new hash is what is sent', (
    tester,
  ) async {
    final server = _Server({'RUN_COMMAND.txt': 'go test ./...\n'});
    await _pump(tester, server);
    await _open(tester, 'RUN_COMMAND.txt');
    expect(_approveEnabled(tester), isTrue);

    server.files['RUN_COMMAND.txt'] = 'go test ./internal/...\n';
    await tester.tap(find.byKey(const ValueKey('oracle-reload-button')));
    await tester.pumpAndSettle();

    // The open tile re-fetched the changed bytes, so it shows the new
    // content and approval carries the new hash.
    expect(find.text('go test ./internal/...\n'), findsOneWidget);
    await tester.tap(_approve());
    await tester.pumpAndSettle();
    await tester.tap(find.byKey(const ValueKey('approve-confirm-button')));
    await tester.pumpAndSettle();
    expect(server.approveBody!['expected_sha256'], {
      'oracle/RUN_COMMAND.txt': sha256Hex('go test ./internal/...\n'),
    });
  });

  testWidgets('editing RUN_COMMAND.txt PUTs the content and shows the saved '
      'file', (tester) async {
    final server = _Server(Map.of(files));
    await _pump(tester, server);
    await _open(tester, 'RUN_COMMAND.txt');

    await tester.tap(find.byKey(const ValueKey('oracle-edit-run-command')));
    await tester.pumpAndSettle();
    await tester.enterText(
      find.byKey(const ValueKey('oracle-run-command-field')),
      'go test ./internal/a/...\n',
    );
    await tester.tap(find.byKey(const ValueKey('oracle-run-command-save')));
    await tester.pumpAndSettle();

    expect(server.putBody, {'content': 'go test ./internal/a/...\n'});
    expect(find.text('go test ./internal/a/...\n'), findsOneWidget);
    expect(
      find.byKey(const ValueKey('oracle-run-command-field')),
      findsNothing,
    );
  });

  testWidgets('a 422 from saving RUN_COMMAND.txt renders inline and keeps the '
      'editor open', (tester) async {
    final server = _Server(Map.of(files))
      ..putError = 'RUN_COMMAND.txt does not run a test';
    await _pump(tester, server);
    await _open(tester, 'RUN_COMMAND.txt');
    await tester.tap(find.byKey(const ValueKey('oracle-edit-run-command')));
    await tester.pumpAndSettle();
    await tester.tap(find.byKey(const ValueKey('oracle-run-command-save')));
    await tester.pumpAndSettle();

    expect(
      find.text('Could not save: RUN_COMMAND.txt does not run a test'),
      findsOneWidget,
    );
    expect(
      find.byKey(const ValueKey('oracle-run-command-field')),
      findsOneWidget,
    );
  });

  testWidgets('a file collapsed and then rewritten on the server is not '
      'shown again until re-opened: Approve stays disabled after Reload', (
    tester,
  ) async {
    final server = _Server({'RUN_COMMAND.txt': 'go test ./...\n'});
    await _pump(tester, server);
    await _open(tester, 'RUN_COMMAND.txt');
    expect(_approveEnabled(tester), isTrue);

    await _open(tester, 'RUN_COMMAND.txt'); // collapse
    expect(_approveEnabled(tester), isFalse);

    server.files['RUN_COMMAND.txt'] = 'rm -rf /\n';
    await tester.tap(find.byKey(const ValueKey('oracle-reload-button')));
    await tester.pumpAndSettle();
    expect(_approveEnabled(tester), isFalse);
    expect(
      server.requests.where((r) => r.endsWith('/oracle/RUN_COMMAND.txt')),
      hasLength(1),
      reason: 'a collapsed changed file must not be auto-refetched',
    );

    await _open(tester, 'RUN_COMMAND.txt');
    expect(find.text('rm -rf /\n'), findsOneWidget);
    expect(_approveEnabled(tester), isTrue);
    await tester.tap(_approve());
    await tester.pumpAndSettle();
    await tester.tap(find.byKey(const ValueKey('approve-confirm-button')));
    await tester.pumpAndSettle();
    expect(server.approveBody!['expected_sha256'], {
      'oracle/RUN_COMMAND.txt': sha256Hex('rm -rf /\n'),
    });
  });

  testWidgets('invisible and bidi characters render as visible escapes, '
      'empty files get a marker, and the editor previews escapes', (
    tester,
  ) async {
    final server = _Server({
      'RUN_COMMAND.txt': 'go test\u202e ./...\u200b\x01\n',
      'empty_test.go': '',
    });
    await _pump(tester, server);
    await _open(tester, 'RUN_COMMAND.txt');
    expect(
      find.text('go test\\u{202E} ./...\\u{200B}\\u{1}\n'),
      findsOneWidget,
    );
    await _open(tester, 'empty_test.go');
    expect(find.byKey(const ValueKey('oracle-empty-file')), findsOneWidget);
    expect(find.text('(empty file)'), findsOneWidget);

    await tester.tap(find.byKey(const ValueKey('oracle-edit-run-command')));
    await tester.pumpAndSettle();
    expect(
      find.byKey(const ValueKey('oracle-run-command-preview')),
      findsOneWidget,
    );
    await tester.enterText(
      find.byKey(const ValueKey('oracle-run-command-field')),
      'go test ./...',
    );
    await tester.pump();
    expect(
      find.byKey(const ValueKey('oracle-run-command-preview')),
      findsNothing,
    );
  });

  test('escapeInvisible escapes controls, bidi and zero-width only', () {
    expect(escapeInvisible('a\tb\nc'), 'a\tb\nc');
    expect(
      escapeInvisible('\u2066x\u2069\ufeff\u2060'),
      '\\u{2066}x\\u{2069}\\u{FEFF}\\u{2060}',
    );
    expect(escapeInvisible('\r\x7f\u0085'), '\\u{D}\\u{7F}\\u{85}');
    expect(escapeInvisible('héllo 日本'), 'héllo 日本');
  });

  testWidgets('a refused approval shows the server message without the JSON '
      'envelope and re-lists the files', (tester) async {
    final server = _Server({'RUN_COMMAND.txt': 'go test ./...\n'})
      ..approveError =
          'oracle/RUN_COMMAND.txt artifact changed since it was '
          'fetched';
    await _pump(tester, server);
    await _open(tester, 'RUN_COMMAND.txt');
    await tester.tap(_approve());
    await tester.pumpAndSettle();
    await tester.tap(find.byKey(const ValueKey('approve-confirm-button')));
    await tester.pumpAndSettle();

    expect(
      find.textContaining(
        'Could not update request: oracle/RUN_COMMAND.txt artifact changed',
      ),
      findsOneWidget,
    );
    expect(find.textContaining('Run API request failed'), findsNothing);
    final lastApprove = server.requests.lastIndexOf(
      'POST /requests/req-1/approve',
    );
    expect(
      server.requests.skip(lastApprove).contains('GET /requests/req-1/oracle'),
      isTrue,
    );
  });

  testWidgets('every manifest-derived, problem, proposed-command and file '
      'name string shows hidden characters as visible escapes', (tester) async {
    final manifest = jsonEncode([
      {
        'criterion': '1. Cri\u202Eterion',
        'oracle_file': 'a\u200B_test.go',
        'target_path': 'x/\u2066y.go',
        'supersedes': ['old\u061C.go'],
        'criterion_index': 1,
      },
      {
        'criterion': '2. Other',
        'oracle_file': null,
        'rationale': 'why\u200E',
        'criterion_index': 2,
      },
    ]);
    final server =
        _Server({'MANIFEST.json': manifest, 'n\u202Eame_test.go': 'x'})
          ..problems = ['bad\u202Efile']
          ..draft = {
            'status': 'failed',
            'detail': 'det\u200Bail',
            'proposed_command': 'go test\u202E ./...',
          };
    await _pump(tester, server);

    String plain(Finder f) => tester
        .widgetList<SelectableText>(f)
        .map((w) => w.textSpan?.toPlainText() ?? w.data ?? '')
        .join('|');
    final coverage = plain(
      find.descendant(
        of: find.byKey(const ValueKey('oracle-coverage')),
        matching: find.byType(SelectableText),
      ),
    );
    expect(coverage, contains(r'Cri\u{202E}terion'));
    expect(coverage, contains(r'a\u{200B}_test.go'));
    expect(coverage, contains(r'x/\u{2066}y.go'));
    expect(coverage, contains(r'old\u{61C}.go'));
    expect(coverage, contains(r'why\u{200E}'));
    expect(
      plain(
        find.descendant(
          of: find.byKey(const ValueKey('oracle-problems')),
          matching: find.byType(SelectableText),
        ),
      ),
      contains(r'bad\u{202E}file'),
    );
    expect(
      plain(
        find.descendant(
          of: find.byKey(const ValueKey('oracle-proposed-command')),
          matching: find.byType(SelectableText),
        ),
      ),
      contains(r'go test\u{202E} ./...'),
    );
    expect(
      plain(
        find.descendant(
          of: find.byKey(const ValueKey('oracle-draft-status')),
          matching: find.byType(SelectableText),
        ),
      ),
      contains(r'det\u{200B}ail'),
    );
    expect(find.text(r'n\u{202E}ame_test.go'), findsOneWidget);
  });

  testWidgets('a file with invalid UTF-8 shows explicit \\xNN escapes and the '
      'approval carries the hash of the raw bytes', (tester) async {
    final bytes = [0x67, 0x6F, 0x80, 0xFF, 0x0A];
    final server = _Server({})..raw = {'RUN_COMMAND.txt': bytes};
    await _pump(tester, server);
    await _open(tester, 'RUN_COMMAND.txt');

    final box = find.descendant(
      of: find.byKey(const ValueKey('oracle-content-RUN_COMMAND.txt')),
      matching: find.byType(SelectableText),
    );
    expect(
      tester.widget<SelectableText>(box).textSpan!.toPlainText(),
      'go\\x80\\xFF\n',
    );
    expect(find.textContaining('\uFFFD'), findsNothing);
    await tester.tap(_approve());
    await tester.pumpAndSettle();
    await tester.tap(find.byKey(const ValueKey('approve-confirm-button')));
    await tester.pumpAndSettle();
    expect(server.approveBody!['expected_sha256'], {
      'oracle/RUN_COMMAND.txt': sha256HexBytes(bytes),
    });
  });

  testWidgets('a failed Reload keeps the stale listing visible with its error '
      'and disables Approve until a reload succeeds', (tester) async {
    final server = _Server({'RUN_COMMAND.txt': 'go test ./...\n'});
    await _pump(tester, server);
    await _open(tester, 'RUN_COMMAND.txt');
    expect(_approveEnabled(tester), isTrue);

    server.failListing = true;
    await tester.tap(find.byKey(const ValueKey('oracle-reload-button')));
    await tester.pumpAndSettle();
    expect(find.byKey(const ValueKey('oracle-stale-listing')), findsOneWidget);
    expect(find.text('RUN_COMMAND.txt'), findsOneWidget);
    expect(_approveEnabled(tester), isFalse);

    server.failListing = false;
    await tester.tap(find.byKey(const ValueKey('oracle-reload-button')));
    await tester.pumpAndSettle();
    expect(find.byKey(const ValueKey('oracle-stale-listing')), findsNothing);
    expect(_approveEnabled(tester), isTrue);
  });

  testWidgets('without an override token there is no Edit and Approve stays '
      'disabled', (tester) async {
    final server = _Server(Map.of(files));
    await _pump(tester, server, overrideToken: null);
    await _open(tester, 'RUN_COMMAND.txt');
    expect(find.byKey(const ValueKey('oracle-edit-run-command')), findsNothing);
    for (final name in ['a_oracle_test.go', 'MANIFEST.json']) {
      await _open(tester, name);
    }
    expect(_approveEnabled(tester), isFalse);
  });

  testWidgets('shows the drafting status, its detail and the proposed '
      'command, and keeps Request changes available', (tester) async {
    final server = _Server(Map.of(files))
      ..draft = {
        'status': 'failed',
        'detail': 'model returned no manifest',
        'proposed_command': 'go test ./internal/a/...',
      };
    await _pump(tester, server);

    expect(
      find.textContaining(
        'Oracle draft: Drafting failed -- model returned no manifest',
      ),
      findsOneWidget,
    );
    expect(
      find.byKey(const ValueKey('oracle-proposed-command')),
      findsOneWidget,
    );
    expect(find.byKey(const ValueKey('reject-request-button')), findsOneWidget);
  });

  testWidgets('oracle_drafting shows an in-progress notice, and the previous '
      "pass's failure when there was one", (tester) async {
    final server = _Server({}, state: 'oracle_drafting')
      ..draft = {'status': 'failed', 'detail': 'timeout'};
    await _pump(tester, server, settle: false);

    expect(
      find.byKey(const ValueKey('oracle-drafting-section')),
      findsOneWidget,
    );
    expect(
      find.textContaining('Drafting acceptance-test oracles'),
      findsOneWidget,
    );
    expect(
      find.textContaining('Previous pass: Drafting failed -- timeout'),
      findsOneWidget,
    );
    expect(find.byKey(const ValueKey('oracle-review-panel')), findsNothing);
    expect(_approve(), findsNothing);
  });

  test('parseOracleManifest tolerates non-array and non-object input', () {
    expect(parseOracleManifest('not json'), isNull);
    expect(parseOracleManifest('{"a":1}'), isNull);
    expect(parseOracleManifest('[1, {"criterion":"x"}]'), hasLength(1));
  });

  testWidgets('shows a per-criterion eligibility verdict alongside the '
      'drafting status', (tester) async {
    final server = _Server(Map.of(files))
      ..draft = {
        'status': 'none_eligible',
        'detail': 'no criterion was testable',
        'criteria': [
          {
            'number': 1,
            'eligible': false,
            'reason':
                'divide_numbers(a, 0) raises ValueError, not a return value',
          },
          {
            'number': 2,
            'eligible': true,
            'reason': 'pure function, deterministic',
          },
        ],
      };
    await _pump(tester, server);

    expect(find.byKey(const ValueKey('oracle-draft-criteria')), findsOneWidget);
    expect(
      find.textContaining(
        '1. Not eligible -- divide_numbers(a, 0) raises ValueError, not a '
        'return value',
      ),
      findsOneWidget,
    );
    expect(
      find.textContaining('2. Eligible -- pure function, deterministic'),
      findsOneWidget,
    );
  });
}
