// Rendering helpers shared by the request-page tests.
import { setOperatorName } from "@/platform/operatorIdentity";
import { type FakeRoute, type FakeServer, fakeServer, renderApp } from "@/test/render";

import { RequestDetailScreen } from "./RequestDetailScreen";
import { type Wire, requestRoute } from "./testRequests";

/** A returning operator: the name prompt does not interrupt a flow. Call from `beforeEach`. */
export function seedOperator(): void {
  setOperatorName("operator");
}

export interface OpenOptions {
  /** Whether the console can write (an override token configured). Default true. */
  readonly writes?: boolean;
  readonly extra?: readonly FakeRoute[];
  readonly id?: string;
}

/** The request page at /requests/req-1 over a fake server whose detail answers `body`. */
export function openRequest(body: Wire | (() => Wire), options: OpenOptions = {}) {
  const id = options.id ?? "req-1";
  const server: FakeServer = fakeServer([requestRoute(body, id), ...(options.extra ?? [])]);
  const view = renderApp(<RequestDetailScreen />, {
    server,
    path: `/requests/${id}`,
    pattern: "/requests/:id",
    config: { writesEnabled: options.writes ?? true },
  });
  return { ...view, server };
}
