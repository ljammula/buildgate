import { decodeRequestSummary } from "@/domain/request";
import { requestJson } from "@/test/requestFixtures";

import { boardFreshness, freshnessLabel, queueRunWarning } from "./boardModel";

function summary(o: { id: string; state: string }) {
  return decodeRequestSummary(requestJson({ project: "app", ...o }), "test");
}

describe("queueRunWarning", () => {
  const now = new Date("2026-09-24T09:05:00Z");
  const building = [summary({ id: "a", state: "building" })];
  const done = [summary({ id: "a", state: "done" })];

  test("stale warns, with the heartbeat age", () => {
    const text = queueRunWarning(
      { state: "stale", lastHeartbeat: "2026-09-24T09:00:00Z" },
      [],
      now,
    );
    expect(text).toBe(
      "worker is not running (last heartbeat 5m ago) -- requests won't advance; start `factoryd worker`",
    );
  });

  test("stale without a parseable heartbeat has no age", () => {
    expect(queueRunWarning({ state: "stale", lastHeartbeat: "" }, [], now)).toBe(
      "worker is not running -- requests won't advance; start `factoryd worker`",
    );
  });

  test("absent warns only when a request waits on a worker", () => {
    const status = { state: "absent", lastHeartbeat: "" };
    expect(queueRunWarning(status, building, now)).toContain("no worker has run");
    expect(queueRunWarning(status, done, now)).toBeNull();
  });

  test("alive and unknown never warn", () => {
    expect(queueRunWarning({ state: "alive", lastHeartbeat: "" }, building, now)).toBeNull();
    expect(queueRunWarning(null, building, now)).toBeNull();
  });
});

describe("freshness", () => {
  test("boardFreshness: connected always wins", () => {
    expect(boardFreshness({ live: true, disconnected: true })).toBe("live");
    expect(boardFreshness({ live: false, disconnected: false })).toBe("recent");
    expect(boardFreshness({ live: false, disconnected: true })).toBe("disconnected");
  });

  test("freshnessLabel", () => {
    const now = new Date("2026-09-10T09:00:30Z");
    expect(freshnessLabel("live", null, now)).toBe("Live");
    expect(freshnessLabel("disconnected", null, now)).toBe("Disconnected");
    expect(freshnessLabel("recent", null, now)).toBe("Connecting…");
    expect(freshnessLabel("recent", Date.parse("2026-09-10T09:00:00Z"), now)).toBe(
      "Last updated 30s ago",
    );
  });
});
