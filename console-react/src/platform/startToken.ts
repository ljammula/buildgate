// The start token arrives in a URL fragment so it never reaches the server or
// an access log. Capture it into localStorage, then remove the fragment from
// the address bar so it does not linger in history, bookmarks, or referrers.

const storageKey = "factoryStartToken";

/** Extracts the exact `t` component from a window.location.hash-shaped value. */
export function parseStartTokenFromHash(hash: string): string | null {
  const value = hash.startsWith("#") ? hash.slice(1) : hash;
  if (value === "") return null;
  for (const part of value.split("&")) {
    const equals = part.indexOf("=");
    if (equals < 0 || part.slice(0, equals) !== "t") continue;
    let token: string;
    try {
      token = decodeURIComponent(part.slice(equals + 1));
    } catch {
      // A malformed escape is not a token; it must not stop the console starting.
      return null;
    }
    return token === "" ? null : token;
  }
  return null;
}

function readStorage(): Storage | null {
  try {
    return window.localStorage;
  } catch {
    return null;
  }
}

/** Returns the start token last stored in this browser, if storage is usable. */
export function getStoredStartToken(): string | null {
  try {
    return readStorage()?.getItem(storageKey) ?? null;
  } catch {
    return null;
  }
}

/** Persists the start token; unavailable or private-mode storage is a no-op. */
export function setStoredStartToken(token: string): void {
  try {
    readStorage()?.setItem(storageKey, token);
  } catch {
    // Private-mode storage can exist but throw on access.
  }
}

/** Captures `#t=...` and strips only the fragment, preserving path and query. */
export function captureStartTokenFromLocation(): void {
  const token = parseStartTokenFromHash(window.location.hash);
  if (token === null) return;
  setStoredStartToken(token);
  window.history.replaceState(null, "", window.location.pathname + window.location.search);
}
