import { recentActivity } from "@/domain/activity";
import type { BoardWindowDays } from "@/domain/boardFilters";
import {
  activityInBoardWindow,
  applyBoardWindow,
  boardWindowChoiceLabel,
  boardWindowLabel,
  instantInBoardWindow,
  requestInBoardWindow,
  scopeLabel,
  statsSince,
} from "@/domain/boardWindow";
import { decodeRequestSummary } from "@/domain/request";
import { requestJson, requestSummary, ticketJson } from "@/test/requestFixtures";

const now = new Date("2026-10-01T12:00:00Z");
const threeWeeksAgo = "2026-09-10T12:00:00Z";
const twoDaysAgo = "2026-09-29T12:00:00Z";
const windows: readonly BoardWindowDays[] = [7, 30, "all"];

describe("requestInBoardWindow", () => {
  test("a three-week-old request in Needs you stays in view under 7 days", () => {
    const waiting = requestSummary({ id: "old", state: "spec_review", updatedAt: threeWeeksAgo });
    expect(requestInBoardWindow(waiting, 7, now)).toBe(true);
  });

  test("no request in Drafting, Needs you, Building or PR review is ever hidden, whatever its age", () => {
    const ancient = "2025-01-01T00:00:00Z";
    const inFlight = [
      "submitted",
      "spec_drafting",
      "oracle_drafting",
      "planning",
      "spec_review",
      "oracle_review",
      "plan_review",
      "resume_review",
      "halted",
      "quarantined",
      "building",
      "pr_review",
      // A state this console does not know is not finished work either.
      "security_review",
    ].map((state) => requestSummary({ id: state, state, updatedAt: ancient }));
    const readyPr = requestSummary({
      id: "ready",
      state: "pr_review",
      updatedAt: ancient,
      tickets: [ticketJson({ index: 1, prState: "ready" })],
    });
    for (const days of windows) {
      for (const request of [...inFlight, readyPr]) {
        expect(requestInBoardWindow(request, days, now), `${request.id} under ${days}`).toBe(true);
      }
    }
  });

  test("a done or cancelled request is judged by its last update", () => {
    for (const state of ["done", "cancelled"]) {
      const old = requestSummary({ id: "old", state, updatedAt: threeWeeksAgo });
      const recent = requestSummary({ id: "new", state, updatedAt: twoDaysAgo });
      expect(requestInBoardWindow(old, 7, now)).toBe(false);
      expect(requestInBoardWindow(old, 30, now)).toBe(true);
      expect(requestInBoardWindow(old, "all", now)).toBe(true);
      expect(requestInBoardWindow(recent, 7, now)).toBe(true);
    }
  });

  test("the edge of the window is inside it; an unreadable time is kept in view", () => {
    const edge = requestSummary({ id: "e", state: "done", updatedAt: "2026-09-24T12:00:00Z" });
    const justOut = requestSummary({ id: "o", state: "done", updatedAt: "2026-09-24T11:59:59Z" });
    expect(requestInBoardWindow(edge, 7, now)).toBe(true);
    expect(requestInBoardWindow(justOut, 7, now)).toBe(false);
    const unreadable = requestSummary({ id: "u", state: "done", updatedAt: "not a time" });
    expect(requestInBoardWindow(unreadable, 7, now)).toBe(true);
  });
});

test("applyBoardWindow takes out only old finished requests and counts them", () => {
  const requests = [
    requestSummary({ id: "waiting", state: "plan_review", updatedAt: threeWeeksAgo }),
    requestSummary({ id: "building", state: "building", updatedAt: threeWeeksAgo }),
    requestSummary({ id: "old-done", state: "done", updatedAt: threeWeeksAgo }),
    requestSummary({ id: "old-cancelled", state: "cancelled", updatedAt: threeWeeksAgo }),
    requestSummary({ id: "new-done", state: "done", updatedAt: twoDaysAgo }),
  ];
  const week = applyBoardWindow(requests, 7, now, true);
  expect(week.shown.map((r) => r.id)).toEqual(["waiting", "building", "new-done"]);
  expect(week.olderHidden).toBe(2);
  expect(applyBoardWindow(requests, 30, now, true).olderHidden).toBe(0);
  expect(applyBoardWindow(requests, "all", now, true).shown).toHaveLength(5);
  // A cancelled request the view would not draw anyway is not promised by the note.
  const boardWeek = applyBoardWindow(requests, 7, now, false);
  expect(boardWeek.olderHidden).toBe(1);
  expect(boardWeek.shown.map((r) => r.id)).toEqual(["waiting", "building", "new-done"]);
});

test("the scope label names the window, and the projects when some are chosen", () => {
  expect(scopeLabel("Last 7 days", new Set())).toBe("Last 7 days");
  expect(scopeLabel("All time", new Set(["web", "api"]))).toBe("All time · api, web");
});

test("an instant is judged against the window's edge", () => {
  expect(instantInBoardWindow(twoDaysAgo, 7, now)).toBe(true);
  expect(instantInBoardWindow(threeWeeksAgo, 7, now)).toBe(false);
  expect(instantInBoardWindow(threeWeeksAgo, "all", now)).toBe(true);
  expect(instantInBoardWindow("", 7, now)).toBe(true);
});

test("activity keeps the moves made inside the window", () => {
  const request = decodeRequestSummary(
    {
      ...requestJson({ id: "req-a", state: "spec_review" }),
      history: [
        { from: "submitted", to: "spec_drafting", at: threeWeeksAgo, by: "factoryd" },
        { from: "spec_drafting", to: "spec_review", at: twoDaysAgo, by: "factoryd" },
      ],
    },
    "test",
  );
  const all = recentActivity([request]);
  expect(activityInBoardWindow(all, 7, now).map((entry) => entry.to)).toEqual(["spec_review"]);
  expect(activityInBoardWindow(all, 30, now)).toHaveLength(2);
  expect(activityInBoardWindow(all, "all", now)).toHaveLength(2);
});

test("the window as GET /stats's since, and as words", () => {
  expect(windows.map(statsSince)).toEqual(["7d", "30d", null]);
  expect(windows.map(boardWindowChoiceLabel)).toEqual(["7 days", "30 days", "All"]);
  expect(windows.map(boardWindowLabel)).toEqual(["Last 7 days", "Last 30 days", "All time"]);
});
