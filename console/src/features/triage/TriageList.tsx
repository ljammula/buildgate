import { useEffect, useRef } from "react";

import { cardAge, cardSince } from "@/domain/boardCard";
import { formatLocalTimestamp } from "@/domain/elapsed";
import { type RequestSummary, requestAwaitingPullRequest } from "@/domain/request";
import { RequestStageChip } from "@/shared/request/RequestStageChip";
import { cn } from "@/ui/cn";

export interface TriageListProps {
  readonly requests: readonly RequestSummary[];
  readonly focusedId: string;
  /** The clock the waiting ages are counted against. */
  readonly now: Date;
  readonly onFocus: (index: number) => void;
}

/** The queue, oldest wait first; the focused row is the one j/k, a and r act on. */
export function TriageList({ requests, focusedId, now, onFocus }: TriageListProps) {
  // The list is its own scroll box, so a long queue never pushes the decision
  // pane out of view: the focused row is kept in sight as j/k move it.
  const list = useRef<HTMLUListElement>(null);
  useEffect(() => {
    const row = list.current?.querySelector('[aria-current="true"]');
    // Absent in a test DOM.
    if (row instanceof HTMLElement && typeof row.scrollIntoView === "function") {
      row.scrollIntoView({ block: "nearest" });
    }
  }, [focusedId]);
  return (
    <div className="flex min-h-0 flex-col gap-1.5 self-start lg:max-h-[calc(100vh-7rem)]">
      <p data-testid="triage-count" className="text-fg-muted shrink-0 px-1 text-xs">
        {requests.length === 1 ? "1 waiting" : `${requests.length} waiting, oldest first`}
      </p>
      <ul
        ref={list}
        aria-label="Requests to review"
        // Focusable, so the keyboard can scroll it.
        tabIndex={0}
        className="border-border divide-border min-h-0 divide-y overflow-y-auto rounded-lg border focus-visible:outline-2 focus-visible:-outline-offset-2"
      >
        {requests.map((request, index) => {
          const selected = request.id === focusedId;
          const since = cardSince(request, "needsYou");
          const age = cardAge(request, "needsYou", now);
          return (
            <li key={request.id}>
              <button
                type="button"
                data-testid={`triage-row-${request.id}`}
                aria-current={selected ? "true" : undefined}
                onClick={() => {
                  onFocus(index);
                }}
                className={cn(
                  "flex w-full items-center justify-between gap-3 px-3 py-2 text-left transition-colors first:rounded-t-lg last:rounded-b-lg",
                  selected ? "bg-accent-soft" : "hover:bg-surface-hover",
                )}
              >
                <span className="min-w-0 flex-1 truncate">
                  <span
                    title={request.title !== "" ? request.title : request.id}
                    className="text-fg block truncate text-sm font-medium"
                  >
                    {request.title !== "" ? request.title : request.id}
                  </span>
                  <span className="text-fg-muted flex min-w-0 items-baseline gap-2 text-xs">
                    {/* The id tells same-titled rows apart: it keeps its width, the project gives way. */}
                    <span className="shrink-0 font-mono">{request.id}</span>
                    <span className="min-w-0 truncate">{request.project}</span>
                    {age === null ? null : (
                      <time
                        dateTime={since}
                        title={formatLocalTimestamp(since)}
                        className="text-fg-muted shrink-0 tabular-nums"
                      >
                        {age}
                      </time>
                    )}
                  </span>
                </span>
                <span className="flex shrink-0 justify-end">
                  <RequestStageChip
                    state={request.state}
                    needsYou
                    awaitingPullRequest={requestAwaitingPullRequest(request)}
                  />
                </span>
              </button>
            </li>
          );
        })}
      </ul>
    </div>
  );
}
