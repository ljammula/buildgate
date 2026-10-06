import { createHttp } from "@/api/http";
import { parseSseFrame, readLines, sseOnce, watch, watchSse } from "@/api/sse";
import { ApiError } from "@/domain/apiError";

const encoder = new TextEncoder();

function streamOf(chunks: string[], end: "close" | "hang" = "close"): ReadableStream<Uint8Array> {
  return new ReadableStream({
    start(controller) {
      for (const chunk of chunks) controller.enqueue(encoder.encode(chunk));
      if (end === "close") controller.close();
    },
  });
}

async function lines(chunks: string[]): Promise<string[]> {
  const out: string[] = [];
  await readLines(streamOf(chunks), (line) => {
    out.push(line);
    return true;
  });
  return out;
}

/** A fetch whose nth call is answered by the nth responder. */
function fakeFetch(responders: ((init: RequestInit) => Response | Promise<Response>)[]) {
  const calls: { url: string; init: RequestInit }[] = [];
  const fetch = vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
    const responder = responders[calls.length];
    calls.push({ url: input instanceof Request ? input.url : input.toString(), init: init ?? {} });
    if (!responder) return Promise.reject(new TypeError("no more responses"));
    return Promise.resolve(responder(init ?? {}));
  });
  return { fetch: fetch as unknown as typeof globalThis.fetch, calls };
}

const httpWith = (fetch: typeof globalThis.fetch, readToken: string | null = "read-secret") =>
  createHttp({ baseUrl: "", readToken, startToken: null, overrideToken: null, fetch });

const stateFrame = (value: unknown) => `event: state\ndata: ${JSON.stringify(value)}\n\n`;

describe("parseSseFrame", () => {
  test("parses the data of an event with the expected name", () => {
    expect(parseSseFrame('event: state\ndata: {"id":"run-1"}\n', "state")).toEqual({ id: "run-1" });
  });

  test("joins several data lines with newlines", () => {
    expect(parseSseFrame('event: state\ndata: {"a":\ndata: 1}\n', "state")).toEqual({ a: 1 });
  });

  test("refuses another event name and an event with no data", () => {
    expect(() => parseSseFrame("event: progress\ndata: {}\n", "state")).toThrow(
      "Expected an SSE state event with data",
    );
    expect(() => parseSseFrame("event: state\n", "state")).toThrow();
  });
});

describe("readLines", () => {
  test("splits on LF, CRLF and CR", async () => {
    expect(await lines(["a\nb\r\nc\rd\n"])).toEqual(["a", "b", "c", "d"]);
  });

  test("keeps blank lines, which end an event", async () => {
    expect(await lines(["a\n\nb\n"])).toEqual(["a", "", "b"]);
  });

  test("joins a line split across chunks, and a CRLF split across chunks", async () => {
    expect(await lines(["he", "llo\r", "\nwor", "ld\n"])).toEqual(["hello", "world"]);
  });

  test("joins a multi-byte character split across chunks", async () => {
    const bytes = encoder.encode("né\n");
    const stream = new ReadableStream<Uint8Array>({
      start(controller) {
        controller.enqueue(bytes.slice(0, 2));
        controller.enqueue(bytes.slice(2));
        controller.close();
      },
    });
    const out: string[] = [];
    await readLines(stream, (line) => {
      out.push(line);
      return true;
    });
    expect(out).toEqual(["né"]);
  });

  test("delivers a final line with no terminator", async () => {
    expect(await lines(["a\nlast"])).toEqual(["a", "last"]);
  });
});

