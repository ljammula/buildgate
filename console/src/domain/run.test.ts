import { DecodeError, asObject } from "@/domain/decode";
import {
  type Run,
  agentEvidenceRoundOutcome,
  composeServiceAddress,
  decodeAgentEvidenceRound,
  decodeComposePhase,
  decodeProgressEvent,
  decodeRun,
  decodeRunDiff,
  decodeRunList,
  runIsTerminal,
  runIsTerminalForDisplay,
} from "@/domain/run";
import { readFixtureJson, readFixtureText } from "@/test/fixtures";

function sseData(file: string): unknown[] {
  return readFixtureText(file)
    .split("\n")
    .filter((line) => line.startsWith("data: "))
    .map((line) => JSON.parse(line.slice("data: ".length)) as unknown);
}

function minimalRun(extra: Record<string, unknown> = {}): Record<string, unknown> {
  return {
    id: "run-1",
    ticket: "ticket-1",
    project_path: "/projects/app",
    workspace_path: "/workspaces/run-1",
    spec_path: "/specs/ticket-1.md",
    spec_sha256: "spec-1",
    state: "accepted",
    base_sha: "base-1",
    committed_by_factoryd: false,
    attempts: [],
    gate_results: [],
    notifications: [],
    overrides: [],
    created_at: "2026-09-18T11:00:00Z",
    updated_at: "2026-09-18T11:04:00Z",
    ...extra,
  };
}

const run = (extra: Record<string, unknown> = {}): Run =>
  decodeRun(minimalRun(extra), "GET /runs/{id}");

