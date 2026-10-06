import { useRequests } from "@/api/requestQueries";
import { needsHumanCount } from "@/domain/requestOrder";

/** How often the sidebar's count is refreshed: a poll, not a second event stream. */
export const needsYouPollMs = 10_000;

/**
 * How many requests wait on the operator, on every page. It reads the board's
 * request list (one shared cache entry), so the count and the board agree.
 * Null until the first answer or when the list cannot be read: no number is
 * better than a stale one.
 */
export function useNeedsYouCount(): number | null {
  const { data, isError } = useRequests(needsYouPollMs);
  if (data === undefined || isError) return null;
  return needsHumanCount(data);
}
