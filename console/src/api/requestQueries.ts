// The request hooks screens use. Reads are queries; the board and a request's
// detail are also kept live by the request event stream; every write is a
// mutation that puts the server's answer in the cache and invalidates what
// it changed.
import {
  type QueryClient,
  type UseQueryResult,
  queryOptions,
  useMutation,
  useQuery,
  useQueryClient,
} from "@tanstack/react-query";
import { useEffect, useRef, useState } from "react";

import { useApi } from "@/api/ApiProvider";
import type { Http } from "@/api/http";
import { keepNewer } from "@/api/keepNewer";
import { requestListRefreshMs } from "@/api/polling";
import { isUnderRequest, queryKeys } from "@/api/queryKeys";
import {
  type ApproveRequestOptions,
  type CreateRequestOptions,
  type RejectRequestOptions,
  type RequestReasonOptions,
  type RetryRequestOptions,
  type ResumeRequestOptions,
  type UpdateRequestContentOptions,
  answerReadBackQuestion,
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
import { compareTimestamps } from "@/domain/elapsed";
import type { RequestSummary, RevisionDetail, RevisionSummary } from "@/domain/request";

/**
 * Whichever of two records of one request is newer. An event from the
 * stream can arrive before an older list response that was already in
 * flight; the older record must not overwrite the newer one. A tie goes
 * to the incoming record.
 */
export function newerRequest(current: RequestSummary, incoming: RequestSummary): RequestSummary {
  return compareTimestamps(incoming.updatedAt, current.updatedAt) >= 0 ? incoming : current;
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

/**
 * The request list's query: the one definition the board, the sidebar count
 * and the run list's titles share (one cache entry). A fetched list never
 * replaces a newer record already in the cache.
 */
function requestListOptions(http: Http) {
  return queryOptions({
    queryKey: queryKeys.requests.list(),
    queryFn: ({ signal }) => listRequests(http, signal),
    structuralSharing: keepNewer(mergeRequestList),
  });
}

/** One request's full detail record (`GET /requests/{id}`). */
function requestDetailOptions(http: Http, id: string) {
  return queryOptions({
    queryKey: queryKeys.requests.detail(id),
    queryFn: ({ signal }) => getRequest(http, id, signal),
  });
}

export interface RequestBoard {
  readonly query: UseQueryResult<RequestSummary[]>;
  /** True while the event stream is connected; false while it reconnects. */
  readonly live: boolean;
  /** A permanent stream failure (a 4xx): the board is then kept fresh by polling only. */
  readonly streamError: ApiError | null;
  /**
   * Connection attempts that have ended since the stream was last open; 0
   * while it is live. A board shows "disconnected" only after several, so
   * one dropped connection that reconnects at once is not an alarm.
   */
  readonly failedAttempts: number;
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
  const [failedAttempts, setFailedAttempts] = useState(0);

  const query = useQuery({
    ...requestListOptions(http),
    refetchInterval: requestListRefreshMs,
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
      onConnectionChange: (connected) => {
        setLive(connected);
        // A stream that opens again has recovered: the error banner goes.
        if (connected) setStreamError(null);
        setFailedAttempts((count) => (connected ? 0 : count + 1));
      },
    });
    return () => {
      unsubscribe();
      setLive(false);
    };
  }, [http, client]);

  return { query, live, streamError, failedAttempts };
}

/**
 * The stream a tab holds while it raises the requests' browser
 * notifications: `GET /requests/events?notifier=1`, on every screen. The
 * parameter is what tells the server a tab does the job, so the host's own
 * banner stands down for as long as this stream is open. Each frame also
 * refreshes the list cache and is handed to `onRequest`. A stream that ends
 * for good (a rotated token) is left ended: the list's own polling goes on,
 * and the host's banner comes back.
 */
