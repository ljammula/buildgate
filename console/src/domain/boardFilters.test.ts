import {
  type RequestBoardFilters,
  type RequestBoardSection,
  distinctProjects,
  emptyRequestBoardFilters,
  filtersFromSearchParams,
  matchesRequestBoardFilters,
  requestBoardFiltersEqual,
  requestStageGroup,
  requestStageGroupOf,
  sectionForRequest,
  sectionForState,
  toQueryParameters,
} from "@/domain/boardFilters";
import { type RequestSummary, decodeRequestSummary } from "@/domain/request";
import { requestJson, ticketJson } from "@/test/requestFixtures";

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

  test("quarantined is needsYou: the request page offers retry, send back and cancel for it", () => {
    expect(sectionForState("quarantined")).toBe("needsYou");
    expect(requestStageGroup("quarantined")).toBe("review");
    expect(requestStageGroup("cancelled")).toBe("failed");
  });

  test("drafting/building/pr_review states are working", () => {
    expect(sectionForState("submitted")).toBe("working");
    expect(sectionForState("building")).toBe("working");
    expect(sectionForState("pr_review")).toBe("working");
  });

  test("done, failed, and unmapped states are finished", () => {
    expect(sectionForState("done")).toBe("finished");
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
    expect(matching("needsYou")).toEqual(["waiting", "stopped"]);
    expect(matching("working")).toEqual(["busy"]);
    expect(matching("finished")).toEqual(["shipped"]);
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

  test("the window is 7 days when the URL says nothing, and that default is not written", () => {
    expect(filtersFromSearchParams(new URLSearchParams()).days).toBe(7);
    expect(toQueryParameters(filters({ days: 7 }))).toEqual({});
    // A link from before the window existed has no `days`: it opens on the default.
    expect(filtersFromSearchParams(new URLSearchParams("group=working&q=x")).days).toBe(7);
  });

  test("30 days and All round-trip as days=30 and days=all", () => {
    expect(toQueryParameters(filters({ days: 30 }))).toEqual({ days: "30" });
    expect(toQueryParameters(filters({ days: "all" }))).toEqual({ days: "all" });
    expect(filtersFromSearchParams(new URLSearchParams("days=30")).days).toBe(30);
    expect(filtersFromSearchParams(new URLSearchParams("days=all")).days).toBe("all");
    const original = filters({ days: "all", section: "finished", search: "x" });
    const restored = filtersFromSearchParams(toSearchParams(toQueryParameters(original)));
    expect(requestBoardFiltersEqual(restored, original)).toBe(true);
    expect(requestBoardFiltersEqual(filters({ days: 30 }), filters({ days: 7 }))).toBe(false);
  });

  test("a days value that is not one of the three reads as the default", () => {
    for (const value of ["14", "0", "", "ALL", "7d"]) {
      expect(filtersFromSearchParams(new URLSearchParams({ days: value })).days).toBe(7);
    }
  });

  test("the window is not part of matching: it never hides a request there", () => {
    const old = decodeRequestSummary(
      requestJson({ id: "old", state: "done", updatedAt: "2020-01-01T00:00:00Z" }),
      "test",
    );
    expect(matchesRequestBoardFilters(old, filters({ days: 7 }))).toBe(true);
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

describe("requestStageGroupOf", () => {
  const withPrs = (prStates: string[]) =>
    decodeRequestSummary(
      requestJson({
        id: "r",
        state: "pr_review",
        tickets: prStates.map((prState, i) => ticketJson({ index: i + 1, prState })),
      }),
      "test",
    );

  test("a pr_review request whose PRs all wait on a human needs you", () => {
    expect(requestStageGroupOf(withPrs(["ready", "merged"]))).toBe("review");
    expect(requestStageGroupOf(withPrs(["stacked"]))).toBe("review");
  });

  test("a draft or approved PR means the factory still has work", () => {
    expect(requestStageGroupOf(withPrs(["ready", "approved"]))).toBe("working");
    expect(requestStageGroupOf(withPrs(["merged"]))).toBe("working");
  });
});
