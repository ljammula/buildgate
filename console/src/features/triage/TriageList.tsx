import { cardAge, cardSince } from "@/domain/boardCard";
import { formatLocalTimestamp } from "@/domain/elapsed";
import type { RequestSummary } from "@/domain/request";
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
  return (
    <ul
      aria-label="Requests to review"
      className="border-border divide-border divide-y rounded-lg border"
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
              <span className="flex w-32 shrink-0 justify-end">
                <RequestStageChip state={request.state} needsYou />
              </span>
            </button>
          </li>
        );
      })}
    </ul>
  );
}
