import {
  type RequestBoardFilters,
  type RequestBoardSection,
  distinctProjects,
  emptyRequestBoardFilters,
  filtersFromSearchParams,
  matchesRequestBoardFilters,
  requestBoardFiltersEqual,
  sectionForRequest,
  sectionForState,
  toQueryParameters,
} from "@/domain/boardFilters";
import { type RequestSummary, decodeRequestSummary } from "@/domain/request";

function summaryOf(args: {
  id: string;
  state: string;
  project?: string;
  title?: string;
  workspace?: string;
}): RequestSummary {
  return decodeRequestSummary(
    {
      id: args.id,
      workspace: args.workspace ?? "/repos/checkouts",
      project: args.project ?? "checkouts",
      state: args.state,
      submitted_at: "2026-09-10T08:00:00Z",
      updated_at: "2026-09-10T09:00:00Z",
      title: args.title ?? "",
    },
    "test",
  );
}

function filters(partial: Partial<RequestBoardFilters> = {}): RequestBoardFilters {
  return { ...emptyRequestBoardFilters, ...partial };
}

// What Uri(queryParameters: ...) does: an array value repeats its key.
function toSearchParams(params: Record<string, string | readonly string[]>): URLSearchParams {
  const out = new URLSearchParams();
  for (const [key, value] of Object.entries(params)) {
    for (const v of typeof value === "string" ? [value] : value) out.append(key, v);
  }
  return out;
}

describe("sectionForState", () => {
  test("review states are needsYou", () => {
    expect(sectionForState("spec_review")).toBe("needsYou");
    expect(sectionForState("plan_review")).toBe("needsYou");
  });

  test("halted is needsYou, not finished (regression: the shared status vocabulary already maps halted to Status.needsHuman -- this board grouping used to disagree and put it under Finished, silently excluding a resumable request from both the section and the tab-title count)", () => {
    expect(sectionForState("halted")).toBe("needsYou");
  });

  test("drafting/building/pr_review states are working", () => {
    expect(sectionForState("submitted")).toBe("working");
    expect(sectionForState("building")).toBe("working");
    expect(sectionForState("pr_review")).toBe("working");
  });

  test("done, failed, and unmapped states are finished", () => {
    expect(sectionForState("done")).toBe("finished");
    expect(sectionForState("quarantined")).toBe("finished");
    expect(sectionForState("cancelled")).toBe("finished");
    expect(sectionForState("some_new_state")).toBe("finished");
  });
});

describe("sectionForRequest", () => {
  const prReview = (prStates: string[]): RequestSummary =>
    decodeRequestSummary(
      {
        id: "r",
        workspace: "/repos/checkouts",
        project: "checkouts",
        state: "pr_review",
        submitted_at: "2026-09-10T08:00:00Z",
        updated_at: "2026-09-10T09:00:00Z",
        tickets: prStates.map((prState, i) => ({
          index: i + 1,
          pr_url: `https://github.com/o/r/pull/${i + 1}`,
          pr_state: prState,
        })),
      },
      "test",
    );

  test("a pr_review request whose PRs all wait on their reviewer needs you", () => {
    expect(sectionForRequest(prReview(["ready"]))).toBe("needsYou");
    expect(sectionForRequest(prReview(["merged", "ready"]))).toBe("needsYou");
  });

  test("a stacked PR (waiting on its base PR merging) also needs you", () => {
    expect(sectionForRequest(prReview(["ready", "stacked"]))).toBe("needsYou");
  });

  test("a pr_review request with a draft or approved PR is still working", () => {
    expect(sectionForRequest(prReview(["draft"]))).toBe("working");
    expect(sectionForRequest(prReview(["ready", "approved"]))).toBe("working");
    expect(sectionForRequest(prReview([]))).toBe("working");
  });

  test("other states keep their state-based section", () => {
    expect(sectionForRequest(summaryOf({ id: "a", state: "building" }))).toBe("working");
    expect(sectionForRequest(summaryOf({ id: "b", state: "done" }))).toBe("finished");
  });
});

