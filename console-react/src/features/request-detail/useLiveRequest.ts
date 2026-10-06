import { type UseQueryResult, useQuery, useQueryClient } from "@tanstack/react-query";
import { useEffect } from "react";

import { useApi } from "@/api/ApiProvider";
import { queryKeys } from "@/api/queryKeys";
import { getRequest, watchRequests } from "@/api/requests";
import type { RequestSummary } from "@/domain/request";

export interface LiveRequest {
  readonly query: UseQueryResult<RequestSummary>;
  /**
   * True only when `GET /requests/{id}` itself has returned successfully and
   * no refetch is in flight. Approve and Request changes wait for it: while a
   * refresh runs (or after one failed) the screen shows content that may be
   * older than the server's, and an approval is bound to what is shown.
   */
  readonly detailLoaded: boolean;
}

/**
 * One request's full detail, kept current by the board-wide
 * `GET /requests/events` stream, filtered to this id. An event carries only
 * the board's summary shape (no spec, ticket content or revisions), so it is
 * a signal to refetch the detail, never a replacement for it: applying it
 * directly blanked the spec on a live redraft. An event that repeats the
 * cached `updated_at` and state is a no-op. A permanent stream failure is
 * ignored: the Refresh button and the query's own retry remain, and a live
 * outage must never block reading or acting on the last loaded request.
 */
export function useLiveRequest(id: string): LiveRequest {
  const { http } = useApi();
  const client = useQueryClient();
  const query = useQuery({
    queryKey: queryKeys.requests.detail(id),
    queryFn: ({ signal }) => getRequest(http, id, signal),
  });

  useEffect(() => {
    const unsubscribe = watchRequests(http, {
      onValue: (event) => {
        if (event.id !== id) return;
        const current = client.getQueryData<RequestSummary>(queryKeys.requests.detail(id));
        if (current?.updatedAt === event.updatedAt && current.state === event.state) return;
        void client.invalidateQueries({ queryKey: queryKeys.requests.detail(id), exact: true });
      },
      onError: () => undefined,
      onConnectionChange: () => undefined,
    });
    return unsubscribe;
  }, [http, client, id]);

  return { query, detailLoaded: query.isSuccess && !query.isFetching };
}
