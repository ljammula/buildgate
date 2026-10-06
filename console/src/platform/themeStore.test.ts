import { getStoredThemeMode, setStoredThemeMode, type ThemeMode } from "@/platform/themeStore";

afterEach(() => {
  window.localStorage.clear();
});

test("starts unset", () => {
  expect(getStoredThemeMode()).toBeNull();
});

test("stores each theme mode", () => {
  const modes: readonly ThemeMode[] = ["system", "light", "dark"];
  for (const mode of modes) {
    setStoredThemeMode(mode);
    expect(getStoredThemeMode()).toBe(mode);
  }
});

test("rejects a stored value that is not a theme mode", () => {
  window.localStorage.setItem("factoryThemeMode", "sepia");
  expect(getStoredThemeMode()).toBeNull();
});

test("uses the exact storage key", () => {
  window.localStorage.setItem("factoryThemeModeOther", "dark");
  expect(getStoredThemeMode()).toBeNull();
});
