// Shared elapsed/duration formatting: the run list's row subtitle and the run
// detail's Timeline section both need the same "mm:ss" (or "hh:mm:ss" once
// past an hour) rendering of a wall-clock duration, and the same tolerant
// parsing of an RFC3339 timestamp string that might be empty or malformed.
//
// stallStatus (below) is the "silence is a bug" rule (progress-contract.md,
// 2026-09-18): the shared rule every screen that shows a run uses to decide
// whether to flag it as stalled. This used to be computed client-side,
// independently of cmd/factoryd's own copy of the same rule, and the two
// drifted (see internal/progress.Stalled's own doc comment). The rule now
// lives once, server-side, in internal/progress.Stalled; every route this
// console reads a Run from is the same factoryd HTTP API, so stallStatus
// simply trusts the server's `stalled` verdict instead of re-deriving it from
// timestamps.
//
// Durations are milliseconds (a number). Time is a parameter: nothing here
// reads the clock.
import type { Run } from "@/domain/run";

/**
 * Formats a duration in milliseconds as "mm:ss", or "hh:mm:ss" once it
 * reaches an hour. Negative durations (a clock skew between this client and
 * the server) render as zero rather than a confusing negative time.
 */
export function formatDuration(durationMs: number): string {
  const clamped = durationMs < 0 ? 0 : durationMs;
  const totalSeconds = Math.trunc(clamped / 1000);
  const hours = Math.trunc(totalSeconds / 3600);
  const minutes = Math.trunc(totalSeconds / 60) % 60;
  const seconds = totalSeconds % 60;
  const two = (n: number): string => String(n).padStart(2, "0");
  return hours > 0
    ? `${two(hours)}:${two(minutes)}:${two(seconds)}`
    : `${two(minutes)}:${two(seconds)}`;
}

/**
 * Parses an RFC3339 timestamp, returning null (rather than throwing) for an
 * empty or unparseable value.
 */
export function tryParseTimestamp(value: string): Date | null {
  if (value === "") return null;
  const parsed = new Date(value);
  return Number.isNaN(parsed.getTime()) ? null : parsed;
}

/**
 * The elapsed milliseconds between `start` and `end` (defaulting to `now`
 * when `end` is null, e.g. a still-running run): an unparseable or empty
 * `start` renders as zero rather than throwing, since a timeline or list row
 * must always show something.
 */
export function elapsedBetween(start: string, end: string | null, now: Date): number {
  const startTime = tryParseTimestamp(start);
  if (startTime === null) return 0;
  const endTime = end === null ? null : tryParseTimestamp(end);
  return (endTime ?? now).getTime() - startTime.getTime();
}

function two(n: number): string {
  return String(n).padStart(2, "0");
}

/**
 * Formats an RFC3339 `value` in local time as "YYYY-MM-DD HH:MM:SS": every
 * screen's own raw-UTC timestamp (found in the 2026-09-26 operator demo
 * showing UTC verbatim across the request, run and release screens) routes
 * through this one formatter rather than each keeping its own copy. Falls
 * back to the raw value verbatim for an empty or unparseable timestamp
 * rather than throwing.
 */
export function formatLocalTimestamp(value: string): string {
  const parsed = tryParseTimestamp(value);
  if (parsed === null) return value;
  return (
    `${parsed.getFullYear()}-${two(parsed.getMonth() + 1)}-${two(parsed.getDate())} ` +
    `${two(parsed.getHours())}:${two(parsed.getMinutes())}:${two(parsed.getSeconds())}`
  );
}

/** The full UTC value shown on hover next to a local time, as ISO-8601 with milliseconds. */
export function utcTooltip(value: string): string | null {
  const parsed = tryParseTimestamp(value);
  return parsed === null ? null : `UTC: ${parsed.toISOString()}`;
}

/**
 * Returns "stalled" when the server's own `stalled` verdict is true, null
 * otherwise: see this file's own comment for why that verdict is trusted
 * directly rather than re-derived from timestamps here.
 */
export function stallStatus(run: Pick<Run, "stalled">): "stalled" | null {
  return run.stalled ? "stalled" : null;
}

/** What the "silence is a bug" chip shows for a run; null renders nothing. */
export interface StallChipDisplay {
  readonly kind: "stalled" | "waiting";
  readonly label: string;
  /** Dart: red for stalled, amber.shade800 for waiting. */
  readonly tone: "danger" | "warning";
  readonly icon: "warning_amber" | "hourglass_top";
}

/**
 * A "stalled" chip when the server reports `stalled`, or a "waiting: reason"
 * chip when the server reports a `waitingReason` (queued behind another run,
 * e.g.): shown instead, since a run the factory itself has already explained
 * the silence for is not the same as genuine, unexplained silence. Null when
 * neither applies. Stalled takes priority over a stale waiting reason.
 */
export function stallChipDisplay(
  run: Pick<Run, "stalled" | "waitingReason">,
): StallChipDisplay | null {
  if (stallStatus(run) === "stalled") {
    return { kind: "stalled", label: "stalled", tone: "danger", icon: "warning_amber" };
  }
  const waitingReason = run.waitingReason;
  if (waitingReason !== null && waitingReason !== "") {
    return {
      kind: "waiting",
      label: `waiting: ${waitingReason}`,
      tone: "warning",
      icon: "hourglass_top",
    };
  }
  return null;
}
