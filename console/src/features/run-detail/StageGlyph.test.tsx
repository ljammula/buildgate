import { render } from "@testing-library/react";

import { StageGlyph } from "@/features/run-detail/StageGlyph";

test("a running stage spins only when live", () => {
  const { container, rerender } = render(<StageGlyph glyph="running" live />);
  expect(container.querySelector(".animate-spin")).not.toBeNull();
  rerender(<StageGlyph glyph="running" live={false} />);
  expect(container.querySelector(".animate-spin")).toBeNull();
  expect(container.querySelector("svg")).toHaveClass("text-tone-info");
});

test("a running stage of a stalled run is a danger-toned static icon", () => {
  const { container } = render(<StageGlyph glyph="running" live={false} stalled />);
  expect(container.querySelector(".animate-spin")).toBeNull();
  expect(container.querySelector("svg")).toHaveClass("text-tone-danger");
});

test("every glyph keeps its status word", () => {
  const words = {
    running: "Running",
    passed: "Passed",
    failed: "Failed",
    skipped: "Skipped",
    pending: "Pending",
  } as const;
  for (const [glyph, word] of Object.entries(words) as [keyof typeof words, string][]) {
    const { container, unmount } = render(<StageGlyph glyph={glyph} live={false} />);
    expect(container.querySelector(".sr-only")).toHaveTextContent(word);
    unmount();
  }
});

test("passed is success-toned and failed danger-toned, neither spinning", () => {
  const passed = render(<StageGlyph glyph="passed" live />);
  expect(passed.container.querySelector("svg")).toHaveClass("text-tone-success");
  passed.unmount();
  const failed = render(<StageGlyph glyph="failed" live />);
  expect(failed.container.querySelector("svg")).toHaveClass("text-tone-danger");
  expect(failed.container.querySelector(".animate-spin")).toBeNull();
});
