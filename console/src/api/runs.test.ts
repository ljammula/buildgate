import { createHttp } from "@/api/http";
import { listProjects } from "@/api/projects";
import {
  getRun,
  getRunDiff,
  isFinishedEvent,
  listRuns,
  overrideRun,
  startRun,
  watchRun,
  watchRunLog,
  watchRunProgress,
} from "@/api/runs";
import { ApiError } from "@/domain/apiError";
import { type ProgressEvent, type Run, runIsTerminal } from "@/domain/run";
import { readFixtureText } from "@/test/fixtures";

interface Call {
  readonly url: string;
  readonly init: RequestInit;
}

type Responder = (call: Call) => Response | Promise<Response>;

/** A fetch that records every request and answers each with `respond`. */
function recordingFetch(respond: Responder) {
  const calls: Call[] = [];
  const fetch = vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
    const call = { url: input as string, init: init ?? {} };
    calls.push(call);
    return Promise.resolve(respond(call));
  });
  return { fetch: fetch as unknown as typeof globalThis.fetch, calls };
}

const tokens = {
  readToken: "read-t",
  startToken: "start-t",
  overrideToken: "override-t",
  gateToken: "gate-t",
};

function httpFor(fetch: typeof globalThis.fetch, baseUrl = "") {
  return createHttp({ baseUrl, ...tokens, fetch });
}

function noTokenHttp(fetch: typeof globalThis.fetch) {
  return createHttp({
    baseUrl: "http://factory.test",
    readToken: null,
    startToken: null,
    overrideToken: null,
    gateToken: null,
    fetch,
  });
}

const json = (body: string, status = 200) =>
  new Response(body, { status, headers: { "content-type": "application/json" } });

const encoder = new TextEncoder();

function bodyOf(chunks: string[], end: "close" | "hang" = "close", onCancel?: () => void) {
  return new ReadableStream<Uint8Array>({
    start(controller) {
      for (const chunk of chunks) controller.enqueue(encoder.encode(chunk));
      if (end === "close") controller.close();
    },
    cancel() {
      onCancel?.();
    },
  });
}

const compact = (text: string) => JSON.stringify(JSON.parse(text));
const stateFrame = (text: string) => `event: state\ndata: ${compact(text)}\n\n`;

function runWith(patch: Record<string, unknown>, fixture = "api/run-accepted.json"): string {
  return JSON.stringify({ ...(JSON.parse(readFixtureText(fixture)) as object), ...patch });
}

const authOf = (call: Call) => (call.init.headers as Record<string, string>)["Authorization"];

describe("listRuns", () => {
  test("listRuns parses in-progress, accepted, and quarantined shapes", async () => {
    const { fetch, calls } = recordingFetch(() => json(readFixtureText("api/runs.json")));
    const runs = await listRuns(httpFor(fetch, "http://factory.test"));

    expect(calls).toHaveLength(1);
    expect(calls[0]?.url).toBe("http://factory.test/runs");
    expect(runs).toHaveLength(4);
    expect(runs[0]?.state).toBe("slice_running");
    expect(runs[0]?.changedFiles).toBeNull();
    expect(runs[0]?.resultSha).toBeNull();

    const accepted = runs[2];
    expect(accepted?.id).toBe("run-accepted");
    expect(accepted?.resultSha).toBe("2".repeat(40));
    expect(accepted?.committedByFactoryd).toBe(true);
    expect(accepted?.gateResults.length).toBeGreaterThan(0);

    // Not terminal: an operator override can still move a quarantined run to
    // accepted/halted later, so a screen must keep watching it.
    expect(runs[1]?.state).toBe("quarantined");
    expect(runs[1]?.changedFiles).toBeNull();
  });

  test("an empty changed_files list decodes as empty, distinct from absent", async () => {
    const { fetch } = recordingFetch(() => json(`[${runWith({ changed_files: [] })}]`));
    const runs = await listRuns(httpFor(fetch));
    expect(runs[0]?.changedFiles).toEqual([]);
  });

  test("empty baseUrl (same-origin console) sends relative paths", async () => {
    const { fetch, calls } = recordingFetch(() => json("[]"));
    const runs = await listRuns(httpFor(fetch));
    expect(runs).toEqual([]);
    expect(calls[0]?.url).toBe("/runs");
  });

  test("sends GET /runs with only the read token and no body", async () => {
    const { fetch, calls } = recordingFetch(() => json("[]"));
    await listRuns(httpFor(fetch));
    expect(calls).toHaveLength(1);
    expect(calls[0]?.init.method).toBe("GET");
    expect(calls[0]?.init.headers).toEqual({ Authorization: "Bearer read-t" });
    expect(calls[0]?.init.body).toBeUndefined();
  });

  test("a halted run is terminal only once halt_confirmed is true", async () => {
    const halted = (confirmed: boolean) =>
      runWith({ state: "halted", halt_confirmed: confirmed, result_sha: undefined });
    const { fetch } = recordingFetch(() => json(`[${halted(false)},${halted(true)}]`));
    const runs = await listRuns(httpFor(fetch));
    expect(runs[0]?.haltConfirmed).toBe(false);
    expect(runs[0] && runIsTerminal(runs[0])).toBe(false);
    expect(runs[1]?.haltConfirmed).toBe(true);
    expect(runs[1] && runIsTerminal(runs[1])).toBe(true);
  });
});

