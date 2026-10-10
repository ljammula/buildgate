import { render, screen, within } from "@testing-library/react";

import { asObject } from "@/domain/decode";
import { type Attempt, decodeRun } from "@/domain/run";

import { AttemptCard } from "./AttemptCard";
import { readFixtureJson } from "@/test/fixtures";

function reviewAttempt(maskedPaths: readonly string[]): Attempt {
  const at = "GET /runs/{id}";
  const run = decodeRun(asObject(readFixtureJson("api/run-every-field.json"), at), at);
  return { ...run.attempts[0]!, kind: "review", reviewMaskedPaths: maskedPaths };
}

function renderCard(attempt: Attempt) {
  return render(<AttemptCard attempt={attempt} onOpenLog={() => undefined} />);
}

test("a review attempt names the few instruction files it read from the base commit", () => {
  renderCard(reviewAttempt(["AGENTS.md", ".github/copilot-instructions.md"]));
  const block = screen.getByTestId("review-masked-paths");
  expect(block).toHaveTextContent("This review read these files as they were before the build:");
  expect(
    within(block)
      .getAllByRole("listitem")
      .map((li) => li.textContent),
  ).toEqual(["AGENTS.md", ".github/copilot-instructions.md"]);
  expect(block.tagName).toBe("DIV");
});

test("a long list is collapsed behind one line that says how many", () => {
  const paths = ["AGENTS.md", "a/AGENTS.md", "b/AGENTS.md", "c/AGENTS.md", "... and 3 more"];
  renderCard(reviewAttempt(paths));
  const block = screen.getByTestId("review-masked-paths");
  expect(block.tagName).toBe("DETAILS");
  expect(block).not.toHaveAttribute("open");
  const toggle = within(block).getByRole("button", {
    name: /This review read these files as they were before the build/,
  });
  expect(toggle).toHaveAttribute("aria-expanded", "false");
  expect(toggle).toHaveTextContent("5 listed");
  expect(
    within(block)
      .getAllByRole("listitem", { hidden: true })
      .map((li) => li.textContent),
  ).toEqual(paths);
});

test("a path is rendered as text, and an attempt that masked nothing shows no block", () => {
  const first = renderCard(reviewAttempt(["<img src=x onerror=alert(1)>/AGENTS.md"]));
  const block = screen.getByTestId("review-masked-paths");
  expect(block).toHaveTextContent("<img src=x onerror=alert(1)>/AGENTS.md");
  expect(block.querySelector("img")).toBeNull();
  first.unmount();
  renderCard(reviewAttempt([]));
  expect(screen.queryByTestId("review-masked-paths")).toBeNull();
});

test("the contract fixture's attempt renders its masked path", () => {
  const at = "GET /runs/{id}";
  const run = decodeRun(asObject(readFixtureJson("api/run-every-field.json"), at), at);
  renderCard(run.attempts[0]!);
  expect(screen.getByTestId("review-masked-paths")).toHaveTextContent(
    "every-field review_masked_paths",
  );
});
