// The Mission Control health strip: what the worker is doing right now, read
// from GET /queue-run and the request list. Pure.
import { compareTimestamps } from "@/domain/elapsed";
import { type QueueRunStatus, type WorkerLiveness, workerLiveness } from "@/domain/ops";
import { type RequestSummary, requestShortTitle } from "@/domain/request";
import { stateLabel } from "@/domain/status";

/** One request the worker runs a job for now. */
export interface RunningJob {
  readonly requestId: string;
  /** The request's short title; its id when the list does not (yet) hold it. */
  readonly title: string;
  /** "ticket 2 of 3 · round 1 · verify" for a build, the stage's name otherwise; "" when unknown. */
  readonly detail: string;
  /** The server's stalled verdict on the build. */
  readonly stalled: boolean;
}

export interface FactoryHealth {
  /** Null while GET /queue-run has not answered (or cannot: an older server). */
  readonly worker: WorkerLiveness | null;
  /** The worker's last heartbeat; "" when it never wrote one. */
  readonly lastHeartbeat: string;
  /** Jobs running and the worker's capacity; null unless a live worker reports its slots. */
  readonly slots: { readonly busy: number; readonly total: number } | null;
  readonly running: readonly RunningJob[];
  /**
   * Requests waiting for a slot of a live worker (those the server gave a
   * queue position). Null unless the worker is alive: the server gives no
   * position without one, and "0 queued" behind a dead worker would read as
   * nothing waiting.
   */
  readonly queued: number | null;
  /**
   * Requests in a state only a worker can advance, whatever the worker's
   * state: what is stuck when there is none.
   */
  readonly needWorker: number;
  /** When any request last changed state; null when no request has a history. */
  readonly lastTransitionAt: string | null;
}

function runningJob(id: string, requests: readonly RequestSummary[]): RunningJob {
  const request = requests.find((r) => r.id === id);
  if (request === undefined) return { requestId: id, title: id, detail: "", stalled: false };
  const build = request.build;
  const detail =
    build === null
      ? stateLabel(request.state)
      : [
          `ticket ${build.ticket} of ${build.tickets}`,
          build.round > 0 ? `round ${build.round}` : "",
          build.stage,
        ]
          .filter((part) => part !== "")
          .join(" · ");
  return {
    requestId: id,
    title: requestShortTitle(request),
    detail,
    stalled: build?.stalled ?? false,
  };
}

/**
 * The request states only a worker's job advances: drafting, planning and
 * building. Every review state waits on the operator and every terminal state
 * on nothing, so a request in one of these is what stands still without a
 * live worker. The one list: the worker strip and the health strip both read
 * it.
 */
export const workerJobStates: ReadonlySet<string> = new Set([
  "submitted",
  "spec_drafting",
  "oracle_drafting",
  "planning",
  "building",
]);

/** The newest `at` over every request's history; null when there is none. */
export function lastTransitionAt(requests: readonly RequestSummary[]): string | null {
  let latest: string | null = null;
  for (const request of requests) {
    for (const move of request.history) {
      if (latest === null || compareTimestamps(move.at, latest) > 0) latest = move.at;
    }
  }
  return latest;
}

export function factoryHealth(
  status: QueueRunStatus | null,
  requests: readonly RequestSummary[],
): FactoryHealth {
  const worker = status === null ? null : workerLiveness(status.state);
  const active = status !== null && worker?.alive === true ? status.activeRequests : [];
  return {
    worker,
    lastHeartbeat: status?.lastHeartbeat ?? "",
    slots:
      status !== null && worker?.alive === true && status.jobSlots > 0
        ? { busy: active.length, total: status.jobSlots }
        : null,
    running: active.map((id) => runningJob(id, requests)),
    queued: worker?.alive === true ? requests.filter((r) => r.queuePosition !== null).length : null,
    needWorker: requests.filter((r) => workerJobStates.has(r.state)).length,
    lastTransitionAt: lastTransitionAt(requests),
  };
}
