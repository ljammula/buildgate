import { DecodeError, type JsonObject, asObject } from "@/domain/decode";
import {
  type ActiveJob,
  type RequestSummary,
  activeJobLabel,
  decodeRequestList,
  decodeRequestSummary,
  decodeRevisionDetail,
  decodeRevisionList,
  rejectionStage,
  requestAwaitingPullRequest,
  requestAwaitingPullRequestLabel,
  requestRunningJob,
  requestShortTitle,
  requestWaitingSinceOrEnteredAt,
} from "@/domain/request";
import { readFixtureJson, readFixtureText } from "@/test/fixtures";

const route = "GET /requests/{id}";

function detail(name: string): RequestSummary {
  return decodeRequestSummary(asObject(readFixtureJson(`api/${name}.json`), route), route);
}

// A minimal request body, for tests that vary one field.
function requestJson(overrides: JsonObject = {}): JsonObject {
  return {
    id: "req-1",
    workspace: "/w",
    project: "p",
    state: "planning",
    submitted_at: "2026-09-28T00:00:00Z",
    updated_at: "2026-09-28T00:00:00Z",
    ...overrides,
  };
}

function request(overrides: JsonObject = {}): RequestSummary {
  return decodeRequestSummary(requestJson(overrides), route);
}

const planningJob = {
  stage: "planning",
  role: "planning",
  model: "luna",
  model_id: "gpt-5.6-luna",
  harness: "pi",
  thinking: "max",
  route: "codex",
  started_at: "2026-09-28T02:32:00Z",
};

test("a running job parses and labels role, model, harness, effort and route", () => {
  const job = requestRunningJob(request({ state: "planning", active_job: planningJob }));
  expect(job).not.toBeNull();
  expect(activeJobLabel(job!)).toBe(
    "planning role · luna (gpt-5.6-luna) · harness pi · thinking max · route codex",
  );
});

test("a job left over from another stage is never shown as running", () => {
  expect(requestRunningJob(request({ state: "plan_review", active_job: planningJob }))).toBeNull();
});

test("no active_job means nothing running", () => {
  expect(requestRunningJob(request({ state: "planning" }))).toBeNull();
});

test("the label drops empty parts and a model id equal to its name", () => {
  const job: ActiveJob = {
    stage: "spec_drafting",
    role: "",
    model: "qwen",
    modelId: "qwen",
    harness: "",
    thinking: "",
    route: "",
    startedAt: "",
  };
  expect(activeJobLabel(job)).toBe("qwen");
});

test("only a halted request with the accepted_no_pr kind awaits a PR", () => {
  const of = (state: string, haltKind: string) => request({ state, halt_kind: haltKind });
  expect(requestAwaitingPullRequest(of("halted", "accepted_no_pr"))).toBe(true);
  expect(requestAwaitingPullRequest(of("halted", ""))).toBe(false);
  expect(requestAwaitingPullRequest(of("halted", "oracle_materialize"))).toBe(false);
  expect(requestAwaitingPullRequest(of("building", "accepted_no_pr"))).toBe(false);
  expect(requestAwaitingPullRequestLabel(of("halted", "accepted_no_pr"))).toContain(
    "factoryd retry req-1",
  );
});

test("waitingOn/quarantineCheck are null when absent from the JSON", () => {
  const r = request({ state: "building" });
  expect(r.waitingOn).toBeNull();
  expect(r.quarantineCheck).toBeNull();
});

test("waitingOn/quarantineCheck parse verbatim when present", () => {
  const r = request({
    state: "building",
    waiting_on: "req-ahead-1",
    quarantine_check: "spec_conformity",
  });
  expect(r.waitingOn).toBe("req-ahead-1");
  expect(r.quarantineCheck).toBe("spec_conformity");
});

test("oracle_skip_warning parses tolerantly", () => {
  expect(request({ oracle_skip_warning: "warn" }).oracleSkipWarning).toBe("warn");
  expect(request({ oracle_skip_warning: 42 }).oracleSkipWarning).toBe("");
  expect(request({ oracle_skip_warning: null }).oracleSkipWarning).toBe("");
});

