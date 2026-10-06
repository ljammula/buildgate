import { renderHook } from "@testing-library/react";
import { act } from "react";

import { harness } from "@/api/apiTestHarness";
import type { WatchRequestsHandlers } from "@/api/requests";
import { useRequestBoard } from "@/api/requestQueries";
import { ApiError } from "@/domain/apiError";

const captured: { handlers: WatchRequestsHandlers | null } = { handlers: null };

vi.mock("@/api/requests", async (importOriginal) => ({
  ...(await importOriginal<Record<string, unknown>>()),
  watchRequests: (_http: unknown, handlers: WatchRequestsHandlers) => {
    captured.handlers = handlers;
    return () => undefined;
  },
}));

describe("useRequestBoard stream state", () => {
  test("a stream that connects again clears the permanent-error banner", () => {
    const { wrapper } = harness([
      { match: (url) => url === "/requests", respond: () => new Response("[]") },
    ]);
    const { result, unmount } = renderHook(() => useRequestBoard(), { wrapper });
    const handlers = captured.handlers!;

    act(() => {
      handlers.onError(new ApiError(403, '{"error": "not authorized"}'));
    });
    expect(result.current.streamError?.serverMessage).toBe("not authorized");
    expect(result.current.live).toBe(false);

    act(() => {
      handlers.onConnectionChange?.(true);
    });
    expect(result.current.streamError).toBeNull();
    expect(result.current.live).toBe(true);
    unmount();
  });

  test("a dropped connection keeps an error that is still current", () => {
    const { wrapper } = harness([
      { match: (url) => url === "/requests", respond: () => new Response("[]") },
    ]);
    const { result, unmount } = renderHook(() => useRequestBoard(), { wrapper });
    const handlers = captured.handlers!;
    act(() => {
      handlers.onError(new ApiError(403, '{"error": "not authorized"}'));
      handlers.onConnectionChange?.(false);
    });
    expect(result.current.streamError).not.toBeNull();
    unmount();
  });
});
