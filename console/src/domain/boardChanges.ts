// Which board cards changed since the last list the screen held. Pure.
import type { RequestSummary } from "@/domain/request";

/** Request id to the state it was last seen in. */
export type BoardSnapshot = ReadonlyMap<string, string>;

export interface BoardChanges {
  /** Requests whose state differs from the snapshot, or that it did not hold. */
  readonly changed: ReadonlySet<string>;
  /** The snapshot to compare the next list against. */
  readonly next: BoardSnapshot;
}

/**
 * The first load (`previous` null) changes nothing: a page that has just
 * opened highlights nothing. A request that left the list is dropped from
 * the snapshot, so one that comes back counts as new.
 */
export function boardChanges(
  previous: BoardSnapshot | null,
  requests: readonly RequestSummary[],
): BoardChanges {
  const next = new Map<string, string>();
  const changed = new Set<string>();
  for (const request of requests) {
    next.set(request.id, request.state);
    if (previous !== null && previous.get(request.id) !== request.state) changed.add(request.id);
  }
  return { changed, next };
}
