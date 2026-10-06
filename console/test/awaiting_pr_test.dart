import 'package:console/models.dart';
import 'package:console/request_list_screen.dart';
import 'package:console/status.dart';
import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';

RequestSummary _request(String state, String haltKind) =>
    RequestSummary.fromJson({
      'id': 'req-1',
      'workspace': '/w',
      'project': 'p',
      'state': state,
      'submitted_at': '2026-09-21T00:00:00Z',
      'updated_at': '2026-09-21T00:00:00Z',
      'halt_kind': haltKind,
    });

void main() {
  test('only a halted request with the accepted_no_pr kind awaits a PR', () {
    expect(_request('halted', 'accepted_no_pr').awaitingPullRequest, isTrue);
    expect(_request('halted', '').awaitingPullRequest, isFalse);
    expect(
      _request('halted', 'oracle_materialize').awaitingPullRequest,
      isFalse,
    );
    expect(_request('building', 'accepted_no_pr').awaitingPullRequest, isFalse);
    expect(
      _request('halted', 'accepted_no_pr').awaitingPullRequestLabel,
      contains('factoryd retry req-1'),
    );
  });

  testWidgets('chip shows a calm accepted label, not halted', (tester) async {
    await tester.pumpWidget(
      const MaterialApp(
        home: Scaffold(
          body: RequestStageChip(state: 'halted', awaitingPullRequest: true),
        ),
      ),
    );
    expect(find.text('accepted · awaiting PR'), findsOneWidget);
    expect(find.text('halted'), findsNothing);
  });

  // C5 (operator demo, 2026-09-26): waiting_on/quarantine_check are both
  // optional additions to the request summary/detail JSON -- absent
  // decodes as null, present decodes verbatim.
  test('waitingOn/quarantineCheck are null when absent from the JSON', () {
    final request = RequestSummary.fromJson({
      'id': 'req-1',
      'workspace': '/w',
      'project': 'p',
      'state': 'building',
      'submitted_at': '2026-09-21T00:00:00Z',
      'updated_at': '2026-09-21T00:00:00Z',
    });
    expect(request.waitingOn, isNull);
    expect(request.quarantineCheck, isNull);
  });

  test('waitingOn/quarantineCheck parse verbatim when present', () {
    final request = RequestSummary.fromJson({
      'id': 'req-1',
      'workspace': '/w',
      'project': 'p',
      'state': 'building',
      'submitted_at': '2026-09-21T00:00:00Z',
      'updated_at': '2026-09-21T00:00:00Z',
      'waiting_on': 'req-ahead-1',
      'quarantine_check': 'spec_conformity',
    });
    expect(request.waitingOn, 'req-ahead-1');
    expect(request.quarantineCheck, 'spec_conformity');
  });

  testWidgets(
    'the state chip shows "Queued behind <short id>" instead of Building '
    'when waitingOn is set',
    (tester) async {
      await tester.pumpWidget(
        const MaterialApp(
          home: Scaffold(
            body: RequestStageChip(state: 'building', waitingOn: 'req-ahead'),
          ),
        ),
      );
      expect(find.text('Queued behind req-ahead'), findsOneWidget);
      expect(find.text('Building'), findsNothing);
    },
  );

  testWidgets(
    'the state chip shows plain "Building" when waitingOn is absent',
    (tester) async {
      await tester.pumpWidget(
        const MaterialApp(
          home: Scaffold(body: RequestStageChip(state: 'building')),
        ),
      );
      expect(find.text('Building'), findsOneWidget);
      expect(find.textContaining('Queued behind'), findsNothing);
    },
  );

  testWidgets(
    'a pr_review chip on a row that needs you takes the needs-you status',
    (tester) async {
      await tester.pumpWidget(
        const MaterialApp(
          home: Scaffold(
            body: RequestStageChip(state: 'pr_review', needsYou: true),
          ),
        ),
      );
      final chip = tester.widget<StatusChip>(find.byType(StatusChip));
      expect(chip.status, Status.needsHuman);
    },
  );
}
