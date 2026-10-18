// The gate token reaches the console pasted into its gate screen
// (app/GateScreen) or in a URL fragment (`#gate=<token>`), which never
// reaches the server or an access log. It is kept in sessionStorage, not
// localStorage: it opens a console reached through a proxy, and it should
// not outlive the tab it was opened in.
//
// Capturing and storing are separate. A pasted or linked token is only a
// candidate until the server accepts it, so a stale or mistyped one cannot
// replace a token that works (app/session.ts decides).

import {
  captureFragmentToken,
  hashWithoutFragmentToken,
  parseFragmentToken,
} from "@/platform/fragmentToken";

const storageKey = "factoryGateToken";
const fragmentKey = "gate";

/** Extracts the exact `gate` component from a window.location.hash-shaped value. */
export function parseGateTokenFromHash(hash: string): string | null {
  return parseFragmentToken(hash, fragmentKey);
}

/**
 * The fragment with every `gate` component removed and the rest kept in
 * order: a start token (`t=`) beside it is still there for
 * platform/startToken to capture. "" when nothing is left.
 */
export function hashWithoutGateToken(hash: string): string {
  return hashWithoutFragmentToken(hash, fragmentKey);
}

function readStorage(): Storage | null {
  try {
    return window.sessionStorage;
  } catch {
    return null;
  }
}

/** Returns the gate token stored for this tab, if storage is usable. */
export function getStoredGateToken(): string | null {
  try {
    const token = readStorage()?.getItem(storageKey) ?? null;
    return token === "" ? null : token;
  } catch {
    return null;
  }
}

/** Keeps the gate token for this tab; unavailable or private-mode storage is a no-op. */
export function setStoredGateToken(token: string): void {
  try {
    readStorage()?.setItem(storageKey, token);
  } catch {
    // Private-mode storage can exist but throw on access.
  }
}

/** Forgets the stored gate token: the server no longer accepts it. */
export function clearStoredGateToken(): void {
  try {
    readStorage()?.removeItem(storageKey);
  } catch {
    // As above.
  }
}

/**
 * Returns the gate token the address bar carries, or null, and removes the
 * `gate` component from it (platform/fragmentToken). Stores nothing.
 */
export function captureGateTokenFromLocation(): string | null {
  return captureFragmentToken(fragmentKey);
}
