// Shared elapsed/duration formatting: the run list's row subtitle and the run
// detail's Timeline section both need the same "mm:ss" (or "hh:mm:ss" once
// past an hour) rendering of a wall-clock duration, and the same tolerant
// parsing of an RFC3339 timestamp string that might be empty or malformed.
//
// stallStatus (below) is the "silence is a bug" rule (progress-contract.md,
// 2026-09-18): the shared rule every screen that shows a run uses to decide
// whether to flag it as stalled. The rule lives once, server-side, in
// internal/progress.Stalled: a client-side copy drifted from the server's (see
// that function's doc comment). Every route this console reads a Run from is
// the same factoryd HTTP API, so stallStatus trusts the server's `stalled`
// verdict instead of re-deriving it from timestamps.
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

// The long end of a duration: "2h 05m" past an hour, "3d 4h" past a day. Whole
// seconds are noise at that scale, so both drop them.
function longForm(totalMinutes: number): string {
  const days = Math.trunc(totalMinutes / 1440);
  const hours = Math.trunc(totalMinutes / 60) % 24;
  const minutes = totalMinutes % 60;
  if (days > 0) return `${days}d ${hours}h`;
  return `${hours}h ${two(minutes)}m`;
}

/**
 * An elapsed duration for reading, not for a stopwatch: "mm:ss" under an
 * hour (the same as formatDuration), "2h 05m" from an hour, "3d 4h" from a
 * day. A negative duration (clock skew) renders as zero.
 */
export function formatElapsedCompact(durationMs: number): string {
  const clamped = durationMs < 0 ? 0 : durationMs;
  const totalMinutes = Math.trunc(clamped / 60_000);
  return totalMinutes < 60 ? formatDuration(clamped) : longForm(totalMinutes);
}

/** An age: "5m", "2h 05m", "2h", "3d 4h". Under a minute is relativeAge's "just now". */
export function formatAgeCompact(ageMs: number): string {
  const clamped = ageMs < 0 ? 0 : ageMs;
  const totalMinutes = Math.trunc(clamped / 60_000);
  if (totalMinutes < 60) return `${totalMinutes}m`;
  // An exact hour or day drops the zero tail: "2h", "1d".
  return longForm(totalMinutes).replace(/ 00m$/, "").replace(/ 0h$/, "");
}

/**
 * "just now", "5m ago", "2h 05m ago", "3d 4h ago" for how long before `now`
 * `value` was; the raw value for an empty or unparseable timestamp. The exact
 * time belongs in a tooltip beside it (ui/RelativeTime does that).
 */
export function relativeAge(value: string, now: Date): string {
  const parsed = tryParseTimestamp(value);
  if (parsed === null) return value;
  const ageMs = now.getTime() - parsed.getTime();
  return ageMs < 60_000 ? "just now" : `${formatAgeCompact(ageMs)} ago`;
}

/** A whole-seconds age: "0s", "75s" (no minutes form), for a counter that ticks every second. A negative age renders as "0s". */
export function formatAgeSeconds(ageMs: number): string {
  return `${Math.max(0, Math.floor(ageMs / 1000))}s`;
}

/**
 * Orders two server timestamps: negative when `a` is earlier, positive when
 * later, 0 when they are the same instant or either cannot be read.
 *
 * Never compare them as text. Go writes RFC 3339 with the fraction's
 * trailing zeros dropped, so "…05Z" sorts after "…05.5Z" as text ("Z" is
 * above "."), and a local offset breaks text order outright. A newer event
 * lost to an older cached record that way (found in review, 2026-10-05).
 * Instants in the same millisecond compare by their written fraction.
 */
export function compareTimestamps(a: string, b: string): number {
  const left = Date.parse(a);
  const right = Date.parse(b);
  if (Number.isNaN(left) || Number.isNaN(right)) return 0;
  if (left !== right) return left - right;
  return fractionOf(a) - fractionOf(b);
}

/** The fraction of a second a timestamp carries, as a number in [0, 1). */
function fractionOf(value: string): number {
  const match = /T\d{2}:\d{2}:\d{2}(\.\d+)?/.exec(value);
  return match?.[1] === undefined ? 0 : Number(`0${match[1]}`);
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

const MONTHS = ["Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec"];

/**
 * An RFC 3339 timestamp as local "HH:mm" when it falls on `now`'s day, or
 * "MMM d HH:mm" otherwise; the raw value when empty or unparseable.
 */
export function formatWhen(at: string, now: Date): string {
  const parsed = tryParseTimestamp(at);
  if (parsed === null) return at;
  const hm = `${two(parsed.getHours())}:${two(parsed.getMinutes())}`;
  const sameDay =
    parsed.getFullYear() === now.getFullYear() &&
    parsed.getMonth() === now.getMonth() &&
    parsed.getDate() === now.getDate();
  return sameDay ? hm : `${MONTHS[parsed.getMonth()] ?? ""} ${parsed.getDate()} ${hm}`;
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
interface StallChipDisplay {
  readonly kind: "stalled" | "waiting";
  readonly label: string;
  /** Danger for stalled, warning for waiting. */
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
