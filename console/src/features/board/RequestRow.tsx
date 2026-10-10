import { Link } from "react-router";

import {
  formatRequestTokenTotal,
  formatUsageSummary,
  formatUsageTokens,
  requestTokenTotal,
} from "@/domain/cost";
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
import { MemoryBadge } from "@/shared/request/MemoryBadge";
import { PrStateChip } from "@/shared/request/PrStateChip";
import { RequestStageChip } from "@/shared/request/RequestStageChip";
import { RequestStatusUnit } from "@/shared/request/RequestStatusUnit";
import { CompactId } from "@/ui/CompactId";
import { cn } from "@/ui/cn";
import { TableCell, TableRow } from "@/ui/Table";
import { RelativeTime } from "@/ui/RelativeTime";
import { TicketRollupStrip } from "@/ui/TicketRollupStrip";

export interface RequestRowProps {
  readonly request: RequestSummary;
  readonly now: Date;
  /** The project column: absent when every request is in one project, which then says nothing. */
  readonly showProject: boolean;
}

/**
 * One request. The whole row is the link: the title's anchor stretches over
 * it, so a click anywhere opens the request and the keyboard reaches it with
 * one Tab stop.
 */
export function RequestRow({ request, now, showProject }: RequestRowProps) {
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
  // The row keeps its rhythm: a one-line gist here, the server's whole
  // explanation in the tooltip and on the request page.
  const progressLines = [
    job === null ? null : { text: activeJobLabel(job), title: activeJobLabel(job) },
    awaitingPr
      ? {
          text: "Built and verified · no PR opened",
          title: requestAwaitingPullRequestLabel(request),
        }
      : null,
  ].filter((line): line is { text: string; title: string } => line !== null);

  return (
    <TableRow
      data-testid={`request-${request.id}`}
      className={cn(
        "relative align-top focus-within:bg-surface-hover",
        needsYou && "bg-tone-warning-soft/30 shadow-[inset_2px_0_0_var(--color-tone-warning)]",
      )}
    >
      <TableCell>
        <RequestStatusUnit request={request} now={now}>
          <RequestStageChip
            state={request.state}
            awaitingPullRequest={awaitingPr}
            waitingOn={request.waitingOn}
            needsYou={needsYou}
          />
        </RequestStatusUnit>
      </TableCell>
      <TableCell>
        <Link
          to={requestPath(request.id)}
          title={request.title !== "" ? request.title : request.id}
          className="text-fg block truncate font-medium after:absolute after:inset-0 after:content-[''] hover:underline focus-visible:outline-2 focus-visible:-outline-offset-2"
        >
          {requestShortTitle(request)}
        </Link>
        <CompactId
          value={request.id}
          max={36}
          label={`request id ${request.id}`}
          className="text-fg-subtle relative z-10 flex text-xs"
        />
        <MemoryBadge request={request} />
      </TableCell>
      {showProject ? (
        <TableCell className="text-fg-muted truncate" title={request.project}>
          {request.project}
        </TableCell>
      ) : null}
      <TableCell>
        <div className="flex flex-col items-start gap-1">
          {progressLines.map((line) => (
            <span key={line.text} title={line.title} className="text-fg-muted line-clamp-2 text-xs">
              {line.text}
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
        {/* One token figure from the server's cost_summary (no dollar figure),
            the model breakdown on hover; else the older token-total proxy. */}
        {cost !== null ? (
          <span data-testid="request-cost-total" title={formatUsageSummary(cost)}>
            {formatUsageTokens(cost)}
          </span>
        ) : (
          <span data-testid="request-token-total">
            {formatRequestTokenTotal(requestTokenTotal(request))}
          </span>
        )}
      </TableCell>
      <TableCell className="text-fg-muted text-xs whitespace-nowrap tabular-nums">
        <RelativeTime value={request.updatedAt} />
      </TableCell>
    </TableRow>
  );
}
