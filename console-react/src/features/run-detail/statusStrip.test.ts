import { type ProgressEvent, decodeRun } from "@/domain/run";
import { computeStatusStrip } from "@/features/run-detail/statusStrip";
import { acceptedRun, inProgressRun } from "@/features/run-detail/testRuns";

function event(over: Partial<ProgressEvent>): ProgressEvent {
  return {
    ts: new Date("2026-08-26T12:00:10.000Z"),
    source: "factory",
    stage: "build",
    event: "start",
    round: 0,
    maxRounds: 0,
    outcome: "",
    detail: "",
    ...over,
  };
}

const now = new Date("2026-08-26T12:02:00.000Z");

test("a running run shows its stage, round, elapsed time and last activity", () => {
  const run = decodeRun(inProgressRun(), "test");
  const strip = computeStatusStrip(
    [
      event({}),
      event({ source: "worker", stage: "round", round: 2, maxRounds: 5, ts: new Date(now) }),
    ],
    run,
    now,
  );
  expect(strip).toEqual({
    label: "Build",
    round: "Round 2/5",
    elapsed: "02:00",
    // The newer of the feed's last factory line (12:00:10) and the run's field (none).
    lastActivity: "01:50",
  });
});

test("a run with no lines is Pending, and a waiting reason replaces the stage label", () => {
  const run = decodeRun(inProgressRun(), "test");
  expect(computeStatusStrip([], run, now).label).toBe("Pending");
  const waiting = decodeRun({ ...inProgressRun(), waiting_reason: "behind 1 run" }, "test");
  expect(computeStatusStrip([event({})], waiting, now).label).toBe("behind 1 run");
});

test("a finished run counts elapsed time to its finished line and has no last activity", () => {
  const run = decodeRun(acceptedRun(), "test");
  const strip = computeStatusStrip(
    [event({ stage: "finished", event: "end", ts: new Date("2026-08-26T11:03:00.000Z") })],
    run,
    now,
  );
  expect(strip.elapsed).toBe("03:00");
  expect(strip.lastActivity).toBeNull();
  expect(strip.round).toBeNull();
});
