import { type RequestStageGroup, requestStageGroupOf } from "@/domain/boardFilters";
import { type RequestSummary, requestWaitingSinceOrEnteredAt } from "@/domain/request";

// Review first: what waits on the operator leads every list that sorts.
const GROUP_RANK: Readonly<Record<RequestStageGroup, number>> = {
  review: 0,
  working: 1,
  done: 2,
  failed: 3,
  other: 4,
};

/**
 * An RFC 3339 timestamp as a sortable number; empty or unparseable sorts as
 * the oldest possible time, so one malformed field never crashes the sort.
 */
function timeKey(value: string): number {
  const ms = Date.parse(value);
  return Number.isNaN(ms) ? 0 : ms;
}

/**
 * Review states first (oldest wait first), then working, then done, failed
 * and other, each by updatedAt descending. Returns a new list.
 */
export function sortedRequests(requests: readonly RequestSummary[]): RequestSummary[] {
  return [...requests].sort((a, b) => {
    const groupA = requestStageGroupOf(a);
    const groupB = requestStageGroupOf(b);
    if (groupA !== groupB) return GROUP_RANK[groupA] - GROUP_RANK[groupB];
    if (groupA === "review") {
      return (
        timeKey(requestWaitingSinceOrEnteredAt(a)) - timeKey(requestWaitingSinceOrEnteredAt(b))
      );
    }
    return timeKey(b.updatedAt) - timeKey(a.updatedAt);
  });
}

function formatAge(ms: number): string {
  const minutes = Math.max(0, Math.floor(ms / 60_000));
  if (minutes < 60) return `${minutes}m`;
  const hours = Math.floor(minutes / 60);
  const remainder = minutes % 60;
  return remainder === 0 ? `${hours}h` : `${hours}h ${remainder}m`;
}

/** "Waiting on you · 45m" for a request that needs the operator; null otherwise. */
export function waitingBadgeLabel(request: RequestSummary, now: Date): string | null {
  if (requestStageGroupOf(request) !== "review") return null;
  const since = Date.parse(requestWaitingSinceOrEnteredAt(request));
  if (Number.isNaN(since)) return "Waiting on you";
  return `Waiting on you · ${formatAge(now.getTime() - since)}`;
}

/** How many requests wait on the operator: the tab title's count. */
export function needsHumanCount(requests: readonly RequestSummary[]): number {
  return requests.filter((r) => requestStageGroupOf(r) === "review").length;
}
