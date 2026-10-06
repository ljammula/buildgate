import 'dart:convert';

import 'package:console/api_client.dart';
import 'package:console/new_request_screen.dart';
import 'package:console/operator_identity.dart';
import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:http/http.dart' as http;
import 'package:http/testing.dart';

import 'fixtures.dart';

void main() {
  setUp(() => setOperatorName('operator'));
  tearDown(clearOperatorNameForTest);

  testWidgets(
    'new-request intake posts its fields and reports the created id',
    (tester) async {
      await tester.binding.setSurfaceSize(const Size(800, 1400));
      addTearDown(() => tester.binding.setSurfaceSize(null));
      http.Request? submitted;
      String? createdId;
      final client = MockClient((request) async {
        if (request.url.path == '/workspaces') {
          return http.Response(jsonEncode([]), 200);
        }
        submitted = request;
        return http.Response(
          requestJson(id: 'req-new', state: 'submitted', title: 'do the thing'),
          201,
        );
      });
      final api = RunApi(
        baseUrl: 'http://factory.test',
        client: client,
        overrideToken: 'override-token',
      );

      await tester.pumpWidget(
        MaterialApp(
          home: NewRequestScreen(api: api, onCreated: (id) => createdId = id),
        ),
      );
      await tester.pumpAndSettle();

      await tester.enterText(
        find.byKey(const ValueKey('new-request-workspace')),
        '/repos/app',
      );
      await tester.enterText(
        find.byKey(const ValueKey('new-request-text')),
        'Add idempotency keys to POST /refunds',
      );
      await tester.tap(find.byKey(const ValueKey('new-request-draft-oracles')));
      await tester.tap(find.byKey(const ValueKey('new-request-advanced')));
      await tester.pumpAndSettle();
      await tester.enterText(
        find.byKey(const ValueKey('new-request-verify-command')),
        'make ci-verify',
      );
      await tester.enterText(
        find.byKey(const ValueKey('new-request-preflight-profile')),
        'brownfield',
      );

      await tester.tap(find.byKey(const ValueKey('submit-request-button')));
      await tester.pumpAndSettle();

      expect(submitted, isNotNull);
      expect(submitted!.method, 'POST');
      expect(submitted!.url.toString(), 'http://factory.test/requests');
      expect(submitted!.headers['authorization'], 'Bearer override-token');
      expect(jsonDecode(submitted!.body), {
        'workspace': '/repos/app',
        'text': 'Add idempotency keys to POST /refunds',
        'verify_command': 'make ci-verify',
        'preflight_profile': 'brownfield',
        'draft_oracles': true,
        'by': 'operator',
      });
      expect(createdId, 'req-new');
    },
  );

  testWidgets('new-request intake reports required fields without posting', (
    tester,
  ) async {
    await tester.binding.setSurfaceSize(const Size(800, 1400));
    addTearDown(() => tester.binding.setSurfaceSize(null));
    var requests = 0;
    final api = RunApi(
      baseUrl: 'http://factory.test',
      overrideToken: 'override-token',
      client: MockClient((request) async {
        if (request.url.path == '/workspaces') {
          return http.Response(jsonEncode([]), 200);
        }
        requests++;
        return http.Response(requestJson(id: 'x', state: 'submitted'), 201);
      }),
    );

    await tester.pumpWidget(MaterialApp(home: NewRequestScreen(api: api)));
    await tester.pumpAndSettle();
    await tester.tap(find.byKey(const ValueKey('submit-request-button')));
    await tester.pump();

    expect(requests, 0);
    expect(find.text('Workspace path is required'), findsOneWidget);
    expect(find.text('Request is required'), findsOneWidget);
  });

  testWidgets(
    'the workspace dropdown fills the workspace field and shows its verify-command hint',
    (tester) async {
      await tester.binding.setSurfaceSize(const Size(800, 1400));
      addTearDown(() => tester.binding.setSurfaceSize(null));
      final api = RunApi(
        baseUrl: 'http://factory.test',
        overrideToken: 'override-token',
        client: MockClient((request) async {
          if (request.url.path == '/workspaces') {
            return http.Response(
              jsonEncode([
                {
                  'workspace': '/repos/app',
                  'has_factory_yml': true,
                  'resolved_verify_command': 'make ci-verify',
                  'verify_command_source': '.factory.yml',
                },
              ]),
              200,
            );
          }
          return http.Response(requestJson(id: 'x', state: 'submitted'), 201);
        }),
      );

      await tester.pumpWidget(MaterialApp(home: NewRequestScreen(api: api)));
      await tester.pumpAndSettle();

      await tester.tap(
        find.byKey(const ValueKey('new-request-workspace-dropdown')),
      );
      await tester.pumpAndSettle();
      await tester.tap(find.text('/repos/app').last);
      await tester.pumpAndSettle();

      expect(
        tester
            .widget<TextFormField>(
              find.byKey(const ValueKey('new-request-workspace')),
            )
            .controller!
            .text,
        '/repos/app',
      );
      expect(
        find.textContaining('Verify command from .factory.yml: make ci-verify'),
        findsOneWidget,
      );
    },
  );

  testWidgets(
    'submit is disabled and explained when the console cannot write',
    (tester) async {
      await tester.binding.setSurfaceSize(const Size(800, 1400));
      addTearDown(() => tester.binding.setSurfaceSize(null));
      final api = RunApi(
        baseUrl: 'http://factory.test',
        client: MockClient((request) async {
          return http.Response(jsonEncode([]), 200);
        }),
      );

      await tester.pumpWidget(MaterialApp(home: NewRequestScreen(api: api)));
      await tester.pumpAndSettle();

      final button = tester.widget<FilledButton>(
        find.byKey(const ValueKey('submit-request-button')),
      );
      expect(button.onPressed, isNull);
      expect(
        find.byKey(const ValueKey('new-request-writes-disabled')),
        findsOneWidget,
      );
    },
  );
}
