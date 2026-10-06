import 'package:flutter/material.dart';

import 'models.dart';
import 'status.dart';

/// One bucket of the ticket fan-out roll-up: how
/// many of a request's tickets share [status], with a short display
/// [label].
class TicketRollupEntry {
  const TicketRollupEntry({
    required this.status,
    required this.label,
    required this.count,
  });

  final Status status;
  final String label;
  final int count;
}

// Fixed display order, most-actionable first -- not ticket-index order or
// an illustrative "3 done · 1 changes requested · 2 building" example
// order, which is presentational rather than a spec: an operator
// scanning this strip for the one thing they need to act on should find
// it first, matching the "visibility first" triage principle.
const _displayOrder = [
  Status.needsHuman,
  Status.working,
  Status.unknown,
  Status.done,
  Status.failed,
];

const Map<Status, String> _labels = {
  Status.needsHuman: 'changes requested',
  Status.working: 'building',
  Status.unknown: 'queued',
  Status.done: 'done',
  Status.failed: 'stopped',
};

/// ticketStatus derives one ticket's own roll-up status from the two
/// fields a RequestSummary's tickets already carry -- no per-ticket
/// GET /runs/{id} fetch. This stays true
/// even after Phase 4 (the follow-along console): the server's `Ticket`
/// struct (internal/request/request.go) still has no `run_state` field for
/// GET /requests or GET /requests/{id} to carry, so there is nothing this
/// function could read without a fetch -- confirmed by reading
/// internal/api/server.go's requestSummaryView/requestTicketView, neither
/// of which has one. Phase 4 instead added a real per-ticket run fetch,
/// but only on the request *detail* screen's own ticket cards
/// (request_detail_screen.dart's `_TicketLinks`), which can afford it for
/// the handful of tickets one open request has; this board-wide roll-up
/// strip (also reused on the detail header) still can't, for the same
/// "not one fetch per ticket on a list" reason ruled out from the start.
///
/// A ticket with a runId but no prState yet counts as [Status.working]
/// ("building") -- an acknowledged approximation, not a real signal read
/// from the ticket's own run: the run could equally be queued, verifying,
/// or itself halted/quarantined, none of which this request-level view
/// can distinguish without that per-ticket fetch. A ticket with neither a
/// runId nor a prState hasn't started yet, and rolls up as
/// [Status.unknown] ("queued") rather than being silently omitted.
///
/// A ticket the request has already moved past (its index below
/// [request]'s current [RequestSummary.ticketIndex]), or the current
/// ticket once the request itself has reached pr_review/done/accepted-
/// awaiting-PR, counts as [Status.done] regardless of runId/prState --
/// found in the operator demo (C7): an accepted ticket with a runId but
/// no prState yet (no PR opened) previously still rolled up as "building"
/// even though the request itself had already moved on.
Status ticketStatus(RequestTicket ticket, RequestSummary request) {
  if (ticket.prState.isNotEmpty && ticket.prUrl.isNotEmpty) {
    return statusForPRState(ticket.prState);
  }
  final pastThisTicket = ticket.index < request.ticketIndex;
  final currentTicketAccepted =
      ticket.index == request.ticketIndex &&
      (request.state == 'pr_review' ||
          request.state == 'done' ||
          request.awaitingPullRequest);
  if (pastThisTicket || currentTicketAccepted) return Status.done;
  // The current ticket of a request that stopped (quarantined, cancelled,
  // or halted for a reason other than a missing PR) is not building.
  final stopped =
      request.state == 'quarantined' ||
      request.state == 'cancelled' ||
      request.state == 'halted';
  if (ticket.index == request.ticketIndex && stopped) return Status.failed;
  if (ticket.runId.isNotEmpty) return Status.working;
  return Status.unknown;
}

/// Groups [tickets] into the roll-up strip's buckets, in [_displayOrder],
/// omitting empty buckets. Returns an empty list for a request with no
/// tickets yet (still in spec_drafting/spec_review/planning).
List<TicketRollupEntry> computeTicketRollup(
  List<RequestTicket> tickets,
  RequestSummary request,
) {
  // Keyed by label, not just status: a ticket whose PR is open or ready
  // shares the working colour with a building one, but is waiting on a
  // reviewer, so it reads "in review" (a request with two accepted tickets
  // and two ready PRs showed "2 building" in the 2026-09-26 run on a Flutter + Go app repo).
  final counts = <String, int>{};
  final statusOf = <String, Status>{};
  for (final ticket in tickets) {
    final status = ticketStatus(ticket, request);
    final hasPR = ticket.prState.isNotEmpty && ticket.prUrl.isNotEmpty;
    final label = switch ((status, hasPR)) {
      (Status.needsHuman, true) when ticket.prState == 'ready' =>
        'awaiting review',
      (Status.needsHuman, true) when ticket.prState == 'stacked' =>
        'waiting on base PR',
      (Status.working, true) => 'in review',
      _ => _labels[status]!,
    };
    counts[label] = (counts[label] ?? 0) + 1;
    statusOf[label] = status;
  }
  return [
    for (final status in _displayOrder)
      for (final label in counts.keys)
        if (statusOf[label] == status)
          TicketRollupEntry(
            status: status,
            label: label,
            count: counts[label]!,
          ),
  ];
}

/// The compact "● 3 done · ● 1 changes requested · ● 2 building" strip
/// itself, shown on both the board row and the
/// detail header for building/pr_review requests. Renders nothing for a
/// request with no tickets yet, so a caller can include it unconditionally
/// without checking `tickets.isEmpty` first.
class TicketRollupStrip extends StatelessWidget {
  const TicketRollupStrip({required this.request, super.key});

  final RequestSummary request;

  @override
  Widget build(BuildContext context) {
    final entries = computeTicketRollup(request.tickets, request);
    if (entries.isEmpty) return const SizedBox.shrink();
    final style = Theme.of(context).textTheme.bodySmall;
    final brightness = Theme.of(context).brightness;
    return Wrap(
      spacing: 12,
      runSpacing: 4,
      crossAxisAlignment: WrapCrossAlignment.center,
      children: [
        for (final entry in entries)
          Row(
            mainAxisSize: MainAxisSize.min,
            children: [
              Icon(
                Icons.circle,
                size: 10,
                color: statusColor(entry.status, brightness: brightness),
              ),
              const SizedBox(width: 4),
              Text('${entry.count} ${entry.label}', style: style),
            ],
          ),
      ],
    );
  }
}
