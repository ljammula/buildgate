import { render, screen } from "@testing-library/react";

import { RequestStageChip } from "./RequestStageChip";

test("chip shows a calm accepted label, not halted", () => {
  render(<RequestStageChip state="halted" awaitingPullRequest />);
  expect(screen.getByText("Accepted · awaiting PR")).toBeInTheDocument();
  expect(screen.queryByText("Halted")).not.toBeInTheDocument();
  expect(screen.queryByText("halted")).not.toBeInTheDocument();
});

test("the awaiting-PR chip is in the waiting tone with the stuck shape, not the green of a passed state", () => {
  render(<RequestStageChip state="halted" awaitingPullRequest />);
  const chip = screen.getByText("Accepted · awaiting PR").closest("[data-tone]");
  expect(chip).toHaveAttribute("data-tone", "warning");
  expect(chip?.querySelector("svg")).toHaveClass("lucide-octagon-pause");
});

test('the state chip shows "Queued behind <short id>" instead of Building when a job_slot wait is set', () => {
  render(
    <RequestStageChip
      state="building"
      wait={{ kind: "job_slot", since: "", on: "req-ahead", position: 1, slots: 1 }}
    />,
  );
  expect(screen.getByText("Queued behind req-ahead")).toBeInTheDocument();
  expect(screen.queryByText("Building")).not.toBeInTheDocument();
});

test("a long waiting-on id is shortened to twelve characters", () => {
  render(
    <RequestStageChip
      state="building"
      wait={{ kind: "job_slot", since: "", on: "req-ahead-1234567890", position: 1, slots: 1 }}
    />,
  );
  expect(screen.getByText("Queued behind req-ahead-12…")).toBeInTheDocument();
});

test('the state chip shows plain "Building" when no wait is set', () => {
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

test("each kind of wait is named on the chip in place of the state", () => {
  const wait = (kind: string) => ({ kind, since: "", on: "", position: null, slots: null });
  for (const [kind, label] of [
    ["repository", "Waiting for repository"],
    ["repository_queue", "Queued on repository"],
    ["model_host", "Waiting for model host"],
    ["compose_slot", "Waiting for sidecars"],
    ["no_worker", "No worker"],
  ] as const) {
    const { unmount } = render(<RequestStageChip state="building" wait={wait(kind)} />);
    expect(screen.getByText(label)).toBeInTheDocument();
    unmount();
  }
});
