import {
  type BoardColumn,
  boardColumnLabels,
  boardColumns,
  buildBoard,
  activeGroup,
  columnForRequest,
  columnGroups,
  groupCards,
  groupChips,
  groupPreviewSize,
  isCompactColumn,
  isNarrowColumn,
  isCancelledRequest,
  showsNeedsYou,
  visibleColumns,
  waitsOnYou,
} from "@/domain/boardColumns";
import { requestStageGroupOf } from "@/domain/boardFilters";
import { decodeRequestList, decodeRequestSummary } from "@/domain/request";
import { asObject } from "@/domain/decode";
import { apiFixtures, readFixtureJson } from "@/test/fixtures";
import { requestJson, requestSummary, ticketJson } from "@/test/requestFixtures";

/** A decoded request with wire fields the builder has no option for. */
const wire = (o: Parameters<typeof requestJson>[0], extra: Record<string, unknown>) =>
  decodeRequestSummary({ ...requestJson(o), ...extra }, "test");

const at = (state: string) => columnForRequest(requestSummary({ id: `req-${state}`, state }));

/** A request in `state` whose history ends with the move from `from` into it. */
const movedFrom = (id: string, from: string, state: string, extra: Record<string, unknown> = {}) =>
  wire(
    { id, state },
    {
      history: [
        { from: "submitted", to: "spec_drafting", at: "2026-09-10T09:00:00Z", by: "factory" },
        { from, to: state, at: "2026-09-10T09:05:00Z", by: "factory" },
      ],
      ...extra,
    },
  );

const none: Readonly<Record<BoardColumn, number>> = {
  spec: 0,
  plan: 0,
  building: 0,
  prReview: 0,
  finished: 0,
};