describe("fixtures", () => {
  test("api/runs.json decodes", () => {
    const runs = decodeRunList(readFixtureJson("api/runs.json"), "GET /runs");
    expect(runs.map((r) => r.id)).toEqual([
      "run-running",
      "run-quarantined",
      "run-accepted",
      "run-every-field",
    ]);
    expect(runs[0]!.state).toBe("slice_running");
    expect(runs[0]!.currentStage).toBe("build");
    expect(runs[0]!.changedFiles).toBeNull();
    expect(runs[2]!.diffStat).toEqual({ filesChanged: 2, insertions: 48, deletions: 3 });
  });

  test("api/run-accepted.json decodes", () => {
    const r = decodeRun(asObject(readFixtureJson("api/run-accepted.json"), "f"), "GET /runs/{id}");
    expect(r.state).toBe("accepted");
    expect(r.haltConfirmed).toBe(true);
    expect(r.resultSha).toBe("2222222222222222222222222222222222222222");
    expect(r.changedFiles).toEqual(["checkout/idempotency.go", "checkout/idempotency_test.go"]);
    expect(r.attempts[0]!.command).toEqual(["build_app.py", "--harness", "pi"]);
    expect(r.attempts[0]!.logPath).toBe("/data/runs/run-accepted/001-build_app.log");
    expect(r.gateResults).toHaveLength(2);
    expect(r.gateResults[0]!.durationMs).toBe(4200);
    expect(r.byModel).toEqual([
      { role: "execution", model: "gpt-5.6-luna", tokens: 478300, costMicroUsd: 1500000 },
    ]);
    expect(r.tokensComplete).toBe(true);
    expect(runIsTerminal(r)).toBe(true);
  });

  test("api/run-quarantined.json decodes", () => {
    const r = decodeRun(
      asObject(readFixtureJson("api/run-quarantined.json"), "f"),
      "GET /runs/{id}",
    );
    expect(r.state).toBe("quarantined");
    expect(r.resultSha).toBeNull();
    expect(r.changedFiles).toBeNull();
    expect(r.attempts).toEqual([]);
    expect(r.requestId).toBe("req-building");
    expect(r.tokensComplete).toBe(false);
    expect(runIsTerminal(r)).toBe(false);
    expect(runIsTerminalForDisplay(r)).toBe(true);
  });

  test("api/run-running.json decodes", () => {
    const r = decodeRun(asObject(readFixtureJson("api/run-running.json"), "f"), "GET /runs/{id}");
    expect(r.state).toBe("slice_running");
    expect(r.lastProgressAt).toBe("2026-09-10T09:46:00.000Z");
    expect(r.currentStage).toBe("build");
    expect(r.stalled).toBe(true);
    expect(r.stalledSinceSeconds).toBe(86400);
    expect(r.currentRound).toBe(0);
    expect(r.maxRounds).toBe(0);
    expect(r.waitingReason).toBeNull();
    expect(runIsTerminal(r)).toBe(false);
  });

  test("api/run-every-field.json populates every optional field on the wire", () => {
    const r = decodeRun(
      asObject(readFixtureJson("api/run-every-field.json"), "f"),
      "GET /runs/{id}",
    );
    expect(r.haltConfirmed).toBe(true);
    expect(r.resultSha).toBe("every-field result_sha");
    expect(r.changedFiles).toEqual(["every-field changed_files"]);
    expect(r.diffStat).toEqual({ filesChanged: 3, insertions: 3, deletions: 3 });
    expect(r.diffAvailable).toBe(true);
    expect(r.diffTruncated).toBe(true);
    expect(r.requestId).toBe("req-done");
    expect(r.temporalWorkflowId).toBe("every-field temporal_workflow_id");
    expect(r.referenceOracleDir).toBe("every-field reference_oracle_dir");
    expect(r.tokensComplete).toBe(false);
    expect(r.byModel).toEqual([
      {
        role: "every-field role",
        model: "every-field relay_worker_model_id",
        tokens: 6,
        costMicroUsd: 3,
      },
    ]);

    const a = r.attempts[0]!;
    expect(a.kind).toBe("build");
    expect(a.command).toEqual(["every-field command"]);
    expect(a.exitCode).toBe(3);
    expect(a.role).toBe("every-field role");
    expect(a.harness).toBe("every-field harness");
    expect(a.thinking).toBe("every-field thinking");
    expect(a.expectedEffort).toBe("every-field expected_effort");
    expect(a.relayWorkerModelId).toBe("every-field relay_worker_model_id");
    expect(a.relayReasoningEffort).toBe("every-field relay_reasoning_effort");
    expect(a.relayReasoningEffortAnomaly).toBe(true);

    const g = r.gateResults[0]!;
    expect(g).toEqual({
      check: "every-field check",
      command: ["every-field command"],
      passed: true,
      exitCode: 3,
      durationMs: 3,
      logSha256: "every-field log_sha256",
      baseCheck: {
        outcome: "every-field outcome",
        baseSha: "every-field base_sha",
        reason: "every-field reason",
      },
    });
    expect(r.notifications[0]).toEqual({
      runId: "every-field run_id",
      ticket: "every-field ticket",
      reason: "every-field reason",
      state: "accepted",
      sentAt: "2026-09-10T09:00:00Z",
    });
    expect(r.overrides[0]).toEqual({
      by: "every-field by",
      reason: "every-field reason",
      at: "2026-09-10T09:00:00Z",
      priorState: "quarantined",
      newState: "accepted",
    });

    const round = r.agentEvidence!.rounds[0]!;
    expect(round).toEqual({
      index: 3,
      agentReturnCode: 3,
      agentTimedOut: true,
      verifyPassed: true,
      verifyTimedOut: true,
      fastCheckRan: true,
      fastCheckPassed: true,
      durationS: 1.5,
      tokens: 150,
      blockers: ["every-field blockers"],
      changedFiles: ["every-field changed_files"],
      failureSignature: "every-field failure_signature",
      failureLog: "every-field failure_log",
      agentNotes: "every-field agent_notes",
    });
  });

  test("api/run-diff.json decodes", () => {
    const d = decodeRunDiff(
      asObject(readFixtureJson("api/run-diff.json"), "f"),
      "GET /runs/{id}/diff",
    );
    expect(d.truncated).toBe(false);
    expect(
      d.diff.startsWith("diff --git a/checkout/idempotency.go b/checkout/idempotency.go\n"),
    ).toBe(true);
  });

  test("api/run-events.sse lines decode as runs", () => {
    const lines = sseData("api/run-events.sse");
    expect(lines).toHaveLength(1);
    const r = decodeRun(asObject(lines[0], "f"), "GET /runs/{id}/events");
    expect(r.id).toBe("run-accepted");
    expect(r.state).toBe("accepted");
  });

  test("api/run-progress.sse lines decode as progress events", () => {
    const lines = sseData("api/run-progress.sse");
    expect(lines).toHaveLength(3);
    const events = lines.map((l) =>
      decodeProgressEvent(asObject(l, "f"), "GET /runs/{id}/progress"),
    );
    expect(events[0]).toEqual({
      ts: new Date("2026-09-10T09:10:00Z"),
      source: "factory",
      stage: "build",
      event: "start",
      round: 1,
      maxRounds: 3,
      outcome: "",
      detail: "",
    });
    expect(events[1]!.source).toBe("agent");
    expect(events[1]!.detail).toBe("478.3k tokens");
    expect(events[2]!.outcome).toBe("pass");
  });
});

