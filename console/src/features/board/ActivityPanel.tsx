import { Link } from "react-router";

import type { ActivityEntry } from "@/domain/activity";
import { stateLabel } from "@/domain/status";
import { escapeInvisible } from "@/domain/textEscape";
import { requestPath } from "@/routes/paths";
import { cn } from "@/ui/cn";
import { RelativeTime } from "@/ui/RelativeTime";

export interface ActivityPanelProps {
  readonly entries: readonly ActivityEntry[];
  /** "Last 7 days", "Last 7 days · alpha": the window and the projects the moves are from. */
  readonly windowLabel: string;
  readonly className?: string;
}

/**
 * The latest state moves across every request, newest first: which request,
 * from which state to which, who moved it and how long ago. The list scrolls
 * inside its own box. Absent while no request has moved in the window.
 */
export function ActivityPanel({ entries, windowLabel, className }: ActivityPanelProps) {
  if (entries.length === 0) return null;
  return (
    <section aria-label="Activity" className={cn("flex min-h-0 min-w-0 flex-col gap-2", className)}>
      <h2 className="text-fg flex shrink-0 items-baseline gap-2 text-base font-semibold">
        Activity
        <span className="text-fg-muted text-xs font-normal">{windowLabel}</span>
      </h2>
      {/* Focusable, so the keyboard can scroll it. */}
      <ol
        aria-label="Recent activity"
        tabIndex={0}
        className="border-border bg-surface divide-border min-h-0 flex-1 divide-y overflow-y-auto rounded-lg border text-sm focus-visible:outline-2 focus-visible:-outline-offset-2"
      >
        {entries.map((entry) => (
          <li
            key={`${entry.requestId} ${entry.at} ${entry.from} ${entry.to}`}
            data-testid="activity-entry"
            className="flex flex-col gap-0.5 px-3 py-2"
          >
            <Link
              to={requestPath(entry.requestId)}
              title={entry.requestId}
              className="text-fg truncate font-medium hover:underline"
            >
              {entry.title}
            </Link>
            <span className="text-fg-muted flex flex-wrap items-center gap-x-1.5 text-xs">
              <span title={`${entry.from} -> ${entry.to}`}>
                {`${stateLabel(entry.from)} → ${stateLabel(entry.to)}`}
              </span>
              <span aria-hidden>·</span>
              <span>{`by ${escapeInvisible(entry.by)}`}</span>
              <span aria-hidden>·</span>
              <RelativeTime value={entry.at} />
            </span>
          </li>
        ))}
      </ol>
    </section>
  );
}
