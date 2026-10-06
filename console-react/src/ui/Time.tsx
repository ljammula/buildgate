import { Hourglass, TriangleAlert } from "lucide-react";
import { useCallback, useMemo, useState, useSyncExternalStore } from "react";

import {
  elapsedBetween,
  formatDuration,
  formatLocalTimestamp,
  stallChipDisplay,
  tryParseTimestamp,
  utcTooltip,
} from "@/domain/elapsed";
import type { Run } from "@/domain/run";
import { cn } from "@/ui/cn";
import { toneClasses } from "@/ui/tone";

/**
 * The current instant, refreshed every `intervalMs`; a null interval never
 * ticks (a finished duration). Render stays pure: the clock is read in an
 * effect-driven subscription, and the interval is cleared on unmount.
 */
export function useNow(intervalMs: number | null): Date {
  const [store] = useState(() => {
    let snapshot: number | null = null;
    return {
      read: (): number => (snapshot ??= Date.now()),
      tick: (): void => {
        snapshot = Date.now();
      },
    };
  });
  const subscribe = useCallback(
    (notify: () => void) => {
      if (intervalMs === null) return () => undefined;
      store.tick();
      const id = setInterval(() => {
        store.tick();
        notify();
      }, intervalMs);
      return () => {
        clearInterval(id);
      };
    },
    [intervalMs, store],
  );
  const ms = useSyncExternalStore(subscribe, store.read);
  return useMemo(() => new Date(ms), [ms]);
}

/**
 * A timestamp in local time with the full UTC value on hover; the raw value
 * verbatim, with no tooltip, when it is empty or unparseable.
 */
export function LocalTimeText({ value, className }: { value: string; className?: string }) {
  if (tryParseTimestamp(value) === null) return <span className={className}>{value}</span>;
  return (
    <time dateTime={value} title={utcTooltip(value) ?? undefined} className={className}>
      {formatLocalTimestamp(value)}
    </time>
  );
}

export interface ElapsedTextProps {
  /** RFC3339 instant to count from; empty or unparseable renders 00:00. */
  readonly since: string;
  /** RFC3339 instant to count to; null (or omitted) counts to now, ticking. */
  readonly until?: string | null;
  readonly intervalMs?: number;
  readonly className?: string;
}

/** "mm:ss" (or "hh:mm:ss") elapsed between two instants, ticking while open-ended. */
export function ElapsedText({
  since,
  until = null,
  intervalMs = 1000,
  className,
}: ElapsedTextProps) {
  const now = useNow(until === null ? intervalMs : null);
  const ms = elapsedBetween(since, until, now);
  return <span className={cn("tabular-nums", className)}>{formatDuration(ms)}</span>;
}

/**
 * The "silence is a bug" chip: "stalled" when the server reports the run
 * stalled, or "waiting: reason" when it reports a waiting reason; nothing
 * when neither applies.
 */
export function StallChip({ run }: { run: Pick<Run, "stalled" | "waitingReason"> }) {
  const display = stallChipDisplay(run);
  if (display === null) return null;
  const classes = toneClasses[display.tone];
  const Icon = display.icon === "warning_amber" ? TriangleAlert : Hourglass;
  return (
    <span
      data-testid={`${display.kind}-chip`}
      data-tone={display.tone}
      className={cn(
        "inline-flex items-center gap-1 rounded-full border px-2 py-0.5 text-xs font-medium whitespace-nowrap",
        classes.text,
        classes.soft,
        classes.border,
      )}
    >
      <Icon aria-hidden className="size-3.5 shrink-0" />
      {display.label}
    </span>
  );
}
