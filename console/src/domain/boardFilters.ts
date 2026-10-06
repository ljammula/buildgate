import type { RequestSummary } from "@/domain/request";

/**
 * The five groups a request's state sorts and colors by -- "waiting-on-you =
 * review states, halted, quarantined; working =
 * drafting/planning/building/pr_review; done; failed = cancelled". The single implementation: the board, triage
 * and the request ordering all read it. The board's own filter only needs the
 * three-way RequestBoardSection below.
 *
 * `review` means "waiting on you", not literally "in a review state" -- it
 * includes `halted`, which is not one of the two request review states
 * (spec_review/plan_review) but is genuinely operator-actionable right now
 * via the retry flow, exactly like the shared status vocabulary already says
 * (status.ts maps `halted` to needsHuman). Found in review: the grouping
 * disagreed with that vocabulary, so a halted request was sorted into
 * "Finished" on the board and silently excluded from the "needs you"
 * tab-title count. `quarantined` is here for the same reason: the request
 * page offers Retry, Send back and Cancel for it, so it waits on the operator
 * and is not finished. Only `cancelled` is a dead end.
 */
export type RequestStageGroup = "review" | "working" | "done" | "failed" | "other";

const REVIEW_STATES: ReadonlySet<string> = new Set([
  "spec_review",
  "oracle_review",
  "plan_review",
  "halted",
  "quarantined",
  "resume_review",
]);
// submitted is working, not "other": other rendered under Finished, so a
// just-submitted request queued behind another looked finished.
const WORKING_STATES: ReadonlySet<string> = new Set([
  "submitted",
  "spec_drafting",
  "oracle_drafting",
  "planning",
  "building",
  "pr_review",
]);
const DONE_STATES: ReadonlySet<string> = new Set(["done"]);
const FAILED_STATES: ReadonlySet<string> = new Set(["cancelled"]);

export function requestStageGroup(state: string): RequestStageGroup {
  if (REVIEW_STATES.has(state)) return "review";
  if (WORKING_STATES.has(state)) return "working";
  if (DONE_STATES.has(state)) return "done";
  if (FAILED_STATES.has(state)) return "failed";
  return "other";
}

/**
 * requestStageGroup for a whole request: a `pr_review` request whose PRs all
 * wait on a human (every opened PR `ready`, `stacked` on another ticket's
 * unmerged PR, or already `merged`, at least one not merged) needs you, not
 * the factory. A `draft` or `approved` PR means the factory still has work
 * to do on it.
 */
export function requestStageGroupOf(request: RequestSummary): RequestStageGroup {
  if (request.state === "pr_review") {
    const prStates = request.tickets
      .filter((t) => t.prUrl !== "" && t.prState !== "")
      .map((t) => t.prState);
    const waitingOnHuman = (s: string): boolean => s === "ready" || s === "stacked";
    if (
      prStates.some(waitingOnHuman) &&
      prStates.every((s) => waitingOnHuman(s) || s === "merged")
    ) {
      return "review";
    }
  }
  return requestStageGroup(request.state);
}

/**
 * The three sections the request board groups into: "review" is Needs you,
 * "working" is Working, and done, failed and other all collapse into
 * Finished, matching a three-header example ("Needs you (3)", "Working (7)",
 * "Finished (41)") rather than a fifth "other" bucket nobody asked to see.
 */
export type RequestBoardSection = "needsYou" | "working" | "finished";

function sectionForGroup(group: RequestStageGroup): RequestBoardSection {
  switch (group) {
    case "review":
      return "needsYou";
    case "working":
      return "working";
    case "done":
    case "failed":
    case "other":
      return "finished";
  }
}

/**
 * The display grouping for the board's sticky headers, layered on top of the
 * per-state stage group rather than replacing it.
 */
export function sectionForState(state: string): RequestBoardSection {
  return sectionForGroup(requestStageGroup(state));
}

/**
 * sectionForState for a whole request: a `pr_review` request waiting only on
 * its reviewer shows under Needs you. The board and its section filter both
 * use this, so a filter never disagrees with the header it matches.
 */
export function sectionForRequest(request: RequestSummary): RequestBoardSection {
  return sectionForGroup(requestStageGroupOf(request));
}

export const sectionLabels: Readonly<Record<RequestBoardSection, string>> = {
  needsYou: "Needs you",
  working: "Working",
  finished: "Finished",
};