test("oracle_draft parses tolerantly: a non-object or wrong-typed field reads as absent", () => {
  expect(request({ oracle_draft: "drafted" }).oracleDraftStatus).toBe("");
  expect(request({ oracle_draft: { status: 3, detail: "d" } }).oracleDraftStatus).toBe("");
  expect(request({ oracle_draft: { status: 3, detail: "d" } }).oracleDraftDetail).toBe("d");
  expect(request({ oracle_draft: { criteria: "none" } }).oracleDraftCriteria).toEqual([]);
  const mixed = request({
    oracle_draft: { criteria: [7, { number: 2, eligible: true }, { reason: "r" }] },
  });
  expect(mixed.oracleDraftCriteria).toEqual([
    { number: 2, eligible: true, reason: "" },
    { number: 0, eligible: false, reason: "r" },
  ]);
  expect(request({ active_job: "running" }).activeJob).toBeNull();
});

test("a rejection's stage is its send-back stage, else the state it was rejected in", () => {
  const r = detail("request-plan-review");
  expect(rejectionStage(r.rejections[0]!)).toBe("planning");
  expect(
    rejectionStage({
      by: "a",
      at: "t",
      reason: "r",
      fromState: "halted",
      forStage: null,
      anchors: [],
      note: "",
    }),
  ).toBe("halted");
});

test("requestShortTitle strips markdown, falls back to the id and caps at 100", () => {
  expect(requestShortTitle(request({ title: "" }))).toBe("req-1");
  expect(requestShortTitle(request({ title: "# Fix **the** `parser` *now*" }))).toBe(
    "Fix the parser now",
  );
  expect(requestShortTitle(request({ title: "**" }))).toBe("**");
  const long = requestShortTitle(request({ title: "x".repeat(150) }));
  expect(long).toBe(`${"x".repeat(100)}…`);
});

test("waitingSince wins over enteredAt, which is its fallback", () => {
  expect(requestWaitingSinceOrEnteredAt(request({ waiting_since: "w", entered_at: "e" }))).toBe(
    "w",
  );
  expect(requestWaitingSinceOrEnteredAt(request({ entered_at: "e" }))).toBe("e");
});

test("a required field missing throws a DecodeError naming the route and the field", () => {
  const body = requestJson();
  delete body.workspace;
  expect(() => decodeRequestSummary(body, route)).toThrow(DecodeError);
  expect(() => decodeRequestSummary(body, route)).toThrow("GET /requests/{id}.workspace");
  expect(() =>
    decodeRequestList([requestJson({ rejections: [{ by: "a" }] })], "GET /requests"),
  ).toThrow("GET /requests[0].rejections[0].at");
});

test("GET /requests decodes every fixture row", () => {
  const list = decodeRequestList(readFixtureJson("api/requests.json"), "GET /requests");
  expect(list.map((r) => r.id)).toEqual([
    "req-building",
    "req-done",
    "req-every-field",
    "req-halted",
    "req-oracle-review",
    "req-plan-review",
    "req-quarantined",
    "req-spec-review",
  ]);
  const building = list[0]!;
  expect(building.state).toBe("building");
  expect(building.ticketIndex).toBe(2);
  expect(building.ticketCount).toBe(2);
  expect(building.tickets.map((t) => t.prState)).toEqual(["open", ""]);
  expect(building.costSummary?.tokens).toBe(479500);
  expect(building.activeJob?.stage).toBe("build");
  expect(building.spec).toBe("");
  expect(building.nextAction).toBe("");
  expect(list[1]!.tickets[0]!.prState).toBe("merged");
  expect(list[4]!.oracleDraftStatus).toBe("drafted");
});

test("request-spec-review decodes", () => {
  const r = detail("request-spec-review");
  expect(r.id).toBe("req-spec-review");
  expect(r.state).toBe("spec_review");
  expect(r.title).toBe("Add idempotency keys to checkout");
  expect(r.spec.startsWith("# Idempotency keys for checkout")).toBe(true);
  expect(r.specFullPath).toBe("/data/requests/req-spec-review/spec.md");
  expect(r.approveNextState).toBe("planning");
  expect(r.nextAction).toBe(
    "review the drafted spec: `factoryd approve req-spec-review`, or `factoryd reject -reason ... req-spec-review` to redraft",
  );
  expect(r.waitingSince).toBe("2026-09-10T09:05:00Z");
  expect(r.specEvidence?.usage?.fields.totalTokens).toBe(1200);
  expect(r.planEvidence).toBeNull();
  expect(r.rejections).toEqual([]);
  expect(r.history.map((h) => h.to)).toEqual(["spec_drafting", "spec_review"]);
  expect(r.history[0]!.reason).toBe("");
  expect(r.history[1]!.reason).toBe("spec drafted");
  expect(r.costSummary?.total).toBe(0.012);
});