describe("sseOnce", () => {
  test("sends the read token as an Authorization header, never in the URL", async () => {
    const { fetch, calls } = fakeFetch([() => new Response(streamOf([stateFrame({ n: 1 })]))]);
    const values: unknown[] = [];
    await sseOnce(
      httpWith(fetch),
      {
        path: "/runs/run-1/events",
        token: "read",
        parseFrame: (frame) => parseSseFrame(frame, "state"),
        onValue: (value) => values.push(value),
      },
      new AbortController().signal,
    );
    expect(values).toEqual([{ n: 1 }]);
    expect(calls[0]!.url).toBe("/runs/run-1/events");
    expect(calls[0]!.url).not.toContain("read-secret");
    expect(calls[0]!.init.headers).toEqual({ Authorization: "Bearer read-secret" });
  });

  test("sends no Authorization header when no token is configured", async () => {
    const { fetch, calls } = fakeFetch([() => new Response(streamOf([]))]);
    await sseOnce(
      httpWith(fetch, null),
      { path: "/requests/events", token: "read", parseFrame: (f) => f, onValue: () => undefined },
      new AbortController().signal,
    );
    expect(calls[0]!.init.headers).toEqual({});
  });

  test("stops at the first value closeWhen accepts", async () => {
    const { fetch } = fakeFetch([
      () =>
        new Response(
          streamOf([stateFrame({ n: 1 }), stateFrame({ n: 2 }), stateFrame({ n: 3 })], "hang"),
        ),
    ]);
    const values: unknown[] = [];
    await sseOnce(
      httpWith(fetch),
      {
        path: "/runs/run-1/events",
        token: "read",
        parseFrame: (frame) => parseSseFrame(frame, "state") as { n: number },
        closeWhen: (value) => value.n === 2,
        onValue: (value) => values.push(value),
      },
      new AbortController().signal,
    );
    expect(values).toEqual([{ n: 1 }, { n: 2 }]);
  });

  test("skips a frame that does not parse and keeps reading", async () => {
    const { fetch } = fakeFetch([
      () =>
        new Response(
          streamOf([
            stateFrame({ n: 1 }),
            "event: state\ndata: {not json\n\n",
            stateFrame({ n: 2 }),
          ]),
        ),
    ]);
    const values: unknown[] = [];
    await sseOnce(
      httpWith(fetch),
      {
        path: "/requests/events",
        token: "read",
        parseFrame: (frame) => parseSseFrame(frame, "state"),
        onValue: (value) => values.push(value),
      },
      new AbortController().signal,
    );
    expect(values).toEqual([{ n: 1 }, { n: 2 }]);
  });

  test("a non-2xx rejects with the status and body, and never calls onOpen", async () => {
    const { fetch } = fakeFetch([
      () => new Response('{"error": "read endpoint is not authorized"}', { status: 403 }),
    ]);
    const onOpen = vi.fn();
    await expect(
      sseOnce(
        httpWith(fetch),
        {
          path: "/runs/run-1/events",
          token: "read",
          parseFrame: (f) => f,
          onOpen,
          onValue: () => undefined,
        },
        new AbortController().signal,
      ),
    ).rejects.toMatchObject({ status: 403, serverMessage: "read endpoint is not authorized" });
    expect(onOpen).not.toHaveBeenCalled();
  });
});

