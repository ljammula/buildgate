import { render, screen } from "@testing-library/react";

import { CodeBlock, LogView } from "@/ui/CodeBlock";

test("shows text verbatim, never as HTML", () => {
  const { container } = render(<CodeBlock>{"<script>alert(1)</script>\n<b>x</b>"}</CodeBlock>);
  expect(container.querySelector("script, b")).toBeNull();
  expect(container.textContent).toBe("<script>alert(1)</script>\n<b>x</b>");
});

test("is a focusable, named, scrollable region", () => {
  render(<CodeBlock maxHeight="max-h-40">x</CodeBlock>);
  const region = screen.getByRole("region", { name: "Text" });
  expect(region).toHaveAttribute("tabindex", "0");
  expect(region).toHaveClass("font-mono", "overflow-auto", "max-h-40", "whitespace-pre");
});

test("wrap swaps horizontal scrolling for wrapping", () => {
  render(<CodeBlock wrap>x</CodeBlock>);
  expect(screen.getByRole("region")).toHaveClass("whitespace-pre-wrap");
});

test("writes invisible and bidi characters out but keeps tabs and newlines", () => {
  const { container } = render(<CodeBlock>{"a‮b\u0000\tc\nd"}</CodeBlock>);
  expect(container.textContent).toBe("a\\u{202E}b\\u{0}\tc\nd");
});

test("LogView says so when there is no output", () => {
  render(<LogView text="" />);
  expect(screen.getByRole("region", { name: "Build log" })).toHaveTextContent(
    "(no log output yet)",
  );
});

test("LogView turns progress lines into readable steps and keeps other lines", () => {
  const { container } = render(
    <LogView
      text={
        'FACTORY_PROGRESS {"stage":"round","event":"started","round":1,"max_rounds":3}\nplain <i>line</i>\nFACTORY_PROGRESS {cut'
      }
    />,
  );
  expect(container.textContent).toContain("round 1/3");
  expect(container.textContent).not.toContain('FACTORY_PROGRESS {"stage');
  expect(container.textContent).toContain("plain <i>line</i>");
  expect(container.textContent).toContain("FACTORY_PROGRESS {cut");
  expect(container.querySelector("i")).toBeNull();
});
