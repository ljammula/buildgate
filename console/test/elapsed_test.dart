import 'package:console/elapsed.dart';
import 'package:console/models.dart';
import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';

// runAt builds a minimal non-required-field Run for stallStatus/StallChip
// tests -- only the fields those two care about vary per case. stallStatus
// now trusts the server's own Run.stalled verdict directly (see
// elapsed.dart's doc comment), so these tests set that field rather than
// deriving it from timestamps.
Run runAt({
  String state = 'slice_running',
  bool haltConfirmed = false,
  String createdAt = '2026-09-18T11:00:00Z',
  String? waitingReason,
  bool stalled = false,
}) => Run(
  id: 'run-1',
  ticket: 'ticket-1',
  projectPath: '/projects/app',
  workspacePath: '/workspaces/run-1',
  specPath: '/specs/ticket-1.md',
  specSha256: 'spec-1',
  state: state,
  haltConfirmed: haltConfirmed,
  baseSha: 'base-1',
  resultSha: null,
  committedByFactoryd: false,
  changedFiles: null,
  diffStat: null,
  agentEvidence: null,
  diffAvailable: false,
  diffTruncated: false,
  attempts: const [],
  gateResults: const [],
  notifications: const [],
  overrides: const [],
  createdAt: createdAt,
  updatedAt: createdAt,
  waitingReason: waitingReason,
  stalled: stalled,
);

