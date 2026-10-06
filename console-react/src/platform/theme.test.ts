import { act, renderHook } from "@testing-library/react";

import { initThemeMode, nextThemeMode, useThemeMode } from "@/platform/theme";
import { getStoredThemeMode, setStoredThemeMode } from "@/platform/themeStore";

afterEach(() => {
  window.localStorage.clear();
  initThemeMode();
});

test("the app bar toggle cycles system -> light -> dark -> system and persists each choice", () => {
  initThemeMode();
  const root = document.documentElement;
  const { result } = renderHook(() => useThemeMode());
  expect(result.current[0]).toBe("system");

  for (const expected of ["light", "dark", "system"] as const) {
    act(() => {
      result.current[1](nextThemeMode(result.current[0]));
    });
    expect(result.current[0]).toBe(expected);
    expect(getStoredThemeMode()).toBe(expected);
    if (expected === "system") expect(root).not.toHaveAttribute("data-theme");
    else expect(root).toHaveAttribute("data-theme", expected);
  }
});

test("a previously stored theme mode is restored on launch", () => {
  setStoredThemeMode("dark");
  initThemeMode();
  expect(document.documentElement).toHaveAttribute("data-theme", "dark");
  const { result } = renderHook(() => useThemeMode());
  expect(result.current[0]).toBe("dark");
});
