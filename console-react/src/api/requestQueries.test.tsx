import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, renderHook, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";

import { ApiProvider } from "@/api/ApiProvider";
import { createHttp } from "@/api/http";
import {
  mergeRequestList,
  newerRequest,
  upsertRequest,
  useApproveRequest,
  useRequest,
  useRequestBoard,
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

interface Route {
  readonly match: (url: string, init: RequestInit) => boolean;
  readonly respond: () => Response | Promise<Response>;
}

function harness(routes: Route[]) {
  const calls: { url: string; init: RequestInit }[] = [];
  const fetch = ((input: RequestInfo | URL, init?: RequestInit) => {
    const url = input instanceof Request ? input.url : input.toString();
    calls.push({ url, init: init ?? {} });
    const route = routes.find((r) => r.match(url, init ?? {}));
    if (!route) return Promise.resolve(new Response('{"error": "no route"}', { status: 404 }));
    return Promise.resolve(route.respond());
  }) as typeof globalThis.fetch;
  const http = createHttp({
    baseUrl: "",
    readToken: null,
    startToken: null,
    overrideToken: null,
    fetch,
  });
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const wrapper = ({ children }: { children: ReactNode }) => (
    <QueryClientProvider client={client}>
      <ApiProvider
        http={http}
        config={{ writesEnabled: true, temporalUiUrl: null, releasePolicyWarning: null }}
      >
        {children}
      </ApiProvider>
    </QueryClientProvider>
  );
  return { wrapper, client, calls };
}

const hang = () => new Response(new ReadableStream<Uint8Array>({ start: () => undefined }));

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
    expect(result.current.query.data).toHaveLength(8);
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
      expect(result.current.data).toHaveLength(8);
    });
    fail = true;
    await act(async () => {
      await result.current.query.refetch();
    });
    await waitFor(() => {
      expect(result.current.isError).toBe(true);
    });
    expect(result.current.data).toHaveLength(8);
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
      expect(result.current.query.data).toHaveLength(8);
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
