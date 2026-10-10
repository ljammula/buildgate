import { ExternalLink, FileSearch, TriangleAlert, X } from "lucide-react";
import { useState } from "react";
import { Link } from "react-router";

import { cardAge, cardFacts, cardSince } from "@/domain/boardCard";
import type { BoardColumn } from "@/domain/boardColumns";
import { formatLocalTimestamp } from "@/domain/elapsed";
import {
  type RequestSummary,
  requestAwaitingPullRequest,
  requestShortTitle,
} from "@/domain/request";
import { REQUEST_VERBS } from "@/domain/status";
import { escapeInvisible } from "@/domain/textEscape";
import { requestPath } from "@/routes/paths";
import { PrStateChip } from "@/shared/request/PrStateChip";
import { RequestStageChip } from "@/shared/request/RequestStageChip";
import { Button } from "@/ui/Button";
import { CompactId } from "@/ui/CompactId";
import { cn } from "@/ui/cn";
import { StallChip } from "@/ui/Time";

import { CardDecision } from "./CardDecision";

export interface KanbanCardProps {
  readonly request: RequestSummary;
  readonly column: BoardColumn;
  readonly now: Date;
  /** Name the project on the card: several projects are on the board with no lane to say which. */
  readonly showProject: boolean;
  /** The console may write: only then is Request changes offered. */
  readonly canWrite: boolean;
}

/**
 * The states whose card carries Review and Request changes. Review is a link
 * to the request page: an approval is made there, with the spec, plan or
 * oracle files on screen. A card never offers Approve.
 */
const REVIEW_STATES: ReadonlySet<string> = new Set(["spec_review", "plan_review", "oracle_review"]);

/**
 * One request on the board. The title is the link and stretches over the
 * card, so a click anywhere opens the request; the buttons and the pull
 * request links sit above it. A card never moves a request and never
 * approves one: Request changes is the one write, through its existing dialog.
 */
export function KanbanCard({ request, column, now, showProject, canWrite }: KanbanCardProps) {
  const [rejecting, setRejecting] = useState(false);
  const facts = cardFacts(request, column);
  const age = cardAge(request, column, now);
  const reviewable = column === "needsYou" && REVIEW_STATES.has(request.state);
  return (
    <li
      data-testid={`card-${request.id}`}
      className={cn(
        "border-border bg-surface relative flex shrink-0 flex-col gap-1.5 rounded-lg border p-3 text-xs transition-colors",
        "hover:bg-surface-hover focus-within:bg-surface-hover",
        column === "needsYou" && "shadow-[inset_2px_0_0_var(--color-tone-warning)]",
      )}
    >
      <Link
        to={requestPath(request.id)}
        title={request.title !== "" ? request.title : request.id}
        className="text-fg line-clamp-2 text-sm font-medium after:absolute after:inset-0 after:content-[''] hover:underline focus-visible:outline-2 focus-visible:-outline-offset-2"
      >
        {requestShortTitle(request)}
      </Link>
      <CompactId value={request.id} max={22} copy={false} className="text-fg-subtle" />
      {showProject ? (
        <span className="text-fg-muted truncate" title={request.project}>
          {request.project}
        </span>
      ) : null}
      {column === "needsYou" ? (
        <div>
          <RequestStageChip
            state={request.state}
            awaitingPullRequest={requestAwaitingPullRequest(request)}
            needsYou
          />
        </div>
      ) : null}
      {facts.alert === null ? null : (
        <p
          data-testid="kanban-alert"
          className="text-tone-danger flex items-start gap-1 font-medium"
        >
          <TriangleAlert aria-hidden className="mt-0.5 size-3.5 shrink-0" />
          <span className="line-clamp-3">
            {facts.alert.reason === ""
              ? facts.alert.label
              : `${facts.alert.label}: ${escapeInvisible(facts.alert.reason)}`}
          </span>
        </p>
      )}
      {facts.lines.map((line) => (
        <p key={line} title={line} className="text-fg-muted line-clamp-2">
          {line}
        </p>
      ))}
      {facts.queue === null ? null : (
        <p data-testid="kanban-queue" className="text-fg-muted">
          {facts.queue}
        </p>
      )}
      {facts.stalled ? (
        <div>
          <StallChip run={{ stalled: true, waitingReason: null }} />
        </div>
      ) : null}
      {facts.pullRequests.length > 0 ? (
        <ul aria-label="Pull requests" className="flex flex-col gap-1">
          {facts.pullRequests.map((pr) => (
            <li key={pr.ticket} className="flex flex-wrap items-center gap-1.5">
              {pr.url === null ? (
                <span className="text-fg-muted">{`Ticket ${pr.ticket}`}</span>
              ) : (
                <a
                  href={pr.url}
                  target="_blank"
                  rel="noopener noreferrer"
                  className="text-accent relative z-10 inline-flex items-center gap-1 hover:underline"
                >
                  {`Ticket ${pr.ticket} PR`}
                  <ExternalLink aria-hidden className="size-3" />
                </a>
              )}
              <PrStateChip prState={pr.prState} />
            </li>
          ))}
        </ul>
      ) : null}
      {age === null ? null : (
        <time
          dateTime={cardSince(request, column)}
          title={formatLocalTimestamp(cardSince(request, column))}
          className="text-fg-subtle tabular-nums"
        >
          {age}
        </time>
      )}
      {reviewable ? (
        <div className="relative z-10 flex flex-wrap gap-1.5 pt-0.5">
          <Button asChild size="sm" variant="primary">
            <Link to={requestPath(request.id)}>
              <FileSearch aria-hidden />
              Review
            </Link>
          </Button>
          {canWrite ? (
            <Button
              size="sm"
              disabled={rejecting}
              onClick={() => {
                setRejecting(true);
              }}
            >
              <X aria-hidden />
              {REQUEST_VERBS.requestChanges}
            </Button>
          ) : null}
        </div>
      ) : null}
      {rejecting ? (
        <CardDecision
          request={request}
          onClose={() => {
            setRejecting(false);
          }}
        />
      ) : null}
    </li>
  );
}
