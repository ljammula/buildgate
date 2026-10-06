import type { UseQueryResult } from "@tanstack/react-query";

import { useRequest, useRequestDetailEvents } from "@/api/requestQueries";
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

/** One request's full detail, kept current by the event stream (see `useRequestDetailEvents`). */
export function useLiveRequest(id: string): LiveRequest {
  const query = useRequest(id);
  useRequestDetailEvents(id);
  return { query, detailLoaded: query.isSuccess && !query.isFetching };
}
