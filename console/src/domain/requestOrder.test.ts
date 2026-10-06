import { requestStageGroup } from "@/domain/boardFilters";
import { needsHumanCount, sortedRequests, waitingBadgeLabel } from "@/domain/requestOrder";
import { requestSummary } from "@/test/requestFixtures";

function summary(o: {
  id: string;
  state: string;
  updatedAt?: string;
  enteredAt?: string;
  waitingSince?: string;
}) {
  return requestSummary({
    project: "app",
    updatedAt: "2026-09-10T09:00:00Z",
    enteredAt: "2026-09-10T09:00:00Z",
    ...o,
  });
}

const ids = (list: readonly { id: string }[]) => list.map((r) => r.id);

describe("sortedRequests", () => {
  test("a review-state request sorts above a working one regardless of age", () => {
    const working = summary({
      id: "working",
      state: "building",
      updatedAt: "2026-09-10T12:00:00Z",
    });
    const review = summary({
      id: "review",
      state: "spec_review",
      enteredAt: "2026-09-10T05:00:00Z",
    });
    expect(ids(sortedRequests([working, review]))).toEqual(["review", "working"]);
  });

  test("two review-state requests sort oldest-wait first", () => {
    const newer = summary({
      id: "newer",
      state: "plan_review",
      waitingSince: "2026-09-10T09:00:00Z",
    });
    const older = summary({
      id: "older",
      state: "spec_review",
      waitingSince: "2026-09-10T07:00:00Z",
    });
    expect(ids(sortedRequests([newer, older]))).toEqual(["older", "newer"]);
  });

  test("working states sort by updatedAt descending", () => {
    const stale = summary({ id: "stale", state: "building", updatedAt: "2026-09-10T08:00:00Z" });
    const fresh = summary({ id: "fresh", state: "planning", updatedAt: "2026-09-10T10:00:00Z" });
    expect(ids(sortedRequests([stale, fresh]))).toEqual(["fresh", "stale"]);
  });

  test("a resume_review request needs a human and sorts with review", () => {
    expect(requestStageGroup("resume_review")).toBe("review");
    const review = summary({ id: "lost", state: "resume_review" });
    const working = summary({ id: "working", state: "building" });
    expect(sortedRequests([working, review])[0]?.id).toBe("lost");
  });

  test("done and failed requests sort after review and working", () => {
    const sorted = sortedRequests([
      summary({ id: "done", state: "done" }),
      summary({ id: "failed", state: "cancelled" }),
      summary({ id: "review", state: "spec_review" }),
      summary({ id: "working", state: "building" }),
    ]);
    expect(sorted[0]?.id).toBe("review");
    expect(sorted[1]?.id).toBe("working");
    expect(new Set(ids(sorted.slice(2)))).toEqual(new Set(["done", "failed"]));
  });

  test("an unparseable timestamp sorts as the oldest instead of throwing", () => {
    const bad = summary({ id: "bad", state: "building", updatedAt: "not a time" });
    const good = summary({ id: "good", state: "building" });
    expect(ids(sortedRequests([bad, good]))).toEqual(["good", "bad"]);
  });

  test("does not mutate its input", () => {
    const input = [summary({ id: "a", state: "done" }), summary({ id: "b", state: "spec_review" })];
    sortedRequests(input);
    expect(ids(input)).toEqual(["a", "b"]);
  });
});

describe("waitingBadgeLabel", () => {
  test("renders with the age for a review state", () => {
    const request = summary({
      id: "req-1",
      state: "spec_review",
      waitingSince: "2026-09-10T09:00:00Z",
    });
    expect(waitingBadgeLabel(request, new Date("2026-09-10T09:45:00Z"))).toBe(
      "Waiting on you · 45m",
    );
  });

  test("renders hours and minutes past an hour", () => {
    const request = summary({
      id: "req-1",
      state: "plan_review",
      waitingSince: "2026-09-10T09:00:00Z",
    });
    expect(waitingBadgeLabel(request, new Date("2026-09-10T11:05:00Z"))).toBe(
      "Waiting on you · 2h 05m",
    );
  });

  test("is null outside a review state", () => {
    expect(waitingBadgeLabel(summary({ id: "req-1", state: "building" }), new Date())).toBeNull();
  });

  test("an exact number of hours drops the minutes", () => {
    const request = summary({
      id: "r",
      state: "spec_review",
      waitingSince: "2026-09-10T09:00:00Z",
    });
    expect(waitingBadgeLabel(request, new Date("2026-09-10T11:00:00Z"))).toBe(
      "Waiting on you · 2h",
    );
  });
});

describe("needsHumanCount", () => {
  test("counts halted requests", () => {
    expect(
      needsHumanCount([
        summary({ id: "a", state: "halted" }),
        summary({ id: "b", state: "done" }),
        summary({ id: "c", state: "spec_review" }),
      ]),
    ).toBe(2);
  });
});