describe("getRun", () => {
  test("getRun fetches an encoded run id and parses the response", async () => {
    const { fetch, calls } = recordingFetch(() => json(readFixtureText("api/run-accepted.json")));
    const run = await getRun(httpFor(fetch, "http://factory.test"), "run-accepted");
    expect(calls[0]?.url).toBe("http://factory.test/runs/run-accepted");
    expect(run.id).toBe("run-accepted");
    expect(run.gateResults[0]?.passed).toBe(true);
  });

  test("encodes an id with reserved characters and sends the read token", async () => {
    const { fetch, calls } = recordingFetch(() => json(readFixtureText("api/run-accepted.json")));
    await getRun(httpFor(fetch), "a/b c?d");
    expect(calls[0]?.url).toBe("/runs/a%2Fb%20c%3Fd");
    expect(authOf(calls[0] as Call)).toBe("Bearer read-t");
  });

  test("decodes every committed run fixture", async () => {
    const { fetch } = recordingFetch((call) => {
      const name = call.url.slice("/runs/".length);
      return json(readFixtureText(`api/${name}.json`));
    });
    const http = httpFor(fetch);
    expect((await getRun(http, "run-running")).state).toBe("slice_running");
    expect((await getRun(http, "run-quarantined")).state).toBe("quarantined");
    expect((await getRun(http, "run-every-field")).id).toBe("run-every-field");
  });

  test("a 404 rejects with an ApiError carrying the server message", async () => {
    const { fetch } = recordingFetch(() => json(readFixtureText("api/error-not-found.json"), 404));
    const error = await getRun(httpFor(fetch), "run-missing").catch((e: unknown) => e);
    expect(error).toBeInstanceOf(ApiError);
    expect((error as ApiError).status).toBe(404);
  });
});

describe("startRun and overrideRun", () => {
  test("startRun posts every key to /runs with the start token", async () => {
    const { fetch, calls } = recordingFetch(() => json(readFixtureText("api/run-running.json")));
    const run = await startRun(httpFor(fetch), {
      ticket: "t-1",
      workspace: "/repos/app",
      spec: "/specs/a.md",
      repository: "acme/app",
      temporalAddress: "",
    });
    expect(run.id).toBe("run-running");
    expect(calls).toHaveLength(1);
    expect(calls[0]?.url).toBe("/runs");
    expect(calls[0]?.init.method).toBe("POST");
    expect(calls[0]?.init.headers).toEqual({
      "Content-Type": "application/json",
      Authorization: "Bearer start-t",
    });
    expect(JSON.parse(calls[0]?.init.body as string)).toEqual({
      ticket: "t-1",
      workspace: "/repos/app",
      spec: "/specs/a.md",
      repository: "acme/app",
      temporal_address: "",
    });
  });

  test("overrideRun posts by, reason and state to the encoded id with the override token", async () => {
    const { fetch, calls } = recordingFetch(() => json(readFixtureText("api/run-accepted.json")));
    const run = await overrideRun(httpFor(fetch), "run/1", {
      by: "op@example.com",
      reason: "reviewed",
      state: "accepted",
    });
    expect(run.state).toBe("accepted");
    expect(calls[0]?.url).toBe("/runs/run%2F1/override");
    expect(calls[0]?.init.method).toBe("POST");
    expect(calls[0]?.init.headers).toEqual({
      "Content-Type": "application/json",
      Authorization: "Bearer override-t",
    });
    expect(JSON.parse(calls[0]?.init.body as string)).toEqual({
      by: "op@example.com",
      reason: "reviewed",
      state: "accepted",
    });
  });
});

