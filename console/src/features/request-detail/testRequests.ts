// Wire bodies the request-page tests serve, as objects a test adjusts before serving.
import { readFixtureJson } from "@/test/fixtures";
import { requestJson } from "@/test/requestFixtures";
import { type FakeRoute, json } from "@/test/render";

export type Wire = Record<string, unknown>;

export interface RequestWireOptions {
  readonly id?: string;
  readonly state: string;
  readonly title?: string;
  readonly spec?: string;
  readonly [key: string]: unknown;
}

/** A `GET /requests/{id}` body with these defaults. */
export function requestWire({ id = "req-1", state, ...rest }: RequestWireOptions): Wire {
  return {
    ...requestJson({ id, state }),
    error: "",
    spec: "",
    approved_by: "",
    approved_at: "",
    history: [],
    next_action: "",
    approve_next_state: "",
    ...rest,
  };
}

export function ticketWire(o: {
  index: number;
  specPath?: string;
  runId?: string;
  prUrl?: string;
  prState?: string;
  content?: string;
}): Wire {
  return {
    index: o.index,
    spec_path: o.specPath ?? "",
    run_id: o.runId ?? "",
    pr_url: o.prUrl ?? "",
    pr_state: o.prState ?? "",
    content: o.content ?? "",
  };
}

export function historyWire(o: {
  from: string;
  to: string;
  at: string;
  by: string;
  reason?: string;
}): Wire {
  return { from: o.from, to: o.to, at: o.at, by: o.by, reason: o.reason ?? "" };
}

export function rejectionWire(o: {
  by: string;
  at: string;
  reason: string;
  fromState: string;
}): Wire {
  return { by: o.by, at: o.at, reason: o.reason, from_state: o.fromState };
}

export function revisionWire(o: {
  index: number;
  at: string;
  by: string;
  reason: string;
  fromState: string;
  files: readonly string[] | Record<string, string>;
}): Wire {
  return {
    index: o.index,
    at: o.at,
    by: o.by,
    reason: o.reason,
    from_state: o.fromState,
    files: o.files,
  };
}

/** A committed run fixture with another id. */
export function runWire(fixture: string, patch: Wire = {}): Wire {
  return { ...(readFixtureJson(`api/${fixture}`) as Wire), ...patch };
}

/** The route of the request itself. The events stream answers 404, which the page treats as "no live updates". */
export function requestRoute(body: Wire | (() => Wire), id = "req-1"): FakeRoute {
  return {
    on: `GET /requests/${id}`,
    reply: () => json(typeof body === "function" ? body() : body),
  };
}
