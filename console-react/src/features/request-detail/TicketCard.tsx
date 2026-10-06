import { CircleAlert, ExternalLink } from "lucide-react";
import { Link } from "react-router";

import type { RequestTicket } from "@/domain/request";
import { runPath } from "@/routes/paths";
import { Button } from "@/ui/Button";
import { Card, CardBody } from "@/ui/Card";
import { Spinner } from "@/ui/Feedback";
import { StatusChipForToken } from "@/ui/StatusChip";
import { StallChip } from "@/ui/Time";

import { TicketPrChip } from "./TicketPrChip";
import { useTicketRun } from "./useTicketRun";

// Agent-supplied text must not become a link to a javascript: or data: URL.
function isWebUrl(value: string): boolean {
  try {
    const url = new URL(value);
    return url.protocol === "https:" || url.protocol === "http:";
  } catch {
    return false;
  }
}

/** One ticket: its own run's real state, PR state and link, and a link to the run. */
export function TicketCard({ ticket }: { readonly ticket: RequestTicket }) {
  const run = useTicketRun(ticket.runId);
  return (
    <Card data-testid={`ticket-card-${ticket.index}`}>
      <CardBody className="flex flex-col gap-2">
        <h3 className="text-sm font-semibold">{`Ticket ${ticket.index}`}</h3>
        <div className="flex flex-wrap items-center gap-2">
          {run.data !== undefined ? (
            <>
              <StatusChipForToken token={run.data.state} />
              <StallChip run={run.data} />
            </>
          ) : run.isFetching && ticket.runId !== "" ? (
            <Spinner label="Loading the ticket's run" />
          ) : run.isError ? (
            <CircleAlert
              aria-label="Could not load the ticket's run"
              className="text-tone-danger size-4"
            />
          ) : null}
          {ticket.prState !== "" && ticket.prUrl !== "" ? (
            <TicketPrChip prState={ticket.prState} />
          ) : null}
        </div>
        {ticket.prUrl === "" ? null : (
          <p className="text-sm break-all">
            PR:{" "}
            {isWebUrl(ticket.prUrl) ? (
              <a
                href={ticket.prUrl}
                target="_blank"
                rel="noopener noreferrer"
                className="text-accent underline underline-offset-2"
              >
                {ticket.prUrl}
              </a>
            ) : (
              <span className="font-mono">{ticket.prUrl}</span>
            )}
          </p>
        )}
        {ticket.runId === "" ? null : (
          <div>
            <Button asChild size="sm">
              <Link to={runPath(ticket.runId)}>
                <ExternalLink aria-hidden="true" />
                View run
              </Link>
            </Button>
          </div>
        )}
      </CardBody>
    </Card>
  );
}
