import { act, renderHook, waitFor } from "@testing-library/react";

import { queryKeys } from "@/api/queryKeys";
import { hang, harness } from "@/api/apiTestHarness";
import {
  mergeRequestList,
  newerRequest,
  upsertRequest,
  useApproveRequest,
  useRejectRequest,
  useRequest,
  useRequestBoard,
  useRequestDetailEvents,
  useRequestRevisions,
  useRequests,
} from "@/api/requestQueries";
import { asObject } from "@/domain/decode";
import { type RequestSummary, decodeRequestList } from "@/domain/request";
import { readFixtureJson, readFixtureText } from "@/test/fixtures";

const fixture = decodeRequestList(readFixtureJson("api/requests.json"), "GET /requests");
const base = fixture.find((r) => r.id === "req-spec-review")!;
const at = (request: RequestSummary, updatedAt: string, state = request.state): RequestSummary => ({
  ...request,
  updatedAt,
  state,
});

describe("merging request records", () => {
  test("the newer record wins, whichever arrives second", () => {
    const older = at(base, "2026-09-10T09:05:00Z");
    const newer = at(base, "2026-09-10T09:06:00Z", "planning");
    expect(newerRequest(older, newer)).toBe(newer);
    expect(newerRequest(newer, older)).toBe(newer);
  });

  test("an older list response does not overwrite a newer record from the stream", () => {
    const streamed = at(base, "2026-09-10T09:06:00Z", "planning");
    const stale = at(base, "2026-09-10T09:05:00Z");
    expect(mergeRequestList([streamed], [stale])).toEqual([streamed]);
  });

  test("a list response drops a request the server no longer lists and adds new ones", () => {
    const other = { ...base, id: "req-2" };
    expect(mergeRequestList([base], [other]).map((r) => r.id)).toEqual(["req-2"]);
  });

  test("upsert replaces by id and appends an unknown request", () => {
    const changed = at(base, "2026-09-10T10:00:00Z", "planning");
    expect(upsertRequest([base], changed)).toEqual([changed]);
    expect(upsertRequest([base], { ...base, id: "req-new" }).map((r) => r.id)).toEqual([
      base.id,
      "req-new",
    ]);
    expect(upsertRequest(undefined, base)).toEqual([base]);
  });
});

describe("useRequests", () => {
  test("polls only when given an interval, and never opens the event stream", async () => {
    const { wrapper, calls } = harness([
      { match: (url) => url === "/requests", respond: () => new Response("[]") },
    ]);
    const polled = renderHook(() => useRequests(20), { wrapper });
    await waitFor(() => {
      expect(calls.filter((c) => c.url === "/requests").length).toBeGreaterThanOrEqual(3);
    });
    expect(calls.some((c) => c.url === "/requests/events")).toBe(false);
    polled.unmount();

    const once = harness([
      { match: (url) => url === "/requests", respond: () => new Response("[]") },
    ]);
    const plain = renderHook(() => useRequests(), { wrapper: once.wrapper });
    await waitFor(() => {
      expect(plain.result.current.isSuccess).toBe(true);
    });
    await new Promise((resolve) => setTimeout(resolve, 80));
    expect(once.calls.filter((c) => c.url === "/requests")).toHaveLength(1);
    plain.unmount();
  });
});

