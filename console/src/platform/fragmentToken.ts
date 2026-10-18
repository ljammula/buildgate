// A token handed to the console in one component of the URL fragment,
// `<key>=<token>`, which never reaches the server or an access log. The gate
// token (`gate=`) and the console token (`c=`) arrive this way, beside the
// start token (`t=`).

function fragmentParts(hash: string): string[] {
  const value = hash.startsWith("#") ? hash.slice(1) : hash;
  return value === "" ? [] : value.split("&");
}

function isPart(part: string, key: string): boolean {
  const equals = part.indexOf("=");
  return equals >= 0 && part.slice(0, equals) === key;
}

/** Extracts the exact `key` component from a window.location.hash-shaped value. */
export function parseFragmentToken(hash: string, key: string): string | null {
  const part = fragmentParts(hash).find((p) => isPart(p, key));
  if (part === undefined) return null;
  let token: string;
  try {
    token = decodeURIComponent(part.slice(key.length + 1));
  } catch {
    // A malformed escape is not a token; it must not stop the console starting.
    return null;
  }
  return token === "" ? null : token;
}

/**
 * The fragment with every `key` component removed and the rest kept in
 * order: a start token (`t=`) beside it is still there for
 * platform/startToken to capture. "" when nothing is left.
 */
export function hashWithoutFragmentToken(hash: string, key: string): string {
  const rest = fragmentParts(hash).filter((part) => !isPart(part, key));
  return rest.length === 0 ? "" : `#${rest.join("&")}`;
}

/**
 * Returns the `key` token the address bar carries, or null, and always
 * removes the `key` component from it (a malformed or empty one too), so it
 * does not linger in history, bookmarks or referrers. Path, query and every
 * other fragment component are kept. Stores nothing.
 *
 * Call it before captureStartTokenFromLocation, which removes the whole
 * fragment once it has found its own component.
 */
export function captureFragmentToken(key: string): string | null {
  const hash = window.location.hash;
  if (!fragmentParts(hash).some((part) => isPart(part, key))) return null;
  const token = parseFragmentToken(hash, key);
  window.history.replaceState(
    null,
    "",
    window.location.pathname + window.location.search + hashWithoutFragmentToken(hash, key),
  );
  return token;
}
