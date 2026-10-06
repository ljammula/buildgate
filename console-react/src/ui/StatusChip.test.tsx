import { render, screen } from "@testing-library/react";

import { KillSwitchChip, StatusChip } from "@/ui/StatusChip";

describe("KillSwitchChip", () => {
  test('never renders "clear" from a null (unknown) input', () => {
    render(<KillSwitchChip engaged={null} />);
    expect(screen.getByText("Kill switch unknown")).toBeInTheDocument();
    expect(screen.queryByText("Kill switch clear")).not.toBeInTheDocument();
    expect(screen.queryByText("Kill switch engaged")).not.toBeInTheDocument();
  });

  test("renders engaged distinctly from clear", () => {
    const { rerender } = render(<KillSwitchChip engaged={true} />);
    expect(screen.getByText("Kill switch engaged")).toHaveAttribute("data-tone", "danger");
    rerender(<KillSwitchChip engaged={false} />);
    expect(screen.getByText("Kill switch clear")).toBeInTheDocument();
    expect(screen.queryByText("Kill switch engaged")).not.toBeInTheDocument();
  });

  test("renders a different, still-visible grey in dark mode", () => {
    const { rerender } = render(<KillSwitchChip engaged={null} brightness="light" />);
    expect(screen.getByText("Kill switch unknown")).toHaveAttribute("data-tone", "neutral");
    rerender(<KillSwitchChip engaged={null} brightness="dark" />);
    expect(screen.getByText("Kill switch unknown")).toHaveAttribute("data-tone", "neutralOnDark");
  });
});

test("StatusChip shows the operator word, raw token as tooltip", () => {
  render(<StatusChip status="needsHuman" label="spec_review" />);
  const chip = screen.getByText("Spec review");
  expect(chip).toHaveAttribute("title", "spec_review");
  expect(chip).toHaveAttribute("data-tone", "warning");
  expect(screen.queryByText("spec_review")).not.toBeInTheDocument();
});

test("StatusChip with an unmapped label shows it verbatim, outlined, with no tooltip", () => {
  render(<StatusChip status="unknown" label="never_seen" />);
  const chip = screen.getByText("never_seen");
  expect(chip).not.toHaveAttribute("title");
  expect(chip.className).not.toContain("bg-tone-");
});
