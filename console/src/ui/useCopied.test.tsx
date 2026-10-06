import { act, renderHook } from "@testing-library/react";

import { copiedResetMs, useCopied } from "@/ui/useCopied";

afterEach(() => {
  vi.useRealTimers();
});

describe("useCopied", () => {
  test("copy writes the text, sets copied, and clears it after copiedResetMs", async () => {
    vi.useFakeTimers();
    const writeText = vi.fn(() => Promise.resolve());
    const { result } = renderHook(() => useCopied(writeText));
    expect(result.current.copied).toBe(false);

    await act(() => result.current.copy("abc"));
    expect(writeText).toHaveBeenCalledWith("abc");
    expect(result.current.copied).toBe(true);

    act(() => {
      vi.advanceTimersByTime(copiedResetMs - 1);
    });
    expect(result.current.copied).toBe(true);
    act(() => {
      vi.advanceTimersByTime(1);
    });
    expect(result.current.copied).toBe(false);
  });

  test("a second copy restarts the reset window", async () => {
    vi.useFakeTimers();
    const { result } = renderHook(() => useCopied(() => Promise.resolve()));
    await act(() => result.current.copy("a"));
    act(() => {
      vi.advanceTimersByTime(copiedResetMs - 500);
    });
    await act(() => result.current.copy("b"));
    act(() => {
      vi.advanceTimersByTime(copiedResetMs - 1);
    });
    expect(result.current.copied).toBe(true);
    act(() => {
      vi.advanceTimersByTime(1);
    });
    expect(result.current.copied).toBe(false);
  });

  test("a refused write leaves copied false and does not throw", async () => {
    vi.useFakeTimers();
    const { result } = renderHook(() => useCopied(() => Promise.reject(new Error("denied"))));
    await act(() => result.current.copy("x"));
    expect(result.current.copied).toBe(false);
    expect(vi.getTimerCount()).toBe(0);
  });

  test("unmounting clears the timer", async () => {
    vi.useFakeTimers();
    const { result, unmount } = renderHook(() => useCopied(() => Promise.resolve()));
    await act(() => result.current.copy("x"));
    expect(vi.getTimerCount()).toBe(1);
    unmount();
    expect(vi.getTimerCount()).toBe(0);
  });

  test("a write that settles after unmount starts no timer", async () => {
    vi.useFakeTimers();
    let settle: () => void = () => undefined;
    const pending = new Promise<void>((resolve) => {
      settle = resolve;
    });
    const { result, unmount } = renderHook(() => useCopied(() => pending));
    const copying = result.current.copy("x");
    unmount();
    settle();
    await copying;
    expect(vi.getTimerCount()).toBe(0);
  });
});
