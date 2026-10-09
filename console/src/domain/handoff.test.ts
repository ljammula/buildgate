import { asObject } from "@/domain/decode";
import {
  decodeHandoff,
  handoffBinLabel,
  handoffNextSentence,
  handoffNoteSections,
} from "@/domain/handoff";
import { readFixtureJson } from "@/test/fixtures";

const at = "GET /runs/{id}/handoff";

test("GET /runs/{id}/handoff decodes", () => {
  const handoff = decodeHandoff(asObject(readFixtureJson("api/run-handoff.json"), at), at);
  expect(handoff).toEqual({
    runId: "run-quarantined",
    state: "quarantined",
    next: "corrective",
    checks: [
      {
        check: "canonical_verify",
        bin: "corrective",
        exitCode: 1,
        finding: "",
        output: [],
        notJudged: false,
      },
      {
        check: "tests_added",
        bin: "never",
        exitCode: 0,
        finding: "tests_added: no test file changed",
        output: [],
        notJudged: true,
      },
    ],
    agentNotes: {
      did: ["Scoped the idempotency key by account id in checkout/idempotency.go."],
      triedAndFailed: [
        "Hashing the account id into the key: TestKeyScopedToAccount compares the raw key.",
      ],
      hypothesis: ["The key is built before the account is loaded, so the scope is always empty."],
      leftToDo: ["Load the account before building the key.", "Run the checkout tests."],
      repository: ["The checkout tests need the cart fixtures to be generated first."],
    },
  });
});

test("the agent's notes are listed under the record's five labels, an empty heading left out", () => {
  expect(handoffNoteSections(null)).toEqual([]);
  const handoff = decodeHandoff(
    {
      run_id: "r",
      state: "halted",
      next: "operator",
      agent_notes: { hypothesis: ["a"], repository: ["b", "c"] },
    },
    at,
  );
  expect(handoffNoteSections(handoff.agentNotes)).toEqual([
    { label: "Its hypothesis", items: ["a"] },
    { label: "What it said about this repository", items: ["b", "c"] },
  ]);
  expect(
    handoffNoteSections(
      decodeHandoff({ ...{ run_id: "r", state: "s", next: "never" }, agent_notes: {} }, at)
        .agentNotes,
    ),
  ).toEqual([]);
  expect(() =>
    decodeHandoff({ run_id: "r", state: "s", next: "never", agent_notes: { did: [1] } }, at),
  ).toThrow(/agent_notes\.did/);
});

test("a handoff with no checks decodes, and one missing a required field names it", () => {
  const halted = decodeHandoff({ run_id: "r", state: "halted", next: "operator" }, at);
  expect(halted.checks).toEqual([]);
  expect(halted.agentNotes).toBeNull();
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
