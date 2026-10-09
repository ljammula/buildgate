// The run routes of the API: reads, the two run writes, and the three
// watchers. Every request goes through the Http it is given, so a token only
// ever travels in the Authorization header.
import type { Http, TokenKind } from "@/api/http";
import { parseSseFrame, watchSse } from "@/api/sse";
import type { ApiError } from "@/domain/apiError";
import { asObject } from "@/domain/decode";
import { type Handoff, decodeHandoff } from "@/domain/handoff";
import { type SavedPrompt, decodeSavedPrompts } from "@/domain/savedPrompt";
import {
  type ProgressEvent,
  type Run,
  type RunDiff,
  decodeProgressEvent,
  decodeRun,
  decodeRunDiff,
  decodeRunList,
  runIsTerminal,
} from "@/domain/run";

const readToken: TokenKind = "read";

export async function listRuns(http: Http, signal?: AbortSignal): Promise<Run[]> {
  const at = "GET /runs";
  return decodeRunList(await http.getJson("/runs", readToken, signal), at);
}

export async function getRun(http: Http, id: string, signal?: AbortSignal): Promise<Run> {
  const at = "GET /runs/{id}";
  const json = await http.getJson(`/runs/${encodeURIComponent(id)}`, readToken, signal);
  return decodeRun(asObject(json, at), at);
}

export interface StartRunInput {
  readonly ticket: string;
  readonly workspace: string;
  readonly spec: string;
  readonly repository: string;
  readonly temporalAddress: string;
}

/**
 * Starts a run through the authenticated control-plane endpoint (start
 * token).
 *
 * Docker containment is unconditional server-side (there is no
 * host-execution opt-out anywhere in factoryd), so this sends no sandbox
 * controls of its own: a console-started run always runs sandboxed, using
 * whatever sandbox defaults the daemon's own -api-allowed-sandbox-images
 * configuration provides for API-started runs. The screen still does not
 * expose per-request sandbox controls; that is a separate, pre-existing gap.
 */
export async function startRun(http: Http, input: StartRunInput): Promise<Run> {
  const at = "POST /runs";
  const json = await http.sendJson("POST", "/runs", "start", {
    ticket: input.ticket,
    workspace: input.workspace,
    spec: input.spec,
    repository: input.repository,
    temporal_address: input.temporalAddress,
  });
  return decodeRun(asObject(json, at), at);
}

export interface OverrideRunInput {
  readonly by: string;
  readonly reason: string;
  readonly state: string;
}

/** Applies an authenticated operator override to a quarantined run (override token). */
export async function overrideRun(http: Http, id: string, input: OverrideRunInput): Promise<Run> {
  const at = "POST /runs/{id}/override";
  const json = await http.sendJson("POST", `/runs/${encodeURIComponent(id)}/override`, "override", {
    by: input.by,
    reason: input.reason,
    state: input.state,
  });
  return decodeRun(asObject(json, at), at);
}

/**
 * Fetches the full unified diff between a run's BaseSHA and ResultSHA. Throws
 * an ApiError with status 409 if the run has no result yet.
 */
export async function getRunDiff(http: Http, id: string, signal?: AbortSignal): Promise<RunDiff> {
  const at = "GET /runs/{id}/diff";
  const json = await http.getJson(`/runs/${encodeURIComponent(id)}/diff`, readToken, signal);
  return decodeRunDiff(asObject(json, at), at);
}

/**
 * GET /runs/{id}/handoff: what a stopped run left for a later attempt. Throws
 * an ApiError with status 409 for a run that has none, or whose handoff no
 * longer matches the run.
 */
export async function getRunHandoff(
  http: Http,
  id: string,
  signal?: AbortSignal,
): Promise<Handoff> {
  const at = "GET /runs/{id}/handoff";
  const json = await http.getJson(`/runs/${encodeURIComponent(id)}/handoff`, readToken, signal);
  return decodeHandoff(asObject(json, at), at);
}

/**
 * GET /runs/{id}/prompts: the prompts the run's launches were handed, oldest
 * first. Operator-only; may quote repository content.
 */
export async function getRunPrompts(
  http: Http,
  id: string,
  signal?: AbortSignal,
): Promise<SavedPrompt[]> {
  const at = "GET /runs/{id}/prompts";
  const json = await http.getJson(`/runs/${encodeURIComponent(id)}/prompts`, readToken, signal);
  return decodeSavedPrompts(asObject(json, at), at);
}

