/**
 * `value` as an absolute http(s) URL, or null. A link built from data the
 * console did not write (a pull request URL, the configured Temporal UI
 * address) is only made clickable through this: any other scheme, such as
 * `javascript:` or `data:`, would run or load in the console's own origin,
 * where the start token is stored.
 */
export function safeHttpUrl(value: string): string | null {
  const trimmed = value.trim();
  if (!/^https?:\/\//i.test(trimmed)) return null;
  try {
    const url = new URL(trimmed);
    return url.protocol === "http:" || url.protocol === "https:" ? url.href : null;
  } catch {
    return null;
  }
}
