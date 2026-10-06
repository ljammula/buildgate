import type { QueueRunStatus } from "@/domain/ops";
import type { RequestSummary } from "@/domain/request";

// States only a live worker can advance (the Go side's queueRunDependentState):
// every *_review state waits on the operator and every terminal state needs
// nothing further.
const QUEUE_RUN_DEPENDENT_STATES: ReadonlySet<string> = new Set([
  "submitted",
  "spec_drafting",
  "oracle_drafting",
  "planning",
  "building",
]);

/** "Ns/Nm/Nh" since a heartbeat; null when it is not a parseable timestamp. */
function heartbeatAge(lastHeartbeat: string, now: Date): string | null {
  const at = Date.parse(lastHeartbeat);
  if (Number.isNaN(at)) return null;
  const seconds = Math.max(0, Math.floor((now.getTime() - at) / 1000));
  if (seconds >= 3600) return `${Math.floor(seconds / 3600)}h`;
  if (seconds >= 60) return `${Math.floor(seconds / 60)}m`;
  return `${seconds}s`;
}

/**
 * The worker strip's text, or null when it stays hidden. `stale` (the worker
 * ran and stopped refreshing its heartbeat) always warns; `absent` (no worker
 * ever ran against this data dir) warns only when a request is waiting on
 * one, so an empty board on a fresh data dir does not warn about a worker
 * nobody has started. `alive`, and a call that failed or 404ed (null), never
 * warn: this is a best-effort signal, not worth a false alarm on an older
 * server or a network blip.
 */
export function queueRunWarning(
  status: QueueRunStatus | null,
  requests: readonly RequestSummary[],
  now: Date,
): string | null {
  if (status === null) return null;
  if (status.state === "stale") {
    const age = heartbeatAge(status.lastHeartbeat, now);
    const suffix = age === null ? "" : ` (last heartbeat ${age} ago)`;
    return `worker is not running${suffix} -- requests won't advance; start \`factoryd worker\``;
  }
  if (status.state === "absent") {
    if (!requests.some((r) => QUEUE_RUN_DEPENDENT_STATES.has(r.state))) return null;
    return (
      "no worker has run against this data dir -- requests won't advance; " +
      "start `factoryd worker`"
    );
  }
  return null;
}

/** The live-connection state of the board's event stream; display only. */
export type BoardFreshness = "live" | "recent" | "disconnected";

export function boardFreshness(input: {
  readonly live: boolean;
  readonly disconnected: boolean;
}): BoardFreshness {
  if (input.live) return "live";
  return input.disconnected ? "disconnected" : "recent";
}

/** "Live" / "Last updated Ns ago" / "Connecting…" / "Disconnected". */
export function freshnessLabel(
  freshness: BoardFreshness,
  lastUpdateAtMs: number | null,
  now: Date,
): string {
  switch (freshness) {
    case "live":
      return "Live";
    case "disconnected":
      return "Disconnected";
    case "recent":
      return lastUpdateAtMs === null || lastUpdateAtMs === 0
        ? "Connecting…"
        : `Last updated ${Math.max(0, Math.floor((now.getTime() - lastUpdateAtMs) / 1000))}s ago`;
  }
}