/** GET /runs/{id}/prompts/{attempt}/{name}: one prompt's text, as the build saved it. */
export async function getRunPromptText(
  http: Http,
  id: string,
  attempt: string,
  name: string,
  signal?: AbortSignal,
): Promise<string> {
  const path = `/runs/${encodeURIComponent(id)}/prompts/${encodeURIComponent(attempt)}/${encodeURIComponent(name)}`;
  return new TextDecoder().decode(await http.getBytes(path, readToken, signal));
}

export interface WatchBackoff {
  readonly initialBackoffMs?: number;
  readonly maxBackoffMs?: number;
}

export interface WatchHandlers<T> {
  readonly onValue: (value: T) => void;
  /** A permanent failure (a 4xx): the watch has ended. */
  readonly onError: (error: ApiError) => void;
  /** The watch ended by itself: a terminal value or a permanent failure. */
  readonly onDone?: () => void;
}

// An absent backoff field must stay absent: watch() applies its defaults only
// to undefined fields, and exactOptionalPropertyTypes forbids passing one.
function backoffOptions(backoff: WatchBackoff | undefined): WatchBackoff {
  return {
    ...(backoff?.initialBackoffMs !== undefined
      ? { initialBackoffMs: backoff.initialBackoffMs }
      : {}),
    ...(backoff?.maxBackoffMs !== undefined ? { maxBackoffMs: backoff.maxBackoffMs } : {}),
  };
}

function doneOption(handlers: { readonly onDone?: () => void }): { onDone?: () => void } {
  const { onDone } = handlers;
  return onDone ? { onDone } : {};
}

/**
 * Subscribes to a run's server-sent-event stream (`GET /runs/{id}/events`,
 * frames of `event: state`), reconnecting with exponential backoff for as
 * long as the run stays nonterminal. Returns the unsubscribe function.
 *
 * It is a streamed fetch rather than a native EventSource (found via a real
 * GitHub Codex App review): EventSource cannot set an Authorization header,
 * so an earlier version sent the read token as a `?token=` query parameter,
 * a reusable credential in the request target that a reverse proxy or access
 * log in front of a non-loopback deployment can persist. The streamed
 * request carries the same Authorization header every other read sends.
 *
 * That replacement traded away two things EventSource gives for free, both
 * found via later review rounds, and api/sse.ts restores both:
 *
 * - Reconnection: an intermediate version left a transient network or proxy
 *   disconnect permanently stale. Each connection attempt that ends before
 *   the run is terminal is retried with backoff.
 * - Cancellation: a quarantined run stays nonterminal indefinitely and the
 *   server emits no heartbeat, so navigating away mid-request could leave
 *   the HTTP connection and its server handler alive forever. Unsubscribing
 *   aborts the active request whether it is connecting, streaming or
 *   waiting out a backoff.
 *
 * A 4xx (403 after a rotated read token, 404 after the run is pruned) is
 * permanent, not transient: retrying it forever would leave the UI silently
 * stale, so it is delivered to `onError` once and the watch ends. Network
 * failures and 5xx are retried silently.
 */
export function watchRun(
  http: Http,
  id: string,
  handlers: WatchHandlers<Run>,
  backoff?: WatchBackoff,
): () => void {
  const at = "GET /runs/{id}/events";
  return watchSse<Run>(
    http,
    {
      path: `/runs/${encodeURIComponent(id)}/events`,
      token: readToken,
      parseFrame: (frame) => decodeRun(asObject(parseSseFrame(frame, "state"), at), at),
      closeWhen: runIsTerminal,
    },
    {
      onValue: handlers.onValue,
      onError: handlers.onError,
      isTerminal: runIsTerminal,
      ...doneOption(handlers),
      ...backoffOptions(backoff),
    },
  );
}

/**
 * The progress feed's own terminal marker: progress-contract.md's single
 * `stage: "finished", event: "end"` line, always the last line the server
 * writes for a run.
 */
export function isFinishedEvent(event: ProgressEvent): boolean {
  return event.stage === "finished" && event.event === "end";
}

