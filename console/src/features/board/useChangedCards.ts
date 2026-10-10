import { useEffect, useRef, useState } from "react";

import { type BoardSnapshot, boardChanges } from "@/domain/boardChanges";
import type { RequestSummary } from "@/domain/request";

/** How long a card stays marked changed; the highlight's own animation is 1500ms. */
export const changedCardMs = 1600;

const none: ReadonlySet<string> = new Set();

/**
 * The ids of the requests whose state changed (or that appeared) in the list
 * while the screen was open, for `changedCardMs` each. The first list marks
 * nothing. A poll that changes nothing leaves the set as it is, so a running
 * highlight is not restarted. Memory for the visit only.
 */
export function useChangedCards(
  requests: readonly RequestSummary[] | undefined,
): ReadonlySet<string> {
  const snapshot = useRef<BoardSnapshot | null>(null);
  const timers = useRef(new Map<string, ReturnType<typeof setTimeout>>());
  const [changed, setChanged] = useState<ReadonlySet<string>>(none);

  useEffect(() => {
    if (requests === undefined) return;
    const result = boardChanges(snapshot.current, requests);
    snapshot.current = result.next;
    if (result.changed.size === 0) return;
    for (const id of result.changed) {
      // A second change within the window restarts that id's clock.
      clearTimeout(timers.current.get(id));
      timers.current.set(
        id,
        setTimeout(() => {
          timers.current.delete(id);
          setChanged((current) => {
            const next = new Set(current);
            next.delete(id);
            return next;
          });
        }, changedCardMs),
      );
    }
    setChanged((current) => new Set([...current, ...result.changed]));
  }, [requests]);

  useEffect(() => {
    const pending = timers.current;
    return () => {
      for (const timer of pending.values()) clearTimeout(timer);
      pending.clear();
    };
  }, []);

  return changed;
}
