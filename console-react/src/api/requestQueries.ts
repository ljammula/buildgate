// The request hooks screens use. Reads are queries; the board and a request's
// detail are also kept live by the request event stream; every write is a
// mutation that puts the server's answer in the cache and invalidates what
// it changed.
import {
  type QueryClient,
  type UseQueryResult,
  useMutation,
  useQuery,
  useQueryClient,
} from "@tanstack/react-query";
import { useEffect, useState } from "react";

import { useApi } from "@/api/ApiProvider";
import {
  type FetchedOracleFile,
  type PutOracleRunCommandOptions,
  getRequestOracle,
  getRequestOracleFile,
  getRequestTicketOracle,
  getRequestTicketOracleFile,
  putRequestOracleRunCommand,
} from "@/api/oracle";
import { queryKeys } from "@/api/queryKeys";
import {
  type ApproveRequestOptions,
  type CreateRequestOptions,
  type RejectRequestOptions,
  type RequestReasonOptions,
  type ResumeRequestOptions,
  type UpdateRequestContentOptions,
  approveRequest,
  cancelRequest,
  createRequest,
  getRequest,
  getRevision,
  listRequests,
  listRevisions,
  rejectRequest,
  resumeRequest,
  retryRequest,
  updateRequestSpec,
  updateRequestTicket,
  watchRequests,
} from "@/api/requests";
import type { ApiError } from "@/domain/apiError";
import type { OracleListing } from "@/domain/oracle";
import type { RequestSummary, RevisionDetail, RevisionSummary } from "@/domain/request";

/** The board's poll, as a fallback under the event stream. */
export const requestListRefreshMs = 5_000;

/**
 * Whichever of two records of one request is newer. An event from the
 * stream can arrive before an older list response that was already in
 * flight; the older record must not overwrite the newer one. `updated_at`
 * is RFC 3339 UTC, so it orders as text.
 */
export function newerRequest(current: RequestSummary, incoming: RequestSummary): RequestSummary {
  return incoming.updatedAt >= current.updatedAt ? incoming : current;
}

/** A fetched list with any newer record already in the cache kept in place. */
export function mergeRequestList(
  cached: readonly RequestSummary[] | undefined,
  fetched: readonly RequestSummary[],
): RequestSummary[] {
  if (!cached) return [...fetched];
  const byId = new Map(cached.map((request) => [request.id, request]));
  return fetched.map((request) => {
    const current = byId.get(request.id);
    return current ? newerRequest(current, request) : request;
  });
}

/** The list with one request replaced, or appended when it is new. */
export function upsertRequest(
  list: readonly RequestSummary[] | undefined,
  incoming: RequestSummary,
): RequestSummary[] {
  if (!list) return [incoming];
  if (!list.some((request) => request.id === incoming.id)) return [...list, incoming];
  return list.map((request) =>
    request.id === incoming.id ? newerRequest(request, incoming) : request,
  );
}

/**
 * Puts one request record into both caches. The board's record is a summary
 * and the detail's carries more (spec text, ticket content), so a summary
 * from the stream marks the detail stale instead of replacing it.
 */
function cacheRequest(client: QueryClient, request: RequestSummary, isDetail: boolean): void {
  client.setQueryData<RequestSummary[]>(queryKeys.requests.list(), (list) =>
    upsertRequest(list, request),
  );
  if (isDetail) {
    client.setQueryData(queryKeys.requests.detail(request.id), request);
  } else {
    void client.invalidateQueries({ queryKey: queryKeys.requests.detail(request.id), exact: true });
  }
}

export interface RequestBoard {
  readonly query: UseQueryResult<RequestSummary[]>;
  /** True while the event stream is connected; false while it reconnects. */
  readonly live: boolean;
  /** A permanent stream failure (a 4xx): the board is then kept fresh by polling only. */
  readonly streamError: ApiError | null;
}

/**
 * The request board: the list, refreshed on an interval and kept current by
 * `GET /requests/events`. A failed refresh keeps the last list
 * (`query.data`) alongside the error (`query.error`).
 */
export function useRequestBoard(): RequestBoard {
  const { http } = useApi();
  const client = useQueryClient();
  const [live, setLive] = useState(false);
  const [streamError, setStreamError] = useState<ApiError | null>(null);

  const query = useQuery({
    queryKey: queryKeys.requests.list(),
    queryFn: ({ signal }) => listRequests(http, signal),
    refetchInterval: requestListRefreshMs,
    structuralSharing: (cached, fetched) =>
      mergeRequestList(
        cached as RequestSummary[] | undefined,
        fetched as readonly RequestSummary[],
      ),
  });

  useEffect(() => {
    const unsubscribe = watchRequests(http, {
      onValue: (request) => {
        cacheRequest(client, request, false);
      },
      onError: (error) => {
        setLive(false);
        setStreamError(error);
      },
      onConnectionChange: setLive,
    });
    return () => {
      unsubscribe();
      setLive(false);
    };
  }, [http, client]);

  return { query, live, streamError };
}

