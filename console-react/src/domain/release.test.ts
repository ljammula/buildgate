import { DecodeError, asObject } from "@/domain/decode";
import { decodeProjectReleaseView, decodeReleaseView } from "@/domain/release";
import { readFixtureJson } from "@/test/fixtures";

const engagedSwitch = {
  project: "app",
  engaged: true,
  history: [
    {
      engaged: true,
      by: "operator@example.com",
      reason: "incident 42",
      at: "2026-09-10T09:26:00Z",
    },
  ],
};

const release = (file: string) =>
  decodeReleaseView(asObject(readFixtureJson(file), file), "GET /runs/{id}/release");

test("GET /projects/{project}/release decodes", () => {
  const view = decodeProjectReleaseView(
    asObject(readFixtureJson("api/project-release.json"), "project-release.json"),
    "GET /projects/{project}/release",
  );
  expect(view).toEqual({ project: "app", killSwitch: engagedSwitch });
});

test("GET /runs/{id}/release decodes a denied decision", () => {
  const view = release("api/run-release.json");
  expect(view.runId).toBe("run-accepted");
  expect(view.project).toBe("app");
  expect(view.decision).toEqual({
    runId: "run-accepted",
    project: "app",
    allowed: false,
    reasons: ['kill switch is engaged for project "app"'],
    evaluatedAt: "2026-09-10T09:27:00Z",
  });
  expect(view.recordingFailure).toBeNull();
  expect(view.killSwitch).toEqual(engagedSwitch);
});

test("a run with no decision decodes as null, never as an allowed decision", () => {
  const view = release("api/run-release-no-decision.json");
  expect(view.runId).toBe("run-quarantined");
  expect(view.decision).toBeNull();
  expect(view.recordingFailure).toBeNull();
  expect(view.killSwitch.engaged).toBe(true);
});

test("a recording failure is kept apart from a missing decision", () => {
  const view = decodeReleaseView(
    {
      run_id: "r",
      project: "p",
      decision: null,
      recording_failure: { error: "kill-switch.json is corrupt", at: "2026-09-10T09:00:00Z" },
      kill_switch: { project: "p" },
    },
    "GET /runs/{id}/release",
  );
  expect(view.decision).toBeNull();
  expect(view.recordingFailure).toEqual({
    error: "kill-switch.json is corrupt",
    at: "2026-09-10T09:00:00Z",
    killSwitchReadable: false,
  });
  expect(view.killSwitch).toEqual({ project: "p", engaged: false, history: [] });
});

test("an allowed decision with omitted reasons has an empty list", () => {
  const view = decodeReleaseView(
    {
      run_id: "r",
      project: "p",
      decision: { run_id: "r", project: "p", allowed: true, evaluated_at: "t" },
      kill_switch: { project: "p", engaged: false, history: null },
    },
    "GET /runs/{id}/release",
  );
  expect(view.decision?.reasons).toEqual([]);
  expect(view.killSwitch.history).toEqual([]);
});

test("a release view missing its kill switch names the route and the field", () => {
  const bad = { run_id: "r", project: "p" };
  expect(() => decodeReleaseView(bad, "GET /runs/{id}/release")).toThrow(DecodeError);
  expect(() => decodeReleaseView(bad, "GET /runs/{id}/release")).toThrow(
    /GET \/runs\/\{id\}\/release\.kill_switch:/,
  );
});