/**
 * Subscribes to a run's progress feed (`GET /runs/{id}/progress`, frames of
 * `event: progress`), one per ProgressEvent, with the same reconnect,
 * cancellation and permanent-failure mechanics as watchRun.
 *
 * The server sends every existing line first, then follows the file, and
 * closes for good once the run is terminal and the file is drained, so the
 * `finished`/`end` line doubles as this stream's terminal marker. A run that
 * is already terminal still gets its full recorded history, so callers can
 * subscribe unconditionally rather than special-casing a one-shot fetch.
 */
export function watchRunProgress(
  http: Http,
  id: string,
  handlers: WatchHandlers<ProgressEvent>,
  backoff?: WatchBackoff,
): () => void {
  const at = "GET /runs/{id}/progress";
  return watchSse<ProgressEvent>(
    http,
    {
      path: `/runs/${encodeURIComponent(id)}/progress`,
      token: readToken,
      parseFrame: (frame) =>
        decodeProgressEvent(asObject(parseSseFrame(frame, "progress"), at), at),
      closeWhen: isFinishedEvent,
    },
    {
      onValue: handlers.onValue,
      onError: handlers.onError,
      isTerminal: isFinishedEvent,
      ...doneOption(handlers),
      ...backoffOptions(backoff),
    },
  );
}

export interface LogHandlers {
  readonly onChunk: (text: string) => void;
  readonly onError: (error: unknown) => void;
  /** After the stream ended or failed; never after an unsubscribe. */
  readonly onDone: () => void;
}

/**
 * `GET /runs/{id}/log?follow=1`: the run's build-loop log, tailed as it
 * grows. Delivers each chunk of text as the server writes it; `onDone` fires
 * once the server closes the stream (the run reached a terminal state with
 * nothing left to flush). Read token. Returns the unsubscribe function.
 *
 * Deliberately plain text with no SSE framing and no reconnect or backoff:
 * this is an opt-in viewer a run detail screen subscribes to only while its
 * log pane is toggled on (so a hundred open tabs are not a hundred active
 * tail streams), not a signal an operator depends on the continuity of. A
 * dropped connection just means toggling the pane off and on reissues a
 * fresh request, which per the server returns the log's current full
 * contents again, so nothing already seen is lost.
 *
 * The log is worker-authored, untrusted content (safety-contract.md's
 * Acceptance/Filesystem trust-boundary rows): the raw decoded text goes to
 * the caller with no interpretation, and the log pane renders it as plain
 * text, never as HTML.
 *
 * Unsubscribing aborts the request whether it is still connecting (found via
 * review: leaving a run page while its log tail connected kept the follow
 * connection open for good) or already streaming; nothing is delivered
 * after it.
 */
export function watchRunLog(http: Http, id: string, handlers: LogHandlers): () => void {
  const controller = new AbortController();
  const { signal } = controller;
  // Read through a function: an unsubscribe during an await is invisible to
  // control-flow narrowing.
  const aborted = (): boolean => signal.aborted;

  const run = async () => {
    let stream: ReadableStream<Uint8Array>;
    try {
      stream = await http.openStream(
        `/runs/${encodeURIComponent(id)}/log?follow=1`,
        readToken,
        signal,
      );
    } catch (error) {
      if (aborted()) return;
      handlers.onError(error);
      handlers.onDone();
      return;
    }
    if (aborted()) {
      // The response arrived after the unsubscribe: release it.
      await stream.cancel().catch(() => undefined);
      return;
    }
    const reader = stream.getReader();
    const cancel = () => {
      void reader.cancel().catch(() => undefined);
    };
    signal.addEventListener("abort", cancel);
    const decoder = new TextDecoder("utf-8");
    try {
      for (;;) {
        const { done, value } = await reader.read();
        if (aborted()) return;
        if (done) {
          const rest = decoder.decode();
          if (rest !== "") handlers.onChunk(rest);
          handlers.onDone();
          return;
        }
        const text = decoder.decode(value, { stream: true });
        if (text !== "") handlers.onChunk(text);
      }
    } catch (error) {
      if (aborted()) return;
      handlers.onError(error);
      handlers.onDone();
    } finally {
      signal.removeEventListener("abort", cancel);
      await reader.cancel().catch(() => undefined);
    }
  };
  void run();

  return () => {
    controller.abort();
  };
}
