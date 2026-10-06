// Server-sent events over a streamed fetch, not the browser's EventSource:
// EventSource cannot set an Authorization header, so using it would put the
// read token in the URL. A streamed fetch loses two things EventSource does
// by itself, and this file provides both:
//
//   - Reconnection. `watch` retries a dropped connection with exponential
//     backoff until a value is terminal, the failure is permanent, or the
//     caller unsubscribes.
//   - Cancellation. Every attempt runs under one AbortController, so
//     unsubscribing ends the request whether it is still connecting, mid
//     stream, or waiting out a backoff. A quarantined run stays nonterminal
//     indefinitely and the server sends no heartbeat, so a watch that is
//     not cancelled holds its connection and its server handler open for
//     the life of the tab.
import { type Http, type TokenKind, isAbort } from "@/api/http";
import { ApiError } from "@/domain/apiError";

/**
 * The JSON payload of one frame: its `data:` lines joined with newlines and
 * parsed. Throws unless the frame's `event:` is `eventName` and it has data.
 */
export function parseSseFrame(frame: string, eventName: string): unknown {
  let event: string | null = null;
  const data: string[] = [];
  for (const line of frame.split(/\r\n|\n|\r/)) {
    if (line.startsWith("event:")) event = line.slice("event:".length).trimStart();
    else if (line.startsWith("data:")) data.push(line.slice("data:".length).trimStart());
  }
  if (event !== eventName || data.length === 0) {
    throw new Error(`Expected an SSE ${eventName} event with data`);
  }
  return JSON.parse(data.join("\n")) as unknown;
}

/**
 * Reads `stream` as UTF-8 and calls `onLine` for each line, whichever of
 * "\n", "\r\n" or "\r" ends it and however the bytes are chunked. Returns
 * when the stream ends or `onLine` returns false.
 */
export async function readLines(
  stream: ReadableStream<Uint8Array>,
  onLine: (line: string) => boolean,
  signal?: AbortSignal,
): Promise<void> {
  const reader = stream.getReader();
  // A read that is waiting for the next chunk only ends when the reader is
  // cancelled, so an abort cancels it here rather than relying on the
  // transport to fail the read.
  const cancel = () => {
    void reader.cancel().catch(() => undefined);
  };
  signal?.addEventListener("abort", cancel);
  const decoder = new TextDecoder("utf-8");
  let pending = "";
  const drain = (final: boolean): boolean => {
    for (;;) {
      const at = pending.search(/[\r\n]/);
      if (at === -1) break;
      // A "\r" at the very end of a chunk may be half of a "\r\n".
      if (pending[at] === "\r" && at === pending.length - 1 && !final) break;
      const width = pending[at] === "\r" && pending[at + 1] === "\n" ? 2 : 1;
      const line = pending.slice(0, at);
      pending = pending.slice(at + width);
      if (!onLine(line)) return false;
    }
    if (final && pending !== "") {
      const line = pending;
      pending = "";
      return onLine(line);
    }
    return true;
  };
  try {
    for (;;) {
      const { done, value } = await reader.read();
      if (done) {
        pending += decoder.decode();
        drain(true);
        return;
      }
      pending += decoder.decode(value, { stream: true });
      if (!drain(false)) return;
    }
  } finally {
    signal?.removeEventListener("abort", cancel);
    await reader.cancel().catch(() => undefined);
  }
}

export interface SseAttempt<T> {
  readonly path: string;
  readonly token: TokenKind;
  /** Decodes one frame's text; a throw skips that frame. */
  readonly parseFrame: (frame: string) => T;
  /** The first value this accepts ends the attempt. */
  readonly closeWhen?: (value: T) => boolean;
  /** Called once the response headers have arrived with a 2xx. */
  readonly onOpen?: () => void;
  readonly onValue: (value: T) => void;
}

/**
 * One connection: resolves when the server closes the stream or `closeWhen`
 * accepts a value; rejects with ApiError on a non-2xx, or with the network
 * error. A frame that does not parse is skipped, as one malformed event must
 * not end a stream the operator is relying on.
 */
