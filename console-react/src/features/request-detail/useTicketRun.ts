import { type UseQueryResult, useQuery } from "@tanstack/react-query";

import { useApi } from "@/api/ApiProvider";
import { queryKeys } from "@/api/queryKeys";
import { getRun } from "@/api/runs";
import type { Run } from "@/domain/run";

/**
 * One ticket's own run, with a plain `GET /runs/{id}` and no event stream.
 * The server's ticket JSON carries no run state, so the card fetches it: a
 * handful of tickets on one request can afford it, a board of requests
 * cannot, which is why the board rolls tickets up from runId alone. Keyed by
 * run id, so a retry landing a new run under the same ticket index fetches
 * the new run rather than showing the old one's state.
 */
export function useTicketRun(runId: string): UseQueryResult<Run> {
  const { http } = useApi();
  return useQuery({
    queryKey: queryKeys.runs.detail(runId),
    queryFn: ({ signal }) => getRun(http, runId, signal),
    enabled: runId !== "",
  });
}