describe("columnForRequest", () => {
  test("each pipeline state is in its stage's column", () => {
    const expected: Readonly<Record<string, BoardColumn>> = {
      submitted: "spec",
      spec_drafting: "spec",
      spec_review: "spec",
      oracle_drafting: "spec",
      oracle_review: "spec",
      planning: "plan",
      plan_review: "plan",
      building: "building",
      pr_review: "prReview",
      done: "finished",
      closed: "finished",
      cancelled: "finished",
    };
    for (const [state, column] of Object.entries(expected)) {
      expect(at(state), state).toBe(column);
    }
  });

  test("a request only moves right along one ticket's pipeline", () => {
    const path = [
      "submitted",
      "spec_drafting",
      "spec_review",
      "oracle_drafting",
      "oracle_review",
      "planning",
      "plan_review",
      "building",
      "pr_review",
      "done",
    ];
    const indexes = path.map((state) => boardColumns.indexOf(at(state)));
    for (let i = 1; i < indexes.length; i++) {
      expect(indexes[i], path[i]).toBeGreaterThanOrEqual(indexes[i - 1]!);
    }
    // A pull request closed without merging ends where a merged one does.
    expect(at("closed")).toBe(at("done"));
  });

  test("the request's own moves back are the only moves left", () => {
    // advance_on: pr_approved: the next ticket builds after this one's PR is approved.
    expect(boardColumns.indexOf(at("building"))).toBeLessThan(
      boardColumns.indexOf(at("pr_review")),
    );
    // A send-back from a stuck build redrafts the plan.
    expect(columnForRequest(movedFrom("sent-back", "quarantined", "planning"))).toBe("plan");
  });

  test("a stuck request stays in the column of the step it stopped at", () => {
    expect(columnForRequest(movedFrom("h", "building", "halted"))).toBe("building");
    expect(columnForRequest(movedFrom("q", "pr_review", "quarantined"))).toBe("prReview");
    expect(columnForRequest(movedFrom("p", "planning", "halted"))).toBe("plan");
    // resume_review names the lost step itself.
    expect(
      columnForRequest(
        movedFrom("r", "oracle_drafting", "resume_review", {
          resume: { from_state: "planning" },
        }),
      ),
    ).toBe("plan");
    // A move between two stuck states is skipped back to the step.
    expect(
      columnForRequest(
        wire(
          { id: "hq", state: "quarantined" },
          {
            history: [
              { from: "building", to: "halted", at: "2026-09-10T09:00:00Z", by: "factory" },
              { from: "halted", to: "quarantined", at: "2026-09-10T09:05:00Z", by: "op" },
            ],
          },
        ),
      ),
    ).toBe("building");
    // A record with no history to say where: the first column.
    expect(at("halted")).toBe("spec");
  });

  test("a cancelled request is Finished, wherever it stopped", () => {
    expect(columnForRequest(movedFrom("c", "building", "cancelled"))).toBe("finished");
  });

  test("a pr_review request is in PR review whoever has the next move", () => {
    const withPrs = (...prStates: string[]) =>
      requestSummary({
        id: "req-pr",
        state: "pr_review",
        tickets: prStates.map((prState, i) => ticketJson({ index: i + 1, prState })),
      });
    for (const request of [withPrs("draft"), withPrs("ready"), withPrs("merged", "stacked")]) {
      expect(columnForRequest(request)).toBe("prReview");
    }
    // Who acts is the one decision the tab title's count reads.
    for (const request of [withPrs("draft"), withPrs("ready"), withPrs("merged", "ready")]) {
      expect(waitsOnYou(request)).toBe(requestStageGroupOf(request) === "review");
    }
    expect(waitsOnYou(withPrs("draft"))).toBe(false);
    expect(waitsOnYou(withPrs("ready"))).toBe(true);
  });

  test("every request state in the contract fixtures lands in exactly one column", () => {
    const requests = [
      ...decodeRequestList(readFixtureJson("api/requests.json"), "GET /requests"),
      ...apiFixtures()
        .filter((fixture) => fixture.pattern === "GET /requests/{id}")
        .map((fixture) =>
          decodeRequestSummary(
            asObject(readFixtureJson(`api/${fixture.file}`), fixture.pattern),
            fixture.pattern,
          ),
        ),
    ];
    const states = new Set(requests.map((r) => r.state));
    // Whatever the fixtures hold today: the server owns the set.
    expect(states.size).toBeGreaterThan(5);
    for (const request of requests) {
      const board = buildBoard([request], { showCancelled: true });
      const cells = boardColumns.filter((column) =>
        board.lanes.some((lane) => lane.cells[column].requests.includes(request)),
      );
      expect(cells, request.id).toEqual([columnForRequest(request)]);
    }
  });

  test("a state this console has never seen goes to Spec, without throwing", () => {
    expect(at("security_review")).toBe("spec");
    expect(at("")).toBe("spec");
    const board = buildBoard([requestSummary({ id: "req-new", state: "security_review" })], {
      showCancelled: false,
    });
    expect(board.counts).toEqual({ ...none, spec: 1 });
  });

  test("the columns are named, in order", () => {
    expect(boardColumns.map((column) => boardColumnLabels[column])).toEqual([
      "Spec",
      "Plan",
      "Building",
      "PR review",
      "Finished",
    ]);
  });
});

