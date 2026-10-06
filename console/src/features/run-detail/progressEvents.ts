import type { ProgressEvent } from "@/domain/run";

/**
 * Worker lines (round and agent notes) are bounded so a tab left open on a
 * long build cannot grow without limit; factory lines are few and are what
 * the stepper is built from, so they are always kept.
 */
export const maxWorkerEvents = 4000;

export interface ProgressFeed {
  readonly events: readonly ProgressEvent[];
  /** The key of every line the feed has delivered, kept even after a line is dropped by the cap. */
  readonly seen: ReadonlySet<string>;
}

export const emptyProgressFeed: ProgressFeed = { events: [], seen: new Set() };

/**
 * A line's identity. The server replays the whole file from its start on
 * every reconnect, so without this a transient drop would append the run's
 * entire history a second time.
 */
export function progressKey(event: ProgressEvent): string {
  return [
    event.ts.toISOString(),
    event.source,
    event.stage,
    event.event,
    event.round,
    event.detail,
  ].join("|");
}

/**
 * Adds one delivered line: ignored when its key was already seen; past
 * `maxWorkerEvents` worker lines the oldest worker line is dropped. Pure, so
 * a strict-mode double invocation of a reducer using it is harmless.
 */
export function addProgressEvent(feed: ProgressFeed, event: ProgressEvent): ProgressFeed {
  const key = progressKey(event);
  if (feed.seen.has(key)) return feed;
  const seen = new Set(feed.seen).add(key);
  let events = [...feed.events, event];
  if (event.source === "worker") {
    const workerCount = events.reduce((n, e) => (e.source === "worker" ? n + 1 : n), 0);
    if (workerCount > maxWorkerEvents) {
      const drop = events.findIndex((e) => e.source === "worker");
      if (drop >= 0) events = events.filter((_, i) => i !== drop);
    }
  }
  return { events, seen };
}
