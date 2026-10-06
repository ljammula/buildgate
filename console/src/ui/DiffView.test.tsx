import { render, screen } from "@testing-library/react";

import { DiffView, TextDiffView, classifyDiffLine } from "@/ui/DiffView";

const DIFF =
  "diff --git a/lib/a.dart b/lib/a.dart\n" +
  "--- a/lib/a.dart\n" +
  "+++ b/lib/a.dart\n" +
  "@@ -1,2 +1,2 @@\n" +
  " same\n" +
  "-old line\n" +
  "+new line\n";

function kindOf(container: HTMLElement, text: string): string | null {
  const row = [...container.querySelectorAll("[data-line]")].find((r) => r.textContent === text);
  return row?.getAttribute("data-line") ?? null;
}

test("classifies each line by its first characters", () => {
  const { container } = render(<DiffView diff={DIFF} />);
  expect(kindOf(container, "+new line")).toBe("add");
  expect(kindOf(container, "-old line")).toBe("del");
  expect(kindOf(container, "@@ -1,2 +1,2 @@")).toBe("hunk");
  expect(kindOf(container, "--- a/lib/a.dart")).toBe("meta");
  expect(kindOf(container, "+++ b/lib/a.dart")).toBe("meta");
  expect(kindOf(container, " same")).toBe("context");
  expect(kindOf(container, "diff --git a/lib/a.dart b/lib/a.dart")).toBe("context");
});

test("colours added, removed and hunk lines with the diff tokens", () => {
  const { container } = render(<DiffView diff={DIFF} />);
  expect(container.querySelector('[data-line="add"]')).toHaveClass(
    "bg-diff-add",
    "text-diff-add-fg",
  );
  expect(container.querySelector('[data-line="del"]')).toHaveClass(
    "bg-diff-del",
    "text-diff-del-fg",
  );
  expect(container.querySelector('[data-line="hunk"]')).toHaveClass("bg-diff-hunk");
});

test("classifyDiffLine", () => {
  expect(classifyDiffLine("")).toBe("context");
  expect(classifyDiffLine("+")).toBe("add");
  expect(classifyDiffLine("-")).toBe("del");
  expect(classifyDiffLine("@@")).toBe("hunk");
  expect(classifyDiffLine("+++")).toBe("meta");
});

test('shows "No changes." for an empty diff', () => {
  render(<DiffView diff="" />);
  expect(screen.getByText("No changes.")).toBeInTheDocument();
});

test("shows the truncation notice only when truncated", () => {
  const { rerender } = render(<DiffView diff={DIFF} truncated />);
  expect(screen.getByText("This diff was too large and has been truncated.")).toBeInTheDocument();
  rerender(<DiffView diff={DIFF} />);
  expect(screen.queryByText("This diff was too large and has been truncated.")).toBeNull();
});

test("is a focusable, named region in monospace", () => {
  render(<DiffView diff={DIFF} />);
  const region = screen.getByRole("region", { name: "Diff" });
  expect(region).toHaveAttribute("tabindex", "0");
  expect(region).toHaveClass("font-mono", "overflow-auto");
});

test("hostile content is visible text and the DOM is inert", () => {
  const { container } = render(
    <DiffView
      diff={"+<script>alert(1)</script>\n-<img src=x onerror=alert(1)>\n <svg onload=alert(1)>"}
    />,
  );
  expect(container.textContent).toContain("+<script>alert(1)</script>");
  expect(container.textContent).toContain("-<img src=x onerror=alert(1)>");
  for (const tag of ["script", "iframe", "img", "object", "embed", "style", "svg", "form"]) {
    expect(container.querySelector(tag)).toBeNull();
  }
  for (const el of container.querySelectorAll("*")) {
    for (const attr of el.attributes) expect(attr.name.startsWith("on")).toBe(false);
  }
});

test("writes invisible and bidi characters out", () => {
  const { container } = render(<DiffView diff={"+a‮b​"} />);
  expect(kindOf(container, "+a\\u{202E}b\\u{200B}")).toBe("add");
});

test("TextDiffView shows a before/after line diff under file headers", () => {
  const { container } = render(
    <TextDiffView
      before={"a\nb"}
      after={"a\nc"}
      beforeLabel="current (on disk)"
      afterLabel="yours (unsaved)"
    />,
  );
  expect(kindOf(container, "--- current (on disk)")).toBe("meta");
  expect(kindOf(container, "+++ yours (unsaved)")).toBe("meta");
  expect(kindOf(container, "  a")).toBe("context");
  expect(kindOf(container, "- b")).toBe("del");
  expect(kindOf(container, "+ c")).toBe("add");
});
