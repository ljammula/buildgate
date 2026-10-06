// The request board's routes: list, read, the operator's write actions, the
// rejected-draft revisions and the live board stream.
import type { Http } from "@/api/http";
import { parseSseFrame, watchSse } from "@/api/sse";
import type { ApiError } from "@/domain/apiError";
import { asObject } from "@/domain/decode";
import {
  type RequestSummary,
  type RevisionDetail,
  type RevisionSummary,
  decodeRequestList,
  decodeRequestSummary,
  decodeRevisionDetail,
  decodeRevisionList,
} from "@/domain/request";

const requestPath = (id: string) => `/requests/${encodeURIComponent(id)}`;

/**
 * GET /requests: every request currently on disk, oldest-submitted first
 * (see internal/request.List). Gated with the read token, the same as the
 * run reads: the request board works with read credentials, so this sends
 * the read token and not the override token that approve and reject, which
 * are separately gated write routes, still need.
 */
export async function listRequests(http: Http, signal?: AbortSignal): Promise<RequestSummary[]> {
  return decodeRequestList(await http.getJson("/requests", "read", signal), "GET /requests");
}

/**
 * GET /requests/{id}: one request's full durable record. Gated with the
 * read token, as {@link listRequests}.
 */
export async function getRequest(
  http: Http,
  id: string,
  signal?: AbortSignal,
): Promise<RequestSummary> {
  const at = "GET /requests/{id}";
  return decodeRequestSummary(
    asObject(await http.getJson(requestPath(id), "read", signal), at),
    at,
  );
}

export interface CreateRequestOptions {
  readonly workspace: string;
  readonly text: string;
  readonly verifyCommand?: string;
  readonly fullSuiteCommand?: string;
  readonly preflightProfile?: string;
  readonly draftOracles?: boolean;
  readonly by?: string | null;
}

/**
 * POST /requests: starts a request from the console instead of a terminal
 * `factoryd submit`. Gated the same way approve and reject are
 * (internal/api.Server.authorizeRequestWrite): the override token, not the
 * start token that starting a run uses, since this can only ever create a
 * new request in `submitted`, the same class of write approve and reject
 * already are, not POST /runs' stronger authorizeStart. The server
 * enforces the actual workspace allowlist (see internal/api's
 * workspaceAllowed doc comment); this function just sends the operator's
 * choice and surfaces whatever the server decides through ApiError. An
 * empty optional string, or a false `draftOracles`, is omitted from the
 * body.
 */
export async function createRequest(
  http: Http,
  options: CreateRequestOptions,
  signal?: AbortSignal,
): Promise<RequestSummary> {
  const at = "POST /requests";
  const body = {
    workspace: options.workspace,
    text: options.text,
    ...(options.verifyCommand ? { verify_command: options.verifyCommand } : {}),
    ...(options.fullSuiteCommand ? { full_suite_command: options.fullSuiteCommand } : {}),
    ...(options.preflightProfile ? { preflight_profile: options.preflightProfile } : {}),
    ...(options.draftOracles ? { draft_oracles: true } : {}),
    ...(options.by ? { by: options.by } : {}),
  };
  return decodeRequestSummary(
    asObject(await http.sendJson("POST", "/requests", "override", body, signal), at),
    at,
  );
}

export interface ApproveRequestOptions {
  readonly by?: string | null;
  /**
   * Per relPath ("spec.md" or "tickets/NNN.spec.md"), the SHA-256 this
   * console's own last fetch of that file's content hashed to (see
   * domain/contentHash). Sent exactly as given.
   */
  readonly expectedSha256?: Readonly<Record<string, string>> | null;
}

/**
 * POST /requests/{id}/approve: advances a request out of spec_review (to
 * planning) or plan_review (to building). `by` optionally names the
 * approving operator; with neither `by` nor hashes, no body is sent at
 * all, matching the server's own fallback when By is absent.
 *
 * `expectedSha256`, when non-empty, binds the approval to the artifact
 * shown: the server refuses it if a file changed since the hash. This
 * function sends the map the caller passes, unchanged. It never computes,
 * adds or drops a hash itself, because a hash computed here could be of
 * content the operator never saw. An empty map is the same as omitting it:
 * no `expected_sha256` key, not an empty object the server would treat as
 * "expects nothing".
 */
