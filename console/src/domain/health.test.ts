import { factoryHealth, isRunningNow, lastTransitionAt } from "@/domain/health";
import { decodeQueueRunStatus } from "@/domain/ops";
import { decodeRequestList, decodeRequestSummary } from "@/domain/request";
import { asObject } from "@/domain/decode";
import { readFixtureJson } from "@/test/fixtures";
import { requestJson } from "@/test/requestFixtures";

const fixtureRequests = decodeRequestList(readFixtureJson("api/requests.json"), "GET /requests");
const status = (body: Record<string, unknown>) => decodeQueueRunStatus(body, "GET /queue-run");
const wire = (o: Parameters<typeof requestJson>[0], extra: Record<string, unknown> = {}) =>
  decodeRequestSummary({ ...requestJson(o), ...extra }, "test");

test("a live worker: its slots, and the build it runs with ticket, stage and stalled verdict", () => {
  const health = factoryHealth(
    status({ state: "alive", active_requests: ["req-building"], job_slots: 2 }),
    fixtureRequests,
  );
  expect(health.worker).toEqual({ label: "Running", tone: "success", alive: true });
  expect(health.slots).toEqual({ busy: 1, total: 2 });
  expect(health.running).toEqual([
    {
      requestId: "req-building",
      title: "Add idempotency keys to checkout",
      detail: "ticket 2 of 2 · build",
      stalled: true,
    },
  ]);
});

test("a running build names its round; a drafting job names its stage", () => {
  const health = factoryHealth(
    status({ state: "alive", active_requests: ["req-b", "req-d"], job_slots: 2 }),
    [
      wire(
        { id: "req-b", state: "building", title: "Build it" },
        { build: { run_id: "run-1", ticket: 1, tickets: 3, stage: "verify", round: 2 } },
      ),
      wire({ id: "req-d", state: "planning", title: "Plan it" }),
    ],
  );
  expect(health.running.map((job) => [job.title, job.detail, job.stalled])).toEqual([
    ["Build it", "ticket 1 of 3 · round 2 · verify", false],
    ["Plan it", "Planning", false],
  ]);
});

test("an active request the list does not hold yet is named by its id", () => {
  const health = factoryHealth(status({ state: "alive", active_requests: ["req-new"] }), []);
  expect(health.running).toEqual([
    { requestId: "req-new", title: "req-new", detail: "", stalled: false },
  ]);
  // No job_slots from the server: no capacity is claimed.
  expect(health.slots).toBeNull();
});

test("the stale fixture: not alive, nothing running, its last heartbeat kept", () => {
  const health = factoryHealth(
    decodeQueueRunStatus(asObject(readFixtureJson("api/queue-run.json"), "GET /queue-run"), "x"),
    fixtureRequests,
  );
  expect(health.worker).toEqual({ label: "Stale", tone: "warning", alive: false });
  expect(health.lastHeartbeat).toBe("2026-09-10T09:50:00Z");
  expect(health.slots).toBeNull();
  expect(health.running).toEqual([]);
});

test("absent reads as not running; no answer at all is no worker fact", () => {
  expect(factoryHealth(status({}), []).worker).toEqual({
    label: "Not running",
    tone: "danger",
    alive: false,
  });
  expect(factoryHealth(null, []).worker).toBeNull();
});

test("queued counts the requests a live worker's server gave a position", () => {
  const requests = [
    wire({ id: "a", state: "submitted" }, { queue_position: 1 }),
    wire({ id: "b", state: "building" }, { queue_position: 2 }),
    wire({ id: "c", state: "building" }),
    wire({ id: "d", state: "spec_review" }),
  ];
  const health = factoryHealth(status({ state: "alive" }), requests);
  expect(health.queued).toBe(2);
  expect(health.needWorker).toBe(3);
});

test("with no live worker there is no queue length, only what needs a worker", () => {
  // The server gives no queue position without a live worker: five requests
  // in job states must never read as an empty queue.
  const requests = ["submitted", "spec_drafting", "oracle_drafting", "planning", "building"].map(
    (state) => wire({ id: state, state }),
  );
  const waiting = [
    ...requests,
    wire({ id: "r", state: "plan_review" }),
    wire({ id: "x", state: "done" }),
  ];
  for (const body of [{ state: "stale" }, { state: "absent" }, {}]) {
    const health = factoryHealth(status(body), waiting);
    expect(health.queued).toBeNull();
    expect(health.needWorker).toBe(5);
    expect(health.running).toEqual([]);
  }
  const unknown = factoryHealth(null, waiting);
  expect(unknown.queued).toBeNull();
  expect(unknown.needWorker).toBe(5);
});

test("the last transition is the newest move of any request", () => {
  const history = (...ats: string[]) => ({
    history: ats.map((at) => ({ from: "a", to: "b", at, by: "factoryd" })),
  });
  expect(
    lastTransitionAt([
      wire({ id: "a", state: "done" }, history("2026-09-10T09:00:00Z", "2026-09-10T09:30:00Z")),
      wire({ id: "b", state: "done" }, history("2026-09-10T09:30:00.5Z")),
      wire({ id: "c", state: "done" }),
    ]),
  ).toBe("2026-09-10T09:30:00.5Z");
  expect(lastTransitionAt([wire({ id: "c", state: "done" })])).toBeNull();
  expect(factoryHealth(null, fixtureRequests).lastTransitionAt).toBe("2026-09-10T09:50:00Z");
});

describe("isRunningNow", () => {
  const health = {
    running: [
      { requestId: "ok", title: "ok", detail: "", stalled: false },
      { requestId: "stuck", title: "stuck", detail: "", stalled: true },
    ],
  };

  test("a healthy running job on a live feed", () => {
    expect(isRunningNow(health, "ok", true)).toBe(true);
  });

  test("a stalled job, a feed that is not live, or a request not running is not", () => {
    expect(isRunningNow(health, "stuck", true)).toBe(false);
    expect(isRunningNow(health, "ok", false)).toBe(false);
    expect(isRunningNow(health, "queued", true)).toBe(false);
  });

  test("no worker lists nothing running", () => {
    expect(isRunningNow(factoryHealth(null, []), "ok", true)).toBe(false);
  });
});
