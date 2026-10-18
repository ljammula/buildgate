// The console token is the credential of the console on the host once
// `serve` requires a token from every caller (`factoryd install-service
// -require-tokens`). `factoryd console` hands it over in the URL fragment
// (`#c=<token>`). It is kept in localStorage, like the start token, so every
// tab and every restart of the browser keeps it: the server accepts it only
// from a request made on the host itself, never through a proxy.
//
// As with the gate token, a linked token is only a candidate until the
// server accepts it (app/session.ts decides).

import { captureFragmentToken } from "@/platform/fragmentToken";

const storageKey = "factoryConsoleToken";
const fragmentKey = "c";

function readStorage(): Storage | null {
  try {
    return window.localStorage;
  } catch {
    return null;
  }
}

/** Returns the console token stored in this browser, if storage is usable. */
export function getStoredConsoleToken(): string | null {
  try {
    const token = readStorage()?.getItem(storageKey) ?? null;
    return token === "" ? null : token;
  } catch {
    return null;
  }
}

/** Keeps the console token; unavailable or private-mode storage is a no-op. */
export function setStoredConsoleToken(token: string): void {
  try {
    readStorage()?.setItem(storageKey, token);
  } catch {
    // Private-mode storage can exist but throw on access.
  }
}

/** Forgets the stored console token: the server no longer accepts it. */
export function clearStoredConsoleToken(): void {
  try {
    readStorage()?.removeItem(storageKey);
  } catch {
    // As above.
  }
}

/**
 * Returns the console token the address bar carries, or null, and removes
 * the `c` component from it. Stores nothing. Call it before
 * captureStartTokenFromLocation, which removes the whole fragment.
 */
export function captureConsoleTokenFromLocation(): string | null {
  return captureFragmentToken(fragmentKey);
}
