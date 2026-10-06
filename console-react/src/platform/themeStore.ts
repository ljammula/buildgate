// Persists the operator's manual dark/light/system choice across reloads.

export type ThemeMode = "system" | "light" | "dark";

const storageKey = "factoryThemeMode";
const modes: readonly ThemeMode[] = ["system", "light", "dark"];

function storage(): Storage | null {
  try {
    return window.localStorage;
  } catch {
    return null;
  }
}

/** Returns the stored theme mode, rejecting values outside ThemeMode. */
export function getStoredThemeMode(): ThemeMode | null {
  try {
    const value = storage()?.getItem(storageKey);
    return value !== null && modes.includes(value as ThemeMode) ? (value as ThemeMode) : null;
  } catch {
    return null;
  }
}

/** Persists the selected theme mode; unavailable storage is a no-op. */
export function setStoredThemeMode(mode: ThemeMode): void {
  try {
    storage()?.setItem(storageKey, mode);
  } catch {
    // Storage may throw in private browsing mode.
  }
}
