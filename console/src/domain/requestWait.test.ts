import { decodeRequestSummary } from "@/domain/request";
import { waitChipLabel, waitLine } from "@/domain/requestWait";
import { requestJson } from "@/test/requestFixtures";

const waitOf = (wait: Record<string, unknown> | undefined) =>
  decodeRequestSummary(
    { ...requestJson({ id: "a", state: "building" }), ...(wait === undefined ? {} : { wait }) },
    "test",
  ).wait;

describe("waitLine", () => {
  test("a job slot names the server's position and what the request is behind", () => {
    expect(waitLine(waitOf({ kind: "job_slot", position: 2, slots: 3 }))).toBe(
      "Queued · position 2",
    );
    expect(waitLine(waitOf({ kind: "job_slot", position: 1, on: "req-ahead", slots: 1 }))).toBe(
      "Queued · position 1 · behind req-ahead",
    );
    expect(waitLine(waitOf({ kind: "job_slot", on: "run-9" }))).toBe("Queued · behind run-9");
  });

  test("each other kind reads as its own sentence", () => {
    expect(waitLine(waitOf({ kind: "repository" }))).toBe(
      "Waiting for the repository · another build holds it",
    );
    expect(waitLine(waitOf({ kind: "repository_queue", on: "behind 2 run(s) on acme/app" }))).toBe(
      "Waiting in the repository queue · behind 2 run(s) on acme/app",
    );
    expect(waitLine(waitOf({ kind: "model_host", on: "queued behind run-a" }))).toBe(
      "Waiting for the model host · queued behind run-a",
    );
    expect(waitLine(waitOf({ kind: "compose_slot", on: "queued behind run-b" }))).toBe(
      "Waiting for a sidecar slot · queued behind run-b",
    );
    expect(waitLine(waitOf({ kind: "no_worker" }))).toBe("No worker is running");
  });

  test("a kind this console does not know still reads as a wait", () => {
    expect(waitLine(waitOf({ kind: "something_new" }))).toBe("Waiting");
    expect(waitChipLabel(waitOf({ kind: "something_new" })!)).toBe("Waiting");
  });

  test("is null for a request that is not waiting", () => {
    expect(waitLine(waitOf(undefined))).toBeNull();
  });
});
