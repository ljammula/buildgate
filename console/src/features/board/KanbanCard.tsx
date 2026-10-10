import { ExternalLink, FileSearch, TriangleAlert, X } from "lucide-react";
import { memo, useState } from "react";
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
  /**
   * The dense card of a grouped column (Needs you, Drafting): the title on one
   * line, the stage and the age. The id, the sentence of what is asked, the
   * reason of a failure and the controls appear while the card is hovered or
   * holds the focus. The controls stay in the tab order while folded, so the
   * keyboard reaches them and reaching them opens the card. Where nothing can
   * hover (a touch screen) the card is drawn in full.
   */
  readonly compact?: boolean;
  /** The console may write: only then is Request changes offered. */
  readonly canWrite: boolean;
}

/**
 * The states whose card carries Review and Request changes. Review is a link
 * to the request page: an approval is made there, with the spec, plan or
 * oracle files on screen. A card never offers Approve.
 */
// What a compact card folds away. Text is taken out of the layout; the
// controls are only clipped to no height, never `display: none` or
// `visibility: hidden`, which would take them out of the tab order.
const revealed = "group-hover/card:block group-focus-within/card:block [@media(hover:none)]:block";
const foldedText = `hidden ${revealed}`;
const foldedInline =
  "hidden group-hover/card:inline group-focus-within/card:inline [@media(hover:none)]:inline";
const foldedControls =
  "max-h-0 overflow-hidden opacity-0 group-hover/card:max-h-40 group-hover/card:overflow-visible group-hover/card:opacity-100 group-focus-within/card:max-h-40 group-focus-within/card:overflow-visible group-focus-within/card:opacity-100 [@media(hover:none)]:max-h-none [@media(hover:none)]:overflow-visible [@media(hover:none)]:opacity-100";

const REVIEW_STATES: ReadonlySet<string> = new Set(["spec_review", "plan_review", "oracle_review"]);

/**
 * One request on the board. The title is the link and stretches over the
 * card, so a click anywhere opens the request; the buttons and the pull
 * request links sit above it. A card never moves a request and never
 * approves one: Request changes is the one write, through its existing dialog.
 * Memoised: a list update re-renders only the cards whose record changed.
 */
export const KanbanCard = memo(function KanbanCard({
  request,
  column,
  now,
  showProject,
  compact = false,
  canWrite,
}: KanbanCardProps) {
  const [rejecting, setRejecting] = useState(false);
  const facts = cardFacts(request, column);
  const age = cardAge(request, column, now);
  const reviewable = column === "needsYou" && REVIEW_STATES.has(request.state);
  return (
    <li
      data-testid={`card-${request.id}`}
      data-density={compact ? "compact" : "full"}
      className={cn(
        "group/card border-border bg-surface relative flex shrink-0 flex-col rounded-lg border text-xs transition-colors",
        compact ? "gap-1 p-2" : "gap-1.5 p-3",
        "hover:bg-surface-hover focus-within:bg-surface-hover",
        column === "needsYou" && "shadow-[inset_2px_0_0_var(--color-tone-warning)]",
      )}
    >
      <Link
        to={requestPath(request.id)}
        title={request.title !== "" ? request.title : request.id}
        className={cn(
          "text-fg text-sm font-medium after:absolute after:inset-0 after:content-[''] hover:underline focus-visible:outline-2 focus-visible:-outline-offset-2",
          // One line while folded; the whole title is the link's name and its tooltip.
          compact ? "truncate" : "line-clamp-2",
        )}
      >
        {requestShortTitle(request)}
      </Link>
      <CompactId
        value={request.id}
        max={22}
        copy={false}
        className={cn("text-fg-subtle", compact && foldedInline)}
      />
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
          {/* The marker and its word stay on a folded card; the reason opens with it. */}
          <span className="line-clamp-3">
            {facts.alert.label}
            {facts.alert.reason === "" ? null : (
              <span className={cn(compact && foldedInline)}>
                {`: ${escapeInvisible(facts.alert.reason)}`}
              </span>
            )}
          </span>
        </p>
      )}
      {facts.lines.map((line, index) => (
        // Two lines can read the same; their place cannot.
        <p
          key={`${index} ${line}`}
          title={line}
          className={cn(
            "text-fg-muted line-clamp-2",
            // Drafting's first line is its stage, which a folded card keeps.
            compact && !(column === "drafting" && index === 0) && foldedText,
          )}
        >
          {line}
        </p>
      ))}
      {facts.queue === null ? null : (
        <p data-testid="kanban-queue" className={cn("text-fg-muted", compact && foldedText)}>
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
        <div
          data-testid="kanban-controls"
          className={cn("relative z-10 flex flex-wrap gap-1.5 pt-0.5", compact && foldedControls)}
        >
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
});