export function useRequest(id: string): UseQueryResult<RequestSummary> {
  const { http } = useApi();
  return useQuery({
    queryKey: queryKeys.requests.detail(id),
    queryFn: ({ signal }) => getRequest(http, id, signal),
  });
}

export function useRequestRevisions(id: string, enabled = true): UseQueryResult<RevisionSummary[]> {
  const { http } = useApi();
  return useQuery({
    queryKey: queryKeys.requests.revisions(id),
    queryFn: ({ signal }) => listRevisions(http, id, signal),
    enabled,
  });
}

export function useRequestRevision(
  id: string,
  index: number | null,
): UseQueryResult<RevisionDetail> {
  const { http } = useApi();
  return useQuery({
    queryKey: queryKeys.requests.revision(id, index ?? -1),
    queryFn: ({ signal }) => getRevision(http, id, index ?? -1, signal),
    enabled: index !== null,
    // A revision is a snapshot: it never changes.
    staleTime: Infinity,
  });
}

export function useRequestOracle(id: string, enabled = true): UseQueryResult<OracleListing> {
  const { http } = useApi();
  return useQuery({
    queryKey: queryKeys.requests.oracle(id),
    queryFn: ({ signal }) => getRequestOracle(http, id, signal),
    enabled,
  });
}

export function useRequestOracleFile(
  id: string,
  name: string | null,
): UseQueryResult<FetchedOracleFile> {
  const { http } = useApi();
  return useQuery({
    queryKey: queryKeys.requests.oracleFile(id, name ?? ""),
    queryFn: ({ signal }) => getRequestOracleFile(http, id, name ?? "", signal),
    enabled: name !== null,
  });
}

export function useTicketOracle(
  id: string,
  ticket: number,
  enabled = true,
): UseQueryResult<OracleListing> {
  const { http } = useApi();
  return useQuery({
    queryKey: queryKeys.requests.ticketOracle(id, ticket),
    queryFn: ({ signal }) => getRequestTicketOracle(http, id, ticket, signal),
    enabled,
  });
}

export function useTicketOracleFile(
  id: string,
  ticket: number,
  name: string | null,
): UseQueryResult<FetchedOracleFile> {
  const { http } = useApi();
  return useQuery({
    queryKey: queryKeys.requests.ticketOracleFile(id, ticket, name ?? ""),
    queryFn: ({ signal }) => getRequestTicketOracleFile(http, id, ticket, name ?? "", signal),
    enabled: name !== null,
  });
}

/**
 * A write that answers with the request's new record: the record goes
 * straight into the cache, and everything under the request (revisions,
 * oracle listings) is refetched, since a state change can alter any of it.
 */
function useRequestWrite<TInput>(write: (input: TInput) => Promise<RequestSummary>) {
  const client = useQueryClient();
  return useMutation<RequestSummary, ApiError, TInput>({
    mutationFn: write,
    onSuccess: (request) => {
      cacheRequest(client, request, true);
      void client.invalidateQueries({
        queryKey: queryKeys.requests.detail(request.id),
        predicate: (query) => query.queryKey.length > 3,
      });
    },
  });
}

export function useCreateRequest() {
  const { http } = useApi();
  return useRequestWrite((options: CreateRequestOptions) => createRequest(http, options));
}

export function useApproveRequest(id: string) {
  const { http } = useApi();
  return useRequestWrite((options: ApproveRequestOptions) => approveRequest(http, id, options));
}

export function useRejectRequest(id: string) {
  const { http } = useApi();
  return useRequestWrite((options: RejectRequestOptions) => rejectRequest(http, id, options));
}

export function useRetryRequest(id: string) {
  const { http } = useApi();
  return useRequestWrite((options: RequestReasonOptions) => retryRequest(http, id, options));
}

export function useResumeRequest(id: string) {
  const { http } = useApi();
  return useRequestWrite((options: ResumeRequestOptions) => resumeRequest(http, id, options));
}

export function useCancelRequest(id: string) {
  const { http } = useApi();
  return useRequestWrite((options: RequestReasonOptions) => cancelRequest(http, id, options));
}

export interface UpdateContentInput extends UpdateRequestContentOptions {
  readonly content: string;
}

export function useUpdateRequestSpec(id: string) {
  const { http } = useApi();
  return useRequestWrite(({ content, ...options }: UpdateContentInput) =>
    updateRequestSpec(http, id, content, options),
  );
}

export function useUpdateRequestTicket(id: string, ticket: number) {
  const { http } = useApi();
  return useRequestWrite(({ content, ...options }: UpdateContentInput) =>
    updateRequestTicket(http, id, ticket, content, options),
  );
}

export interface PutOracleRunCommandInput extends PutOracleRunCommandOptions {
  readonly content: string;
}

export function usePutOracleRunCommand(id: string) {
  const { http } = useApi();
  const client = useQueryClient();
  return useMutation<undefined, ApiError, PutOracleRunCommandInput>({
    mutationFn: async ({ content, ...options }) => {
      await putRequestOracleRunCommand(http, id, content, options);
      return undefined;
    },
    onSuccess: () => {
      void client.invalidateQueries({ queryKey: queryKeys.requests.detail(id) });
    },
  });
}
