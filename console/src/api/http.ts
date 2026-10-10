// The one place a request leaves the console. Every resource file in api/
// goes through an Http built here, so the three rules below hold for all of
// them:
//
//   - A token travels only in an Authorization header, never in a URL: a
//     request target is what a proxy or access log persists.
//   - Every write carries Content-Type: application/json. The server's
//     loopback write check requires it, so a write without it is refused
//     even though the same request with it is allowed.
//   - No request sends Accept: text/html. The console's deep links share
//     their paths with API reads (/requests/{id}, /runs/{id}), and the
//     server picks the console shell over JSON by that header.
import { ApiError } from "@/domain/apiError";

/**
 * Which credential a route is checked against on the server
 * (internal/api's WithReadToken, WithStartToken, WithOverrideToken, and the
 * gate token). They are separate and may be distinct, so each call names the
 * one its route uses:
 *
 *   read      a read route: the read token, else the gate token
 *   gate      a request write: the gate token, else the override token (a
 *             bundle built with one keeps writing)
 *   override  POST /runs/{id}/override: the override token only. The server
 *             never accepts the gate token there, so it is never sent
 *   start     the start token only, for the same reason
 */
export type TokenKind = "read" | "start" | "override" | "gate";

export interface HttpConfig {
  /** Empty means same origin: the console is served by the factoryd it talks to. */
  readonly baseUrl: string;
  readonly readToken: string | null;
  readonly startToken: string | null;
  readonly overrideToken: string | null;
  /**
   * The operator's gate token (platform/gateToken): what lets a console
   * opened through a proxy read and do request writes.
   */
  readonly gateToken: string | null;
  /** Injected in tests. */
  readonly fetch?: typeof fetch;
}

export interface HttpHooks {
  /**
   * Called for a 403 answer to a call the gate token was sent on (a read or
   * a request write, streamed reads included). The server has no status of
   * its own for a refused gate token, so a 403 is only a reason to ask it
   * again (app/session.ts). A start-token or run-override route never gets
   * the gate token and refuses a console that holds it alone, so its 403
   * says nothing about the token and is not reported: the board polls one.
   */
  readonly onForbidden?: () => void;
}

export interface Http {
  readonly config: HttpConfig;
  url(path: string): string;
  /** GET `path` and return the parsed JSON body. Throws ApiError on a non-2xx. */
  getJson(path: string, token: TokenKind, signal?: AbortSignal): Promise<unknown>;
  /** GET `path` and return the raw body bytes. Throws ApiError on a non-2xx. */
  getBytes(path: string, token: TokenKind, signal?: AbortSignal): Promise<Uint8Array>;
  /** Send `body` as JSON and return the parsed JSON response (null for an empty one). */
  sendJson(
    method: "POST" | "PUT",
    path: string,
    token: TokenKind,
    body: unknown,
    signal?: AbortSignal,
  ): Promise<unknown>;
  /**
   * Open a streamed GET. Resolves once the response headers arrive; throws
   * ApiError (with the body read) on a non-2xx. Aborting `signal` cancels
   * the request whether it is still connecting or already streaming.
   */
  openStream(
    path: string,
    token: TokenKind,
    signal: AbortSignal,
  ): Promise<ReadableStream<Uint8Array>>;
}

function tokenFor(config: HttpConfig, kind: TokenKind): string | null {
  switch (kind) {
    // `||`, not `??`: an empty token is no token, and must not hide the other.
    case "read":
      return config.readToken || config.gateToken;
    case "start":
      return config.startToken;
    case "override":
      return config.overrideToken;
    case "gate":
      return config.gateToken || config.overrideToken;
  }
}

function authHeaders(config: HttpConfig, kind: TokenKind): Record<string, string> {
  const token = tokenFor(config, kind);
  return token ? { Authorization: `Bearer ${token}` } : {};
}

// The server's JSON is UTF-8 and declares no charset; decode the bytes as
// UTF-8 here rather than trusting a default.
async function bodyText(response: Response): Promise<string> {
  return new TextDecoder("utf-8").decode(await response.arrayBuffer());
}

export function createHttp(config: HttpConfig, hooks: HttpHooks = {}): Http {
  const doFetch: typeof fetch = config.fetch ?? ((input, init) => fetch(input, init));
  // Every method checks its response here, so the 403 hook sees every call.
  const requireSuccess = async (response: Response, token: TokenKind): Promise<void> => {
    if (response.ok) return;
    const sentGateToken =
      config.gateToken !== null &&
      config.gateToken !== "" &&
      tokenFor(config, token) === config.gateToken;
    if (response.status === 403 && sentGateToken) hooks.onForbidden?.();
    throw new ApiError(response.status, await bodyText(response));
  };
  const url = (path: string) =>
    config.baseUrl === "" ? path : new URL(path, config.baseUrl).toString();

  const get = (path: string, token: TokenKind, signal?: AbortSignal) =>
    doFetch(url(path), {
      method: "GET",
      headers: authHeaders(config, token),
      ...(signal ? { signal } : {}),
    });

  return {
    config,
    url,
    async getJson(path, token, signal) {
      const response = await get(path, token, signal);
      await requireSuccess(response, token);
      return JSON.parse(await bodyText(response)) as unknown;
    },
    async getBytes(path, token, signal) {
      const response = await get(path, token, signal);
      await requireSuccess(response, token);
      return new Uint8Array(await response.arrayBuffer());
    },
    async sendJson(method, path, token, body, signal) {
      const response = await doFetch(url(path), {
        method,
        headers: { "Content-Type": "application/json", ...authHeaders(config, token) },
        body: JSON.stringify(body),
        ...(signal ? { signal } : {}),
      });
      await requireSuccess(response, token);
      const text = await bodyText(response);
      return text.trim() === "" ? null : (JSON.parse(text) as unknown);
    },
    async openStream(path, token, signal) {
      const response = await get(path, token, signal);
      await requireSuccess(response, token);
      if (!response.body) throw new Error(`GET ${path}: the response has no body to stream`);
      return response.body;
    },
  };
}

/** Whether a failure is the caller's own cancellation, not an error to show. */
export function isAbort(error: unknown): boolean {
  return error instanceof DOMException && error.name === "AbortError";
}