test("request-oracle-review decodes", () => {
  const r = detail("request-oracle-review");
  expect(r.state).toBe("oracle_review");
  expect(r.draftOracles).toBe(true);
  expect(r.oracleDraftStatus).toBe("drafted");
  expect(r.oracleDraftDetail).toBe("one criterion is covered");
  expect(r.oracleDraftCriteria).toEqual([
    { number: 1, eligible: true, reason: "" },
    { number: 2, eligible: false, reason: "needs a second account" },
  ]);
  expect(r.waitingSince).toBe("2026-09-10T09:09:00Z");
});

test("request-plan-review decodes", () => {
  const r = detail("request-plan-review");
  expect(r.state).toBe("plan_review");
  expect(r.approvedBy).toBe("alice");
  expect(r.approvedAt).toBe("2026-09-10T09:06:00Z");
  expect(r.approveNextState).toBe("building");
  expect(r.ticketCount).toBe(2);
  expect(r.tickets.map((t) => t.fullPath)).toEqual([
    "/data/requests/req-plan-review/tickets/001.spec.md",
    "/data/requests/req-plan-review/tickets/002.spec.md",
  ]);
  expect(r.tickets[0]!.content.startsWith("Verify-Command: true")).toBe(true);
  expect(r.planEvidence?.usage?.fields.totalTokens).toBe(900);
  expect(r.rejections).toEqual([
    {
      by: "alice",
      at: "2026-09-10T09:08:00Z",
      reason: "split the migration out",
      fromState: "plan_review",
      forStage: "planning",
      anchors: [],
      note: "",
    },
  ]);
});

test("request-building decodes", () => {
  const r = detail("request-building");
  expect(r.state).toBe("building");
  expect(r.ticketIndex).toBe(2);
  expect(r.tickets[0]!.runId).toBe("run-accepted");
  expect(r.tickets[0]!.prUrl).toBe("https://github.com/acme/app/pull/7");
  expect(r.tickets[0]!.prState).toBe("open");
  expect(r.tickets[1]!.runId).toBe("run-running");
  expect(r.nextAction).toBe("");
  expect(r.approveNextState).toBe("");
  expect(r.activeJob).toEqual({
    stage: "build",
    role: "execution",
    model: "luna",
    modelId: "gpt-5.6-luna",
    harness: "pi",
    thinking: "high",
    route: "chatgpt-codex",
    startedAt: "2026-09-10T09:45:00Z",
  });
  // The fixture's stage "build" is not the state "building" (Go records
  // only drafting stages), so the stale-job rule hides it.
  expect(requestRunningJob(r)).toBeNull();
  expect(r.costSummary?.acceptedTickets).toBe(1);
  expect(r.costSummary?.costPerAcceptedTicketMicroUsd).toBe(1512000);
});

test("request-done decodes", () => {
  const r = detail("request-done");
  expect(r.state).toBe("done");
  expect(r.tickets).toHaveLength(1);
  expect(r.tickets[0]!.prState).toBe("merged");
  expect(r.tickets[0]!.runId).toBe("run-every-field");
  expect(r.costSummary?.byModel[0]?.model).toBe("every-field relay_worker_model_id");
  expect(r.activeJob).toBeNull();
});