describe("useRequestBoard", () => {
  test("lists the requests and applies events from the stream", async () => {
    const event = { ...asObject(JSON.parse(readFixtureText("api/requests.json"))[0], "x") };
    const first = decodeRequestList([event], "x")[0]!;
    const changed = { ...event, state: "cancelled", updated_at: "2026-09-10T11:00:00Z" };
    const { wrapper } = harness([
      {
        match: (url) => url === "/requests/events",
        respond: () =>
          new Response(
            new ReadableStream<Uint8Array>({
              start(controller) {
                controller.enqueue(
                  new TextEncoder().encode(`event: state\ndata: ${JSON.stringify(changed)}\n\n`),
                );
              },
            }),
          ),
      },
      {
        match: (url) => url === "/requests",
        respond: () => new Response(readFixtureText("api/requests.json")),
      },
    ]);
    const { result, unmount } = renderHook(() => useRequestBoard(), { wrapper });
    await waitFor(() => {
      expect(result.current.query.data?.find((r) => r.id === first.id)?.state).toBe("cancelled");
    });
    expect(result.current.query.data).toHaveLength(7);
    expect(result.current.live).toBe(true);
    unmount();
  });

  test("a failed refresh keeps the last list and reports the error", async () => {
    let fail = false;
    const { wrapper } = harness([
      { match: (url) => url === "/requests/events", respond: hang },
      {
        match: (url) => url === "/requests",
        respond: () =>
          fail
            ? new Response('{"error": "list requests"}', { status: 500 })
            : new Response(readFixtureText("api/requests.json")),
      },
    ]);
    // A component reads these while rendering; the query re-renders only for
    // the fields a render has read.
    const { result, unmount } = renderHook(
      () => {
        const { query } = useRequestBoard();
        return { query, isError: query.isError, error: query.error, data: query.data };
      },
      { wrapper },
    );
    await waitFor(() => {
      expect(result.current.data).toHaveLength(7);
    });
    fail = true;
    await act(async () => {
      await result.current.query.refetch();
    });
    await waitFor(() => {
      expect(result.current.isError).toBe(true);
    });
    expect(result.current.data).toHaveLength(7);
    unmount();
  });

  test("a permanent stream failure is reported and the list still loads", async () => {
    const { wrapper } = harness([
      {
        match: (url) => url === "/requests/events",
        respond: () =>
          new Response('{"error": "requests endpoint is not authorized"}', { status: 403 }),
      },
      {
        match: (url) => url === "/requests",
        respond: () => new Response(readFixtureText("api/requests.json")),
      },
    ]);
    const { result, unmount } = renderHook(() => useRequestBoard(), { wrapper });
    await waitFor(() => {
      expect(result.current.streamError?.serverMessage).toBe("requests endpoint is not authorized");
    });
    await waitFor(() => {
      expect(result.current.query.data).toHaveLength(7);
    });
    expect(result.current.live).toBe(false);
    unmount();
  });
});

describe("useApproveRequest", () => {
  test("puts the server's answer in the detail cache without a second fetch", async () => {
    const detail = readFixtureJson("api/request-spec-review.json") as Record<string, unknown>;
    const approved = { ...detail, state: "planning", updated_at: "2026-09-10T12:00:00Z" };
    const { wrapper, calls } = harness([
      {
        match: (url, init) => url === "/requests/req-spec-review/approve" && init.method === "POST",
        respond: () => new Response(JSON.stringify(approved)),
      },
      {
        match: (url) => url === "/requests/req-spec-review",
        respond: () => new Response(JSON.stringify(detail)),
      },
    ]);
    const { result } = renderHook(
      () => ({
        request: useRequest("req-spec-review"),
        approve: useApproveRequest("req-spec-review"),
      }),
      { wrapper },
    );
    await waitFor(() => {
      expect(result.current.request.data?.state).toBe("spec_review");
    });
    await act(async () => {
      await result.current.approve.mutateAsync({
        by: "alice",
        expectedSha256: { "spec.md": "abc" },
      });
    });
    await waitFor(() => {
      expect(result.current.request.data?.state).toBe("planning");
    });
    const post = calls.find((c) => c.init.method === "POST")!;
    expect(JSON.parse(post.init.body as string)).toEqual({
      by: "alice",
      expected_sha256: { "spec.md": "abc" },
    });
    expect(calls.filter((c) => c.url === "/requests/req-spec-review")).toHaveLength(1);
  });
});

describe("useApproveRequest invalidation", () => {
  test("refetches revisions and leaves the list alone", async () => {
    const detail = readFixtureJson("api/request-spec-review.json") as Record<string, unknown>;
    const { wrapper, client, calls } = harness([
      {
        match: (url, init) => url === "/requests/req-spec-review/approve" && init.method === "POST",
        respond: () => new Response(JSON.stringify(detail)),
      },
      {
        match: (url) => url === "/requests/req-spec-review/revisions",
        respond: () => new Response("[]"),
      },
    ]);
    const k = queryKeys.requests;
    client.setQueryData(k.list(), []);
    const revisions = renderHook(
      () => ({
        rev: useRequestRevisions("req-spec-review"),
        a: useApproveRequest("req-spec-review"),
      }),
      { wrapper },
    );
    await waitFor(() => {
      expect(revisions.result.current.rev.isSuccess).toBe(true);
    });
    await act(async () => {
      await revisions.result.current.a.mutateAsync({ by: "a", expectedSha256: {} });
    });
    const stale = (key: readonly unknown[]) => client.getQueryState(key)?.isInvalidated;
    expect(stale(k.list())).toBe(false);
    await waitFor(() => {
      expect(calls.filter((c) => c.url.endsWith("/revisions"))).toHaveLength(2);
    });
  });
});