describe("watch", () => {
  beforeEach(() => {
    vi.useFakeTimers();
  });
  afterEach(() => {
    vi.useRealTimers();
  });

  test("reconnects after a dropped connection, with a doubling backoff capped at the maximum", async () => {
    const attempts: number[] = [];
    const ended = vi.fn();
    const unsubscribe = watch<number>({
      connect: () => {
        attempts.push(Date.now());
        return Promise.reject(new TypeError("network down"));
      },
      onValue: () => undefined,
      onError: () => undefined,
      onAttemptEnded: ended,
      initialBackoffMs: 100,
      maxBackoffMs: 350,
    });
    await vi.advanceTimersByTimeAsync(0);
    expect(attempts).toHaveLength(1);
    await vi.advanceTimersByTimeAsync(100);
    expect(attempts).toHaveLength(2);
    await vi.advanceTimersByTimeAsync(199);
    expect(attempts).toHaveLength(2);
    await vi.advanceTimersByTimeAsync(1);
    expect(attempts).toHaveLength(3);
    await vi.advanceTimersByTimeAsync(350);
    expect(attempts).toHaveLength(4);
    await vi.advanceTimersByTimeAsync(350);
    expect(attempts).toHaveLength(5);
    expect(ended).toHaveBeenCalledTimes(5);
    unsubscribe();
  });

  test("a 5xx is transient: retried and never reported", async () => {
    const onError = vi.fn();
    let calls = 0;
    const unsubscribe = watch<number>({
      connect: () => {
        calls += 1;
        return Promise.reject(new ApiError(502, "bad gateway"));
      },
      onValue: () => undefined,
      onError,
      initialBackoffMs: 10,
    });
    await vi.advanceTimersByTimeAsync(10);
    expect(calls).toBe(2);
    expect(onError).not.toHaveBeenCalled();
    unsubscribe();
  });

  test("a 4xx is permanent: reported once, and the watch ends", async () => {
    const onError = vi.fn();
    const onDone = vi.fn();
    let calls = 0;
    watch<number>({
      connect: () => {
        calls += 1;
        return Promise.reject(new ApiError(403, '{"error": "read endpoint is not authorized"}'));
      },
      onValue: () => undefined,
      onError,
      onDone,
      initialBackoffMs: 10,
    });
    await vi.advanceTimersByTimeAsync(1000);
    expect(calls).toBe(1);
    expect(onError).toHaveBeenCalledTimes(1);
    expect((onError.mock.calls[0]![0] as ApiError).status).toBe(403);
    expect(onDone).toHaveBeenCalledTimes(1);
  });

  test("a 403 followed by nothing does not retry, but a 500 followed by a 200 recovers", async () => {
    const values: number[] = [];
    let calls = 0;
    const unsubscribe = watch<number>({
      connect: (onValue) => {
        calls += 1;
        if (calls === 1) return Promise.reject(new ApiError(500, "boom"));
        onValue(7);
        return Promise.resolve();
      },
      onValue: (value) => values.push(value),
      onError: () => undefined,
      isTerminal: (value) => value === 7,
      initialBackoffMs: 10,
    });
    await vi.advanceTimersByTimeAsync(10);
    expect(values).toEqual([7]);
    await vi.advanceTimersByTimeAsync(1000);
    expect(calls).toBe(2);
    unsubscribe();
  });

  test("stops for good once a value is terminal", async () => {
    const onDone = vi.fn();
    let calls = 0;
    watch<string>({
      connect: (onValue) => {
        calls += 1;
        onValue("slice_running");
        onValue("accepted");
        return Promise.resolve();
      },
      onValue: () => undefined,
      onError: () => undefined,
      isTerminal: (state) => state === "accepted",
      onDone,
      initialBackoffMs: 10,
    });
    await vi.advanceTimersByTimeAsync(1000);
    expect(calls).toBe(1);
    expect(onDone).toHaveBeenCalledTimes(1);
  });

  test("with no terminal state it reconnects whenever the server closes the stream", async () => {
    let calls = 0;
    const unsubscribe = watch<number>({
      connect: () => {
        calls += 1;
        return Promise.resolve();
      },
      onValue: () => undefined,
      onError: () => undefined,
      initialBackoffMs: 10,
      maxBackoffMs: 10,
    });
    await vi.advanceTimersByTimeAsync(35);
    expect(calls).toBe(4);
    unsubscribe();
  });

  test("unsubscribing during a backoff cancels the pending retry", async () => {
    let calls = 0;
    const unsubscribe = watch<number>({
      connect: () => {
        calls += 1;
        return Promise.reject(new TypeError("network down"));
      },
      onValue: () => undefined,
      onError: () => undefined,
      initialBackoffMs: 100,
    });
    await vi.advanceTimersByTimeAsync(50);
    unsubscribe();
    await vi.advanceTimersByTimeAsync(10_000);
    expect(calls).toBe(1);
    expect(vi.getTimerCount()).toBe(0);
  });

  test("unsubscribing while still connecting aborts the request and delivers nothing later", async () => {
    let seenSignal: AbortSignal | null = null;
    let resolveFetch: (response: Response) => void = () => undefined;
    const fetch = vi.fn((_input: RequestInfo | URL, init?: RequestInit) => {
      seenSignal = init?.signal ?? null;
      return new Promise<Response>((resolve) => {
        resolveFetch = resolve;
      });
    }) as unknown as typeof globalThis.fetch;
    const onValue = vi.fn();
    const onOpen = vi.fn();
    const unsubscribe = watchSse(
      httpWith(fetch),
      { path: "/requests/events", token: "read", parseFrame: (f) => f, onOpen },
      { onValue, onError: () => undefined, initialBackoffMs: 10 },
    );
    await vi.advanceTimersByTimeAsync(0);
    unsubscribe();
    expect(seenSignal!.aborted).toBe(true);
    // The response arriving after the cancel must attach nothing.
    resolveFetch(new Response(streamOf([stateFrame({ n: 1 })])));
    await vi.advanceTimersByTimeAsync(1000);
    expect(onValue).not.toHaveBeenCalled();
    expect(fetch).toHaveBeenCalledTimes(1);
  });

  test("unsubscribing mid-stream cancels the open connection", async () => {
    let cancelled = false;
    const body = new ReadableStream<Uint8Array>({
      start(controller) {
        controller.enqueue(encoder.encode(stateFrame({ n: 1 })));
      },
      cancel() {
        cancelled = true;
      },
    });
    // This transport never fails a pending read on abort: the cancel must
    // come from the reader.
    const fetch = vi.fn(() =>
      Promise.resolve(new Response(body)),
    ) as unknown as typeof globalThis.fetch;
    const values: unknown[] = [];
    const unsubscribe = watchSse(
      httpWith(fetch),
      { path: "/requests/events", token: "read", parseFrame: (f) => parseSseFrame(f, "state") },
      { onValue: (value) => values.push(value), onError: () => undefined, initialBackoffMs: 10 },
    );
    await vi.advanceTimersByTimeAsync(0);
    expect(values).toEqual([{ n: 1 }]);
    unsubscribe();
    await vi.advanceTimersByTimeAsync(1000);
    expect(cancelled).toBe(true);
    expect(fetch).toHaveBeenCalledTimes(1);
  });

  test("onOpen and onAttemptEnded report the connection going live and dropping", async () => {
    const events: string[] = [];
    const { fetch } = fakeFetch([
      () => new Response(streamOf([stateFrame({ n: 1 })])),
      () => new Response(streamOf([stateFrame({ n: 2 })])),
    ]);
    const unsubscribe = watchSse(
      httpWith(fetch),
      {
        path: "/requests/events",
        token: "read",
        parseFrame: (f) => parseSseFrame(f, "state"),
        onOpen: () => events.push("open"),
      },
      {
        onValue: () => events.push("value"),
        onError: () => undefined,
        onAttemptEnded: () => events.push("ended"),
        initialBackoffMs: 10,
      },
    );
    await vi.advanceTimersByTimeAsync(10);
    expect(events).toEqual(["open", "value", "ended", "open", "value", "ended"]);
    unsubscribe();
  });
});