export function useNotifierStream(active: boolean, onRequest: (request: RequestSummary) => void) {
  const { http } = useApi();
  const client = useQueryClient();
  const handler = useRef(onRequest);
  useEffect(() => {
    handler.current = onRequest;
  }, [onRequest]);
  useEffect(() => {
    if (!active) return undefined;
    return watchRequests(
      http,
      {
        onValue: (request) => {
          cacheRequest(client, request, false);
          handler.current(request);
        },
        onError: () => undefined,
      },
      {},
      { notifier: true },
    );
  }, [active, http, client]);
}

/**
 * The request list alone, with no event stream: for a screen that only
 * needs requests to label something else (the run list's titles). It shares
 * the board's cache entry.
 */
export function useRequests(refetchIntervalMs?: number): UseQueryResult<RequestSummary[]> {
  const { http } = useApi();
  return useQuery({
    ...requestListOptions(http),
    // Polled only when a caller asks (the sidebar's count); never an event stream.
    ...(refetchIntervalMs === undefined ? {} : { refetchInterval: refetchIntervalMs }),
  });
}

export function useRequest(id: string): UseQueryResult<RequestSummary> {
  const { http } = useApi();
  return useQuery(requestDetailOptions(http, id));
}

/**
 * Keeps one request's detail current from the board-wide event stream: an
 * event for `id` whose `updatedAt` or state differs from the cached detail
 * marks the detail stale (the stream's summary has no spec or ticket content,
 * so it never replaces the detail). A stream failure is ignored: the detail's
 * own refresh and retry remain. Pair it with `useRequest(id)`.
 */
export function useRequestDetailEvents(id: string): void {
  const { http } = useApi();
  const client = useQueryClient();
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

/**
 * A write that answers with the request's new record: the record goes
 * straight into the cache, and everything under the request (its
 * revisions) is refetched, since a state change can alter any of it.
 */
function useRequestWrite<TInput>(
  write: (input: TInput) => Promise<RequestSummary>,
  onError?: (error: ApiError, client: QueryClient) => void,
) {
  const client = useQueryClient();
  return useMutation<RequestSummary, ApiError, TInput>({
    mutationFn: write,
    ...(onError === undefined
      ? {}
      : {
          onError: (error) => {
            onError(error, client);
          },
        }),
    onSuccess: (request) => {
      cacheRequest(client, request, true);
      // Everything under the request is stale now.
      const id = request.id;
      void client.invalidateQueries({
        predicate: (query) => isUnderRequest(query.queryKey, id),
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

/**
 * Request changes and Send back. The caller passes the stage the operator
 * saw (`seen`). A 409 means the request is no longer in it: the request's
 * record and the list are read again, so the page behind the dialog shows
 * the stage it is in now, while the dialog keeps the error and what was typed.
 */
export function useRejectRequest(id: string) {
  const { http } = useApi();
  return useRequestWrite(
    (options: RejectRequestOptions) => rejectRequest(http, id, options),
    (error, client) => {
      if (error.status !== 409) return;
      void client.invalidateQueries({ queryKey: queryKeys.requests.detail(id), exact: true });
      void client.invalidateQueries({ queryKey: queryKeys.requests.list(), exact: true });
    },
  );
}

export function useRetryRequest(id: string) {
  const { http } = useApi();
  return useRequestWrite((options: RetryRequestOptions) => retryRequest(http, id, options));
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

export interface AnswerReadBackInput {
  readonly questionId: string;
  /** SHA-256 of the question text shown (sha256Hex). */
  readonly questionSha256: string;
  readonly answer: string;
  readonly by?: string | null;
}

export function useAnswerReadBack(id: string) {
  const { http } = useApi();
  return useRequestWrite(({ questionId, questionSha256, answer, by }: AnswerReadBackInput) =>
    answerReadBackQuestion(http, id, questionId, questionSha256, answer, { by: by ?? null }),
  );
}

export function useUpdateRequestTicket(id: string, ticket: number) {
  const { http } = useApi();
  return useRequestWrite(({ content, ...options }: UpdateContentInput) =>
    updateRequestTicket(http, id, ticket, content, options),
  );
}
