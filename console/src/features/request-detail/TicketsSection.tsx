import type { RequestSummary } from "@/domain/request";

import { Panel } from "./Panel";
import { TicketCard } from "./TicketCard";

/**
 * The tickets that have something to show: a run, or a pull request. A ticket
 * that is only a plan (before it builds) has nothing here but its own name,
 * and its plan is the card above; an empty card is noise at plan review.
 */
export function TicketsSection({ request }: { readonly request: RequestSummary }) {
  const tickets = request.tickets.filter((t) => t.runId !== "" || t.prUrl !== "");
  if (tickets.length === 0) return null;
  return (
    <Panel title="Tickets" testId="tickets-section">
      {tickets.map((ticket) => (
        // Keyed by index: a retry landing a new runId under the same ticket
        // must update this card, not be matched to a neighbour's.
        <TicketCard key={ticket.index} ticket={ticket} live={request.state === "building"} />
      ))}
    </Panel>
  );
}
