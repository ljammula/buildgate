// The operator name is recorded once per browser and sent as `by` on later
// approve/reject actions. Storage is optional because private-mode browsers
// may expose it while throwing when it is accessed.

const storageKey = "factoryOperatorName";

function storage(): Storage | null {
  try {
    return window.localStorage;
  } catch {
    return null;
  }
}

/** Returns the stored operator name, or null when browser storage is absent. */
export function getOperatorName(): string | null {
  try {
    return storage()?.getItem(storageKey) ?? null;
  } catch {
    return null;
  }
}

/** Persists the operator name; unavailable storage is a no-op. */
export function setOperatorName(name: string): void {
  try {
    storage()?.setItem(storageKey, name);
  } catch {
    // Storage may throw in private browsing mode.
  }
}