describe("getRunDiff", () => {
  test("fetches the diff with the read token and decodes the fixture", async () => {
    const { fetch, calls } = recordingFetch(() => json(readFixtureText("api/run-diff.json")));
    const diff = await getRunDiff(httpFor(fetch), "run-accepted");
    expect(calls[0]?.url).toBe("/runs/run-accepted/diff");
    expect(calls[0]?.init.method).toBe("GET");
    expect(authOf(calls[0] as Call)).toBe("Bearer read-t");
    expect(diff.truncated).toBe(false);
    expect(diff.diff).toContain("diff --git a/checkout/idempotency.go");
  });

  test("a 409 (no result yet) rejects with status 409", async () => {
    const { fetch } = recordingFetch(() => json('{"error":"run has no result"}', 409));
    await expect(getRunDiff(httpFor(fetch), "run-1")).rejects.toMatchObject({ status: 409 });
  });
});

describe("read token routes", () => {
  test("read routes send the configured read token as a bearer header", async () => {
    const seen: Record<string, string | undefined> = {};
    const { fetch } = recordingFetch((call) => {
      seen[call.url] = authOf(call);
      if (call.url === "/runs/run-1") return json(readFixtureText("api/run-accepted.json"));
      if (call.url === "/runs/run-1/diff") return json('{"diff":"diff --git a b\\n"}');
      return json("[]");
    });
    const http = httpFor(fetch);
    await listProjects(http);
    await listRuns(http);
    await getRun(http, "run-1");
    await getRunDiff(http, "run-1");
    expect(seen["/projects"]).toBe("Bearer read-t");
    expect(seen["/runs"]).toBe("Bearer read-t");
    expect(seen["/runs/run-1"]).toBe("Bearer read-t");
    expect(seen["/runs/run-1/diff"]).toBe("Bearer read-t");
  });

  test("read routes send no Authorization header when no read token is configured", async () => {
    const { fetch, calls } = recordingFetch(() => json("[]"));
    await listRuns(noTokenHttp(fetch));
    expect(calls[0]?.init.headers).toEqual({});
  });
});