export async function sseOnce<T>(
  http: Http,
  attempt: SseAttempt<T>,
  signal: AbortSignal,
): Promise<void> {
  const stream = await http.openStream(attempt.path, attempt.token, signal);
  attempt.onOpen?.();
  let frame = "";
  await readLines(
    stream,
    (line) => {
      if (signal.aborted) return false;
      if (line !== "") {
        frame += `${line}\n`;
        return true;
      }
      if (frame === "") return true;
      const text = frame;
      frame = "";
      let value: T;
      try {
        value = attempt.parseFrame(text);
      } catch {
        return true;
      }
      attempt.onValue(value);
      return !(attempt.closeWhen?.(value) ?? false);
    },
    signal,
  );
}

export interface WatchOptions<T> {
  /** One connection attempt; it must honour `signal`. */
  readonly connect: (onValue: (value: T) => void, signal: AbortSignal) => Promise<void>;
  readonly onValue: (value: T) => void;
  /**
   * A permanent failure (a 4xx: a rotated token, a pruned run). The watch
   * has ended; nothing more is delivered.
   */
  readonly onError: (error: ApiError) => void;
  /** A value this accepts ends the watch once its attempt finishes. */
  readonly isTerminal?: (value: T) => boolean;
  /** Called for each attempt that ended and will be retried. */
  readonly onAttemptEnded?: () => void;
  /** The watch ended by itself: a terminal value or a permanent failure. */
  readonly onDone?: () => void;
  readonly initialBackoffMs?: number;
  readonly maxBackoffMs?: number;
}

export const defaultInitialBackoffMs = 1_000;
export const defaultMaxBackoffMs = 30_000;

/**
 * Runs `connect` again and again, forwarding every value, until a value is
 * terminal, a 4xx arrives, or the returned function is called. A network
 * failure or a 5xx is transient: it is not reported, and the next attempt
 * starts after a backoff that doubles up to `maxBackoffMs`.
 *
 * Returns the unsubscribe function. After it is called nothing more is
 * delivered: no value, no error, no onDone.
 */
export function watch<T>(options: WatchOptions<T>): () => void {
  const controller = new AbortController();
  const { signal } = controller;
  const maxBackoff = options.maxBackoffMs ?? defaultMaxBackoffMs;

  const sleep = (ms: number) =>
    new Promise<void>((resolve) => {
      const timer = setTimeout(done, ms);
      function done() {
        clearTimeout(timer);
        signal.removeEventListener("abort", done);
        resolve();
      }
      signal.addEventListener("abort", done);
    });

  // One attempt: "terminal" when a value ended the watch, "retry" when the
  // connection ended or failed transiently, the error when it is permanent,
  // "cancelled" when the caller unsubscribed.
  const attempt = async (): Promise<"terminal" | "retry" | "cancelled" | ApiError> => {
    // Read through a function: the flag is set inside the callback, which
    // control-flow narrowing cannot see.
    const seen = { terminal: false };
    const sawTerminal = (): boolean => seen.terminal;
    try {
      await options.connect((value) => {
        if (signal.aborted) return;
        options.onValue(value);
        if (options.isTerminal?.(value)) seen.terminal = true;
      }, signal);
    } catch (error) {
      if (signal.aborted || isAbort(error)) return "cancelled";
      if (error instanceof ApiError && error.isPermanent) return error;
      return "retry";
    }
    if (signal.aborted) return "cancelled";
    return sawTerminal() ? "terminal" : "retry";
  };

  const pump = async () => {
    let backoff = options.initialBackoffMs ?? defaultInitialBackoffMs;
    for (;;) {
      const outcome = await attempt();
      if (outcome === "cancelled") return;
      if (outcome !== "retry") {
        if (outcome !== "terminal") options.onError(outcome);
        options.onDone?.();
        return;
      }
      options.onAttemptEnded?.();
      await sleep(backoff);
      if (signal.aborted) return;
      backoff = Math.min(Math.max(backoff * 2, 1), maxBackoff);
    }
  };
  void pump();

  return () => {
    controller.abort();
  };
}

/** `watch` over an SSE route: the common case. */
export function watchSse<T>(
  http: Http,
  attempt: Omit<SseAttempt<T>, "onValue">,
  options: Omit<WatchOptions<T>, "connect">,
): () => void {
  return watch({
    ...options,
    connect: (onValue, signal) => sseOnce(http, { ...attempt, onValue }, signal),
  });
}
