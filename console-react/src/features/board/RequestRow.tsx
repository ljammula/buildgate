import { Link } from "react-router";

import { formatRequestTokenTotal, formatUsageSummary, requestTokenTotal } from "@/domain/cost";
import { sectionForRequest } from "@/domain/boardFilters";
import {
  type RequestSummary,
  activeJobLabel,
  requestAwaitingPullRequest,
  requestAwaitingPullRequestLabel,
  requestRunningJob,
  requestShortTitle,
} from "@/domain/request";
import { requestPath } from "@/routes/paths";
import { PrStateChip } from "@/shared/request/PrStateChip";
import { RequestStageChip } from "@/shared/request/RequestStageChip";
import { WaitingBadge } from "@/shared/request/WaitingBadge";
import { cn } from "@/ui/cn";
import { TableCell, TableRow } from "@/ui/Table";
import { LocalTimeText } from "@/ui/Time";
import { TicketRollupStrip } from "@/ui/TicketRollupStrip";

export interface RequestRowProps {
  readonly request: RequestSummary;
  readonly now: Date;
}

/**
 * One request. The whole row is the link: the title's anchor stretches over
 * it, so a click anywhere opens the request and the keyboard reaches it with
 * one Tab stop.
 */
export function RequestRow({ request, now }: RequestRowProps) {
  const needsYou = sectionForRequest(request) === "needsYou";
  const awaitingPr = requestAwaitingPullRequest(request);
  const job = requestRunningJob(request);
  const cost = request.costSummary;
  // The roll-up only shows for requests actively building or in PR review:
  // earlier states have no tickets, and a done request's strip is noise.
  const showRollup = request.state === "building" || request.state === "pr_review";
  // A PR state with no PR URL is a record from before the server stopped
  // writing "draft" for an unopened PR: no chip for a PR that does not exist.
  const prTickets = request.tickets.filter((t) => t.prState !== "" && t.prUrl !== "");
  const progressLines = [
    job === null ? null : activeJobLabel(job),
    awaitingPr ? requestAwaitingPullRequestLabel(request) : null,
  ].filter((line): line is string => line !== null);

  return (
    <TableRow
      data-testid={`request-${request.id}`}
      className={cn(
        "relative align-top focus-within:bg-surface-hover",
        needsYou && "bg-tone-warning-soft/30 shadow-[inset_2px_0_0_var(--color-tone-warning)]",
      )}
    >
      <TableCell>
        <div className="flex flex-col items-start gap-1">
          <RequestStageChip
            state={request.state}
            awaitingPullRequest={awaitingPr}
            waitingOn={request.waitingOn}
            needsYou={needsYou}
          />
          <WaitingBadge request={request} now={now} />
        </div>
      </TableCell>
      <TableCell>
        <Link
          to={requestPath(request.id)}
          title={request.title !== "" ? request.title : request.id}
          className="text-fg block truncate font-medium after:absolute after:inset-0 after:content-[''] hover:underline focus-visible:outline-2 focus-visible:-outline-offset-2"
        >
          {requestShortTitle(request)}
        </Link>
        <span className="text-fg-subtle font-mono text-xs">{request.id}</span>
      </TableCell>
      <TableCell className="text-fg-muted truncate" title={request.project}>
        {request.project}
      </TableCell>
      <TableCell>
        <div className="flex flex-col items-start gap-1">
          {progressLines.map((line) => (
            <span key={line} className="text-fg-muted text-xs">
              {line}
            </span>
          ))}
          <div className="text-fg-muted flex flex-wrap items-center gap-x-3 gap-y-1 text-xs">
            {request.ticketCount > 0 ? (
              <span>{`Ticket ${request.ticketIndex} / ${request.ticketCount}`}</span>
            ) : null}
            {request.rejections.length > 0 ? (
              <span data-testid="rejection-marker" title="Rejected drafts">
                {`↻${request.rejections.length}`}
              </span>
            ) : null}
          </div>
          {showRollup ? <TicketRollupStrip request={request} /> : null}
          {prTickets.length > 0 ? (
            <div className="flex flex-wrap gap-1">
              {prTickets.map((ticket) => (
                <PrStateChip key={ticket.index} prState={ticket.prState} />
              ))}
            </div>
          ) : null}
        </div>
      </TableCell>
      <TableCell className="text-fg-muted font-mono text-xs break-words">
        {/* The server's cost_summary (model id and tokens, no dollar figure)
            when the list provided one, else the older token-total proxy. */}
        {cost !== null ? (
          <span data-testid="request-cost-total">{formatUsageSummary(cost)}</span>
        ) : (
          <span data-testid="request-token-total">
            {formatRequestTokenTotal(requestTokenTotal(request))}
          </span>
        )}
      </TableCell>
      <TableCell className="text-fg-muted text-xs whitespace-nowrap tabular-nums">
        <LocalTimeText value={request.updatedAt} />
      </TableCell>
    </TableRow>
  );
}
