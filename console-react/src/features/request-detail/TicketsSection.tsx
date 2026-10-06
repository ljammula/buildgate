import type { RequestSummary } from "@/domain/request";

import { Panel } from "./Panel";
import { TicketCard } from "./TicketCard";

/** Every ticket of the request: its run's state, PR and links. */
export function TicketsSection({ request }: { readonly request: RequestSummary }) {
  return (
    <Panel title="Tickets" testId="tickets-section">
      {request.tickets.map((ticket) => (
        // Keyed by index: a retry landing a new runId under the same ticket
        // must update this card, not be matched to a neighbour's.
        <TicketCard key={ticket.index} ticket={ticket} />
      ))}
    </Panel>
  );
}