export async function approveRequest(
  http: Http,
  id: string,
  options: ApproveRequestOptions = {},
  signal?: AbortSignal,
): Promise<RequestSummary> {
  const at = "POST /requests/{id}/approve";
  const hasBy = options.by !== undefined && options.by !== null && options.by !== "";
  const hasHashes =
    options.expectedSha256 !== undefined &&
    options.expectedSha256 !== null &&
    Object.keys(options.expectedSha256).length > 0;
  const body =
    !hasBy && !hasHashes
      ? undefined
      : {
          ...(hasBy ? { by: options.by } : {}),
          ...(hasHashes ? { expected_sha256: options.expectedSha256 } : {}),
        };
  return decodeRequestSummary(
    asObject(
      await http.sendJson("POST", `${requestPath(id)}/approve`, "override", body, signal),
      at,
    ),
    at,
  );
}

export interface RejectRequestOptions {
  readonly reason: string;
  readonly by?: string | null;
  /** "plan" or "spec": send a quarantined or halted request back instead. */
  readonly to?: string | null;
}

/**
 * POST /requests/{id}/reject: sends a request in spec_review or plan_review
 * back to the prior drafting state, with the reason appended to request.md
 * so the redraft sees it. `by` optionally names the rejecting operator,
 * symmetric with {@link approveRequest}.
 *
 * `to` ("plan" or "spec") sends a quarantined or halted request back
 * instead, routed server-side to internal/request.SendBack rather than
 * Reject (see RequestSummary.canSendBack). Omitted, it is the
 * review-state-only behaviour.
 */
export async function rejectRequest(
  http: Http,
  id: string,
  options: RejectRequestOptions,
  signal?: AbortSignal,
): Promise<RequestSummary> {
  const at = "POST /requests/{id}/reject";
  const body = {
    reason: options.reason,
    ...(options.by ? { by: options.by } : {}),
    ...(options.to ? { to: options.to } : {}),
  };
  return decodeRequestSummary(
    asObject(
      await http.sendJson("POST", `${requestPath(id)}/reject`, "override", body, signal),
      at,
    ),
    at,
  );
}

export interface UpdateRequestContentOptions {
  /**
   * The sha256 of the content the editor was opened with
   * (domain/contentHash). The server refuses with a 409 carrying
   * `current_sha256` (ApiError.currentSha256) if the file changed
   * underneath the editor since, closing the race where two edits land on
   * stale content.
   */
  readonly baseSha256?: string | null;
}

/**
 * PUT /requests/{id}/spec: overwrites spec.md in place with `content` while
 * the request is in spec_review, so the operator can edit the drafted spec
 * from the console (review-and-approve-in-place) instead of an external
 * editor. The server re-validates `content` against the same structural
 * check the drafting job itself applies, refusing with a 422
 * (ApiError.serverMessage carries the reason) rather than saving something
 * the pipeline would halt on later. Override-token gated, like approve and
 * reject. `base_sha256` is omitted when no base is given.
 */
export async function updateRequestSpec(
  http: Http,
  id: string,
  content: string,
  options: UpdateRequestContentOptions = {},
  signal?: AbortSignal,
): Promise<RequestSummary> {
  const at = "PUT /requests/{id}/spec";
  const body = {
    content,
    ...(options.baseSha256 != null ? { base_sha256: options.baseSha256 } : {}),
  };
  return decodeRequestSummary(
    asObject(await http.sendJson("PUT", `${requestPath(id)}/spec`, "override", body, signal), at),
    at,
  );
}

/**
 * PUT /requests/{id}/tickets/{n}: {@link updateRequestSpec}'s reasoning,
 * one stage later: overwrites ticket `n`'s plan file in place while the
 * request is in plan_review. See {@link updateRequestSpec} for the
 * `baseSha256` and 409 handling.
 */
export async function updateRequestTicket(
  http: Http,
  id: string,
  n: number,
  content: string,
  options: UpdateRequestContentOptions = {},
  signal?: AbortSignal,
): Promise<RequestSummary> {
  const at = "PUT /requests/{id}/tickets/{n}";
  const body = {
    content,
    ...(options.baseSha256 != null ? { base_sha256: options.baseSha256 } : {}),
  };
  return decodeRequestSummary(
    asObject(
      await http.sendJson("PUT", `${requestPath(id)}/tickets/${n}`, "override", body, signal),
      at,
    ),
    at,
  );
}

export interface RequestReasonOptions {
  readonly reason: string;
  readonly by?: string | null;
}

/**
 * POST /requests/{id}/retry: recovers a halted or quarantined request, with
 * the same identity and confirm shape as {@link rejectRequest}. 409 when
 * the request is not currently in a state retry accepts.
 */
export async function retryRequest(
  http: Http,
  id: string,
  options: RequestReasonOptions,
  signal?: AbortSignal,
): Promise<RequestSummary> {
  const at = "POST /requests/{id}/retry";
  const body = { reason: options.reason, ...(options.by ? { by: options.by } : {}) };
  return decodeRequestSummary(
    asObject(await http.sendJson("POST", `${requestPath(id)}/retry`, "override", body, signal), at),
    at,
  );
}

