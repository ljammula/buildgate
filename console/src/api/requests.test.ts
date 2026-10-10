import { createHttp } from "@/api/http";
import {
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
import { ApiError } from "@/domain/apiError";
import type { RequestSummary } from "@/domain/request";
import { readFixtureText } from "@/test/fixtures";

interface Call {
  readonly url: string;
  readonly init: RequestInit;
}

const encoder = new TextEncoder();

/** A fetch that records every call and answers each with `respond`. */
function recording(respond: (call: Call) => Response | Promise<Response>) {
  const calls: Call[] = [];
  const fetch = vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
    const call = { url: input instanceof Request ? input.url : input.toString(), init: init ?? {} };
    calls.push(call);
    return Promise.resolve(respond(call));
  }) as unknown as typeof globalThis.fetch;
  return { fetch, calls };
}

const httpWith = (fetch: typeof globalThis.fetch) =>
  createHttp({
    baseUrl: "",
    readToken: "read-t",
    startToken: "start-t",
    overrideToken: "override-t",
    gateToken: "gate-t",
    fetch,
  });

const requestJson = (id: string, state: string) =>
  JSON.stringify({
    id,
    workspace: "/repos/app",
    project: "app",
    state,
    submitted_at: "2026-09-10T09:00:00Z",
    updated_at: "2026-09-10T09:00:00Z",
  });

const READ = { Authorization: "Bearer read-t" };
const WRITE = { "Content-Type": "application/json", Authorization: "Bearer gate-t" };

/** One recorded write: a fake that answers 200 with a request, and the body it was sent. */
async function sentBy(send: (http: ReturnType<typeof httpWith>) => Promise<unknown>) {
  const { fetch, calls } = recording(() => new Response(requestJson("req-1", "planning")));
  await send(httpWith(fetch));
  const call = calls[0]!;
  return {
    url: call.url,
    method: call.init.method,
    headers: call.init.headers,
    body: call.init.body,
  };
}

describe("reads", () => {
  test("listRequests sends the read token and decodes the committed fixture", async () => {
    const { fetch, calls } = recording(() => new Response(readFixtureText("api/requests.json")));
    const requests = await listRequests(httpWith(fetch));
    expect(calls[0]!.url).toBe("/requests");
    expect(calls[0]!.init.method).toBe("GET");
    expect(calls[0]!.init.headers).toEqual(READ);
    expect(requests.map((r) => [r.id, r.state])).toEqual([
      ["req-building", "building"],
      ["req-done", "done"],
      ["req-every-field", "resume_review"],
      ["req-halted", "halted"],
      ["req-oracle-review", "oracle_review"],
      ["req-plan-review", "plan_review"],
      ["req-quarantined", "quarantined"],
      ["req-spec-review", "spec_review"],
    ]);
  });

  test("getRequest fetches an encoded id with the read token and decodes the fixture", async () => {
    const { fetch, calls } = recording(
      () => new Response(readFixtureText("api/request-plan-review.json")),
    );
    const request = await getRequest(httpWith(fetch), "req-plan-review");
    expect(calls[0]!.url).toBe("/requests/req-plan-review");
    expect(calls[0]!.init.headers).toEqual(READ);
    expect(request.id).toBe("req-plan-review");
    expect(request.state).toBe("plan_review");

    await getRequest(httpWith(fetch), "a/b c");
    expect(calls[1]!.url).toBe("/requests/a%2Fb%20c");
  });

  test("listRevisions/getRevision parse the revision routes", async () => {
    const { fetch, calls } = recording((call) =>
      call.url.endsWith("/revisions")
        ? new Response(readFixtureText("api/request-revisions.json"))
        : new Response(readFixtureText("api/request-revision.json")),
    );
    const http = httpWith(fetch);

    const revisions = await listRevisions(http, "req-plan-review");
    expect(calls[0]!.url).toBe("/requests/req-plan-review/revisions");
    expect(calls[0]!.init.headers).toEqual(READ);
    expect(revisions).toHaveLength(1);
    expect(revisions[0]!.index).toBe(1);
    expect(revisions[0]!.by).toBe("alice");
    expect(revisions[0]!.fromState).toBe("plan_review");
    expect(revisions[0]!.files).toEqual(["spec.md", "tickets/001.spec.md", "tickets/002.spec.md"]);

    const detail = await getRevision(http, "req-plan-review", 1);
    expect(calls[1]!.url).toBe("/requests/req-plan-review/revisions/1");
    expect(calls[1]!.init.headers).toEqual(READ);
    expect(detail.reason).toBe(
      "- tickets/001.spec.md, ### Steps: split the migration out\n\nOtherwise fine.",
    );
    expect(detail.files["spec.md"]).toContain("# Idempotency keys for checkout");
  });
});

