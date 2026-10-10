import { act, renderHook } from "@testing-library/react";

import { requestSummary } from "@/test/requestFixtures";

import { changedCardMs, useChangedCards } from "./useChangedCards";

const req = (id: string, state: string) => requestSummary({ id, state });

describe("useChangedCards", () => {
  beforeEach(() => {
    vi.useFakeTimers();
  });
  afterEach(() => {
    vi.useRealTimers();
  });

  test("marks nothing on the first list, a changed state for 1600ms, and nothing for an unchanged poll", () => {
    const { result, rerender } = renderHook(({ list }) => useChangedCards(list), {
      initialProps: { list: [req("a", "building")] },
    });
    expect([...result.current]).toEqual([]);

    rerender({ list: [req("a", "pr_review")] });
    expect([...result.current]).toEqual(["a"]);

    // A poll that changes nothing neither adds nor restarts anything.
    act(() => {
      vi.advanceTimersByTime(1000);
    });
    rerender({ list: [req("a", "pr_review")] });
    act(() => {
      vi.advanceTimersByTime(changedCardMs - 1000 - 1);
    });
    expect([...result.current]).toEqual(["a"]);
    act(() => {
      vi.advanceTimersByTime(1);
    });
    expect([...result.current]).toEqual([]);
  });

  test("a new request is marked", () => {
    const { result, rerender } = renderHook(({ list }) => useChangedCards(list), {
      initialProps: { list: [req("a", "building")] },
    });
    rerender({ list: [req("a", "building"), req("b", "submitted")] });
    expect([...result.current]).toEqual(["b"]);
  });

  test("clears its timers on unmount", () => {
    const { rerender, unmount } = renderHook(({ list }) => useChangedCards(list), {
      initialProps: { list: [req("a", "building")] },
    });
    rerender({ list: [req("a", "done")] });
    unmount();
    expect(vi.getTimerCount()).toBe(0);
  });
});
