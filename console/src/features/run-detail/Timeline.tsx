import { useMemo } from "react";

import type { ApiError } from "@/domain/apiError";
import type { ProgressEvent, Run } from "@/domain/run";
import { runIsTerminalForDisplay } from "@/domain/run";
import { computeTimeline } from "@/domain/runDetail";
import { TimelineNotStarted } from "@/features/run-detail/TimelineNotStarted";
import { TimelineRowItem } from "@/features/run-detail/TimelineRowItem";
import { COLLAPSE_AT, firstUntouchedTail } from "@/features/run-detail/timelineFold";
import { computeStatusStrip } from "@/features/run-detail/statusStrip";
import { stallChipDisplay } from "@/domain/elapsed";
import { ErrorCallout } from "@/ui/ErrorDisplay";
import { Callout } from "@/ui/Feedback";
import { StatusChipForToken } from "@/ui/StatusChip";
import { StallChip, useNow } from "@/ui/Time";

export interface TimelineProps {
  readonly run: Run;
  readonly events: readonly ProgressEvent[];
  readonly error: ApiError | null;
}

/**
 * A vertical stepper over the factory stages, built from the progress feed.
 * It is an ordered list of text, so it is in the accessibility tree: each
 * stage names its status word, label and duration.
 */
export function Timeline({ run, events, error }: TimelineProps) {
  // Durations tick while the run can still change; a finished run's are fixed.
  const now = useNow(runIsTerminalForDisplay(run) ? null : 1000);
  const rows = useMemo(() => computeTimeline(events, now, run), [events, now, run]);
  const stalled = stallChipDisplay(run)?.kind === "stalled";
  // A spinner means work is happening: not for a stalled run, a lost feed or a finished run.
  const live = !stalled && error === null && !runIsTerminalForDisplay(run);
  const strip = computeStatusStrip(events, run, now);
  const tail = firstUntouchedTail(rows);
  const folded = rows.length - tail >= COLLAPSE_AT;
  const shown = folded ? rows.slice(0, tail) : rows;
  return (
    <div className="flex flex-col gap-3">
      {error !== null ? <ErrorCallout error={error} /> : null}
      <div
        data-testid="timeline-status-strip"
        className="flex flex-wrap items-center gap-x-3 gap-y-1 border-b border-border pb-3 text-sm text-fg-muted"
      >
        <span className="text-sm font-semibold text-fg">{strip.label}</span>
        {strip.round !== null ? <span>{strip.round}</span> : null}
        <span className="tabular-nums">Elapsed {strip.elapsed}</span>
        {strip.lastActivity !== null ? (
          <span className="tabular-nums">Last activity {strip.lastActivity} ago</span>
        ) : null}
        <StallChip run={run} />
        <StatusChipForToken token={run.state} />
      </div>
      {strip.lastActivity !== null && stalled ? (
        <Callout tone="danger" data-testid="stall-explanation">
          Stalled: nothing has been reported for {strip.lastActivity}. The worker may have stopped;
          check that `factoryd worker` is running.
        </Callout>
      ) : null}
      {strip.latest !== null ? (
        <p data-testid="timeline-latest" className="truncate text-xs text-fg-muted">
          Latest: {strip.latest}
        </p>
      ) : null}
      <ol aria-label="Timeline stages" className="divide-y divide-border/60">
        {shown.map((row) => (
          <TimelineRowItem key={row.rowKey} row={row} live={live} stalled={stalled} />
        ))}
        {folded ? <TimelineNotStarted rows={rows.slice(tail)} /> : null}
      </ol>
    </div>
  );
}
