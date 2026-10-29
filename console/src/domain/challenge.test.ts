import { asObject } from "@/domain/decode";

import { challengeVerdictLabel, decodeChallenge, roundKindLabel } from "./challenge";

test("a red challenge decodes its files, failures and notes", () => {
  const c = decodeChallenge(
    asObject(
      {
        verdict: "red",
        rerun: true,
        committed_sha: "",
        files: [
          {
            path: "tests/ac1_test.go",
            sha256: "ignored",
            criterion: "AC-1",
            verdict: "red",
            failures: [{ test: "TestOne", assertion: "want 1, got 2" }],
          },
          { path: "tests/x_test.go", refused: "outside the test directory" },
        ],
        notes: ["one file refused"],
      },
      "challenge",
    ),
    "challenge",
  );
  expect(c).toEqual({
    verdict: "red",
    cause: "",
    rerun: true,
    committedSha: "",
    files: [
      {
        path: "tests/ac1_test.go",
        criterion: "AC-1",
        verdict: "red",
        refused: "",
        notRun: "",
        failures: [{ test: "TestOne", assertion: "want 1, got 2" }],
      },
      {
        path: "tests/x_test.go",
        criterion: "",
        verdict: "",
        refused: "outside the test directory",
        notRun: "",
        failures: [],
      },
    ],
    notes: ["one file refused"],
  });
});

test("a challenge without a verdict is refused", () => {
  expect(() => decodeChallenge({}, "GET /runs/{id}")).toThrow();
});

test("verdicts and round kinds read as words", () => {
  expect(challengeVerdictLabel("no_verdict")).toBe("No verdict");
  expect(challengeVerdictLabel("")).toBe("Not run");
  expect(challengeVerdictLabel("odd")).toBe("odd");
  expect(roundKindLabel("challenge")).toBe("Challenge round");
  expect(roundKindLabel("advice")).toBe("Advice round");
  expect(roundKindLabel("build")).toBe("build");
});
