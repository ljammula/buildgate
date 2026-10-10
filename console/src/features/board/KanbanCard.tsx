import { ExternalLink, FileSearch, TriangleAlert, X } from "lucide-react";
import { memo } from "react";
import { Link } from "react-router";

import { cardAge, cardFacts, cardSince } from "@/domain/boardCard";
import type { BoardColumn } from "@/domain/boardColumns";
import { formatLocalTimestamp } from "@/domain/elapsed";
import {
  type RequestSummary,
  requestAwaitingPullRequest,
  requestShortTitle,
} from "@/domain/request";
import { REQUEST_VERBS, stateLabel } from "@/domain/status";
import { escapeInvisible } from "@/domain/textEscape";
import { requestPath } from "@/routes/paths";
import { PrStateChip } from "@/shared/request/PrStateChip";
import { RequestStageChip } from "@/shared/request/RequestStageChip";
import { Button } from "@/ui/Button";
import { CompactId } from "@/ui/CompactId";
import { CopyButton } from "@/ui/CopyButton";
import { cn } from "@/ui/cn";
import { StallChip } from "@/ui/Time";

import { RunningMark } from "./RunningMark";

export interface KanbanCardProps {
  readonly request: RequestSummary;
  readonly column: BoardColumn;
  readonly now: Date;
  /** Name the project on the card: several projects are on the board with no lane to say which. */
  readonly showProject: boolean;
  /**
   * The dense card of a grouped column (Needs you, Drafting): the title on up
   * to two lines, the id, the stage and the age. The sentence of what is
   * asked, the reason of a failure and the controls appear while the card is
   * hovered or holds the focus. The controls stay in the tab order while folded, so the
   * keyboard reaches them and reaching them opens the card. Where nothing can
   * hover, or the pointer is a finger, the card is drawn in full.
   */
  readonly compact?: boolean;
  /**
   * The group's heading already names the card's one state (`oneState` of the
   * group): the stage chip is not drawn, and the state stays in the card for
   * assistive technology.
   */
  readonly stateInHeading?: boolean;
  /** This card's Request changes dialog is open (the screen holds it, so no list change can unmount it). */
  readonly rejecting: boolean;
  /** Opens Request changes for this request. */
  readonly onReject: (id: string) => void;
  /** The console may write: only then is Request changes offered. */
  readonly canWrite: boolean;
  /** The request's state changed, or it appeared, a moment ago: its card is highlighted once. */
  readonly changed?: boolean;
  /** The worker runs a healthy job for this request now and the feed is live: the stage line carries a spinner. */
  readonly running?: boolean;
}

// What a compact card folds away. It is hidden from the eye only (clipped,
// as `sr-only` does), never with `display: none` or `visibility: hidden`: a
// screen reader reading the page still gets what is asked and a
// failure's reason, and the controls stay in the tab order. It is drawn while
// the card is hovered or holds the focus, and always where there is no hover
// or the pointer is a finger (a phone, a touch laptop).
const foldedText =
  "sr-only group-hover/card:not-sr-only group-focus-within/card:not-sr-only [@media(hover:none)]:not-sr-only [@media(any-pointer:coarse)]:not-sr-only";
const foldedControls =
  "max-h-0 overflow-hidden opacity-0 group-hover/card:max-h-40 group-hover/card:overflow-visible group-hover/card:opacity-100 group-focus-within/card:max-h-40 group-focus-within/card:overflow-visible group-focus-within/card:opacity-100 [@media(hover:none)]:max-h-none [@media(hover:none)]:overflow-visible [@media(hover:none)]:opacity-100 [@media(any-pointer:coarse)]:max-h-none [@media(any-pointer:coarse)]:overflow-visible [@media(any-pointer:coarse)]:opacity-100";

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
 * approves one: Request changes is the one write, through its existing dialog,
 * which the screen holds.
 * Memoised: a list update re-renders only the cards whose record changed.
 */
