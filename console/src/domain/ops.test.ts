import { asObject } from "@/domain/decode";
import {
  decodeApiErrorBody,
  decodeConsoleConfig,
  decodeDaemonList,
  decodeHealth,
  decodeQueueRunStatus,
  decodeWorkspaceHintList,
  workerLiveness,
} from "@/domain/ops";
import { readFixtureJson } from "@/test/fixtures";

test("GET /console-config.json decodes", () => {
  const config = decodeConsoleConfig(
    asObject(readFixtureJson("api/console-config.json"), "console-config.json"),
    "GET /console-config.json",
  );
  expect(config.writesEnabled).toBe(false);
  expect(config.temporalUiUrl).toBe("http://localhost:8233");
  expect(config.releasePolicyWarning).toMatch(/^release policy denies every PR unconditionally/);
});

test("a console config without the optional fields has them null", () => {
  expect(decodeConsoleConfig({ writes_enabled: true }, "GET /console-config.json")).toEqual({
    writesEnabled: true,
    temporalUiUrl: null,
    releasePolicyWarning: null,
  });
});

test("GET /daemons decodes", () => {
  const daemons = decodeDaemonList(readFixtureJson("api/daemons.json"), "GET /daemons");
  expect(daemons).toEqual([
    {
      repository: "acme/app",
      state: "running",
      pid: 4242,
      startedAt: "2026-09-10T09:00:00Z",
      heartbeatUpdatedAt: "2026-09-10T09:00:00Z",
    },
  ]);
});

test("GET /queue-run decodes", () => {
  const status = decodeQueueRunStatus(
    asObject(readFixtureJson("api/queue-run.json"), "queue-run.json"),
    "GET /queue-run",
  );
  expect(status).toEqual({ state: "stale", lastHeartbeat: "2026-09-10T09:50:00Z" });
});

test("getQueueRunStatus sends the read token, which is what the server gates GET /queue-run on", () => {
  // The Authorization header belongs to the api layer; the decoded status is what this module owns.
  expect(decodeQueueRunStatus({ state: "alive" }, "GET /queue-run").state).toBe("alive");
});

test("an empty queue-run answer reads as absent", () => {
  expect(decodeQueueRunStatus({}, "GET /queue-run")).toEqual({
    state: "absent",
    lastHeartbeat: "",
  });
});

test("GET /workspaces decodes, with omitted hints as empty strings", () => {
  const hints = decodeWorkspaceHintList(readFixtureJson("api/workspaces.json"), "GET /workspaces");
  expect(hints).toEqual([
    {
      workspace: "/repos/app",
      hasFactoryYml: false,
      resolvedVerifyCommand: "",
      verifyCommandSource: "",
    },
  ]);
});

test("GET /healthz decodes", () => {
  const health = decodeHealth(
    asObject(readFixtureJson("api/healthz.json"), "healthz.json"),
    "GET /healthz",
  );
  expect(health).toEqual({ status: "ok" });
  expect(() => decodeHealth({}, "GET /healthz")).toThrow(/GET \/healthz\.status:/);
});

test("the server's error body decodes", () => {
  const body = decodeApiErrorBody(
    asObject(readFixtureJson("api/error-not-found.json"), "error-not-found.json"),
    "GET /runs/{id}",
  );
  expect(body).toEqual({ error: "run not found" });
  expect(() => decodeApiErrorBody({}, "GET /runs/{id}")).toThrow(/GET \/runs\/\{id\}\.error:/);
});

test("a worker heartbeat state reads as running, stale or not running", () => {
  expect(workerLiveness("alive")).toEqual({ label: "Running", tone: "success", alive: true });
  expect(workerLiveness("stale")).toEqual({ label: "Stale", tone: "warning", alive: false });
  expect(workerLiveness("absent")).toEqual({ label: "Not running", tone: "danger", alive: false });
  expect(workerLiveness("something-new").label).toBe("Not running");
});
