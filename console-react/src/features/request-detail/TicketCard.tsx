import { CircleAlert, ExternalLink } from "lucide-react";
import { Link } from "react-router";

import { useRunRecord } from "@/api/runQueries";
import type { RequestTicket } from "@/domain/request";
import { type Run, runIsTerminalForDisplay } from "@/domain/run";
import { safeHttpUrl } from "@/domain/safeUrl";
import { runPath } from "@/routes/paths";
import { PrStateChip } from "@/shared/request/PrStateChip";
import { Button } from "@/ui/Button";
import { Card, CardBody } from "@/ui/Card";
import { Spinner } from "@/ui/Feedback";
import { RelativeTime } from "@/ui/RelativeTime";
import { StatusChipForToken } from "@/ui/StatusChip";
import { StallChip } from "@/ui/Time";

const runPollMs = 10_000;

function RunActivity({ run }: { readonly run: Run }) {
  const parts = [
    run.currentStage === null ? "" : `Now: ${run.currentStage}`,
    run.currentRound > 0
      ? `round ${run.currentRound}${run.maxRounds > 0 ? `/${run.maxRounds}` : ""}`
      : "",
  ].filter((p) => p !== "");
  if (parts.length === 0 && run.lastProgressAt === null) return null;
  return (
    <p data-testid="ticket-activity" className="text-fg-muted text-xs">
      {parts.join(" · ")}
      {run.lastProgressAt === null ? null : (
        <>
          {parts.length > 0 ? " · " : ""}last activity <RelativeTime value={run.lastProgressAt} />
        </>
      )}
    </p>
  );
}

/** One ticket: its own run's real state, PR state and link, and a link to the run. */
export function TicketCard({
  ticket,
  live,
}: {
  readonly ticket: RequestTicket;
  readonly live: boolean;
}) {
  // The ticket's own run: the server's ticket JSON carries no run state. Keyed
  // by run id, so a retry landing a new run under the same ticket fetches the
  // new run. While the request builds, its run moves without the request
  // changing, so it is polled.
  const run = useRunRecord(ticket.runId, {
    enabled: ticket.runId !== "",
    ...(live ? { refetchIntervalMs: runPollMs } : {}),
  });
  const prHref = safeHttpUrl(ticket.prUrl);
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
            <PrStateChip prState={ticket.prState} />
          ) : null}
        </div>
        {run.data === undefined || runIsTerminalForDisplay(run.data) ? null : (
          <RunActivity run={run.data} />
        )}
        {ticket.prUrl === "" ? null : (
          <p className="text-sm break-all">
            PR:{" "}
            {prHref !== null ? (
              <a
                href={prHref}
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
