import 'dart:convert';

import 'package:console/api_client.dart';
import 'package:console/new_run_screen.dart';
import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:http/http.dart' as http;
import 'package:http/testing.dart';

import 'fixtures.dart';

void main() {
  testWidgets('new-run intake posts its fields and opens the run detail', (
    tester,
  ) async {
    await tester.binding.setSurfaceSize(const Size(800, 1200));
    addTearDown(() => tester.binding.setSurfaceSize(null));
    http.Request? submitted;
    final client = MockClient((request) async {
      // First request only (`??=`): once the run detail screen opens, its
      // Timeline section (Phase 4) also issues its own GET
      // /runs/{id}/progress request, which must not overwrite the POST
      // /runs request this test actually asserts on.
      submitted ??= request;
      return http.Response(acceptedRunJson, 202);
    });
    final api = RunApi(
      baseUrl: 'http://factory.test',
      client: client,
      authToken: 'start-token',
    );

    await tester.pumpWidget(MaterialApp(home: NewRunScreen(api: api)));
    await tester.enterText(
      find.byKey(const ValueKey('new-run-ticket')),
      'ticket-new',
    );
    await tester.enterText(
      find.byKey(const ValueKey('new-run-workspace')),
      '/workspaces/ticket-new',
    );
    await tester.enterText(
      find.byKey(const ValueKey('new-run-spec')),
      '/specs/ticket-new.md',
    );
    await tester.enterText(
      find.byKey(const ValueKey('new-run-repository')),
      '/projects/app',
    );
    await tester.enterText(
      find.byKey(const ValueKey('new-run-temporal-address')),
      'localhost:7233',
    );

    await tester.tap(find.byKey(const ValueKey('start-run-button')));
    await tester.pumpAndSettle();

    expect(submitted, isNotNull);
    expect(submitted!.method, 'POST');
    expect(submitted!.url.toString(), 'http://factory.test/runs');
    expect(submitted!.headers['authorization'], 'Bearer start-token');
    expect(jsonDecode(submitted!.body), {
      'ticket': 'ticket-new',
      'workspace': '/workspaces/ticket-new',
      'spec': '/specs/ticket-new.md',
      'repository': '/projects/app',
      'temporal_address': 'localhost:7233',
    });
    expect(find.text('ticket-accepted'), findsOneWidget);
  });

  testWidgets('new-run intake reports required fields without posting', (
    tester,
  ) async {
    await tester.binding.setSurfaceSize(const Size(800, 1200));
    addTearDown(() => tester.binding.setSurfaceSize(null));
    var requests = 0;
    final api = RunApi(
      baseUrl: 'http://factory.test',
      client: MockClient((_) async {
        requests++;
        return http.Response(acceptedRunJson, 202);
      }),
    );

    await tester.pumpWidget(MaterialApp(home: NewRunScreen(api: api)));
    await tester.tap(find.byKey(const ValueKey('start-run-button')));
    await tester.pump();

    expect(requests, 0);
    expect(find.text('Ticket is required'), findsOneWidget);
    expect(find.text('Workspace path is required'), findsOneWidget);
    expect(find.text('Spec path is required'), findsOneWidget);
    expect(find.text('Repository is required'), findsOneWidget);
    expect(find.text('Temporal address is required'), findsOneWidget);
  });

  testWidgets(
    'a multi-part project-bootstrap failure renders as one bullet per check',
    (tester) async {
      // Regression test for a real console live-validation run,
      // 2026-09-08: before RunApiException.messageParts existed, this
      // exact server response rendered as one unparsed, escaped JSON
      // blob -- 'Could not start run: Run API request failed (400):
      // {"error":"project-bootstrap preflight failed...: product_spec_...
      // | program_design_...' -- instead of one legible bullet per
      // failing check.
      await tester.binding.setSurfaceSize(const Size(800, 1200));
      addTearDown(() => tester.binding.setSurfaceSize(null));
      final api = RunApi(
        baseUrl: 'http://factory.test',
        client: MockClient((_) async {
          return http.Response(
            jsonEncode({
              'error':
                  'project-bootstrap preflight failed, run not started: '
                  'product_spec_frozen (spec/spec.md): could not read '
                  'artifact | ticket_structure (spec/tickets/012.md): '
                  'could not read artifact',
            }),
            400,
          );
        }),
        authToken: 'start-token',
      );

      await tester.pumpWidget(MaterialApp(home: NewRunScreen(api: api)));
      await tester.enterText(
        find.byKey(const ValueKey('new-run-ticket')),
        'ticket-new',
      );
      await tester.enterText(
        find.byKey(const ValueKey('new-run-workspace')),
        '/repo/workspace',
      );
      await tester.enterText(
        find.byKey(const ValueKey('new-run-spec')),
        '/repo/spec/spec.md',
      );
      await tester.enterText(
        find.byKey(const ValueKey('new-run-repository')),
        '/projects/app',
      );
      await tester.enterText(
        find.byKey(const ValueKey('new-run-temporal-address')),
        'localhost:7233',
      );

      await tester.tap(find.byKey(const ValueKey('start-run-button')));
      await tester.pumpAndSettle();

      final errorText = tester.widget<Text>(
        find.byKey(const ValueKey('new-run-error')),
      );
      expect(
        errorText.data,
        'Could not start run:\n'
        '• project-bootstrap preflight failed, run not started: '
        'product_spec_frozen (spec/spec.md): could not read artifact\n'
        '• ticket_structure (spec/tickets/012.md): could not read artifact',
      );
    },
  );

  testWidgets(
    '"Check project setup" reports a passing project without starting a run',
    (tester) async {
      await tester.binding.setSurfaceSize(const Size(800, 1200));
      addTearDown(() => tester.binding.setSurfaceSize(null));
      http.Request? checked;
      var startCalls = 0;
      final api = RunApi(
        baseUrl: 'http://factory.test',
        authToken: 'start-token',
        client: MockClient((request) async {
          if (request.url.path == '/projects/check') {
            checked = request;
            return http.Response(
              jsonEncode({
                'passed': true,
                'checks': [
                  {
                    'check': 'product_spec_frozen',
                    'path': '/repo/spec/spec.md',
                    'passed': true,
                  },
                ],
              }),
              200,
            );
          }
          startCalls++;
          return http.Response(acceptedRunJson, 202);
        }),
      );

      await tester.pumpWidget(MaterialApp(home: NewRunScreen(api: api)));
      await tester.enterText(
        find.byKey(const ValueKey('new-run-workspace')),
        '/repo/workspace',
      );
      await tester.enterText(
        find.byKey(const ValueKey('new-run-repository')),
        '/projects/app',
      );

      await tester.tap(find.byKey(const ValueKey('check-project-button')));
      await tester.pumpAndSettle();

      expect(checked, isNotNull);
      expect(checked!.method, 'POST');
      expect(jsonDecode(checked!.body), {
        'workspace': '/repo/workspace',
        'repository': '/projects/app',
      });
      expect(startCalls, 0);
      expect(
        tester
            .widget<Text>(find.byKey(const ValueKey('check-project-summary')))
            .data,
        'Project setup looks ready.',
      );
      expect(
        find.textContaining('product_spec_frozen (/repo/spec/spec.md)'),
        findsOneWidget,
      );
    },
  );

  testWidgets(
    '"Check project setup" reports every failing artifact with its reason',
    (tester) async {
      await tester.binding.setSurfaceSize(const Size(800, 1200));
      addTearDown(() => tester.binding.setSurfaceSize(null));
      final api = RunApi(
        baseUrl: 'http://factory.test',
        authToken: 'start-token',
        client: MockClient((_) async {
          return http.Response(
            jsonEncode({
              'passed': false,
              'checks': [
                {
                  'check': 'product_spec_frozen',
                  'path': '/repo/spec/spec.md',
                  'passed': false,
                  'reasons': ['could not read artifact: no such file'],
                },
              ],
            }),
            200,
          );
        }),
      );

      await tester.pumpWidget(MaterialApp(home: NewRunScreen(api: api)));
      await tester.enterText(
        find.byKey(const ValueKey('new-run-workspace')),
        '/repo/workspace',
      );

      await tester.tap(find.byKey(const ValueKey('check-project-button')));
      await tester.pumpAndSettle();

      expect(
        tester
            .widget<Text>(find.byKey(const ValueKey('check-project-summary')))
            .data,
        'Project setup is not ready yet:',
      );
      expect(
        find.textContaining(
          'product_spec_frozen (/repo/spec/spec.md): '
          'could not read artifact: no such file',
        ),
        findsOneWidget,
      );
    },
  );

  testWidgets(
    '"Check project setup" without a workspace reports the requirement locally, without a request',
    (tester) async {
      await tester.binding.setSurfaceSize(const Size(800, 1200));
      addTearDown(() => tester.binding.setSurfaceSize(null));
      var requests = 0;
      final api = RunApi(
        baseUrl: 'http://factory.test',
        authToken: 'start-token',
        client: MockClient((_) async {
          requests++;
          return http.Response(acceptedRunJson, 202);
        }),
      );

      await tester.pumpWidget(MaterialApp(home: NewRunScreen(api: api)));
      await tester.tap(find.byKey(const ValueKey('check-project-button')));
      await tester.pump();

      expect(requests, 0);
      expect(find.byKey(const ValueKey('check-project-error')), findsOneWidget);
      expect(find.text('Something went wrong'), findsOneWidget);
      await tester.tap(find.text('Details'));
      await tester.pumpAndSettle();
      expect(
        find.text('Workspace path is required to check a project'),
        findsOneWidget,
      );
    },
  );
}