describe("buildBoard", () => {
  const requests = [
    requestSummary({ id: "a-draft", state: "planning", project: "alpha" }),
    requestSummary({ id: "a-review", state: "spec_review", project: "alpha" }),
    requestSummary({ id: "b-build", state: "building", project: "beta" }),
    requestSummary({ id: "b-done", state: "done", project: "beta" }),
    requestSummary({ id: "b-closed", state: "closed", project: "beta" }),
    requestSummary({ id: "b-cancelled", state: "cancelled", project: "beta" }),
  ];

  test("one lane per project, by name, each request in its column", () => {
    const board = buildBoard(requests, { showCancelled: false });
    expect(board.lanes.map((lane) => [lane.project, lane.count, lane.waiting])).toEqual([
      ["alpha", 2, 1],
      ["beta", 3, 0],
    ]);
    const ids = (lane: number, column: BoardColumn) =>
      board.lanes[lane]!.cells[column].requests.map((r) => r.id);
    expect(ids(0, "plan")).toEqual(["a-draft"]);
    expect(ids(0, "spec")).toEqual(["a-review"]);
    expect(ids(1, "building")).toEqual(["b-build"]);
    expect(ids(1, "finished").sort()).toEqual(["b-closed", "b-done"]);
    expect(board.counts).toEqual({ spec: 1, plan: 1, building: 1, prReview: 0, finished: 2 });
  });

  test("cancelled requests are counted, and drawn in Finished only when asked for", () => {
    expect(isCancelledRequest(requests[5]!)).toBe(true);
    expect(isCancelledRequest(requests[4]!)).toBe(false);
    const hidden = buildBoard(requests, { showCancelled: false });
    expect(hidden.cancelled).toBe(1);
    expect(hidden.counts.finished).toBe(2);
    const shown = buildBoard(requests, { showCancelled: true });
    expect(shown.cancelled).toBe(1);
    expect(shown.counts.finished).toBe(3);
  });

  test("a project whose only request is a hidden cancelled one has no lane", () => {
    const board = buildBoard([requestSummary({ id: "x", state: "cancelled", project: "gone" })], {
      showCancelled: false,
    });
    expect(board.lanes).toEqual([]);
    expect(board.cancelled).toBe(1);
  });

  test("Finished shows a lane's newest 20 and says how many it left out", () => {
    const done = Array.from({ length: 23 }, (_, i) =>
      requestSummary({
        id: `done-${String(i).padStart(2, "0")}`,
        state: i % 2 === 0 ? "done" : "closed",
        updatedAt: `2026-09-10T09:${String(i).padStart(2, "0")}:00Z`,
      }),
    );
    const board = buildBoard(done, { showCancelled: false });
    const cell = board.lanes[0]!.cells.finished;
    expect(cell.requests).toHaveLength(20);
    expect(cell.more).toBe(3);
    // A group chip counts the whole cell, those behind the cap included.
    expect(cell.all).toHaveLength(23);
    expect(groupChips("finished", cell.all)).toEqual([
      { label: "Merged", short: "Merged", count: 12 },
      { label: "Closed", short: "Closed", count: 11 },
    ]);
    expect(cell.requests[0]!.id).toBe("done-22");
    expect(cell.requests.at(-1)!.id).toBe("done-03");
    // The header counts every finished request, not only the cards drawn.
    expect(board.counts.finished).toBe(23);
    expect(board.lanes[0]!.count).toBe(23);
    expect(
      buildBoard(done, { showCancelled: false, doneCap: 5 }).lanes[0]!.cells.finished.more,
    ).toBe(18);
  });

  test("what waits on the operator is oldest wait first; jobs follow the running ones in queue order", () => {
    const board = buildBoard(
      [
        requestSummary({ id: "late", state: "spec_review", waitingSince: "2026-09-10T10:00:00Z" }),
        requestSummary({
          id: "early",
          state: "oracle_review",
          waitingSince: "2026-09-10T08:00:00Z",
        }),
        wire({ id: "third", state: "oracle_drafting" }, { queue_position: 2 }),
        wire({ id: "second", state: "submitted" }, { queue_position: 1 }),
        wire({ id: "running", state: "spec_drafting" }, {}),
      ],
      { showCancelled: false },
    );
    const groups = columnGroups("spec", board.lanes[0]!.cells.spec.requests);
    expect(groups.map((g) => [g.label, g.requests.map((r) => r.id)])).toEqual([
      ["Spec review", ["late"]],
      ["Oracle review", ["early"]],
      ["Drafting", ["running", "second", "third"]],
    ]);
  });
});

describe("visibleColumns", () => {
  test("no section filter draws all five", () => {
    expect(visibleColumns(null, none)).toEqual(boardColumns);
  });

  test("a section filter draws that section's columns", () => {
    const stages = ["spec", "plan", "building", "prReview"];
    expect(visibleColumns("needsYou", none)).toEqual(stages);
    expect(visibleColumns("working", none)).toEqual(stages);
    expect(visibleColumns("finished", none)).toEqual(["finished"]);
  });

  test("a column that holds a card is drawn even outside the section", () => {
    // An unknown state passes the Finished filter and sits in Spec.
    expect(visibleColumns("finished", { ...none, spec: 1 })).toEqual(["spec", "finished"]);
  });
});

