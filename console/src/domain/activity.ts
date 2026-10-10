// The Mission Control activity feed: the latest state moves across every
// request, from the `history` each list record already carries. Pure.
import { compareTimestamps } from "@/domain/elapsed";
import { type RequestSummary, requestShortTitle } from "@/domain/request";

/** One state move of one request. */
export interface ActivityEntry {
  readonly requestId: string;
  readonly title: string;
  readonly from: string;
  readonly to: string;
  readonly at: string;
  readonly by: string;
  /** Why, as the server recorded it; "" when it gave none. */
  readonly reason: string;
}

/** How many moves the feed shows. */
export const activityLimit = 15;

/**
 * The newest `limit` moves, newest first. Moves at the same instant keep the
 * order they were made in, last made first, so a request's own history never
 * reads backwards.
 */
export function recentActivity(
  requests: readonly RequestSummary[],
  limit = activityLimit,
): ActivityEntry[] {
  const all: { entry: ActivityEntry; order: number }[] = [];
  for (const request of requests) {
    const title = requestShortTitle(request);
    request.history.forEach((move, index) => {
      all.push({
        entry: {
          requestId: request.id,
          title,
          from: move.from,
          to: move.to,
          at: move.at,
          by: move.by,
          reason: move.reason,
        },
        order: index,
      });
    });
  }
  return all
    .sort((a, b) => compareTimestamps(b.entry.at, a.entry.at) || b.order - a.order)
    .slice(0, limit)
    .map((item) => item.entry);
}
