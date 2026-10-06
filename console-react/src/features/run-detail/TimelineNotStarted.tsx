import { ChevronRight } from "lucide-react";

import type { TimelineRow } from "@/domain/runDetail";
import { TimelineRowItem } from "@/features/run-detail/TimelineRowItem";

/**
 * The stages after the last one that has started, folded into one row: a run
 * in its first stage should show that stage, not a screenful of pending
 * ones. The rows stay in the list (closed `details`), so every stage name is
 * still in the page for assistive technology and a click away for the eye.
 */
export function TimelineNotStarted({ rows }: { readonly rows: readonly TimelineRow[] }) {
  return (
    <li data-testid="timeline-not-started" className="py-1.5">
      <details className="group">
        <summary className="text-fg-muted hover:text-fg flex cursor-pointer list-none items-center gap-2 text-sm select-none">
          <ChevronRight
            aria-hidden="true"
            className="text-fg-subtle size-4 transition-transform group-open:rotate-90"
          />
          {`${rows.length} later stages not started`}
        </summary>
        <ol className="divide-border/60 mt-1 divide-y">
          {rows.map((row) => (
            <TimelineRowItem key={row.rowKey} row={row} />
          ))}
        </ol>
      </details>
    </li>
  );
}