test("request-every-field decodes with every optional field populated", () => {
  const r = detail("request-every-field");
  expect(r.state).toBe("resume_review");
  expect(r.error).toBe("every-field error");
  expect(r.approvedBy).toBe("every-field approved_by");
  expect(r.approvedAt).toBe("2026-09-10T09:00:00Z");
  expect(r.enteredAt).toBe("2026-09-10T09:00:00Z");
  expect(r.waitingSince).toBe("2026-09-10T09:00:00Z");
  expect(r.title).toBe("Add idempotency keys to checkout");
  expect(r.ticketIndex).toBe(1);
  expect(r.ticketCount).toBe(1);
  expect(r.spec).not.toBe("");
  expect(r.specFullPath).toBe("/data/requests/req-every-field/spec.md");
  expect(r.nextAction).toContain("factoryd resume -from scratch req-every-field");
  expect(r.draftOracles).toBe(true);
  expect(r.quarantineCheck).toBe("every-field quarantine_check");
  expect(r.oracleSkipWarning).toBe("every-field oracle_skip_warning");
  expect(r.oracleDraftStatus).toBe("drafted");
  expect(r.oracleDraftDetail).toBe("every-field detail");
  expect(r.oracleDraftCriteria).toEqual([
    { number: 3, eligible: true, reason: "every-field reason" },
  ]);
  expect(r.resume).toEqual({
    fromState: "building",
    lostRunId: "every-field lost_run_id",
    generation: 3,
    refused: ["every-field refused"],
  });
  expect(r.specEvidence?.usage?.fields.totalTokens).toBe(150);
  expect(r.planEvidence?.usage?.fields.totalTokens).toBe(150);
  expect(r.rejections).toEqual([
    {
      by: "every-field by",
      at: "2026-09-10T09:00:00Z",
      reason: "every-field reason",
      fromState: "spec_review",
      forStage: "spec_drafting",
      anchors: [
        {
          path: "every-field path",
          section: "every-field section",
          item: 3,
          note: "every-field note",
        },
      ],
      note: "every-field note",
    },
  ]);
  expect(r.history).toEqual([
    {
      from: "building",
      to: "resume_review",
      at: "2026-09-10T09:50:00Z",
      by: "factoryd",
      reason: "the build was lost",
    },
  ]);
  expect(r.tickets).toEqual([
    {
      index: 1,
      specPath: "/data/requests/req-every-field/tickets/001.spec.md",
      runId: "run-quarantined",
      prUrl: "every-field pr_url",
      prState: "every-field pr_state",
      content: expect.stringContaining("Verify-Command: true") as string,
      fullPath: "/data/requests/req-every-field/tickets/001.spec.md",
      // The fixture's one round carries a kind (a conformity round), so it is
      // not a review round.
      reviewRounds: [],
      activeRoundRunId: "",
    },
  ]);
  expect(r.costSummary?.currency).toBe("usd");
  expect(r.costSummary?.byModel[0]?.role).toBe("every-field role");
});

test("request-every-field leaves only fields the API sends in other states unset", () => {
  // The fixture is one request in resume_review: a running job, a queue
  // position, a halt kind and the approve/send-back flags belong to other
  // states, so these decode to their fallbacks here.
  const r = detail("request-every-field");
  expect(r.activeJob).toBeNull();
  expect(r.waitingOn).toBeNull();
  expect(r.haltKind).toBe("");
  expect(r.approveNextState).toBe("");
  expect(r.canSendBack).toBe(false);
  expect(r.canSendBackToPlan).toBe(false);
});

test("request-quarantined decodes with its send-back fields and quarantine check", () => {
  const r = detail("request-quarantined");
  expect(r.id).toBe("req-quarantined");
  expect(r.state).toBe("quarantined");
  expect(r.canSendBack).toBe(true);
  expect(r.canSendBackToPlan).toBe(true);
  expect(r.quarantineCheck).toBe("verify");
  expect(r.haltKind).toBe("");
  expect(r.tickets).toHaveLength(1);
  expect(r.tickets[0]!.runId).toBe("run-quarantined");
});

test("request-halted decodes with its halt kind and no send-back", () => {
  const r = detail("request-halted");
  expect(r.id).toBe("req-halted");
  expect(r.state).toBe("halted");
  expect(r.haltKind).toBe("accepted_no_pr");
  expect(r.canSendBack).toBe(false);
  expect(r.canSendBackToPlan).toBe(false);
  expect(r.quarantineCheck).toBeNull();
  expect(r.tickets).toHaveLength(1);
  expect(r.tickets[0]!.runId).toBe("run-accepted");
});

test("request-events.sse frames decode as requests", () => {
  const frames = readFixtureText("api/request-events.sse")
    .split("\n")
    .filter((line) => line.startsWith("data: "))
    .map((line) => JSON.parse(line.slice("data: ".length)) as unknown);
  expect(frames).toHaveLength(8);
  const decoded = frames.map((frame, i) =>
    decodeRequestSummary(asObject(frame, "event"), `event ${i}`),
  );
  expect(decoded.map((r) => r.id)).toEqual([
    "req-building",
    "req-done",
    "req-every-field",
    "req-halted",
    "req-oracle-review",
    "req-plan-review",
    "req-quarantined",
    "req-spec-review",
  ]);
  expect(decoded[0]!.activeJob?.stage).toBe("build");
});

