import 'package:console/status.dart';
import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';

void main() {
  // Table test over every state/PR-review string literal that used to be
  // hand-colored inside one of the five pre-status.dart chip
  // implementations (RequestStageChip/PrStateChip in
  // request_list_screen.dart, StateBadge in run_list_screen.dart, the
  // release-decision chip in release_screen.dart, and the kill-switch
  // chips). Every literal here must map to the
  // exact semantic status _statusByToken assigns it, so a
  // future edit to _statusByToken that silently drifts one of these
  // strings is caught here rather than discovered as a wrong-colored chip
  // in the running console.
  group('statusForToken maps every known chip literal', () {
    const expected = {
      // Request state (request_list_screen.dart's RequestStageChip).
      'spec_review': Status.needsHuman,
      'oracle_review': Status.needsHuman,
      'plan_review': Status.needsHuman,
      'spec_drafting': Status.working,
      'oracle_drafting': Status.working,
      'planning': Status.working,
      'building': Status.working,
      'pr_review': Status.working,
      'done': Status.done,
      // Terminal request states.
      'quarantined': Status.failed,
      'halted': Status.needsHuman,
      'resume_review': Status.needsHuman,
      'cancelled': Status.failed,
      // PR review state (request_list_screen.dart's PrStateChip).
      'approved': Status.done,
      'changes_requested': Status.needsHuman,
      'merged': Status.done,
      'closed': Status.failed,
      'open': Status.working,
      'draft': Status.working,
      // Run state (run_list_screen.dart's StateBadge).
      'accepted': Status.done,
      'slice_running': Status.working,
    };

    for (final entry in expected.entries) {
      test('${entry.key} -> ${entry.value}', () {
        expect(statusForToken(entry.key), entry.value);
      });
    }

    test('an unmapped/unrecognized string is Status.unknown, never hidden', () {
      expect(
        statusForToken('some_future_state_nobody_wrote_a_case_for'),
        Status.unknown,
      );
    });

    test("'submitted'/'ready'/'verifying' map to working (no longer "
        'unmapped/raw "?")', () {
      expect(statusForToken('submitted'), Status.working);
      expect(statusForToken('ready'), Status.working);
      expect(statusForToken('verifying'), Status.working);
    });
  });

  test('a release decision maps allowed/denied to done/failed', () {
    expect(statusForReleaseDecision(allowed: true), Status.done);
    expect(statusForReleaseDecision(allowed: false), Status.failed);
  });

  group('KillSwitchChip', () {
    // Ports the PR #64 regression test to the new shared widget: a
    // fetch-failed/not-yet-loaded kill switch (engaged == null) must
    // never render as "clear". Collapsing "unknown" into "clear" is the
    // exact bug this widget exists to make structurally impossible.
    testWidgets('never renders "clear" from a null (unknown) input', (
      tester,
    ) async {
      await tester.pumpWidget(
        const MaterialApp(home: Scaffold(body: KillSwitchChip(engaged: null))),
      );

      expect(find.text('Kill switch unknown'), findsOneWidget);
      expect(find.text('Kill switch clear'), findsNothing);
      expect(find.text('Kill switch engaged'), findsNothing);
    });

    testWidgets('renders engaged distinctly from clear', (tester) async {
      await tester.pumpWidget(
        const MaterialApp(home: Scaffold(body: KillSwitchChip(engaged: true))),
      );
      expect(find.text('Kill switch engaged'), findsOneWidget);

      await tester.pumpWidget(
        const MaterialApp(home: Scaffold(body: KillSwitchChip(engaged: false))),
      );
      expect(find.text('Kill switch clear'), findsOneWidget);
      expect(find.text('Kill switch engaged'), findsNothing);
    });

    // The "unknown" outlined chip's grey must still
    // read against a dark surface, not just a light one -- see
    // statusColor's own doc comment for why only this variant (transparent
    // background, text/border color drawn directly against the
    // surrounding surface) needs a brightness-specific shade at all.
    testWidgets('renders a different, still-visible grey in dark mode', (
      tester,
    ) async {
      await tester.pumpWidget(
        MaterialApp(
          theme: ThemeData(brightness: Brightness.dark),
          home: const Scaffold(body: KillSwitchChip(engaged: null)),
        ),
      );

      final text = tester.widget<Text>(find.text('Kill switch unknown'));
      expect(text.style?.color, Colors.grey.shade400);
    });
  });

  test('statusColor(Status.unknown) picks a distinct shade per brightness', () {
    final light = statusColor(Status.unknown, brightness: Brightness.light);
    final dark = statusColor(Status.unknown, brightness: Brightness.dark);
    expect(light, isNot(dark));
    expect(light, Colors.grey.shade700);
    expect(dark, Colors.grey.shade400);
  });

  test('the four filled statuses are unaffected by brightness -- only the '
      'outlined "unknown" variant needs a dark-mode shade', () {
    for (final status in [
      Status.needsHuman,
      Status.working,
      Status.done,
      Status.failed,
    ]) {
      expect(
        statusColor(status, brightness: Brightness.light),
        statusColor(status, brightness: Brightness.dark),
      );
    }
  });

  testWidgets('StatusChip shows the operator word, raw token as tooltip', (
    tester,
  ) async {
    await tester.pumpWidget(
      const MaterialApp(
        home: Scaffold(
          body: StatusChip(status: Status.needsHuman, label: 'spec_review'),
        ),
      ),
    );
    expect(find.text('Spec review'), findsOneWidget);
    expect(find.text('spec_review'), findsNothing);
    expect(find.byTooltip('spec_review'), findsOneWidget);
  });

  test('stateLabel passes composed or unknown labels through unchanged', () {
    expect(stateLabel('slice_running'), 'Building');
    expect(stateLabel('accepted · awaiting PR'), 'accepted · awaiting PR');
    expect(stateLabel('never_seen'), 'never_seen');
  });
}
