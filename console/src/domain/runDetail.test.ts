import {
  attemptExitText,
  attemptModelLine,
  computeTimeline,
  glyphFor,
  overallGlyph,
  roundDetailLines,
} from "@/domain/runDetail";
import { type ProgressEvent, type Run, decodeAgentEvidenceRound, decodeRun } from "@/domain/run";

function exit(kind: string, exitCode: number): string {
  return attemptExitText({ kind, exitCode });
}

describe("attemptExitText", () => {
  test("decodes a combined review launch exit code", () => {
    expect(exit("review", 40)).toBe("Exit code: 40 (spec conformity passed, code review passed)");
    expect(exit("review", 41)).toBe("Exit code: 41 (spec conformity failed, code review passed)");
    expect(exit("review", 43)).toBe("Exit code: 43 (spec conformity failed, code review failed)");
  });

  test("leaves other exit codes alone", () => {
    expect(exit("review", 1)).toBe("Exit code: 1");
    expect(exit("build", 40)).toBe("Exit code: 40");
  });
});

const noModel = {
  role: "",
  harness: "",
  thinking: "",
  relayWorkerModelId: "",
  expectedEffort: "",
  relayReasoningEffort: "",
  relayReasoningEffortAnomaly: false,
};

describe("attemptModelLine", () => {
  test("is null when the attempt recorded no role, harness, model or thinking", () => {
    expect(attemptModelLine(noModel)).toBeNull();
  });

  test("joins the recorded parts and flags a clamp only against the expected effort", () => {
    const base = {
      ...noModel,
      role: "execution",
      harness: "pi",
      relayWorkerModelId: "gpt-5.6-luna",
      thinking: "max",
    };
    expect(
      attemptModelLine({ ...base, expectedEffort: "xhigh", relayReasoningEffort: "xhigh" }),
    ).toBe("Role: execution · Harness: pi · Model: gpt-5.6-luna · Thinking: max");
    expect(
      attemptModelLine({
        ...base,
        expectedEffort: "xhigh",
        relayReasoningEffort: "high",
        relayReasoningEffortAnomaly: true,
      }),
    ).toBe(
      "Role: execution · Harness: pi · Model: gpt-5.6-luna · Thinking: max (sent: high) (anomaly)",
    );
  });
});

describe("glyphFor and overallGlyph", () => {
  const t = new Date("2026-09-29T14:00:00Z");

  test("a finished stage reads passed unless it failed", () => {
    expect(glyphFor({ start: t, end: t, outcome: "pass", finished: false })).toBe("passed");
    expect(glyphFor({ start: t, end: t, outcome: "accepted", finished: true })).toBe("passed");
    expect(glyphFor({ start: t, end: t, outcome: "quarantined", finished: true })).toBe("failed");
    expect(glyphFor({ start: t, end: t, outcome: "fail", finished: false })).toBe("failed");
  });

  test("an unreached stage is pending, or skipped once the run finished", () => {
    expect(glyphFor({ start: t, end: null, outcome: "", finished: false })).toBe("running");
    expect(glyphFor({ start: null, end: null, outcome: "", finished: false })).toBe("pending");
    expect(glyphFor({ start: null, end: null, outcome: "", finished: true })).toBe("skipped");
  });

  test("overallGlyph: failed beats running beats passed", () => {
    expect(overallGlyph([], false)).toBe("pending");
    expect(overallGlyph([], true)).toBe("skipped");
    expect(overallGlyph(["passed", "failed", "running"], false)).toBe("failed");
    expect(overallGlyph(["passed", "running"], false)).toBe("running");
    expect(overallGlyph(["passed", "passed"], false)).toBe("passed");
    expect(overallGlyph(["passed", "pending"], false)).toBe("pending");
  });
});

