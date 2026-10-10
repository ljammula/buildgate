// What the operator chose on Mission Control, kept per browser: the Board or
// List view, and which project lanes are collapsed. Storage is optional, as
// for every preference: private-mode browsers may throw when it is touched.

export type BoardView = "board" | "list";

const viewKey = "factoryBoardView";
const lanesKey = "factoryBoardCollapsedLanes";
const views: readonly BoardView[] = ["board", "list"];

function storage(): Storage | null {
  try {
    return window.localStorage;
  } catch {
    return null;
  }
}

/** The stored view, or null when none (or something else) is stored. */
export function getStoredBoardView(): BoardView | null {
  try {
    const value = storage()?.getItem(viewKey);
    return value !== null && views.includes(value as BoardView) ? (value as BoardView) : null;
  } catch {
    return null;
  }
}

export function setStoredBoardView(view: BoardView): void {
  try {
    storage()?.setItem(viewKey, view);
  } catch {
    // Storage may throw in private browsing mode.
  }
}

/** The projects whose lane is collapsed; empty when nothing readable is stored. */
export function getCollapsedLanes(): string[] {
  try {
    const parsed: unknown = JSON.parse(storage()?.getItem(lanesKey) ?? "[]");
    return Array.isArray(parsed)
      ? parsed.filter((item): item is string => typeof item === "string")
      : [];
  } catch {
    return [];
  }
}

export function setCollapsedLanes(projects: readonly string[]): void {
  try {
    storage()?.setItem(lanesKey, JSON.stringify([...projects].sort()));
  } catch {
    // Storage may throw in private browsing mode.
  }
}
