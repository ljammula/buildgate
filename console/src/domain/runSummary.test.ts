import { asObject } from "@/domain/decode";
import { type Run, decodeRun } from "@/domain/run";
import {
  attemptFailed,
  attemptsSummary,
  baselineVerifyFailed,
  baselineVerifySummary,
  composeSummary,
  formatGateDuration,
  gatesSummary,
  runTokensText,
  runVerdictLine,
} from "@/domain/runSummary";

function run(extra: Record<string, unknown> = {}): Run {
  return decodeRun(
    asObject(
      {
        id: "run-1",
        ticket: "ticket-1",
        project_path: "/projects/app",
        workspace_path: "/workspaces/run-1",
        spec_path: "/specs/ticket-1.md",
        spec_sha256: "spec-1",
        state: "accepted",
        base_sha: "base-1",
        committed_by_factoryd: true,
        attempts: [],
        gate_results: [],
        notifications: [],
        overrides: [],
        created_at: "2026-09-18T11:00:00Z",
        updated_at: "2026-09-18T11:20:00Z",
        ...extra,
      },
      "test",
    ),
    "test",
  );
}

const gate = (check: string, passed: boolean) => ({
  check,
  command: ["make", check],
  passed,
  exit_code: passed ? 0 : 2,
  duration_ms: 900,
  log_sha256: "abc",
});

describe("runVerdictLine", () => {
  test("an accepted run reads state, rounds, gates, tokens and duration", () => {
    const line = runVerdictLine(
      run({
        gate_results: [gate("verify", true), gate("lint", true)],
        agent_evidence: { rounds: [{ index: 1, agent_returncode: 0, verify_passed: true }] },
        by_model: [{ model: "gpt-5.6-luna", tokens: 478300 }],
      }),
    );
    expect(line).toBe("Accepted · 1 round · 2 gates passed · 478.3k tokens · 20:00");
  });

  test("a failed gate is counted as such", () => {
    const line = runVerdictLine(
      run({ state: "halted", gate_results: [gate("verify", true), gate("lint", false)] }),
    );
    expect(line).toBe("Halted · 1 of 2 gates failed · 20:00");
  });

  test("parts with nothing behind them are left out, not printed as none", () => {
    expect(runVerdictLine(run())).toBe("Accepted · 20:00");
  });

  test("a run over an hour reads hours and minutes", () => {
    expect(runVerdictLine(run({ updated_at: "2026-09-18T13:05:00Z" }))).toBe("Accepted · 2h 05m");
  });
});

describe("runTokensText", () => {
  test("sums the models and marks a lower bound", () => {
    const models = [
      { model: "a", tokens: 1000 },
      { model: "b", tokens: 500 },
    ];
    expect(runTokensText(run({ by_model: models }))).toBe("1.5k tokens");
    expect(runTokensText(run({ by_model: models, tokens_complete: false }))).toBe("≥ 1.5k tokens");
  });

  test("no usage recorded is null", () => {
    expect(runTokensText(run())).toBeNull();
  });
});

describe("block summaries", () => {
  test("gates name the failures", () => {
    const failed = gatesSummary(
      run({ gate_results: [gate("verify", true), gate("lint", false)] }).gateResults,
    );
    expect(failed).toEqual({ text: "1 of 2 gates failed: lint", failed: true });
    const passed = gatesSummary(run({ gate_results: [gate("verify", true)] }).gateResults);
    expect(passed).toEqual({ text: "1 gate passed: verify", failed: false });
  });

  test("attempts fail on a non-zero exit, but not on a combined review's pass", () => {
    expect(attemptFailed({ kind: "build", exitCode: 1 })).toBe(true);
    expect(attemptFailed({ kind: "build", exitCode: 0 })).toBe(false);
    expect(attemptFailed({ kind: "review", exitCode: 40 })).toBe(false);
    expect(attemptFailed({ kind: "review", exitCode: 41 })).toBe(true);
  });

  test("attempts summary counts the failures", () => {
    const attempt = (exit_code: number) => ({
      command: [],
      started_at: "2026-09-18T11:00:00Z",
      finished_at: "2026-09-18T11:01:00Z",
      exit_code,
      log_path: "/l",
    });
    const ok = run({ attempts: [attempt(0)] }).attempts;
    const bad = run({ attempts: [attempt(0), attempt(3)] }).attempts;
    expect(attemptsSummary(ok)).toEqual({ text: "1 attempt · all exited 0", failed: false });
    expect(attemptsSummary(bad)).toEqual({ text: "1 attempt failed of 2", failed: true });
  });

  test("compose names each phase's services or why it did not launch", () => {
    const phases = run({
      compose_phases: [
        { phase: "build", enabled: true, services: [{ name: "redis" }, { name: "kafka" }] },
        { phase: "verify", enabled: false, disabled_reason: "no compose file" },
      ],
    }).composePhases;
    expect(composeSummary(phases)).toBe("build: redis, kafka · verify: not launched");
  });
});