describe("watchers", () => {
  beforeEach(() => {
    vi.useFakeTimers();
  });
  afterEach(() => {
    vi.useRealTimers();
  });

  const backoff = { initialBackoffMs: 1, maxBackoffMs: 5 };

  function collect<T>() {
    const values: T[] = [];
    const errors: ApiError[] = [];
    const done = vi.fn();
    return {
      values,
      errors,
      done,
      handlers: {
        onValue: (value: T) => values.push(value),
        onError: (error: ApiError) => errors.push(error),
        onDone: done,
      },
    };
  }

  test("watchRun sends the read token as a header, never in the request URL", async () => {
    const { fetch, calls } = recordingFetch(
      () =>
        new Response(bodyOf([stateFrame(readFixtureText("api/run-accepted.json"))]), {
          status: 200,
        }),
    );
    const sink = collect<Run>();
    watchRun(httpFor(fetch), "run-1", sink.handlers, backoff);
    await vi.advanceTimersByTimeAsync(0);

    expect(sink.values.map((run) => run.id)).toEqual(["run-accepted"]);
    // The regression this covers (a real review finding): an earlier version
    // placed the read token in the query string, which a proxy or access log
    // can persist far beyond this one request.
    expect(calls[0]?.url).toBe("/runs/run-1/events");
    expect(calls[0]?.url).not.toContain("?");
    expect(calls[0]?.init.method).toBe("GET");
    expect(calls[0]?.init.headers).toEqual({ Authorization: "Bearer read-t" });
  });

  test("watchRun retries with backoff on a non-2xx response instead of throwing", async () => {
    const { fetch, calls } = recordingFetch(() => new Response("server error", { status: 500 }));
    const sink = collect<Run>();
    const stop = watchRun(httpFor(fetch), "run-1", sink.handlers, backoff);
    await vi.advanceTimersByTimeAsync(50);
    stop();
    // A single failure must not end the subscription for good.
    expect(calls.length).toBeGreaterThan(1);
    expect(sink.errors).toEqual([]);
  });

  test("watchRun retries with backoff if the connection closes before a terminal state", async () => {
    const { fetch, calls } = recordingFetch(() => new Response("", { status: 200 }));
    const sink = collect<Run>();
    const stop = watchRun(httpFor(fetch), "run-1", sink.handlers, backoff);
    await vi.advanceTimersByTimeAsync(50);
    stop();
    expect(calls.length).toBeGreaterThan(1);
    expect(sink.done).not.toHaveBeenCalled();
  });

  test("watchRun stops retrying once a terminal state is observed", async () => {
    const { fetch, calls } = recordingFetch(
      () =>
        new Response(bodyOf([stateFrame(readFixtureText("api/run-accepted.json"))]), {
          status: 200,
        }),
    );
    const sink = collect<Run>();
    watchRun(httpFor(fetch), "run-1", sink.handlers, backoff);
    await vi.advanceTimersByTimeAsync(50);
    expect(sink.values.map((run) => run.id)).toEqual(["run-accepted"]);
    // A terminal state stops the subscription for good, not just its first event.
    expect(calls).toHaveLength(1);
    expect(sink.done).toHaveBeenCalledTimes(1);
    expect(sink.errors).toEqual([]);
  });

  test("a halted but unconfirmed run does not end the watch", async () => {
    const unconfirmed = runWith({ state: "halted", halt_confirmed: false });
    const { fetch, calls } = recordingFetch(
      () => new Response(bodyOf([stateFrame(unconfirmed)]), { status: 200 }),
    );
    const sink = collect<Run>();
    const stop = watchRun(httpFor(fetch), "run-1", sink.handlers, backoff);
    await vi.advanceTimersByTimeAsync(50);
    stop();
    expect(calls.length).toBeGreaterThan(1);
    expect(sink.done).not.toHaveBeenCalled();
  });

  test("watchRun cancels the active inner connection when its subscription is cancelled", async () => {
    let cancelled = false;
    const { fetch } = recordingFetch(
      () =>
        new Response(
          bodyOf([], "hang", () => {
            cancelled = true;
          }),
          { status: 200 },
        ),
    );
    const sink = collect<Run>();
    const stop = watchRun(httpFor(fetch), "run-1", sink.handlers);
    await vi.advanceTimersByTimeAsync(0);
    stop();
    await vi.advanceTimersByTimeAsync(0);
    // The connection's own body is cancelled, not left open.
    expect(cancelled).toBe(true);
  });

  test("watchRun forwards a permanent 4xx failure instead of retrying it", async () => {
    const { fetch, calls } = recordingFetch(() => new Response("forbidden", { status: 403 }));
    const sink = collect<Run>();
    watchRun(httpFor(fetch), "run-1", sink.handlers, backoff);
    await vi.advanceTimersByTimeAsync(100);
    // A rotated read token (403) or a pruned run (404) must not leave the
    // console silently stale: exactly one attempt, one error, then done.
    expect(calls).toHaveLength(1);
    expect(sink.errors).toHaveLength(1);
    expect(sink.errors[0]?.status).toBe(403);
    expect(sink.done).toHaveBeenCalledTimes(1);
  });

  test("watchRun delivers nothing after unsubscribe", async () => {
    const frame = stateFrame(runWith({ state: "slice_running" }));
    let release: () => void = () => undefined;
    const gate = new Promise<void>((resolve) => {
      release = resolve;
    });
    const { fetch } = recordingFetch(async () => {
      await gate;
      return new Response(bodyOf([frame]), { status: 200 });
    });
    const sink = collect<Run>();
    const stop = watchRun(httpFor(fetch), "run-1", sink.handlers, backoff);
    stop();
    release();
    await vi.advanceTimersByTimeAsync(50);
    expect(sink.values).toEqual([]);
    expect(sink.errors).toEqual([]);
    expect(sink.done).not.toHaveBeenCalled();
  });

  test("watchRun serves the committed event fixture", async () => {
    const { fetch } = recordingFetch(
      () => new Response(bodyOf([readFixtureText("api/run-events.sse")]), { status: 200 }),
    );
    const sink = collect<Run>();
    watchRun(httpFor(fetch), "run-accepted", sink.handlers, backoff);
    await vi.advanceTimersByTimeAsync(0);
    expect(sink.values).toHaveLength(1);
    expect(sink.values[0]?.state).toBe("accepted");
    expect(sink.values[0]?.resultSha).toBe("2".repeat(40));
    expect(sink.done).toHaveBeenCalledTimes(1);
  });

  test("watchRunProgress reads the progress fixture and ends on finished/end", async () => {
    const finished =
      'event: progress\ndata: {"ts":"2026-09-10T09:26:00Z","source":"factory","stage":"finished","event":"end"}\n\n';
    const { fetch, calls } = recordingFetch(
      () =>
        new Response(bodyOf([readFixtureText("api/run-progress.sse"), finished]), { status: 200 }),
    );
    const sink = collect<ProgressEvent>();
    watchRunProgress(httpFor(fetch), "run-accepted", sink.handlers, backoff);
    await vi.advanceTimersByTimeAsync(50);

    expect(calls).toHaveLength(1);
    expect(calls[0]?.url).toBe("/runs/run-accepted/progress");
    expect(calls[0]?.init.headers).toEqual({ Authorization: "Bearer read-t" });
    expect(sink.values.map((event) => `${event.stage}/${event.event}`)).toEqual([
      "build/start",
      "build/round_summary",
      "build/end",
      "finished/end",
    ]);
    expect(sink.values[1]?.detail).toBe("478.3k tokens");
    expect(sink.done).toHaveBeenCalledTimes(1);
  });

  test("watchRunProgress retries a stream that ends without finished/end", async () => {
    const { fetch, calls } = recordingFetch(
      () => new Response(bodyOf([readFixtureText("api/run-progress.sse")]), { status: 200 }),
    );
    const sink = collect<ProgressEvent>();
    const stop = watchRunProgress(httpFor(fetch), "run-1", sink.handlers, backoff);
    await vi.advanceTimersByTimeAsync(50);
    stop();
    expect(calls.length).toBeGreaterThan(1);
    expect(sink.done).not.toHaveBeenCalled();
  });

  test("watchRunProgress reports a 403 once and ends", async () => {
    const { fetch, calls } = recordingFetch(() => new Response("no", { status: 403 }));
    const sink = collect<ProgressEvent>();
    watchRunProgress(httpFor(fetch), "run-1", sink.handlers, backoff);
    await vi.advanceTimersByTimeAsync(100);
    expect(calls).toHaveLength(1);
    expect(sink.errors.map((e) => e.status)).toEqual([403]);
    expect(sink.done).toHaveBeenCalledTimes(1);
  });

  test("isFinishedEvent is true only for stage finished with event end", () => {
    const event = (stage: string, name: string): ProgressEvent => ({
      ts: new Date(0),
      source: "factory",
      stage,
      event: name,
      round: 0,
      maxRounds: 0,
      outcome: "",
      detail: "",
    });
    expect(isFinishedEvent(event("finished", "end"))).toBe(true);
    expect(isFinishedEvent(event("build", "end"))).toBe(false);
    expect(isFinishedEvent(event("finished", "start"))).toBe(false);
  });

  function logSink() {
    const chunks: string[] = [];
    const errors: unknown[] = [];
    const done = vi.fn();
    return {
      chunks,
      errors,
      done,
      handlers: {
        onChunk: (text: string) => chunks.push(text),
        onError: (error: unknown) => errors.push(error),
        onDone: done,
      },
    };
  }

  test("watchRunLog sends the read token as a header, never in the request URL, and streams the response body as text chunks", async () => {
    const { fetch, calls } = recordingFetch(
      () => new Response(bodyOf(["building ticket-1\n"]), { status: 200 }),
    );
    const sink = logSink();
    watchRunLog(httpFor(fetch), "run-1", sink.handlers);
    await vi.advanceTimersByTimeAsync(0);

    expect(sink.chunks.join("")).toBe("building ticket-1\n");
    expect(sink.done).toHaveBeenCalledTimes(1);
    expect(calls[0]?.url).toBe("/runs/run-1/log?follow=1");
    expect(calls[0]?.url).not.toContain("token");
    expect(calls[0]?.init.headers).toEqual({ Authorization: "Bearer read-t" });
  });

  test("watchRunLog joins a multi-byte character split across chunks", async () => {
    const bytes = encoder.encode("café\n");
    const body = new ReadableStream<Uint8Array>({
      start(controller) {
        controller.enqueue(bytes.slice(0, 4));
        controller.enqueue(bytes.slice(4));
        controller.close();
      },
    });
    const { fetch } = recordingFetch(() => new Response(body, { status: 200 }));
    const sink = logSink();
    watchRunLog(httpFor(fetch), "run-1", sink.handlers);
    await vi.advanceTimersByTimeAsync(0);
    expect(sink.chunks.join("")).toBe("café\n");
  });

  test("watchRunLog streams the committed log fixture", async () => {
    const text = readFixtureText("api/run-log.txt");
    const { fetch } = recordingFetch(() => new Response(bodyOf([text]), { status: 200 }));
    const sink = logSink();
    watchRunLog(httpFor(fetch), "run-accepted", sink.handlers);
    await vi.advanceTimersByTimeAsync(0);
    expect(sink.chunks.join("")).toBe(text);
    expect(sink.chunks.join("")).toContain("round 1: building");
  });

  test("watchRunLog releases the connection if cancelled before it finishes connecting (regression: leaving a run page while its log tail connected kept the follow connection open for good)", async () => {
    let release: (response: Response) => void = () => undefined;
    const pending = new Promise<Response>((resolve) => {
      release = resolve;
    });
    let bodyCancelled = false;
    const { fetch, calls } = recordingFetch(() => pending);
    const sink = logSink();
    const stop = watchRunLog(httpFor(fetch), "run-1", sink.handlers);
    await vi.advanceTimersByTimeAsync(0);
    expect(calls).toHaveLength(1);
    expect((calls[0]?.init.signal as AbortSignal).aborted).toBe(false);
    stop();
    expect((calls[0]?.init.signal as AbortSignal).aborted).toBe(true);

    release(
      new Response(
        bodyOf(["late\n"], "hang", () => {
          bodyCancelled = true;
        }),
        { status: 200 },
      ),
    );
    await vi.advanceTimersByTimeAsync(0);
    expect(bodyCancelled).toBe(true);
    expect(sink.chunks).toEqual([]);
    expect(sink.done).not.toHaveBeenCalled();
  });

  test("watchRunLog unsubscribe while streaming aborts the request and delivers nothing more", async () => {
    let controllerRef: ReadableStreamDefaultController<Uint8Array> | undefined;
    let bodyCancelled = false;
    const body = new ReadableStream<Uint8Array>({
      start(controller) {
        controllerRef = controller;
        controller.enqueue(encoder.encode("first\n"));
      },
      cancel() {
        bodyCancelled = true;
      },
    });
    const { fetch, calls } = recordingFetch(() => new Response(body, { status: 200 }));
    const sink = logSink();
    const stop = watchRunLog(httpFor(fetch), "run-1", sink.handlers);
    await vi.advanceTimersByTimeAsync(0);
    expect(sink.chunks).toEqual(["first\n"]);

    stop();
    await vi.advanceTimersByTimeAsync(0);
    expect((calls[0]?.init.signal as AbortSignal).aborted).toBe(true);
    expect(bodyCancelled).toBe(true);
    try {
      controllerRef?.enqueue(encoder.encode("second\n"));
    } catch {
      // The stream is already cancelled: nothing can arrive.
    }
    await vi.advanceTimersByTimeAsync(0);
    expect(sink.chunks).toEqual(["first\n"]);
    expect(sink.done).not.toHaveBeenCalled();
    expect(sink.errors).toEqual([]);
  });

  test("watchRunLog surfaces a non-2xx response as an error", async () => {
    const { fetch } = recordingFetch(() => new Response("not found", { status: 404 }));
    const sink = logSink();
    watchRunLog(httpFor(fetch), "missing-run", sink.handlers);
    await vi.advanceTimersByTimeAsync(0);
    expect(sink.errors).toHaveLength(1);
    expect(sink.errors[0]).toBeInstanceOf(ApiError);
    expect((sink.errors[0] as ApiError).status).toBe(404);
    expect(sink.done).toHaveBeenCalledTimes(1);
    expect(sink.chunks).toEqual([]);
  });
});