describe("computeTimeline", () => {
  const run: Run = decodeRun(
    {
      id: "run-1",
      ticket: "ticket-1",
      project_path: "/p",
      workspace_path: "/w",
      spec_path: "/s",
      spec_sha256: "x",
      state: "building",
      base_sha: "b",
      committed_by_factoryd: false,
      attempts: [],
      gate_results: [
        {
          check: "diff_scope",
          command: [],
          passed: true,
          exit_code: 0,
          duration_ms: 1,
          log_sha256: "x",
        },
      ],
      notifications: [],
      overrides: [],
      created_at: "2026-09-29T14:00:00Z",
      updated_at: "2026-09-29T14:00:00Z",
    },
    "GET /runs/{id}",
  );

  function ev(
    source: string,
    stage: string,
    event: string,
    sec: number,
    extra: Partial<ProgressEvent> = {},
  ): ProgressEvent {
    return {
      ts: new Date(Date.UTC(2026, 8, 29, 14, 0, sec)),
      source,
      stage,
      event,
      round: 0,
      maxRounds: 0,
      outcome: "",
      detail: "",
      ...extra,
    };
  }

  test("omits the oracle stages for a run with no reference oracle dir, and falls back to gate results", () => {
    const rows = computeTimeline([], new Date("2026-09-29T14:00:10Z"), run);
    const keys = rows.map((r) => r.rowKey);
    expect(keys).not.toContain("commit_oracles");
    expect(keys).not.toContain("post_oracle_commit_verify");
    expect(keys).toContain("finished");
    const gate = rows.find((r) => r.rowKey === "gate");
    expect(gate?.glyph).toBe("passed");
    expect(gate?.subtitle).toBe("1 passed: diff_scope");
  });

  test("a running build shows its worker round and the newest agent notes", () => {
    const events = [
      ev("factory", "build", "start", 0),
      ev("worker", "round", "start", 1, { round: 1, maxRounds: 3 }),
      ev("worker", "agent", "note", 2, { round: 1, detail: "read: main.go" }),
    ];
    const rows = computeTimeline(events, new Date("2026-09-29T14:00:05Z"), run);
    const build = rows.find((r) => r.rowKey === "build");
    expect(build?.glyph).toBe("running");
    expect(build?.durationText).toBe("00:05");
    expect(build?.subRows.map((r) => [r.label, r.glyph, r.durationText])).toEqual([
      ["Round 1 of 3", "running", "00:04"],
    ]);
    expect(build?.noteLines).toEqual(["read: main.go"]);
  });
});

describe("roundDetailLines", () => {
  const round = (o: Record<string, unknown>) =>
    decodeAgentEvidenceRound({ index: 2, verify_passed: false, ...o }, "r");

  test("a round recorded without the feedback fields has no lines", () => {
    expect(roundDetailLines(round({}), undefined)).toEqual([]);
  });

  test("names the blockers, the files, a repeated failure and the saved output", () => {
    const first = round({ index: 1, failure_signature: "5f2c9d0a71e4b386" });
    const second = round({
      blockers: ["no changes made to the workspace", "canonical verification failed"],
      changed_files: [],
      failure_signature: "5f2c9d0a71e4b386",
      failure_log: ".pi-build-session/feedback/verify.log",
    });
    expect(roundDetailLines(second, first)).toEqual([
      "Blocked by: no changes made to the workspace; canonical verification failed",
      "Changed no files",
      "The same failure as round 1",
      "Full output saved in the build workspace: .pi-build-session/feedback/verify.log",
    ]);
  });

  test("a different or missing signature is not a repeat, and a passed round names its files", () => {
    const first = round({ index: 1, failure_signature: "aaaa" });
    expect(roundDetailLines(round({ failure_signature: "bbbb" }), first)).toEqual([]);
    expect(roundDetailLines(round({ failure_signature: "" }), round({ index: 1 }))).toEqual([]);
    expect(roundDetailLines(round({ failure_signature: "aaaa" }), undefined)).toEqual([]);
    const files = Array.from({ length: 11 }, (_, i) => `f${i}.go`);
    expect(roundDetailLines(round({ blockers: [], changed_files: files }), first)).toEqual([
      "Changed: f0.go, f1.go, f2.go, f3.go, f4.go, f5.go, f6.go, f7.go and 3 more",
    ]);
  });
});