test("showsNeedsYou is true exactly when a request in view waits on the operator", () => {
  expect(showsNeedsYou([requestSummary({ id: "a", state: "building" })])).toBe(false);
  expect(showsNeedsYou([requestSummary({ id: "a", state: "halted" })])).toBe(true);
  expect(showsNeedsYou([requestSummary({ id: "a", state: "closed" })])).toBe(false);
});

describe("columnGroups", () => {
  const shape = (column: BoardColumn, requests: Parameters<typeof buildBoard>[0]) =>
    columnGroups(
      column,
      buildBoard(requests, { showCancelled: true }).lanes[0]?.cells[column].requests ?? [],
    ).map((group) => [group.label, group.requests.map((r) => r.id)]);
  const pr = (id: string, prState: string) =>
    requestSummary({ id, state: "pr_review", tickets: [ticketJson({ index: 1, prState })] });

  test("Spec is Stuck, the reviews, then drafting", () => {
    expect(
      shape("spec", [
        movedFrom("h", "spec_drafting", "halted"),
        requestSummary({ id: "d", state: "spec_drafting" }),
        requestSummary({ id: "o", state: "oracle_review" }),
        requestSummary({ id: "s", state: "spec_review" }),
        requestSummary({ id: "x", state: "security_review" }),
      ]),
    ).toEqual([
      ["Stuck", ["h"]],
      ["Spec review", ["s"]],
      ["Oracle review", ["o"]],
      ["Drafting", ["d"]],
      ["Other", ["x"]],
    ]);
  });

  test("Plan is Stuck, Plan review, Planning", () => {
    expect(
      shape("plan", [
        requestSummary({ id: "p", state: "planning" }),
        requestSummary({ id: "r", state: "plan_review" }),
        movedFrom("q", "planning", "quarantined"),
      ]),
    ).toEqual([
      ["Stuck", ["q"]],
      ["Plan review", ["r"]],
      ["Planning", ["p"]],
    ]);
  });

  test("PR review keeps PR ready apart from the pull requests the factory still works on", () => {
    expect(
      shape("prReview", [
        pr("draft", "draft"),
        pr("ready", "ready"),
        movedFrom("lost", "pr_review", "resume_review", { resume: { from_state: "pr_review" } }),
      ]),
    ).toEqual([
      ["Stuck", ["lost"]],
      ["PR ready", ["ready"]],
      ["Factory working", ["draft"]],
    ]);
  });

  test("Finished is grouped by how the request ended", () => {
    expect(
      shape("finished", [
        requestSummary({ id: "c", state: "cancelled" }),
        requestSummary({ id: "x", state: "closed" }),
        requestSummary({ id: "m", state: "done" }),
      ]),
    ).toEqual([
      ["Merged", ["m"]],
      ["Closed", ["x"]],
      ["Cancelled", ["c"]],
    ]);
  });

  test("Building puts a stuck build above the running ones", () => {
    expect(
      shape("building", [
        requestSummary({ id: "b", state: "building" }),
        movedFrom("h", "building", "halted"),
      ]),
    ).toEqual([
      ["Stuck", ["h"]],
      ["Building", ["b"]],
    ]);
  });

  test("only a group that holds exactly one state is flagged oneState", () => {
    const flags = (column: BoardColumn, requests: Parameters<typeof buildBoard>[0]) =>
      columnGroups(column, requests).map((g) => [g.label, g.oneState]);
    expect(
      flags("spec", [
        requestSummary({ id: "s", state: "spec_review" }),
        requestSummary({ id: "d", state: "submitted" }),
      ]),
    ).toEqual([
      ["Spec review", true],
      ["Drafting", false],
    ]);
    expect(flags("prReview", [pr("r", "ready")])).toEqual([["PR ready", false]]);
    expect(flags("finished", [requestSummary({ id: "x", state: "closed" })])).toEqual([
      ["Closed", true],
    ]);
  });

  test("an empty column has no group", () => {
    for (const column of boardColumns) expect(columnGroups(column, [])).toEqual([]);
  });

  test("grouping moves no request and changes no count", () => {
    const requests = [
      requestSummary({ id: "a", state: "spec_review" }),
      requestSummary({ id: "b", state: "halted" }),
      requestSummary({ id: "c", state: "planning" }),
      requestSummary({ id: "d", state: "security_review" }),
      pr("e", "draft"),
      requestSummary({ id: "f", state: "closed" }),
    ];
    const board = buildBoard(requests, { showCancelled: false });
    for (const column of boardColumns) {
      const grouped = columnGroups(column, board.lanes[0]!.cells[column].requests);
      expect(grouped.reduce((sum, group) => sum + group.requests.length, 0)).toBe(
        board.counts[column],
      );
    }
  });
});

