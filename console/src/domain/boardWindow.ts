// The "last x days" window of Mission Control. It applies to finished work
// only: the Done column (done and cancelled requests), the activity feed and
// the numbers. Time is a parameter; nothing here reads the clock.
import type { ActivityEntry } from "@/domain/activity";
import { columnForRequest } from "@/domain/boardColumns";
import type { BoardWindowDays } from "@/domain/boardFilters";
import type { RequestSummary } from "@/domain/request";

const dayMs = 24 * 60 * 60 * 1000;

/** Whether the instant `at` is inside the window ending at `now`. An unreadable time is kept in view. */
function instantInWindow(at: string, days: BoardWindowDays, now: Date): boolean {
  if (days === "all") return true;
  const ms = Date.parse(at);
  return Number.isNaN(ms) || ms >= now.getTime() - days * dayMs;
}

/**
 * Whether the window keeps `request` in view. A request the factory or the
 * operator still has work on (Drafting, Needs you, Building, PR review) is
 * ALWAYS in view, whatever its age: the window must never hide something that
 * waits. Only a request in the Done column (done or cancelled) is judged, by
 * its last update.
 */
export function requestInBoardWindow(
  request: RequestSummary,
  days: BoardWindowDays,
  now: Date,
): boolean {
  if (columnForRequest(request) !== "done") return true;
  return instantInWindow(request.updatedAt, days, now);
}

export interface WindowedRequests {
  readonly shown: readonly RequestSummary[];
  /** Finished requests the window left out. */
  readonly olderHidden: number;
}

/** `requests` with the finished ones outside the window taken out, and how many that was. */
export function applyBoardWindow(
  requests: readonly RequestSummary[],
  days: BoardWindowDays,
  now: Date,
): WindowedRequests {
  const shown = requests.filter((request) => requestInBoardWindow(request, days, now));
  return { shown, olderHidden: requests.length - shown.length };
}

/** The moves made inside the window. */
export function activityInBoardWindow(
  entries: readonly ActivityEntry[],
  days: BoardWindowDays,
  now: Date,
): ActivityEntry[] {
  return entries.filter((entry) => instantInWindow(entry.at, days, now));
}

/** `since` for GET /stats, in the form the trend route takes: "7d", "30d"; null (omitted) for all. */
export function statsSince(days: BoardWindowDays): string | null {
  return days === "all" ? null : `${days}d`;
}

/** The toolbar's word for a choice: "7 days", "30 days", "All". */
export function boardWindowChoiceLabel(days: BoardWindowDays): string {
  return days === "all" ? "All" : `${days} days`;
}

/** What a panel under the window is headed with: "Last 7 days", "All time". */
export function boardWindowLabel(days: BoardWindowDays): string {
  return days === "all" ? "All time" : `Last ${days} days`;
}
