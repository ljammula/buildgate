import { asObject } from "@/domain/decode";
import {
  decodeObservationReport,
  observationCounts,
  observationKindLabel,
} from "@/domain/observation";
import { readFixtureJson } from "@/test/fixtures";

const at = "GET /projects/{project}/observations";

test("GET /projects/{project}/observations decodes", () => {
  const report = decodeObservationReport(
    asObject(readFixtureJson("api/project-observations.json"), at),
    at,
  );
  expect(report.project).toBe("app");
  expect([report.runs, report.acceptedFirstRound, report.truncated]).toEqual([3, 1, false]);
  expect(report.observations.map((o) => o.kind)).toEqual([
    "repeated_failure",
    "round_changed_nothing",
    "check_failed",
  ]);
  expect(report.observations[0]).toEqual({
    kind: "repeated_failure",
    runId: "run-quarantined",
    ticket: "ticket-run-quarantined",
    at: "2026-09-10T09:40:00Z",
    what: "Rounds 2 and 3 failed the same way (no changes made to the workspace; canonical verification failed).",
    rounds: [2, 3],
    blockers: ["no changes made to the workspace", "canonical verification failed"],
    changedFiles: [],
    check: "",
    log: "round-logs/round-3/verify.log",
    excerpt:
      "--- FAIL: TestKeyScopedToAccount (0.00s)\n    idempotency_test.go:41: key reused across accounts\nFAIL\nFAIL\tapp/checkout\t0.031s",
  });
  expect(report.observations[2]?.check).toBe("canonical_verify");
});

test("counts list only the kinds that happened, known kinds first", () => {
  const report = decodeObservationReport(
    {
      project: "p",
      runs: 4,
      accepted_first_round: 1,
      counts: { run_halted: 2, a_newer_kind: 1, fixed_after_failure: 3, check_failed: 0 },
      observations: null,
    },
    at,
  );
  expect(observationCounts(report)).toEqual([
    ["fixed_after_failure", 3],
    ["run_halted", 2],
    ["a_newer_kind", 1],
  ]);
  expect(report.observations).toEqual([]);
  expect(observationKindLabel("fixed_after_failure")).toBe("Fixed after a failed round");
  expect(observationKindLabel("a_newer_kind")).toBe("a_newer_kind");
});

test("a report missing a required field names the route and the field", () => {
  expect(() => decodeObservationReport({ project: "p" }, at)).toThrow(/observations\.runs/);
  const bad = { project: "p", runs: 1, accepted_first_round: 0, observations: [{ kind: "x" }] };
  expect(() => decodeObservationReport(bad, at)).toThrow(/observations\[0\]\.run_id/);
});