describe("section filter", () => {
  const requests = [
    summaryOf({ id: "waiting", state: "plan_review" }),
    summaryOf({ id: "busy", state: "building" }),
    summaryOf({ id: "shipped", state: "done" }),
    summaryOf({ id: "stopped", state: "quarantined" }),
  ];
  const matching = (section: RequestBoardSection): string[] =>
    requests.filter((r) => matchesRequestBoardFilters(r, filters({ section }))).map((r) => r.id);

  test("each section shows only its own requests", () => {
    expect(matching("needsYou")).toEqual(["waiting"]);
    expect(matching("working")).toEqual(["busy"]);
    expect(matching("finished")).toEqual(["shipped", "stopped"]);
  });
});

describe("RequestBoardFilters URL round-trip", () => {
  test("an empty filter set produces no query parameters", () => {
    expect(toQueryParameters(emptyRequestBoardFilters)).toEqual({});
    expect(
      requestBoardFiltersEqual(
        filtersFromSearchParams(new URLSearchParams()),
        emptyRequestBoardFilters,
      ),
    ).toBe(true);
  });

  test("every filter field round-trips through a URI", () => {
    const original = filters({
      projects: new Set(["checkouts", "billing"]),
      section: "needsYou",
      search: "idempotency",
    });

    const restored = filtersFromSearchParams(toSearchParams(toQueryParameters(original)));

    expect(requestBoardFiltersEqual(restored, original)).toBe(true);
  });

  test('a project name containing a comma round-trips as one project, not two (regression: a single comma-joined "project" value can\'t distinguish a literal comma in a name from the delimiter)', () => {
    const original = filters({ projects: new Set(["billing,legacy", "checkouts"]) });

    const restored = filtersFromSearchParams(toSearchParams(toQueryParameters(original)));

    expect(restored.projects).toEqual(new Set(["billing,legacy", "checkouts"]));
  });

  test("an unrecognized group query value is ignored, not crashed on", () => {
    const params = new URLSearchParams({ group: "bogus" });
    expect(filtersFromSearchParams(params).section).toBeNull();
  });
});

describe("matchesRequestBoardFilters", () => {
  const review = summaryOf({ id: "r1", state: "spec_review", project: "checkouts" });
  const working = summaryOf({ id: "r2", state: "building", project: "billing" });

  test("no filters matches everything", () => {
    expect(matchesRequestBoardFilters(review, emptyRequestBoardFilters)).toBe(true);
    expect(matchesRequestBoardFilters(working, emptyRequestBoardFilters)).toBe(true);
  });

  test("a project filter excludes other projects", () => {
    const f = filters({ projects: new Set(["checkouts"]) });
    expect(matchesRequestBoardFilters(review, f)).toBe(true);
    expect(matchesRequestBoardFilters(working, f)).toBe(false);
  });

  test("a section filter excludes other sections", () => {
    const f = filters({ section: "working" });
    expect(matchesRequestBoardFilters(review, f)).toBe(false);
    expect(matchesRequestBoardFilters(working, f)).toBe(true);
  });

  test("free-text search matches id, title, or workspace case-insensitively", () => {
    const titled = summaryOf({ id: "r3", state: "done", title: "Add idempotency keys" });
    expect(matchesRequestBoardFilters(titled, filters({ search: "IDEMPOTENCY" }))).toBe(true);
    expect(matchesRequestBoardFilters(titled, filters({ search: "nonexistent" }))).toBe(false);
  });
});

test("distinctProjects sorts and de-duplicates", () => {
  const requests = [
    summaryOf({ id: "a", state: "done", project: "billing" }),
    summaryOf({ id: "b", state: "done", project: "checkouts" }),
    summaryOf({ id: "c", state: "done", project: "billing" }),
  ];
  expect(distinctProjects(requests)).toEqual(["billing", "checkouts"]);
});
