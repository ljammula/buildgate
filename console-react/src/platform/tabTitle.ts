// The title is the ambient "needs you" signal for a board left in a
// background tab. A failed poll must not present a stale count as current.

declare global {
  interface Window {
    __factoryFavicon?: {
      readonly setBadge: (count: number) => void;
    };
  }
}

/** Returns the title for the last successful count and latest poll result. */
export function needsHumanTabTitle(needsHumanCount: number | null, pollFailed: boolean): string {
  if (pollFailed) return "(?) Factory Console";
  if (needsHumanCount !== null && needsHumanCount > 0) {
    return `(${needsHumanCount}) Factory Console`;
  }
  return "Factory Console";
}

/** Updates document.title and the optional favicon badge helper. */
export function updateNeedsHumanSignal(needsHumanCount: number | null, pollFailed: boolean): void {
  document.title = needsHumanTabTitle(needsHumanCount, pollFailed);
  const favicon = window.__factoryFavicon;
  favicon?.setBadge(needsHumanCount ?? 0);
}
