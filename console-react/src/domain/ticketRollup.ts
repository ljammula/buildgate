import {
  type RequestSummary,
  type RequestTicket,
  requestAwaitingPullRequest,
} from "@/domain/request";
import { type Status, statusForPRState } from "@/domain/status";

/**
 * One bucket of the ticket fan-out roll-up: how many of a request's tickets
 * share `status`, with a short display `label`.
 */
export interface TicketRollupEntry {
  readonly status: Status;
  readonly label: string;
  readonly count: number;
}

// Fixed display order, most-actionable first -- not ticket-index order or
// an illustrative "3 done · 1 changes requested · 2 building" example
// order, which is presentational rather than a spec: an operator
// scanning this strip for the one thing they need to act on should find
// it first, matching the "visibility first" triage principle.
const DISPLAY_ORDER: readonly Status[] = ["needsHuman", "working", "unknown", "done", "failed"];

const LABELS: Readonly<Record<Status, string>> = {
  needsHuman: "changes requested",
  working: "building",
  unknown: "queued",
  done: "done",
  failed: "stopped",
};

/**
 * ticketStatus derives one ticket's own roll-up status from the two
 * fields a RequestSummary's tickets already carry -- no per-ticket
 * GET /runs/{id} fetch. The server's `Ticket` struct
 * (internal/request/request.go) has no `run_state` field for GET /requests
 * or GET /requests/{id} to carry, so there is nothing this function could
 * read without a fetch. The request detail screen's own ticket cards can
 * afford a real per-ticket run fetch for the handful of tickets one open
 * request has; this board-wide roll-up strip (also reused on the detail
 * header) cannot, for the "not one fetch per ticket on a list" reason.
 *
 * A ticket with a runId but no prState yet counts as "working"
 * ("building") -- an acknowledged approximation, not a real signal read
 * from the ticket's own run: the run could equally be queued, verifying,
 * or itself halted/quarantined, none of which this request-level view
 * can distinguish without that per-ticket fetch. A ticket with neither a
 * runId nor a prState hasn't started yet, and rolls up as
 * "unknown" ("queued") rather than being silently omitted.
 *
 * A ticket the request has already moved past (its index below
 * the request's current ticketIndex), or the current
 * ticket once the request itself has reached pr_review/done/accepted-
 * awaiting-PR, counts as "done" regardless of runId/prState --
 * found in the operator demo (C7): an accepted ticket with a runId but
 * no prState yet (no PR opened) previously still rolled up as "building"
 * even though the request itself had already moved on.
 */
export function ticketStatus(ticket: RequestTicket, request: RequestSummary): Status {
  if (ticket.prState !== "" && ticket.prUrl !== "") {
    return statusForPRState(ticket.prState);
  }
  const pastThisTicket = ticket.index < request.ticketIndex;
  const currentTicketAccepted =
    ticket.index === request.ticketIndex &&
    (request.state === "pr_review" ||
      request.state === "done" ||
      requestAwaitingPullRequest(request));
  if (pastThisTicket || currentTicketAccepted) return "done";
  // The current ticket of a request that stopped (quarantined, cancelled,
  // or halted for a reason other than a missing PR) is not building.
  const stopped =
    request.state === "quarantined" || request.state === "cancelled" || request.state === "halted";
  if (ticket.index === request.ticketIndex && stopped) return "failed";
  if (ticket.runId !== "") return "working";
  return "unknown";
}

/**
 * Groups `tickets` into the roll-up strip's buckets, in display order,
 * omitting empty buckets. Returns an empty list for a request with no
 * tickets yet (still in spec_drafting/spec_review/planning).
 */
export function computeTicketRollup(
  tickets: readonly RequestTicket[],
  request: RequestSummary,
): TicketRollupEntry[] {
  // Keyed by label, not just status: a ticket whose PR is open or ready
  // shares the working colour with a building one, but is waiting on a
  // reviewer, so it reads "in review" (a request with two accepted tickets
  // and two ready PRs showed "2 building" in the 2026-09-26 run on a Flutter + Go app repo).
  const counts = new Map<string, number>();
  const statusOf = new Map<string, Status>();
  for (const ticket of tickets) {
    const status = ticketStatus(ticket, request);
    const hasPR = ticket.prState !== "" && ticket.prUrl !== "";
    let label = LABELS[status];
    if (status === "needsHuman" && hasPR && ticket.prState === "ready") {
      label = "awaiting review";
    } else if (status === "needsHuman" && hasPR && ticket.prState === "stacked") {
      label = "waiting on base PR";
    } else if (status === "working" && hasPR) {
      label = "in review";
    }
    counts.set(label, (counts.get(label) ?? 0) + 1);
    statusOf.set(label, status);
  }
  const entries: TicketRollupEntry[] = [];
  for (const status of DISPLAY_ORDER) {
    for (const [label, count] of counts) {
      if (statusOf.get(label) === status) entries.push({ status, label, count });
    }
  }
  return entries;
}