test("a required field missing throws a DecodeError naming the route and the field", () => {
  const body = minimalRun();
  delete body.spec_sha256;
  expect(() => decodeRunList([body], "GET /runs")).toThrow(DecodeError);
  expect(() => decodeRunList([body], "GET /runs")).toThrow(/GET \/runs\[0\]\.spec_sha256/);
  expect(() => decodeRun(minimalRun({ attempts: [{ kind: "build" }] }), "GET /runs/{id}")).toThrow(
    /GET \/runs\/\{id\}\.attempts\[0\]\.started_at/,
  );
});

describe("ProgressEvent.fromJson", () => {
  const decode = (o: Record<string, unknown>) => decodeProgressEvent(o, "progress");

  test("parses a full factory-stage line (progress-contract.md)", () => {
    const event = decode({
      ts: "2026-09-17T10:00:00.123Z",
      source: "factory",
      stage: "build",
      event: "start",
      round: 0,
      max_rounds: 0,
      outcome: "",
      detail: "",
    });
    expect(event.ts).toEqual(new Date("2026-09-17T10:00:00.123Z"));
    expect(event.source).toBe("factory");
    expect(event.stage).toBe("build");
    expect(event.event).toBe("start");
    expect(event.round).toBe(0);
    expect(event.maxRounds).toBe(0);
    expect(event.outcome).toBe("");
    expect(event.detail).toBe("");
  });

  test("parses a worker round line", () => {
    const event = decode({
      ts: "2026-09-17T10:01:00.000Z",
      source: "worker",
      stage: "round",
      event: "end",
      round: 2,
      max_rounds: 6,
      outcome: "fail",
      detail: "verify failed: go test ./...",
    });
    expect(event.source).toBe("worker");
    expect(event.round).toBe(2);
    expect(event.maxRounds).toBe(6);
    expect(event.outcome).toBe("fail");
    expect(event.detail).toBe("verify failed: go test ./...");
  });

  // Every int/string field must tolerate being missing entirely:
  // progress-contract.md's own "omitempty is fine in Go, readers must treat
  // missing as 0/''" rather than throwing on a genuinely minimal line.
  test("tolerates missing round/max_rounds/outcome/detail", () => {
    const event = decode({
      ts: "2026-09-17T10:00:00.000Z",
      source: "factory",
      stage: "preflight",
      event: "start",
    });
    expect(event.round).toBe(0);
    expect(event.maxRounds).toBe(0);
    expect(event.outcome).toBe("");
    expect(event.detail).toBe("");
  });

  // An unparseable/missing ts must not throw: a malformed line from a future
  // server build must never crash the screen.
  test("tolerates a missing or malformed ts", () => {
    const event = decode({ source: "factory", stage: "preflight", event: "start" });
    expect(event.ts).toEqual(new Date(0));
    expect(decode({ ts: "not a time" }).ts).toEqual(new Date(0));
  });

  test("tolerates missing source/stage/event", () => {
    const event = decode({ ts: "2026-09-17T10:00:00.000Z" });
    expect(event.source).toBe("");
    expect(event.stage).toBe("");
    expect(event.event).toBe("");
  });
});

test("a halted run is terminal only once halt_confirmed is true", () => {
  const unconfirmed = run({ state: "halted", changed_files: null });
  expect(unconfirmed.haltConfirmed).toBe(false);
  expect(runIsTerminal(unconfirmed)).toBe(false);

  const confirmed = run({ state: "halted", halt_confirmed: true });
  expect(confirmed.haltConfirmed).toBe(true);
  expect(runIsTerminal(confirmed)).toBe(true);
});

