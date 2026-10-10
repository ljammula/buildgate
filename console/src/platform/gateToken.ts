// The gate token reaches the console pasted into its gate screen
// (app/GateScreen) or in a URL fragment (`#gate=<token>`), which never
// reaches the server or an access log. It is kept in sessionStorage, not
// localStorage: it opens a console reached through a proxy, and it should
// not outlive the tab it was opened in.
//
// Capturing and storing are separate. A pasted or linked token is only a
// candidate until the server accepts it, so a stale or mistyped one cannot
// replace a token that works (app/session.ts decides).

const storageKey = "factoryGateToken";
const fragmentKey = "gate";

function fragmentParts(hash: string): string[] {
  const value = hash.startsWith("#") ? hash.slice(1) : hash;
  return value === "" ? [] : value.split("&");
}

function isGatePart(part: string): boolean {
  const equals = part.indexOf("=");
  return equals >= 0 && part.slice(0, equals) === fragmentKey;
}

/** Extracts the exact `gate` component from a window.location.hash-shaped value. */
export function parseGateTokenFromHash(hash: string): string | null {
  const part = fragmentParts(hash).find(isGatePart);
  if (part === undefined) return null;
  let token: string;
  try {
    token = decodeURIComponent(part.slice(fragmentKey.length + 1));
  } catch {
    // A malformed escape is not a token; it must not stop the console starting.
    return null;
  }
  return token === "" ? null : token;
}

/**
 * The fragment with every `gate` component removed and the rest kept in
 * order: a start token (`t=`) beside it is still there for
 * platform/startToken to capture. "" when nothing is left.
 */
export function hashWithoutGateToken(hash: string): string {
  const rest = fragmentParts(hash).filter((part) => !isGatePart(part));
  return rest.length === 0 ? "" : `#${rest.join("&")}`;
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
 * Returns the gate token the address bar carries, or null, and always
 * removes the `gate` component from it (a malformed or empty one too), so it
 * does not linger in history, bookmarks or referrers. Path, query and every
 * other fragment component are kept. Stores nothing.
 *
 * Call it before captureStartTokenFromLocation, which removes the whole
 * fragment once it has found its own component.
 */
export function captureGateTokenFromLocation(): string | null {
  const hash = window.location.hash;
  if (!fragmentParts(hash).some(isGatePart)) return null;
  const token = parseGateTokenFromHash(hash);
  window.history.replaceState(
    null,
    "",
    window.location.pathname + window.location.search + hashWithoutGateToken(hash),
  );
  return token;
}
