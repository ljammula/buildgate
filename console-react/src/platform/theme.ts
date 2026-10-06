import { useCallback, useSyncExternalStore } from "react";

import { type ThemeMode, getStoredThemeMode, setStoredThemeMode } from "@/platform/themeStore";

// The operator's choice lives in one place for the page: every component
// that shows or changes it subscribes here.
let current: ThemeMode = getStoredThemeMode() ?? "system";
const listeners = new Set<() => void>();

/**
 * Applies a mode to the document: an explicit choice sets
 * `data-theme`; "system" removes it so the stylesheet follows the operating
 * system's preference.
 */
export function applyThemeMode(mode: ThemeMode): void {
  const root = document.documentElement;
  if (mode === "system") root.removeAttribute("data-theme");
  else root.setAttribute("data-theme", mode);
}

export function initThemeMode(): void {
  current = getStoredThemeMode() ?? "system";
  applyThemeMode(current);
}

function setThemeMode(mode: ThemeMode): void {
  current = mode;
  setStoredThemeMode(mode);
  applyThemeMode(mode);
  for (const listener of listeners) listener();
}

function subscribe(listener: () => void): () => void {
  listeners.add(listener);
  return () => listeners.delete(listener);
}

/** system -> light -> dark -> system, the order the toggle cycles in. */
export function nextThemeMode(mode: ThemeMode): ThemeMode {
  switch (mode) {
    case "system":
      return "light";
    case "light":
      return "dark";
    case "dark":
      return "system";
  }
}

export function useThemeMode(): readonly [ThemeMode, (mode: ThemeMode) => void] {
  const mode = useSyncExternalStore(subscribe, () => current);
  const set = useCallback((next: ThemeMode) => {
    setThemeMode(next);
  }, []);
  return [mode, set];
}