describe("density and chips", () => {
  const waits = (n: number, state: string) =>
    Array.from({ length: n }, (_, i) =>
      requestSummary({
        id: `${state}-${i}`,
        state,
        waitingSince: `2026-09-10T0${i}:00:00Z`,
      }),
    );

  test("Spec and Plan are the compact columns", () => {
    expect(boardColumns.filter(isCompactColumn)).toEqual(["spec", "plan"]);
  });

  test("one chip per non-empty group, in the groups' order, with its real size", () => {
    const requests = [...waits(3, "halted"), ...waits(5, "spec_review"), ...waits(2, "submitted")];
    expect(groupChips("spec", requests)).toEqual([
      { label: "Stuck", short: "Stuck", count: 3 },
      { label: "Spec review", short: "Spec", count: 5 },
      { label: "Drafting", short: "Drafting", count: 2 },
    ]);
    expect(groupChips("spec", [])).toEqual([]);
  });

  test("a pressed chip is in force only while its group still holds a card", () => {
    const chips = groupChips("spec", waits(2, "spec_review"));
    expect(activeGroup("Spec review", chips)).toBe("Spec review");
    expect(activeGroup(null, chips)).toBeNull();
    // Its last request moved on: every group is shown again.
    expect(activeGroup("Oracle review", chips)).toBeNull();
  });

  test("a group shows its first three and counts the rest behind +N more", () => {
    const [group] = columnGroups("spec", waits(5, "spec_review"));
    expect(groupPreviewSize).toBe(3);
    const folded = groupCards(group!, { expanded: false, alone: false });
    expect(folded.shown.map((r) => r.id)).toEqual([
      "spec_review-0",
      "spec_review-1",
      "spec_review-2",
    ]);
    expect(folded.more).toBe(2);
    expect(groupCards(group!, { expanded: true, alone: false })).toEqual({
      shown: group!.requests,
      more: 0,
    });
    // Chosen by a chip, the whole group is shown.
    expect(groupCards(group!, { expanded: false, alone: true }).shown).toHaveLength(5);
  });

  test("no card is ever dropped", () => {
    for (const size of [1, 3, 4, 20]) {
      const [group] = columnGroups("spec", waits(size, "halted"));
      const view = groupCards(group!, { expanded: false, alone: false });
      expect(view.shown.length + view.more).toBe(size);
    }
  });

  test("a column is narrow exactly when it holds no card in any lane", () => {
    const board = buildBoard(
      [
        requestSummary({ id: "a", state: "spec_review", project: "alpha" }),
        requestSummary({ id: "b", state: "building", project: "beta" }),
      ],
      { showCancelled: false },
    );
    expect(boardColumns.filter((column) => isNarrowColumn(column, board.counts))).toEqual([
      "plan",
      "prReview",
      "finished",
    ]);
    // Building is empty in alpha's lane and not narrow: beta holds a card.
    expect(board.lanes[0]!.cells.building.requests).toEqual([]);
    expect(isNarrowColumn("building", board.counts)).toBe(false);
  });
});