describe("useRequestDetailEvents", () => {
  test("an event for this request that changes it marks the detail stale; others do not", async () => {
    const detail = readFixtureJson("api/request-spec-review.json") as Record<string, unknown>;
    const other = { ...detail, id: "someone-else", updated_at: "2026-09-10T13:00:00Z" };
    const changed = { ...detail, state: "planning", updated_at: "2026-09-10T12:00:00Z" };
    let detailFetches = 0;
    let push: (event: object) => void = () => undefined;
    const { wrapper } = harness([
      {
        match: (url) => url === "/requests/events",
        respond: () =>
          new Response(
            new ReadableStream<Uint8Array>({
              start(controller) {
                push = (event) => {
                  controller.enqueue(
                    new TextEncoder().encode(`event: state\ndata: ${JSON.stringify(event)}\n\n`),
                  );
                };
              },
            }),
          ),
      },
      {
        match: (url) => url === "/requests/req-spec-review",
        respond: () => {
          detailFetches += 1;
          return new Response(JSON.stringify(detail));
        },
      },
    ]);
    const { result, unmount } = renderHook(
      () => {
        useRequestDetailEvents("req-spec-review");
        return useRequest("req-spec-review");
      },
      { wrapper },
    );
    await waitFor(() => {
      expect(result.current.isSuccess).toBe(true);
    });
    await waitFor(() => {
      push(other);
      expect(detailFetches).toBe(1);
    });
    await new Promise((resolve) => setTimeout(resolve, 50));
    expect(detailFetches).toBe(1);
    push(changed);
    await waitFor(() => {
      expect(detailFetches).toBe(2);
    });
    unmount();
  });
});

describe("useRejectRequest", () => {
  const seen = { state: "spec_review", enteredAt: "2026-09-10T09:05:00Z" };
  const refused = (status: number) => ({
    match: (url: string, init: RequestInit) =>
      url === "/requests/req-spec-review/reject" && init.method === "POST",
    respond: () =>
      new Response('{"error": "request req-spec-review is in plan_review now"}', { status }),
  });
  const invalidated = (client: ReturnType<typeof harness>["client"]) => ({
    detail: client.getQueryState(queryKeys.requests.detail("req-spec-review"))?.isInvalidated,
    list: client.getQueryState(queryKeys.requests.list())?.isInvalidated,
  });

  test("sends the stage the caller saw", async () => {
    const { wrapper, calls } = harness([
      {
        match: (url) => url === "/requests/req-spec-review/reject",
        respond: () =>
          new Response(JSON.stringify(readFixtureJson("api/request-spec-review.json"))),
      },
    ]);
    const { result } = renderHook(() => useRejectRequest("req-spec-review"), { wrapper });
    await act(async () => {
      await result.current.mutateAsync({ reason: "too broad", by: "jane", seen });
    });
    expect(JSON.parse(calls[0]?.init.body as string)).toEqual({
      reason: "too broad",
      expected_state: "spec_review",
      expected_entered_at: "2026-09-10T09:05:00Z",
      by: "jane",
    });
  });

  test("a 409 marks the request and the list to be read again, so the page shows the stage it is in now", async () => {
    const { wrapper, client } = harness([refused(409)]);
    client.setQueryData(queryKeys.requests.detail("req-spec-review"), base);
    client.setQueryData(queryKeys.requests.list(), fixture);
    const { result } = renderHook(() => useRejectRequest("req-spec-review"), { wrapper });
    await act(async () => {
      await result.current.mutateAsync({ reason: "too broad", seen }).catch(() => undefined);
    });
    await waitFor(() => {
      expect(result.current.error?.status).toBe(409);
    });
    expect(invalidated(client)).toEqual({ detail: true, list: true });
  });

  test("another refusal leaves both as they are", async () => {
    const { wrapper, client } = harness([refused(422)]);
    client.setQueryData(queryKeys.requests.detail("req-spec-review"), base);
    client.setQueryData(queryKeys.requests.list(), fixture);
    const { result } = renderHook(() => useRejectRequest("req-spec-review"), { wrapper });
    await act(async () => {
      await result.current.mutateAsync({ reason: "too broad", seen }).catch(() => undefined);
    });
    await waitFor(() => {
      expect(result.current.error?.status).toBe(422);
    });
    expect(invalidated(client)).toEqual({ detail: false, list: false });
  });
});
