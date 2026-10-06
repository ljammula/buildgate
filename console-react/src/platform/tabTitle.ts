// The title is the ambient "needs you" signal for a board left in a
// background tab. A failed poll must not present a stale count as current.

import { setFaviconBadge } from "@/platform/faviconBadge";

/** Returns the title for the last successful count and latest poll result. */
export function needsHumanTabTitle(needsHumanCount: number | null, pollFailed: boolean): string {
  if (pollFailed) return "(?) Buildgate";
  if (needsHumanCount !== null && needsHumanCount > 0) {
    return `(${needsHumanCount}) Buildgate`;
  }
  return "Buildgate";
}

/** Updates document.title and the tab icon's count badge. */
export function updateNeedsHumanSignal(needsHumanCount: number | null, pollFailed: boolean): void {
  document.title = needsHumanTabTitle(needsHumanCount, pollFailed);
  setFaviconBadge(needsHumanCount ?? 0);
}
