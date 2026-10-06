import type { ProgressEvent } from "@/domain/run";
import {
  addProgressEvent,
  emptyProgressFeed,
  maxWorkerEvents,
  progressKey,
} from "@/features/run-detail/progressEvents";

function event(over: Partial<ProgressEvent> = {}): ProgressEvent {
  return {
    ts: new Date("2026-09-17T10:00:00.000Z"),
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

test("the key is the timestamp, source, stage, event, round and detail", () => {
  expect(progressKey(event({ source: "worker", stage: "agent", round: 2, detail: "x" }))).toBe(
    "2026-09-17T10:00:00.000Z|worker|agent|start|2|x",
  );
});

test("a replayed line is not added twice", () => {
  const once = addProgressEvent(emptyProgressFeed, event());
  const twice = addProgressEvent(once, event());
  expect(twice).toBe(once);
  expect(twice.events).toHaveLength(1);
});

test("lines that differ in any key field are all kept, in arrival order", () => {
  let feed = emptyProgressFeed;
  feed = addProgressEvent(feed, event({ detail: "a" }));
  feed = addProgressEvent(feed, event({ detail: "b" }));
  feed = addProgressEvent(feed, event({ round: 1 }));
  expect(feed.events.map((e) => `${e.detail}${e.round}`)).toEqual(["a0", "b0", "1"]);
});

test("past the cap the oldest worker line is dropped and factory lines are kept", () => {
  let feed = addProgressEvent(emptyProgressFeed, event({ stage: "prepare_workspace" }));
  for (let i = 0; i < maxWorkerEvents + 2; i++) {
    feed = addProgressEvent(feed, event({ source: "worker", stage: "agent", detail: `n${i}` }));
  }
  const workers = feed.events.filter((e) => e.source === "worker");
  expect(workers).toHaveLength(maxWorkerEvents);
  expect(workers[0]?.detail).toBe("n2");
  expect(feed.events.filter((e) => e.source === "factory")).toHaveLength(1);
});

test("a dropped line stays seen, so a replay does not bring it back", () => {
  let feed = emptyProgressFeed;
  for (let i = 0; i < maxWorkerEvents + 1; i++) {
    feed = addProgressEvent(feed, event({ source: "worker", stage: "agent", detail: `n${i}` }));
  }
  const replayed = addProgressEvent(
    feed,
    event({ source: "worker", stage: "agent", detail: "n0" }),
  );
  expect(replayed).toBe(feed);
});