describe("formatGateDuration", () => {
  test.each([
    [900, "900 ms"],
    [4200, "4.2 s"],
    [125_000, "02:05"],
  ])("%d ms reads %s", (ms, want) => {
    expect(formatGateDuration(ms)).toBe(want);
  });
});

describe("baselineVerifySummary", () => {
  const bv = (extra: Record<string, unknown>) =>
    run({ baseline_verify: { command: "go test ./...", exit_code: 1, passed: false, ...extra } })
      .baselineVerify!;

  test("a run without the record decodes to null", () => {
    expect(run().baselineVerify).toBeNull();
  });

  test.each([
    ["passed", { passed: true, exit_code: 0 }, "passed"],
    ["failed, no test named", { exit_code: 2 }, "failed: exit 2"],
    [
      "failed, first error",
      { exit_code: 127, first_error: "sh: 1: pytest: not found" },
      'failed: exit 127; first error in log: "sh: 1: pytest: not found"',
    ],
    [
      "needs a path the ticket creates",
      { exit_code: 1, expected: true, needs_created: "tests", first_error: "ImportError: x" },
      "failed as the ticket expects: the command needs tests, which the ticket creates",
    ],
    [
      "expected",
      { expected: true, failing_tests: ["TestA", "TestB", "TestC"], failing_count: 3 },
      "failed as the ticket expects: TestA and 2 more",
    ],
    [
      "one failure the ticket does not name",
      {
        failing_tests: ["TestTrimBOM"],
        failing_count: 1,
        unnamed: ["TestTrimBOM"],
        unnamed_count: 1,
      },
      "failed: TestTrimBOM; the ticket does not name it",
    ],
    [
      "several, none named",
      {
        failing_tests: ["TestA", "TestB"],
        failing_count: 2,
        unnamed: ["TestA", "TestB"],
        unnamed_count: 2,
      },
      "failed: TestA and 1 more; the ticket names none of them",
    ],
    [
      "some named",
      {
        failing_tests: ["TestA", "TestB", "TestC"],
        failing_count: 3,
        unnamed: ["TestB", "TestC"],
        unnamed_count: 2,
      },
      "failed: TestA and 2 more; the ticket does not name TestB and 1 more",
    ],
    [
      "passed, leaving files",
      {
        passed: true,
        exit_code: 0,
        left_out_of_scope: ["__pycache__/x.pyc", "a.out", "b.out"],
        left_out_of_scope_count: 3,
      },
      "passed, but the command leaves __pycache__/x.pyc and 2 more outside the ticket's Allowed-Files",
    ],
    [
      "names omitted",
      { failing_count: 4, unnamed_count: 1 },
      "failed: 4 tests; the ticket does not name 1 tests",
    ],
  ])("%s", (_name, extra, want) => {
    expect(baselineVerifySummary(bv(extra))).toBe(want);
  });

  test("only a failure the ticket does not expect counts as failed", () => {
    expect(baselineVerifyFailed(bv({ passed: true }))).toBe(false);
    expect(baselineVerifyFailed(bv({ expected: true, failing_count: 1 }))).toBe(false);
    expect(baselineVerifyFailed(bv({ failing_count: 1 }))).toBe(true);
    expect(
      baselineVerifyFailed(
        bv({ passed: true, left_out_of_scope: ["x.pyc"], left_out_of_scope_count: 1 }),
      ),
    ).toBe(true);
  });
});