void main() {
  group('stallStatus', () {
    test('returns stalled when the server reports Run.stalled', () {
      final run = runAt(stalled: true);
      expect(stallStatus(run), 'stalled');
    });

    test('returns null when the server reports not stalled', () {
      final run = runAt(stalled: false);
      expect(stallStatus(run), isNull);
    });

    test('a terminal run is never stalled, no matter what the server reports '
        '(the server itself never sets it true for a terminal run)', () {
      final run = runAt(state: 'accepted', stalled: false);
      expect(stallStatus(run), isNull);
    });
  });

  group('StallChip', () {
    testWidgets('renders the stalled chip when Run.stalled is true', (
      tester,
    ) async {
      final run = runAt(stalled: true);
      await tester.pumpWidget(
        MaterialApp(
          home: Scaffold(body: StallChip(run: run)),
        ),
      );
      expect(find.byKey(const ValueKey('stalled-chip')), findsOneWidget);
      expect(find.text('stalled'), findsOneWidget);
      expect(find.byKey(const ValueKey('waiting-chip')), findsNothing);
    });

    testWidgets(
      'renders the waiting chip, not stalled, when waitingReason is set '
      'and Run.stalled is false',
      (tester) async {
        final run = runAt(
          waitingReason: 'behind 1 run(s) on foo/bar',
          stalled: false,
        );
        await tester.pumpWidget(
          MaterialApp(
            home: Scaffold(body: StallChip(run: run)),
          ),
        );
        expect(find.byKey(const ValueKey('waiting-chip')), findsOneWidget);
        expect(
          find.text('waiting: behind 1 run(s) on foo/bar'),
          findsOneWidget,
        );
        expect(find.byKey(const ValueKey('stalled-chip')), findsNothing);
      },
    );

    testWidgets('renders nothing when neither condition applies', (
      tester,
    ) async {
      final run = runAt();
      await tester.pumpWidget(
        MaterialApp(
          home: Scaffold(body: StallChip(run: run)),
        ),
      );
      expect(find.byKey(const ValueKey('stalled-chip')), findsNothing);
      expect(find.byKey(const ValueKey('waiting-chip')), findsNothing);
    });

    testWidgets('stalled takes priority over a stale waiting reason', (
      tester,
    ) async {
      final run = runAt(
        stalled: true,
        waitingReason: 'behind 1 run(s) on foo/bar',
      );
      await tester.pumpWidget(
        MaterialApp(
          home: Scaffold(body: StallChip(run: run)),
        ),
      );
      expect(find.byKey(const ValueKey('stalled-chip')), findsOneWidget);
      expect(find.byKey(const ValueKey('waiting-chip')), findsNothing);
    });
  });

  group('Run.fromJson progress fields', () {
    test('parses all seven when present', () {
      final run = Run.fromJson({
        'id': 'run-1',
        'ticket': 'ticket-1',
        'project_path': '/projects/app',
        'workspace_path': '/workspaces/run-1',
        'spec_path': '/specs/ticket-1.md',
        'spec_sha256': 'spec-1',
        'state': 'slice_running',
        'base_sha': 'base-1',
        'committed_by_factoryd': false,
        'attempts': [],
        'gate_results': [],
        'notifications': [],
        'overrides': [],
        'created_at': '2026-09-18T11:00:00Z',
        'updated_at': '2026-09-18T11:00:00Z',
        'last_progress_at': '2026-09-18T11:05:00.000Z',
        'current_stage': 'verify',
        'current_round': 2,
        'max_rounds': 6,
        'waiting_reason': 'behind 1 run(s) on foo/bar',
        'stalled': true,
        'stalled_since_seconds': 360,
      });
      expect(run.lastProgressAt, '2026-09-18T11:05:00.000Z');
      expect(run.currentStage, 'verify');
      expect(run.currentRound, 2);
      expect(run.maxRounds, 6);
      expect(run.waitingReason, 'behind 1 run(s) on foo/bar');
      expect(run.stalled, isTrue);
      expect(run.stalledSinceSeconds, 360);
    });

    test('tolerates all seven absent (a terminal run, or an old server)', () {
      final run = Run.fromJson({
        'id': 'run-1',
        'ticket': 'ticket-1',
        'project_path': '/projects/app',
        'workspace_path': '/workspaces/run-1',
        'spec_path': '/specs/ticket-1.md',
        'spec_sha256': 'spec-1',
        'state': 'accepted',
        'base_sha': 'base-1',
        'committed_by_factoryd': false,
        'attempts': [],
        'gate_results': [],
        'notifications': [],
        'overrides': [],
        'created_at': '2026-09-18T11:00:00Z',
        'updated_at': '2026-09-18T11:04:00Z',
      });
      expect(run.lastProgressAt, isNull);
      expect(run.currentStage, isNull);
      expect(run.currentRound, 0);
      expect(run.maxRounds, 0);
      expect(run.waitingReason, isNull);
      expect(run.stalled, isFalse);
      expect(run.stalledSinceSeconds, isNull);
    });

    // C6 (operator demo, 2026-09-26): reference_oracle_dir tells the
    // console whether this run has an oracle stage to report on.
    test('parses reference_oracle_dir when present', () {
      final run = Run.fromJson({
        'id': 'run-1',
        'ticket': 'ticket-1',
        'project_path': '/projects/app',
        'workspace_path': '/workspaces/run-1',
        'spec_path': '/specs/ticket-1.md',
        'spec_sha256': 'spec-1',
        'state': 'accepted',
        'base_sha': 'base-1',
        'committed_by_factoryd': false,
        'attempts': [],
        'gate_results': [],
        'notifications': [],
        'overrides': [],
        'created_at': '2026-09-18T11:00:00Z',
        'updated_at': '2026-09-18T11:04:00Z',
        'reference_oracle_dir': '/data/requests/req-1/oracle',
      });
      expect(run.referenceOracleDir, '/data/requests/req-1/oracle');
    });

    test('reference_oracle_dir defaults to empty when absent', () {
      final run = Run.fromJson({
        'id': 'run-1',
        'ticket': 'ticket-1',
        'project_path': '/projects/app',
        'workspace_path': '/workspaces/run-1',
        'spec_path': '/specs/ticket-1.md',
        'spec_sha256': 'spec-1',
        'state': 'accepted',
        'base_sha': 'base-1',
        'committed_by_factoryd': false,
        'attempts': [],
        'gate_results': [],
        'notifications': [],
        'overrides': [],
        'created_at': '2026-09-18T11:00:00Z',
        'updated_at': '2026-09-18T11:04:00Z',
      });
      expect(run.referenceOracleDir, '');
    });
  });

  // C4 (operator demo, 2026-09-26): every raw-UTC timestamp site now
  // routes through this one shared formatter rather than each screen's
  // own copy.
  group('formatLocalTimestamp', () {
    test('renders an RFC3339 UTC timestamp in local time', () {
      const value = '2026-09-18T11:00:00Z';
      final expected = DateTime.parse(value).toLocal();
      String two(int n) => n.toString().padLeft(2, '0');
      final want =
          '${expected.year}-${two(expected.month)}-${two(expected.day)} '
          '${two(expected.hour)}:${two(expected.minute)}:${two(expected.second)}';
      expect(formatLocalTimestamp(value), want);
    });

    test('falls back to the raw value when unparseable', () {
      expect(formatLocalTimestamp('not-a-date'), 'not-a-date');
    });

    test('falls back to the raw (empty) value when empty', () {
      expect(formatLocalTimestamp(''), '');
    });
  });

  group('LocalTimeText', () {
    testWidgets('shows the local time with the UTC value in a tooltip', (
      tester,
    ) async {
      const value = '2026-09-18T11:00:00Z';
      await tester.pumpWidget(
        const MaterialApp(home: Scaffold(body: LocalTimeText(value))),
      );
      expect(find.text(formatLocalTimestamp(value)), findsOneWidget);
      final tooltip = tester.widget<Tooltip>(find.byType(Tooltip));
      expect(tooltip.message, 'UTC: 2026-09-18T11:00:00.000Z');
    });

    testWidgets('falls back to the raw value when unparseable', (tester) async {
      await tester.pumpWidget(
        const MaterialApp(home: Scaffold(body: LocalTimeText('not-a-date'))),
      );
      expect(find.text('not-a-date'), findsOneWidget);
      expect(find.byType(Tooltip), findsNothing);
    });
  });
}