export interface ResumeRequestOptions {
  /** `round` (continue the lost step) or `scratch` (rebuild the ticket). */
  readonly from: string;
  readonly by?: string | null;
}

/**
 * POST /requests/{id}/resume: the operator's decision for a request in
 * resume_review. `from` is `round` (continue the lost step) or `scratch`
 * (rebuild the ticket). 409 when the request is not in resume_review or a
 * round resume's preconditions do not hold (ApiError.serverMessage carries
 * the refusal reasons); 503 when the server could not check them
 * (retryable, ApiError.isRetryable).
 */
export async function resumeRequest(
  http: Http,
  id: string,
  options: ResumeRequestOptions,
  signal?: AbortSignal,
): Promise<RequestSummary> {
  const at = "POST /requests/{id}/resume";
  const body = { from: options.from, ...(options.by ? { by: options.by } : {}) };
  return decodeRequestSummary(
    asObject(
      await http.sendJson("POST", `${requestPath(id)}/resume`, "override", body, signal),
      at,
    ),
    at,
  );
}

/**
 * POST /requests/{id}/cancel: the shape of {@link retryRequest}, for
 * abandoning a halted or quarantined request instead of recovering it.
 */
export async function cancelRequest(
  http: Http,
  id: string,
  options: RequestReasonOptions,
  signal?: AbortSignal,
): Promise<RequestSummary> {
  const at = "POST /requests/{id}/cancel";
  const body = { reason: options.reason, ...(options.by ? { by: options.by } : {}) };
  return decodeRequestSummary(
    asObject(
      await http.sendJson("POST", `${requestPath(id)}/cancel`, "override", body, signal),
      at,
    ),
    at,
  );
}

/**
 * GET /requests/{id}/revisions: every rejected-spec/plan snapshot recorded
 * for this request, oldest first. Read-token gated, like
 * {@link listRequests}.
 */
export async function listRevisions(
  http: Http,
  id: string,
  signal?: AbortSignal,
): Promise<RevisionSummary[]> {
  return decodeRevisionList(
    await http.getJson(`${requestPath(id)}/revisions`, "read", signal),
    "GET /requests/{id}/revisions",
  );
}

/**
 * GET /requests/{id}/revisions/{n}: one revision's own recorded file
 * contents, gated the same as {@link listRevisions}.
 */
export async function getRevision(
  http: Http,
  id: string,
  index: number,
  signal?: AbortSignal,
): Promise<RevisionDetail> {
  const at = "GET /requests/{id}/revisions/{n}";
  return decodeRevisionDetail(
    asObject(await http.getJson(`${requestPath(id)}/revisions/${index}`, "read", signal), at),
    at,
  );
}

export interface WatchRequestsHandlers {
  onValue(request: RequestSummary): void;
  /** A permanent failure (a 4xx); the watch has ended. */
  onError(error: ApiError): void;
  /**
   * true when a connection opened, false when an attempt ended and will be
   * retried. The board's freshness indicator reflects real connection
   * state, not just event recency: a quiet board with nothing to report can
   * go a long time between events while perfectly connected.
   */
  onConnectionChange?(live: boolean): void;
}

export interface WatchBackoff {
  readonly initialBackoffMs?: number;
  readonly maxBackoffMs?: number;
}

/**
 * GET /requests/events: every request's state as it changes. Read-token
 * gated, the token sent as a header, never in the URL. There is no terminal
 * state, so a dropped connection or a 5xx is retried with backoff
 * indefinitely (like a native EventSource) until the returned function is
 * called or a 4xx (a rotated token) ends it through `onError`. After
 * unsubscribing nothing more is delivered, and the connection is released
 * even if it was still connecting.
 */
export function watchRequests(
  http: Http,
  handlers: WatchRequestsHandlers,
  backoff: WatchBackoff = {},
): () => void {
  const at = "GET /requests/events";
  return watchSse<RequestSummary>(
    http,
    {
      path: "/requests/events",
      token: "read",
      parseFrame: (frame) => decodeRequestSummary(asObject(parseSseFrame(frame, "state"), at), at),
      onOpen: () => handlers.onConnectionChange?.(true),
    },
    {
      onValue: (request) => {
        handlers.onValue(request);
      },
      onError: (error) => {
        handlers.onError(error);
      },
      onAttemptEnded: () => handlers.onConnectionChange?.(false),
      ...(backoff.initialBackoffMs !== undefined
        ? { initialBackoffMs: backoff.initialBackoffMs }
        : {}),
      ...(backoff.maxBackoffMs !== undefined ? { maxBackoffMs: backoff.maxBackoffMs } : {}),
    },
  );
}
