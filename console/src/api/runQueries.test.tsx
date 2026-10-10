import { act, renderHook, waitFor } from "@testing-library/react";

import { harness } from "@/api/apiTestHarness";
import { createHttp } from "@/api/http";
import { queryKeys } from "@/api/queryKeys";
import {
  runDetailOptions,
  useOverrideRun,
  useProjectOpsQueries,
  useRun,
  useRunRecord,
} from "@/api/runQueries";
import { asObject } from "@/domain/decode";
import { decodeRun } from "@/domain/run";
import { readFixtureJson, readFixtureText } from "@/test/fixtures";

const running = readFixtureJson("api/run-running.json") as Record<string, unknown>;
const runRoute = (body: () => unknown, id = "run-running") => ({
  match: (url: string) => url === `/runs/${id}`,
  respond: () => new Response(JSON.stringify(body())),
});

describe("useRunRecord", () => {
  test("fetches the run with no event stream, and not while disabled", async () => {
    const { wrapper, calls } = harness([runRoute(() => running)]);
    const off = renderHook(() => useRunRecord("run-running", { enabled: false }), { wrapper });
    await new Promise((resolve) => setTimeout(resolve, 30));
    expect(calls).toHaveLength(0);
    off.unmount();

    const on = renderHook(() => useRunRecord("run-running"), { wrapper });
    await waitFor(() => {
      expect(on.result.current.data?.id).toBe("run-running");
    });
    expect(calls.map((c) => c.url)).toEqual(["/runs/run-running"]);
    on.unmount();
  });

  test("polls when given an interval", async () => {
    const { wrapper, calls } = harness([runRoute(() => running)]);
    const { unmount } = renderHook(() => useRunRecord("run-running", { refetchIntervalMs: 20 }), {
      wrapper,
    });
    await waitFor(() => {
      expect(calls.length).toBeGreaterThanOrEqual(3);
    });
    unmount();
  });

  test("a plain refetch cannot replace a newer record already in the shared entry", async () => {
    const { wrapper, client } = harness([
      runRoute(() => ({ ...running, updated_at: "2026-09-10T09:00:00Z" })),
    ]);
    const newer = decodeRun(asObject({ ...running, updated_at: "2026-09-10T10:00:00Z" }, "x"), "x");
    client.setQueryData(queryKeys.runs.detail("run-running"), newer);
    const { result, unmount } = renderHook(() => useRunRecord("run-running"), { wrapper });
    await waitFor(() => {
      expect(result.current.isFetching).toBe(false);
    });
    expect(result.current.data?.updatedAt).toBe(newer.updatedAt);
    unmount();
  });

  test("uses the key and guard useRun uses", () => {
    const options = runDetailOptions(
      createHttp({
        baseUrl: "",
        readToken: null,
        startToken: null,
        overrideToken: null,
        gateToken: null,
      }),
      "run-1",
    );
    expect(options.queryKey).toEqual(queryKeys.runs.detail("run-1"));
    expect(typeof options.structuralSharing).toBe("function");
  });
});

describe("useRun", () => {
  test("a terminal run's evidence is marked stale, its detail is not", async () => {
    const done = { ...running, state: "accepted", updated_at: "2026-09-10T11:00:00Z" };
    const { wrapper, client } = harness([
      runRoute(() => running),
      {
        match: (url) => url === "/runs/run-running/events",
        respond: () =>
          new Response(
            new ReadableStream<Uint8Array>({
              start(controller) {
                controller.enqueue(
                  new TextEncoder().encode(`event: state\ndata: ${JSON.stringify(done)}\n\n`),
                );
              },
            }),
          ),
      },
    ]);
    client.setQueryData(queryKeys.runs.diff("run-running"), "diff");
    const { result, unmount } = renderHook(() => useRun("run-running"), { wrapper });
    await waitFor(() => {
      expect(result.current.query.data?.state).toBe("accepted");
    });
    expect(client.getQueryState(queryKeys.runs.diff("run-running"))?.isInvalidated).toBe(true);
    unmount();
  });
});

describe("useOverrideRun", () => {
  test("marks the run's evidence, the lists and the requests stale", async () => {
    const { wrapper, client } = harness([
      {
        match: (url, init) => url === "/runs/run-running/override" && init.method === "POST",
        respond: () => new Response(JSON.stringify(running)),
      },
    ]);
    client.setQueryData(queryKeys.runs.release("run-running"), "release");
    client.setQueryData(queryKeys.requests.list(), []);
    const { result } = renderHook(() => useOverrideRun("run-running"), { wrapper });
    await act(async () => {
      await result.current.mutateAsync({ by: "a", reason: "r", state: "halted" });
    });
    expect(client.getQueryState(queryKeys.runs.release("run-running"))?.isInvalidated).toBe(true);
    expect(client.getQueryState(queryKeys.requests.list())?.isInvalidated).toBe(true);
  });
});

describe("useProjectOpsQueries", () => {
  test("returns each project's stats and release as typed pairs, in order", async () => {
    const { wrapper, calls } = harness([
      {
        match: (url) => url.endsWith("/stats"),
        respond: () => new Response(readFixtureText("api/project-stats.json")),
      },
      {
        match: (url) => url.endsWith("/release"),
        respond: () => new Response('{"error": "boom"}', { status: 500 }),
      },
    ]);
    const { result, unmount } = renderHook(() => useProjectOpsQueries(["app", "lib"]), {
      wrapper,
    });
    await waitFor(() => {
      expect(result.current.every((p) => p.stats.isSuccess && p.release.isError)).toBe(true);
    });
    expect(result.current.map((p) => p.project)).toEqual(["app", "lib"]);
    expect(result.current[0]?.stats.data).toBeDefined();
    expect(calls.map((c) => c.url).sort()).toEqual([
      "/projects/app/release",
      "/projects/app/stats",
      "/projects/lib/release",
      "/projects/lib/stats",
    ]);
    unmount();
  });

  test("is empty for no projects", () => {
    const { wrapper } = harness([]);
    const { result } = renderHook(() => useProjectOpsQueries([]), { wrapper });
    expect(result.current).toEqual([]);
  });
});
