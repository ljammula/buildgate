import { activityLimit, recentActivity } from "@/domain/activity";
import { decodeRequestList, decodeRequestSummary } from "@/domain/request";
import { readFixtureJson } from "@/test/fixtures";
import { requestJson } from "@/test/requestFixtures";

const move = (from: string, to: string, at: string, by = "factoryd", reason?: string) => ({
  from,
  to,
  at,
  by,
  ...(reason === undefined ? {} : { reason }),
});
const withHistory = (id: string, title: string, history: readonly unknown[]) =>
  decodeRequestSummary({ ...requestJson({ id, state: "done", title }), history }, "test");

test("moves across requests come newest first, each naming its request", () => {
  const entries = recentActivity([
    withHistory("req-a", "Alpha", [
      move("submitted", "spec_drafting", "2026-09-10T09:00:00Z"),
      move("spec_drafting", "spec_review", "2026-09-10T09:05:00Z", "factoryd", "spec drafted"),
    ]),
    withHistory("req-b", "Beta", [move("spec_review", "planning", "2026-09-10T09:03:00Z", "kim")]),
  ]);
  expect(entries).toEqual([
    {
      requestId: "req-a",
      title: "Alpha",
      from: "spec_drafting",
      to: "spec_review",
      at: "2026-09-10T09:05:00Z",
      by: "factoryd",
      reason: "spec drafted",
    },
    {
      requestId: "req-b",
      title: "Beta",
      from: "spec_review",
      to: "planning",
      at: "2026-09-10T09:03:00Z",
      by: "kim",
      reason: "",
    },
    {
      requestId: "req-a",
      title: "Alpha",
      from: "submitted",
      to: "spec_drafting",
      at: "2026-09-10T09:00:00Z",
      by: "factoryd",
      reason: "",
    },
  ]);
});

test("two moves of one request at the same instant keep the later one first", () => {
  const entries = recentActivity([
    withHistory("req-a", "Alpha", [
      move("submitted", "spec_drafting", "2026-09-10T09:00:00Z"),
      move("spec_drafting", "spec_review", "2026-09-10T09:00:00Z"),
    ]),
  ]);
  expect(entries.map((entry) => entry.to)).toEqual(["spec_review", "spec_drafting"]);
});

test("timestamps are compared as instants, not as text", () => {
  const entries = recentActivity([
    withHistory("req-a", "Alpha", [move("a", "whole", "2026-09-10T09:00:05Z")]),
    withHistory("req-b", "Beta", [move("a", "fraction", "2026-09-10T09:00:05.5Z")]),
  ]);
  expect(entries.map((entry) => entry.to)).toEqual(["fraction", "whole"]);
});

test("the feed is capped", () => {
  const history = Array.from({ length: 20 }, (_, i) =>
    move("a", `s${i}`, `2026-09-10T09:${String(i).padStart(2, "0")}:00Z`),
  );
  const entries = recentActivity([withHistory("req-a", "Alpha", history)]);
  expect(activityLimit).toBe(15);
  expect(entries).toHaveLength(15);
  expect(entries[0]?.to).toBe("s19");
  expect(recentActivity([withHistory("req-a", "Alpha", history)], 3)).toHaveLength(3);
});

test("the fixture list yields its moves; a request with no history yields none", () => {
  const requests = decodeRequestList(readFixtureJson("api/requests.json"), "GET /requests");
  const entries = recentActivity(requests, 100);
  expect(entries).toHaveLength(15);
  expect(entries[0]?.at).toBe("2026-09-10T09:50:00Z");
  expect(
    recentActivity([decodeRequestSummary(requestJson({ id: "x", state: "done" }), "t")]),
  ).toEqual([]);
});