test("parseRequestStateEvent decodes a raw state event", () => {
  const data = JSON.stringify(requestJson({ id: "req-1", state: "plan_review" }));
  const frame = `event: state\ndata: ${data}\n\n`;
  const line = frame.split("\n").find((l) => l.startsWith("data: "))!;
  const r = decodeRequestSummary(asObject(JSON.parse(line.slice(6)), "event"), "event");
  expect(r.id).toBe("req-1");
  expect(r.state).toBe("plan_review");
});

test("listRevisions/getRevision parse the revision routes", () => {
  const revisions = decodeRevisionList(
    readFixtureJson("api/request-revisions.json"),
    "GET /requests/{id}/revisions",
  );
  expect(revisions).toHaveLength(1);
  expect(revisions[0]).toEqual({
    index: 1,
    at: "2026-09-10T09:08:00Z",
    by: "alice",
    reason: "split the migration out",
    fromState: "plan_review",
    files: ["spec.md", "tickets/001.spec.md", "tickets/002.spec.md"],
  });
  const route2 = "GET /requests/{id}/revisions/{n}";
  const detailJson = asObject(readFixtureJson("api/request-revision.json"), route2);
  const d = decodeRevisionDetail(detailJson, route2);
  expect(d.index).toBe(1);
  expect(d.reason).toBe("split the migration out");
  expect(Object.keys(d.files)).toEqual(["spec.md", "tickets/001.spec.md", "tickets/002.spec.md"]);
  expect(d.files["spec.md"]!.startsWith("# Idempotency keys for checkout")).toBe(true);

  const small = decodeRevisionDetail(
    {
      index: 1,
      at: "a",
      by: "jane",
      reason: "too broad",
      from_state: "spec_review",
      files: { "spec.md": "# Old spec" },
    },
    route2,
  );
  expect(small.files["spec.md"]).toBe("# Old spec");
  expect(
    decodeRevisionList(
      [{ index: 1, at: "a", by: "b", reason: "r", from_state: "s", files: null }],
      "x",
    )[0]!.files,
  ).toEqual([]);
});

test("a rejection's anchored notes are decoded; a plain rejection has none", () => {
  const withAnchors = request({
    rejections: [
      {
        by: "a",
        at: "t",
        reason: "- spec.md, ## Scope: too wide\n\nOtherwise fine.",
        from_state: "spec_review",
        note: "Otherwise fine.",
        anchors: [
          { path: "spec.md", section: "## Scope", note: "too wide" },
          { path: "spec.md", section: "## Acceptance criteria", item: 2, note: "which account?" },
        ],
      },
      { by: "a", at: "t", reason: "plain", from_state: "spec_review" },
    ],
  });
  expect(withAnchors.rejections[0]?.anchors).toEqual([
    { path: "spec.md", section: "## Scope", item: 0, note: "too wide" },
    { path: "spec.md", section: "## Acceptance criteria", item: 2, note: "which account?" },
  ]);
  expect(withAnchors.rejections[0]?.note).toBe("Otherwise fine.");
  expect(withAnchors.rejections[1]).toMatchObject({ anchors: [], note: "" });
});

test("a ticket's PR-review rounds are decoded; conformity rounds are left out", () => {
  const r = request({
    state: "pr_review",
    tickets: [
      {
        index: 1,
        active_round_run_id: "req-1-001-review3-x",
        rounds: [
          { index: 1, kind: "spec_conformity", run_id: "c1", outcome: "accepted", at: "t0" },
          {
            index: 1,
            thread_ids: ["T1"],
            run_id: "r1",
            outcome: "quarantined",
            at: "2026-10-06T07:26:19Z",
            error: "policy gate did not pass: code_review",
          },
          { index: 2, run_id: "r2", outcome: "accepted", at: "2026-10-06T07:40:00Z", pushed: true },
        ],
      },
      { index: 2 },
    ],
  });
  expect(r.tickets[0]?.reviewRounds).toEqual([
    {
      index: 1,
      runId: "r1",
      outcome: "quarantined",
      at: "2026-10-06T07:26:19Z",
      error: "policy gate did not pass: code_review",
      pushed: false,
    },
    {
      index: 2,
      runId: "r2",
      outcome: "accepted",
      at: "2026-10-06T07:40:00Z",
      error: "",
      pushed: true,
    },
  ]);
  expect(r.tickets[0]?.activeRoundRunId).toBe("req-1-001-review3-x");
  expect(r.tickets[1]).toMatchObject({ reviewRounds: [], activeRoundRunId: "" });
});
