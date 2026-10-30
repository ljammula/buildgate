import { screen, within } from "@testing-library/react";

import { asObject } from "@/domain/decode";
import { decodeRun } from "@/domain/run";
import { readFixtureJson } from "@/test/fixtures";
import { renderApp } from "@/test/render";

import { RunChallengeCard } from "./RunChallengeCard";

function runWith(challenge: Record<string, unknown> | undefined) {
  const at = "GET /runs/{id}";
  const base = asObject(readFixtureJson("api/run-accepted.json"), at);
  return decodeRun(challenge === undefined ? base : { ...base, challenge }, at);
}

test("a run with no challenge shows no challenge card", () => {
  renderApp(<RunChallengeCard run={runWith(undefined)} />);
  expect(screen.queryByTestId("run-challenge")).not.toBeInTheDocument();
});

test("a green challenge shows the short commit and each file with its criterion", () => {
  renderApp(
    <RunChallengeCard
      run={runWith({
        verdict: "green",
        committed_sha: "0123456789abcdef0123",
        files: [{ path: "tests/ac1_test.go", criterion: "AC-1", verdict: "green" }],
        notes: ["one file was already covered"],
      })}
    />,
  );
  expect(screen.getByRole("heading", { name: "Challenge" })).toBeInTheDocument();
  expect(screen.getByText("Verdict: Green")).toBeInTheDocument();
  expect(screen.getByText("Committed 0123456789ab")).toBeInTheDocument();
  const file = screen.getByTestId("run-challenge-file");
  expect(within(file).getByText("tests/ac1_test.go")).toBeInTheDocument();
  expect(within(file).getByText("AC-1")).toBeInTheDocument();
  expect(within(file).getByText("Green")).toBeInTheDocument();
  expect(screen.getByText("one file was already covered")).toBeInTheDocument();
});

test("a red challenge lists the failing tests and shows no commit", () => {
  renderApp(
    <RunChallengeCard
      run={runWith({
        verdict: "red",
        committed_sha: "0123456789abcdef0123",
        files: [
          {
            path: "tests/ac2_test.go",
            criterion: "AC-2",
            verdict: "red",
            failures: [{ test: "TestTwo", assertion: "want 2, got 3" }],
          },
        ],
      })}
    />,
  );
  expect(screen.getByText("Verdict: Red")).toBeInTheDocument();
  expect(screen.queryByText(/^Committed/)).not.toBeInTheDocument();
  expect(screen.getByText("TestTwo: want 2, got 3")).toBeInTheDocument();
});

test("a challenge with no verdict names its cause", () => {
  renderApp(
    <RunChallengeCard run={runWith({ verdict: "no_verdict", cause: "the job was lost" })} />,
  );
  expect(screen.getByText("Verdict: No verdict")).toBeInTheDocument();
  expect(screen.getByText("Cause: the job was lost")).toBeInTheDocument();
});
