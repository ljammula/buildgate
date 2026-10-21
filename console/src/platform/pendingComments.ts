// Comments an operator has written on passages of a spec or plan under review
// and not sent yet, kept per browser until "Request changes" sends them as one
// batch. A batch belongs to one stage of one request (its id and the time it
// entered the stage), so a redraft starts with none: the passages it was
// written against are gone. Storage is optional, as for every preference:
// private-mode browsers may throw when it is touched, and the batch then
// lives for the page only.

import type { RejectionAnchor } from "@/domain/request";

const prefix = "factoryPendingComments:";

const batches = new Map<string, readonly RejectionAnchor[]>();
const listeners = new Set<() => void>();
const none: readonly RejectionAnchor[] = [];

/** The key of one request's batch for the stage it entered at `enteredAt`. */
export function pendingKey(requestId: string, enteredAt: string): string {
  return `${requestId}@${enteredAt}`;
}

function storage(): Storage | null {
  try {
    return window.localStorage;
  } catch {
    return null;
  }
}

function decode(value: unknown): RejectionAnchor | null {
  if (typeof value !== "object" || value === null) return null;
  const o = value as Record<string, unknown>;
  const { path, section, item, note, quote } = o;
  if (typeof path !== "string" || typeof note !== "string") return null;
  return {
    path,
    section: typeof section === "string" ? section : "",
    item: typeof item === "number" ? item : 0,
    note,
    quote: typeof quote === "string" ? quote : "",
  };
}

function load(key: string): readonly RejectionAnchor[] {
  try {
    const parsed: unknown = JSON.parse(storage()?.getItem(prefix + key) ?? "[]");
    if (!Array.isArray(parsed)) return none;
    const comments = parsed.map(decode).filter((c): c is RejectionAnchor => c !== null);
    return comments.length === 0 ? none : comments;
  } catch {
    return none;
  }
}

/** The batch for `key`; the same array until it changes, so a component can subscribe to it. */
export function getPendingComments(key: string): readonly RejectionAnchor[] {
  let batch = batches.get(key);
  if (batch === undefined) {
    batch = load(key);
    batches.set(key, batch);
  }
  return batch;
}

/** The batch for `key` read again from storage, replacing what is held in memory. */
export function freshPendingComments(key: string): readonly RejectionAnchor[] {
  batches.set(key, load(key));
  return getPendingComments(key);
}

/** Drops the batches of the same request written for an earlier stage entry: their passages are gone. */
function pruneOlder(key: string): void {
  const at = key.lastIndexOf("@");
  const own = key.slice(0, at + 1);
  try {
    const store = storage();
    if (store === null) return;
    for (const stored of Object.keys(store)) {
      if (stored.startsWith(prefix + own) && stored !== prefix + key) store.removeItem(stored);
    }
  } catch {
    // Nothing readable to prune.
  }
  for (const held of [...batches.keys()]) {
    if (held.startsWith(own) && held !== key) batches.delete(held);
  }
}

function setPendingComments(key: string, comments: readonly RejectionAnchor[]): void {
  batches.set(key, comments.length === 0 ? none : comments);
  try {
    if (comments.length === 0) storage()?.removeItem(prefix + key);
    else {
      pruneOlder(key);
      storage()?.setItem(prefix + key, JSON.stringify(comments));
    }
  } catch {
    // Storage may throw in private browsing mode; the batch stays in memory.
  }
  for (const listener of [...listeners]) listener();
}

// Another tab changed a batch: forget what this one holds and tell its readers.
if (typeof window !== "undefined") {
  window.addEventListener("storage", (event) => {
    if (event.key !== null && !event.key.startsWith(prefix)) return;
    if (event.key === null) batches.clear();
    else batches.delete(event.key.slice(prefix.length));
    for (const listener of [...listeners]) listener();
  });
}

export function addPendingComment(key: string, comment: RejectionAnchor): void {
  setPendingComments(key, [...getPendingComments(key), comment]);
}

export function removePendingComment(key: string, index: number): void {
  setPendingComments(
    key,
    getPendingComments(key).filter((_, i) => i !== index),
  );
}

export function clearPendingComments(key: string): void {
  setPendingComments(key, none);
}

export function subscribePendingComments(listener: () => void): () => void {
  listeners.add(listener);
  return () => {
    listeners.delete(listener);
  };
}

/** Forgets every batch, in memory and in storage, for a test. */
export function clearAllPendingCommentsForTest(): void {
  batches.clear();
  try {
    const store = storage();
    if (store === null) return;
    for (const key of Object.keys(store)) if (key.startsWith(prefix)) store.removeItem(key);
  } catch {
    // Nothing stored that could be read.
  }
}
