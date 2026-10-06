// Pure route parsing/building for every addressable console path. Browser
// history and location are deliberately outside this module.

import {
  type RequestBoardFilters,
  emptyRequestBoardFilters,
  filtersFromSearchParams,
  toQueryParameters,
} from "@/domain/boardFilters";

export type RequestBoardDeepLink =
  | "none"
  | "requestDetail"
  | "runDetail"
  | "projectRelease"
  | "runList"
  | "projectStats"
  | "ops"
  | "newRequest";

export interface RequestBoardRouteConfig {
  readonly filters: RequestBoardFilters;
  readonly triage: boolean;
  readonly deepLink: RequestBoardDeepLink;
  readonly deepLinkId: string | null;
}

const emptyFilters = emptyRequestBoardFilters;

function deepLink(
  kind: Exclude<RequestBoardDeepLink, "none">,
  id: string | null,
): RequestBoardRouteConfig {
  return { filters: emptyFilters, triage: false, deepLink: kind, deepLinkId: id };
}

export function boardRoute(filters: RequestBoardFilters = emptyFilters): RequestBoardRouteConfig {
  return { filters, triage: false, deepLink: "none", deepLinkId: null };
}

export function triageRoute(): RequestBoardRouteConfig {
  return { filters: emptyFilters, triage: true, deepLink: "none", deepLinkId: null };
}

export function requestDetailRoute(id: string): RequestBoardRouteConfig {
  return deepLink("requestDetail", id);
}

export function runDetailRoute(id: string): RequestBoardRouteConfig {
  return deepLink("runDetail", id);
}

export function projectReleaseRoute(project: string): RequestBoardRouteConfig {
  return deepLink("projectRelease", project);
}

export function runListRoute(): RequestBoardRouteConfig {
  return deepLink("runList", null);
}

export function projectStatsRoute(project: string): RequestBoardRouteConfig {
  return deepLink("projectStats", project);
}

export function opsRoute(): RequestBoardRouteConfig {
  return deepLink("ops", null);
}

export function newRequestRoute(): RequestBoardRouteConfig {
  return deepLink("newRequest", null);
}

function urlFor(input: string | URL): URL {
  return typeof input === "string" ? new URL(input, "http://localhost") : input;
}

function decodeSegment(segment: string): string {
  return decodeURIComponent(segment);
}

function segmentAt(segments: readonly string[], index: number): string {
  const segment = segments[index];
  if (segment === undefined) throw new Error(`Missing route segment at ${index}`);
  return segment;
}

function requiredDeepLinkId(route: RequestBoardRouteConfig): string {
  if (route.deepLinkId === null) throw new Error(`Missing id for ${route.deepLink}`);
  return route.deepLinkId;
}

/** Parses a URL or path into the board's base/deep-link route state. */
export function parseRoute(input: string | URL): RequestBoardRouteConfig {
  const url = urlFor(input);
  if (url.pathname === "/triage") return triageRoute();
  if (url.pathname === "/runs") return runListRoute();
  if (url.pathname === "/ops") return opsRoute();
  // This must precede the generic request-id branch: "new" is a screen.
  if (url.pathname === "/requests/new") return newRequestRoute();

  const segments = url.pathname.split("/").filter((segment) => segment !== "");
  if (segments.length === 2 && segments[0] === "requests") {
    return requestDetailRoute(decodeSegment(segmentAt(segments, 1)));
  }
  if (segments.length === 2 && segments[0] === "runs") {
    return runDetailRoute(decodeSegment(segmentAt(segments, 1)));
  }
  if (segments.length === 3 && segments[0] === "projects" && segments[2] === "release") {
    return projectReleaseRoute(decodeSegment(segmentAt(segments, 1)));
  }
  if (segments.length === 3 && segments[0] === "projects" && segments[2] === "stats") {
    return projectStatsRoute(decodeSegment(segmentAt(segments, 1)));
  }
  return boardRoute(filtersFromSearchParams(url.searchParams));
}

/** Builds a path with deep-link ids encoded exactly once. */
export function pathForRoute(route: RequestBoardRouteConfig): string {
  switch (route.deepLink) {
    case "requestDetail":
      return `/requests/${encodeURIComponent(requiredDeepLinkId(route))}`;
    case "runDetail":
      return `/runs/${encodeURIComponent(requiredDeepLinkId(route))}`;
    case "projectRelease":
      return `/projects/${encodeURIComponent(requiredDeepLinkId(route))}/release`;
    case "runList":
      return "/runs";
    case "projectStats":
      return `/projects/${encodeURIComponent(requiredDeepLinkId(route))}/stats`;
    case "ops":
      return "/ops";
    case "newRequest":
      return "/requests/new";
    case "none":
      if (route.triage) return "/triage";
      return boardPath(route.filters);
  }
}

/** The board, with its filters in the query string. */
export function boardPath(filters: RequestBoardFilters = emptyFilters): string {
  const params = new URLSearchParams();
  for (const [key, value] of Object.entries(toQueryParameters(filters))) {
    if (typeof value === "string") params.set(key, value);
    else for (const item of value) params.append(key, item);
  }
  const query = params.toString();
  return query === "" ? "/" : `/?${query}`;
}

export function requestPath(id: string): string {
  return pathForRoute(requestDetailRoute(id));
}

export function runPath(id: string): string {
  return pathForRoute(runDetailRoute(id));
}

export function projectReleasePath(project: string): string {
  return pathForRoute(projectReleaseRoute(project));
}

export function projectStatsPath(project: string): string {
  return pathForRoute(projectStatsRoute(project));
}

export function runsPath(): string {
  return "/runs";
}

export function opsPath(): string {
  return "/ops";
}

export function newRequestPath(): string {
  return "/requests/new";
}

// The screens below have no deep link in the route state above: they are
// reached from another screen. Their paths sit under /app/ or in a query
// string because a bare /projects or /runs/{id}/diff is an API read, which a
// browser reload would answer with JSON instead of the console.

export function triagePath(): string {
  return "/triage";
}

export function projectsPath(): string {
  return "/app/projects";
}

export function newRunPath(): string {
  return "/app/runs/new";
}

export type RunView = "diff" | "release";

/** A run's diff or release view: the run page with `?view=`. */
export function runViewPath(id: string, view: RunView): string {
  return `${runPath(id)}?view=${view}`;
}

/** The view a run page URL asks for; null for the run page itself. */
export function parseRunView(search: string): RunView | null {
  const view = new URLSearchParams(search).get("view");
  return view === "diff" || view === "release" ? view : null;
}

/**
 * The router's patterns, one per screen. Every `:param` is decoded by the
 * router and encoded by the builders above, exactly once each way.
 */
export const routePatterns = {
  board: "/",
  triage: "/triage",
  newRequest: "/requests/new",
  requestDetail: "/requests/:id",
  runs: "/runs",
  runDetail: "/runs/:id",
  newRun: "/app/runs/new",
  projects: "/app/projects",
  projectStats: "/projects/:project/stats",
  projectRelease: "/projects/:project/release",
  ops: "/ops",
} as const;
