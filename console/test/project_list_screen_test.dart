import 'dart:convert';

import 'package:console/api_client.dart';
import 'package:console/new_run_screen.dart';
import 'package:console/project_list_screen.dart';
import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:http/http.dart' as http;
import 'package:http/testing.dart';

void main() {
  testWidgets('project list renders known projects and prefills a run', (
    tester,
  ) async {
    final client = MockClient((request) async {
      if (request.url.path == '/projects') {
        return http.Response(
          jsonEncode([
            {
              'project_path': '/workspaces/app',
              'project': 'app',
              'workspace_path': '/workspaces/app',
              'spec_path': '/specs/app-latest.md',
              'repository': 'app-repo',
              'run_count': 3,
              'last_run_at': '2026-08-26T12:00:00Z',
            },
          ]),
          200,
        );
      }
      return http.Response('not found', 404);
    });
    final api = RunApi(baseUrl: 'http://factory.test', client: client);

    await tester.pumpWidget(MaterialApp(home: ProjectListScreen(api: api)));
    await tester.pumpAndSettle();

    expect(find.text('/workspaces/app'), findsOneWidget);
    expect(find.textContaining('kill-switch id: app'), findsOneWidget);
    expect(find.textContaining('3 runs'), findsOneWidget);

    await tester.tap(find.byKey(const ValueKey('project-/workspaces/app')));
    await tester.pumpAndSettle();

    expect(find.byType(NewRunScreen), findsOneWidget);
    final workspaceField = tester.widget<TextFormField>(
      find.byKey(const ValueKey('new-run-workspace')),
    );
    expect(workspaceField.controller!.text, '/workspaces/app');
    final specField = tester.widget<TextFormField>(
      find.byKey(const ValueKey('new-run-spec')),
    );
    expect(specField.controller!.text, '/specs/app-latest.md');
    final repositoryField = tester.widget<TextFormField>(
      find.byKey(const ValueKey('new-run-repository')),
    );
    expect(repositoryField.controller!.text, 'app-repo');
  });

  testWidgets('empty project list still offers a custom run', (tester) async {
    final client = MockClient((request) async {
      return http.Response(jsonEncode(<dynamic>[]), 200);
    });
    final api = RunApi(baseUrl: 'http://factory.test', client: client);

    await tester.pumpWidget(MaterialApp(home: ProjectListScreen(api: api)));
    await tester.pumpAndSettle();

    expect(find.textContaining('No projects yet'), findsOneWidget);

    await tester.tap(find.byKey(const ValueKey('custom-run-button')));
    await tester.pumpAndSettle();

    expect(find.byType(NewRunScreen), findsOneWidget);
  });
}
