import { asObject } from "@/domain/decode";
import { decodeHandoff, handoffBinLabel, handoffNextSentence } from "@/domain/handoff";
import { readFixtureJson } from "@/test/fixtures";

const at = "GET /runs/{id}/handoff";

test("GET /runs/{id}/handoff decodes", () => {
  const handoff = decodeHandoff(asObject(readFixtureJson("api/run-handoff.json"), at), at);
  expect(handoff).toEqual({
    runId: "run-quarantined",
    state: "quarantined",
    next: "never",
    checks: [
      { check: "canonical_verify", bin: "corrective", exitCode: 1, finding: "" },
      {
        check: "tests_added",
        bin: "never",
        exitCode: 0,
        finding: "tests_added: no test file changed",
      },
    ],
  });
});

test("a handoff with no checks decodes, and one missing a required field names it", () => {
  const halted = decodeHandoff({ run_id: "r", state: "halted", next: "operator" }, at);
  expect(halted.checks).toEqual([]);
  expect(() => decodeHandoff({ run_id: "r", state: "halted" }, at)).toThrow(/handoff\.next/);
  const bad = { run_id: "r", state: "s", next: "never", checks: [{ check: "lint" }] };
  expect(() => decodeHandoff(bad, at)).toThrow(/checks\[0\]\.bin/);
});

test("every bin the server writes has its own wording, and an unknown one keeps its name", () => {
  for (const bin of ["corrective", "corrective_if_oracle_in_loop", "never", "operator"]) {
    expect(handoffBinLabel(bin)).not.toBe(bin);
    expect(handoffNextSentence(bin)).not.toContain("Sorted as");
  }
  expect(handoffBinLabel("a_newer_bin")).toBe("a_newer_bin");
  expect(handoffNextSentence("a_newer_bin")).toBe("Sorted as: a_newer_bin.");
});