describe("writes", () => {
  test("createRequest sends the gate token and only the fields that are set", async () => {
    const sent = await sentBy((http) =>
      createRequest(http, {
        workspace: "/repos/app",
        text: "add a thing",
        verifyCommand: "make verify",
        fullSuiteCommand: "make test",
        preflightProfile: "go",
        draftOracles: true,
        by: "jane",
      }),
    );
    expect(sent.url).toBe("/requests");
    expect(sent.method).toBe("POST");
    expect(sent.headers).toEqual(WRITE);
    expect(sent.body).toBe(
      '{"workspace":"/repos/app","text":"add a thing","verify_command":"make verify",' +
        '"full_suite_command":"make test","preflight_profile":"go","draft_oracles":true,"by":"jane"}',
    );

    const minimal = await sentBy((http) =>
      createRequest(http, {
        workspace: "/repos/app",
        text: "t",
        verifyCommand: "",
        draftOracles: false,
        by: "",
      }),
    );
    expect(minimal.body).toBe('{"workspace":"/repos/app","text":"t"}');
  });

  test("createRequest decodes the response", async () => {
    const { fetch } = recording(() => new Response(requestJson("req-9", "submitted")));
    const request = await createRequest(httpWith(fetch), { workspace: "/repos/app", text: "t" });
    expect(request.id).toBe("req-9");
    expect(request.state).toBe("submitted");
  });

  test('approveRequest/rejectRequest send an optional "by"', async () => {
    // Omitted `by` sends no body at all, matching the server's own fallback.
    const none = await sentBy((http) => approveRequest(http, "req-1"));
    expect(none.url).toBe("/requests/req-1/approve");
    expect(none.method).toBe("POST");
    expect(none.headers).toEqual(WRITE);
    expect(none.body).toBeUndefined();

    const by = await sentBy((http) => approveRequest(http, "req-1", { by: "jane" }));
    expect(by.body).toBe('{"by":"jane"}');

    const seen = { state: "spec_review", enteredAt: "2026-09-10T09:05:00Z" };
    const stage = '"expected_state":"spec_review","expected_entered_at":"2026-09-10T09:05:00Z"';
    const reject = await sentBy((http) =>
      rejectRequest(http, "req-1", { reason: "scope creep", seen }),
    );
    expect(reject.url).toBe("/requests/req-1/reject");
    expect(reject.headers).toEqual(WRITE);
    expect(reject.body).toBe(`{"reason":"scope creep",${stage}}`);

    const rejectBy = await sentBy((http) =>
      rejectRequest(http, "req-1", { reason: "scope creep", by: "jane", seen }),
    );
    expect(rejectBy.body).toBe(`{"reason":"scope creep",${stage},"by":"jane"}`);
  });

  test("rejectRequest names the stage the operator saw, on a rejection and on a send-back", async () => {
    const rejection = await sentBy((http) =>
      rejectRequest(http, "req-1", {
        reason: "",
        by: "jane",
        seen: { state: "plan_review", enteredAt: "2026-09-10T09:30:00.5Z" },
        anchors: [{ path: "tickets/001.spec.md", section: "Steps", item: 2, note: "split it" }],
      }),
    );
    expect(JSON.parse(rejection.body as string)).toEqual({
      reason: "",
      expected_state: "plan_review",
      expected_entered_at: "2026-09-10T09:30:00.5Z",
      by: "jane",
      anchors: [{ path: "tickets/001.spec.md", section: "Steps", item: 2, note: "split it" }],
    });

    const sendBack = await sentBy((http) =>
      rejectRequest(http, "req-1", {
        reason: "again",
        by: "jane",
        to: "plan",
        seen: { state: "quarantined", enteredAt: "2026-09-10T10:00:00Z" },
      }),
    );
    expect(sendBack.body).toBe(
      '{"reason":"again","expected_state":"quarantined","expected_entered_at":"2026-09-10T10:00:00Z","by":"jane","to":"plan"}',
    );
  });

  test(
    "approveRequest sends expected_sha256 alongside by, binding approval " +
      "to the artifact shown",
    async () => {
      const hashes = { "spec.md": "abc123", "tickets/001.spec.md": "def456" };
      const bound = await sentBy((http) =>
        approveRequest(http, "req-1", { by: "jane", expectedSha256: hashes }),
      );
      expect(bound.body).toBe(
        '{"by":"jane","expected_sha256":{"spec.md":"abc123","tickets/001.spec.md":"def456"}}',
      );

      // Hashes without a name go through too.
      const noBy = await sentBy((http) =>
        approveRequest(http, "req-1", { expectedSha256: hashes }),
      );
      expect(JSON.parse(noBy.body as string)).toEqual({ expected_sha256: hashes });

      // An empty map is the same as omitting it: no expected_sha256 key.
      const empty = await sentBy((http) =>
        approveRequest(http, "req-1", { by: "jane", expectedSha256: {} }),
      );
      expect(empty.body).toBe('{"by":"jane"}');
      const emptyOnly = await sentBy((http) =>
        approveRequest(http, "req-1", { expectedSha256: {} }),
      );
      expect(emptyOnly.body).toBeUndefined();
    },
  );

  test("updateRequestSpec and updateRequestTicket PUT the content and the base hash", async () => {
    const spec = await sentBy((http) =>
      updateRequestSpec(http, "req-1", "# Spec\n", { baseSha256: "base1" }),
    );
    expect(spec.url).toBe("/requests/req-1/spec");
    expect(spec.method).toBe("PUT");
    expect(spec.headers).toEqual(WRITE);
    expect(spec.body).toBe('{"content":"# Spec\\n","base_sha256":"base1"}');

    const noBase = await sentBy((http) => updateRequestSpec(http, "req-1", "x"));
    expect(noBase.body).toBe('{"content":"x"}');

    const ticket = await sentBy((http) =>
      updateRequestTicket(http, "req-1", 2, "plan", { baseSha256: "base2" }),
    );
    expect(ticket.url).toBe("/requests/req-1/tickets/2");
    expect(ticket.method).toBe("PUT");
    expect(ticket.headers).toEqual(WRITE);
    expect(ticket.body).toBe('{"content":"plan","base_sha256":"base2"}');
  });

  test("retry, cancel and resume send the reason or source, and by only when set", async () => {
    const retry = await sentBy((http) => retryRequest(http, "req-1", { reason: "flaky" }));
    expect(retry.url).toBe("/requests/req-1/retry");
    expect(retry.method).toBe("POST");
    expect(retry.headers).toEqual(WRITE);
    expect(retry.body).toBe('{"reason":"flaky"}');
    const scratch = await sentBy((http) =>
      retryRequest(http, "req-1", { reason: "bad start", by: "jane", fromScratch: true }),
    );
    expect(scratch.body).toBe('{"reason":"bad start","by":"jane","from":"scratch"}');
    const attempt = await sentBy((http) =>
      retryRequest(http, "req-1", { reason: "flaky", fromScratch: false }),
    );
    expect(attempt.body).toBe('{"reason":"flaky"}');

    const cancel = await sentBy((http) =>
      cancelRequest(http, "req-1", { reason: "obsolete", by: "jane" }),
    );
    expect(cancel.url).toBe("/requests/req-1/cancel");
    expect(cancel.headers).toEqual(WRITE);
    expect(cancel.body).toBe('{"reason":"obsolete","by":"jane"}');

    const resume = await sentBy((http) => resumeRequest(http, "req-1", { from: "round" }));
    expect(resume.url).toBe("/requests/req-1/resume");
    expect(resume.headers).toEqual(WRITE);
    expect(resume.body).toBe('{"from":"round"}');
    const resumeBy = await sentBy((http) =>
      resumeRequest(http, "req-1", { from: "scratch", by: "jane" }),
    );
    expect(resumeBy.body).toBe('{"from":"scratch","by":"jane"}');
  });

  test("an update refused with a 409 rejects with the server's current_sha256", async () => {
    const { fetch } = recording(
      () =>
        new Response(JSON.stringify({ error: "spec.md changed", current_sha256: "abc" }), {
          status: 409,
        }),
    );
    const http = httpWith(fetch);
    for (const update of [
      () => updateRequestSpec(http, "req-1", "x", { baseSha256: "old" }),
      () => updateRequestTicket(http, "req-1", 1, "x", { baseSha256: "old" }),
    ]) {
      const error = (await update().then(
        () => null,
        (e: unknown) => e,
      )) as ApiError;
      expect(error).toBeInstanceOf(ApiError);
      expect(error.status).toBe(409);
      expect(error.currentSha256).toBe("abc");
      expect(error.serverMessage).toBe("spec.md changed");
    }
  });
});

