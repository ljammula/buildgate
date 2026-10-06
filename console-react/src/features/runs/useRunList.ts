import { useMemo } from "react";

import { useRequests } from "@/api/requestQueries";
import { useRuns } from "@/api/runQueries";
import type { RequestSummary } from "@/domain/request";

/**
 * How often the list re-polls GET /runs on its own (found via the
 * console-observability review, 2026-09-10): a list that only reloaded on an
 * explicit refresh left an operator watching a stale list while the factory
 * built a ticket.
 */
export const runListAutoRefreshMs = 5000;

/**
 * The run list's data: the runs (the authority, with their own error) and the
 * requests that title them. The requests are a best-effort enhancement over
 * the plain ticket-id title: their failure is tolerated silently and never
 * blanks or flags the runs list.
 */
export function useRunList() {
  const runs = useRuns(runListAutoRefreshMs);
  const requests = useRequests();
  const requestsById = useMemo(
    () => new Map<string, RequestSummary>((requests.data ?? []).map((r) => [r.id, r])),
    [requests.data],
  );
  return {
    runs,
    requestsById,
    /** True while either list is loading: the refresh controls are disabled meanwhile, so an out-of-order response cannot replace a newer one. */
    loading: runs.isFetching || requests.isFetching,
    refresh: async (): Promise<void> => {
      await Promise.allSettled([runs.refetch(), requests.refetch()]);
    },
  };
}