export const KanbanCard = memo(function KanbanCard({
  request,
  column,
  now,
  showProject,
  compact = false,
  stateInHeading = false,
  rejecting,
  onReject,
  canWrite,
  changed = false,
  running = false,
}: KanbanCardProps) {
  const facts = cardFacts(request, column);
  const age = cardAge(request, column, now);
  const ageElement =
    age === null ? null : (
      <time
        dateTime={cardSince(request, column)}
        title={formatLocalTimestamp(cardSince(request, column))}
        className="text-fg-muted tabular-nums"
      >
        {age}
      </time>
    );
  const reviewable = column === "needsYou" && REVIEW_STATES.has(request.state);
  return (
    <li
      data-testid={`card-${request.id}`}
      data-density={compact ? "compact" : "full"}
      data-changed={changed ? "true" : undefined}
      className={cn(
        "group/card border-border bg-surface relative flex shrink-0 flex-col rounded-lg border text-xs transition-colors",
        compact ? "gap-1 p-2" : "gap-1.5 p-3",
        "hover:bg-surface-hover focus-within:bg-surface-hover",
        // The "look here" tint fades back to the card's own background, once.
        "data-[changed=true]:animate-card-changed motion-reduce:animate-none",
        column === "needsYou" && "shadow-[inset_2px_0_0_var(--color-tone-warning)]",
      )}
    >
      <Link
        to={requestPath(request.id)}
        title={request.title !== "" ? request.title : request.id}
        className={cn(
          "text-fg text-sm font-medium after:absolute after:inset-0 after:content-[''] hover:underline focus-visible:outline-2 focus-visible:-outline-offset-2",
          // Two lines at most; the whole title is the link's name and its tooltip.
          "line-clamp-2",
        )}
      >
        {requestShortTitle(request)}
      </Link>
      {/* A compact card keeps its id and its age on one row. */}
      <div className={cn(compact && "flex flex-wrap items-baseline justify-between gap-x-2")}>
        {/* Beside the id, which every card shows: a folded line would hide it. */}
        <span className="inline-flex items-center gap-1.5">
          {running ? <RunningMark /> : null}
          {/* Above the title link that stretches over the card, so the copy button takes the click. */}
          <CompactId
            value={request.id}
            max={22}
            label={`request id ${request.id}`}
            className="text-fg-subtle relative z-10"
          />
        </span>
        {compact ? ageElement : null}
      </div>
      {showProject ? (
        <span className="text-fg-muted truncate" title={request.project}>
          {request.project}
        </span>
      ) : null}
      {column === "needsYou" && stateInHeading ? (
        <span className="sr-only">{stateLabel(request.state)}</span>
      ) : column === "needsYou" ? (
        // A long label ("Accepted · awaiting PR") in a narrow lane is clipped, never wider than the card.
        <div className="truncate">
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
              <span className={cn(compact && foldedText)}>
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
            compact && !(column === "drafting" && index === 0 && !stateInHeading) && foldedText,
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
                <span className="relative z-10 inline-flex items-center gap-0.5">
                  <a
                    href={pr.url}
                    target="_blank"
                    rel="noopener noreferrer"
                    className="text-accent inline-flex items-center gap-1 hover:underline"
                  >
                    {`Ticket ${pr.ticket} PR`}
                    <ExternalLink aria-hidden className="size-3" />
                  </a>
                  <CopyButton
                    size="sm"
                    text={pr.url}
                    label={`Copy pull request link for ${request.id} ticket ${pr.ticket}`}
                  />
                </span>
              )}
              <PrStateChip prState={pr.prState} />
            </li>
          ))}
        </ul>
      ) : null}
      {compact ? null : ageElement}
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
              // Not `disabled`: the button keeps the focus, so it comes
              // back here when the dialog closes.
              aria-disabled={rejecting}
              className={cn(rejecting && "opacity-50")}
              onClick={() => {
                if (!rejecting) onReject(request.id);
              }}
            >
              <X aria-hidden />
              {REQUEST_VERBS.requestChanges}
            </Button>
          ) : null}
        </div>
      ) : null}
    </li>
  );
});
