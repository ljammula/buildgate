import type { RequestSummary } from "@/domain/request";
import { RequestStageChip } from "@/shared/request/RequestStageChip";
import { cn } from "@/ui/cn";

export interface TriageListProps {
  readonly requests: readonly RequestSummary[];
  readonly focusedId: string;
  readonly onFocus: (index: number) => void;
}

/** The queue, oldest wait first; the focused row is the one j/k, a and r act on. */
export function TriageList({ requests, focusedId, onFocus }: TriageListProps) {
  return (
    <ul
      aria-label="Requests to review"
      className="border-border divide-border divide-y rounded-lg border"
    >
      {requests.map((request, index) => {
        const selected = request.id === focusedId;
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
              <span className="min-w-0">
                <span className="text-fg block truncate text-sm font-medium">
                  {request.title !== "" ? request.title : request.id}
                </span>
                <span className="text-fg-muted block truncate text-xs">{request.project}</span>
              </span>
              <RequestStageChip state={request.state} needsYou />
            </button>
          </li>
        );
      })}
    </ul>
  );
}
