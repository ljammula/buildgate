import { ArrowRight, CircleAlert, ExternalLink } from "lucide-react";
import { Link } from "react-router";

import { useRunRecord } from "@/api/runQueries";
import type { RequestTicket } from "@/domain/request";
import { type Run, runIsTerminalForDisplay } from "@/domain/run";
import { runStop } from "@/domain/runStop";
import { safeHttpUrl } from "@/domain/safeUrl";
import { EscapedText } from "@/shared/oracle/EscapedText";
import { runPath } from "@/routes/paths";
import { PrStateChip } from "@/shared/request/PrStateChip";
import { Button } from "@/ui/Button";
import { Spinner } from "@/ui/Feedback";
import { RelativeTime } from "@/ui/RelativeTime";
import { StatusChip, StatusChipForToken } from "@/ui/StatusChip";
import { StallChip } from "@/ui/Time";

import { ReviewRounds } from "./ReviewRounds";

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
  const stop = run.data === undefined ? null : runStop(run.data);
  return (
    <div
      data-testid={`ticket-card-${ticket.index}`}
      className="flex flex-col gap-2 py-3 first:pt-0 last:pb-0"
    >
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
      {stop === null || stop.cause === "" ? null : (
        <p
          data-testid={`ticket-stop-cause-${ticket.index}`}
          className="font-mono text-xs break-words whitespace-pre-wrap"
        >
          {/* Why this ticket's run stopped, here where the request names
                only the stage: the run's own cause usually names the fix. */}
          <EscapedText text={stop.cause} />
        </p>
      )}
      {ticket.prUrl === "" ? null : (
        <p className="text-sm break-all">
          PR:{" "}
          {prHref !== null ? (
            <a
              href={prHref}
              target="_blank"
              rel="noopener noreferrer"
              className="text-accent inline-flex items-baseline gap-1 hover:underline"
            >
              {ticket.prUrl}
              <ExternalLink aria-hidden="true" className="size-3.5 shrink-0 self-center" />
            </a>
          ) : (
            <span className="font-mono">{ticket.prUrl}</span>
          )}
        </p>
      )}
      <MergeReadinessLine ticket={ticket} />
      <ReviewRounds ticket={ticket} />
      {ticket.runId === "" ? null : (
        <div>
          <Button asChild size="sm">
            <Link to={runPath(ticket.runId)}>
              View run
              <ArrowRight aria-hidden="true" />
            </Link>
          </Button>
        </div>
      )}
    </div>
  );
}

/**
 * The last ready-to-merge check of the ticket's open pull request: a chip
 * when it passed, else each thing the bar still lacks. Nothing before the
 * first check, while a round runs, or once the pull request has merged.
 */
function MergeReadinessLine({ ticket }: { readonly ticket: RequestTicket }) {
  const readiness = ticket.mergeReadiness;
  if (readiness === null || ticket.prUrl === "" || ticket.prState === "merged") return null;
  return (
    <div data-testid={`ticket-merge-readiness-${ticket.index}`} className="flex flex-col gap-1">
      {readiness.ready ? (
        <div className="flex flex-wrap items-center gap-2 text-sm">
          <StatusChip status="done" label="Ready to merge" />
          <span className="text-fg-muted">
            checks pass, no review thread is open, last code review of the whole diff clean
          </span>
        </div>
      ) : (
        <>
          <p className="text-fg-muted text-xs font-semibold">Not ready to merge</p>
          <ul className="list-disc pl-5 text-sm">
            {readiness.blockers.map((blocker) => (
              <li key={blocker}>
                <EscapedText text={blocker} />
              </li>
            ))}
          </ul>
        </>
      )}
      {readiness.checkedAt === "" ? null : (
        <span className="text-fg-subtle text-xs">
          checked <RelativeTime value={readiness.checkedAt} />
        </span>
      )}
    </div>
  );
}