describe("Run.fromJson progress fields", () => {
  test("parses all seven when present", () => {
    const r = run({
      state: "slice_running",
      last_progress_at: "2026-09-18T11:05:00.000Z",
      current_stage: "verify",
      current_round: 2,
      max_rounds: 6,
      waiting_reason: "behind 1 run(s) on foo/bar",
      stalled: true,
      stalled_since_seconds: 360,
    });
    expect(r.lastProgressAt).toBe("2026-09-18T11:05:00.000Z");
    expect(r.currentStage).toBe("verify");
    expect(r.currentRound).toBe(2);
    expect(r.maxRounds).toBe(6);
    expect(r.waitingReason).toBe("behind 1 run(s) on foo/bar");
    expect(r.stalled).toBe(true);
    expect(r.stalledSinceSeconds).toBe(360);
  });

  test("tolerates all seven absent (a terminal run, or an old server)", () => {
    const r = run();
    expect(r.lastProgressAt).toBeNull();
    expect(r.currentStage).toBeNull();
    expect(r.currentRound).toBe(0);
    expect(r.maxRounds).toBe(0);
    expect(r.waitingReason).toBeNull();
    expect(r.stalled).toBe(false);
    expect(r.stalledSinceSeconds).toBeNull();
  });

  test("parses reference_oracle_dir when present", () => {
    expect(run({ reference_oracle_dir: "/data/requests/req-1/oracle" }).referenceOracleDir).toBe(
      "/data/requests/req-1/oracle",
    );
  });

  test("reference_oracle_dir defaults to empty when absent", () => {
    expect(run().referenceOracleDir).toBe("");
  });
});

describe("derived values", () => {
  test("quarantined is not terminal but is terminal for display", () => {
    const r = run({ state: "quarantined" });
    expect(runIsTerminal(r)).toBe(false);
    expect(runIsTerminalForDisplay(r)).toBe(true);
  });

  test("a compose service address omits a zero port", () => {
    const phase = decodeComposePhase(
      {
        phase: "build",
        enabled: true,
        services: [
          { name: "db", alias: "postgres", image: "postgres:16", port: 5432 },
          { name: "x", alias: "worker-x", image: "x" },
        ],
      },
      "p",
    );
    expect(phase.services.map(composeServiceAddress)).toEqual(["postgres:5432", "worker-x"]);
    expect(phase.disabledReason).toBe("");
  });

  test("a round outcome follows round_blockers' priority order", () => {
    const base = { index: 1, agent_returncode: 0, verify_passed: true };
    const outcome = (o: Record<string, unknown>) =>
      agentEvidenceRoundOutcome(decodeAgentEvidenceRound({ ...base, ...o }, "r"));
    expect(outcome({})).toBe("pass");
    expect(outcome({ verify_passed: false })).toBe("fail (verify)");
    expect(outcome({ verify_timed_out: true })).toBe("fail (timed out)");
    expect(outcome({ agent_timed_out: true })).toBe("fail (timed out)");
    expect(outcome({ agent_returncode: 2 })).toBe("fail (error)");
    expect(outcome({ fast_check_ran: true, fast_check_passed: false })).toBe("fail (verify)");
    expect(outcome({ verify_passed: null })).toBe("fail (error)");
  });

  test("reported blockers add a failure and never remove one", () => {
    const base = { index: 1, agent_returncode: 0, verify_passed: true };
    const outcome = (o: Record<string, unknown>) =>
      agentEvidenceRoundOutcome(decodeAgentEvidenceRound({ ...base, ...o }, "r"));
    expect(outcome({ blockers: [] })).toBe("pass");
    expect(outcome({ blockers: ["no changes made to the workspace"] })).toBe("fail (blocked)");
    expect(outcome({ blockers: ["canonical verification failed"], verify_passed: false })).toBe(
      "fail (verify)",
    );
    expect(outcome({ blockers: null, verify_passed: false })).toBe("fail (verify)");
    expect(outcome({ blockers: [], verify_passed: false })).toBe("fail (verify)");
    expect(outcome({ blockers: [], agent_timed_out: true })).toBe("fail (timed out)");
  });

  test("a round's feedback fields keep never-reported apart from empty", () => {
    const old = decodeAgentEvidenceRound({ index: 1, blockers: null }, "r");
    expect([old.blockers, old.changedFiles]).toEqual([null, null]);
    expect([old.failureSignature, old.failureLog, old.agentNotes]).toEqual(["", "", ""]);
    const passed = decodeAgentEvidenceRound({ index: 1, blockers: [], changed_files: [] }, "r");
    expect([passed.blockers, passed.changedFiles]).toEqual([[], []]);
  });

  test("a round's tokens use the shared usage total, 0 without a usable figure", () => {
    const tokens = (usage: unknown) => decodeAgentEvidenceRound({ usage }, "r").tokens;
    expect(tokens({ input: 20000, output: 1200 })).toBe(21200);
    expect(tokens({ note: "none" })).toBe(0);
    expect(tokens(null)).toBe(0);
  });
});
