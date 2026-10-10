import {
  type BoardColumn,
  boardColumnLabels,
  boardColumns,
  buildBoard,
  activeGroup,
  cardsAcross,
  columnForRequest,
  columnGroups,
  groupCards,
  groupChips,
  groupPreviewSize,
  isGroupedColumn,
  isNarrowColumn,
  isCancelledRequest,
  showsNeedsYou,
  visibleColumns,
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

describe("columnForRequest", () => {
  test("each state is in the column of who has to act", () => {
    const expected: Readonly<Record<string, BoardColumn>> = {
      submitted: "drafting",
      spec_drafting: "drafting",
      oracle_drafting: "drafting",
      planning: "drafting",
      spec_review: "needsYou",
      oracle_review: "needsYou",
      plan_review: "needsYou",
      resume_review: "needsYou",
      halted: "needsYou",
      quarantined: "needsYou",
      building: "building",
      pr_review: "prReview",
      done: "done",
      cancelled: "done",
    };
    for (const [state, column] of Object.entries(expected)) {
      expect(at(state), state).toBe(column);
    }
  });

  test("a pr_review request is in PR review while the factory has work on a PR, and in Needs you once every PR waits on a human", () => {
    const withPrs = (...prStates: string[]) =>
      requestSummary({
        id: "req-pr",
        state: "pr_review",
        tickets: prStates.map((prState, i) => ticketJson({ index: i + 1, prState })),
      });
    expect(columnForRequest(withPrs("draft"))).toBe("prReview");
    expect(columnForRequest(withPrs("ready", "approved"))).toBe("prReview");
    expect(columnForRequest(withPrs("ready"))).toBe("needsYou");
    expect(columnForRequest(withPrs("merged", "stacked"))).toBe("needsYou");
    // The column and the tab title's count are one decision.
    for (const request of [withPrs("draft"), withPrs("ready"), withPrs("merged", "ready")]) {
      expect(columnForRequest(request) === "needsYou").toBe(
        requestStageGroupOf(request) === "review",
      );
    }
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

  test("a state this console has never seen goes to Drafting, without throwing", () => {
    expect(at("security_review")).toBe("drafting");
    expect(at("")).toBe("drafting");
    const board = buildBoard([requestSummary({ id: "req-new", state: "security_review" })], {
      showCancelled: false,
    });
    expect(board.counts).toEqual({ drafting: 1, needsYou: 0, building: 0, prReview: 0, done: 0 });
  });

  test("the columns are named, in order", () => {
    expect(boardColumns.map((column) => boardColumnLabels[column])).toEqual([
      "Drafting",
      "Needs you",
      "Building",
      "PR review",
      "Done",
    ]);
  });
});

describe("buildBoard", () => {
  const requests = [
    requestSummary({ id: "a-draft", state: "planning", project: "alpha" }),
    requestSummary({ id: "a-review", state: "spec_review", project: "alpha" }),
    requestSummary({ id: "b-build", state: "building", project: "beta" }),
    requestSummary({ id: "b-done", state: "done", project: "beta" }),
    requestSummary({ id: "b-cancelled", state: "cancelled", project: "beta" }),
  ];

  test("one lane per project, by name, each request in its column", () => {
    const board = buildBoard(requests, { showCancelled: false });
    expect(board.lanes.map((lane) => [lane.project, lane.count])).toEqual([
      ["alpha", 2],
      ["beta", 2],
    ]);
    const ids = (lane: number, column: BoardColumn) =>
      board.lanes[lane]!.cells[column].requests.map((r) => r.id);
    expect(ids(0, "drafting")).toEqual(["a-draft"]);
    expect(ids(0, "needsYou")).toEqual(["a-review"]);
    expect(ids(1, "building")).toEqual(["b-build"]);
    expect(ids(1, "done")).toEqual(["b-done"]);
    expect(board.counts).toEqual({ drafting: 1, needsYou: 1, building: 1, prReview: 0, done: 1 });
  });

  test("cancelled requests are counted, and drawn in Done only when asked for", () => {
    expect(isCancelledRequest(requests[4]!)).toBe(true);
    const hidden = buildBoard(requests, { showCancelled: false });
    expect(hidden.cancelled).toBe(1);
    expect(hidden.counts.done).toBe(1);
    const shown = buildBoard(requests, { showCancelled: true });
    expect(shown.cancelled).toBe(1);
    expect(shown.counts.done).toBe(2);
    expect(shown.lanes[1]!.cells.done.requests.map((r) => r.id).sort()).toEqual([
      "b-cancelled",
      "b-done",
    ]);
  });

  test("a project whose only request is a hidden cancelled one has no lane", () => {
    const board = buildBoard([requestSummary({ id: "x", state: "cancelled", project: "gone" })], {
      showCancelled: false,
    });
    expect(board.lanes).toEqual([]);
    expect(board.cancelled).toBe(1);
  });

  test("Done shows a lane's newest 20 and says how many it left out", () => {
    const done = Array.from({ length: 23 }, (_, i) =>
      requestSummary({
        id: `done-${String(i).padStart(2, "0")}`,
        state: "done",
        updatedAt: `2026-09-10T09:${String(i).padStart(2, "0")}:00Z`,
      }),
    );
    const board = buildBoard(done, { showCancelled: false });
    const cell = board.lanes[0]!.cells.done;
    expect(cell.requests).toHaveLength(20);
    expect(cell.more).toBe(3);
    expect(cell.requests[0]!.id).toBe("done-22");
    expect(cell.requests.at(-1)!.id).toBe("done-03");
    // The header counts every finished request, not only the cards drawn.
    expect(board.counts.done).toBe(23);
    expect(board.lanes[0]!.count).toBe(23);
    expect(buildBoard(done, { showCancelled: false, doneCap: 5 }).lanes[0]!.cells.done.more).toBe(
      18,
    );
  });

  test("Needs you is oldest wait first; a waiting request follows the running ones in queue order", () => {
    const board = buildBoard(
      [
        requestSummary({ id: "late", state: "spec_review", waitingSince: "2026-09-10T10:00:00Z" }),
        requestSummary({ id: "early", state: "plan_review", waitingSince: "2026-09-10T08:00:00Z" }),
        wire({ id: "third", state: "planning" }, { queue_position: 2 }),
        wire({ id: "second", state: "submitted" }, { queue_position: 1 }),
        wire({ id: "running", state: "spec_drafting" }, {}),
      ],
      { showCancelled: false },
    );
    expect(board.lanes[0]!.cells.needsYou.requests.map((r) => r.id)).toEqual(["early", "late"]);
    expect(board.lanes[0]!.cells.drafting.requests.map((r) => r.id)).toEqual([
      "running",
      "second",
      "third",
    ]);
  });
});

describe("visibleColumns", () => {
  const none = { drafting: 0, needsYou: 0, building: 0, prReview: 0, done: 0 };

  test("no section filter draws all five", () => {
    expect(visibleColumns(null, none)).toEqual(boardColumns);
  });

  test("a section filter draws that section's columns", () => {
    expect(visibleColumns("needsYou", none)).toEqual(["needsYou"]);
    expect(visibleColumns("working", none)).toEqual(["drafting", "building", "prReview"]);
    expect(visibleColumns("finished", none)).toEqual(["done"]);
  });

  test("a column that holds a card is drawn even outside the section", () => {
    // An unknown state passes the Finished filter and sits in Drafting.
    expect(visibleColumns("finished", { ...none, drafting: 1 })).toEqual(["drafting", "done"]);
  });
});

test("showsNeedsYou is true exactly when a request in view waits on the operator", () => {
  expect(showsNeedsYou([requestSummary({ id: "a", state: "building" })])).toBe(false);
  expect(showsNeedsYou([requestSummary({ id: "a", state: "halted" })])).toBe(true);
});

describe("columnGroups", () => {
  const cell = (column: BoardColumn, requests: Parameters<typeof buildBoard>[0]) =>
    buildBoard(requests, { showCancelled: true }).lanes[0]?.cells[column].requests ?? [];
  const shape = (column: BoardColumn, requests: Parameters<typeof buildBoard>[0]) =>
    columnGroups(column, cell(column, requests)).map((group) => [
      group.label,
      group.requests.map((r) => r.id),
    ]);
  const readyPr = requestSummary({
    id: "pr",
    state: "pr_review",
    tickets: [ticketJson({ index: 1, prState: "ready" })],
  });

  test("Needs you is grouped by what is asked, in order", () => {
    expect(
      shape("needsYou", [
        requestSummary({ id: "q", state: "quarantined" }),
        readyPr,
        requestSummary({ id: "plan", state: "plan_review" }),
        requestSummary({ id: "oracle", state: "oracle_review" }),
        requestSummary({ id: "spec", state: "spec_review" }),
      ]),
    ).toEqual([
      ["Spec review", ["spec"]],
      ["Oracle review", ["oracle"]],
      ["Plan review", ["plan"]],
      ["PR ready", ["pr"]],
      ["Stuck", ["q"]],
    ]);
  });

  test("every Needs-you and Drafting state is in exactly one group", () => {
    const expected: Readonly<Record<string, [BoardColumn, string]>> = {
      spec_review: ["needsYou", "Spec review"],
      oracle_review: ["needsYou", "Oracle review"],
      plan_review: ["needsYou", "Plan review"],
      halted: ["needsYou", "Stuck"],
      quarantined: ["needsYou", "Stuck"],
      resume_review: ["needsYou", "Stuck"],
      submitted: ["drafting", "Spec and oracles"],
      spec_drafting: ["drafting", "Spec and oracles"],
      oracle_drafting: ["drafting", "Spec and oracles"],
      planning: ["drafting", "Planning"],
    };
    for (const [state, [column, label]] of Object.entries(expected)) {
      const request = requestSummary({ id: state, state });
      expect(columnForRequest(request), state).toBe(column);
      expect(shape(column, [request]), state).toEqual([[label, [state]]]);
    }
    expect(columnForRequest(readyPr)).toBe("needsYou");
    expect(shape("needsYou", [readyPr])).toEqual([["PR ready", ["pr"]]]);
  });

  test("a group with no request is left out, and an empty column has none", () => {
    expect(shape("needsYou", [requestSummary({ id: "h", state: "halted" })])).toEqual([
      ["Stuck", ["h"]],
    ]);
    expect(columnGroups("needsYou", [])).toEqual([]);
    expect(columnGroups("building", [])).toEqual([]);
  });

  test("a state this console does not know has a group, without throwing", () => {
    const unknown = requestSummary({ id: "new", state: "security_review" });
    expect(shape("drafting", [unknown])).toEqual([["Other", ["new"]]]);
    // Whatever is handed to Needs you that is not a review is Stuck.
    expect(columnGroups("needsYou", [unknown]).map((g) => g.label)).toEqual(["Stuck"]);
  });

  test("the columns without groups are one unnamed group", () => {
    for (const [column, state] of [
      ["building", "building"],
      ["done", "done"],
    ] as const) {
      expect(shape(column, [requestSummary({ id: "a", state })])).toEqual([[null, ["a"]]]);
    }
    const draftPr = requestSummary({
      id: "d",
      state: "pr_review",
      tickets: [ticketJson({ index: 1, prState: "draft" })],
    });
    expect(shape("prReview", [draftPr])).toEqual([[null, ["d"]]]);
  });

  test("inside a Needs-you group the longest wait is first", () => {
    expect(
      shape("needsYou", [
        requestSummary({ id: "late", state: "spec_review", waitingSince: "2026-09-10T10:00:00Z" }),
        requestSummary({ id: "halt-late", state: "halted", enteredAt: "2026-09-10T09:00:00Z" }),
        requestSummary({ id: "early", state: "spec_review", waitingSince: "2026-09-10T08:00:00Z" }),
        requestSummary({ id: "q-early", state: "quarantined", enteredAt: "2026-09-10T07:00:00Z" }),
      ]),
    ).toEqual([
      ["Spec review", ["early", "late"]],
      ["Stuck", ["q-early", "halt-late"]],
    ]);
  });

  test("inside a Drafting group the running jobs come first, then the queue by position", () => {
    expect(
      shape("drafting", [
        wire({ id: "spec-3", state: "submitted" }, { queue_position: 3 }),
        wire({ id: "plan-2", state: "planning" }, { queue_position: 2 }),
        wire({ id: "spec-1", state: "spec_drafting" }, { queue_position: 1 }),
        wire({ id: "plan-run", state: "planning" }, {}),
        wire({ id: "spec-run", state: "oracle_drafting" }, {}),
      ]),
    ).toEqual([
      ["Spec and oracles", ["spec-run", "spec-1", "spec-3"]],
      ["Planning", ["plan-run", "plan-2"]],
    ]);
  });

  test("grouping moves no request and changes no count", () => {
    const requests = [
      requestSummary({ id: "a", state: "spec_review" }),
      requestSummary({ id: "b", state: "halted" }),
      requestSummary({ id: "c", state: "planning" }),
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

describe("density of the grouped columns", () => {
  const waits = (n: number, state: string) =>
    Array.from({ length: n }, (_, i) =>
      requestSummary({
        id: `${state}-${i}`,
        state,
        waitingSince: `2026-09-10T0${i}:00:00Z`,
      }),
    );
  const none = { drafting: 0, needsYou: 0, building: 0, prReview: 0, done: 0 };

  test("Needs you and Drafting are the grouped columns", () => {
    expect(boardColumns.filter(isGroupedColumn)).toEqual(["drafting", "needsYou"]);
  });

  test("one chip per non-empty group, in the groups' order, with its real size", () => {
    const requests = [
      ...waits(3, "quarantined"),
      ...waits(5, "spec_review"),
      ...waits(2, "plan_review"),
    ];
    expect(groupChips("needsYou", requests)).toEqual([
      { label: "Spec review", short: "Spec", count: 5 },
      { label: "Plan review", short: "Plan", count: 2 },
      { label: "Stuck", short: "Stuck", count: 3 },
    ]);
    expect(groupChips("needsYou", [])).toEqual([]);
    // A column without groups has no chips.
    expect(groupChips("building", [requestSummary({ id: "b", state: "building" })])).toEqual([]);
  });

  test("a pressed chip is in force only while its group still holds a card", () => {
    const chips = groupChips("needsYou", waits(2, "spec_review"));
    expect(activeGroup("Spec review", chips)).toBe("Spec review");
    expect(activeGroup(null, chips)).toBeNull();
    // Its last request moved on: every group is shown again.
    expect(activeGroup("Plan review", chips)).toBeNull();
  });

  test("a group shows its first three and counts the rest behind +N more", () => {
    const [group] = columnGroups("needsYou", waits(5, "spec_review"));
    expect(groupPreviewSize).toBe(3);
    const folded = groupCards(group!, { expanded: false, alone: false });
    // The oldest three waits, in order.
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

  test("a group of three or fewer has nothing more; no card is ever dropped", () => {
    const [small] = columnGroups("needsYou", waits(3, "plan_review"));
    expect(groupCards(small!, { expanded: false, alone: false })).toEqual({
      shown: small!.requests,
      more: 0,
    });
    for (const size of [1, 3, 4, 20]) {
      const [group] = columnGroups("needsYou", waits(size, "halted"));
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
      "drafting",
      "prReview",
      "done",
    ]);
    // Building is empty in alpha's lane and not narrow: beta holds a card.
    expect(board.lanes[0]!.cells.building.requests).toEqual([]);
    expect(isNarrowColumn("building", board.counts)).toBe(false);
  });

  test("Needs you lays out two across once it is at least twice a normal column's width", () => {
    const all = boardColumns;
    // Five columns sharing: the normal width.
    expect(
      cardsAcross("needsYou", all, { drafting: 1, needsYou: 4, building: 1, prReview: 1, done: 1 }),
    ).toBe(1);
    // Three sharing: a third each, less than twice a fifth.
    expect(cardsAcross("needsYou", all, { ...none, needsYou: 4, building: 1, done: 2 })).toBe(1);
    // Two sharing: half each, more than twice a fifth.
    expect(cardsAcross("needsYou", all, { ...none, needsYou: 4, building: 1 })).toBe(2);
    expect(cardsAcross("needsYou", all, { ...none, needsYou: 4 })).toBe(2);
    // The section filter draws Needs you alone.
    expect(cardsAcross("needsYou", ["needsYou"], { ...none, needsYou: 1 })).toBe(2);
    // Never another column, and never an empty Needs you.
    expect(cardsAcross("building", all, { ...none, building: 9 })).toBe(1);
    expect(cardsAcross("needsYou", all, none)).toBe(1);
  });
});