describe("watchRequests", () => {
  beforeEach(() => {
    vi.useFakeTimers();
  });
  afterEach(() => {
    vi.useRealTimers();
  });

  const frame = (id: string) => `event: state\ndata: ${requestJson(id, "plan_review")}\n\n`;

  test("sends the read token as a header, never in the request URL, and delivers the committed stream", async () => {
    const { fetch, calls } = recording(
      () =>
        new Response(
          new ReadableStream<Uint8Array>({
            start(controller) {
              controller.enqueue(encoder.encode(readFixtureText("api/request-events.sse")));
            },
          }),
        ),
    );
    const values: RequestSummary[] = [];
    const unsubscribe = watchRequests(httpWith(fetch), {
      onValue: (request) => values.push(request),
      onError: () => undefined,
    });
    await vi.advanceTimersByTimeAsync(0);
    unsubscribe();
    expect(calls[0]!.url).toBe("/requests/events");
    expect(calls[0]!.init.headers).toEqual(READ);
    expect(values).toHaveLength(8);
    expect(values[0]!.id).toBe("req-building");
    expect(values[0]!.state).toBe("building");
  });

  test("retries with backoff on a non-2xx response instead of throwing", async () => {
    const { fetch, calls } = recording(() => new Response("server error", { status: 500 }));
    const onError = vi.fn();
    const unsubscribe = watchRequests(
      httpWith(fetch),
      { onValue: () => undefined, onError },
      { initialBackoffMs: 1, maxBackoffMs: 5 },
    );
    await vi.advanceTimersByTimeAsync(50);
    unsubscribe();
    expect(calls.length).toBeGreaterThan(1);
    expect(onError).not.toHaveBeenCalled();
  });

  test("forwards a permanent 4xx failure once instead of retrying it", async () => {
    const { fetch, calls } = recording(() => new Response("forbidden", { status: 403 }));
    const errors: ApiError[] = [];
    const onConnectionChange = vi.fn();
    watchRequests(
      httpWith(fetch),
      { onValue: () => undefined, onError: (e) => errors.push(e), onConnectionChange },
      { initialBackoffMs: 1, maxBackoffMs: 5 },
    );
    await vi.advanceTimersByTimeAsync(1000);
    expect(calls).toHaveLength(1);
    expect(errors).toHaveLength(1);
    expect(errors[0]!.status).toBe(403);
    expect(onConnectionChange).not.toHaveBeenCalled();
  });

  test("reports connection open/close via onConnectionChange", async () => {
    let attempts = 0;
    const { fetch } = recording(() => {
      attempts++;
      // A 200 whose body ends with no event: a connection that opened and
      // then closed. The second attempt delivers one and stays open.
      if (attempts === 1) return new Response("");
      return new Response(
        new ReadableStream<Uint8Array>({
          start(controller) {
            controller.enqueue(encoder.encode(frame("req-1")));
          },
        }),
      );
    });
    const events: string[] = [];
    const unsubscribe = watchRequests(
      httpWith(fetch),
      {
        onValue: (request) => events.push(`value:${request.id}`),
        onError: () => undefined,
        onConnectionChange: (live) => events.push(live ? "open" : "closed"),
      },
      { initialBackoffMs: 1, maxBackoffMs: 5 },
    );
    await vi.advanceTimersByTimeAsync(50);
    unsubscribe();
    expect(events).toEqual(["open", "closed", "open", "value:req-1"]);
  });

  test("works without onConnectionChange", async () => {
    const { fetch } = recording(
      () =>
        new Response(
          new ReadableStream<Uint8Array>({
            start(controller) {
              controller.enqueue(encoder.encode(frame("req-1")));
            },
          }),
        ),
    );
    const ids: string[] = [];
    const unsubscribe = watchRequests(httpWith(fetch), {
      onValue: (request) => ids.push(request.id),
      onError: () => undefined,
    });
    await vi.advanceTimersByTimeAsync(0);
    unsubscribe();
    expect(ids).toEqual(["req-1"]);
  });

  test(
    "releases the connection if cancelled before it finishes connecting (regression: a " +
      "screen disposed mid-connect used to leave the eventual response's HTTP connection " +
      "open for good once it arrived)",
    async () => {
      let resolveFetch!: (response: Response) => void;
      let cancelled = false;
      const fetch = vi.fn(
        () =>
          new Promise<Response>((resolve) => {
            resolveFetch = resolve;
          }),
      ) as unknown as typeof globalThis.fetch;
      const onValue = vi.fn();
      const onConnectionChange = vi.fn();
      const unsubscribe = watchRequests(httpWith(fetch), {
        onValue,
        onError: () => undefined,
        onConnectionChange,
      });
      await vi.advanceTimersByTimeAsync(0);
      unsubscribe();
      resolveFetch(
        new Response(
          new ReadableStream<Uint8Array>({
            start(controller) {
              controller.enqueue(encoder.encode(frame("req-1")));
            },
            cancel() {
              cancelled = true;
            },
          }),
        ),
      );
      await vi.advanceTimersByTimeAsync(1000);
      expect(cancelled).toBe(true);
      expect(onValue).not.toHaveBeenCalled();
      expect(fetch).toHaveBeenCalledTimes(1);
    },
  );

  test("unsubscribing stops delivery and further attempts", async () => {
    let controllerRef!: ReadableStreamDefaultController<Uint8Array>;
    const { fetch, calls } = recording(
      () =>
        new Response(
          new ReadableStream<Uint8Array>({
            start(controller) {
              controllerRef = controller;
              controller.enqueue(encoder.encode(frame("req-1")));
            },
          }),
        ),
    );
    const ids: string[] = [];
    const unsubscribe = watchRequests(httpWith(fetch), {
      onValue: (request) => ids.push(request.id),
      onError: () => undefined,
    });
    await vi.advanceTimersByTimeAsync(0);
    expect(ids).toEqual(["req-1"]);
    unsubscribe();
    try {
      controllerRef.enqueue(encoder.encode(frame("req-2")));
    } catch {
      // The reader already cancelled the stream.
    }
    await vi.advanceTimersByTimeAsync(60_000);
    expect(ids).toEqual(["req-1"]);
    expect(calls).toHaveLength(1);
  });
});