// The query-string spelling for each section, used by toQueryParameters and
// filtersFromSearchParams -- kept distinct from the section's own identifier
// so the URL stays stable even if the identifier is ever renamed.
const SECTION_QUERY_VALUES: Readonly<Record<RequestBoardSection, string>> = {
  needsYou: "needs-you",
  working: "working",
  finished: "finished",
};

function sectionFromQueryValue(value: string): RequestBoardSection | null {
  for (const section of Object.keys(SECTION_QUERY_VALUES) as RequestBoardSection[]) {
    if (SECTION_QUERY_VALUES[section] === value) return section;
  }
  return null;
}

/**
 * The request board's filter state: a project multi-select, an optional
 * single section filter, and free-text search. Immutable and round-trips
 * through a URL query string via filtersFromSearchParams/toQueryParameters so
 * the board's filters survive a reload and are shareable -- no server-side
 * filtering, no persistence beyond the URL itself.
 */
export interface RequestBoardFilters {
  /**
   * Empty means "every project" -- not "no project", which would be an
   * unreachable filter state for an operator to select their way into.
   */
  readonly projects: ReadonlySet<string>;
  /** Null means "every section". */
  readonly section: RequestBoardSection | null;
  readonly search: string;
}

export const emptyRequestBoardFilters: RequestBoardFilters = {
  projects: new Set(),
  section: null,
  search: "",
};

/** Value equality: the project set compares by members, not identity. */
export function requestBoardFiltersEqual(a: RequestBoardFilters, b: RequestBoardFilters): boolean {
  return (
    a.projects.size === b.projects.size &&
    [...a.projects].every((p) => b.projects.has(p)) &&
    a.section === b.section &&
    a.search === b.search
  );
}

/** A copy with the given fields replaced; `section: null` clears the section. */
export function copyRequestBoardFilters(
  filters: RequestBoardFilters,
  changes: Partial<RequestBoardFilters>,
): RequestBoardFilters {
  return { ...filters, ...changes };
}

/**
 * Reads the filters from a URL query string. Repeated ?project=a&project=b
 * params, not one comma-joined value -- see toQueryParameters' own doc
 * comment for why.
 */
export function filtersFromSearchParams(params: URLSearchParams): RequestBoardFilters {
  const groupParam = params.get("group");
  return {
    projects: new Set(params.getAll("project").filter((p) => p !== "")),
    section: groupParam === null ? null : sectionFromQueryValue(groupParam),
    search: params.get("q") ?? "",
  };
}

/**
 * The filters as query parameters; `project` is an array meaning "repeat
 * this key once per element", so a project name that itself contains a comma
 * round-trips correctly. Found in review: the earlier single comma-joined
 * value made a project named e.g. "billing,legacy" indistinguishable from
 * two projects "billing" and "legacy" once reloaded from that URL -- there is
 * no way to escape a comma within one joined value and also use comma as the
 * delimiter between values. Repeated keys have no such ambiguity: each
 * occurrence is one full, separately-encoded value.
 */
export function toQueryParameters(
  filters: RequestBoardFilters,
): Record<string, string | readonly string[]> {
  const out: Record<string, string | readonly string[]> = {};
  if (filters.projects.size > 0) out.project = [...filters.projects].sort();
  if (filters.section !== null) out.group = SECTION_QUERY_VALUES[filters.section];
  if (filters.search !== "") out.q = filters.search;
  return out;
}

/**
 * True when `request` matches every active filter in `filters` -- pure
 * client-side filtering over already-fetched data; does not add a
 * server-side filter.
 */
export function matchesRequestBoardFilters(
  request: RequestSummary,
  filters: RequestBoardFilters,
): boolean {
  if (filters.projects.size > 0 && !filters.projects.has(request.project)) return false;
  if (filters.section !== null && sectionForRequest(request) !== filters.section) return false;
  if (filters.search !== "") {
    const needle = filters.search.toLowerCase();
    const haystack = `${request.id} ${request.title} ${request.workspace}`.toLowerCase();
    if (!haystack.includes(needle)) return false;
  }
  return true;
}

/**
 * Every distinct project value across `requests`, sorted -- the project
 * multi-select's own option list.
 */
export function distinctProjects(requests: readonly RequestSummary[]): string[] {
  return [...new Set(requests.map((r) => r.project))].sort();
}
