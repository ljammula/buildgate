import 'package:console/models.dart';
import 'package:console/status.dart';
import 'package:console/ticket_rollup.dart';
import 'package:flutter_test/flutter_test.dart';

RequestTicket _ticket({
  required int index,
  String runId = '',
  String prState = '',
}) => RequestTicket(
  index: index,
  runId: runId,
  prState: prState,
  prUrl: prState.isEmpty ? '' : 'https://github.com/acme/app/pull/$index',
);

RequestSummary _request({
  String state = 'building',
  int ticketIndex = 1,
  String haltKind = '',
}) => RequestSummary(
  id: 'req-1',
  workspace: 'ws',
  project: 'proj',
  state: state,
  submittedAt: '',
  updatedAt: '',
  ticketIndex: ticketIndex,
  haltKind: haltKind,
);

void main() {
  group('ticketStatus', () {
    test('a ticket with a prState uses the shared status vocabulary', () {
      expect(
        ticketStatus(
          _ticket(index: 1, prState: 'changes_requested'),
          _request(),
        ),
        Status.needsHuman,
      );
      expect(
        ticketStatus(_ticket(index: 1, prState: 'merged'), _request()),
        Status.done,
      );
    });

    test(
      'a ticket with a runId but no prState counts as building (Status.working) -- '
      'an acknowledged approximation, see ticketStatus doc comment',
      () {
        expect(
          ticketStatus(
            _ticket(index: 1, runId: 'run-1'),
            _request(ticketIndex: 1),
          ),
          Status.working,
        );
      },
    );

    test(
      'a ticket with neither a runId nor a prState is unknown ("queued")',
      () {
        expect(
          ticketStatus(_ticket(index: 1), _request(ticketIndex: 1)),
          Status.unknown,
        );
      },
    );

    // C7 (operator demo, 2026-09-26): a ticket the request has already
    // moved past still had a runId and no prState (no PR was ever opened
    // for it), so it read as "1 building" on an otherwise-accepted
    // request. Its index below the request's current ticketIndex is
    // itself the "this one is done" signal, independent of runId/prState.
    test('a ticket below the request current ticket index is done', () {
      expect(
        ticketStatus(
          _ticket(index: 1, runId: 'run-1'),
          _request(ticketIndex: 2),
        ),
        Status.done,
      );
    });

    test('the current ticket is done once the request reaches pr_review', () {
      expect(
        ticketStatus(
          _ticket(index: 2, runId: 'run-2'),
          _request(state: 'pr_review', ticketIndex: 2),
        ),
        Status.done,
      );
    });

    test('the current ticket is done once the request is done', () {
      expect(
        ticketStatus(
          _ticket(index: 2, runId: 'run-2'),
          _request(state: 'done', ticketIndex: 2),
        ),
        Status.done,
      );
    });

    test(
      'the current ticket is done when the request is accepted awaiting its PR',
      () {
        expect(
          ticketStatus(
            _ticket(index: 2, runId: 'run-2'),
            _request(
              state: 'halted',
              haltKind: 'accepted_no_pr',
              ticketIndex: 2,
            ),
          ),
          Status.done,
        );
      },
    );

    test('the current ticket still building stays Status.working', () {
      expect(
        ticketStatus(
          _ticket(index: 2, runId: 'run-2'),
          _request(state: 'building', ticketIndex: 2),
        ),
        Status.working,
      );
    });
  });

  group('computeTicketRollup', () {
    test('an empty ticket list produces no entries', () {
      expect(computeTicketRollup(const [], _request()), isEmpty);
    });

    test('groups tickets by status and counts them, most-actionable first', () {
      final tickets = [
        _ticket(index: 1, prState: 'merged'),
        _ticket(index: 2, prState: 'merged'),
        _ticket(index: 3, prState: 'changes_requested'),
        _ticket(index: 4, runId: 'run-4'),
        _ticket(index: 5, runId: 'run-5'),
      ];

      // ticketIndex 4 (not past ticket 4 or 5 yet, and still building) --
      // keeps both runId-only tickets counted as "building", matching this
      // test's pre-existing expectation.
      final entries = computeTicketRollup(tickets, _request(ticketIndex: 4));

      expect(entries.map((e) => e.label).toList(), [
        'changes requested',
        'building',
        'done',
      ]);
      expect(entries.map((e) => e.count).toList(), [1, 2, 2]);
    });
  });

  test('a PR state with no PR URL (an unopened PR) is not a PR status', () {
    expect(
      ticketStatus(
        const RequestTicket(index: 1, runId: 'run-1', prState: 'draft'),
        _request(ticketIndex: 1),
      ),
      Status.working,
    );
  });

  test('the current ticket of a stopped request counts as stopped, not '
      'building', () {
    for (final state in ['quarantined', 'cancelled', 'halted']) {
      expect(
        ticketStatus(
          _ticket(index: 1, runId: 'run-1'),
          _request(state: state, ticketIndex: 1),
        ),
        Status.failed,
        reason: state,
      );
    }
  });

  test(
    'tickets whose PR is ready roll up as awaiting review (needs you), not building',
    () {
      final entries = computeTicketRollup([
        _ticket(index: 1, runId: 'run-1', prState: 'ready'),
        _ticket(index: 2, runId: 'run-2', prState: 'ready'),
      ], _request(state: 'pr_review', ticketIndex: 2));
      expect(entries.map((e) => '${e.count} ${e.label}'), [
        '2 awaiting review',
      ]);
    },
  );

  test('a stacked PR rolls up as waiting on its base PR', () {
    final entries = computeTicketRollup([
      _ticket(index: 1, runId: 'run-1', prState: 'ready'),
      _ticket(index: 2, runId: 'run-2', prState: 'stacked'),
    ], _request(state: 'pr_review', ticketIndex: 2));
    expect(entries.map((e) => '${e.count} ${e.label}').toSet(), {
      '1 awaiting review',
      '1 waiting on base PR',
    });
  });

  test('an open (not yet ready) PR still rolls up as in review', () {
    final entries = computeTicketRollup([
      _ticket(index: 1, runId: 'run-1', prState: 'open'),
    ], _request(state: 'pr_review', ticketIndex: 1));
    expect(entries.map((e) => '${e.count} ${e.label}'), ['1 in review']);
  });

  test('a building ticket beside a ready PR shows both buckets', () {
    final entries = computeTicketRollup([
      _ticket(index: 1, runId: 'run-1', prState: 'ready'),
      _ticket(index: 2, runId: 'run-2'),
    ], _request(ticketIndex: 2));
    expect(entries.map((e) => '${e.count} ${e.label}').toSet(), {
      '1 awaiting review',
      '1 building',
    });
  });
}
