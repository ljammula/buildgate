import { render, screen } from "@testing-library/react";

import { RequestStageChip } from "./RequestStageChip";

test("chip shows a calm accepted label, not halted", () => {
  render(<RequestStageChip state="halted" awaitingPullRequest />);
  expect(screen.getByText("Accepted · awaiting PR")).toBeInTheDocument();
  expect(screen.queryByText("Halted")).not.toBeInTheDocument();
  expect(screen.queryByText("halted")).not.toBeInTheDocument();
});

test('the state chip shows "Queued behind <short id>" instead of Building when waitingOn is set', () => {
  render(<RequestStageChip state="building" waitingOn="req-ahead" />);
  expect(screen.getByText("Queued behind req-ahead")).toBeInTheDocument();
  expect(screen.queryByText("Building")).not.toBeInTheDocument();
});

test("a long waiting-on id is shortened to twelve characters", () => {
  render(<RequestStageChip state="building" waitingOn="req-ahead-1234567890" />);
  expect(screen.getByText("Queued behind req-ahead-12…")).toBeInTheDocument();
});

test('the state chip shows plain "Building" when waitingOn is absent', () => {
  render(<RequestStageChip state="building" />);
  expect(screen.getByText("Building")).toBeInTheDocument();
  expect(screen.queryByText(/Queued behind/)).not.toBeInTheDocument();
});

test("a pr_review chip on a row that needs you takes the needs-you status", () => {
  const { rerender } = render(<RequestStageChip state="pr_review" needsYou />);
  // needsHuman draws as the warning tone, a working pr_review as info.
  expect(screen.getByText("PR review").closest("[data-tone]")).toHaveAttribute(
    "data-tone",
    "warning",
  );
  rerender(<RequestStageChip state="pr_review" />);
  expect(screen.getByText("PR review").closest("[data-tone]")).toHaveAttribute("data-tone", "info");
});

test("a quarantined chip that needs you keeps its failure colour", () => {
  render(<RequestStageChip state="quarantined" needsYou />);
  expect(screen.getByText("Quarantined").closest("[data-tone]")).toHaveAttribute(
    "data-tone",
    "danger",
  );
});
