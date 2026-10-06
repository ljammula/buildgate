import { Hourglass, TriangleAlert } from "lucide-react";
import { useCallback, useMemo, useState, useSyncExternalStore } from "react";

import {
  elapsedBetween,
  formatElapsedCompact,
  formatLocalTimestamp,
  stallChipDisplay,
  tryParseTimestamp,
  utcTooltip,
} from "@/domain/elapsed";
import type { Run } from "@/domain/run";
import { cn } from "@/ui/cn";
import { toneClasses } from "@/ui/tone";

interface SharedClock {
  now: number;
  readonly listeners: Set<() => void>;
  timer: ReturnType<typeof setInterval> | undefined;
}

// One timer per interval value, shared by every subscriber: fifty elapsed
// counters on a screen are one setInterval, not fifty. It starts with the
// first subscriber and stops, and is forgotten, with the last.
const clocks = new Map<number, SharedClock>();

function clockFor(intervalMs: number): SharedClock {
  let clock = clocks.get(intervalMs);
  if (clock === undefined) {
    clock = { now: Date.now(), listeners: new Set(), timer: undefined };
    clocks.set(intervalMs, clock);
  }
  return clock;
}

function subscribeClock(intervalMs: number, notify: () => void): () => void {
  const clock = clockFor(intervalMs);
  if (clock.listeners.size === 0) {
    clock.now = Date.now();
    clock.timer = setInterval(() => {
      clock.now = Date.now();
      for (const listener of [...clock.listeners]) listener();
    }, intervalMs);
  }
  clock.listeners.add(notify);
  return () => {
    clock.listeners.delete(notify);
    if (clock.listeners.size === 0) {
      clearInterval(clock.timer);
      clocks.delete(intervalMs);
    }
  };
}

/**
 * The current instant, refreshed every `intervalMs`; a null interval never
 * ticks (a finished duration). All callers with the same interval share one
 * timer. Render stays pure: the clock is read through a subscription.
 */
export function useNow(intervalMs: number | null): Date {
  const [frozen] = useState(() => Date.now());
  const subscribe = useCallback(
    (notify: () => void) =>
      intervalMs === null ? () => undefined : subscribeClock(intervalMs, notify),
    [intervalMs],
  );
  const read = useCallback(
    () => (intervalMs === null ? frozen : clockFor(intervalMs).now),
    [intervalMs, frozen],
  );
  const ms = useSyncExternalStore(subscribe, read);
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

/** Elapsed between two instants ("mm:ss", then "2h 05m", "3d 4h"), ticking while open-ended. */
export function ElapsedText({
  since,
  until = null,
  intervalMs = 1000,
  className,
}: ElapsedTextProps) {
  const now = useNow(until === null ? intervalMs : null);
  const ms = elapsedBetween(since, until, now);
  return <span className={cn("tabular-nums", className)}>{formatElapsedCompact(ms)}</span>;
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
