import type { TimelineRow } from "@/domain/runDetail";
import { TimelineRowItem } from "@/features/run-detail/TimelineRowItem";
import { Disclosure } from "@/ui/Disclosure";

/**
 * The stages after the last one that has started, folded into one row: a run
 * in its first stage should show that stage, not a screenful of pending
 * ones. The rows stay in the list (closed `details`), so every stage name is
 * still in the page for assistive technology and a click away for the eye.
 */
export function TimelineNotStarted({ rows }: { readonly rows: readonly TimelineRow[] }) {
  return (
    <li data-testid="timeline-not-started" className="py-1.5">
      <Disclosure bare headingLevel={null} title={`${rows.length} later stages not started`}>
        <ol className="divide-border/60 -mt-2 divide-y">
          {rows.map((row) => (
            <TimelineRowItem key={row.rowKey} row={row} live={false} stalled={false} />
          ))}
        </ol>
      </Disclosure>
    </li>
  );
}
