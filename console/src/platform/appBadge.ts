// The installed console's badge: the count on its Dock or taskbar icon (the
// Badging API). Only a console installed as an app shows one; a browser tab
// without the API, or one that refuses the call, is left as it is.

/**
 * Shows `count` on the app icon, or clears it for zero and for an unknown
 * count (null): no number is better than a stale one.
 */
export function setAppBadge(count: number | null): void {
  try {
    if (typeof navigator === "undefined" || !("setAppBadge" in navigator)) return;
    const done =
      count !== null && count > 0 ? navigator.setAppBadge(count) : navigator.clearAppBadge();
    // A browser may refuse the badge (no permission, not installed).
    done.catch(() => undefined);
  } catch {
    // Some browsers throw instead of rejecting.
  }
}
